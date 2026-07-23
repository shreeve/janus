package janus

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Cold auth configuration (docs/20260722-134812-capability-auth.md
// "Cold config"). Site-scoped capability, the cache/hub pattern: global
// default → site override; explicit off beats an inherited on; built-in
// default off. ttl cascades per key; users resolve at exactly one level
// — a site block with at least one user line supplies the site's entire
// user set, never merged with the global set.

// Built-in defaults and fixed v1 parameters.
const (
	authDefaultTTL = 8 * time.Hour

	authCookieName     = "__Host-janus_auth"
	authCSRFCookieName = "__Host-janus_auth_csrf"
	authCSRFCookieAge  = 600 // seconds

	// Cheap caps enforced before any argon2 work.
	authBodyCap     = 64 << 10
	authUserCap     = 256
	authPasswordCap = 1024
	authReturnToCap = 2048

	// Login throttle: fixed window per (client key, username).
	authThrottleLimit  = 5
	authThrottleWindow = 15 * time.Minute

	// authThrottleMaxEntries bounds the throttle map: at the cap,
	// expired windows are swept; if none are expired, an arbitrary
	// entry is evicted so the map can never grow past the bound.
	authThrottleMaxEntries = 65536

	// Argon2 verification concurrency: at most authVerifyConcurrency
	// KDF runs at once; at most authVerifyQueueMax more may wait behind
	// them. Past the queue depth a login attempt fast-fails 429 —
	// a distinct-pair spray can never stack 64 MiB allocations.
	authVerifyConcurrency = 2
	authVerifyQueueMax    = 16
)

// The g1 credential format: argon2id with parameters fixed as module
// constants under the version tag. Stored form is "g1:" +
// base64.RawStdEncoding(salt‖digest) — standard alphabet, no padding,
// exactly 64 encoded characters for the 48 decoded bytes (16-byte salt,
// 32-byte key). There is nothing to parse but base64 and a length.
const (
	g1Prefix  = "g1:"
	g1Memory  = 64 * 1024 // KiB → 64 MiB
	g1Time    = 2
	g1Threads = 1
	g1SaltLen = 16
	g1KeyLen  = 32
	g1RawLen  = g1SaltLen + g1KeyLen // 48
	g1EncLen  = 64                   // RawStdEncoding of 48 bytes
)

// AuthUser is one cold-configured credential: a lowercased, header-safe
// username and its g1 blob.
type AuthUser struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// AuthSettings configures the site-scoped auth wall. It appears in the
// global janus options (default posture + default user set) and per site
// (override). The full contract is docs/20260722-134812-capability-auth.md.
type AuthSettings struct {
	// Enabled turns the wall on or off for the site. Default: off.
	// Sites may override the global default; explicit off beats an
	// inherited on.
	Enabled *bool `json:"enabled,omitempty"`

	// Users is this level's user set. Site users REPLACE the global set
	// (one-level resolution, never merged); a site block with no user
	// lines inherits the global set whole.
	Users []AuthUser `json:"users,omitempty"`

	// TTL is the sliding idle session timeout. Default: 8h.
	TTL *caddy.Duration `json:"ttl,omitempty"`
}

// authSite is one site's effective auth configuration after cascade.
type authSite struct {
	users map[string]string // lowercased name → g1 blob
	ttl   time.Duration
	store *authStore

	// plainHTTPLogged makes the dead-wall ERROR (plain-HTTP guarded
	// site → 421) fire once per site, not once per request.
	plainHTTPLogged atomic.Bool
}

// parseAuthDirective parses one auth directive:
//
//	auth
//	auth on
//	auth off
//	auth { user alice g1:…; user bob g1:…; ttl 8h }
//	auth on { … }
//
// Hard errors per the capability contract: unknown argument, a block on
// "off", unknown subdirectives, duplicate ttl, user without exactly two
// arguments, duplicate usernames, illegal usernames, malformed g1
// blobs, non-positive ttl, nested blocks.
func parseAuthDirective(d *caddyfile.Dispenser) (*AuthSettings, error) {
	as := &AuthSettings{}
	on, err := parseOnOff(d.RemainingArgs())
	if err != nil {
		return nil, d.Errf("auth: %v", err)
	}
	as.Enabled = &on

	seen := map[string]bool{}
	users := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if !on {
			return nil, d.Err("auth off does not take a block (a block on an off switch is a contradiction)")
		}
		sub := d.Val()
		if sub != "user" && seen[sub] {
			return nil, d.Errf("auth: duplicate subdirective %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "user":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return nil, d.Err("auth user: want exactly two arguments (user <name> g1:<credential>)")
			}
			name, verr := validateAuthUsername(args[0])
			if verr != nil {
				return nil, d.Errf("auth user: %v", verr)
			}
			if users[name] {
				return nil, d.Errf("auth user: duplicate username %q in one block", name)
			}
			users[name] = true
			if verr := validateG1(args[1]); verr != nil {
				return nil, d.Errf("auth user %s: %v", name, verr)
			}
			as.Users = append(as.Users, AuthUser{Name: name, Credential: args[1]})
		case "ttl":
			val, err := oneDirectiveArg(d, "auth", sub)
			if err != nil {
				return nil, err
			}
			dur, perr := caddy.ParseDuration(val)
			if perr != nil || dur <= 0 {
				return nil, d.Errf("auth ttl: want a positive duration (e.g. 8h), got %q", val)
			}
			cd := caddy.Duration(dur)
			as.TTL = &cd
		default:
			return nil, d.Errf("unrecognized auth subdirective: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("auth %s does not take a nested block", sub)
		}
	}
	return as, nil
}

// validateAuthUsername lowercases and checks a username: 1–64 bytes of
// a-z 0-9 . _ - after lowercasing — header-safe for Remote-User by
// construction. Login lowercases the submitted name the same way.
func validateAuthUsername(raw string) (string, error) {
	name := strings.ToLower(raw)
	if name == "" {
		return "", fmt.Errorf("username must not be empty")
	}
	if len(name) > 64 {
		return "", fmt.Errorf("username %q is too long (max 64 bytes)", raw)
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return "", fmt.Errorf("username %q has an illegal character %q (want a-z 0-9 . _ - after lowercasing)", raw, string(r))
		}
	}
	return name, nil
}

// validateG1 checks a stored credential without decoding it into use:
// the g1: tag, the raw-standard-base64 alphabet (no padding, no url
// alphabet), and the exact decoded length. Malformed blobs reject at
// parse/provision, never at first login.
func validateG1(cred string) error {
	_, err := decodeG1(cred)
	return err
}

// decodeG1 decodes a g1 blob into its 48 raw bytes (salt‖digest) with
// precise rejections for every malformed shape.
func decodeG1(cred string) ([]byte, error) {
	rest, ok := strings.CutPrefix(cred, g1Prefix)
	if !ok {
		if i := strings.IndexByte(cred, ':'); i > 0 {
			return nil, fmt.Errorf("credential has unknown version tag %q (want g1:)", cred[:i+1])
		}
		return nil, fmt.Errorf("credential is missing the g1: prefix")
	}
	if strings.ContainsAny(rest, "=-_") {
		return nil, fmt.Errorf("credential is not a g1 blob (g1 uses standard base64 with no padding: no '=', '-', or '_')")
	}
	raw, err := base64.RawStdEncoding.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("credential is not a g1 blob (invalid base64: %v)", err)
	}
	if len(raw) != g1RawLen {
		return nil, fmt.Errorf("credential is not a g1 blob (decoded %d bytes, want %d)", len(raw), g1RawLen)
	}
	return raw, nil
}

// validateAuthSettings applies the parse-time rejections to a settings
// block arriving through native JSON — the same rejections the
// Caddyfile grammar enforces.
func validateAuthSettings(as *AuthSettings) error {
	if as == nil {
		return nil
	}
	names := map[string]bool{}
	for i := range as.Users {
		name, err := validateAuthUsername(as.Users[i].Name)
		if err != nil {
			return err
		}
		if names[name] {
			return fmt.Errorf("duplicate username %q in one block", name)
		}
		names[name] = true
		as.Users[i].Name = name
		if err := validateG1(as.Users[i].Credential); err != nil {
			return fmt.Errorf("user %s: %w", name, err)
		}
	}
	if as.TTL != nil && *as.TTL <= 0 {
		return fmt.Errorf("ttl must be a positive duration")
	}
	return nil
}

// --- cascade -------------------------------------------------------------------

func authEnabledPtr(as *AuthSettings) *bool {
	if as == nil {
		return nil
	}
	return as.Enabled
}

// resolveAuthUsers applies the one-level user rule: a site block that
// declares at least one user line supplies the site's entire set; a
// site block with no user lines inherits the global set whole.
func resolveAuthUsers(site, global *AuthSettings) map[string]string {
	src := global
	if site != nil && len(site.Users) > 0 {
		src = site
	}
	if src == nil {
		return map[string]string{}
	}
	users := make(map[string]string, len(src.Users))
	for _, u := range src.Users {
		users[u.Name] = u.Credential
	}
	return users
}

// provisionAuth resolves this site's effective auth configuration.
// Effective off leaves h.authCfg nil — the request-path check is one nil
// compare. The zero-users lockout is a hard provision error here (the
// check is computable locally, per the contract's lifecycle split);
// cross-site work — removed-user session revocation, the site table —
// runs at App Start.
func (h *Handler) provisionAuth() error {
	var g *AuthSettings
	if h.app != nil {
		g = h.app.Auth
	}
	s := h.Auth
	for _, as := range []*AuthSettings{s, g} {
		if err := validateAuthSettings(as); err != nil {
			return fmt.Errorf("janus auth: %w", err)
		}
	}
	if !cascadeBool(authEnabledPtr(s), authEnabledPtr(g), false) {
		return nil
	}
	users := resolveAuthUsers(s, g)
	if len(users) == 0 {
		return fmt.Errorf("janus auth: this site's effective auth is on but its resolved user set is empty" +
			" (the site block declares no user lines and the global janus block has none)" +
			" — an enabled wall with zero credentials is a lockout; add user lines or 'auth off'")
	}
	if h.app == nil || h.app.state == nil {
		return nil // no data plane to guard (every request 404s already)
	}
	var sTTL, gTTL *caddy.Duration
	if s != nil {
		sTTL = s.TTL
	}
	if g != nil {
		gTTL = g.TTL
	}
	h.authCfg = &authSite{
		users: users,
		ttl:   cascadeDuration(sTTL, gTTL, authDefaultTTL),
		store: h.app.state.auth,
	}
	return nil
}

// --- the site table (App Start) -----------------------------------------------

// authSiteEntry pairs one compiled route's host patterns with the janus
// handler's effective auth configuration (nil when the site's effective
// auth is off).
type authSiteEntry struct {
	patterns []string // empty = the route matches every host on its server
	cfg      *authSite
}

// startAuth runs the cross-site Start work: build the site table, revoke
// sessions whose user appears in no enabled site's resolved set (counted
// and logged), and refresh the reaper's idle bound to the max effective
// ttl. Runs at App Start — past the reload's point of no return — so an
// aborted reload never logs users out; with the per-request set check
// this revocation is promptness, not the security boundary.
func (a *App) startAuth() error {
	if a.state == nil || a.state.auth == nil {
		return nil // no pooled state (native-config edge): nothing to reconcile
	}
	if err := a.buildAuthSiteTable(); err != nil {
		return err
	}
	keep := map[string]bool{}
	maxTTL := authDefaultTTL
	enabled := false
	for _, e := range a.authSites {
		if e.cfg == nil {
			continue
		}
		enabled = true
		for name := range e.cfg.users {
			keep[name] = true
		}
		if e.cfg.ttl > maxTTL {
			maxTTL = e.cfg.ttl
		}
	}
	st := a.state.auth
	st.reapTTL.Store(int64(maxTTL))
	if n := st.revokeUsersNotIn(keep); n > 0 {
		a.logger.Warn("janus auth: sessions revoked at reload (user absent from every enabled site's resolved set)",
			zap.Int("revoked", n),
			zap.Bool("auth_enabled_anywhere", enabled),
		)
	}
	return nil
}

// buildAuthSiteTable walks the provisioned HTTP app's servers and
// routes, pairing each janus site with its host matchers (the hub
// site-table seam, reused).
func (a *App) buildAuthSiteTable() error {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil || httpAppI == nil {
		return nil // no HTTP app: no sites, no wall
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return fmt.Errorf("janus auth: http app is unexpected type %T", httpAppI)
	}
	var entries []authSiteEntry
	for _, srv := range ha.Servers {
		collectAuthRoutes(srv.Routes, nil, &entries)
	}
	a.authSites = entries
	return nil
}

func collectAuthRoutes(routes caddyhttp.RouteList, hosts []string, entries *[]authSiteEntry) {
	for _, route := range routes {
		routeHosts := hosts
		if h := hostsFromMatcherSets(route.MatcherSets); len(h) > 0 {
			routeHosts = h
		}
		for _, handler := range route.Handlers {
			switch v := handler.(type) {
			case *Handler:
				*entries = append(*entries, authSiteEntry{patterns: routeHosts, cfg: v.authCfg})
			case *caddyhttp.Subroute:
				collectAuthRoutes(v.Routes, routeHosts, entries)
			}
		}
	}
}

// authEnabledSites reports the host patterns of every auth-enabled site
// (sorted, unique; a catch-all route reads as "*").
func (a *App) authEnabledSites() []string {
	seen := map[string]bool{}
	var out []string
	for _, e := range a.authSites {
		if e.cfg == nil {
			continue
		}
		patterns := e.patterns
		if len(patterns) == 0 {
			patterns = []string{"*"}
		}
		for _, p := range patterns {
			p = strings.ToLower(p)
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}
