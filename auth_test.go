package janus

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"go.uber.org/zap"
)

// --- fixture credentials ------------------------------------------------------

var (
	authTestCredsOnce sync.Once
	authTestCreds     map[string]string // password → g1 blob
)

func testCred(t *testing.T, password string) string {
	t.Helper()
	authTestCredsOnce.Do(func() {
		authTestCreds = map[string]string{}
		for _, pw := range []string{"open-sesame", "carol-pass", "larry-pass"} {
			blob, err := g1Mint(pw)
			if err != nil {
				panic(err)
			}
			authTestCreds[pw] = blob
		}
	})
	blob, ok := authTestCreds[password]
	if !ok {
		t.Fatalf("no fixture credential for password %q", password)
	}
	return blob
}

// --- harness -------------------------------------------------------------------

type authHarness struct {
	h  *Handler
	st *authStore
}

func newAuthHarness(t *testing.T, users map[string]string, ttl time.Duration) *authHarness {
	t.Helper()
	return newAuthHarnessGates(t, users, ttl, map[string][]string{"/": keysOf(users)})
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func newAuthHarnessGates(t *testing.T, users map[string]string, ttl time.Duration, gates map[string][]string) *authHarness {
	t.Helper()
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	if ttl == 0 {
		ttl = authDefaultTTL
	}
	var ag []AuthGate
	for p, allow := range gates {
		ag = append(ag, AuthGate{Prefix: p, Allow: append([]string(nil), allow...)})
	}
	as := &AuthSettings{Users: usersToSlice(users), Gates: ag}
	if err := finalizeAuthSettings(as); err != nil {
		t.Fatal(err)
	}
	cfg, err := compileAuthSite(as.Users, as.Gates, ttl, st)
	if err != nil {
		t.Fatal(err)
	}
	return &authHarness{h: &Handler{logger: zap.NewNop(), authCfg: cfg}, st: st}
}

func usersToSlice(users map[string]string) []AuthUser {
	out := make([]AuthUser, 0, len(users))
	for n, c := range users {
		out = append(out, AuthUser{Name: n, Credential: c})
	}
	return out
}

func aliceUsers(t *testing.T) map[string]string {
	return map[string]string{"alice": testCred(t, "open-sesame")}
}

func authReq(method, host, target string, body io.Reader, kv ...string) *http.Request {
	r := httptest.NewRequest(method, "https://"+host+target, body)
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	return r
}

func (ah *authHarness) wall(t *testing.T, r *http.Request) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()
	rr := httptest.NewRecorder()
	out, handled, err := ah.h.serveAuthWall(rr, r)
	if err != nil {
		t.Fatalf("serveAuthWall: %v", err)
	}
	if handled {
		return rr, nil
	}
	return rr, out
}

var csrfFieldRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (ah *authHarness) getLoginForm(t *testing.T, host, door string) string {
	t.Helper()
	if door == "" {
		door = "/auth"
	}
	rr, _ := ah.wall(t, authReq(http.MethodGet, host, door, nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET %s: %d", door, rr.Code)
	}
	m := csrfFieldRE.FindStringSubmatch(rr.Body.String())
	if m == nil {
		t.Fatalf("no csrf field in login form: %s", rr.Body.String())
	}
	cookie := authCookieValue(rr, authCSRFCookieName)
	if cookie != m[1] {
		t.Fatalf("csrf cookie %q != field %q", cookie, m[1])
	}
	return m[1]
}

func authCookieValue(rr *httptest.ResponseRecorder, name string) string {
	for _, c := range rr.Result().Cookies() {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

func (ah *authHarness) login(t *testing.T, host, user, password string) string {
	t.Helper()
	return ah.loginDoor(t, host, "/auth", user, password)
}

func (ah *authHarness) loginDoor(t *testing.T, host, door, user, password string) string {
	t.Helper()
	csrf := ah.getLoginForm(t, host, door)
	rr := ah.postFormDoor(t, host, door, url.Values{
		"csrf": {csrf}, "user": {user}, "password": {password},
	}, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login POST %s: %d body=%s", door, rr.Code, rr.Body.String())
	}
	session := authCookieValue(rr, authCookieName)
	if session == "" {
		t.Fatal("login set no session cookie")
	}
	return session
}

func (ah *authHarness) postForm(t *testing.T, host string, vals url.Values, csrfCookie string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	return ah.postFormDoor(t, host, "/auth", vals, csrfCookie, kv...)
}

func (ah *authHarness) postFormDoor(t *testing.T, host, door string, vals url.Values, csrfCookie string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := authReq(http.MethodPost, host, door, strings.NewReader(vals.Encode()),
		append([]string{"Content-Type", "application/x-www-form-urlencoded"}, kv...)...)
	if csrfCookie != "" {
		r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrfCookie})
	}
	rr, _ := ah.wall(t, r)
	return rr
}

// --- parse ------------------------------------------------------------------------

func jblock(lines string) string {
	return "janus {\n" + lines + "\n}"
}

func TestAuthParseLegalLines(t *testing.T) {
	blob := testCred(t, "open-sesame")
	cases := []struct {
		name string
		cf   string
		want func(*AuthSettings) bool
	}{
		{"bare", jblock(`auth`), func(a *AuthSettings) bool {
			return a != nil && a.Enabled != nil && *a.Enabled && !a.Replace && len(a.Users) == 0
		}},
		{"on", jblock(`auth on`), func(a *AuthSettings) bool {
			return a != nil && *a.Enabled && !a.Replace
		}},
		{"off", jblock(`auth off`), func(a *AuthSettings) bool {
			return a != nil && !*a.Enabled
		}},
		{"users block + gate", jblock(`auth {
			users {
				alice ` + blob + `
				bob ` + blob + `
			}
			ttl 2h
			gate /one/ { alice bob }
		}`), func(a *AuthSettings) bool {
			return a != nil && *a.Enabled && a.Replace && len(a.Users) == 2 &&
				len(a.Gates) == 1 && a.Gates[0].Prefix == "/one/" &&
				a.TTL != nil && time.Duration(*a.TTL) == 2*time.Hour
		}},
		{"top-level user + gate /", jblock(`auth {
			user alice ` + blob + `
			gate / { alice }
		}`), func(a *AuthSettings) bool {
			return a != nil && a.Replace && len(a.Users) == 1 &&
				a.Gates[0].Prefix == "/" && a.Gates[0].Allow[0] == "alice"
		}},
		{"normalize trailing slash", jblock(`auth {
			user alice ` + blob + `
			gate /foo/bar/baz { alice }
		}`), func(a *AuthSettings) bool {
			return a != nil && a.Gates[0].Prefix == "/foo/bar/baz/"
		}},
		{"lowercased username", jblock(`auth {
			user ALICE ` + blob + `
			gate / { ALICE }
		}`), func(a *AuthSettings) bool {
			return a != nil && a.Users[0].Name == "alice" && a.Gates[0].Allow[0] == "alice"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tc.cf)
			app := new(App)
			if err := app.UnmarshalCaddyfile(d); err != nil {
				t.Fatal(err)
			}
			if !tc.want(app.Auth) {
				t.Fatalf("parse of %q: %+v", tc.cf, app.Auth)
			}
			var h Handler
			if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(tc.cf)); err != nil {
				t.Fatalf("site parse of %q: %v", tc.cf, err)
			}
			if !tc.want(h.Auth) {
				t.Fatalf("site parse of %q: %+v", tc.cf, h.Auth)
			}
		})
	}
}

func TestAuthParseHardErrors(t *testing.T) {
	blob := testCred(t, "open-sesame")
	padded := blob + "=="
	cases := []string{
		jblock(`auth maybe`),
		jblock(`auth on off`),
		jblock("auth off {\n user alice " + blob + "\n gate / { alice }\n}"),
		jblock("auth\nauth off"),
		jblock("auth {\n}"),
		jblock("auth {\n bogus 1\n}"),
		jblock("auth {\n user\n}"),
		jblock("auth {\n user alice\n}"),
		jblock("auth {\n user alice " + blob + " extra\n}"),
		jblock("auth {\n user alice " + blob + "\n user alice " + blob + "\n}"),
		jblock("auth {\n user \"\" " + blob + "\n}"),
		jblock("auth {\n user al:ice " + blob + "\n}"),
		jblock("auth {\n user " + strings.Repeat("a", 65) + " " + blob + "\n}"),
		jblock("auth {\n user alice plaintext\n}"),
		jblock("auth {\n user alice g2:" + strings.Repeat("A", 64) + "\n}"),
		jblock("auth {\n user alice g1:not-base64!!\n}"),
		jblock("auth {\n user alice g1:" + strings.Repeat("A", 44) + "\n}"),
		jblock("auth {\n user alice " + padded + "\n}"),
		jblock("auth {\n ttl\n}"),
		jblock("auth {\n ttl 0\n}"),
		jblock("auth {\n ttl -5m\n}"),
		jblock("auth {\n ttl nope\n}"),
		jblock("auth {\n ttl 1h\n ttl 2h\n}"),
		jblock("auth {\n ttl 1h {\n nested\n }\n}"),
		jblock("auth {\n user alice " + blob + "\n gate\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /one/\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /one/ {\n}\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /one/ { bob }\n}"), // bob missing
		jblock("auth {\n user alice " + blob + "\n gate ones { alice }\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /a//b/ { alice }\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /a/../b/ { alice }\n}"),
		jblock("auth {\n user alice " + blob + "\n gate /one/ { alice }\n gate /one { alice }\n}"), // dup after norm
		jblock("auth {\n user alice " + blob + "\n gate /one/ { alice }\n gate /one/auth/ { alice }\n}"), // shadow
	}
	for _, cf := range cases {
		if err := new(App).UnmarshalCaddyfile(caddyfile.NewTestDispenser(cf)); err == nil {
			t.Errorf("global parse accepted %q", cf)
		}
		var h Handler
		if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(cf)); err == nil {
			t.Errorf("site parse accepted %q", cf)
		}
	}
}

// --- the g1 codec -------------------------------------------------------------------

func TestG1MintVerifyRoundTrip(t *testing.T) {
	blob := testCred(t, "open-sesame")
	if !strings.HasPrefix(blob, g1Prefix) {
		t.Fatalf("minted blob %q lacks the g1: prefix", blob)
	}
	if got := len(blob) - len(g1Prefix); got != g1EncLen {
		t.Fatalf("minted blob encodes %d chars, want %d", got, g1EncLen)
	}
	if err := validateG1(blob); err != nil {
		t.Fatalf("minted blob rejected: %v", err)
	}
	if !g1Verify("open-sesame", blob) {
		t.Fatal("correct password rejected")
	}
	if g1Verify("wrong", blob) {
		t.Fatal("wrong password accepted")
	}
}

func TestG1Rejections(t *testing.T) {
	good := strings.TrimPrefix(testCred(t, "open-sesame"), g1Prefix)
	cases := []struct {
		name string
		blob string
		want string
	}{
		{"missing prefix", good, "missing the g1: prefix"},
		{"unknown tag", "g2:" + good, "unknown version tag"},
		{"padding", g1Prefix + good + "==", "no padding"},
		{"url alphabet", g1Prefix + strings.Repeat("-", g1EncLen), "no padding"},
		{"bad base64", g1Prefix + "!!!!", "invalid base64"},
		{"short", g1Prefix + good[:44], "decoded 33 bytes, want 48"},
		{"long", g1Prefix + good + "AAAA", "decoded 51 bytes, want 48"},
		{"empty", g1Prefix, "decoded 0 bytes, want 48"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateG1(tc.blob)
			if err == nil {
				t.Fatalf("accepted %q", tc.blob)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not name %q", err, tc.want)
			}
		})
	}
}

// --- safeTo + path match ------------------------------------------------------------

func TestSafeTo(t *testing.T) {
	long := "/" + strings.Repeat("a", authToCap)
	cases := []struct {
		raw, prefix, want string
	}{
		{"", "/one/", "/one/"},
		{"/one/reports", "/one/", "/one/reports"},
		{"/one/reports?q=1", "/one/", "/one/reports?q=1"},
		{"/other", "/one/", "/one/"},
		{"//evil.example", "/one/", "/one/"},
		{`/\evil.example`, "/", "/"},
		{"/has space", "/", "/"},
		{long, "/", "/"},
		{"/reports", "/", "/reports"},
	}
	for _, tc := range cases {
		if got := safeTo(tc.raw, tc.prefix); got != tc.want {
			t.Errorf("safeTo(%q, %q) = %q, want %q", tc.raw, tc.prefix, got, tc.want)
		}
	}
}

func TestGatePathMatches(t *testing.T) {
	cases := []struct {
		prefix, path string
		want         bool
	}{
		{"/one/", "/one", true},
		{"/one/", "/one/", true},
		{"/one/", "/one/two", true},
		{"/one/", "/ones", false},
		{"/", "/anything", true},
		{"/", "/", true},
		{"/foo/bar/baz/", "/foo/bar/baz", true},
		{"/foo/bar/baz/", "/foo/bar/baz/x", true},
		{"/foo/bar/baz/", "/foo/bar/ba", false},
	}
	for _, tc := range cases {
		if got := gatePathMatches(tc.prefix, tc.path); got != tc.want {
			t.Errorf("gatePathMatches(%q, %q) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}
}

// --- endpoint + wall ---------------------------------------------------------------

func TestAuthEndpointDispatch(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)

	t.Run("GET no session → login form", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/auth", nil))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Sign in") {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
	})

	t.Run("login round trip", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		if user, _, ok := ah.st.lookup(session, time.Hour); !ok || user != "alice" {
			t.Fatalf("minted session does not resolve: %q %v", user, ok)
		}
	})

	t.Run("status + sign-out", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/auth", nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr, _ := ah.wall(t, r)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "alice") {
			t.Fatalf("status page: %d %s", rr.Code, rr.Body.String())
		}
		csrf := authCookieValue(rr, authCSRFCookieName)
		out := authReq(http.MethodPost, "app.test", "/auth",
			strings.NewReader(url.Values{"csrf": {csrf}}.Encode()),
			"Content-Type", "application/x-www-form-urlencoded")
		out.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
		out.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr2, _ := ah.wall(t, out)
		if rr2.Code != http.StatusSeeOther || rr2.Header().Get("Location") != "/auth" {
			t.Fatalf("sign-out: %d %q", rr2.Code, rr2.Header().Get("Location"))
		}
	})

	t.Run("safeTo under gate prefix", func(t *testing.T) {
		csrf := ah.getLoginForm(t, "app.test", "/auth")
		rr := ah.postForm(t, "app.test", url.Values{
			"csrf": {csrf}, "user": {"alice"}, "password": {"open-sesame"},
			"to": {"//evil.example"},
		}, csrf)
		if rr.Header().Get("Location") != "/" {
			t.Fatalf("hostile to → %q, want /", rr.Header().Get("Location"))
		}
	})

	t.Run("CSRF enforced", func(t *testing.T) {
		rr := ah.postForm(t, "app.test", url.Values{"user": {"alice"}, "password": {"open-sesame"}}, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("want 403, got %d", rr.Code)
		}
	})

	t.Run("other methods → 405", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodPut, "app.test", "/auth", nil))
		if rr.Code != http.StatusMethodNotAllowed {
			t.Fatalf("want 405, got %d", rr.Code)
		}
	})
}

func TestAuthPrefixGates(t *testing.T) {
	users := map[string]string{
		"alice": testCred(t, "open-sesame"),
		"bob":   testCred(t, "open-sesame"),
		"larry": testCred(t, "larry-pass"),
	}
	ah := newAuthHarnessGates(t, users, 0, map[string][]string{
		"/":             {"alice", "bob"},
		"/one/":         {"alice", "larry"},
		"/foo/bar/baz/": {"bob", "larry"},
	})

	t.Run("longest prefix wins", func(t *testing.T) {
		// bob may pass gate / but not /one/
		session := ah.login(t, "app.test", "bob", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/one/x", nil, "Accept", "text/html")
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr, out := ah.wall(t, r)
		if out != nil {
			t.Fatal("bob passed /one/ gate")
		}
		if rr.Code != http.StatusFound || !strings.HasPrefix(rr.Header().Get("Location"), "/one/auth?") {
			t.Fatalf("want 302 to /one/auth, got %d %q", rr.Code, rr.Header().Get("Location"))
		}
		// bob passes /
		r = authReq(http.MethodGet, "app.test", "/reports", nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		if _, out = ah.wall(t, r); out == nil {
			t.Fatal("bob blocked on gate /")
		}
	})

	t.Run("one login opens every allow-listed gate", func(t *testing.T) {
		session := ah.loginDoor(t, "app.test", "/one/auth", "alice", "open-sesame")
		for _, p := range []string{"/", "/one/x", "/foo/bar/baz/y"} {
			// alice is not on /foo/bar/baz — only / and /one/
			r := authReq(http.MethodGet, "app.test", p, nil)
			r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
			_, out := ah.wall(t, r)
			wantOut := p != "/foo/bar/baz/y"
			if (out != nil) != wantOut {
				t.Fatalf("path %s: fallthrough=%v want %v", p, out != nil, wantOut)
			}
		}
	})

	t.Run("login door requires gate allow list", func(t *testing.T) {
		csrf := ah.getLoginForm(t, "app.test", "/one/auth")
		rr := ah.postFormDoor(t, "app.test", "/one/auth", url.Values{
			"csrf": {csrf}, "user": {"bob"}, "password": {"open-sesame"},
		}, csrf)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("bob minting via /one/auth: want 401, got %d", rr.Code)
		}
	})

	t.Run("open path outside gates strips only", func(t *testing.T) {
		ah2 := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{
			"/one/": {"alice"},
		})
		r := authReq(http.MethodGet, "app.test", "/public", nil, "Remote-User", "root")
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: "x"})
		r.AddCookie(&http.Cookie{Name: "sid", Value: "1"})
		_, out := ah2.wall(t, r)
		if out == nil {
			t.Fatal("open path must fall through")
		}
		if out.Header.Get("Remote-User") != "" {
			t.Fatal("open path must not inject Remote-User")
		}
		if strings.Contains(out.Header.Get("Cookie"), authCookieName) {
			t.Fatal("auth cookies must be stripped on open fall-through")
		}
	})

	t.Run("/one/auth/ is gated traffic, not the door", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/one/auth/", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("trailing-slash door variant: want 401, got %d", rr.Code)
		}
	})
}

func TestAuthWallFork(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)

	t.Run("browser-shaped → 302", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/reports?q=1", nil,
			"Accept", "text/html"))
		if rr.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/auth?to=%2Freports%3Fq%3D1" {
			t.Fatalf("Location %q", loc)
		}
	})

	t.Run("API → 401", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/api", nil))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
	})

	t.Run("valid session falls through", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/data", nil, "Remote-User", "root")
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		r.AddCookie(&http.Cookie{Name: "sid", Value: "42"})
		_, out := ah.wall(t, r)
		if out == nil {
			t.Fatal("did not fall through")
		}
		if out.Header.Get("Remote-User") != "alice" {
			t.Fatalf("Remote-User = %q", out.Header.Get("Remote-User"))
		}
		if strings.Contains(out.Header.Get("Cookie"), authCookieName) {
			t.Fatal("session cookie rode through")
		}
		if authIdentityOf(out.Context()) != "alice" {
			t.Fatal("cache bypass signal missing")
		}
	})
}

func TestAuthPlainHTTPDeadWall(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	rr, out := ah.wall(t, httptest.NewRequest(http.MethodGet, "http://app.test/x", nil))
	if out != nil || rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("want 421, got fallthrough=%v code=%d", out != nil, rr.Code)
	}
	// Open paths are not exempt.
	ah2 := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/one/": {"alice"}})
	rr, _ = ah2.wall(t, httptest.NewRequest(http.MethodGet, "http://app.test/public", nil))
	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("open path plain HTTP: want 421, got %d", rr.Code)
	}
}

// --- cascade -----------------------------------------------------------------------

func TestAuthCascadeResolution(t *testing.T) {
	blob := testCred(t, "open-sesame")
	carol := testCred(t, "carol-pass")
	on, off := true, false
	ttl2h := caddy.Duration(2 * time.Hour)

	app := func(g *AuthSettings) *App {
		st, err := newAuthStore()
		if err != nil {
			t.Fatal(err)
		}
		return &App{Auth: g, state: &janusState{auth: st}}
	}
	full := func(users []AuthUser, gates []AuthGate, ttl *caddy.Duration) *AuthSettings {
		return &AuthSettings{Enabled: &on, Replace: true, Users: users, Gates: gates, TTL: ttl}
	}
	cases := []struct {
		name       string
		global     *AuthSettings
		site       *AuthSettings
		wantOn     bool
		wantUsers  []string
		wantGates  int
		wantTTL    time.Duration
		wantErrSub string
	}{
		{"unset/unset → off", nil, nil, false, nil, 0, 0, ""},
		{"global on / unset → inherit", full([]AuthUser{{"alice", blob}}, []AuthGate{{"/", []string{"alice"}}}, nil), nil,
			true, []string{"alice"}, 1, authDefaultTTL, ""},
		{"global on / site off → off", full([]AuthUser{{"alice", blob}}, []AuthGate{{"/", []string{"alice"}}}, nil), &AuthSettings{Enabled: &off},
			false, nil, 0, 0, ""},
		{"site replaces global", full([]AuthUser{{"alice", blob}}, []AuthGate{{"/", []string{"alice"}}}, nil),
			full([]AuthUser{{"carol", carol}}, []AuthGate{{"/admin/", []string{"carol"}}}, &ttl2h),
			true, []string{"carol"}, 1, 2 * time.Hour, ""},
		{"bare auth inherits", full([]AuthUser{{"alice", blob}}, []AuthGate{{"/", []string{"alice"}}}, nil),
			&AuthSettings{Enabled: &on},
			true, []string{"alice"}, 1, authDefaultTTL, ""},
		{"bare auth with no global → error", nil, &AuthSettings{Enabled: &on},
			false, nil, 0, 0, "no auth config to inherit"},
		{"zero gates", &AuthSettings{Enabled: &on, Replace: true, Users: []AuthUser{{"alice", blob}}}, nil,
			false, nil, 0, 0, "incomplete"},
		{"zero users", &AuthSettings{Enabled: &on, Replace: true, Gates: []AuthGate{{"/", []string{"alice"}}}}, nil,
			false, nil, 0, 0, ""}, // validate fails earlier (allow name missing) or incomplete
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &Handler{logger: zap.NewNop(), app: app(tc.global), Auth: tc.site}
			err := h.provisionAuth()
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("want error naming %q, got %v", tc.wantErrSub, err)
				}
				return
			}
			if err != nil {
				// zero users with gate referencing missing user fails validate
				if tc.name == "zero users" {
					return
				}
				t.Fatal(err)
			}
			if (h.authCfg != nil) != tc.wantOn {
				t.Fatalf("effective on = %v, want %v", h.authCfg != nil, tc.wantOn)
			}
			if !tc.wantOn {
				return
			}
			if len(h.authCfg.users) != len(tc.wantUsers) {
				t.Fatalf("users = %v, want %v", h.authCfg.users, tc.wantUsers)
			}
			if len(h.authCfg.gates) != tc.wantGates {
				t.Fatalf("gates = %d, want %d", len(h.authCfg.gates), tc.wantGates)
			}
			if h.authCfg.ttl != tc.wantTTL {
				t.Fatalf("ttl = %v, want %v", h.authCfg.ttl, tc.wantTTL)
			}
		})
	}
}

// --- session store -------------------------------------------------------------------

func TestAuthSessionStore(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.now = func() time.Time { return now }

	token, err := st.mint("alice", "app.test")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Minute)
	if _, _, ok := st.lookup(token, time.Hour); !ok {
		t.Fatal("live session missed")
	}
	now = now.Add(2 * time.Hour)
	if _, _, ok := st.lookup(token, time.Hour); ok {
		t.Fatal("idle session survived past ttl")
	}

	tokA, _ := st.mint("alice", "a.test")
	tokB, _ := st.mint("bob", "b.test")
	if n := st.revokeUsersNotIn(map[string]bool{"alice": true}); n != 1 {
		t.Fatalf("revokeUsersNotIn = %d, want 1", n)
	}
	if _, _, ok := st.lookup(tokA, time.Hour); !ok {
		t.Fatal("kept user's session revoked")
	}
	if _, _, ok := st.lookup(tokB, time.Hour); ok {
		t.Fatal("removed user's session survived")
	}

	list := st.sessionListRaw()
	if len(list) != 1 || list[0].User != "alice" || list[0].Host != "a.test" {
		t.Fatalf("session list: %+v", list)
	}
	if list[0].Gates == nil {
		t.Fatal("gates must be a non-nil array")
	}
}

func TestAuthSessionIDPrefixResolution(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := st.mint("alice", "app.test")
	list := st.sessionListRaw()
	if len(list) != 1 || len(list[0].ID) != 12 {
		t.Fatalf("session list: %+v", list)
	}
	if found, _ := st.revokeID(list[0].ID); !found {
		t.Fatal("listed id failed to revoke")
	}
	if _, _, ok := st.lookup(tok, time.Hour); ok {
		t.Fatal("revoked session still resolves")
	}
}

// --- throttle ladder ----------------------------------------------------------------

func TestAuthThrottleLadder(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.now = func() time.Time { return now }
	gate, client, user := "/", "203.0.113.9", "alice"

	for i := 1; i <= 3; i++ {
		if blocked, _ := st.throttleCheck(gate, client, user); blocked {
			t.Fatalf("blocked after %d fails", i-1)
		}
		st.throttleFail(gate, client, user)
	}
	// 4th failure sets a 10s wait; the failure itself still returns (caller renders 401).
	st.throttleFail(gate, client, user)
	blocked, retry := st.throttleCheck(gate, client, user)
	if !blocked || retry < 9*time.Second {
		t.Fatalf("after 4th fail: blocked=%v retry=%v", blocked, retry)
	}
	// 429 wait does not increment.
	st.mu.Lock()
	failsBefore := st.throttle[authThrottlePairKey(gate, client, user)].fails
	st.mu.Unlock()
	if blocked, _ := st.throttleCheck(gate, client, user); !blocked {
		t.Fatal("expected still blocked")
	}
	st.mu.Lock()
	failsAfter := st.throttle[authThrottlePairKey(gate, client, user)].fails
	st.mu.Unlock()
	if failsAfter != failsBefore {
		t.Fatalf("429 wait incremented fails: %d → %d", failsBefore, failsAfter)
	}

	now = now.Add(11 * time.Second)
	st.throttleFail(gate, client, user) // 5th
	if blocked, retry = st.throttleCheck(gate, client, user); !blocked || retry < 59*time.Second {
		t.Fatalf("after 5th: blocked=%v retry=%v", blocked, retry)
	}
	now = now.Add(61 * time.Second)
	st.throttleFail(gate, client, user) // 6th
	now = now.Add(301 * time.Second)
	st.throttleFail(gate, client, user) // 7th → ban
	if blocked, retry = st.throttleCheck(gate, client, user); !blocked || retry < 23*time.Hour {
		t.Fatalf("ban: blocked=%v retry=%v", blocked, retry)
	}
	if st.banned.Load() < 1 {
		t.Fatal("banned counter unmoved")
	}

	// Aggregate: spray across usernames shares the client budget.
	st2, _ := newAuthStore()
	st2.now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		st2.throttleFail(gate, client, "u"+itoa(i))
	}
	if blocked, _ := st2.throttleCheck(gate, client, "fresh"); !blocked {
		t.Fatal("aggregate ladder did not block a fresh username")
	}
}

func TestAuthClientKeyIPv6Prefix(t *testing.T) {
	a := authClientKey("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443")
	b := authClientKey("[2001:db8:1:2:1111:2222:3333:4444]:443")
	if a != b {
		t.Fatalf("same /64 must share a key: %q vs %q", a, b)
	}
}

func TestAuthThrottleBeforeKDF(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	gate, client := "/", "192.0.2.1"
	for i := 0; i < 4; i++ {
		ah.st.throttleFail(gate, client, "alice")
	}
	csrf := ah.getLoginForm(t, "app.test", "/auth")
	before := authKDFRuns.Load()
	r := authReq(http.MethodPost, "app.test", "/auth",
		strings.NewReader(url.Values{"csrf": {csrf}, "user": {"alice"}, "password": {"open-sesame"}}.Encode()),
		"Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "192.0.2.1:9999"
	r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
	rr, _ := ah.wall(t, r)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d", rr.Code)
	}
	if authKDFRuns.Load() != before {
		t.Fatal("throttled attempt performed argon2 work")
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.ReplaceAll(time.Duration(i).String(), "ns", ""))
}

func TestAuthTimingEqualization(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	ah.st.verifyCredential("warmup", "", false)
	before := authKDFRuns.Load()
	ah.st.verifyCredential("wrong", ah.h.authCfg.users["alice"], true)
	wrongPW := authKDFRuns.Load() - before
	before = authKDFRuns.Load()
	ah.st.verifyCredential("whatever", "", false)
	unknownUser := authKDFRuns.Load() - before
	if wrongPW != 1 || unknownUser != 1 {
		t.Fatalf("KDF runs: wrong=%d unknown=%d", wrongPW, unknownUser)
	}
}

func TestAuthVerifySemaphoreAndQueueBound(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	total := authVerifyConcurrency + authVerifyQueueMax + 10
	var admitted, rejected atomic.Int64
	var wg sync.WaitGroup
	release := make(chan struct{})
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !st.acquireVerify() {
				rejected.Add(1)
				return
			}
			admitted.Add(1)
			<-release
			st.releaseVerify()
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for admitted.Load() < int64(authVerifyConcurrency) ||
		rejected.Load() < int64(total-authVerifyConcurrency-authVerifyQueueMax) {
		if time.Now().After(deadline) {
			t.Fatalf("flood never settled: admitted=%d rejected=%d", admitted.Load(), rejected.Load())
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()
}

func TestAuthOrderingPingBeforeWall(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	on := true
	ah.h.Ping = &on
	rr := httptest.NewRecorder()
	if err := ah.h.ServeHTTP(rr, authReq(http.MethodGet, "app.test", "/ping", nil), nil); err != nil {
		t.Fatal(err)
	}
	if rr.Code != 200 || rr.Body.String() != "pong\n" {
		t.Fatalf("ping: %d %q", rr.Code, rr.Body.String())
	}
}

func TestAuthOrderingWallBeforeHub(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	ah.h.hubCfg = &hubSite{path: "/hub", maxConns: 4, maxFrame: hubDefaultMaxFrame, maxChannels: 4, originAny: true}
	rr := httptest.NewRecorder()
	r := authReq(http.MethodGet, "app.test", "/hub", nil,
		"Connection", "Upgrade", "Upgrade", "websocket",
		"Sec-WebSocket-Version", "13", "Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	if err := ah.h.ServeHTTP(rr, r, nil); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestAuthCacheBypassSignalExplicit(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "https://app.test/x", nil)
	if cacheBypassRequest(r) {
		t.Fatal("anonymous GET must not bypass")
	}
	r = r.WithContext(context.WithValue(r.Context(), authIdentityKey{}, "alice"))
	if !cacheBypassRequest(r) {
		t.Fatal("authenticated GET must bypass")
	}
}

func TestAuthenticatedTrafficNeverCached(t *testing.T) {
	reg := newAppRegistry()
	dp := newDataPlane(reg, nil)
	store := newCacheStore(defaultCacheMaxBytes, defaultCacheAppShare)
	reg.setPurge(store.purgeApp)
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []http.Header
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Clone())
		mu.Unlock()
		w.Write([]byte("per-identity"))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})
	users := aliceUsers(t)
	cfg, err := compileAuthSite(usersToSlice(users), []AuthGate{{Prefix: "/", Allow: []string{"alice"}}}, authDefaultTTL, st)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{
		logger: zap.NewNop(),
		dp:     dp,
		cacheCfg: &cacheSite{
			store: store, ttl: time.Minute, ttlMax: 10 * time.Minute,
			maxBody: defaultCacheMaxBody, debug: true,
		},
		authCfg: cfg,
	}
	ah := &authHarness{h: h, st: st}
	session := ah.login(t, "app.test", "alice", "open-sesame")
	for i := 0; i < 3; i++ {
		r := authReq(http.MethodGet, "app.test", "/private", nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr := httptest.NewRecorder()
		if err := h.ServeHTTP(rr, r, nil); err != nil {
			t.Fatal(err)
		}
		if v := rr.Header().Get(cacheDebugHeader); v != "BYPASS" {
			t.Fatalf("cache verdict %q, want BYPASS", v)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("upstream saw %d, want 3", len(seen))
	}
}

func TestAuthRespStripReservedCookies(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &authRespStrip{ResponseWriter: rec}
	w.Header().Add("Set-Cookie", "__Host-janus=evil")
	w.Header().Add("Set-Cookie", "__Host-janus_csrf=evil")
	w.Header().Add("Set-Cookie", "sid=ok")
	w.WriteHeader(200)
	got := rec.Header().Values("Set-Cookie")
	if len(got) != 1 || got[0] != "sid=ok" {
		t.Fatalf("Set-Cookie after strip: %v", got)
	}
}
