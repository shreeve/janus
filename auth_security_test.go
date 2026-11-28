package janus

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// --- #1: host-confusion bypass (a wall-less site must not serve an
// auth-walled host) ----------------------------------------------------------

// TestAuthWallHostConfusionRefused pins the trailing-dot / normalization
// bypass: an auth-off handler that receives a request whose normalized host
// belongs to an auth-enabled site must refuse it (404), never proxy it. The
// vector is `Host: incus.example.com.` — Caddy's exact host matcher misses
// the dotted host and drops it on a catch-all auth-off site, while the
// registry resolves the dot-stripped host to the gated upstream.
func TestAuthWallHostConfusionRefused(t *testing.T) {
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/": {"alice"}})
	// An auth-on wildcard site covers the whole zone.
	app := &App{authSites: []authSiteEntry{
		{patterns: []string{"*.example.com"}, cfg: ah.h.authCfg},
	}}

	serveOff := func(h *Handler, host string) error {
		r := authReq(http.MethodGet, host, "/1.0/instances", nil)
		rr := httptest.NewRecorder()
		return h.ServeHTTP(rr, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			t.Fatalf("host %q reached the upstream through a wall-less site", host)
			return nil
		}))
	}

	// A catch-all auth-off handler (no exact host claim): a request naming a
	// covered host — including the trailing-dot bypass vector — is refused by
	// the wall guard.
	catchAll := &Handler{logger: zap.NewNop(), app: app}
	for _, host := range []string{"incus.example.com", "incus.example.com."} {
		err := serveOff(catchAll, host)
		var he caddyhttp.HandlerError
		if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
			t.Fatalf("host %q: err=%v, want a 404 HandlerError", host, err)
		}
		if !strings.Contains(err.Error(), "auth wall") {
			t.Fatalf("host %q refused, but not by the wall guard: %v", host, err)
		}
	}

	// The legitimate carve-out: an auth-off handler that is itself the exact
	// matcher for the host (a public subdomain under the auth-on wildcard)
	// claims it directly, so the guard must not fire. It has no data plane
	// here, so it still 404s — but not via the wall guard.
	carve := &Handler{logger: zap.NewNop(), app: app,
		authExactHosts: map[string]struct{}{"public.example.com": {}}}
	err := serveOff(carve, "public.example.com")
	var he caddyhttp.HandlerError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Fatalf("carve-out: err=%v, want 404", err)
	}
	if strings.Contains(err.Error(), "auth wall") {
		t.Fatalf("carve-out host was wrongly refused by the wall guard: %v", err)
	}

	// The guard is scoped to covered hosts: an unrelated zone is never
	// touched, so legitimate catch-all serving elsewhere is unaffected.
	if app.authCfgForHost("other.test") != nil {
		t.Fatal("authCfgForHost covered an unrelated host")
	}
}

// --- #2: same-site CSWSH / CSRF guard ---------------------------------------

// TestAuthSameOriginRequest pins authSameOriginRequest directly over the
// method/upgrade/header matrix.
func TestAuthSameOriginRequest(t *testing.T) {
	ws := []string{"Connection", "Upgrade", "Upgrade", "websocket",
		"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}
	cases := []struct {
		name   string
		method string
		kv     []string
		want   bool
	}{
		{"safe GET cross-site", http.MethodGet, []string{"Sec-Fetch-Site", "cross-site"}, true},
		{"safe GET sibling", http.MethodGet, []string{"Origin", "https://evil.app.test"}, true},
		{"HEAD cross-site", http.MethodHead, []string{"Sec-Fetch-Site", "same-site"}, true},
		{"OPTIONS cross-site", http.MethodOptions, []string{"Sec-Fetch-Site", "cross-site"}, true},
		{"POST same-origin (SFS)", http.MethodPost, []string{"Sec-Fetch-Site", "same-origin"}, true},
		{"POST none (SFS)", http.MethodPost, []string{"Sec-Fetch-Site", "none"}, true},
		{"POST same-site (SFS)", http.MethodPost, []string{"Sec-Fetch-Site", "same-site"}, false},
		{"POST cross-site (SFS)", http.MethodPost, []string{"Sec-Fetch-Site", "cross-site"}, false},
		{"POST same-origin (Origin)", http.MethodPost, []string{"Origin", "https://app.test"}, true},
		{"POST sibling (Origin)", http.MethodPost, []string{"Origin", "https://evil.app.test"}, false},
		{"POST foreign scheme (Origin)", http.MethodPost, []string{"Origin", "http://app.test"}, false},
		{"POST no headers (curl)", http.MethodPost, nil, true},
		{"PUT sibling (Origin)", http.MethodPut, []string{"Origin", "https://evil.app.test"}, false},
		{"DELETE no headers", http.MethodDelete, nil, true},
		{"WS same-origin (Origin)", http.MethodGet, append(ws, "Origin", "https://app.test"), true},
		{"WS same-origin (SFS)", http.MethodGet, append(ws, "Sec-Fetch-Site", "same-origin"), true},
		{"WS sibling (Origin)", http.MethodGet, append(ws, "Origin", "https://evil.app.test"), false},
		{"WS same-site (SFS)", http.MethodGet, append(ws, "Sec-Fetch-Site", "same-site"), false},
		{"WS no headers", http.MethodGet, ws, true},
	}
	for _, c := range cases {
		r := authReq(c.method, "app.test", "/x", nil, c.kv...)
		if got := authSameOriginRequest(r); got != c.want {
			t.Errorf("%s: authSameOriginRequest = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestAuthWallRefusesCrossOriginGated confirms the guard is wired into the
// wall: a gated, authorized state-bearing request that is cross-origin is
// refused 403; a same-origin one passes through with Remote-User injected; a
// safe read passes regardless.
func TestAuthWallRefusesCrossOriginGated(t *testing.T) {
	users := aliceUsers(t)
	ah := newAuthHarnessGates(t, users, 0, map[string][]string{"/": {"alice"}})
	tok, err := ah.st.mint("alice", "app.test", users["alice"])
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: authCookieName, Value: tok}

	// Cross-origin POST with a valid session: refused before the upstream.
	r := authReq(http.MethodPost, "app.test", "/x", nil, "Sec-Fetch-Site", "same-site")
	r.AddCookie(cookie)
	rr, out := ah.wall(t, r)
	if out != nil || rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin gated POST: code=%d passed=%v, want 403 handled", rr.Code, out != nil)
	}

	// Same-origin POST: passes the wall, Remote-User injected.
	r = authReq(http.MethodPost, "app.test", "/x", nil, "Origin", "https://app.test")
	r.AddCookie(cookie)
	_, out = ah.wall(t, r)
	if out == nil {
		t.Fatal("same-origin gated POST was refused")
	}
	if got := out.Header.Get("Remote-User"); got != "alice" {
		t.Fatalf("same-origin POST Remote-User = %q, want alice", got)
	}

	// Safe cross-origin GET: a read changes no state, so it is allowed.
	r = authReq(http.MethodGet, "app.test", "/x", nil, "Sec-Fetch-Site", "cross-site")
	r.AddCookie(cookie)
	if _, out := ah.wall(t, r); out == nil {
		t.Fatal("safe cross-origin GET was refused")
	}
}

// --- #6: absolute session lifetime cap --------------------------------------

// TestAuthAbsoluteLifetimeCap pins that a session past authMaxLifetime from
// its minting ends even while actively refreshed (idle well inside ttl), so a
// kept-alive or replayed session cannot outlive the absolute bound.
func TestAuthAbsoluteLifetimeCap(t *testing.T) {
	st, now := clockedStore(t)
	tok, err := st.mint("alice", "app.test", "cred")
	if err != nil {
		t.Fatal(err)
	}
	// An idle bound far beyond the lifetime cap, so only the cap can end it.
	idle := authMaxLifetime + 48*time.Hour

	// Just inside the cap, kept active: still valid, and the refresh slides
	// lastSeen forward.
	*now = now.Add(authMaxLifetime - time.Hour)
	if _, _, ok := st.lookup(tok, "app.test", idle, true); !ok {
		t.Fatal("session rejected before the absolute lifetime cap")
	}

	// Past the cap: ends despite having just been seen (idle is only ~2h).
	*now = now.Add(2 * time.Hour)
	if _, _, ok := st.lookup(tok, "app.test", idle, true); ok {
		t.Fatal("session outlived the absolute lifetime cap")
	}
	// And it was actually deleted, not merely reported absent.
	if st.sessionCount() != 0 {
		t.Fatalf("expired session left in the store: count=%d", st.sessionCount())
	}
}
