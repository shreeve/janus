package janus

// Start-time assembly of the relay's configuration: the listen addresses
// from the HTTP servers on the HTTPS port, the route table from the
// provisioned sites, the TLS config from Caddy's tls app, and every gate
// that needs the HTTP app (which is not provisioned when the janus app
// provisions, so these are Start errors like the module's other
// HTTP-dependent gates).

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"github.com/quic-go/quic-go/http3"
)

const wtRelayPort = 443

// startWebtransport stages this generation's relay configuration.
func (a *App) startWebtransport() error {
	if a.state == nil || a.state.webtransport == nil {
		return nil
	}
	ha, err := a.httpApp()
	if err != nil {
		return err
	}
	routes, err := a.collectWebtransportRoutes(ha)
	if err != nil {
		return err
	}
	if a.Webtransport == nil {
		if len(routes) > 0 {
			return fmt.Errorf("janus webtransport: %d route(s) declared but no listener: add `webtransport { }` to the global janus block", len(routes))
		}
		return a.state.webtransport.stage(a, nil, nil)
	}
	if ha == nil {
		return errors.New("janus webtransport: the relay needs the HTTP app; no server listens on the HTTPS port")
	}
	port := ha.HTTPSPort
	if port == 0 {
		port = wtRelayPort
	}
	if ExposureScope() != "" && port != wtRelayPort {
		return fmt.Errorf("janus webtransport: under the service edge the relay's port is 443; https_port is %d, which the exposure mode does not cover", port)
	}
	listen, err := wtListenAddrs(ha, port)
	if err != nil {
		return err
	}
	if len(listen) == 0 {
		return fmt.Errorf("janus webtransport: no HTTP server listens on port %d, so the relay has no addresses to bind", port)
	}
	table := &wtTable{
		maxSessions: a.Webtransport.maxSessions(),
		maxDatagram: a.Webtransport.maxDatagram(),
		idleTimeout: a.Webtransport.idleTimeout(),
		routes:      map[string]*wtRoute{},
		hosts:       map[string]bool{},
		listen:      listen,
	}
	for _, rt := range routes {
		for _, l := range listen {
			if l == rt.target || (l.Port() == rt.target.Port() && l.Addr().IsUnspecified()) {
				return fmt.Errorf("janus webtransport: route %s%s targets %s, one of the relay's own listen addresses", rt.host, rt.path, rt.target)
			}
		}
		table.routes[wtRouteKey(rt.host, rt.path)] = rt
		table.hosts[rt.host] = true
	}
	tlsConf, err := a.webtransportTLSConfig()
	if err != nil {
		return err
	}
	return a.state.webtransport.stage(a, table, tlsConf)
}

func (a *App) httpApp() (*caddyhttp.App, error) {
	httpAppI, err := a.ctx.AppIfConfigured("http")
	if err != nil {
		if errors.Is(err, caddy.ErrNotConfigured) {
			return nil, nil
		}
		return nil, fmt.Errorf("janus webtransport: loading http app: %w", err)
	}
	if httpAppI == nil {
		return nil, nil
	}
	ha, ok := httpAppI.(*caddyhttp.App)
	if !ok {
		return nil, fmt.Errorf("janus webtransport: http app is %T, not *caddyhttp.App", httpAppI)
	}
	return ha, nil
}

// wtListenAddrs derives the relay's UDP addresses from every HTTP
// listener on the port: the same hosts, UDP instead of TCP. A listener
// with HTTP/3 on would bind the same UDP port, so it is refused; so are a
// hostname in place of an address and a port range.
func wtListenAddrs(ha *caddyhttp.App, port int) ([]netip.AddrPort, error) {
	var out []netip.AddrPort
	for name, srv := range ha.Servers {
		for i, l := range srv.Listen {
			na, err := caddy.ParseNetworkAddress(l)
			if err != nil {
				return nil, fmt.Errorf("janus webtransport: server %q listen %q: %w", name, l, err)
			}
			if na.IsUnixNetwork() || na.IsFdNetwork() {
				continue
			}
			if uint(port) < na.StartPort || uint(port) > na.EndPort {
				continue
			}
			if na.StartPort != na.EndPort {
				return nil, fmt.Errorf("janus webtransport: server %q listen %q is a port range; the relay needs one port", name, l)
			}
			protocols := srv.Protocols
			if i < len(srv.ListenProtocols) && srv.ListenProtocols[i] != nil {
				protocols = srv.ListenProtocols[i]
			}
			if len(protocols) == 0 || slices.Contains(protocols, "h3") {
				return nil, fmt.Errorf("janus webtransport: server %q listens on %s with HTTP/3 on, which would bind the relay's UDP port; set `servers { protocols h1 h2 }`", name, l)
			}
			var addr netip.Addr
			switch {
			case na.Host == "":
				addr = netip.IPv6Unspecified()
				if strings.HasSuffix(na.Network, "4") {
					addr = netip.IPv4Unspecified()
				}
			default:
				addr, err = netip.ParseAddr(na.Host)
				if err != nil {
					return nil, fmt.Errorf("janus webtransport: server %q listens on %q; the relay binds addresses, not names", name, l)
				}
			}
			ap := netip.AddrPortFrom(addr, uint16(port))
			if !slices.Contains(out, ap) {
				out = append(out, ap)
			}
		}
	}
	slices.SortFunc(out, func(x, y netip.AddrPort) int { return strings.Compare(x.String(), y.String()) })
	return out, nil
}

// collectWebtransportRoutes pairs each janus site's routes with its host
// matchers. A route belongs to exact hosts only.
func (a *App) collectWebtransportRoutes(ha *caddyhttp.App) ([]*wtRoute, error) {
	if ha == nil {
		return nil, nil
	}
	var routes []*wtRoute
	seen := map[string]bool{}
	for _, srv := range ha.Servers {
		err := walkHostRoutes(srv.Routes, nil, func(handler caddyhttp.MiddlewareHandler, alternatives [][]string) error {
			h, ok := handler.(*Handler)
			if !ok || len(h.Webtransport) == 0 {
				return nil
			}
			patterns := routeHostUnion(alternatives)
			if len(patterns) == 0 {
				return fmt.Errorf("janus webtransport: a route needs an exact-host site; a catch-all site has no certificate for the relay to present")
			}
			for _, p := range patterns {
				if strings.Contains(p, "*") {
					return fmt.Errorf("janus webtransport: a route needs an exact-host site; %q is a wildcard", p)
				}
				host := strings.ToLower(p)
				for _, rt := range h.Webtransport {
					key := wtRouteKey(host, rt.Path)
					if seen[key] {
						return fmt.Errorf("janus webtransport: %s%s is declared twice", host, rt.Path)
					}
					seen[key] = true
					wr := &wtRoute{host: host, path: rt.Path, target: rt.targetAddr()}
					wr.cfg.Store(&wtRouteConfig{origin: rt.Origin, maxSessions: rt.maxSessions(), gated: h.authCfg != nil})
					routes = append(routes, wr)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return routes, nil
}

// webtransportTLSConfig is Caddy's certificate cache behind an HTTP/3
// ALPN: ACME, internal, loaded pairs, and on-demand issuance under
// `permission janus`, unchanged.
func (a *App) webtransportTLSConfig() (*tls.Config, error) {
	policies := caddytls.ConnectionPolicies{{ALPN: []string{http3.NextProtoH3}}}
	if err := policies.Provision(a.ctx); err != nil {
		return nil, fmt.Errorf("janus webtransport: tls policy: %w", err)
	}
	conf := policies.TLSConfig(a.ctx)
	conf.MinVersion = tls.VersionTLS13
	return http3.ConfigureTLSConfig(conf), nil
}
