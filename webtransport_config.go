package janus

// webtransport (capability 10): the cold configuration. The global block
// enables the process-wide relay listener and tunes it; a site route names
// the path a browser dials and the UDP target it reaches. The contract is
// docs/20260928-200621-capability-webtransport.md.

import (
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

const (
	wtDefaultMaxSessions   = 256
	wtDefaultMaxDatagram   = 1200
	wtMaxDatagramCeiling   = 1200 // fits quic-go's first-flight budget; see the contract's Size paragraph
	wtMinDatagram          = 64
	wtDefaultIdleTimeout   = 60 * time.Second
	wtMinIdleTimeout       = 30 * time.Second
	wtDefaultRouteSessions = 4
	wtMaxSessionsCeiling   = 65535
)

// WebtransportSettings is the global `webtransport { … }` block. Presence
// enables the listener; every option has a default.
type WebtransportSettings struct {
	// MaxSessions is the process-wide session ceiling (default 256).
	MaxSessions *int `json:"max_sessions,omitempty"`
	// MaxDatagram is the largest payload relayed in either direction
	// (default and ceiling 1200 bytes).
	MaxDatagram *int `json:"max_datagram,omitempty"`
	// IdleTimeout is the QUIC max idle timeout (default 60s, minimum 30s).
	IdleTimeout caddy.Duration `json:"idle_timeout,omitempty"`
}

func (s *WebtransportSettings) maxSessions() int {
	if s.MaxSessions != nil {
		return *s.MaxSessions
	}
	return wtDefaultMaxSessions
}

func (s *WebtransportSettings) maxDatagram() int {
	if s.MaxDatagram != nil {
		return *s.MaxDatagram
	}
	return wtDefaultMaxDatagram
}

func (s *WebtransportSettings) idleTimeout() time.Duration {
	if s.IdleTimeout != 0 {
		return time.Duration(s.IdleTimeout)
	}
	return wtDefaultIdleTimeout
}

// WebtransportRoute is one site route: `webtransport <path> <target> { … }`.
type WebtransportRoute struct {
	// Path is the absolute, exact path a browser dials.
	Path string `json:"path"`
	// Target is the fixed UDP address, `udp/<ip>:<port>` with an IP literal.
	Target string `json:"target"`
	// Origin is the browser Origin policy: `same` (default) and/or hostnames;
	// `any` is not legal for a relay.
	Origin []string `json:"origin,omitempty"`
	// MaxSessions is the per-route session ceiling (default 4).
	MaxSessions *int `json:"max_sessions,omitempty"`
}

func (r *WebtransportRoute) maxSessions() int {
	if r.MaxSessions != nil {
		return *r.MaxSessions
	}
	return wtDefaultRouteSessions
}

// targetAddr parses the validated target.
func (r *WebtransportRoute) targetAddr() netip.AddrPort {
	ap, _ := parseWebtransportTarget(r.Target)
	return ap
}

// parseWebtransportGlobal parses the global block. It takes no arguments.
func parseWebtransportGlobal(d *caddyfile.Dispenser) (*WebtransportSettings, error) {
	if len(d.RemainingArgs()) > 0 {
		return nil, d.Err("webtransport: the global block takes no arguments (routes belong to site blocks)")
	}
	ws := &WebtransportSettings{}
	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("webtransport: duplicate option %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "max_sessions", "max_datagram":
			val, err := oneDirectiveArg(d, "webtransport", sub)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(val)
			if perr != nil {
				return nil, d.Errf("webtransport %s: want an integer, got %q", sub, val)
			}
			if sub == "max_sessions" {
				if n < 1 || n > wtMaxSessionsCeiling {
					return nil, d.Errf("webtransport max_sessions: want 1 through %d, got %d", wtMaxSessionsCeiling, n)
				}
				ws.MaxSessions = &n
			} else {
				if n < wtMinDatagram || n > wtMaxDatagramCeiling {
					return nil, d.Errf("webtransport max_datagram: want %d through %d bytes, got %d", wtMinDatagram, wtMaxDatagramCeiling, n)
				}
				ws.MaxDatagram = &n
			}
		case "idle_timeout":
			val, err := oneDirectiveArg(d, "webtransport", sub)
			if err != nil {
				return nil, err
			}
			dur, perr := caddy.ParseDuration(val)
			if perr != nil {
				return nil, d.Errf("webtransport idle_timeout: want a duration, got %q", val)
			}
			if dur < wtMinIdleTimeout {
				return nil, d.Errf("webtransport idle_timeout: want at least %v, got %v", wtMinIdleTimeout, dur)
			}
			ws.IdleTimeout = caddy.Duration(dur)
		default:
			return nil, d.Errf("unrecognized webtransport option: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("webtransport %s does not take a nested block", sub)
		}
	}
	return ws, nil
}

// parseWebtransportRoute parses one site route. The site-level word is
// never a block on its own: a path and a target are the route.
func parseWebtransportRoute(d *caddyfile.Dispenser) (*WebtransportRoute, error) {
	args := d.RemainingArgs()
	if len(args) != 2 {
		return nil, d.Err("webtransport: want `webtransport <path> udp/<ip>:<port>`; the listener itself is configured in the global janus block")
	}
	if err := validateWebtransportPath(args[0]); err != nil {
		return nil, d.Errf("webtransport path: %v", err)
	}
	if _, err := parseWebtransportTarget(args[1]); err != nil {
		return nil, d.Errf("webtransport target: %v", err)
	}
	route := &WebtransportRoute{Path: args[0], Target: args[1]}
	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("webtransport: duplicate option %q", sub)
		}
		seen[sub] = true
		switch sub {
		case "origin":
			vals := d.RemainingArgs()
			if err := validateWebtransportOrigin(vals); err != nil {
				return nil, d.Errf("webtransport origin: %v", err)
			}
			route.Origin = vals
		case "max_sessions":
			val, err := oneDirectiveArg(d, "webtransport", sub)
			if err != nil {
				return nil, err
			}
			n, perr := strconv.Atoi(val)
			if perr != nil || n < 1 || n > wtMaxSessionsCeiling {
				return nil, d.Errf("webtransport max_sessions: want 1 through %d, got %q", wtMaxSessionsCeiling, val)
			}
			route.MaxSessions = &n
		default:
			return nil, d.Errf("unrecognized webtransport route option: %s", sub)
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("webtransport %s does not take a nested block", sub)
		}
	}
	return route, nil
}

// validateWebtransportPath accepts an absolute, exact path with no query,
// fragment, whitespace, or control characters.
func validateWebtransportPath(p string) error {
	if err := validateHubPath(p); err != nil {
		return err
	}
	if strings.Contains(p, "//") || strings.HasSuffix(p, "/") && p != "/" {
		return fmt.Errorf("path %q must be exact: no empty segment and no trailing slash", p)
	}
	return nil
}

// parseWebtransportTarget accepts `udp/<addrport>`: an IPv4 literal or a
// bracketed IPv6 literal with a nonzero port, never a name, and never an
// address that cannot be a connected socket's peer.
func parseWebtransportTarget(s string) (netip.AddrPort, error) {
	rest, ok := strings.CutPrefix(s, "udp/")
	if !ok {
		return netip.AddrPort{}, fmt.Errorf("want udp/<ip>:<port>, got %q", s)
	}
	ap, err := netip.ParseAddrPort(rest)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("want udp/<ip>:<port> with an IP literal (an IPv6 literal in brackets), got %q", s)
	}
	a := ap.Addr()
	switch {
	case ap.Port() == 0:
		return netip.AddrPort{}, fmt.Errorf("port must be nonzero in %q", s)
	case a.Zone() != "":
		return netip.AddrPort{}, fmt.Errorf("a zone-scoped address is not a relay target: %q", s)
	case a.IsUnspecified():
		return netip.AddrPort{}, fmt.Errorf("an unspecified address is not a relay target: %q", s)
	case a.IsMulticast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast():
		return netip.AddrPort{}, fmt.Errorf("a multicast address is not a relay target: %q", s)
	case a.Is4() && a == netip.MustParseAddr("255.255.255.255"):
		return netip.AddrPort{}, fmt.Errorf("the broadcast address is not a relay target: %q", s)
	}
	return ap, nil
}

// validateWebtransportOrigin reuses the hub's token grammar and refuses
// `any`: a relay toward a fixed target that any page could drive is an
// open relay.
func validateWebtransportOrigin(vals []string) error {
	if len(vals) == 0 {
		return fmt.Errorf("want same or one or more hostnames")
	}
	for _, v := range vals {
		if strings.EqualFold(v, "any") {
			return fmt.Errorf("`any` is not legal for a relay; name the hosts or use same")
		}
	}
	return validateHubOrigin(vals)
}

// wtOriginAllowed applies a route's policy the way the hub does: the
// Origin header must be present, and its hostname must equal the request
// host (`same`) or be named. Scheme and port are not compared.
func wtOriginAllowed(policy []string, origin, host string) bool {
	if origin == "" {
		return false
	}
	oh := originHostname(origin)
	if oh == "" {
		return false
	}
	if len(policy) == 0 {
		return oh == host
	}
	for _, p := range policy {
		if strings.EqualFold(p, "same") {
			if oh == host {
				return true
			}
			continue
		}
		if strings.EqualFold(p, oh) {
			return true
		}
	}
	return false
}

// originHostname is the lowercase hostname of an Origin header value, ""
// when it does not parse as a URL with a host.
func originHostname(origin string) string {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
