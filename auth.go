package janus

import (
	"bufio"
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

// The auth wall (docs/20260728-160734-capability-auth.md). Shared users,
// path-prefix gates with allow lists, one host-wide session cookie, and
// strip-then-inject Remote-User. The wall runs in the site handler
// between ping and hub interception; sessions survive a config reload
// and die with the process.

//go:embed auth.html
var authPageHTML string

var authPageTmpl = template.Must(template.New("auth").Parse(authPageHTML))

// authPageData feeds the embedded two-state page: User empty renders the
// login form; User set renders the signed-in status page.
type authPageData struct {
	Host  string
	User  string
	Since string
	CSRF  string
	To    string
	Error string
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
	return g1Prefix + g1Encode(raw), nil
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
// client's cookie; the store key is HMAC(per-boot key, token). Sessions
// are host-wide (not bound to a single gate).
type authSession struct {
	user     string
	host     string
	issuedAt time.Time
	lastSeen time.Time
}

// authThrottleEntry is one ladder/ban row for a throttle key.
type authThrottleEntry struct {
	fails        int
	blockedUntil time.Time
	banUntil     time.Time
	lastFail     time.Time
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
	throttle map[string]*authThrottleEntry // gate\x00client[\x00user] → ladder

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
	banned        atomic.Int64
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
// rows (active bans are kept until banUntil).
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
		if t == nil {
			delete(st.throttle, k)
			continue
		}
		if !t.banUntil.IsZero() && now.Before(t.banUntil) {
			continue // active ban
		}
		if !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
			continue // active ladder wait
		}
		// Keep ladder state until 24h after the last failure so counts
		// survive across wait windows; drop cold rows.
		if t.lastFail.IsZero() || now.Sub(t.lastFail) > authThrottleBan {
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
func (st *authStore) mint(user, host string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("janus auth: minting session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := st.now()
	st.mu.Lock()
	st.sessions[st.sessionKey(token)] = &authSession{
		user: user, host: strings.ToLower(host),
		issuedAt: now, lastSeen: now,
	}
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
	ID     string   `json:"id"`
	User   string   `json:"user"`
	Host   string   `json:"host"`
	Gates  []string `json:"gates"`
	AgeMS  int64    `json:"age_ms"`
	IdleMS int64    `json:"idle_ms"`
}

func (st *authStore) sessionListRaw() []authSessionInfo {
	now := st.now()
	st.mu.Lock()
	out := make([]authSessionInfo, 0, len(st.sessions))
	for k, s := range st.sessions {
		out = append(out, authSessionInfo{
			ID:     k[:12],
			User:   s.user,
			Host:   s.host,
			Gates:  []string{},
			AgeMS:  now.Sub(s.issuedAt).Milliseconds(),
			IdleMS: now.Sub(s.lastSeen).Milliseconds(),
		})
	}
	st.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// authSessionList fills host + gates from the live site table.
func (a *App) authSessionList() []authSessionInfo {
	out := a.state.auth.sessionListRaw()
	for i := range out {
		cfg := a.authCfgForHost(out[i].Host)
		if cfg == nil {
			out[i].Gates = []string{}
			continue
		}
		out[i].Gates = cfg.gatesForUser(out[i].User)
	}
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

func authThrottlePairKey(gatePrefix, clientKey, user string) string {
	return gatePrefix + "\x00" + clientKey + "\x00" + user
}

func authThrottleAggKey(gatePrefix, clientKey string) string {
	return gatePrefix + "\x00" + clientKey + "\x00"
}

// throttleCheck reports whether the pair or the client aggregate is
// currently blocked (ladder wait or ban) and, when it is, how long until
// the wait opens. Checked before any argon2 work. A 429 wait does not
// increment the attempt counter — the caller must not call throttleFail.
func (st *authStore) throttleCheck(gatePrefix, clientKey, user string) (blocked bool, retryAfter time.Duration) {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, key := range []string{
		authThrottlePairKey(gatePrefix, clientKey, user),
		authThrottleAggKey(gatePrefix, clientKey),
	} {
		t := st.throttle[key]
		if t == nil {
			continue
		}
		if !t.banUntil.IsZero() && now.Before(t.banUntil) {
			return true, t.banUntil.Sub(now)
		}
		if !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
			return true, t.blockedUntil.Sub(now)
		}
	}
	return false, 0
}

// throttleFail records one failed attempt on the pair and the client
// aggregate for the gate. Map bound: sweep expired first; never casually
// evict active ban rows — refuse new non-ban entries at the cap instead.
func (st *authStore) throttleFail(gatePrefix, clientKey, user string) {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.throttleFailLocked(authThrottlePairKey(gatePrefix, clientKey, user), now)
	st.throttleFailLocked(authThrottleAggKey(gatePrefix, clientKey), now)
}

func (st *authStore) throttleFailLocked(key string, now time.Time) {
	t := st.throttle[key]
	if t != nil && !t.banUntil.IsZero() && now.Before(t.banUntil) {
		return // already banned; further fails are no-ops while banned
	}
	if t != nil && !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return // still in a 429 wait — caller should not reach here
	}
	if t == nil {
		if !st.throttleAdmitLocked(now) {
			return
		}
		t = &authThrottleEntry{}
		st.throttle[key] = t
	}
	t.fails++
	t.lastFail = now
	switch {
	case t.fails >= authThrottleBanAt:
		wasBanned := !t.banUntil.IsZero() && now.Before(t.banUntil)
		t.banUntil = now.Add(authThrottleBan)
		t.blockedUntil = t.banUntil
		if !wasBanned {
			st.banned.Add(1)
		}
	case t.fails == 6:
		t.blockedUntil = now.Add(authThrottleWait6)
	case t.fails == 5:
		t.blockedUntil = now.Add(authThrottleWait5)
	case t.fails == 4:
		t.blockedUntil = now.Add(authThrottleWait4)
	default:
		// 1–3: immediate failure, no added wait
		t.blockedUntil = time.Time{}
	}
}

// throttleAdmitLocked frees cold non-ban rows and reports whether a new
// non-ban entry may be inserted. Active bans are never evicted.
func (st *authStore) throttleAdmitLocked(now time.Time) bool {
	if len(st.throttle) < authThrottleMaxEntries {
		return true
	}
	for k, e := range st.throttle {
		if e == nil {
			delete(st.throttle, k)
			continue
		}
		if !e.banUntil.IsZero() && now.Before(e.banUntil) {
			continue // never casually evict active bans
		}
		if !e.blockedUntil.IsZero() && now.Before(e.blockedUntil) {
			continue
		}
		if e.lastFail.IsZero() || now.Sub(e.lastFail) > authThrottleBan {
			delete(st.throttle, k)
		}
	}
	if len(st.throttle) < authThrottleMaxEntries {
		return true
	}
	// Prefer refusing new non-ban entries at the cap over evicting bans.
	return false
}

// throttleClear forgets the pair and clears the client aggregate ladder
// for the gate. An active client ban is not cleared by success.
func (st *authStore) throttleClear(gatePrefix, clientKey, user string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.throttle, authThrottlePairKey(gatePrefix, clientKey, user))
	aggKey := authThrottleAggKey(gatePrefix, clientKey)
	if t := st.throttle[aggKey]; t != nil {
		now := st.now()
		if !t.banUntil.IsZero() && now.Before(t.banUntil) {
			// Preserve the ban; drop ladder fails so a fresh spray
			// budget starts after the ban expires.
			t.fails = authThrottleBanAt
			t.blockedUntil = t.banUntil
			return
		}
		delete(st.throttle, aggKey)
	}
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
// list.
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
// in a hidden field and the short-lived __Host-janus_csrf cookie;
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
	field := r.PostForm.Get("csrf")
	if field == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(c.Value), []byte(field)) == 1 && st.validCSRF(field)
}

// --- to ---------------------------------------------------------------------------

// safeTo admits same-origin paths only, then requires the result stay
// under the minting gate's normalized prefix. Anything else collapses
// to that prefix.
func safeTo(raw, gatePrefix string) string {
	fallback := gatePrefix
	if raw == "" || len(raw) >= authToCap {
		return fallback
	}
	if raw[0] != '/' {
		return fallback
	}
	if len(raw) > 1 && (raw[1] == '/' || raw[1] == '\\') {
		return fallback
	}
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if b <= 0x20 || b == 0x7f || b == '\\' {
			return fallback
		}
	}
	// Path portion for prefix check (ignore query).
	path := raw
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		path = raw[:i]
	}
	if !gatePathMatches(gatePrefix, path) {
		return fallback
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

// serveAuthWall runs the wall for a site with effective auth on.
// handled=true means the wall wrote the response (login door, 302/401,
// or plain-HTTP 421); otherwise the returned request — cookies stripped,
// Remote-User cleared or injected — proceeds to hub / cache / data plane.
func (h *Handler) serveAuthWall(w http.ResponseWriter, r *http.Request) (*http.Request, bool, error) {
	if r.TLS == nil {
		return r, true, h.authRejectPlainHTTP(w, r)
	}

	cfg := h.authCfg
	path := r.URL.Path

	// Login door (exact {prefix}auth) is reserved for the wall.
	if g := cfg.doorGate(path); g != nil {
		return r, true, h.serveAuthEndpoint(w, r, g)
	}

	gate := cfg.matchGate(path)
	user, sessOK := h.authSessionUser(r)
	authorized := sessOK && gate != nil && gate.allow[user]

	if gate != nil && !authorized {
		w.Header().Set("Cache-Control", "no-store")
		if authBrowserShaped(r) {
			to := safeTo(r.URL.RequestURI(), gate.prefix)
			http.Redirect(w, r, gate.door+"?to="+url.QueryEscape(to), http.StatusFound)
			return r, true, nil
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return r, true, nil
	}

	// Fall-through (gated+authorized or outside every gate): strip on
	// every proxied request when auth is on.
	r.Header.Del("Remote-User")
	stripAuthCookies(r)
	if authorized {
		r.Header.Set("Remote-User", user)
		r = r.WithContext(context.WithValue(r.Context(), authIdentityKey{}, user))
	}
	return r, false, nil
}

// authSessionUser resolves the session cookie against the store AND this
// site's users table. Gate allow-list checks happen at the call site.
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

// authRejectPlainHTTP is the dead-wall answer: a site with effective auth
// reached over plain HTTP can never set the __Host- session cookie, so
// every path answers 421 with a plain body, and one ERROR log per site.
func (h *Handler) authRejectPlainHTTP(w http.ResponseWriter, r *http.Request) error {
	if h.authCfg.plainHTTPLogged.CompareAndSwap(false, true) {
		h.logger.Error("janus auth: site with auth reached over plain HTTP — the __Host-janus cookie cannot be set without HTTPS; answering 421 (add 'auth off' to this site block or serve it over HTTPS)",
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
		if c.Name == authCookieName || c.Name == authCSRFCookieName ||
			strings.HasPrefix(c.Name, authCookieName) {
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

// authCookieNameReserved reports whether a Set-Cookie name is reserved
// for the wall (__Host-janus and __Host-janus*).
func authCookieNameReserved(name string) bool {
	return name == authCookieName || strings.HasPrefix(name, authCookieName)
}

// authRespStrip strips reserved __Host-janus* Set-Cookie values from
// upstream responses when effective auth is on.
type authRespStrip struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *authRespStrip) strip() {
	if w.wroteHeader {
		return
	}
	h := w.Header()
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, v := range vals {
		name := v
		if i := strings.IndexByte(v, '='); i >= 0 {
			name = v[:i]
		}
		if authCookieNameReserved(name) {
			continue
		}
		h.Add("Set-Cookie", v)
	}
}

func (w *authRespStrip) WriteHeader(status int) {
	w.strip()
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *authRespStrip) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *authRespStrip) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response does not implement http.Hijacker")
	}
	return h.Hijack()
}

func (w *authRespStrip) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *authRespStrip) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// --- the {prefix}auth endpoint ------------------------------------------------------

// serveAuthEndpoint is one gate's reserved login door: two visual states,
// a four-arm state machine, every response no-store.
func (h *Handler) serveAuthEndpoint(w http.ResponseWriter, r *http.Request, g *authGate) error {
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		return h.serveAuthPage(w, r, g)
	case http.MethodPost:
		return h.serveAuthPost(w, r, g)
	default:
		w.Header().Set("Allow", "GET, HEAD, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return nil
	}
}

// serveAuthPage renders the login form (no/invalid session) or the
// signed-in status page (valid session). Both plant a fresh CSRF pair —
// the status page re-mints so its sign-out form works after login
// cleared the login form's cookie. HEAD never sets cookies.
func (h *Handler) serveAuthPage(w http.ResponseWriter, r *http.Request, g *authGate) error {
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
		data.To = safeTo(r.URL.Query().Get("to"), g.prefix)
	}
	return h.renderAuthPage(w, http.StatusOK, data)
}

// authPageSession resolves the session for page rendering (live session
// whose user is in this site's users table).
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
func (h *Handler) serveAuthPost(w http.ResponseWriter, r *http.Request, g *authGate) error {
	ct := r.Header.Get("Content-Type")
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil || mt != "application/x-www-form-urlencoded" {
		http.Error(w, "auth accepts application/x-www-form-urlencoded only", http.StatusUnsupportedMediaType)
		return nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, authBodyCap)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "malformed or oversized form body", http.StatusBadRequest)
		return nil
	}
	for _, k := range []string{"user", "password", "csrf"} {
		if len(r.PostForm[k]) > 1 {
			http.Error(w, "duplicated form field "+strconv.Quote(k), http.StatusBadRequest)
			return nil
		}
	}
	_, hasUser := r.PostForm["user"]
	_, hasPassword := r.PostForm["password"]
	if !hasUser && !hasPassword {
		return h.serveAuthSignOut(w, r, g)
	}
	return h.serveAuthSignIn(w, r, g)
}

// serveAuthSignIn is the login arm, in checking order: CSRF, cheap caps,
// throttle, semaphore, then the argon2 burn. Success requires the user
// to be in this gate's allow list.
func (h *Handler) serveAuthSignIn(w http.ResponseWriter, r *http.Request, g *authGate) error {
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
	to := safeTo(r.PostForm.Get("to"), g.prefix)
	clientKey := authClientKey(r.RemoteAddr)

	if blocked, retryAfter := st.throttleCheck(g.prefix, clientKey, name); blocked {
		st.throttled.Add(1)
		secs := int(retryAfter/time.Second) + 1
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return nil
	}
	if !st.acquireVerify() {
		st.throttled.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "verification queue is full", http.StatusTooManyRequests)
		return nil
	}
	blob, known := cfg.users[name]
	ok := st.verifyCredential(password, blob, known)
	st.releaseVerify()

	// Password OK is not enough: the user must be on this gate's allow list.
	if !ok || !g.allow[name] {
		st.throttleFail(g.prefix, clientKey, name)
		st.loginFailures.Add(1)
		return h.renderAuthPage(w, http.StatusUnauthorized, authPageData{
			Host:  normalizeHostHeader(r.Host),
			CSRF:  r.PostForm.Get("csrf"),
			To:    to,
			Error: "Invalid credentials",
		})
	}

	st.throttleClear(g.prefix, clientKey, name)
	token, err := st.mint(name, normalizeHostHeader(r.Host))
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	st.logins.Add(1)
	authSetCookie(w, authCookieName, token, 0)
	authClearCookie(w, authCSRFCookieName)
	http.Redirect(w, r, to, http.StatusSeeOther)
	return nil
}

// serveAuthSignOut is the sign-out arm: CSRF validated, the host session
// the cookie names revoked server-side, both cookies cleared, 303 back
// to this gate's login door.
func (h *Handler) serveAuthSignOut(w http.ResponseWriter, r *http.Request, g *authGate) error {
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
	http.Redirect(w, r, g.door, http.StatusSeeOther)
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
