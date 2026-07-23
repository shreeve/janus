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
//
// Real argon2 at the g1 constants costs tens of milliseconds; the suite
// mints one credential per password and reuses it.

var (
	authTestCredsOnce sync.Once
	authTestCreds     map[string]string // password → g1 blob
)

func testCred(t *testing.T, password string) string {
	t.Helper()
	authTestCredsOnce.Do(func() {
		authTestCreds = map[string]string{}
		for _, pw := range []string{"open-sesame", "carol-pass"} {
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
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	if ttl == 0 {
		ttl = authDefaultTTL
	}
	h := &Handler{
		logger:  zap.NewNop(),
		authCfg: &authSite{users: users, ttl: ttl, store: st},
	}
	return &authHarness{h: h, st: st}
}

func aliceUsers(t *testing.T) map[string]string {
	return map[string]string{"alice": testCred(t, "open-sesame")}
}

// req builds an HTTPS request (r.TLS non-nil) with optional k, v header
// pairs.
func authReq(method, host, target string, body io.Reader, kv ...string) *http.Request {
	r := httptest.NewRequest(method, "https://"+host+target, body)
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Add(kv[i], kv[i+1])
	}
	return r
}

// wall runs serveAuthWall and returns the recorder plus the fall-through
// request (nil when the wall handled the response).
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

var csrfFieldRE = regexp.MustCompile(`name="_csrf" value="([^"]+)"`)

// getLoginForm GETs /auth and returns the CSRF token (cookie == field is
// asserted).
func (ah *authHarness) getLoginForm(t *testing.T, host string) string {
	t.Helper()
	rr, _ := ah.wall(t, authReq(http.MethodGet, host, "/auth", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /auth: %d", rr.Code)
	}
	m := csrfFieldRE.FindStringSubmatch(rr.Body.String())
	if m == nil {
		t.Fatalf("no _csrf field in login form: %s", rr.Body.String())
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

// login runs the full GET-form → POST flow and returns the session
// cookie value.
func (ah *authHarness) login(t *testing.T, host, user, password string) string {
	t.Helper()
	csrf := ah.getLoginForm(t, host)
	rr := ah.postForm(t, host, url.Values{
		"_csrf":    {csrf},
		"user":     {user},
		"password": {password},
	}, csrf)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("login POST: %d body=%s", rr.Code, rr.Body.String())
	}
	session := authCookieValue(rr, authCookieName)
	if session == "" {
		t.Fatal("login set no session cookie")
	}
	return session
}

// postForm POSTs urlencoded values to /auth with the CSRF cookie set
// (pass csrfCookie "" to omit it).
func (ah *authHarness) postForm(t *testing.T, host string, vals url.Values, csrfCookie string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := authReq(http.MethodPost, host, "/auth", strings.NewReader(vals.Encode()),
		append([]string{"Content-Type", "application/x-www-form-urlencoded"}, kv...)...)
	if csrfCookie != "" {
		r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrfCookie})
	}
	rr, _ := ah.wall(t, r)
	return rr
}

// --- parse ------------------------------------------------------------------------

// jblock wraps directive lines in a janus block (multi-line: the
// dispenser treats a same-line "}" as an argument, exactly like a real
// Caddyfile written on one line would).
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
			return a != nil && a.Enabled != nil && *a.Enabled && len(a.Users) == 0 && a.TTL == nil
		}},
		{"on", jblock(`auth on`), func(a *AuthSettings) bool {
			return a != nil && *a.Enabled
		}},
		{"off", jblock(`auth off`), func(a *AuthSettings) bool {
			return a != nil && !*a.Enabled
		}},
		{"block", jblock(`auth {
			user alice ` + blob + `
			user bob ` + blob + `
			ttl 2h
		}`), func(a *AuthSettings) bool {
			return a != nil && *a.Enabled && len(a.Users) == 2 &&
				a.Users[0].Name == "alice" && a.Users[1].Name == "bob" &&
				a.TTL != nil && time.Duration(*a.TTL) == 2*time.Hour
		}},
		{"on block", jblock(`auth on {
			user alice ` + blob + `
		}`), func(a *AuthSettings) bool {
			return a != nil && *a.Enabled && len(a.Users) == 1
		}},
		{"lowercased username", jblock(`auth {
			user ALICE ` + blob + `
		}`), func(a *AuthSettings) bool {
			return a != nil && a.Users[0].Name == "alice"
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
			// The same grammar parses in a site block.
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
		jblock("auth off {\n user alice " + blob + "\n}"),
		jblock("auth\nauth off"), // duplicate directive in one block
		jblock("auth {\n bogus 1\n}"),
		jblock("auth {\n user\n}"),
		jblock("auth {\n user alice\n}"),
		jblock("auth {\n user alice " + blob + " extra\n}"),
		jblock("auth {\n user alice " + blob + "\n user alice " + blob + "\n}"), // duplicate username
		jblock("auth {\n user \"\" " + blob + "\n}"),
		jblock("auth {\n user al:ice " + blob + "\n}"),
		jblock("auth {\n user " + strings.Repeat("a", 65) + " " + blob + "\n}"),
		jblock("auth {\n user alice plaintext\n}"),
		jblock("auth {\n user alice g2:" + strings.Repeat("A", 64) + "\n}"),
		jblock("auth {\n user alice g1:not-base64!!\n}"),
		jblock("auth {\n user alice g1:" + strings.Repeat("A", 44) + "\n}"), // 33 bytes
		jblock("auth {\n user alice " + padded + "\n}"),                    // padding
		jblock("auth {\n ttl\n}"),
		jblock("auth {\n ttl 0\n}"),
		jblock("auth {\n ttl -5m\n}"),
		jblock("auth {\n ttl nope\n}"),
		jblock("auth {\n ttl 1h\n ttl 2h\n}"), // duplicate ttl
		jblock("auth {\n ttl 1h {\n nested\n }\n}"),
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

// --- safeReturnTo -------------------------------------------------------------------

func TestSafeReturnTo(t *testing.T) {
	long := "/" + strings.Repeat("a", authReturnToCap)
	cases := []struct {
		raw, want string
	}{
		{"", "/"},
		{"/", "/"},
		{"/reports", "/reports"},
		{"/reports?q=1&x=2", "/reports?q=1&x=2"},
		{"/a/b/c.html", "/a/b/c.html"},
		{"relative", "/"},
		{"https://evil.example", "/"},
		{"//evil.example", "/"},
		{`/\evil.example`, "/"},
		{"/ok\\evil", "/"},
		{"/has space", "/"},
		{"/has\ttab", "/"},
		{"/has\nnewline", "/"},
		{"/has\x00nul", "/"},
		{"/has\x1fctl", "/"},
		{"/has\x7fdel", "/"},
		{long, "/"},
	}
	for _, tc := range cases {
		if got := safeReturnTo(tc.raw); got != tc.want {
			t.Errorf("safeReturnTo(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

// --- the /auth state machine ----------------------------------------------------------

func TestAuthEndpointDispatch(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)

	t.Run("GET no session → login form, no-store, csrf pair", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/auth", nil))
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Sign in") {
			t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("login form is not no-store")
		}
		if authCookieValue(rr, authCSRFCookieName) == "" {
			t.Fatal("no csrf cookie planted")
		}
	})

	t.Run("HEAD never sets cookies", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodHead, "app.test", "/auth", nil))
		if rr.Code != 200 {
			t.Fatalf("HEAD /auth: %d", rr.Code)
		}
		if len(rr.Result().Cookies()) != 0 {
			t.Fatalf("HEAD set cookies: %v", rr.Result().Cookies())
		}
	})

	t.Run("login round trip → 303 + fresh session cookie", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		if user, _, ok := ah.st.lookup(session, time.Hour); !ok || user != "alice" {
			t.Fatalf("minted session does not resolve: %q %v", user, ok)
		}
	})

	t.Run("status page: signed in, re-minted csrf, sign-out works", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/auth", nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr, _ := ah.wall(t, r)
		if rr.Code != 200 || !strings.Contains(rr.Body.String(), "Signed in as") ||
			!strings.Contains(rr.Body.String(), "alice") {
			t.Fatalf("status page: %d %s", rr.Code, rr.Body.String())
		}
		// The status page re-plants a fresh CSRF pair (login cleared the
		// login form's cookie) so its sign-out form works.
		csrf := authCookieValue(rr, authCSRFCookieName)
		m := csrfFieldRE.FindStringSubmatch(rr.Body.String())
		if csrf == "" || m == nil || m[1] != csrf {
			t.Fatalf("status page csrf pair broken: cookie=%q form=%v", csrf, m)
		}
		// Sign-out: POST with neither user nor password.
		out := authReq(http.MethodPost, "app.test", "/auth",
			strings.NewReader(url.Values{"_csrf": {csrf}}.Encode()),
			"Content-Type", "application/x-www-form-urlencoded")
		out.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
		out.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr2, _ := ah.wall(t, out)
		if rr2.Code != http.StatusSeeOther || rr2.Header().Get("Location") != "/auth" {
			t.Fatalf("sign-out: %d %q", rr2.Code, rr2.Header().Get("Location"))
		}
		if _, _, ok := ah.st.lookup(session, time.Hour); ok {
			t.Fatal("session survived sign-out")
		}
		if ah.st.signouts.Load() == 0 {
			t.Fatal("signouts counter unmoved")
		}
	})

	t.Run("signing out a dead session is a success", func(t *testing.T) {
		csrf := ah.getLoginForm(t, "app.test")
		rr := ah.postForm(t, "app.test", url.Values{"_csrf": {csrf}}, csrf)
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("dead-session sign-out: %d", rr.Code)
		}
	})

	t.Run("dispatch by fields: user-only and password-only and empty values are sign-in attempts", func(t *testing.T) {
		for _, vals := range []url.Values{
			{"user": {"alice"}},
			{"password": {"open-sesame"}},
			{"user": {""}, "password": {""}},
		} {
			csrf := ah.getLoginForm(t, "app.test")
			vals.Set("_csrf", csrf)
			rr := ah.postForm(t, "app.test", vals, csrf)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%v: want 401 re-prompt, got %d", vals, rr.Code)
			}
			if !strings.Contains(rr.Body.String(), "Invalid credentials") {
				t.Fatalf("%v: no generic error in body", vals)
			}
		}
	})

	t.Run("stale login tab: signed-in POST with fields is a login attempt, never a sign-out", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		csrf := ah.getLoginForm(t, "app.test")
		r := authReq(http.MethodPost, "app.test", "/auth",
			strings.NewReader(url.Values{"_csrf": {csrf}, "user": {"alice"}, "password": {"wrong"}}.Encode()),
			"Content-Type", "application/x-www-form-urlencoded")
		r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr, _ := ah.wall(t, r)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("stale-tab login attempt: %d", rr.Code)
		}
		if _, _, ok := ah.st.lookup(session, time.Hour); !ok {
			t.Fatal("stale-tab login attempt revoked the live session")
		}
	})

	t.Run("duplicated fields → 400", func(t *testing.T) {
		csrf := ah.getLoginForm(t, "app.test")
		for _, body := range []string{
			"user=a&user=b&password=x&_csrf=" + url.QueryEscape(csrf),
			"user=a&password=x&password=y&_csrf=" + url.QueryEscape(csrf),
			"user=a&password=x&_csrf=" + url.QueryEscape(csrf) + "&_csrf=" + url.QueryEscape(csrf),
		} {
			r := authReq(http.MethodPost, "app.test", "/auth", strings.NewReader(body),
				"Content-Type", "application/x-www-form-urlencoded")
			r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
			rr, _ := ah.wall(t, r)
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("duplicated field body %q: want 400, got %d", body, rr.Code)
			}
		}
	})

	t.Run("non-urlencoded content types → 415", func(t *testing.T) {
		for _, ct := range []string{
			"multipart/form-data; boundary=x",
			"application/json",
			"text/plain",
			"", // none-with-body
		} {
			r := authReq(http.MethodPost, "app.test", "/auth", strings.NewReader("user=a&password=b"))
			if ct != "" {
				r.Header.Set("Content-Type", ct)
			}
			rr, _ := ah.wall(t, r)
			if rr.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("content type %q: want 415, got %d", ct, rr.Code)
			}
		}
	})

	t.Run("oversized body → 400 before parsing", func(t *testing.T) {
		body := "user=a&password=" + strings.Repeat("x", authBodyCap)
		r := authReq(http.MethodPost, "app.test", "/auth", strings.NewReader(body),
			"Content-Type", "application/x-www-form-urlencoded")
		rr, _ := ah.wall(t, r)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("oversized body: want 400, got %d", rr.Code)
		}
	})

	t.Run("field caps → 400 before any KDF", func(t *testing.T) {
		csrf := ah.getLoginForm(t, "app.test")
		before := authKDFRuns.Load()
		rr := ah.postForm(t, "app.test", url.Values{
			"_csrf":    {csrf},
			"user":     {strings.Repeat("a", authUserCap+1)},
			"password": {"x"},
		}, csrf)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("oversized user: want 400, got %d", rr.Code)
		}
		if authKDFRuns.Load() != before {
			t.Fatal("capped fields still burned a KDF")
		}
	})

	t.Run("CSRF enforced on both POST arms", func(t *testing.T) {
		// Missing pair entirely.
		rr := ah.postForm(t, "app.test", url.Values{"user": {"alice"}, "password": {"open-sesame"}}, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("login without csrf: want 403, got %d", rr.Code)
		}
		rr = ah.postForm(t, "app.test", url.Values{}, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("sign-out without csrf: want 403, got %d", rr.Code)
		}
		// Cookie/field mismatch.
		csrf := ah.getLoginForm(t, "app.test")
		other := ah.getLoginForm(t, "app.test")
		rr = ah.postForm(t, "app.test", url.Values{
			"_csrf": {csrf}, "user": {"alice"}, "password": {"open-sesame"},
		}, other)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("csrf mismatch: want 403, got %d", rr.Code)
		}
		// Forged token (valid shape, wrong signature).
		forged := "AAAAAAAAAAAAAAAAAAAAAA.BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB"
		rr = ah.postForm(t, "app.test", url.Values{
			"_csrf": {forged}, "user": {"alice"}, "password": {"open-sesame"},
		}, forged)
		if rr.Code != http.StatusForbidden {
			t.Fatalf("forged csrf: want 403, got %d", rr.Code)
		}
	})

	t.Run("other methods → 405 with Allow", func(t *testing.T) {
		for _, m := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodOptions} {
			rr, _ := ah.wall(t, authReq(m, "app.test", "/auth", nil))
			if rr.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s /auth: want 405, got %d", m, rr.Code)
			}
			if rr.Header().Get("Allow") == "" {
				t.Fatalf("%s /auth: no Allow header", m)
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("%s /auth: not no-store", m)
			}
		}
	})

	t.Run("return_to carried through login and sanitized", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/auth?return_to=%2Freports%3Fq%3D1", nil))
		if !strings.Contains(rr.Body.String(), `name="return_to" value="/reports?q=1"`) {
			t.Fatalf("return_to not carried into the form: %s", rr.Body.String())
		}
		for target, want := range map[string]string{
			"/reports?q=1":   "/reports?q=1",
			"//evil.example": "/",
			`/\evil`:         "/",
			"https://evil":   "/",
		} {
			csrf := ah.getLoginForm(t, "app.test")
			rr := ah.postForm(t, "app.test", url.Values{
				"_csrf": {csrf}, "user": {"alice"}, "password": {"open-sesame"},
				"return_to": {target},
			}, csrf)
			if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != want {
				t.Fatalf("return_to %q: %d → %q, want %q", target, rr.Code, rr.Header().Get("Location"), want)
			}
		}
	})

	t.Run("fixation dead: a pre-set cookie is never honored, never re-minted", func(t *testing.T) {
		preset := "attacker-chosen-token-value"
		r := authReq(http.MethodGet, "app.test", "/x", nil, "Accept", "text/html")
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: preset})
		rr, _ := ah.wall(t, r)
		if rr.Code != http.StatusFound {
			t.Fatalf("pre-set cookie honored: %d", rr.Code)
		}
		session := ah.login(t, "app.test", "alice", "open-sesame")
		if session == preset {
			t.Fatal("login re-minted the client-proposed token")
		}
	})
}

// --- the wall ----------------------------------------------------------------------

func TestAuthWallFork(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)

	t.Run("browser-shaped → 302 with return_to", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/reports?q=1", nil,
			"Accept", "text/html,application/xhtml+xml"))
		if rr.Code != http.StatusFound {
			t.Fatalf("want 302, got %d", rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != "/auth?return_to=%2Freports%3Fq%3D1" {
			t.Fatalf("Location %q", loc)
		}
		if rr.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("302 is not no-store")
		}
	})

	t.Run("API-shaped → 401, no WWW-Authenticate", func(t *testing.T) {
		for _, r := range []*http.Request{
			authReq(http.MethodGet, "app.test", "/api", nil),
			authReq(http.MethodPost, "app.test", "/submit", strings.NewReader("x"), "Accept", "text/html"),
			authReq(http.MethodGet, "app.test", "/data", nil, "Accept", "application/json"),
		} {
			rr, _ := ah.wall(t, r)
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s: want 401, got %d", r.Method, r.URL.Path, rr.Code)
			}
			if rr.Header().Get("WWW-Authenticate") != "" {
				t.Fatal("the wall's 401 must not carry WWW-Authenticate")
			}
			if rr.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("401 is not no-store")
			}
		}
	})

	t.Run("WS upgrade without a session → 401, never 302", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/hub", nil,
			"Accept", "text/html",
			"Connection", "Upgrade",
			"Upgrade", "websocket"))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("upgrade fork: want 401, got %d", rr.Code)
		}
	})

	t.Run("valid session falls through: strip, inject, signal", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/data", nil,
			"Remote-User", "root", // spoof dies at the edge
			"X-Other", "kept")
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		r.AddCookie(&http.Cookie{Name: "sid", Value: "42"})
		rr, out := ah.wall(t, r)
		if out == nil {
			t.Fatalf("valid session did not fall through: %d", rr.Code)
		}
		if got := out.Header.Get("Remote-User"); got != "alice" {
			t.Fatalf("Remote-User = %q, want alice", got)
		}
		cookie := out.Header.Get("Cookie")
		if strings.Contains(cookie, authCookieName) || strings.Contains(cookie, authCSRFCookieName) {
			t.Fatalf("auth cookies rode through to the upstream: %q", cookie)
		}
		if !strings.Contains(cookie, "sid=42") {
			t.Fatalf("non-auth cookie was stripped: %q", cookie)
		}
		if authIdentityOf(out.Context()) != "alice" {
			t.Fatal("auth→cache bypass signal missing from the context")
		}
		// The hub handshake snapshot (frozen after the wall) carries the
		// injected identity and never a live bearer token.
		snap, ok := hubHeaderSnapshot(out.Header)
		if !ok {
			t.Fatal("snapshot over cap")
		}
		if snap.Get("Remote-User") != "alice" {
			t.Fatal("snapshot lacks the injected Remote-User")
		}
		if strings.Contains(snap.Get("Cookie"), authCookieName) {
			t.Fatal("snapshot holds the session cookie")
		}
	})

	t.Run("spoofed Remote-User without a session → 401", func(t *testing.T) {
		rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", "/x", nil, "Remote-User", "root"))
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
	})

	t.Run("exact /auth only: neighbors hit the wall like any path", func(t *testing.T) {
		for _, p := range []string{"/authors", "/auth/callback", "/auth.js"} {
			rr, _ := ah.wall(t, authReq(http.MethodGet, "app.test", p, nil))
			if rr.Code != http.StatusUnauthorized {
				t.Fatalf("%s: want 401 (walled, not claimed), got %d", p, rr.Code)
			}
		}
		session := ah.login(t, "app.test", "alice", "open-sesame")
		r := authReq(http.MethodGet, "app.test", "/auth/callback", nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		_, out := ah.wall(t, r)
		if out == nil {
			t.Fatal("/auth/callback with a session must proxy, not serve the endpoint")
		}
	})

	t.Run("expired session: POST loses, bare 401 (v3 parity)", func(t *testing.T) {
		session := ah.login(t, "app.test", "alice", "open-sesame")
		ah.st.revokeToken(session)
		r := authReq(http.MethodPost, "app.test", "/submit", strings.NewReader("body"))
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		rr, _ := ah.wall(t, r)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("want 401, got %d", rr.Code)
		}
	})
}

func TestAuthPlainHTTPDeadWall(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	r := httptest.NewRequest(http.MethodGet, "http://app.test/x", nil) // r.TLS == nil
	rr, out := ah.wall(t, r)
	if out != nil {
		t.Fatal("plain-HTTP request fell through the wall")
	}
	if rr.Code != http.StatusMisdirectedRequest {
		t.Fatalf("want 421, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "auth requires HTTPS") {
		t.Fatalf("421 body: %q", rr.Body.String())
	}
	if rr.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("421 is not no-store")
	}
	// /auth itself is just as dead over plain HTTP.
	rr2, _ := ah.wall(t, httptest.NewRequest(http.MethodGet, "http://app.test/auth", nil))
	if rr2.Code != http.StatusMisdirectedRequest {
		t.Fatalf("plain-HTTP /auth: want 421, got %d", rr2.Code)
	}
	if !ah.h.authCfg.plainHTTPLogged.Load() {
		t.Fatal("dead wall never logged")
	}
}

// --- per-request authorization (site-level user replacement) ---------------------------

func TestAuthPerRequestSiteAuthorization(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	siteA := &Handler{logger: zap.NewNop(), authCfg: &authSite{
		users: map[string]string{"alice": testCred(t, "open-sesame")},
		ttl:   authDefaultTTL, store: st,
	}}
	siteB := &Handler{logger: zap.NewNop(), authCfg: &authSite{
		users: map[string]string{"carol": testCred(t, "carol-pass")},
		ttl:   authDefaultTTL, store: st,
	}}
	ahA := &authHarness{h: siteA, st: st}
	session := ahA.login(t, "a.test", "alice", "open-sesame")

	// Alice passes site A.
	r := authReq(http.MethodGet, "a.test", "/x", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	if _, out := ahA.wall(t, r); out == nil {
		t.Fatal("alice blocked on her own site")
	}
	// The same live session is unauthenticated on the carol-only site:
	// authorization is per request, not a login-time filter.
	ahB := &authHarness{h: siteB, st: st}
	r = authReq(http.MethodGet, "b.test", "/x", nil, "Accept", "text/html")
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	rr, out := ahB.wall(t, r)
	if out != nil {
		t.Fatal("alice's session passed the carol-only site")
	}
	if rr.Code != http.StatusFound {
		t.Fatalf("want 302 to login, got %d", rr.Code)
	}
	// And the session itself stays live for site A.
	if _, _, ok := st.lookup(session, time.Hour); !ok {
		t.Fatal("cross-site rejection revoked the session")
	}
}

// --- cascade -----------------------------------------------------------------------

func TestAuthCascadeResolution(t *testing.T) {
	blob := testCred(t, "open-sesame")
	carol := testCred(t, "carol-pass")
	on, off := true, false
	ttl2h := caddy.Duration(2 * time.Hour)
	ttl8h := caddy.Duration(8 * time.Hour)

	app := func(g *AuthSettings) *App {
		st, err := newAuthStore()
		if err != nil {
			t.Fatal(err)
		}
		return &App{Auth: g, state: &janusState{auth: st}}
	}
	cases := []struct {
		name       string
		global     *AuthSettings
		site       *AuthSettings
		wantOn     bool
		wantUsers  []string
		wantTTL    time.Duration
		wantErrSub string
	}{
		{"unset/unset → off", nil, nil, false, nil, 0, ""},
		{"global on / unset → on with global users", &AuthSettings{Enabled: &on, Users: []AuthUser{{"alice", blob}}, TTL: &ttl8h}, nil,
			true, []string{"alice"}, 8 * time.Hour, ""},
		{"global on / site off → off", &AuthSettings{Enabled: &on, Users: []AuthUser{{"alice", blob}}}, &AuthSettings{Enabled: &off},
			false, nil, 0, ""},
		{"unset / site on with users → on", nil, &AuthSettings{Enabled: &on, Users: []AuthUser{{"carol", carol}}},
			true, []string{"carol"}, authDefaultTTL, ""},
		{"site ttl overrides; users inherited whole", &AuthSettings{Enabled: &on, Users: []AuthUser{{"alice", blob}, {"bob", blob}}, TTL: &ttl8h},
			&AuthSettings{Enabled: &on, TTL: &ttl2h},
			true, []string{"alice", "bob"}, 2 * time.Hour, ""},
		{"site users REPLACE global users (one level, no merge)", &AuthSettings{Enabled: &on, Users: []AuthUser{{"alice", blob}}},
			&AuthSettings{Enabled: &on, Users: []AuthUser{{"carol", carol}}},
			true, []string{"carol"}, authDefaultTTL, ""},
		{"global off / site on inherits global users", &AuthSettings{Enabled: &off, Users: nil},
			&AuthSettings{Enabled: &on, Users: []AuthUser{{"carol", carol}}},
			true, []string{"carol"}, authDefaultTTL, ""},
		{"zero users: global on, no users anywhere", &AuthSettings{Enabled: &on}, nil,
			false, nil, 0, "resolved user set is empty"},
		{"zero users: site on, no users anywhere", nil, &AuthSettings{Enabled: &on},
			false, nil, 0, "resolved user set is empty"},
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
			for _, u := range tc.wantUsers {
				if _, ok := h.authCfg.users[u]; !ok {
					t.Fatalf("missing user %q", u)
				}
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

	token, err := st.mint("alice")
	if err != nil {
		t.Fatal(err)
	}

	// HMAC keying: the raw token appears nowhere in the store.
	st.mu.Lock()
	for k := range st.sessions {
		if strings.Contains(k, token) {
			t.Fatal("raw token retained as a store key")
		}
	}
	st.mu.Unlock()

	// Sliding bump: activity at +30m keeps a 1h ttl session alive at +75m.
	now = now.Add(30 * time.Minute)
	if _, _, ok := st.lookup(token, time.Hour); !ok {
		t.Fatal("live session missed")
	}
	now = now.Add(45 * time.Minute)
	if user, _, ok := st.lookup(token, time.Hour); !ok || user != "alice" {
		t.Fatal("slid session expired early")
	}

	// Idle expiry is lazy: the expired lookup deletes and counts.
	now = now.Add(2 * time.Hour)
	if _, _, ok := st.lookup(token, time.Hour); ok {
		t.Fatal("idle session survived past ttl")
	}
	if st.expired.Load() != 1 {
		t.Fatalf("expired counter = %d, want 1", st.expired.Load())
	}
	if st.sessionCount() != 0 {
		t.Fatal("lazy delete left the corpse")
	}

	// The reaper sweeps corpses the lazy path never touches.
	tok2, _ := st.mint("bob")
	now = now.Add(20 * time.Hour) // past the default 8h reap bound
	st.reap()
	if st.sessionCount() != 0 {
		t.Fatal("reaper left an idle corpse")
	}
	if _, _, ok := st.lookup(tok2, 100 * time.Hour); ok {
		t.Fatal("reaped session still resolves")
	}

	// Reload revocation: only users absent from every enabled set die.
	tokA, _ := st.mint("alice")
	tokB, _ := st.mint("bob")
	if n := st.revokeUsersNotIn(map[string]bool{"alice": true}); n != 1 {
		t.Fatalf("revokeUsersNotIn = %d, want 1", n)
	}
	if _, _, ok := st.lookup(tokA, time.Hour); !ok {
		t.Fatal("kept user's session revoked")
	}
	if _, _, ok := st.lookup(tokB, time.Hour); ok {
		t.Fatal("removed user's session survived")
	}
	if st.reloadRevoked.Load() != 1 {
		t.Fatal("reload_revoked counter unmoved")
	}

	// Wipe-all.
	st.mint("alice")
	st.mint("bob")
	if n := st.revokeAll(); n != 3 {
		t.Fatalf("revokeAll = %d, want 3", n)
	}
	if st.sessionCount() != 0 {
		t.Fatal("wipe left sessions")
	}
}

func TestAuthSessionIDPrefixResolution(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := st.mint("alice")
	list := st.sessionList()
	if len(list) != 1 || list[0].User != "alice" || len(list[0].ID) != 12 {
		t.Fatalf("session list: %+v", list)
	}
	if found, ambiguous := st.revokeID("ffffffffffff"); found || ambiguous {
		if list[0].ID != "ffffffffffff" {
			t.Fatal("unknown prefix resolved")
		}
	}
	// Ambiguity: two sessions sharing a fabricated common prefix.
	st.mu.Lock()
	st.sessions["aaaa00000000ffff"] = &authSession{user: "x", issuedAt: st.now(), lastSeen: st.now()}
	st.sessions["aaaa00000000eeee"] = &authSession{user: "y", issuedAt: st.now(), lastSeen: st.now()}
	st.mu.Unlock()
	if _, ambiguous := st.revokeID("aaaa"); !ambiguous {
		t.Fatal("ambiguous prefix not detected")
	}
	if found, _ := st.revokeID("aaaa00000000ffff"); !found {
		t.Fatal("full-key prefix failed to revoke")
	}
	if found, _ := st.revokeID(list[0].ID); !found {
		t.Fatal("listed 12-hex id failed to revoke")
	}
	if _, _, ok := st.lookup(tok, time.Hour); ok {
		t.Fatal("revoked session still resolves")
	}
}

// --- throttle -----------------------------------------------------------------------

func TestAuthThrottleWindow(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.now = func() time.Time { return now }
	key := authClientKey("203.0.113.9:5555") + "\x00alice"
	other := authClientKey("203.0.113.10:5555") + "\x00alice"

	for i := 0; i < authThrottleLimit; i++ {
		if blocked, _ := st.throttleCheck(key); blocked {
			t.Fatalf("blocked after %d fails", i)
		}
		st.throttleFail(key)
	}
	blocked, retry := st.throttleCheck(key)
	if !blocked || retry <= 0 {
		t.Fatalf("want blocked with retry-after, got %v %v", blocked, retry)
	}
	// Pair isolation: another IP, same user, unaffected.
	if blocked, _ := st.throttleCheck(other); blocked {
		t.Fatal("throttle leaked across (IP, user) pairs")
	}
	// Window expiry frees the pair.
	now = now.Add(authThrottleWindow + time.Second)
	if blocked, _ := st.throttleCheck(key); blocked {
		t.Fatal("window never expired")
	}
	// Success clears.
	st.throttleFail(key)
	st.throttleClear(key)
	st.mu.Lock()
	_, present := st.throttle[key]
	st.mu.Unlock()
	if present {
		t.Fatal("throttleClear left the entry")
	}
}

func TestAuthClientKeyIPv6Prefix(t *testing.T) {
	a := authClientKey("[2001:db8:1:2:aaaa:bbbb:cccc:dddd]:443")
	b := authClientKey("[2001:db8:1:2:1111:2222:3333:4444]:443")
	c := authClientKey("[2001:db8:1:3::1]:443")
	if a != b {
		t.Fatalf("same /64 must share a key: %q vs %q", a, b)
	}
	if a == c {
		t.Fatal("different /64s must not share a key")
	}
	if authClientKey("203.0.113.9:1") != authClientKey("203.0.113.9:2") {
		t.Fatal("IPv4 key must ignore the port")
	}
	if authClientKey("203.0.113.9:1") == authClientKey("203.0.113.10:1") {
		t.Fatal("distinct IPv4 addresses must not share a key")
	}
}

func TestAuthThrottleMapBounded(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < authThrottleMaxEntries+100; i++ {
		st.throttleFail("key-" + string(rune('a'+i%26)) + "-" + strings.Repeat("x", i%7) + "-" + itoa(i))
	}
	st.mu.Lock()
	n := len(st.throttle)
	st.mu.Unlock()
	if n > authThrottleMaxEntries {
		t.Fatalf("throttle map grew to %d, bound is %d", n, authThrottleMaxEntries)
	}
}

func itoa(i int) string {
	return strings.TrimSpace(strings.ReplaceAll(time.Duration(i).String(), "ns", ""))
}

func TestAuthThrottleBeforeKDF(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	// Burn the pair's window without touching argon2 (unit-level).
	key := authClientKey("192.0.2.1:1234") + "\x00alice"
	for i := 0; i < authThrottleLimit; i++ {
		ah.st.throttleFail(key)
	}
	csrf := ah.getLoginForm(t, "app.test")
	before := authKDFRuns.Load()
	r := authReq(http.MethodPost, "app.test", "/auth",
		strings.NewReader(url.Values{"_csrf": {csrf}, "user": {"alice"}, "password": {"open-sesame"}}.Encode()),
		"Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "192.0.2.1:9999"
	r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
	rr, _ := ah.wall(t, r)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled attempt: want 429, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("429 carries no Retry-After")
	}
	if authKDFRuns.Load() != before {
		t.Fatal("a throttled attempt performed argon2 work")
	}
	if ah.st.throttled.Load() == 0 {
		t.Fatal("throttled counter unmoved")
	}
}

// --- timing equalization ---------------------------------------------------------------

func TestAuthTimingEqualization(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	// Warm the dummy credential (minted lazily on the first miss).
	ah.st.verifyCredential("warmup", "", false)

	before := authKDFRuns.Load()
	if ah.st.verifyCredential("wrong", ah.h.authCfg.users["alice"], true) {
		t.Fatal("wrong password verified")
	}
	wrongPW := authKDFRuns.Load() - before

	before = authKDFRuns.Load()
	if ah.st.verifyCredential("whatever", "", false) {
		t.Fatal("unknown user verified")
	}
	unknownUser := authKDFRuns.Load() - before

	if wrongPW != 1 || unknownUser != 1 {
		t.Fatalf("KDF runs: wrong-password=%d unknown-user=%d, want exactly 1 each", wrongPW, unknownUser)
	}
}

// --- semaphore + bounded queue ------------------------------------------------------------

func TestAuthVerifySemaphoreAndQueueBound(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	// Fill the semaphore and the queue from goroutines; admissions past
	// concurrency+queue must fast-fail.
	total := authVerifyConcurrency + authVerifyQueueMax + 10
	var admitted, rejected atomic.Int64
	var inFlight, maxInFlight atomic.Int64
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
			cur := inFlight.Add(1)
			for {
				max := maxInFlight.Load()
				if cur <= max || maxInFlight.CompareAndSwap(max, cur) {
					break
				}
			}
			admitted.Add(1)
			<-release
			inFlight.Add(-1)
			st.releaseVerify()
		}()
	}
	// Wait for the flood to settle: everyone either holds, queues, or
	// was rejected.
	deadline := time.Now().Add(5 * time.Second)
	for admitted.Load() < int64(authVerifyConcurrency) || rejected.Load() < int64(total-authVerifyConcurrency-authVerifyQueueMax) {
		if time.Now().After(deadline) {
			t.Fatalf("flood never settled: admitted=%d rejected=%d", admitted.Load(), rejected.Load())
		}
		time.Sleep(time.Millisecond)
	}
	if maxInFlight.Load() > int64(authVerifyConcurrency) {
		t.Fatalf("concurrent verifications = %d, cap is %d", maxInFlight.Load(), authVerifyConcurrency)
	}
	if rejected.Load() != int64(total-authVerifyConcurrency-authVerifyQueueMax) {
		t.Fatalf("rejected = %d, want %d", rejected.Load(), total-authVerifyConcurrency-authVerifyQueueMax)
	}
	close(release)
	wg.Wait()
	if st.verifyWaiting.Load() != 0 {
		t.Fatalf("queue gauge leaked: %d", st.verifyWaiting.Load())
	}
}

// --- ordering ----------------------------------------------------------------------

func TestAuthOrderingPingBeforeWall(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	on := true
	ah.h.Ping = &on
	for _, p := range []string{"/ping", "/ping/"} {
		rr := httptest.NewRecorder()
		if err := ah.h.ServeHTTP(rr, authReq(http.MethodGet, "app.test", p, nil), nil); err != nil {
			t.Fatal(err)
		}
		if rr.Code != 200 || rr.Body.String() != "pong\n" {
			t.Fatalf("%s on a guarded site: %d %q (liveness is not a secret)", p, rr.Code, rr.Body.String())
		}
	}
	// Ping off: /ping is an ordinary walled path.
	off := false
	ah.h.Ping = &off
	rr := httptest.NewRecorder()
	if err := ah.h.ServeHTTP(rr, authReq(http.MethodGet, "app.test", "/ping", nil), nil); err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("ping-off /ping on a guarded site: want 401, got %d", rr.Code)
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
		t.Fatalf("sessionless upgrade on a guarded site: want 401 from the wall, got %d", rr.Code)
	}
}

// --- cache interplay ---------------------------------------------------------------

func TestAuthCacheBypassSignalExplicit(t *testing.T) {
	// The signal, in isolation: a request whose auth cookies were
	// stripped (no Cookie header left) still bypasses on the context
	// value — never on header residue.
	r := httptest.NewRequest(http.MethodGet, "https://app.test/x", nil)
	if cacheBypassRequest(r) {
		t.Fatal("anonymous GET must not bypass")
	}
	r = r.WithContext(context.WithValue(r.Context(), authIdentityKey{}, "alice"))
	if !cacheBypassRequest(r) {
		t.Fatal("authenticated GET (post-strip) must bypass on the explicit signal")
	}
}

func TestAuthenticatedTrafficNeverCached(t *testing.T) {
	// Full stack: wall + cache + data plane. Authenticated GETs reach
	// the upstream every time (BYPASS), and the upstream sees the
	// injected identity but never the wall's cookies.
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
	h := &Handler{
		logger: zap.NewNop(),
		dp:     dp,
		cacheCfg: &cacheSite{
			store: store, ttl: time.Minute, ttlMax: 10 * time.Minute,
			maxBody: defaultCacheMaxBody, debug: true,
		},
		authCfg: &authSite{users: aliceUsers(t), ttl: authDefaultTTL, store: st},
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
		if rr.Code != 200 {
			t.Fatalf("authenticated GET %d: %d", i, rr.Code)
		}
		if v := rr.Header().Get(cacheDebugHeader); v != "BYPASS" {
			t.Fatalf("authenticated GET %d: cache verdict %q, want BYPASS", i, v)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("upstream saw %d requests, want 3 (never a HIT)", len(seen))
	}
	for _, hd := range seen {
		if hd.Get("Remote-User") != "alice" {
			t.Fatalf("upstream Remote-User = %q", hd.Get("Remote-User"))
		}
		if c := hd.Get("Cookie"); strings.Contains(c, authCookieName) {
			t.Fatalf("upstream received the session cookie: %q", c)
		}
	}
}
