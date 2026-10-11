package janus

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

// --- gate matching on the canonical path ---------------------------------------

// phiFilesHarness is an auth-on site whose registered app serves a files
// root (cache forever) holding reports/phi/x.txt behind gate /reports/phi/.
func phiFilesHarness(t *testing.T) *authHarness {
	t.Helper()
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/reports/phi/": {"alice"}})
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "reports", "phi"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "reports", "phi", "x.txt"), []byte("phi"), 0o644); err != nil {
		t.Fatal(err)
	}
	dp, reg := newTestDataPlane(t)
	if _, err := reg.createWithPolicy("app", []string{"app.test"}, nil, &FilesPolicy{
		Roots: []FilesRoot{{Path: root, Cache: filesCacheForever}},
		Shell: filepath.Join(t.TempDir(), "missing-shell.html"),
	}, ""); err != nil {
		t.Fatal(err)
	}
	on := true
	ah.h.dp = dp
	ah.h.Files = &on
	return ah
}

func serveSite(t *testing.T, h *Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	err := h.ServeHTTP(rr, r, caddyhttp.HandlerFunc(func(http.ResponseWriter, *http.Request) error {
		t.Fatal("request reached the next handler")
		return nil
	}))
	if err != nil {
		t.Fatalf("ServeHTTP %s: %v", r.URL.Path, err)
	}
	return rr
}

func TestAuthGateBypassByEmptySegmentOnFilesRoot(t *testing.T) {
	ah := phiFilesHarness(t)
	for _, target := range []string{
		"/reports/phi/x.txt",
		"/reports//phi/x.txt",
		"//reports/phi/x.txt",
		"/reports///phi/x.txt",
		"/REPORTS//phi/x.txt",
		"/reports;v=1/phi/x.txt",
		"/reports/phi;x/x.txt",
		"/reports%2Fphi/x.txt",
		"/reports%252Fphi/x.txt",
	} {
		rr := serveSite(t, ah.h, authReq(http.MethodGet, "app.test", target, nil))
		if rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: %d %q, want 401", target, rr.Code, rr.Body.String())
		}
		rr = serveSite(t, ah.h, authReq(http.MethodGet, "app.test", target, nil, "Accept", "text/html"))
		if rr.Code != http.StatusFound {
			t.Errorf("%s browser-shaped without a session: %d, want 302", target, rr.Code)
		}
	}
}

func TestAuthCanonicalPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"//", "/"},
		{"///", "/"},
		{"/a//b/", "/a/b/"},
		{"//one/secret", "/one/secret"},
		{"/one;x/secret", "/one/secret"},
		{"/one;jsessionid=1;y=2/secret;z", "/one/secret"},
		{"/one/;x", "/one/"},
		{"/ONE/Secret", "/one/secret"},
		{`/one\secret`, "/one/secret"},
		{"/one/", "/one/"},
		{"/one", "/one"},
		{"*", "/*"},
	}
	for _, tc := range cases {
		if got := authCanonicalPath(tc.in); got != tc.want {
			t.Errorf("authCanonicalPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGatePathMatchesCanonicalForms(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/one/", "//one/secret", true},
		{"/one/", "///one/secret", true},
		{"/one/", "/one//secret", true},
		{"/a/b/", "/a//b/", true},
		{"/a/b/", "/a//b", true},
		{"/a/b/", "/a///b/x", true},
		{"/one/", "/one;x/secret", true},
		{"/one/", "/one;x", true},
		{"/reports/phi/", "/reports;v=1/phi;x/x.txt", true},
		{"/one/", "/ONE/secret", true},
		{"/One/", "/one/secret", true},
		{"/one/", "/one/", true},
		{"/one/", "/one", true},
		{"/one/", "/one//", true},
		{"/one/", `/one\secret`, true},
		// r.URL.Path is decoded: /one%2Fsecret arrives as /one/secret.
		{"/one/", "/one/secret", true},
		// A literal %2F left after one decoding (from %252F) is gated in
		// its second-decoding form, as is an encoded letter.
		{"/one/", "/one%2Fsecret", true},
		{"/one/", "/%6Fne/secret", true},
		// Dot segments resolved for upstreams that resolve them.
		{"/one/", "/x/../one/secret", true},
		{"/one/", "/ones", false},
		{"/one/", "/ones/x", false},
		{"/one/", "/o/ne", false},
		{"/one/", "/public//one", false},
		{"/one/", "/one.txt", false},
	}
	for _, tc := range cases {
		if got := gatePathMatches(tc.prefix, tc.path); got != tc.want {
			t.Errorf("gatePathMatches(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}

func TestAuthGatePrefixRejectsUnmatchableBytes(t *testing.T) {
	for _, raw := range []string{"/one;x/", `/one\two/`} {
		if _, err := normalizeGatePrefix(raw); err == nil {
			t.Errorf("normalizeGatePrefix(%q) accepted a prefix canonical matching can never reach", raw)
		}
	}
}

func TestAuthWallGatesNonCanonicalPaths(t *testing.T) {
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/one/": {"alice"}})
	for _, target := range []string{
		"//one/secret", "///one/secret", "/one//secret", "/one;x/secret",
		"/ONE/secret", "/one%2Fsecret", "/one%252Fsecret", "/%6Fne/secret",
		"/one//auth", "//one/auth", "/one/auth/", "/one;x/auth",
	} {
		rr, out := ah.wall(t, authReq(http.MethodGet, "app.test", target, nil))
		if out != nil || rr.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: passed=%v code=%d, want 401", target, out != nil, rr.Code)
		}
	}
	// The login door stays the exact {prefix}auth path.
	rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/one/auth", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `name="csrf"`) {
		t.Fatalf("exact door: %d", rr.Code)
	}
	// The path handed upstream is never rewritten.
	session := ah.loginDoor(t, "app.test", "/one/auth", "alice", "open-sesame")
	r := authReq(http.MethodGet, "app.test", "/one;x//secret", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	_, out := ah.wall(t, r)
	if out == nil || out.URL.Path != "/one;x//secret" || out.Header.Get("Remote-User") != "alice" {
		t.Fatalf("authorized non-canonical path: %+v", out)
	}
}

func TestSafeToCanonicalForms(t *testing.T) {
	cases := []struct{ raw, prefix, want string }{
		{"/one//x", "/one/", "/one//x"},
		{"/one;p/x", "/one/", "/one;p/x"},
		{"/ONE/x", "/one/", "/ONE/x"},
		{"/public//one/x", "/one/", "/one/"},
		{"//one/x", "/one/", "/one/"},
	}
	for _, tc := range cases {
		if got := safeTo(tc.raw, tc.prefix); got != tc.want {
			t.Errorf("safeTo(%q, %q) = %q, want %q", tc.raw, tc.prefix, got, tc.want)
		}
	}
}

// --- identity headers -----------------------------------------------------------

// identityUpstream registers app.test with one upstream that reports every
// identity-shaped header it receives (name folded _→- and lowercased).
func identityUpstream(t *testing.T) (*dataPlane, chan []string) {
	t.Helper()
	dp, reg := newTestDataPlane(t)
	seen := make(chan []string, 8)
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got []string
		for name, values := range r.Header {
			folded := strings.ToLower(strings.ReplaceAll(name, "_", "-"))
			if folded == "remote-user" || folded == "rip-site" {
				for _, v := range values {
					got = append(got, folded+"="+v)
				}
			}
		}
		seen <- got
		w.WriteHeader(http.StatusNoContent)
	}))
	rec, err := reg.create("app", []string{"app.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.setUpstreams(rec.ID, []Upstream{{Path: sock}}); err != nil {
		t.Fatal(err)
	}
	return dp, seen
}

func forgedIdentityRequest(target string) *http.Request {
	r := authReq(http.MethodGet, "app.test", target, nil)
	for _, name := range []string{"Remote-User", "Remote_User", "remote_user", "REMOTE-USER", "Rip-Site", "Rip_Site", "rip_site"} {
		r.Header[name] = append(r.Header[name], "root")
	}
	return r
}

func TestIdentityHeadersStrippedOnAuthOffSite(t *testing.T) {
	dp, seen := identityUpstream(t)
	h := &Handler{dp: dp, logger: zap.NewNop()}
	serveSite(t, h, forgedIdentityRequest("/anything"))
	if got := <-seen; len(got) != 0 {
		t.Fatalf("auth-off site forwarded client identity headers: %v", got)
	}
}

func TestIdentityHeadersStrippedOnAuthOnSite(t *testing.T) {
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/private/": {"alice"}})
	dp, seen := identityUpstream(t)
	ah.h.dp = dp
	serveSite(t, ah.h, forgedIdentityRequest("/public"))
	if got := <-seen; len(got) != 0 {
		t.Fatalf("auth-on open path forwarded client identity headers: %v", got)
	}
	session := ah.loginDoor(t, "app.test", "/private/auth", "alice", "open-sesame")
	r := forgedIdentityRequest("/private/x")
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	serveSite(t, ah.h, r)
	if got := <-seen; len(got) != 1 || got[0] != "remote-user=alice" {
		t.Fatalf("auth-on gated path: upstream saw %v, want only the injected remote-user=alice", got)
	}
}

func TestStripClientIdentityHeaders(t *testing.T) {
	h := http.Header{
		"Remote-User": {"a"}, "Remote_user": {"b"}, "remote_user": {"c"}, "REMOTE_USER": {"d"},
		"Rip-Site": {"e"}, "Rip_site": {"f"}, "X-Remote-User": {"keep"}, "Remote-Users": {"keep"},
	}
	stripClientIdentityHeaders(h)
	if len(h) != 2 || h.Get("X-Remote-User") != "keep" || h.Get("Remote-Users") != "keep" {
		t.Fatalf("after strip: %v", h)
	}
}

// FuzzAuthGateCanonical pins the over-gate-only property: for any path p,
// if the canonical form of p (or p itself) is under a gate prefix, the
// wall treats p as gated — a request without a session never passes.
func FuzzAuthGateCanonical(f *testing.F) {
	for _, seed := range []string{
		"/", "//", "/reports//phi/x.txt", "//one/secret", "/one;x/secret",
		"/ONE/a", "/a//b/", "/x/../one/", "/one%2Fx", `/one\x`, "/ones", "/one/auth",
		"/a/b;c//d", "", "*",
	} {
		f.Add(seed)
	}
	st, err := newAuthStore()
	if err != nil {
		f.Fatal(err)
	}
	gates := []AuthGate{
		{Prefix: "/one/", Allow: []string{"alice"}},
		{Prefix: "/reports/phi/", Allow: []string{"alice"}},
		{Prefix: "/a/b/", Allow: []string{"alice"}},
	}
	cfg, err := compileAuthSite([]AuthUser{{Name: "alice", Credential: "unused"}}, gates, authDefaultTTL, st)
	if err != nil {
		f.Fatal(err)
	}
	h := &Handler{logger: zap.NewNop(), authCfg: cfg}
	f.Fuzz(func(t *testing.T, p string) {
		canon := authCanonicalPath(p)
		under := false
		for _, g := range gates {
			if gatePrefixMatch(g.Prefix, canon) || strings.HasPrefix(strings.ToLower(p), g.Prefix) {
				under = true
			}
		}
		if !under {
			return
		}
		if cfg.matchGate(p) == nil {
			t.Fatalf("path %q (canonical %q) is under a gate but matchGate found none", p, canon)
		}
		r := &http.Request{
			Method: http.MethodGet, Host: "app.test",
			URL: &url.URL{Path: p}, Header: http.Header{},
			TLS: &tls.ConnectionState{}, RemoteAddr: "192.0.2.1:1",
		}
		rr := httptest.NewRecorder()
		_, handled, err := h.serveAuthWall(rr, r)
		if err != nil {
			t.Fatalf("wall error for %q: %v", p, err)
		}
		if !handled {
			t.Fatalf("path %q (canonical %q) passed the wall without a session", p, canon)
		}
	})
}
