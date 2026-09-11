package janus

import (
	"context"
	"crypto/x509"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// Exercise the real site handler: calling serveAuthWall directly would
// miss the early front-door return that used to bypass authentication.
func TestMdnsSharedAuth(t *testing.T) {
	app := newTestSharedMdnsApp(t)
	root := testRootCert(t, "Janus dashboard auth test")
	app.rootCAFor = func() (*x509.Certificate, error) { return root, nil }
	ah := newAuthHarness(t, aliceUsers(t), 0)
	ah.h.app = app
	serve := func(r *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey,
			&net.TCPAddr{IP: net.IPv4zero, Port: 80}))
		w := httptest.NewRecorder()
		if err := ah.h.ServeHTTP(w, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
			t.Fatal("front door fell through")
			return nil
		})); err != nil {
			t.Fatal(err)
		}
		return w
	}
	for _, host := range []string{"janus.local", "janus-2.local", "janus.lan.ripdev.io"} {
		for _, target := range []string{"/", "/status.json", "/trustworthy", "/trust/../status.json", "/trust%2f..%2fstatus.json"} {
			for _, method := range []string{"GET", "HEAD", "POST"} {
				w := serve(authReq(method, host, target, nil))
				if w.Code != 401 || strings.Contains(w.Body.String(), "\"apps\"") {
					t.Fatalf("%s %s%s: status %d, want 401", method, host, target, w.Code)
				}
			}
		}
		w := serve(authReq("GET", host, "/?view=apps", nil, "Accept", "text/html"))
		if w.Code != 302 || w.Header().Get("Location") != "/auth?to=%2F%3Fview%3Dapps" {
			t.Fatalf("browser redirect: %d %v", w.Code, w.Header())
		}
		for _, target := range []string{"/", "/status.json", "/auth"} {
			w = serve(httptest.NewRequest("GET", "http://"+host+target, nil))
			if w.Code != 308 || w.Header().Get("Location") != "https://"+host+target {
				t.Fatalf("HTTP redirect: %d %v", w.Code, w.Header())
			}
		}
		for _, scheme := range []string{"http", "https"} {
			for _, target := range []string{"/trust", "/trust/", "/trust/safari", "/trust/check", "/trust/ca.crt", "/trust/ca.mobileconfig"} {
				w = serve(httptest.NewRequest("GET", scheme+"://"+host+target, nil))
				if w.Code != 200 && w.Code != 204 {
					t.Fatalf("public onboarding %s %s: %d", scheme, target, w.Code)
				}
			}
		}
	}
	ah.h.authExactHosts = map[string]struct{}{"shop.local": {}}
	if w := serve(authReq("GET", "shop.local", "/trust", nil)); w.Code != 401 {
		t.Fatalf("app trust path escaped auth: %d", w.Code)
	}
	w := serve(httptest.NewRequest("POST", "http://janus.local/auth", strings.NewReader("user=alice&password=open-sesame")))
	if w.Code != 421 {
		t.Fatalf("plain HTTP credentials: %d", w.Code)
	}

	// Full login through the front door, with no app registration.
	w = serve(authReq("GET", "janus.local", "/auth", nil))
	csrf := authCookieValue(w, authCSRFCookieName)
	if w.Code != 200 || csrf == "" || !strings.Contains(w.Body.String(), csrf) {
		t.Fatalf("login form: %d", w.Code)
	}
	form := url.Values{"user": {"alice"}, "password": {"open-sesame"}, "csrf": {csrf}}
	w = serve(authReq("POST", "janus.local", "/auth", strings.NewReader(form.Encode()),
		"Content-Type", "application/x-www-form-urlencoded", "Cookie", authCSRFCookieName+"="+csrf))
	session := authCookieValue(w, authCookieName)
	if w.Code != 303 || session == "" {
		t.Fatalf("login: %d %s", w.Code, w.Body.String())
	}
	for _, target := range []string{"/", "/status.json"} {
		w = serve(authReq("GET", "janus.local", target, nil, "Cookie", authCookieName+"="+session))
		if w.Code != 200 || w.Body.Len() == 0 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("authenticated %s: %d %v", target, w.Code, w.Header())
		}
		w = serve(authReq("GET", "janus-2.local", target, nil, "Cookie", authCookieName+"="+session))
		if w.Code != 401 {
			t.Fatalf("session crossed host boundary: %d", w.Code)
		}
	}
	// Explicit auth off still serves an open dashboard on this site.
	ah.h.authCfg = nil
	for _, scheme := range []string{"http", "https"} {
		w = serve(httptest.NewRequest("GET", scheme+"://janus.local/status.json", nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), "\"apps\"") {
			t.Fatalf("auth off %s: %d", scheme, w.Code)
		}
	}
}
