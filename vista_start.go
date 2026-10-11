package janus

// Start-time checks for vista routes that need the HTTP app: each route
// sits on exact hosts, behind TLS that ends at Janus, on a server that
// does not log credentials. The routes the generation declares are what
// its commit keeps (vistaPool.commit).

import (
	"fmt"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func (a *App) startVista() error {
	a.vistaKeys = map[string]bool{}
	if a.state == nil || a.state.vista == nil {
		return nil
	}
	ha, err := a.httpApp()
	if err != nil || ha == nil {
		return err
	}
	return a.collectVista(ha)
}

// collectVista walks the HTTP app's servers for vista routes and records
// the keys this generation declares.
func (a *App) collectVista(ha *caddyhttp.App) error {
	for name, srv := range ha.Servers {
		err := walkHostRoutes(srv.Routes, nil, func(handler caddyhttp.MiddlewareHandler, alternatives [][]string) error {
			h, ok := handler.(*Handler)
			if !ok || len(h.vistaRoutes) == 0 {
				return nil
			}
			patterns := routeHostUnion(alternatives)
			if len(patterns) == 0 {
				return fmt.Errorf("janus vista: a route needs an exact-host site; a catch-all site cannot hold a __Host- session cookie for one host")
			}
			for _, l := range srv.Listen {
				if strings.HasPrefix(l, "unix") {
					return fmt.Errorf("janus vista: server %q listens on %s; a vista route needs TLS ending at Janus", name, l)
				}
			}
			if len(srv.TLSConnPolicies) == 0 {
				return fmt.Errorf("janus vista: server %q serves a vista route without TLS; TLS must end at Janus", name)
			}
			if srv.Logs != nil && srv.Logs.ShouldLogCredentials {
				return fmt.Errorf("janus vista: server %q logs credentials (log_credentials), which would log session tokens", name)
			}
			for _, p := range patterns {
				if strings.Contains(p, "*") {
					return fmt.Errorf("janus vista: a route needs an exact-host site; %q is a wildcard", p)
				}
				host := strings.ToLower(p)
				for _, rt := range h.vistaRoutes {
					key := vistaRouteKey(host, rt.path)
					if a.vistaKeys[key] {
						return fmt.Errorf("janus vista: %s is declared twice", key)
					}
					a.vistaKeys[key] = true
					a.state.vista.route(key, rt.target, a).declare(rt.target, rt.limits)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
