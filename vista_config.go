package janus

// Capability 11, vista: the Caddyfile grammar of a site's routes, their
// defaults and legal ranges, and the provision-time checks that need the
// site's other capabilities (docs/20261010-232539-capability-vista.md).

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
)

const (
	vistaDefaultMaxSessions       = 256
	vistaDefaultMaxUnsigned       = 32
	vistaDefaultPerClient         = 64
	vistaDefaultPerClientUnsigned = 8
	vistaDefaultSignonTimeout     = 180 * time.Second
	vistaDefaultRPCTimeout        = 10 * time.Minute
	vistaDefaultQueue             = 8
	vistaDefaultMaxRequest        = 1 << 20
	vistaDefaultMaxReply          = 64 << 20

	vistaMaxSessionsCeiling = 65535
	vistaMinSignonTimeout   = 10 * time.Second
	vistaMaxSignonTimeout   = 10 * time.Minute
	vistaMinRPCTimeout      = 10 * time.Second
	vistaMaxRPCTimeout      = time.Hour
	vistaMaxQueue           = 64
	vistaMinRequest         = 1 << 10
	vistaMaxRequest         = 16 << 20
	vistaMinReply           = 1 << 20
	vistaMaxReply           = 1 << 30
	// vistaMaxSocketPath is the shorter of the platforms' sun_path limits,
	// less the terminating NUL: a longer path cannot be dialed.
	vistaMaxSocketPath = 103
)

// VistaRoute is one site's route: a path clients post to and the Unix
// socket whose service manager starts one em per connection.
type VistaRoute struct {
	// Path is the exact, canonical request path (`/rpc`).
	Path string `json:"path"`
	// Target is a Caddy network address with network unix.
	Target string `json:"target"`

	MaxSessions       *int            `json:"max_sessions,omitempty"`
	MaxUnsigned       *int            `json:"max_unsigned,omitempty"`
	PerClient         *int            `json:"per_client,omitempty"`
	PerClientUnsigned *int            `json:"per_client_unsigned,omitempty"`
	PerClientExempt   []string        `json:"per_client_exempt,omitempty"`
	SignonTimeout     *caddy.Duration `json:"signon_timeout,omitempty"`
	RPCTimeout        *caddy.Duration `json:"rpc_timeout,omitempty"`
	Queue             *int            `json:"queue,omitempty"`
	MaxRequest        *int64          `json:"max_request,omitempty"`
	MaxReply          *int64          `json:"max_reply,omitempty"`
	// Origin is `same` (the default) or a list of https:// origins.
	Origin []string `json:"origin,omitempty"`
	// AuthExempt is the route's stance under a covering site auth gate.
	AuthExempt bool `json:"auth_exempt,omitempty"`
}

// vistaLimits is a route's effective configuration, resolved once at
// provision and read by every request the handler serves.
type vistaLimits struct {
	socket            string
	maxSessions       int
	maxUnsigned       int
	perClient         int
	perClientUnsigned int
	perClientExempt   []netip.Prefix
	signonTimeout     time.Duration
	rpcTimeout        time.Duration
	queue             int
	maxRequest        int64
	maxReply          int64
	origins           []string // normalized origins; empty means same only
	originSame        bool
	gated             bool // an exact site gate covers the route
}

func intOr(p *int, def int) int {
	if p != nil {
		return *p
	}
	return def
}

func durOr(p *caddy.Duration, def time.Duration) time.Duration {
	if p != nil {
		return time.Duration(*p)
	}
	return def
}

func int64Or(p *int64, def int64) int64 {
	if p != nil {
		return *p
	}
	return def
}

// limits resolves the route's defaults and checks the cross-option rules
// a single option's range cannot.
func (r *VistaRoute) limits() (*vistaLimits, error) {
	socket, err := parseVistaTarget(r.Target)
	if err != nil {
		return nil, err
	}
	l := &vistaLimits{
		socket:            socket,
		maxSessions:       intOr(r.MaxSessions, vistaDefaultMaxSessions),
		maxUnsigned:       intOr(r.MaxUnsigned, vistaDefaultMaxUnsigned),
		perClient:         intOr(r.PerClient, vistaDefaultPerClient),
		perClientUnsigned: intOr(r.PerClientUnsigned, vistaDefaultPerClientUnsigned),
		signonTimeout:     durOr(r.SignonTimeout, vistaDefaultSignonTimeout),
		rpcTimeout:        durOr(r.RPCTimeout, vistaDefaultRPCTimeout),
		queue:             intOr(r.Queue, vistaDefaultQueue),
		maxRequest:        int64Or(r.MaxRequest, vistaDefaultMaxRequest),
		maxReply:          int64Or(r.MaxReply, vistaDefaultMaxReply),
	}
	// Defaults scale down with a lowered max_sessions, so `max_sessions 4`
	// alone is legal; an explicit value above its cap is an error.
	if r.MaxUnsigned == nil && l.maxUnsigned > l.maxSessions {
		l.maxUnsigned = l.maxSessions
	}
	if r.PerClient == nil && l.perClient > l.maxSessions {
		l.perClient = l.maxSessions
	}
	if r.PerClientUnsigned == nil && l.perClientUnsigned > l.maxUnsigned {
		l.perClientUnsigned = l.maxUnsigned
	}
	switch {
	case l.maxUnsigned > l.maxSessions:
		return nil, fmt.Errorf("max_unsigned %d exceeds max_sessions %d", l.maxUnsigned, l.maxSessions)
	case l.perClient > l.maxSessions:
		return nil, fmt.Errorf("per_client %d exceeds max_sessions %d", l.perClient, l.maxSessions)
	case l.perClientUnsigned > l.maxUnsigned:
		return nil, fmt.Errorf("per_client_unsigned %d exceeds max_unsigned %d", l.perClientUnsigned, l.maxUnsigned)
	}
	for _, c := range r.PerClientExempt {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return nil, fmt.Errorf("per_client_exempt: %q is not a CIDR", c)
		}
		l.perClientExempt = append(l.perClientExempt, p.Masked())
	}
	if len(r.Origin) == 0 {
		l.originSame = true
	}
	for _, o := range r.Origin {
		if o == "same" {
			l.originSame = true
			continue
		}
		n, err := normalizeVistaOrigin(o)
		if err != nil {
			return nil, err
		}
		l.origins = append(l.origins, n)
	}
	return l, nil
}

// parseVistaRoute parses `vista <path> <target> [{ … }]`.
func parseVistaRoute(d *caddyfile.Dispenser) (*VistaRoute, error) {
	args := d.RemainingArgs()
	if len(args) != 2 {
		return nil, d.Err("vista: want `vista <path> unix/<socket>`")
	}
	if err := validateVistaPath(args[0]); err != nil {
		return nil, d.Errf("vista path: %v", err)
	}
	if _, err := parseVistaTarget(args[1]); err != nil {
		return nil, d.Errf("vista target: %v", err)
	}
	route := &VistaRoute{Path: args[0], Target: args[1]}
	seen := map[string]bool{}
	for nesting := d.Nesting(); d.NextBlock(nesting); {
		sub := d.Val()
		if seen[sub] {
			return nil, d.Errf("vista: duplicate option %q", sub)
		}
		seen[sub] = true
		var err error
		switch sub {
		case "max_sessions":
			route.MaxSessions, err = vistaIntArg(d, sub, 1, vistaMaxSessionsCeiling)
		case "max_unsigned":
			route.MaxUnsigned, err = vistaIntArg(d, sub, 1, vistaMaxSessionsCeiling)
		case "per_client":
			route.PerClient, err = vistaIntArg(d, sub, 1, vistaMaxSessionsCeiling)
		case "per_client_unsigned":
			route.PerClientUnsigned, err = vistaIntArg(d, sub, 1, vistaMaxSessionsCeiling)
		case "queue":
			route.Queue, err = vistaIntArg(d, sub, 1, vistaMaxQueue)
		case "signon_timeout":
			route.SignonTimeout, err = vistaDurationArg(d, sub, vistaMinSignonTimeout, vistaMaxSignonTimeout)
		case "rpc_timeout":
			route.RPCTimeout, err = vistaDurationArg(d, sub, vistaMinRPCTimeout, vistaMaxRPCTimeout)
		case "max_request":
			route.MaxRequest, err = vistaSizeArg(d, sub, vistaMinRequest, vistaMaxRequest)
		case "max_reply":
			route.MaxReply, err = vistaSizeArg(d, sub, vistaMinReply, vistaMaxReply)
		case "per_client_exempt":
			vals := d.RemainingArgs()
			if len(vals) == 0 {
				return nil, d.Err("vista per_client_exempt: want one or more CIDRs")
			}
			for _, v := range vals {
				if _, perr := netip.ParsePrefix(v); perr != nil {
					return nil, d.Errf("vista per_client_exempt: %q is not a CIDR", v)
				}
			}
			route.PerClientExempt = vals
		case "origin":
			vals := d.RemainingArgs()
			if err := validateVistaOrigins(vals); err != nil {
				return nil, d.Errf("vista origin: %v", err)
			}
			route.Origin = vals
		case "auth":
			vals := d.RemainingArgs()
			if len(vals) != 1 || vals[0] != "exempt" {
				return nil, d.Err("vista auth: the only form is `auth exempt`")
			}
			route.AuthExempt = true
		default:
			return nil, d.Errf("unrecognized vista route option: %s", sub)
		}
		if err != nil {
			return nil, err
		}
		if d.NextBlock(d.Nesting()) {
			return nil, d.Errf("vista %s does not take a nested block", sub)
		}
	}
	if _, err := route.limits(); err != nil {
		return nil, d.Errf("vista: %v", err)
	}
	return route, nil
}

func vistaIntArg(d *caddyfile.Dispenser, sub string, lo, hi int) (*int, error) {
	val, err := oneDirectiveArg(d, "vista", sub)
	if err != nil {
		return nil, err
	}
	n, perr := strconv.Atoi(val)
	if perr != nil || n < lo || n > hi {
		return nil, d.Errf("vista %s: want %d through %d, got %q", sub, lo, hi, val)
	}
	return &n, nil
}

func vistaDurationArg(d *caddyfile.Dispenser, sub string, lo, hi time.Duration) (*caddy.Duration, error) {
	val, err := oneDirectiveArg(d, "vista", sub)
	if err != nil {
		return nil, err
	}
	dur, perr := caddy.ParseDuration(val)
	if perr != nil || dur < lo || dur > hi {
		return nil, d.Errf("vista %s: want %v through %v, got %q", sub, lo, hi, val)
	}
	cd := caddy.Duration(dur)
	return &cd, nil
}

func vistaSizeArg(d *caddyfile.Dispenser, sub string, lo, hi int64) (*int64, error) {
	val, err := oneDirectiveArg(d, "vista", sub)
	if err != nil {
		return nil, err
	}
	n, perr := parseBrowseSize(val)
	if perr != nil || int64(n) < lo || int64(n) > hi {
		return nil, d.Errf("vista %s: want %d through %d bytes, got %q", sub, lo, hi, val)
	}
	v := int64(n)
	return &v, nil
}

// validateVistaPath accepts an absolute, canonical path: no empty, `.`, or
// `..` segment, no trailing slash, no percent escape, no query or fragment.
func validateVistaPath(p string) error {
	if p == "" || p[0] != '/' || p == "/" {
		return fmt.Errorf("want an absolute path below /, got %q", p)
	}
	if strings.ContainsAny(p, "%?#;\\ \t\r\n") {
		return fmt.Errorf("path %q must be canonical: no escape, query, fragment, parameter, or whitespace", p)
	}
	if path.Clean(p) != p || strings.HasSuffix(p, "/") {
		return fmt.Errorf("path %q must be canonical: no empty, . or .. segment and no trailing slash", p)
	}
	return nil
}

// parseVistaTarget accepts `unix/<absolute clean path>`, Caddy's network
// address form, and returns the socket path.
func parseVistaTarget(s string) (string, error) {
	na, err := caddy.ParseNetworkAddress(s)
	if err != nil {
		return "", fmt.Errorf("want unix/<socket path>, got %q: %v", s, err)
	}
	if na.Network != "unix" {
		return "", fmt.Errorf("want a unix socket (unix/<path>), got network %q in %q", na.Network, s)
	}
	if na.StartPort != 0 || na.EndPort != 0 {
		return "", fmt.Errorf("a unix target carries no port: %q", s)
	}
	if !strings.HasPrefix(na.Host, "/") || path.Clean(na.Host) != na.Host {
		return "", fmt.Errorf("the socket path must be absolute and clean: %q", s)
	}
	if len(na.Host) > vistaMaxSocketPath {
		return "", fmt.Errorf("the socket path is %d bytes; a Unix socket path holds at most %d (macOS's sun_path, less its NUL): %q", len(na.Host), vistaMaxSocketPath, s)
	}
	return na.Host, nil
}

// validateVistaOrigins accepts `same` and https:// origins; never `any` or
// `null`: a session cookie any page could drive is a request forgery.
func validateVistaOrigins(vals []string) error {
	if len(vals) == 0 {
		return fmt.Errorf("want same or one or more https:// origins")
	}
	for _, v := range vals {
		if v == "same" {
			continue
		}
		if _, err := normalizeVistaOrigin(v); err != nil {
			return err
		}
	}
	return nil
}

// normalizeVistaOrigin returns an https origin in the form a browser sends
// it: scheme and lowercase host, with the port only when it is not 443.
func normalizeVistaOrigin(v string) (string, error) {
	u, err := url.Parse(v)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("%q is not an https:// origin (or same)", v)
	}
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != "443" {
		return "https://" + netJoinHost(host, port), nil
	}
	return "https://" + netJoinHost(host, ""), nil
}

func netJoinHost(host, port string) string {
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// provisionVista resolves each route's limits and checks what needs the
// site's other capabilities: path collisions and the route's stance under
// a covering auth gate. Host-dependent checks wait for Start.
func (h *Handler) provisionVista() error {
	h.vistaRoutes = nil
	for _, rt := range h.Vista {
		l, err := rt.limits()
		if err != nil {
			return fmt.Errorf("janus vista %s: %w", rt.Path, err)
		}
		switch {
		case rt.Path == "/ping":
			return fmt.Errorf("janus vista %s: the path is /ping's", rt.Path)
		case strings.HasPrefix(rt.Path+"/", browseAssetPrefix):
			return fmt.Errorf("janus vista %s: the path is under browse's asset prefix %s", rt.Path, browseAssetPrefix)
		case h.hubCfg != nil && h.hubCfg.path == rt.Path:
			return fmt.Errorf("janus vista %s: the path is this site's hub path", rt.Path)
		case h.wtRouteFor(rt.Path) != nil:
			return fmt.Errorf("janus vista %s: the path is a webtransport route on this site", rt.Path)
		}
		if h.authCfg != nil {
			if g := h.authCfg.doorGate(rt.Path); g != nil {
				return fmt.Errorf("janus vista %s: the path is gate %s's login door", rt.Path, g.prefix)
			}
			covering := h.authCfg.matchGate(rt.Path)
			switch {
			case covering != nil && covering.prefix == rt.Path+"/":
				if rt.AuthExempt {
					return fmt.Errorf("janus vista %s: `auth exempt` contradicts gate %s, which names the route exactly", rt.Path, covering.prefix)
				}
				l.gated = true
			case covering != nil && !rt.AuthExempt:
				return fmt.Errorf("janus vista %s lies under gate %s; add `auth exempt` to the route, or `gate %s { … }`", rt.Path, covering.prefix, rt.Path)
			case covering == nil && rt.AuthExempt:
				return fmt.Errorf("janus vista %s: `auth exempt` with no gate covering the route", rt.Path)
			}
		} else if rt.AuthExempt {
			return fmt.Errorf("janus vista %s: `auth exempt` on a site without auth", rt.Path)
		}
		h.vistaRoutes = append(h.vistaRoutes, &vistaSiteRoute{path: rt.Path, target: rt.Target, limits: l})
	}
	return nil
}

// vistaSiteRoute is a provisioned route as one generation's handler holds it.
type vistaSiteRoute struct {
	path   string
	target string
	limits *vistaLimits
}

// vistaRouteFor returns the route whose path is the request's escaped path
// byte for byte; any other spelling is not the route.
func (h *Handler) vistaRouteFor(escapedPath string) *vistaSiteRoute {
	for _, rt := range h.vistaRoutes {
		if rt.path == escapedPath {
			return rt
		}
	}
	return nil
}
