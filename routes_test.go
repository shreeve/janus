package janus

import (
	"reflect"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

func TestNestedRouteHostTables(t *testing.T) {
	tests := []struct {
		name          string
		parent, child caddyhttp.MatcherSets
		grandchild    []string
		want          []string
	}{
		{"parent narrows wildcard", caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}}}, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"*.test"}}}, nil, []string{"a.test"}},
		{"child narrows wildcard", caddyhttp.MatcherSets{{caddyhttp.MatchHost{"*.test"}}}, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}}}, nil, []string{"a.test"}},
		{"crossing wildcards", caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.*"}}}, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"*.test"}}}, nil, []string{"a.test"}},
		{"disjoint never revives", caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}}}, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"b.test"}}}, []string{"c.test"}, []string{}},
		{"host-free OR alternative", nil, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}}, {}}, nil, nil},
		{"OR remains bounded by parent", caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}}}, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"b.test"}}, {}}, nil, []string{"a.test"}},
		{"AND within matcher set", nil, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}, caddyhttp.MatchHost{"b.test"}}}, []string{"a.test"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{hubCfg: &hubSite{maxConns: 10}, authCfg: &authSite{}}
			leaf := caddyhttp.Route{Handlers: []caddyhttp.MiddlewareHandler{h}}
			if tt.grandchild != nil {
				leaf.MatcherSets = caddyhttp.MatcherSets{{caddyhttp.MatchHost(tt.grandchild)}}
			}
			routes := caddyhttp.RouteList{{MatcherSets: tt.parent, Handlers: []caddyhttp.MiddlewareHandler{&caddyhttp.Subroute{Routes: caddyhttp.RouteList{{MatcherSets: tt.child, Handlers: []caddyhttp.MiddlewareHandler{&caddyhttp.Subroute{Routes: caddyhttp.RouteList{leaf}}}}}}}}}
			var hubs []hubSiteEntry
			var auth []authSiteEntry
			var sites [][]string
			var browse []browseSiteEntry
			collectHubRoutes(routes, nil, &hubs)
			collectAuthRoutes(routes, nil, &auth)
			collectSiteHosts(routes, nil, &sites)
			if err := collectBrowseRoutes(routes, nil, &browse); err != nil {
				t.Fatal(err)
			}
			if tt.want != nil && len(tt.want) == 0 {
				if len(hubs)+len(auth)+len(sites)+len(browse) != 0 {
					t.Fatalf("unreachable handler indexed: %v %v %v %v", hubs, auth, sites, browse)
				}
				return
			}
			if len(hubs) != 1 || len(auth) != 1 || len(sites) != 1 || len(browse) != 1 {
				t.Fatalf("missing handler: %v %v %v %v", hubs, auth, sites, browse)
			}
			for _, got := range [][]string{hubs[0].patterns, auth[0].patterns, sites[0], browse[0].hosts} {
				if !reflect.DeepEqual(got, tt.want) {
					t.Errorf("hosts = %#v, want %#v", got, tt.want)
				}
			}
			a := &App{hubSites: [][]hubSiteEntry{hubs}}
			if tt.want != nil && a.hubSiteFor("b.test") != nil {
				t.Fatal("hub table widened route admission")
			}
		})
	}
}

func TestImpossibleColdRootCannotAcquireDeeperHost(t *testing.T) {
	impossible := intersectRouteHosts(nil, caddyhttp.MatcherSets{{caddyhttp.MatchHost{"a.test"}, caddyhttp.MatchHost{"b.test"}}})
	routes := caddyhttp.RouteList{{MatcherSets: caddyhttp.MatcherSets{{caddyhttp.MatchHost{"c.test"}}}, Handlers: []caddyhttp.MiddlewareHandler{&Handler{coldRoots: []BrowseRoot{{Path: t.TempDir()}}}}}}
	var entries []browseSiteEntry
	if err := collectBrowseRoutes(routes, impossible, &entries); err == nil {
		t.Fatal("impossible parent was widened to reserve c.test")
	}
}
