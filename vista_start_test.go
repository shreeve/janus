package janus

import (
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddytls"
	"go.uber.org/zap"
)

// vistaStartHandler is a provisioned site handler holding one route.
func vistaStartHandler(t *testing.T) *Handler {
	t.Helper()
	h := &Handler{logger: zap.NewNop(), Vista: []*VistaRoute{{Path: "/rpc", Target: "unix//run/openvista/rpc.sock"}}}
	if err := h.provisionVista(); err != nil {
		t.Fatal(err)
	}
	return h
}

func vistaStartServer(hosts []string, h *Handler, mutate func(*caddyhttp.Server)) *caddyhttp.Server {
	route := caddyhttp.Route{Handlers: []caddyhttp.MiddlewareHandler{h}}
	if hosts != nil {
		route.MatcherSets = caddyhttp.MatcherSets{{caddyhttp.MatchHost(hosts)}}
	}
	srv := &caddyhttp.Server{
		Listen:          []string{":443"},
		TLSConnPolicies: caddytls.ConnectionPolicies{{}},
		Routes:          caddyhttp.RouteList{route},
	}
	if mutate != nil {
		mutate(srv)
	}
	return srv
}

// TestVistaStartChecks pins the Start-time hard errors: exact hosts only,
// TLS ending at Janus, no Unix listener, no log_credentials, no duplicate
// route; and the keys a clean generation declares.
func TestVistaStartChecks(t *testing.T) {
	cases := []struct {
		name   string
		hosts  []string
		mutate func(*caddyhttp.Server)
		want   string
	}{
		{"exact host", []string{"vista.example.org"}, nil, ""},
		{"two exact hosts", []string{"a.example.org", "b.example.org"}, nil, ""},
		{"catch-all", nil, nil, "exact-host site"},
		{"wildcard", []string{"*.example.org"}, nil, "wildcard"},
		{"no TLS", []string{"vista.example.org"}, func(s *caddyhttp.Server) { s.TLSConnPolicies = nil }, "without TLS"},
		{"unix listener", []string{"vista.example.org"}, func(s *caddyhttp.Server) { s.Listen = []string{"unix//run/edge.sock"} }, "listens on unix"},
		{"log_credentials", []string{"vista.example.org"}, func(s *caddyhttp.Server) {
			s.Logs = &caddyhttp.ServerLogConfig{ShouldLogCredentials: true}
		}, "log_credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{state: &janusState{vista: newVistaPool(zap.NewNop())}, vistaKeys: map[string]bool{}}
			ha := &caddyhttp.App{Servers: map[string]*caddyhttp.Server{"s": vistaStartServer(tc.hosts, vistaStartHandler(t), tc.mutate)}}
			err := a.collectVista(ha)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				for _, h := range tc.hosts {
					if !a.vistaKeys[h+"/rpc"] {
						t.Fatalf("keys %v lack %s/rpc", a.vistaKeys, h)
					}
					if a.state.vista.route(h+"/rpc", "", nil).lim.Load() == nil {
						t.Fatalf("route %s/rpc has no limits for status", h)
					}
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %q, got %v", tc.want, err)
			}
		})
	}

	// The same host and path declared by two sites is refused.
	a := &App{state: &janusState{vista: newVistaPool(zap.NewNop())}, vistaKeys: map[string]bool{}}
	ha := &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
		"s1": vistaStartServer([]string{"vista.example.org"}, vistaStartHandler(t), nil),
		"s2": vistaStartServer([]string{"vista.example.org"}, vistaStartHandler(t), nil),
	}}
	if err := a.collectVista(ha); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("duplicate route: want declared twice, got %v", err)
	}
}
