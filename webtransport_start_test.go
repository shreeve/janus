package janus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

func TestWebtransportListenAddrs(t *testing.T) {
	h1h2 := []string{"h1", "h2"}
	ha := &caddyhttp.App{Servers: map[string]*caddyhttp.Server{
		"srv0": {Listen: []string{"10.0.0.249:443", "127.0.0.1:443", "[::1]:443"}, Protocols: h1h2},
		"srv1": {Listen: []string{":80"}, Protocols: h1h2},
		"unix": {Listen: []string{"unix//tmp/x.sock"}, Protocols: h1h2},
	}}
	got, err := wtListenAddrs(ha, 443)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].String() != "10.0.0.249:443" || got[1].String() != "127.0.0.1:443" || got[2].String() != "[::1]:443" {
		t.Fatalf("addrs = %v", got)
	}
	// Port 80 listeners and unix sockets are not the relay's.
	if got, _ := wtListenAddrs(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{"srv1": {Listen: []string{":80"}, Protocols: h1h2}}}, 443); len(got) != 0 {
		t.Fatalf("port 80 became a relay address: %v", got)
	}
	// A bare port binds the unspecified address of the listen network's family.
	got, err = wtListenAddrs(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{
		"a": {Listen: []string{":443"}, Protocols: h1h2}, "b": {Listen: []string{"tcp4/:443"}, Protocols: h1h2},
	}}, 443)
	if err != nil || len(got) != 2 || got[0].String() != "0.0.0.0:443" || got[1].String() != "[::]:443" {
		t.Fatalf("unspecified: %v %v", got, err)
	}
	cases := map[string]struct {
		srv  *caddyhttp.Server
		want string
	}{
		"h3 by default":   {&caddyhttp.Server{Listen: []string{":443"}}, "HTTP/3 on"},
		"h3 explicit":     {&caddyhttp.Server{Listen: []string{":443"}, Protocols: []string{"h1", "h2", "h3"}}, "HTTP/3 on"},
		"h3 per listener": {&caddyhttp.Server{Listen: []string{":443"}, Protocols: h1h2, ListenProtocols: [][]string{{"h3"}}}, "HTTP/3 on"},
		"port range":      {&caddyhttp.Server{Listen: []string{":443-444"}, Protocols: h1h2}, "port range"},
		"hostname":        {&caddyhttp.Server{Listen: []string{"pup.local:443"}, Protocols: h1h2}, "not names"},
	}
	for name, tc := range cases {
		_, err := wtListenAddrs(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{"s": tc.srv}}, 443)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.want, err)
		}
	}
	// A per-listener protocol list without h3 clears the server default.
	got, err = wtListenAddrs(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{
		"s": {Listen: []string{":443"}, ListenProtocols: [][]string{{"h1", "h2"}}},
	}}, 443)
	if err != nil || len(got) != 1 {
		t.Fatalf("per-listener h1 h2: %v %v", got, err)
	}
}

func TestWebtransportCollectRoutes(t *testing.T) {
	a := &App{}
	wt := []*WebtransportRoute{{Path: "/lyte", Target: "udp/127.0.0.1:41151"}}
	exact := testHostRoute(t, []string{"pup.local"}, &Handler{Webtransport: wt})
	wild := testHostRoute(t, []string{"*.local"}, &Handler{Webtransport: wt})
	catchAll := testHostRoute(t, nil, &Handler{Webtransport: wt})
	gated := testHostRoute(t, []string{"lyte.trusthealth.com"}, &Handler{Webtransport: wt, authCfg: &authSite{}})

	routes, err := a.collectWebtransportRoutes(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{"s": {Routes: caddyhttp.RouteList{exact, gated}}}})
	if err != nil || len(routes) != 2 {
		t.Fatalf("exact sites: %v %v", routes, err)
	}
	byHost := map[string]*wtRoute{}
	for _, rt := range routes {
		byHost[rt.host] = rt
	}
	if byHost["pup.local"] == nil || byHost["pup.local"].config().gated || byHost["pup.local"].config().maxSessions != 4 || byHost["pup.local"].target.String() != "127.0.0.1:41151" {
		t.Fatalf("pup route: %+v", byHost["pup.local"])
	}
	if byHost["lyte.trusthealth.com"] == nil || !byHost["lyte.trusthealth.com"].config().gated {
		t.Fatalf("gated route: %+v", byHost["lyte.trusthealth.com"])
	}
	for name, r := range map[string]caddyhttp.Route{"wildcard": wild, "catch-all": catchAll} {
		_, err := a.collectWebtransportRoutes(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{"s": {Routes: caddyhttp.RouteList{r}}}})
		if err == nil || !strings.Contains(err.Error(), "exact-host site") {
			t.Errorf("%s: want exact-host refusal, got %v", name, err)
		}
	}
	// The same host and path declared in two servers is a duplicate.
	_, err = a.collectWebtransportRoutes(&caddyhttp.App{Servers: map[string]*caddyhttp.Server{
		"a": {Routes: caddyhttp.RouteList{exact}}, "b": {Routes: caddyhttp.RouteList{exact}},
	}})
	if err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestWebtransportDescriptor(t *testing.T) {
	st, err := newJanusState(nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Destruct() })
	app := &App{state: st, Webtransport: &WebtransportSettings{}}
	route := &WebtransportRoute{Path: "/lyte", Target: "udp/127.0.0.1:41151"}
	h := &Handler{app: app, logger: zap.NewNop(), Webtransport: []*WebtransportRoute{route}}

	get := func(h *Handler, method, path, user string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(method, "https://lyte.trusthealth.com"+path, nil)
		req.Host = "lyte.trusthealth.com"
		if user != "" {
			req.Header.Set("Remote-User", user)
		}
		h.serveWebtransportDescriptor(rr, req, route)
		return rr
	}
	rr := get(h, http.MethodGet, "/lyte", "")
	if rr.Code != 200 || rr.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(rr.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("GET: %d %v", rr.Code, rr.Header())
	}
	want := `{"url":"https://lyte.trusthealth.com/lyte","max_datagram":1200,"certificate_hashes":[]}`
	if got := strings.TrimSpace(rr.Body.String()); got != want {
		t.Fatalf("body %s", got)
	}
	if rr := get(h, http.MethodHead, "/lyte", ""); rr.Code != 200 || rr.Body.Len() != 0 {
		t.Fatalf("HEAD: %d %d bytes", rr.Code, rr.Body.Len())
	}
	if rr := get(h, http.MethodPost, "/lyte", ""); rr.Code != 405 || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST: %d", rr.Code)
	}
	// A gated site: no user means no ticket and no descriptor; a user gets
	// a URL carrying a redeemable ticket.
	gated := &Handler{app: app, logger: zap.NewNop(), Webtransport: []*WebtransportRoute{route}, authCfg: &authSite{}}
	if rr := get(gated, http.MethodGet, "/lyte", ""); rr.Code != 403 {
		t.Fatalf("gated without user: %d", rr.Code)
	}
	rr = get(gated, http.MethodGet, "/lyte", "steve")
	if rr.Code != 200 {
		t.Fatalf("gated with user: %d", rr.Code)
	}
	body := rr.Body.String()
	i := strings.Index(body, "?t=")
	if i < 0 {
		t.Fatalf("no ticket in %s", body)
	}
	ticket := body[i+3 : strings.Index(body[i:], `"`)+i]
	user, ok := st.webtransport.tickets.redeem(ticket, wtRouteKey("lyte.trusthealth.com", "/lyte"))
	if !ok || user != "steve" {
		t.Fatalf("ticket did not redeem for steve: %v %q", ok, user)
	}
	if body := get(gated, http.MethodGet, "/lyte", "steve").Body.String(); strings.Contains(body, ticket) {
		t.Fatal("two descriptors carried the same ticket")
	}
}

// testHostRoute wraps a handler in the shape the Caddyfile adapter emits:
// a host-matched route whose subroute carries the site's handlers; nil
// hosts is a catch-all.
func testHostRoute(t *testing.T, hosts []string, h *Handler) caddyhttp.Route {
	t.Helper()
	leaf := caddyhttp.Route{Handlers: []caddyhttp.MiddlewareHandler{h}}
	if hosts == nil {
		return leaf
	}
	return caddyhttp.Route{
		MatcherSets: caddyhttp.MatcherSets{{caddyhttp.MatchHost(hosts)}},
		Handlers:    []caddyhttp.MiddlewareHandler{&caddyhttp.Subroute{Routes: caddyhttp.RouteList{leaf}}},
	}
}
