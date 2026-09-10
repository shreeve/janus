package janus

import (
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Host alternatives mirror Caddy's OR of matcher sets and AND of nested
// routes. Within an alternative, patterns are a union; nil means any host,
// while a non-nil empty slice means no host. Keep those states distinct:
// an impossible parent must never become a catch-all in a deeper subroute.
func walkHostRoutes(routes caddyhttp.RouteList, inherited [][]string, visit func(caddyhttp.MiddlewareHandler, [][]string) error) error {
	for _, route := range routes {
		alternatives := intersectRouteHosts(inherited, route.MatcherSets)
		for _, handler := range route.Handlers {
			if sub, ok := handler.(*caddyhttp.Subroute); ok {
				if err := walkHostRoutes(sub.Routes, alternatives, visit); err != nil {
					return err
				}
			} else if err := visit(handler, alternatives); err != nil {
				return err
			}
		}
	}
	return nil
}

func intersectRouteHosts(inherited [][]string, sets caddyhttp.MatcherSets) [][]string {
	if len(sets) == 0 {
		return inherited
	}
	if inherited == nil {
		inherited = [][]string{nil}
	}
	var local [][]string
	for _, set := range sets {
		var current []string
		for _, matcher := range set {
			var hosts []string
			switch value := matcher.(type) {
			case caddyhttp.MatchHost:
				hosts = []string(value)
			case *caddyhttp.MatchHost:
				hosts = []string(*value)
			default:
				continue
			}
			if len(hosts) == 0 {
				hosts = []string{}
			}
			current = intersectHostPatterns(current, hosts)
		}
		local = append(local, current)
	}
	var out [][]string
	for _, parent := range inherited {
		for _, child := range local {
			out = append(out, intersectHostPatterns(parent, child))
		}
	}
	return out
}

func intersectHostPatterns(a, b []string) []string {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	out := []string{}
	for _, left := range a {
		for _, right := range b {
			l, r := strings.Split(strings.ToLower(left), "."), strings.Split(strings.ToLower(right), ".")
			if len(l) != len(r) {
				continue
			}
			compatible := true
			for i := range l {
				switch {
				case l[i] == r[i], r[i] == "*":
				case l[i] == "*":
					l[i] = r[i]
				default:
					compatible = false
				}
			}
			if compatible {
				out = append(out, strings.Join(l, "."))
			}
		}
	}
	return out
}

// A host-free alternative makes the whole union unconstrained. An empty
// result is unreachable, and callers must not turn it into a catch-all.
func routeHostUnion(alternatives [][]string) []string {
	if alternatives == nil {
		return nil
	}
	out := []string{}
	seen := map[string]bool{}
	for _, hosts := range alternatives {
		if hosts == nil {
			return nil
		}
		for _, host := range hosts {
			host = strings.ToLower(host)
			if !seen[host] {
				out = append(out, host)
				seen[host] = true
			}
		}
	}
	return out
}
