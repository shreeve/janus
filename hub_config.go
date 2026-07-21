package janus

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/dustin/go-humanize"
)

// Cold hub configuration (docs/20260720-162350-hub-design.md "Cold
// config"). Site-scoped capability, the ping/cache pattern: global
// default → site override; tuning subdirectives cascade per key; built-in
// default off. There are no process-wide-only keys — every hub knob is
// per-site admission policy.

// Built-in defaults.
const (
	hubDefaultPath        = "/hub"
	hubDefaultMaxConns    = 4096
	hubDefaultMaxFrame    = int64(64 << 10)
	hubDefaultMaxChannels = 128
	hubMinMaxFrame        = int64(1 << 10)
)

// HubSettings is the cold `hub` directive, in the global janus block
// (default) and in site janus blocks (override).
type HubSettings struct {
	Enabled     *bool    `json:"enabled,omitempty"`
	Path        *string  `json:"path,omitempty"`
	MaxConns    *int     `json:"max_conns,omitempty"`
	MaxFrame    *int64   `json:"max_frame,omitempty"`
	MaxChannels *int     `json:"max_channels,omitempty"`
	Origin      []string `json:"origin,omitempty"`
}

// hubSite is one site's effective hub configuration after cascade.
type hubSite struct {
	path        string
	maxConns    int
	maxFrame    int64
	maxChannels int

	originAny   bool
	originSame  bool
	originHosts map[string]bool
}

// parseHubDirective parses one hub directive:
//
//	hub
//	hub on
//	hub off
//	hub { path /realtime; max_conns 4096; max_frame 64kb; max_channels 128; origin same … }
//	hub on { … }
//
// Hard errors per the capability contract: unknown argument, a block on
// "off", unknown or duplicate subdirectives, invalid path/sizes/counts,
// illegal origin combinations, nested blocks.
func parseHubDirective(d *caddyfile.Dispenser) (*HubSettings, error) {
	hs := &HubSettings{}
	on := true
	args := d.RemainingArgs()
	switch len(args) {
	case 0:
	case 1:
		switch args[0] {
		case "on":
		case "off":
			on = false
		default:
			return nil, d.Errf(`hub: want "on" or "off", got %q`, args[0])
		}
	default:
		return nil, d.Err(`hub: want at most one of "on" or "off"`)
	}
	hs.Enabled = &on

	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		if !on {
			return nil, d.Err("hub off does not take a block (a block on an off switch is a contradiction)")
		}
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("hub: duplicate subdirective %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "path":
			val, err := oneHubArg(d, sub)
			if err != nil {
				return nil, err
			}
			if err := validateHubPath(val); err != nil {
				return nil, d.Errf("hub path: %v", err)
			}
			hs.Path = &val
		case "max_conns", "max_channels":
			val, err := oneHubArg(d, sub)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 1 {
				return nil, d.Errf("hub %s: want a positive integer, got %q", sub, val)
			}
			if sub == "max_conns" {
				hs.MaxConns = &n
			} else {
				hs.MaxChannels = &n
			}
		case "max_frame":
			val, err := oneHubArg(d, sub)
			if err != nil {
				return nil, err
			}
			n, perr := humanize.ParseBytes(val)
			if perr != nil || int64(n) < hubMinMaxFrame {
				return nil, d.Errf("hub max_frame: want a size of at least 1kb, got %q", val)
			}
			v := int64(n)
			hs.MaxFrame = &v
		case "origin":
			vals := d.RemainingArgs()
			if len(vals) == 0 {
				return nil, d.Err("hub origin: want same, any, or one or more hostnames")
			}
			if err := validateHubOrigin(vals); err != nil {
				return nil, d.Errf("hub origin: %v", err)
			}
			hs.Origin = vals
		default:
			return nil, d.Errf("unrecognized hub subdirective: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("hub %s does not take a nested block", sub)
		}
	}
	return hs, nil
}

func oneHubArg(d *caddyfile.Dispenser, sub string) (string, error) {
	args := d.RemainingArgs()
	if len(args) != 1 {
		return "", d.Errf("hub %s: want exactly one argument", sub)
	}
	return args[0], nil
}

func validateHubPath(p string) error {
	if p == "" || !strings.HasPrefix(p, "/") {
		return fmt.Errorf("want a /-prefixed path, got %q", p)
	}
	if len(p) > 256 {
		return fmt.Errorf("path %q is too long (max 256 bytes)", p)
	}
	for _, r := range p {
		if r == '?' || r == '#' {
			return fmt.Errorf("path %q must not contain %q (it is a path, not a URL)", p, string(r))
		}
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("path %q must not contain whitespace or control characters", p)
		}
	}
	return nil
}

// validateHubOrigin checks the origin token list: `any` is total; `same`
// may combine with allowlisted hosts; every other token must be a
// plausible hostname (registry host validation reused).
func validateHubOrigin(vals []string) error {
	for i, v := range vals {
		switch v {
		case "any":
			if len(vals) != 1 {
				return fmt.Errorf(`"any" is total and cannot combine with anything else`)
			}
		case "same":
			if i != 0 {
				return fmt.Errorf(`"same" must come first`)
			}
		default:
			if err := validateHostname(strings.ToLower(v)); err != nil {
				return fmt.Errorf("%q is not a plausible hostname", v)
			}
		}
	}
	return nil
}

// --- cascade -------------------------------------------------------------------

func hubEnabledPtr(hs *HubSettings) *bool {
	if hs == nil {
		return nil
	}
	return hs.Enabled
}

// provisionHub resolves this site's effective hub configuration.
// Effective off leaves h.hubCfg nil — the request-path check is one nil
// compare.
func (h *Handler) provisionHub() error {
	var g *HubSettings
	if h.app != nil {
		g = h.app.Hub
	}
	s := h.Hub
	for _, hs := range []*HubSettings{s, g} {
		if hs == nil {
			continue
		}
		if hs.Path != nil {
			if err := validateHubPath(*hs.Path); err != nil {
				return fmt.Errorf("janus hub path: %w", err)
			}
		}
		if hs.MaxConns != nil && *hs.MaxConns < 1 {
			return fmt.Errorf("janus hub: max_conns must be a positive integer, got %d", *hs.MaxConns)
		}
		if hs.MaxChannels != nil && *hs.MaxChannels < 1 {
			return fmt.Errorf("janus hub: max_channels must be a positive integer, got %d", *hs.MaxChannels)
		}
		if hs.MaxFrame != nil && *hs.MaxFrame < hubMinMaxFrame {
			return fmt.Errorf("janus hub: max_frame must be at least 1kb, got %d", *hs.MaxFrame)
		}
		if len(hs.Origin) > 0 {
			if err := validateHubOrigin(hs.Origin); err != nil {
				return fmt.Errorf("janus hub origin: %w", err)
			}
		}
	}
	if !cascadeBoolPtr(hubEnabledPtr(s), hubEnabledPtr(g), false) {
		return nil
	}
	if h.app == nil || h.dp == nil {
		return nil // no registry/data plane to admit into
	}

	cfg := &hubSite{
		path:        hubDefaultPath,
		maxConns:    hubDefaultMaxConns,
		maxFrame:    hubDefaultMaxFrame,
		maxChannels: hubDefaultMaxChannels,
		originSame:  true,
		originHosts: map[string]bool{},
	}
	pick := func(site, global *HubSettings) {
		for _, hs := range []*HubSettings{global, site} { // site overrides last
			if hs == nil {
				continue
			}
			if hs.Path != nil {
				cfg.path = *hs.Path
			}
			if hs.MaxConns != nil {
				cfg.maxConns = *hs.MaxConns
			}
			if hs.MaxFrame != nil {
				cfg.maxFrame = *hs.MaxFrame
			}
			if hs.MaxChannels != nil {
				cfg.maxChannels = *hs.MaxChannels
			}
			if len(hs.Origin) > 0 {
				cfg.originAny = false
				cfg.originSame = false
				cfg.originHosts = map[string]bool{}
				for _, v := range hs.Origin {
					switch v {
					case "any":
						cfg.originAny = true
					case "same":
						cfg.originSame = true
					default:
						cfg.originHosts[strings.ToLower(v)] = true
					}
				}
			}
		}
	}
	pick(s, g)
	h.hubCfg = cfg
	return nil
}

// --- the host → site table ------------------------------------------------------

// hubSiteEntry pairs one compiled route's host patterns with the janus
// handler's effective hub configuration (nil when the site's effective hub
// is off). Entries preserve route order, which Caddy has already sorted by
// specificity — matching a host walks entries first-match, exactly as a
// request would route.
type hubSiteEntry struct {
	patterns []string // empty = the route matches every host on its server
	cfg      *hubSite
}

// buildHubSiteTable walks the provisioned HTTP app's servers and routes,
// pairing each janus site with its host matchers. Called from App.Start,
// after every handler is provisioned.
func (a *App) buildHubSiteTable() error {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil || httpAppI == nil {
		return nil // no HTTP app: no sites, no hub admission
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return fmt.Errorf("janus hub: http app is unexpected type %T", httpAppI)
	}
	var servers [][]hubSiteEntry
	for _, srv := range ha.Servers {
		var entries []hubSiteEntry
		collectHubRoutes(srv.Routes, nil, &entries)
		if len(entries) > 0 {
			servers = append(servers, entries)
		}
	}
	a.hubSites = servers
	return nil
}

// collectHubRoutes recursively walks a route list (subroutes carry the
// site's directive routes under the host-matched wrapper route).
func collectHubRoutes(routes caddyhttp.RouteList, hosts []string, entries *[]hubSiteEntry) {
	for _, route := range routes {
		routeHosts := hosts
		if h := hostsFromMatcherSets(route.MatcherSets); len(h) > 0 {
			routeHosts = h
		}
		for _, handler := range route.Handlers {
			switch v := handler.(type) {
			case *Handler:
				*entries = append(*entries, hubSiteEntry{patterns: routeHosts, cfg: v.hubCfg})
			case *caddyhttp.Subroute:
				collectHubRoutes(v.Routes, routeHosts, entries)
			}
		}
	}
}

// hostsFromMatcherSets extracts host patterns from a route's provisioned
// matcher sets (the raw JSON is zeroed after module loading; the decoded
// MatchHost values carry the patterns).
func hostsFromMatcherSets(sets caddyhttp.MatcherSets) []string {
	var out []string
	for _, set := range sets {
		for _, m := range set {
			switch mh := m.(type) {
			case *caddyhttp.MatchHost:
				out = append(out, *mh...)
			case caddyhttp.MatchHost:
				out = append(out, mh...)
			}
		}
	}
	return out
}

// hubSiteFor resolves one hostname to its effective hub configuration:
// first matching entry per server (mirroring request routing), preferring
// a hub-enabled match across servers.
func (a *App) hubSiteFor(host string) *hubSite {
	var found *hubSite
	for _, entries := range a.hubSites {
		for _, e := range entries {
			if !entryMatchesHost(e, host) {
				continue
			}
			if e.cfg != nil {
				if found == nil || e.cfg.maxConns < found.maxConns {
					found = e.cfg
				}
			}
			break // first match per server, like request routing
		}
	}
	return found
}

func entryMatchesHost(e hubSiteEntry, host string) bool {
	if len(e.patterns) == 0 {
		return true // catch-all route on its server
	}
	for _, p := range e.patterns {
		if hostMatchesPattern(strings.ToLower(p), host) {
			return true
		}
	}
	return false
}

// hostMatchesPattern mirrors caddyhttp.MatchHost's semantics for the
// pattern shapes Janus configs use: exact hostnames and per-label "*"
// wildcards ("*.ripdev.io" matches one label).
func hostMatchesPattern(pattern, host string) bool {
	if pattern == host {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	pl := strings.Split(pattern, ".")
	hl := strings.Split(host, ".")
	if len(pl) != len(hl) {
		return false
	}
	for i := range pl {
		if pl[i] != "*" && pl[i] != hl[i] {
			return false
		}
	}
	return true
}

// hubMaxConnsFloor resolves the app's admission cap: the minimum effective
// max_conns across every hub-enabled host claimed by the app. Every
// admission checks this one floor regardless of the arriving host.
func (a *App) hubMaxConnsFloor(rec AppRecord) (int, bool) {
	floor := 0
	enabled := false
	for _, host := range rec.Hosts {
		cfg := a.hubSiteFor(host)
		if cfg == nil {
			continue
		}
		if !enabled || cfg.maxConns < floor {
			floor = cfg.maxConns
		}
		enabled = true
	}
	return floor, enabled
}

// hubEnabledAnywhere reports whether any of the app's claimed hosts is
// served by a hub-enabled site (the publish plane's 409 gate).
func (a *App) hubEnabledAnywhere(rec AppRecord) bool {
	_, enabled := a.hubMaxConnsFloor(rec)
	return enabled
}

// closeDisabledHubHosts closes connections whose bound host no longer
// resolves to a hub-enabled site after a Caddy config reload ("a host
// whose effective Hub capability flips to off stops intercepting upgrades
// and closes sockets bound through that host").
func (a *App) closeDisabledHubHosts() {
	for _, h := range a.hubs.snapshotAll() {
		h.mu.Lock()
		var victims []*hubConn
		for _, c := range h.conns {
			if a.hubSiteFor(c.host) == nil {
				victims = append(victims, c)
			}
		}
		h.mu.Unlock()
		for _, c := range victims {
			c.closeWith(hubCloseGoingAway, "hub disabled")
		}
	}
}
