package janus

import (
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// Cold auth configuration (docs/20260728-160734-capability-auth.md).
// Site-scoped capability: global default → site override; a site
// auth { … } block replaces the global auth config (no merging of
// users or gates); bare auth / auth on inherits; explicit off beats
// an inherited on; built-in default off.

// Built-in defaults and fixed v1 parameters.
const (
	authDefaultTTL = 8 * time.Hour

	authCookieName     = "__Host-janus"
	authCSRFCookieName = "__Host-janus_csrf"
	authCSRFCookieAge  = 600 // seconds

	// Cheap caps enforced before any argon2 work.
	authBodyCap     = 64 << 10
	authUserCap     = 256
	authPasswordCap = 1024
	authToCap       = 2048

	// Login throttle ladder waits (after the Nth failed attempt).
	authThrottleWait4      = 10 * time.Second
	authThrottleWait5      = 60 * time.Second
	authThrottleWait6      = 300 * time.Second
	authThrottleBan        = 24 * time.Hour
	authThrottleBanAt      = 7 // fails >= 7 → 24h ban
	authThrottleMaxEntries = 65536

	// Argon2 verification concurrency: at most authVerifyConcurrency
	// KDF runs at once; at most authVerifyQueueMax more may wait behind
	// them. Past the queue depth a login attempt fast-fails 429 —
	// a distinct-pair spray can never stack 64 MiB allocations.
	authVerifyConcurrency = 2
	authVerifyQueueMax    = 16
)

// Password credential format (shared with Zift passhash): argon2id with
// parameters fixed as module constants under a version letter. Version
// "a" is `a` + 31 base62 chars (alphabet [0-9A-Za-z] only). Always 32
// characters. Raw payload is 23 bytes (8-byte salt, 15-byte key).
const (
	passPrefix   = "a"
	passMemory   = 64 * 1024 // KiB → 64 MiB
	passTime     = 2
	passThreads  = 1
	passSaltLen  = 8
	passKeyLen   = 15
	passRawLen   = passSaltLen + passKeyLen // 23
	passEncLen   = 31                       // base62 digits for 23 bytes
	passAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

// AuthUser is one cold-configured credential: a lowercased, header-safe
// username and its passhash blob.
type AuthUser struct {
	Name       string `json:"name"`
	Credential string `json:"credential"`
}

// AuthGate is one path-prefix allow list after normalization.
type AuthGate struct {
	Prefix string   `json:"prefix"`
	Allow  []string `json:"allow"`
}

// AuthSettings configures the site-scoped auth wall. It appears in the
// global janus options (default posture) and per site (override). A
// non-empty auth { … } block at the site replaces the global config
// wholesale. The full contract is docs/20260728-160734-capability-auth.md.
type AuthSettings struct {
	// Enabled turns the wall on or off for the site. Default: off.
	// Sites may override the global default; explicit off beats an
	// inherited on. Bare auth / auth on sets Enabled true without
	// Replace — inherit the global users and gates.
	Enabled *bool `json:"enabled,omitempty"`

	// Replace is true when a non-empty auth { … } block was parsed.
	// Site Replace configs supply the entire effective auth config
	// (users, gates, ttl) and do not merge with global.
	Replace bool `json:"replace,omitempty"`

	// Users is this level's credential table.
	Users []AuthUser `json:"users,omitempty"`

	// Gates is this level's path-prefix allow lists.
	Gates []AuthGate `json:"gates,omitempty"`

	// TTL is the sliding idle session timeout. Default: 8h.
	TTL *caddy.Duration `json:"ttl,omitempty"`
}

// authGate is one compiled gate: normalized prefix and allow-set.
type authGate struct {
	prefix string
	allow  map[string]bool
	door   string // exact login path: prefix + "auth"
}

// authSite is one site's effective auth configuration after cascade.
type authSite struct {
	users map[string]string // lowercased name → passhash blob
	gates []authGate        // longest-prefix-first for matching
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
//	auth {
//	  users { alice a… }
//	  user bob a…
//	  ttl 8h
//	  gate /one/ { alice larry }
//	}
//
// Hard errors per the capability contract.
func parseAuthDirective(d *caddyfile.Dispenser) (*AuthSettings, error) {
	as := &AuthSettings{}
	on, err := parseOnOff(d.RemainingArgs())
	if err != nil {
		return nil, d.Errf("auth: %v", err)
	}
	as.Enabled = &on

	nesting := d.Nesting()
	if !d.NextBlock(nesting) {
		// Bare auth / auth on / auth off, or empty auth { }.
		if d.Val() == "}" {
			if !on {
				return nil, d.Err("auth off does not take a block (a block on an off switch is a contradiction)")
			}
			return nil, d.Err("auth: empty auth block")
		}
		return as, nil
	}
	if !on {
		return nil, d.Err("auth off does not take a block (a block on an off switch is a contradiction)")
	}
	as.Replace = true

	seen := map[string]bool{}
	users := map[string]bool{}
	gatePrefixes := map[string]bool{}
	for {
		sub := d.Val()
		switch sub {
		case "user":
			args := d.RemainingArgs()
			if len(args) != 2 {
				return nil, d.Err("auth user: want exactly two arguments (user <name> <passhash>)")
			}
			if err := addAuthUser(as, users, args[0], args[1]); err != nil {
				return nil, d.Errf("auth user: %v", err)
			}
			if d.NextBlock(d.Nesting()) {
				return nil, d.Err("auth user does not take a nested block")
			}
		case "users":
			if seen["users"] {
				return nil, d.Err(`auth: duplicate subdirective "users"`)
			}
			seen["users"] = true
			if len(d.RemainingArgs()) != 0 {
				return nil, d.Err("auth users: want a block of `<name> <passhash>` lines, not arguments")
			}
			userNesting := d.Nesting()
			if !d.NextBlock(userNesting) {
				if d.Val() == "}" {
					return nil, d.Err("auth users: empty users block")
				}
				return nil, d.Err("auth users: want a block of `<name> <passhash>` lines")
			}
			for {
				nameTok := d.Val()
				args := d.RemainingArgs()
				if len(args) != 1 {
					return nil, d.Err("auth users: want `<name> <passhash>` per line")
				}
				if err := addAuthUser(as, users, nameTok, args[0]); err != nil {
					return nil, d.Errf("auth users: %v", err)
				}
				if d.NextBlock(d.Nesting()) {
					return nil, d.Err("auth users entries do not take a nested block")
				}
				if !d.NextBlock(userNesting) {
					break
				}
			}
		case "ttl":
			if seen["ttl"] {
				return nil, d.Err(`auth: duplicate subdirective "ttl"`)
			}
			seen["ttl"] = true
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
			if d.NextBlock(d.Nesting()) {
				return nil, d.Err("auth ttl does not take a nested block")
			}
		case "gate":
			args := d.RemainingArgs()
			if len(args) != 1 {
				return nil, d.Err("auth gate: want exactly one path argument and an allow-list block")
			}
			prefix, perr := normalizeGatePrefix(args[0])
			if perr != nil {
				return nil, d.Errf("auth gate: %v", perr)
			}
			if gatePrefixes[prefix] {
				return nil, d.Errf("auth gate: duplicate gate prefix %q after normalization", prefix)
			}
			gateNesting := d.Nesting()
			if !d.NextBlock(gateNesting) {
				if d.Val() == "}" {
					return nil, d.Errf("auth gate %s: empty allow list", prefix)
				}
				return nil, d.Errf("auth gate %s: want an allow-list block", prefix)
			}
			var allow []string
			allowSeen := map[string]bool{}
			for {
				// Allow-list tokens may sit on one line: { alice larry }.
				// NextArg does not stop at "}", so roll the closer back
				// for NextBlock to consume.
				names := []string{d.Val()}
				for d.NextArg() {
					if d.Val() == "}" {
						d.Prev()
						break
					}
					if d.Val() == "{" {
						return nil, d.Errf("auth gate %s: allow-list names do not take a nested block", prefix)
					}
					names = append(names, d.Val())
				}
				for _, raw := range names {
					name, verr := validateAuthUsername(raw)
					if verr != nil {
						return nil, d.Errf("auth gate %s: %v", prefix, verr)
					}
					if allowSeen[name] {
						return nil, d.Errf("auth gate %s: duplicate allow-list name %q", prefix, name)
					}
					allowSeen[name] = true
					allow = append(allow, name)
				}
				if !d.NextBlock(gateNesting) {
					break
				}
			}
			if len(allow) == 0 {
				return nil, d.Errf("auth gate %s: empty allow list", prefix)
			}
			gatePrefixes[prefix] = true
			as.Gates = append(as.Gates, AuthGate{Prefix: prefix, Allow: allow})
		default:
			return nil, d.Errf("unrecognized auth subdirective: %s", sub)
		}
		if !d.NextBlock(nesting) {
			break
		}
	}

	if err := finalizeAuthSettings(as); err != nil {
		return nil, d.Errf("auth: %v", err)
	}
	return as, nil
}

func addAuthUser(as *AuthSettings, seen map[string]bool, rawName, cred string) error {
	name, err := validateAuthUsername(rawName)
	if err != nil {
		return err
	}
	if seen[name] {
		return fmt.Errorf("duplicate username %q in one block", name)
	}
	if err := validatePasshash(cred); err != nil {
		return fmt.Errorf("user %s: %w", name, err)
	}
	seen[name] = true
	as.Users = append(as.Users, AuthUser{Name: name, Credential: cred})
	return nil
}

// normalizeGatePrefix validates and normalizes a gate path: must start
// with /, no // or .., and gains a trailing / when missing.
func normalizeGatePrefix(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("path must not be empty")
	}
	if !strings.HasPrefix(raw, "/") {
		return "", fmt.Errorf("path %q must start with /", raw)
	}
	if strings.Contains(raw, "//") {
		return "", fmt.Errorf("path %q must not contain //", raw)
	}
	if strings.Contains(raw, "..") {
		return "", fmt.Errorf("path %q must not contain dot-dot", raw)
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return "", fmt.Errorf("path %q must not contain whitespace", raw)
	}
	if !strings.HasSuffix(raw, "/") {
		raw += "/"
	}
	return raw, nil
}

// gateDoor is the exact login path for a normalized prefix.
func gateDoor(prefix string) string {
	return prefix + "auth"
}

// finalizeAuthSettings checks allow-list membership, login-door
// shadowing, and sorts nothing yet (provision sorts for match).
func finalizeAuthSettings(as *AuthSettings) error {
	if as == nil {
		return nil
	}
	userSet := map[string]bool{}
	for _, u := range as.Users {
		userSet[u.Name] = true
	}
	for i := range as.Gates {
		g := &as.Gates[i]
		prefix, err := normalizeGatePrefix(g.Prefix)
		if err != nil {
			return fmt.Errorf("gate: %w", err)
		}
		g.Prefix = prefix
		if len(g.Allow) == 0 {
			return fmt.Errorf("gate %s: empty allow list", prefix)
		}
		for j, raw := range g.Allow {
			name, err := validateAuthUsername(raw)
			if err != nil {
				return fmt.Errorf("gate %s: %w", prefix, err)
			}
			if !userSet[name] {
				return fmt.Errorf("gate %s: allow-list name %q is not present in users", prefix, name)
			}
			g.Allow[j] = name
		}
	}
	// Duplicate prefixes after normalization.
	seen := map[string]bool{}
	for _, g := range as.Gates {
		if seen[g.Prefix] {
			return fmt.Errorf("duplicate gate prefix %q after normalization", g.Prefix)
		}
		seen[g.Prefix] = true
	}
	if err := checkLoginDoorShadowing(as.Gates); err != nil {
		return err
	}
	return nil
}

// checkLoginDoorShadowing rejects configs where a gate's login door
// would be claimed by a different (longer) gate under longest-prefix
// rules — e.g. /one/ together with /one/auth/.
func checkLoginDoorShadowing(gates []AuthGate) error {
	for _, g := range gates {
		door := gateDoor(g.Prefix)
		winner := longestGatePrefix(gates, door)
		if winner != "" && winner != g.Prefix {
			return fmt.Errorf("gate %s: login door %s is shadowed by gate %s", g.Prefix, door, winner)
		}
	}
	return nil
}

// longestGatePrefix returns the normalized prefix of the longest gate
// that matches path, or "" if none match.
func longestGatePrefix(gates []AuthGate, path string) string {
	best := ""
	for _, g := range gates {
		if gatePathMatches(g.Prefix, path) && len(g.Prefix) > len(best) {
			best = g.Prefix
		}
	}
	return best
}

// gatePathMatches reports whether path is under the normalized prefix.
// For /one/: /one, /one/, /one/two match; /ones does not.
func gatePathMatches(prefix, path string) bool {
	if prefix == "/" {
		return true
	}
	// Fold case for the match only (never for the path handed upstream):
	// a case-insensitive backend would otherwise let /ONE/secret slip a
	// gate written /one/. Matching more can only over-gate, never expose.
	prefix = strings.ToLower(prefix)
	path = strings.ToLower(path)
	trimmed := strings.TrimSuffix(prefix, "/")
	if path == trimmed || path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix)
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

// validatePasshash checks a stored credential without decoding it into use:
// the version letter, base62 alphabet, and exact length. Malformed
// blobs reject at parse/provision, never at first login.
func validatePasshash(cred string) error {
	_, err := decodePasshash(cred)
	return err
}

func passEncode(raw []byte) string {
	if len(raw) != passRawLen {
		panic("passEncode: wrong raw length")
	}
	n := new(big.Int).SetBytes(raw)
	base := big.NewInt(62)
	chars := make([]byte, passEncLen)
	for i := passEncLen - 1; i >= 0; i-- {
		var r big.Int
		n.DivMod(n, base, &r)
		chars[i] = passAlphabet[r.Int64()]
	}
	return string(chars)
}

func passDecodePayload(rest string) ([]byte, error) {
	if len(rest) != passEncLen {
		return nil, fmt.Errorf("got %d chars, want %d", len(rest), passEncLen)
	}
	n := new(big.Int)
	base := big.NewInt(62)
	for i := 0; i < len(rest); i++ {
		v := strings.IndexByte(passAlphabet, rest[i])
		if v < 0 {
			return nil, fmt.Errorf("alphabet is [0-9A-Za-z]")
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(v)))
	}
	raw := n.Bytes()
	if len(raw) > passRawLen {
		return nil, fmt.Errorf("value out of range")
	}
	out := make([]byte, passRawLen)
	copy(out[passRawLen-len(raw):], raw)
	return out, nil
}

// decodePasshash decodes a version-a blob into its 23 raw bytes (salt‖digest).
func decodePasshash(cred string) ([]byte, error) {
	wantLen := len(passPrefix) + passEncLen
	if len(cred) == 0 {
		return nil, fmt.Errorf("credential is missing the a prefix")
	}
	if len(cred) != wantLen {
		// Future letter versions (`b`…). Digits/other lengths are
		// plain wrong-size, not a new version.
		if cred[0] >= 'b' && cred[0] <= 'z' {
			return nil, fmt.Errorf("credential has unknown version tag (want a)")
		}
		return nil, fmt.Errorf("credential is not a valid passhash (got %d chars, want %d)", len(cred), wantLen)
	}
	if !strings.HasPrefix(cred, passPrefix) {
		return nil, fmt.Errorf("credential has unknown version tag (want a)")
	}
	raw, err := passDecodePayload(cred[len(passPrefix):])
	if err != nil {
		return nil, fmt.Errorf("credential is not a valid passhash (%v)", err)
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
		if err := validatePasshash(as.Users[i].Credential); err != nil {
			return fmt.Errorf("user %s: %w", name, err)
		}
	}
	if as.TTL != nil && *as.TTL <= 0 {
		return fmt.Errorf("ttl must be a positive duration")
	}
	if as.Replace || len(as.Users) > 0 || len(as.Gates) > 0 {
		if err := finalizeAuthSettings(as); err != nil {
			return err
		}
	}
	return nil
}

// --- cascade -------------------------------------------------------------------

func (as *AuthSettings) replaces() bool {
	if as == nil {
		return false
	}
	return as.Replace || len(as.Users) > 0 || len(as.Gates) > 0 || as.TTL != nil
}

// provisionAuth resolves this site's effective auth configuration.
// Effective off leaves h.authCfg nil — the request-path check is one nil
// compare. Effective on with empty users or zero gates is a hard
// provision error. Cross-site work — removed-user session revocation,
// the site table — runs at App Start.
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
	if s != nil && s.Enabled != nil && !*s.Enabled {
		return nil
	}

	var src *AuthSettings
	switch {
	case s != nil && s.replaces():
		src = s
	default:
		// Site unset, or bare auth / auth on → inherit global.
		src = g
	}

	wantOn := false
	switch {
	case s != nil && s.replaces():
		wantOn = s.Enabled == nil || *s.Enabled
	case s != nil && s.Enabled != nil && *s.Enabled:
		// Bare auth / auth on: on only when global supplies a config.
		wantOn = true
	case s == nil:
		wantOn = g != nil && g.Enabled != nil && *g.Enabled
	}

	if !wantOn {
		return nil
	}
	if src == nil || (src.Enabled != nil && !*src.Enabled) {
		return fmt.Errorf("janus auth: this site's effective auth is on but there is no auth config to inherit" +
			" (bare 'auth'/'auth on' needs a global auth { users… gate… } block, or write a full site auth block)")
	}
	if len(src.Users) == 0 || len(src.Gates) == 0 {
		return fmt.Errorf("janus auth: this site's effective auth is on but its resolved config is incomplete" +
			" (need a non-empty users table and at least one gate)" +
			" — an enabled wall without credentials or gates is a lockout; add users and gates or 'auth off'")
	}
	if h.app == nil || h.app.state == nil {
		return nil // no data plane to guard (every request 404s already)
	}

	ttl := authDefaultTTL
	if src.TTL != nil {
		ttl = time.Duration(*src.TTL)
	}
	cfg, err := compileAuthSite(src.Users, src.Gates, ttl, h.app.state.auth)
	if err != nil {
		return fmt.Errorf("janus auth: %w", err)
	}
	h.authCfg = cfg
	return nil
}

func compileAuthSite(users []AuthUser, gates []AuthGate, ttl time.Duration, store *authStore) (*authSite, error) {
	umap := make(map[string]string, len(users))
	for _, u := range users {
		umap[u.Name] = u.Credential
	}
	compiled := make([]authGate, 0, len(gates))
	for _, g := range gates {
		allow := make(map[string]bool, len(g.Allow))
		for _, n := range g.Allow {
			allow[n] = true
		}
		compiled = append(compiled, authGate{
			prefix: g.Prefix,
			allow:  allow,
			door:   gateDoor(g.Prefix),
		})
	}
	// Longest prefix first so match walks in order.
	sort.Slice(compiled, func(i, j int) bool {
		return len(compiled[i].prefix) > len(compiled[j].prefix)
	})
	return &authSite{
		users: umap,
		gates: compiled,
		ttl:   ttl,
		store: store,
	}, nil
}

// matchGate returns the winning (longest) gate for path, or nil.
func (cfg *authSite) matchGate(path string) *authGate {
	for i := range cfg.gates {
		if gatePathMatches(cfg.gates[i].prefix, path) {
			return &cfg.gates[i]
		}
	}
	return nil
}

// doorGate returns the gate whose exact login door is path, or nil.
func (cfg *authSite) doorGate(path string) *authGate {
	for i := range cfg.gates {
		if cfg.gates[i].door == path {
			return &cfg.gates[i]
		}
	}
	return nil
}

// gatesForUser returns normalized prefixes whose allow lists include user.
func (cfg *authSite) gatesForUser(user string) []string {
	var out []string
	for _, g := range cfg.gates {
		if g.allow[user] {
			out = append(out, g.prefix)
		}
	}
	sort.Strings(out)
	return out
}

// --- the site table (App Start) -----------------------------------------------

// authSiteEntry pairs one compiled route's host patterns with the janus
// handler's effective auth configuration (nil when the site's effective
// auth is off).
type authSiteEntry struct {
	patterns []string // empty = the route matches every host on its server
	cfg      *authSite
}

// prepareAuth builds the generation-local site table during Start. It does
// not mutate the pooled store: Caddy can still reject another app afterward.
func (a *App) prepareAuth() error {
	if a.state == nil || a.state.auth == nil {
		return nil // no pooled state (native-config edge): nothing to reconcile
	}
	return a.buildAuthSiteTable()
}

// reconcileAuth applies irreversible pooled-store changes only after the new
// generation is known to be active. Per-request gate checks remain the
// security boundary while this prompt cleanup waits for Caddy's commit.
func (a *App) reconcileAuth() {
	if a.state == nil || a.state.auth == nil {
		return
	}
	maxTTL := authDefaultTTL
	enabled := false
	for _, e := range a.authSites {
		if e.cfg == nil {
			continue
		}
		enabled = true
		if e.cfg.ttl > maxTTL {
			maxTTL = e.cfg.ttl
		}
	}
	st := a.state.auth
	st.reapTTL.Store(int64(maxTTL))
	st.mu.Lock()
	revoked := 0
	for key, session := range st.sessions {
		if !a.authSessionAllowed(session.user, session.host) {
			delete(st.sessions, key)
			revoked++
		}
	}
	st.mu.Unlock()
	st.reloadRevoked.Add(int64(revoked))
	if revoked > 0 {
		a.logger.Warn("janus auth: sessions revoked at reload (user absent from the minting host's enabled site)",
			zap.Int("revoked", revoked),
			zap.Bool("auth_enabled_anywhere", enabled),
		)
	}
}

// authSessionAllowed retains a host-bound session only when its user remains
// in at least one enabled auth site that matches the minting host. A same-name
// user on another host must not keep an unreachable stale session alive.
func (a *App) authSessionAllowed(user, host string) bool {
	host = strings.ToLower(host)
	for _, e := range a.authSites {
		if e.cfg == nil {
			continue
		}
		if _, ok := e.cfg.users[user]; !ok {
			continue
		}
		if len(e.patterns) == 0 {
			return true
		}
		for _, pattern := range e.patterns {
			if hostMatchesPattern(strings.ToLower(pattern), host) {
				return true
			}
		}
	}
	return false
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
				v.authExactHosts = make(map[string]struct{}, len(routeHosts))
				for _, host := range routeHosts {
					host = strings.ToLower(host)
					if !strings.Contains(host, "*") {
						v.authExactHosts[host] = struct{}{}
					}
				}
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

// authCfgForHost returns the effective authSite for host, or nil.
func (a *App) authCfgForHost(host string) *authSite {
	host = strings.ToLower(host)
	for _, e := range a.authSites {
		if e.cfg == nil {
			continue
		}
		if len(e.patterns) == 0 {
			return e.cfg
		}
		for _, p := range e.patterns {
			if hostMatchesPattern(strings.ToLower(p), host) {
				return e.cfg
			}
		}
	}
	return nil
}
