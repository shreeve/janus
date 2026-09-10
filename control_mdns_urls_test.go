package janus

import (
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"testing"
)

func TestMdnsFrontDoorURLs(t *testing.T) {
	for _, tc := range []struct {
		name, listen, address, scope, canonical, dashboard, trust string
		owns                                                      bool
	}{
		{"shared LAN", "", ":80", "lan", "", "http://edge-2.local/", "http://edge-2.local/trust", true},
		{"custom HTTP port", "", ":8080", "lan", "", "http://edge-2.local:8080/", "http://edge-2.local:8080/trust", true},
		{"another app owns shared name", "", ":80", "lan", "", "", "", false},
		{"shared localhost", "", ":80", "localhost", "", "", "", true},
		{"shared WAN", "", ":80", "wan", "", "", "", true},
		{"canonical WAN", "", ":80", "wan", "https://edge.example", "https://edge.example/", "", true},
		{"canonical LAN", "", ":8080", "lan", "https://edge.example/", "https://edge.example/", "http://edge-2.local:8080/trust", true},
		{"dedicated IPv4", ":7680", "0.0.0.0:7680", "lan", "", "http://127.0.0.1:7680/", "http://edge-2.local:7680/trust", true},
		{"dedicated IPv6", ":7680", "[::]:7680", "lan", "", "http://[::1]:7680/", "http://edge-2.local:7680/trust", true},
		{"dedicated loopback", "127.0.0.2:7680", "127.0.0.2:7680", "lan", "", "http://127.0.0.2:7680/", "", true},
		{"dedicated WAN", ":7680", "[::]:7680", "wan", "", "http://[::1]:7680/", "", true},
		{"resolved hostname", "localhost:7680", "127.0.0.1:7680", "lan", "", "http://127.0.0.1:7680/", "", true},
		{"scoped address", "[fe80::1%en0]:7680", "[fe80::1%en0]:7680", "lan", "", "", "", true},
		{"invalid port", ":0", ":0", "lan", "", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ms := &MdnsSettings{Listen: tc.listen, Canonical: tc.canonical}
			dashboard, trust := mdnsFrontDoorURLs(ms, "edge-2.local", tc.address, tc.scope, tc.owns)
			if dashboard != tc.dashboard || trust != tc.trust {
				t.Fatalf("got (%q, %q), want (%q, %q)", dashboard, trust, tc.dashboard, tc.trust)
			}
		})
	}
}

func TestMdnsSharedURLCandidates(t *testing.T) {
	for _, tc := range []struct {
		listen string
		want   bool
	}{
		{":80", true}, {"0.0.0.0:80", true}, {"[::]:80", true},
		{"192.168.1.5:80", true}, {"127.0.0.1:80", false},
		{"[::1]:80", false}, {"localhost:80", false},
		{"[fe80::1%en0]:80", false}, {":8080", false},
	} {
		if got := mdnsServerLANPort(&caddyhttp.Server{Listen: []string{tc.listen}}, 80); got != tc.want {
			t.Errorf("%s LAN candidate = %v, want %v", tc.listen, got, tc.want)
		}
	}
	route := caddyhttp.Route{MatcherSets: caddyhttp.MatcherSets{{caddyhttp.MatchPath{"/api/*"}}}, Handlers: []caddyhttp.MiddlewareHandler{&Handler{}}}
	visited := false
	_ = walkHostRoutes(caddyhttp.RouteList{route}, [][]string{nil}, func(caddyhttp.MiddlewareHandler, [][]string) error {
		visited = true
		return nil
	}, mdnsHostOnlyRoute)
	if visited {
		t.Fatal("conditional route advertised as the whole front door")
	}
}
