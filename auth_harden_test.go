package janus

import (
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// --- login throttle -------------------------------------------------------------

func clockedStore(t *testing.T) (*authStore, *time.Time) {
	t.Helper()
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	st.now = func() time.Time { return now }
	return st, &now
}

func TestAuthThrottleTracksNewClientsAtCap(t *testing.T) {
	st, now := clockedStore(t)
	// Fill the table with hot, recent, non-ban rows: nothing is cold.
	for i := 0; i < authThrottleMaxEntries; i++ {
		st.throttle[authThrottleAggKey("/", "filler-"+strconv.Itoa(i))] = &authThrottleEntry{fails: 1, lastFail: *now}
	}
	*now = now.Add(time.Second)
	gate, client := "/", "198.51.100.77"
	for i := 0; i < 4; i++ {
		st.throttleFail(gate, client, "alice")
	}
	if blocked, _ := st.throttleCheck(gate, client, "alice"); !blocked {
		t.Fatal("a new client at the table cap was never throttled")
	}
	if blocked, _ := st.throttleCheck(gate, client, "fresh"); !blocked {
		t.Fatal("a new client's aggregate at the table cap was never tracked")
	}
	if n := len(st.throttle); n > authThrottleMaxEntries {
		t.Fatalf("table grew past its bound: %d", n)
	}
}

func TestAuthThrottleEvictsBansOnlyWhenEveryRowIsBanned(t *testing.T) {
	st, now := clockedStore(t)
	for i := 0; i < authThrottleMaxEntries; i++ {
		st.throttle[authThrottleAggKey("/", "ban-"+strconv.Itoa(i))] = &authThrottleEntry{
			fails: authThrottleBanAt, lastFail: *now,
			banUntil: now.Add(authThrottleBan + time.Duration(i)), blockedUntil: now.Add(authThrottleBan),
		}
	}
	st.throttleFail("/", "203.0.113.200", "alice")
	if _, kept := st.throttle[authThrottleAggKey("/", "ban-0")]; kept {
		t.Fatal("the soonest-expiring ban should give way when the table is all bans")
	}
	if _, kept := st.throttle[authThrottleAggKey("/", "ban-9")]; !kept {
		t.Fatal("a later ban was evicted")
	}
	if st.throttle[authThrottleAggKey("/", "203.0.113.200")] == nil {
		t.Fatal("the new client was not tracked")
	}
}

func TestAuthSuccessDoesNotResetSprayBudget(t *testing.T) {
	st, _ := clockedStore(t)
	gate, client := "/", "192.0.2.50"
	// One sprayed username, plus two typos by bob himself.
	for _, u := range []string{"x", "bob", "bob"} {
		st.throttleFail(gate, client, u)
	}
	// bob signs in: his own two failures are refunded, the spray stays.
	st.throttleClear(gate, client, "bob")
	st.mu.Lock()
	agg := st.throttle[authThrottleAggKey(gate, client)].fails
	st.mu.Unlock()
	if agg != 1 {
		t.Fatalf("aggregate after success = %d, want 1 (spray kept, own typos refunded)", agg)
	}
	// Three more sprayed names reach the 4th aggregate failure: the
	// client is held. Had the success cleared the aggregate it would not be.
	for _, u := range []string{"y", "z", "w"} {
		st.throttleFail(gate, client, u)
	}
	if blocked, _ := st.throttleCheck(gate, client, "fresh"); !blocked {
		t.Fatal("a success reset the client's spray budget")
	}
}

func TestAuthAccountLadderSlowsDistributedGuessing(t *testing.T) {
	st, now := clockedStore(t)
	host, user := "app.test", "alice"
	for i := 0; i < authAccountFreeFails; i++ {
		client := "192.0.2." + strconv.Itoa(i+1)
		if blocked, _ := st.throttleCheck("/", client, user); blocked {
			t.Fatalf("client %s blocked by its own ladder", client)
		}
		if blocked, _ := st.accountCheck(host, user); blocked {
			t.Fatalf("account held after %d failures", i)
		}
		st.throttleFail("/", client, user)
		st.accountFail(host, user)
	}
	// The next failure, from yet another address, starts the account wait.
	if out := st.accountFail(host, user); out.wait != authAccountWaitFirst || out.banned {
		t.Fatalf("first account wait: %+v", out)
	}
	blocked, retry := st.accountCheck(host, user)
	if !blocked || retry < authAccountWaitFirst-time.Second {
		t.Fatalf("account not held: blocked=%v retry=%v", blocked, retry)
	}
	// Another host's same-named user is unaffected.
	if blocked, _ := st.accountCheck("other.test", user); blocked {
		t.Fatal("account ladder crossed hosts")
	}
	// The wait doubles to a cap and never becomes a ban.
	waits := []time.Duration{}
	for i := 0; i < 8; i++ {
		*now = now.Add(authAccountWaitMax + time.Second)
		waits = append(waits, st.accountFail(host, user).wait)
	}
	want := []time.Duration{10 * time.Second, 20 * time.Second, 40 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("account waits = %v, want %v", waits, want)
		}
	}
	*now = now.Add(authAccountWaitMax + time.Second)
	if blocked, _ := st.accountCheck(host, user); blocked {
		t.Fatal("account wait outlived its cap")
	}
	// An hour without failures forgets the ladder.
	*now = now.Add(authAccountForget + time.Second)
	if out := st.accountFail(host, user); out.wait != 0 {
		t.Fatalf("ladder not forgotten after the window: %+v", out)
	}
	// A successful login clears it.
	st.accountClear(host, user)
	if blocked, _ := st.accountCheck(host, user); blocked {
		t.Fatal("success did not clear the account ladder")
	}
}

// postLoginFrom posts one sign-in to /auth from remoteAddr, optionally with
// Caddy's client_ip var set, and returns the recorder.
func (ah *authHarness) postLoginFrom(t *testing.T, host, remoteAddr, clientIP, user, password string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	csrf := ah.getLoginForm(t, host, "/auth")
	r := authReq(http.MethodPost, host, "/auth",
		strings.NewReader(url.Values{"csrf": {csrf}, "user": {user}, "password": {password}}.Encode()),
		"Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = remoteAddr
	r.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
	for _, c := range cookies {
		r.AddCookie(c)
	}
	if clientIP != "" {
		r = withClientIP(r, clientIP)
	}
	rr, _ := ah.wall(t, r)
	return rr
}

func TestAuthAccountLadderAtTheDoor(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	// Every guess comes from a fresh address: no client ladder engages.
	for i := 0; i <= authAccountFreeFails; i++ {
		if rr := ah.postLoginFrom(t, "app.test", "192.0.2."+strconv.Itoa(i+1)+":1", "", "alice", "wrong"); rr.Code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i+1, rr.Code)
		}
	}
	rr := ah.postLoginFrom(t, "app.test", "203.0.113.99:4444", "", "alice", "open-sesame")
	if rr.Code != http.StatusTooManyRequests || rr.Header().Get("Retry-After") == "" {
		t.Fatalf("account ladder at the door: %d Retry-After=%q", rr.Code, rr.Header().Get("Retry-After"))
	}
}

func TestAuthThrottleScopedPerHost(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	for i := 0; i < 4; i++ {
		ah.postLoginFrom(t, "public.test", "192.0.2.8:1", "", "eve", "guess")
	}
	if rr := ah.postLoginFrom(t, "public.test", "192.0.2.8:1", "", "alice", "open-sesame"); rr.Code != http.StatusTooManyRequests {
		t.Fatalf("throttled host: %d, want 429", rr.Code)
	}
	if rr := ah.postLoginFrom(t, "app.test", "192.0.2.8:1", "", "alice", "open-sesame"); rr.Code != http.StatusSeeOther {
		t.Fatalf("another host from the same address: %d, want 303", rr.Code)
	}
}

func withClientIP(r *http.Request, ip string) *http.Request {
	vars := map[string]any{caddyhttp.ClientIPVarKey: ip}
	return r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, vars))
}

func TestAuthClientIPHonorsCaddyClientIP(t *testing.T) {
	r := authReq(http.MethodGet, "app.test", "/", nil)
	r.RemoteAddr = "10.0.0.1:5555"
	if got := authClientIP(r); got != "10.0.0.1" {
		t.Fatalf("fallback client ip = %q", got)
	}
	if got := authClientIP(withClientIP(r, "198.51.100.7")); got != "198.51.100.7" {
		t.Fatalf("caddy client ip = %q", got)
	}
}

func TestAuthThrottleKeysOnClientBehindTrustedProxy(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	post := func(ip, password string) int {
		// RemoteAddr is the load balancer, for every client.
		return ah.postLoginFrom(t, "app.test", "10.0.0.1:443", ip, "alice", password).Code
	}
	for i := 0; i < 4; i++ {
		post("198.51.100.1", "wrong")
	}
	if code := post("198.51.100.1", "open-sesame"); code != http.StatusTooManyRequests {
		t.Fatalf("throttled client: %d, want 429", code)
	}
	if code := post("198.51.100.2", "open-sesame"); code != http.StatusSeeOther {
		t.Fatalf("another client behind the same balancer: %d, want 303", code)
	}
}

// --- sessions -------------------------------------------------------------------

func TestAuthCredentialChangeRevokesSessionsAtReload(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	oldCred, newCred := testCred(t, "open-sesame"), testCred(t, "carol-pass")
	app := &App{
		logger: zap.NewNop(),
		state:  &janusState{auth: st},
		authSites: []authSiteEntry{{
			patterns: []string{"app.test"},
			cfg:      &authSite{users: map[string]string{"alice": newCred, "bob": oldCred}, ttl: time.Hour},
		}},
	}
	alice, _ := st.mint("alice", "app.test", oldCred) // passhash since rotated
	bob, _ := st.mint("bob", "app.test", oldCred)     // passhash unchanged
	app.reconcileAuth()
	if _, _, ok := st.lookup(alice, "app.test", time.Hour, false); ok {
		t.Fatal("a changed passhash kept its old session at reload")
	}
	if _, _, ok := st.lookup(bob, "app.test", time.Hour, false); !ok {
		t.Fatal("an unchanged passhash lost its session at reload")
	}
	if st.reloadRevoked.Load() != 1 {
		t.Fatalf("reload_revoked = %d, want 1", st.reloadRevoked.Load())
	}
}

func TestAuthCredentialChangeEndsSessionPerRequest(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	session := ah.login(t, "app.test", "alice", "open-sesame")
	r := authReq(http.MethodGet, "app.test", "/x", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	if _, out := ah.wall(t, r); out == nil {
		t.Fatal("fresh session refused")
	}
	// A reload commits a new passhash for alice; the old session must not
	// pass even before the store sweep runs.
	ah.h.authCfg.users["alice"] = testCred(t, "carol-pass")
	r = authReq(http.MethodGet, "app.test", "/x", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	if rr, out := ah.wall(t, r); out != nil || rr.Code != http.StatusUnauthorized {
		t.Fatalf("session minted under the old passhash passed: code=%d", rr.Code)
	}
	r = authReq(http.MethodGet, "app.test", "/auth", nil)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	if rr, _ := ah.wall(t, r); strings.Contains(rr.Body.String(), "Sign out") {
		t.Fatal("status page honored a session minted under the old passhash")
	}
}

func TestAuthLoginRevokesThePriorSession(t *testing.T) {
	ah := newAuthHarness(t, aliceUsers(t), 0)
	first := ah.login(t, "app.test", "alice", "open-sesame")
	rr := ah.postLoginFrom(t, "app.test", "192.0.2.1:1", "", "alice", "open-sesame",
		&http.Cookie{Name: authCookieName, Value: first})
	second := authCookieValue(rr, authCookieName)
	if rr.Code != http.StatusSeeOther || second == "" || second == first {
		t.Fatalf("second login: %d session=%q", rr.Code, second)
	}
	if _, _, ok := ah.st.lookup(first, "app.test", time.Hour, false); ok {
		t.Fatal("the session a login arrived with outlived the login")
	}
	if _, _, ok := ah.st.lookup(second, "app.test", time.Hour, false); !ok {
		t.Fatal("the new session does not resolve")
	}
}

func TestAuthRevokeIDRequiresMinimumPrefix(t *testing.T) {
	st, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := st.mint("alice", "app.test", "")
	id := st.sessionListRaw()[0].ID
	for _, prefix := range []string{id[:1], id[:7], strings.ToUpper(id[:8]), "", id[:7] + "z"} {
		if found, ambiguous := st.revokeID(prefix); found || ambiguous {
			t.Errorf("revokeID(%q) = %v/%v, want refused", prefix, found, ambiguous)
		}
	}
	if _, _, ok := st.lookup(tok, "app.test", time.Hour, false); !ok {
		t.Fatal("a refused prefix revoked the session")
	}
	if found, _ := st.revokeID(id[:authSessionIDMin]); !found {
		t.Fatal("an 8-character prefix did not revoke")
	}
}

// --- audit trail ----------------------------------------------------------------

func auditEntries(logs *observer.ObservedLogs, msg string) []observer.LoggedEntry {
	return logs.FilterMessage(msg).All()
}

func TestAuthAuditTrail(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ah := newAuthHarness(t, aliceUsers(t), 0)
	ah.h.logger = zap.New(core)
	ah.st.logger.Store(zap.New(core))
	now := time.Now()
	ah.st.now = func() time.Time { return now }
	secrets := []string{"hunter2-unknown", "hunter3-wrong", "open-sesame"}

	// Unknown user and wrong password log the same message and field set.
	ah.postLoginFrom(t, "app.test", "192.0.2.10:1", "", "Ghost", secrets[0])
	ah.postLoginFrom(t, "app.test", "192.0.2.11:1", "", "alice", secrets[1])
	failed := auditEntries(logs, "janus auth: login failed")
	if len(failed) != 2 || failed[0].Level != zapcore.WarnLevel {
		t.Fatalf("login failed events: %+v", failed)
	}
	keys := func(e observer.LoggedEntry) string {
		var ks []string
		for k := range e.ContextMap() {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		return strings.Join(ks, ",")
	}
	if keys(failed[0]) != "client_ip,gate,host,user" || keys(failed[0]) != keys(failed[1]) {
		t.Fatalf("failure fields differ or are incomplete: %q vs %q", keys(failed[0]), keys(failed[1]))
	}
	if m := failed[0].ContextMap(); m["user"] != "ghost" || m["client_ip"] != "192.0.2.10" || m["host"] != "app.test" || m["gate"] != "/" {
		t.Fatalf("failure fields: %v", m)
	}

	// Success, then a replacing login, then sign-out.
	rr := ah.postLoginFrom(t, "app.test", "192.0.2.12:1", "", "alice", "open-sesame")
	first := authCookieValue(rr, authCookieName)
	rr = ah.postLoginFrom(t, "app.test", "192.0.2.12:1", "", "alice", "open-sesame",
		&http.Cookie{Name: authCookieName, Value: first})
	second := authCookieValue(rr, authCookieName)
	if got := auditEntries(logs, "janus auth: login succeeded"); len(got) != 2 || got[0].Level != zapcore.InfoLevel || got[0].ContextMap()["user"] != "alice" {
		t.Fatalf("login succeeded events: %+v", got)
	}
	if got := auditEntries(logs, "janus auth: session revoked"); len(got) != 1 || got[0].ContextMap()["via"] != "login" || got[0].ContextMap()["user"] != "alice" {
		t.Fatalf("replaced-session revocation: %+v", got)
	}
	csrf := ah.getLoginForm(t, "app.test", "/auth")
	out := authReq(http.MethodPost, "app.test", "/auth", strings.NewReader(url.Values{"csrf": {csrf}}.Encode()),
		"Content-Type", "application/x-www-form-urlencoded")
	out.AddCookie(&http.Cookie{Name: authCSRFCookieName, Value: csrf})
	out.AddCookie(&http.Cookie{Name: authCookieName, Value: second})
	ah.wall(t, out)
	if got := auditEntries(logs, "janus auth: signed out"); len(got) != 1 || got[0].Level != zapcore.InfoLevel || got[0].ContextMap()["user"] != "alice" {
		t.Fatalf("sign-out events: %+v", got)
	}

	// Throttle engagement, a 429, then a ban — from one client.
	for i := 0; i < 4; i++ {
		ah.postLoginFrom(t, "app.test", "198.51.100.20:1", "", "mallory", "x")
	}
	if got := auditEntries(logs, "janus auth: client throttle engaged"); len(got) != 1 {
		t.Fatalf("throttle engaged events: %d", len(got))
	}
	ah.postLoginFrom(t, "app.test", "198.51.100.20:1", "", "mallory", "x")
	if got := auditEntries(logs, "janus auth: login throttled"); len(got) != 1 || got[0].Level != zapcore.WarnLevel {
		t.Fatalf("login throttled events: %+v", got)
	}
	for _, wait := range []time.Duration{authThrottleWait4, authThrottleWait5, authThrottleWait6} {
		now = now.Add(wait + time.Second)
		ah.postLoginFrom(t, "app.test", "198.51.100.20:1", "", "mallory", "x")
	}
	if got := auditEntries(logs, "janus auth: client banned"); len(got) != 1 || got[0].ContextMap()["client_ip"] != "198.51.100.20" {
		t.Fatalf("ban events: %+v", got)
	}

	// Control revocations log host and user.
	tok, _ := ah.st.mint("bob", "app.test", "")
	ah.st.revokeID(ah.st.sessionListRaw()[0].ID)
	_ = tok
	ctl := logs.FilterField(zap.String("via", "control")).All()
	if len(ctl) != 1 || ctl[0].ContextMap()["user"] != "bob" || ctl[0].ContextMap()["host"] != "app.test" {
		t.Fatalf("control revocation events: %+v", ctl)
	}

	// Nothing secret ever reaches the log.
	for _, e := range logs.All() {
		for k, v := range e.ContextMap() {
			s, _ := v.(string)
			for _, secret := range append(secrets, first, second, tok) {
				if s != "" && strings.Contains(s, secret) {
					t.Fatalf("%q logged secret material in %s=%q", e.Message, k, s)
				}
			}
			if k == "password" || k == "token" || k == "session" || k == "csrf" {
				t.Fatalf("%q carries field %s", e.Message, k)
			}
		}
	}
}

func TestAuthLogUserSanitizes(t *testing.T) {
	got := authLogUser("evil\nname\x1b[31m " + strings.Repeat("a", 100))
	if len(got) != authLogUserCap || strings.ContainsAny(got, "\n\x1b ") {
		t.Fatalf("authLogUser = %q", got)
	}
}

func TestAuthSetsCaddyUserID(t *testing.T) {
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, map[string][]string{"/private/": {"alice"}})
	session := ah.loginDoor(t, "app.test", "/private/auth", "alice", "open-sesame")
	withRepl := func(target string) (*http.Request, *caddy.Replacer) {
		repl := caddy.NewReplacer()
		r := authReq(http.MethodGet, "app.test", target, nil)
		r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
		return r.WithContext(context.WithValue(r.Context(), caddy.ReplacerCtxKey, repl)), repl
	}
	r, repl := withRepl("/private/x")
	if _, out := ah.wall(t, r); out == nil {
		t.Fatal("gated request refused")
	}
	if got, _ := repl.GetString("http.auth.user.id"); got != "alice" {
		t.Fatalf("http.auth.user.id = %q, want alice", got)
	}
	r, repl = withRepl("/public")
	ah.wall(t, r)
	if got, ok := repl.GetString("http.auth.user.id"); ok && got != "" {
		t.Fatalf("open path set http.auth.user.id = %q", got)
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
