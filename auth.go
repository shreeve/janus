package janus

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html/template"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"golang.org/x/crypto/argon2"
)

// The auth wall (docs/20260722-134812-capability-auth.md). One reserved
// URL — exact /auth — a four-arm state machine dispatched by form
// fields, pooled in-memory sessions keyed by HMAC of the token, and
// strip-then-inject Remote-User identity transport. The wall runs in the
// site handler between ping and hub interception; sessions survive a
// config reload and die with the process.

//go:embed auth.html
var authPageHTML string

var authPageTmpl = template.Must(template.New("auth").Parse(authPageHTML))

// authPageData feeds the embedded two-state page: User empty renders the
// login form; User set renders the signed-in status page.
type authPageData struct {
	Host     string
	User     string
	Since    string
	CSRF     string
	ReturnTo string
	Error    string
}

// --- the g1 codec ---------------------------------------------------------------
//
// Exactly one definition of g1 in the codebase: the janus-auth-hash
// command mints with the same constants the verifier runs.

// g1KDF runs the fixed-parameter argon2id derivation. Every execution is
// counted (the throttle-before-KDF and timing-equalization pins read the
// counter).
func g1KDF(password, salt []byte) []byte {
	authKDFRuns.Add(1)
	return argon2.IDKey(password, salt, g1Time, g1Memory, g1Threads, g1KeyLen)
}

// authKDFRuns counts argon2 executions process-wide.
var authKDFRuns atomic.Int64

// g1Mint derives a fresh credential blob from a password.
func g1Mint(password string) (string, error) {
	salt := make([]byte, g1SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("minting salt: %w", err)
	}
	digest := g1KDF([]byte(password), salt)
	raw := append(salt, digest...)
	return g1Prefix + base64.RawStdEncoding.EncodeToString(raw), nil
}

// g1Verify checks a password against a stored blob (already validated at
// parse/provision) in constant time over the digest compare.
func g1Verify(password, blob string) bool {
	raw, err := decodeG1(blob)
	if err != nil {
		return false // unreachable for provisioned credentials
	}
	digest := g1KDF([]byte(password), raw[:g1SaltLen])
	return subtle.ConstantTimeCompare(digest, raw[g1SaltLen:]) == 1
}

// --- the pooled store ------------------------------------------------------------

// authSession is one live session: the raw token exists only in the
// client's cookie; the store key is HMAC(per-boot key, token).
type authSession struct {
	user     string
	issuedAt time.Time
	lastSeen time.Time
}

// authThrottleEntry is one (client key, username) fixed window.
type authThrottleEntry struct {
	windowStart time.Time
	fails       int
}

// authStore is the pooled session state: per-boot key, live sessions,
// login throttle windows, the argon2 semaphore, and the /1.0/auth
// counters. It lives in janusState (caddy.UsagePool) beside the
// registry: a config reload never logs anyone out; a process restart
// wipes every session and rotates the key.
type authStore struct {
	key [32]byte // per-boot: session-key HMAC + CSRF signing

	mu       sync.Mutex
	sessions map[string]*authSession       // hex(HMAC(key, token)) → session
	throttle map[string]*authThrottleEntry // clientKey+"\x00"+user → window

	dummyOnce sync.Once
	dummySalt []byte
	dummyHash []byte

	// verifySem caps concurrent argon2 verifications; verifyWaiting
	// bounds the queue behind it — past the depth, fast 429.
	verifySem     chan struct{}
	verifyWaiting atomic.Int64

	// reapTTL is the slow reaper's idle bound: the max effective ttl
	// across enabled sites, refreshed at App Start. Per-request expiry
	// uses each site's own effective ttl; the reaper only stops an
	// abandoned store growing forever.
	reapTTL atomic.Int64 // nanoseconds

	// Monotonic counters (the /1.0/cache discipline: monotonic, not
	// mutually atomic).
	logins        atomic.Int64
	loginFailures atomic.Int64
	throttled     atomic.Int64
	signouts      atomic.Int64
	revoked       atomic.Int64
	reloadRevoked atomic.Int64
	expired       atomic.Int64

	reapStop chan struct{}
	reapDone chan struct{}

	now func() time.Time
}

func newAuthStore() (*authStore, error) {
	st := &authStore{
		sessions:  map[string]*authSession{},
		throttle:  map[string]*authThrottleEntry{},
		verifySem: make(chan struct{}, authVerifyConcurrency),
		now:       time.Now,
	}
	if _, err := rand.Read(st.key[:]); err != nil {
		return nil, fmt.Errorf("janus auth: minting the per-boot key: %w", err)
	}
	st.reapTTL.Store(int64(authDefaultTTL))
	return st, nil
}

// run starts the slow reaper (registry-sweeper cadence class: minutes,
// not milliseconds). Lazy deletion on lookup is the authority; the
// reaper only sweeps idle corpses.
func (st *authStore) run() {
	st.reapStop = make(chan struct{})
	st.reapDone = make(chan struct{})
	go func() {
		defer close(st.reapDone)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				st.reap()
			case <-st.reapStop:
				return
			}
		}
	}()
}

func (st *authStore) stop() {
	if st.reapStop == nil {
		return
	}
	close(st.reapStop)
	<-st.reapDone
	st.reapStop = nil
}

// reap sweeps sessions idle past the reap bound and expired throttle
// windows.
func (st *authStore) reap() {
	now := st.now()
	ttl := time.Duration(st.reapTTL.Load())
	st.mu.Lock()
	n := 0
	for k, s := range st.sessions {
		if now.Sub(s.lastSeen) > ttl {
			delete(st.sessions, k)
			n++
		}
	}
	for k, t := range st.throttle {
		if now.Sub(t.windowStart) > authThrottleWindow {
			delete(st.throttle, k)
		}
	}
	st.mu.Unlock()
	if n > 0 {
		st.expired.Add(int64(n))
	}
}

// sessionKey maps a raw token to its store key. The HMAC is
// constant-work and the map is keyed by an attacker-unpredictable
// digest, so lookup timing tells an attacker nothing; a memory
// disclosure of the store yields no replayable tokens.
func (st *authStore) sessionKey(token string) string {
	mac := hmac.New(sha256.New, st.key[:])
	mac.Write([]byte("janus-auth-session:"))
	mac.Write([]byte(token))
	return hex.EncodeToString(mac.Sum(nil))
}

// mint issues a fresh session: 128 bits from crypto/rand, minted only at
// successful login — a client-proposed token is never accepted, so
// session fixation is dead by construction.
func (st *authStore) mint(user string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("janus auth: minting session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := st.now()
	st.mu.Lock()
	st.sessions[st.sessionKey(token)] = &authSession{user: user, issuedAt: now, lastSeen: now}
	st.mu.Unlock()
	return token, nil
}

// lookup resolves a presented token against the requesting site's
// effective ttl: expired entries are deleted lazily; a live hit slides
// lastSeen.
func (st *authStore) lookup(token string, ttl time.Duration) (user string, issuedAt time.Time, ok bool) {
	if token == "" {
		return "", time.Time{}, false
	}
	key := st.sessionKey(token)
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions[key]
	if s == nil {
		return "", time.Time{}, false
	}
	if now.Sub(s.lastSeen) > ttl {
		delete(st.sessions, key)
		st.expired.Add(1)
		return "", time.Time{}, false
	}
	s.lastSeen = now
	return s.user, s.issuedAt, true
}

// revokeToken revokes the session a cookie names. Revoking an
// already-dead session is a success, not an error.
func (st *authStore) revokeToken(token string) {
	if token == "" {
		return
	}
	key := st.sessionKey(token)
	st.mu.Lock()
	delete(st.sessions, key)
	st.mu.Unlock()
}

// revokeID revokes one session by id prefix (the /1.0/auth/sessions id:
// a 12-hex prefix of the HMACed store key). The prefix must resolve a
// unique key: none → found=false; several → ambiguous=true.
func (st *authStore) revokeID(prefix string) (found, ambiguous bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	match := ""
	for k := range st.sessions {
		if strings.HasPrefix(k, prefix) {
			if match != "" {
				return false, true
			}
			match = k
		}
	}
	if match == "" {
		return false, false
	}
	delete(st.sessions, match)
	st.revoked.Add(1)
	return true, false
}

// revokeAll wipes every session ("kick everyone").
func (st *authStore) revokeAll() int {
	st.mu.Lock()
	n := len(st.sessions)
	st.sessions = map[string]*authSession{}
	st.mu.Unlock()
	st.revoked.Add(int64(n))
	return n
}

// revokeUsersNotIn revokes sessions whose user appears in no enabled
// site's resolved set. Runs at App Start — past the reload's point of
// no return, so an aborted reload never logs users out. With the
// per-request set check this is promptness, not the security boundary.
func (st *authStore) revokeUsersNotIn(keep map[string]bool) int {
	st.mu.Lock()
	n := 0
	for k, s := range st.sessions {
		if !keep[s.user] {
			delete(st.sessions, k)
			n++
		}
	}
	st.mu.Unlock()
	st.reloadRevoked.Add(int64(n))
	return n
}

func (st *authStore) sessionCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.sessions)
}

// authSessionInfo is one /1.0/auth/sessions row.
type authSessionInfo struct {
	ID     string `json:"id"`
	User   string `json:"user"`
	AgeMS  int64  `json:"age_ms"`
	IdleMS int64  `json:"idle_ms"`
}

func (st *authStore) sessionList() []authSessionInfo {
	now := st.now()
	st.mu.Lock()
	out := make([]authSessionInfo, 0, len(st.sessions))
	for k, s := range st.sessions {
		out = append(out, authSessionInfo{
			ID:     k[:12],
			User:   s.user,
			AgeMS:  now.Sub(s.issuedAt).Milliseconds(),
			IdleMS: now.Sub(s.lastSeen).Milliseconds(),
		})
	}
	st.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- throttle ---------------------------------------------------------------------

// authClientKey keys the throttle on the connection's RemoteAddr — Janus
// is the TLS-terminating edge, so that is the true peer; X-Forwarded-For
// is attacker-supplied ink. IPv6 clients key on the /64 prefix (one
// subnet, not 2^64 fresh addresses); IPv4 keys on the address.
func authClientKey(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return host
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}

// throttleCheck reports whether the pair is currently blocked and, when
// it is, how long until the window opens. Checked before any argon2
// work: a brute-force loop costs a map lookup, never 64 MiB of KDF.
func (st *authStore) throttleCheck(key string) (blocked bool, retryAfter time.Duration) {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.throttle[key]
	if t == nil {
		return false, 0
	}
	if now.Sub(t.windowStart) > authThrottleWindow {
		delete(st.throttle, key)
		return false, 0
	}
	if t.fails >= authThrottleLimit {
		return true, t.windowStart.Add(authThrottleWindow).Sub(now)
	}
	return false, 0
}

// throttleFail records one failed attempt. The map is bounded: at the
// cap, expired windows are swept; if none are expired, an arbitrary
// entry is evicted — the map can never grow past
// authThrottleMaxEntries.
func (st *authStore) throttleFail(key string) {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.throttle[key]
	if t != nil && now.Sub(t.windowStart) <= authThrottleWindow {
		t.fails++
		return
	}
	if t == nil && len(st.throttle) >= authThrottleMaxEntries {
		for k, e := range st.throttle {
			if now.Sub(e.windowStart) > authThrottleWindow {
				delete(st.throttle, k)
			}
		}
		for k := range st.throttle {
			if len(st.throttle) < authThrottleMaxEntries {
				break
			}
			delete(st.throttle, k)
		}
	}
	st.throttle[key] = &authThrottleEntry{windowStart: now, fails: 1}
}

// throttleClear forgets the pair (successful login).
func (st *authStore) throttleClear(key string) {
	st.mu.Lock()
	delete(st.throttle, key)
	st.mu.Unlock()
}

// --- verification ------------------------------------------------------------------

// acquireVerify admits one verification behind the semaphore, or
// fast-fails when the queue is at depth.
func (st *authStore) acquireVerify() bool {
	if st.verifyWaiting.Add(1) > authVerifyConcurrency+authVerifyQueueMax {
		st.verifyWaiting.Add(-1)
		return false
	}
	st.verifySem <- struct{}{}
	return true
}

func (st *authStore) releaseVerify() {
	<-st.verifySem
	st.verifyWaiting.Add(-1)
}

// verifyCredential checks a password: a known user against their blob,
// an unknown user against the lazily minted dummy credential — the same
// argon2 work either way, so response timing never enumerates the user
// list (v3's verifyMatches).
func (st *authStore) verifyCredential(password, blob string, known bool) bool {
	if known {
		return g1Verify(password, blob)
	}
	st.dummyOnce.Do(func() {
		st.dummySalt = make([]byte, g1SaltLen)
		_, _ = rand.Read(st.dummySalt)
		st.dummyHash = g1KDF([]byte("janus-auth-dummy"), st.dummySalt)
	})
	digest := g1KDF([]byte(password), st.dummySalt)
	subtle.ConstantTimeCompare(digest, st.dummyHash)
	return false
}

// --- CSRF -------------------------------------------------------------------------
//
// Signed double-submit: the token is nonce.HMAC(key, nonce) with the
// per-boot key (domain-separated from the session HMAC). GET plants it
// in a hidden field and the short-lived __Host-janus_auth_csrf cookie;
// POST requires cookie == field and a valid signature, compared in
// constant time. Stateless, which suits a pre-auth form; a restart
// rotates the key and pre-restart forms fail CSRF — the user reloads.

func (st *authStore) csrfSign(nonce string) string {
	mac := hmac.New(sha256.New, st.key[:])
	mac.Write([]byte("janus-auth-csrf:"))
	mac.Write([]byte(nonce))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (st *authStore) mintCSRF() string {
	raw := make([]byte, 16)
	_, _ = rand.Read(raw)
	nonce := base64.RawURLEncoding.EncodeToString(raw)
	return nonce + "." + st.csrfSign(nonce)
}

func (st *authStore) validCSRF(token string) bool {
	nonce, sig, ok := strings.Cut(token, ".")
	if !ok || nonce == "" || sig == "" {
		return false
	}
	want := st.csrfSign(nonce)
	return subtle.ConstantTimeCompare([]byte(sig), []byte(want)) == 1
}

// csrfOK applies the double-submit check: cookie and field present,
// equal, and validly signed.
func (st *authStore) csrfOK(r *http.Request) bool {
	c, err := r.Cookie(authCSRFCookieName)
	if err != nil || c.Value == "" {
		return false
	}
	field := r.PostForm.Get("_csrf")
	if field == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) == 1 && st.validCSRF(field)
}

// --- return_to --------------------------------------------------------------------

// safeReturnTo admits same-origin paths only (v3's safeReturnTo, ported
// exactly): under 2048 bytes, a single leading '/', no '//' or '/\'
// prefix, and no control character, whitespace, or backslash anywhere.
// Anything else collapses to "/" — there is no open-redirect budget.
func safeReturnTo(raw string) string {
	if raw == "" || len(raw) >= authReturnToCap {
		return "/"
	}
	if raw[0] != '/' {
		return "/"
	}
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return "/"
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b <= 0x20 || b == 0x7f || b == '\\' {
			return "/"
		}
	}
	return raw
}

// --- the wall ---------------------------------------------------------------------

// authIdentityKey carries the authenticated username on the request
// context: the EXPLICIT auth→cache bypass signal. The cache must never
// infer authentication from Cookie-header residue — the wall strips its
// own cookies before the request proceeds, and a stripped request whose
// response is per-identity must still bypass.
type authIdentityKey struct{}

// authIdentityOf reports the authenticated user, or "" pre-wall/off.
func authIdentityOf(ctx context.Context) string {
	user, _ := ctx.Value(authIdentityKey{}).(string)
	return user
}

// serveAuthWall runs the wall for a guarded site. handled=true means the
// wall wrote the response (the /auth endpoint, a 302/401 rejection, or
// the plain-HTTP 421); otherwise the returned request — identity
// injected, auth cookies stripped, context signal set — proceeds to hub
// interception, cache, and the data plane.
func (h *Handler) serveAuthWall(w http.ResponseWriter, r *http.Request) (*http.Request, bool, error) {
	if r.TLS == nil {
		return r, true, h.authRejectPlainHTTP(w, r)
	}

	// A spoofed identity from the outside dies at the edge, session or
	// not.
	r.Header.Del("Remote-User")

	if r.URL.Path == "/auth" {
		return r, true, h.serveAuthEndpoint(w, r)
	}

	user, ok := h.authSessionUser(r)
	if !ok {
		w.Header().Set("Cache-Control", "no-store")
		if authBrowserShaped(r) {
			http.Redirect(w, r, "/auth?return_to="+
				url.QueryEscape(safeReturnTo(r.URL.RequestURI())), http.StatusFound)
			return r, true, nil
		}
		// No WWW-Authenticate: the wall speaks cookies, not Bearer.
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return r, true, nil
	}

	stripAuthCookies(r)
	r.Header.Set("Remote-User", user)
	r = r.WithContext(context.WithValue(r.Context(), authIdentityKey{}, user))
	return r, false, nil
}

// authSessionUser resolves the session cookie against the store AND this
// site's effective user set: per-request authorization, not a login-time
// filter. A session minted on site A never passes site B unless B's
// resolved set contains that user.
func (h *Handler) authSessionUser(r *http.Request) (string, bool) {
	cfg := h.authCfg
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	user, _, ok := cfg.store.lookup(c.Value, cfg.ttl)
	if !ok {
		return "", false
	}
	if _, allowed := cfg.users[user]; !allowed {
		return "", false
	}
	return user, true
}

// authRejectPlainHTTP is the dead-wall answer, isolated so the mechanism
// can be swapped on an owner re-ruling: a guarded site reached over
// plain HTTP can never set the __Host- session cookie, so the wall would
// be unpassable — 421 with a plain body, and one ERROR log per site.
func (h *Handler) authRejectPlainHTTP(w http.ResponseWriter, r *http.Request) error {
	if h.authCfg.plainHTTPLogged.CompareAndSwap(false, true) {
		h.logger.Error("janus auth: guarded site reached over plain HTTP — the __Host-janus_auth cookie cannot be set without HTTPS; answering 421 (add 'auth off' to this site block or serve it over HTTPS)",
			zap.String("host", normalizeHostHeader(r.Host)),
		)
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "auth requires HTTPS; the __Host- session cookie cannot be set over plain HTTP",
		http.StatusMisdirectedRequest)
	return nil
}

// authBrowserShaped applies the 302/401 fork: GET/HEAD with an Accept
// containing text/html (a substring test) gets the redirect a human can
// follow; everything else — API calls, fetches, WebSocket upgrades — a
// 401 (an upgrade cannot follow a redirect, so it is never redirected).
func authBrowserShaped(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if websocket.IsWebSocketUpgrade(r) {
		return false
	}
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// stripAuthCookies removes the wall's own cookies from the request
// before it proceeds: the tenant never holds a live bearer token for its
// own wall. Every other cookie rides through untouched.
func stripAuthCookies(r *http.Request) {
	if r.Header.Get("Cookie") == "" {
		return
	}
	kept := make([]string, 0, 4)
	for _, c := range r.Cookies() {
		if c.Name == authCookieName || c.Name == authCSRFCookieName {
			continue
		}
		kept = append(kept, c.Name+"="+c.Value)
	}
	if len(kept) == 0 {
		r.Header.Del("Cookie")
		return
	}
	r.Header.Set("Cookie", strings.Join(kept, "; "))
}

// --- the /auth endpoint -------------------------------------------------------------

// serveAuthEndpoint is the one reserved URL: two visual states, a
// four-arm state machine, every response no-store.
func (h *Handler) serveAuthEndpoint(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return h.serveAuthPage(w, r)
	case http.MethodPost:
		return h.serveAuthPost(w, r)
	default:
		// The method table wins over the wall's 401: /auth is claimed,
		// and a claimed URL answers its own grammar.
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
}

// serveAuthPage renders the login form (no/invalid session) or the
// signed-in status page (valid session). Both plant a fresh CSRF pair —
// the status page re-mints so its sign-out form works after login
// cleared the login form's cookie. HEAD never sets cookies.
func (h *Handler) serveAuthPage(w http.ResponseWriter, r *http.Request) error {
	cfg := h.authCfg
	token := cfg.store.mintCSRF()
	if r.Method != http.MethodHead {
		authSetCookie(w, authCSRFCookieName, token, authCSRFCookieAge)
	}
	data := authPageData{
		Host: normalizeHostHeader(r.Host),
		CSRF: token,
	}
	if user, issuedAt, ok := h.authPageSession(r); ok {
		data.User = user
		data.Since = issuedAt.Local().Format("15:04")
	} else {
		data.ReturnTo = safeReturnTo(r.URL.Query().Get("return_to"))
	}
	return h.renderAuthPage(w, http.StatusOK, data)
}

// authPageSession resolves the session for page rendering (same rules as
// the wall: live session AND the user is in this site's set).
func (h *Handler) authPageSession(r *http.Request) (string, time.Time, bool) {
	cfg := h.authCfg
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return "", time.Time{}, false
	}
	user, issuedAt, ok := cfg.store.lookup(c.Value, cfg.ttl)
	if !ok {
		return "", time.Time{}, false
	}
	if _, allowed := cfg.users[user]; !allowed {
		return "", time.Time{}, false
	}
	return user, issuedAt, true
}

func (h *Handler) renderAuthPage(w http.ResponseWriter, status int, data authPageData) error {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	return authPageTmpl.Execute(w, data)
}

// serveAuthPost parses exactly one application/x-www-form-urlencoded
// body and dispatches by form fields: a POST is a sign-out iff neither a
// user nor a password field is present; otherwise it is a sign-in
// attempt (empty-but-present values are sign-in attempts that fail
// verification, never sign-outs — the stale-login-tab rule).
func (h *Handler) serveAuthPost(w http.ResponseWriter, r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/x-www-form-urlencoded" {
		http.Error(w, "auth accepts application/x-www-form-urlencoded only", http.StatusUnsupportedMediaType)
		return nil
	}
	// The byte cap gates BEFORE parsing: an oversized body dies at the
	// reader, never in a parsed form.
	r.Body = http.MaxBytesReader(w, r.Body, authBodyCap)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed or oversized form body", http.StatusBadRequest)
		return nil
	}
	for _, k := range []string{"user", "password", "_csrf"} {
		if len(r.PostForm[k]) > 1 {
			http.Error(w, "duplicated form field "+strconv.Quote(k), http.StatusBadRequest)
			return nil
		}
	}
	_, hasUser := r.PostForm["user"]
	_, hasPassword := r.PostForm["password"]
	if !hasUser && !hasPassword {
		return h.serveAuthSignOut(w, r)
	}
	return h.serveAuthSignIn(w, r)
}

// serveAuthSignIn is the login arm, in checking order: CSRF, cheap caps,
// throttle, semaphore, then the argon2 burn.
func (h *Handler) serveAuthSignIn(w http.ResponseWriter, r *http.Request) error {
	cfg := h.authCfg
	st := cfg.store
	if !st.csrfOK(r) {
		http.Error(w, "csrf required", http.StatusForbidden)
		return nil
	}
	rawUser := strings.TrimSpace(r.PostForm.Get("user"))
	password := r.PostForm.Get("password")
	if len(rawUser) > authUserCap || len(password) > authPasswordCap {
		http.Error(w, "invalid credentials", http.StatusBadRequest)
		return nil
	}
	name := strings.ToLower(rawUser)
	returnTo := safeReturnTo(r.PostForm.Get("return_to"))

	throttleKey := authClientKey(r.RemoteAddr) + "\x00" + name
	if blocked, retryAfter := st.throttleCheck(throttleKey); blocked {
		st.throttled.Add(1)
		secs := int(retryAfter/time.Second) + 1
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return nil
	}
	if !st.acquireVerify() {
		// The queue behind the argon2 semaphore is bounded: a
		// distinct-pair spray past the depth fast-fails instead of
		// stacking 64 MiB allocations.
		st.throttled.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "verification queue is full", http.StatusTooManyRequests)
		return nil
	}
	blob, known := cfg.users[name]
	ok := st.verifyCredential(password, blob, known)
	st.releaseVerify()

	if !ok {
		st.throttleFail(throttleKey)
		st.loginFailures.Add(1)
		// One generic message — which half was wrong is never
		// disclosed. The re-rendered form reuses the submitted CSRF
		// pair (cookie untouched, still within its window).
		return h.renderAuthPage(w, http.StatusUnauthorized, authPageData{
			Host:     normalizeHostHeader(r.Host),
			CSRF:     r.PostForm.Get("_csrf"),
			ReturnTo: returnTo,
			Error:    "Invalid credentials",
		})
	}

	st.throttleClear(throttleKey)
	token, err := st.mint(name)
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	st.logins.Add(1)
	authSetCookie(w, authCookieName, token, 0)
	authClearCookie(w, authCSRFCookieName)
	http.Redirect(w, r, returnTo, http.StatusSeeOther)
	return nil
}

// serveAuthSignOut is the sign-out arm: CSRF validated, the session the
// cookie names revoked server-side, both cookies cleared, 303 back to
// /auth — which now renders the login form.
func (h *Handler) serveAuthSignOut(w http.ResponseWriter, r *http.Request) error {
	st := h.authCfg.store
	if !st.csrfOK(r) {
		http.Error(w, "csrf required", http.StatusForbidden)
		return nil
	}
	if c, err := r.Cookie(authCookieName); err == nil {
		st.revokeToken(c.Value)
	}
	st.signouts.Add(1)
	authClearCookie(w, authCookieName)
	authClearCookie(w, authCSRFCookieName)
	http.Redirect(w, r, "/auth", http.StatusSeeOther)
	return nil
}

// authSetCookie writes a __Host- shaped cookie: Secure, HttpOnly,
// SameSite=Lax, Path=/, no Domain. maxAge 0 = session-scoped (the
// server's clock is the sole expiry authority).
func authSetCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
	})
}

func authClearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}
