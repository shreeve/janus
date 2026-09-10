package janus

import (
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"net/http/httptest"
	"sync"
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
	if mdnsRouteOwner(caddyhttp.RouteList{route}, "janus.local") != mdnsRouteBlocked {
		t.Fatal("conditional route advertised as the whole front door")
	}
	shadow := caddyhttp.Route{Handlers: []caddyhttp.MiddlewareHandler{&caddyhttp.StaticResponse{Body: "shadow"}, &Handler{}}}
	if mdnsRouteOwner(caddyhttp.RouteList{shadow}, "janus.local") != mdnsRouteBlocked {
		t.Fatal("response before Janus advertised as the front door")
	}
	janus := caddyhttp.Route{Handlers: []caddyhttp.MiddlewareHandler{&Handler{}}}
	if mdnsRouteOwner(caddyhttp.RouteList{shadow, janus}, "janus.local") != mdnsRouteBlocked {
		t.Fatal("earlier response route ignored")
	}
	grouped := janus
	grouped.Group = "door"
	if mdnsRouteOwner(caddyhttp.RouteList{{Group: "door"}, grouped}, "janus.local") != mdnsRouteBlocked {
		t.Fatal("earlier empty grouped route ignored")
	}
	if mdnsRouteOwner(caddyhttp.RouteList{grouped}, "janus.local") != mdnsRouteOwned {
		t.Fatal("direct grouped Janus handler was rejected")
	}
	child := caddyhttp.Route{MatcherSets: caddyhttp.MatcherSets{{caddyhttp.MatchHost{"elsewhere.local"}}}, Handlers: []caddyhttp.MiddlewareHandler{&Handler{}}}
	outer := caddyhttp.Route{MatcherSets: caddyhttp.MatcherSets{{caddyhttp.MatchHost{"janus.local"}}}, Handlers: []caddyhttp.MiddlewareHandler{&caddyhttp.Subroute{Routes: caddyhttp.RouteList{child}}}}
	if mdnsRouteOwner(caddyhttp.RouteList{outer}, "janus.local") == mdnsRouteOwned {
		t.Fatal("impossible host intersection advertised as the front door")
	}
	outer.Handlers = []caddyhttp.MiddlewareHandler{&caddyhttp.Subroute{Routes: caddyhttp.RouteList{janus}}}
	if mdnsRouteOwner(caddyhttp.RouteList{outer}, "janus.local") != mdnsRouteOwned {
		t.Fatal("unconditional nested Janus route not discovered")
	}
}

func TestMdnsMetadataPublication(t *testing.T) {
	a := newTestSharedMdnsApp(t)
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for i := 0; i < 500; i++ {
			a.handleMdnsState(httptest.NewRecorder(), httptest.NewRequest("GET", "/1.0/mdns", nil))
		}
	}()
	for i := 0; i < 500; i++ {
		a.mdnsDoor.Store(&mdnsFrontDoorInfo{address: ":8080", routes: []caddyhttp.RouteList{{{Handlers: []caddyhttp.MiddlewareHandler{&Handler{}}}}}})
	}
	readers.Wait()
}
