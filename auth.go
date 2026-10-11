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

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
	"golang.org/x/crypto/argon2"
)

// The auth wall (docs/20260728-160734-capability-auth.md). Shared users,
// path-prefix gates with allow lists, one host-wide session cookie, and
// strip-then-inject Remote-User. The wall runs in the site handler
// between ping and hub interception; eligible sessions survive config
// reloads and all sessions die with the process.

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
	Door  string
	Error string
}

// --- the passhash codec (version a; shared with Zift) -------------------------
//
// Exactly one definition in the codebase: janus passhash mints with the
// same constants the verifier runs. Wire form is `a` + 31 base62 chars.

// passKDF runs the fixed-parameter argon2id derivation. Every execution is
// counted (the throttle-before-KDF and timing-equalization pins read the
// counter).
func passKDF(password, salt []byte) []byte {
	authKDFRuns.Add(1)
	return argon2.IDKey(password, salt, passTime, passMemory, passThreads, passKeyLen)
}

// authKDFRuns counts argon2 executions process-wide.
var authKDFRuns atomic.Int64

// passMint derives a fresh version-a passhash from a password.
func passMint(password string) (string, error) {
	salt := make([]byte, passSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("minting salt: %w", err)
	}
	digest := passKDF([]byte(password), salt)
	raw := append(salt, digest...)
	return passPrefix + passEncode(raw), nil
}

// passVerify checks a password against a stored passhash (already validated
// at parse/provision) in constant time over the digest compare.
func passVerify(password, blob string) bool {
	raw, err := decodePasshash(blob)
	if err != nil {
		return false // unreachable for provisioned credentials
	}
	digest := passKDF([]byte(password), raw[:passSaltLen])
	return subtle.ConstantTimeCompare(digest, raw[passSaltLen:]) == 1
}

// --- the pooled store ------------------------------------------------------------

// authSession is one live session: the raw token exists only in the
// client's cookie; the store key is HMAC(per-boot key, token). Sessions
// are host-wide (not bound to a single gate).
type authSession struct {
	user     string
	host     string
	cred     string // the passhash the session was minted against
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
// registry: config reloads preserve eligible sessions while a process
// restart wipes every session and rotates the key.
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
	// across enabled sites, refreshed after a config commits. Per-request expiry
	// uses each site's own effective ttl; the reaper only stops an
	// abandoned store growing forever.
	reapTTL atomic.Int64 // nanoseconds

	// Monotonic counters are observed independently, not as one atomic
	// snapshot.
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

	// logger carries the audit events the store itself observes (control
	// and reload revocations); the committed generation's App logger,
	// refreshed at every reconcile. Nil logs nothing.
	logger atomic.Pointer[zap.Logger]

	now func() time.Time
}

// --- audit trail -------------------------------------------------------------
//
// Every login outcome, throttle or ban, sign-out, and session revocation
// is logged through zap: info for success and sign-out, warn for failure,
// throttle, and ban. Each event carries host, gate, client_ip, and user
// where the event has them. Passwords, tokens, CSRF values, and session
// ids are never logged. A failed login logs one message whether the user
// is unknown, the password wrong, or the user not on the gate's list.

// authLogUserCap bounds an attempted username in the audit trail.
const authLogUserCap = 64

// authLogUser renders a submitted username for the audit trail: capped at
// authLogUserCap bytes, every byte outside printable ASCII replaced by ?,
// so a hostile name can neither flood nor forge log lines.
func authLogUser(raw string) string {
	if len(raw) > authLogUserCap {
		raw = raw[:authLogUserCap]
	}
	b := []byte(raw)
	for i, c := range b {
		if c < 0x21 || c > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// authAuditFields are the fields every door event carries.
func authAuditFields(r *http.Request, g *authGate, user string) []zap.Field {
	return []zap.Field{
		zap.String("host", normalizeHostHeader(r.Host)),
		zap.String("gate", g.prefix),
		zap.String("client_ip", authClientIP(r)),
		zap.String("user", authLogUser(user)),
	}
}

func (h *Handler) authLog() *zap.Logger {
	if h.logger == nil {
		return zap.NewNop()
	}
	return h.logger
}

// authSetUserID records the authenticated user for Caddy's access log
// (its user_id field reads the http.auth.user.id placeholder).
func authSetUserID(r *http.Request, user string) {
	if repl, ok := r.Context().Value(caddy.ReplacerCtxKey).(*caddy.Replacer); ok {
		repl.Set("http.auth.user.id", user)
	}
}

// auditRevoked records one session ended by an operator ("control") or a
// reload ("reload"): host and username, never a token or session id.
func (st *authStore) auditRevoked(via string, s authSession) {
	l := st.logger.Load()
	if l == nil {
		return
	}
	l.Info("janus auth: session revoked",
		zap.String("via", via),
		zap.String("host", s.host),
		zap.String("user", s.user),
	)
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
		// Keep ladder state until its window passes after the last failure
		// so counts survive across wait windows; drop cold rows.
		if throttleRowCold(k, t, now) {
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
// session fixation is dead by construction. The session remembers the
// credential it was minted against: a changed passhash ends it.
func (st *authStore) mint(user, host, cred string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("janus auth: minting session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	now := st.now()
	st.mu.Lock()
	st.sessions[st.sessionKey(token)] = &authSession{
		user: user, host: strings.ToLower(host), cred: cred,
		issuedAt: now, lastSeen: now,
	}
	st.mu.Unlock()
	return token, nil
}

// lookup resolves a presented token against its minting host and the
// requesting site's effective ttl. Expired entries are deleted lazily;
// an authorized call may request a sliding lastSeen refresh.
func (st *authStore) lookup(token, host string, ttl time.Duration, refresh bool) (user string, issuedAt time.Time, ok bool) {
	return st.lookupAllowed(token, host, ttl, refresh, nil, nil)
}

// lookupAllowed combines session validation, gate authorization, and the
// optional sliding refresh under one HMAC and one store lock. A nil allow set
// means the caller only needs a host-valid live session. A non-nil users
// table (name → passhash) additionally requires the session's user to be
// present with the very credential the session was minted against, so a
// changed passhash ends the session on its next request, before the
// reload's store sweep runs.
func (st *authStore) lookupAllowed(token, host string, ttl time.Duration, refresh bool, allow map[string]bool, users map[string]string) (user string, issuedAt time.Time, ok bool) {
	if token == "" {
		return "", time.Time{}, false
	}
	host = strings.ToLower(host)
	key := st.sessionKey(token)
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions[key]
	if s == nil {
		return "", time.Time{}, false
	}
	if s.host != host {
		return "", time.Time{}, false
	}
	if now.Sub(s.lastSeen) > ttl {
		delete(st.sessions, key)
		st.expired.Add(1)
		return "", time.Time{}, false
	}
	if allow != nil && !allow[s.user] {
		return "", time.Time{}, false
	}
	if users != nil {
		if cred, present := users[s.user]; !present || cred != s.cred {
			return "", time.Time{}, false
		}
	}
	if refresh {
		s.lastSeen = now
	}
	return s.user, s.issuedAt, true
}

// revokeToken revokes the session a cookie names and reports the session
// it ended (ok=false when none was live). Revoking an already-dead
// session is a success, not an error.
func (st *authStore) revokeToken(token string) (ended authSession, ok bool) {
	if token == "" {
		return authSession{}, false
	}
	key := st.sessionKey(token)
	st.mu.Lock()
	defer st.mu.Unlock()
	s := st.sessions[key]
	if s == nil {
		return authSession{}, false
	}
	delete(st.sessions, key)
	return *s, true
}

// authSessionIDMin is the shortest id prefix revoke-by-id accepts. A
// listed id is 12 hex characters; a prefix shorter than 8 could name a
// session nobody listed.
const authSessionIDMin = 8

// revokeID revokes one session by id prefix (the /1.0/auth/sessions id:
// a 12-hex prefix of the HMACed store key). The prefix must be at least
// authSessionIDMin lowercase hex characters and resolve a unique key:
// too short, malformed, or no match → found=false; several →
// ambiguous=true.
func (st *authStore) revokeID(prefix string) (found, ambiguous bool) {
	if len(prefix) < authSessionIDMin || len(prefix) > sha256.Size*2 {
		return false, false
	}
	for i := 0; i < len(prefix); i++ {
		if c := prefix[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false, false
		}
	}
	st.mu.Lock()
	match := ""
	for k := range st.sessions {
		if strings.HasPrefix(k, prefix) {
			if match != "" {
				st.mu.Unlock()
				return false, true
			}
			match = k
		}
	}
	if match == "" {
		st.mu.Unlock()
		return false, false
	}
	ended := *st.sessions[match]
	delete(st.sessions, match)
	st.mu.Unlock()
	st.revoked.Add(1)
	st.auditRevoked("control", ended)
	return true, false
}

// revokeAll wipes every session ("kick everyone").
func (st *authStore) revokeAll() int {
	st.mu.Lock()
	ended := st.sessions
	st.sessions = map[string]*authSession{}
	st.mu.Unlock()
	st.revoked.Add(int64(len(ended)))
	for _, s := range ended {
		st.auditRevoked("control", *s)
	}
	return len(ended)
}

// revokeUsersNotIn revokes sessions whose user is absent from keep. The
// production reload path additionally considers each session's minting host;
// this helper remains useful for direct store operations and tests.
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

// authClientIP is the client address the throttle and the audit trail
// use: Caddy's client_ip, which honors the server's trusted_proxies (an
// edge behind a trusted load balancer sees each client, not the
// balancer), else the connection's RemoteAddr. With no trusted_proxies
// configured Caddy's client_ip is the peer itself, so a client-supplied
// X-Forwarded-For is never believed.
func authClientIP(r *http.Request) string {
	if ip, ok := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ok && ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// authClientKey maps a client address (bare or host:port) to its throttle
// key. IPv6 clients key on the /64 prefix (one subnet, not 2^64 fresh
// addresses); IPv4 keys on the address.
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

// authThrottleScope names one login door's ladder scope: the host and the
// gate. Failures on one host never spend another host's budget, so an
// attacker on a public host cannot ban a shared address from another.
func authThrottleScope(host, gatePrefix string) string {
	return strings.ToLower(host) + "\x00" + gatePrefix
}

func authThrottlePairKey(gatePrefix, clientKey, user string) string {
	return gatePrefix + "\x00" + clientKey + "\x00" + user
}

func authThrottleAggKey(gatePrefix, clientKey string) string {
	return gatePrefix + "\x00" + clientKey + "\x00"
}

// authThrottleAccountKey keys the per-account ladder: one row per host
// and username, independent of client address and login door. The \x01
// lead byte keeps it disjoint from gate-keyed rows (a gate starts with /).
func authThrottleAccountKey(host, user string) string {
	return "\x01" + strings.ToLower(host) + "\x00" + user
}

func authThrottleAccountRow(key string) bool {
	return strings.HasPrefix(key, "\x01")
}

// authThrottleOutcome reports what one recorded failure set in motion, for
// the audit trail.
type authThrottleOutcome struct {
	wait   time.Duration // the longest ladder wait now running (0: none)
	banned bool          // this failure started a 24h client ban
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
// aggregate for the gate. The map stays bounded without ever refusing a
// row (throttleAdmitLocked), so a new client is always tracked.
func (st *authStore) throttleFail(gatePrefix, clientKey, user string) authThrottleOutcome {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	a := st.throttleFailLocked(authThrottlePairKey(gatePrefix, clientKey, user), now)
	b := st.throttleFailLocked(authThrottleAggKey(gatePrefix, clientKey), now)
	return authThrottleOutcome{wait: max(a.wait, b.wait), banned: a.banned || b.banned}
}

func (st *authStore) throttleFailLocked(key string, now time.Time) authThrottleOutcome {
	t := st.throttle[key]
	if t != nil && !t.banUntil.IsZero() && now.Before(t.banUntil) {
		return authThrottleOutcome{} // already banned; further fails are no-ops while banned
	}
	if t != nil && !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return authThrottleOutcome{} // still in a 429 wait — caller should not reach here
	}
	if t == nil {
		st.throttleAdmitLocked(now)
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
		return authThrottleOutcome{wait: authThrottleBan, banned: !wasBanned}
	case t.fails == 6:
		t.blockedUntil = now.Add(authThrottleWait6)
	case t.fails == 5:
		t.blockedUntil = now.Add(authThrottleWait5)
	case t.fails == 4:
		t.blockedUntil = now.Add(authThrottleWait4)
	default:
		// 1–3: immediate failure, no added wait
		t.blockedUntil = time.Time{}
		return authThrottleOutcome{}
	}
	return authThrottleOutcome{wait: t.blockedUntil.Sub(now)}
}

// throttleRowCold reports whether a row carries nothing worth keeping: no
// active ban or wait, and its failures have aged out of their window (24h
// for gate rows, authAccountForget for account rows).
func throttleRowCold(key string, e *authThrottleEntry, now time.Time) bool {
	if e == nil {
		return true
	}
	if !e.banUntil.IsZero() && now.Before(e.banUntil) {
		return false
	}
	if !e.blockedUntil.IsZero() && now.Before(e.blockedUntil) {
		return false
	}
	window := authThrottleBan
	if authThrottleAccountRow(key) {
		window = authAccountForget
	}
	return e.lastFail.IsZero() || now.Sub(e.lastFail) > window
}

// throttleAdmitLocked makes room for one new row; tracking never stops at
// the cap. Cold rows are swept first. If the table is still full, the
// least-recently-failed row that is not an active ban is evicted; only a
// table made entirely of active bans gives up its soonest-expiring ban.
func (st *authStore) throttleAdmitLocked(now time.Time) {
	if len(st.throttle) < authThrottleMaxEntries {
		return
	}
	for k, e := range st.throttle {
		if throttleRowCold(k, e, now) {
			delete(st.throttle, k)
		}
	}
	for len(st.throttle) >= authThrottleMaxEntries {
		victim, banVictim := "", ""
		var oldest, soonest time.Time
		for k, e := range st.throttle {
			if !e.banUntil.IsZero() && now.Before(e.banUntil) {
				if banVictim == "" || e.banUntil.Before(soonest) {
					banVictim, soonest = k, e.banUntil
				}
				continue
			}
			if victim == "" || e.lastFail.Before(oldest) {
				victim, oldest = k, e.lastFail
			}
		}
		if victim == "" {
			victim = banVictim
		}
		delete(st.throttle, victim)
	}
}

// throttleClear runs on a successful login: it forgets the pair and
// refunds the pair's own failures from the client aggregate, so a person's
// typos before signing in do not count against everyone behind the same
// address. Failures against other usernames stay in the aggregate — a
// success never resets the client's spray budget — and an active
// aggregate wait or ban is never lifted.
func (st *authStore) throttleClear(gatePrefix, clientKey, user string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	pairKey := authThrottlePairKey(gatePrefix, clientKey, user)
	own := 0
	if t := st.throttle[pairKey]; t != nil {
		own = t.fails
	}
	delete(st.throttle, pairKey)
	if t := st.throttle[authThrottleAggKey(gatePrefix, clientKey)]; t != nil && own > 0 {
		t.fails = max(t.fails-own, 0)
	}
}

// accountCheck reports whether the per-account ladder currently holds
// logins to user on host, and for how long. Checked before argon2 work.
func (st *authStore) accountCheck(host, user string) (blocked bool, retryAfter time.Duration) {
	now := st.now()
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.throttle[authThrottleAccountKey(host, user)]
	if t != nil && !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return true, t.blockedUntil.Sub(now)
	}
	return false, 0
}

// accountFail records one failed login against user on host from any
// client. The first authAccountFreeFails failures within the forget window
// cost nothing; each further one holds the account for a doubling wait
// capped at authAccountWaitMax. The ladder never bans: a person waits at
// most authAccountWaitMax, while a distributed guesser is held to about one
// guess per authAccountWaitMax against that account.
func (st *authStore) accountFail(host, user string) authThrottleOutcome {
	now := st.now()
	key := authThrottleAccountKey(host, user)
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.throttle[key]
	if t != nil && !t.blockedUntil.IsZero() && now.Before(t.blockedUntil) {
		return authThrottleOutcome{} // in a 429 wait — the attempt never ran
	}
	if t != nil && now.Sub(t.lastFail) > authAccountForget {
		t.fails = 0
	}
	if t == nil {
		st.throttleAdmitLocked(now)
		t = &authThrottleEntry{}
		st.throttle[key] = t
	}
	t.fails++
	t.lastFail = now
	over := t.fails - authAccountFreeFails
	if over <= 0 {
		t.blockedUntil = time.Time{}
		return authThrottleOutcome{}
	}
	wait := authAccountWaitMax
	if over <= 4 {
		wait = min(authAccountWaitFirst<<(over-1), authAccountWaitMax)
	}
	t.blockedUntil = now.Add(wait)
	return authThrottleOutcome{wait: wait}
}

// accountClear forgets the account ladder after a successful login: only
// someone holding the account's password can reset it.
func (st *authStore) accountClear(host, user string) {
	st.mu.Lock()
	delete(st.throttle, authThrottleAccountKey(host, user))
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
// list.
func (st *authStore) verifyCredential(password, blob string, known bool) bool {
	if known {
		return passVerify(password, blob)
	}
	st.dummyOnce.Do(func() {
		st.dummySalt = make([]byte, passSaltLen)
		_, _ = rand.Read(st.dummySalt)
		st.dummyHash = passKDF([]byte("janus-auth-dummy"), st.dummySalt)
	})
	digest := passKDF([]byte(password), st.dummySalt)
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
	u, err := url.ParseRequestURI(raw)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" {
		return fallback
	}
	escaped := u.EscapedPath()
	for i := 0; i+2 < len(escaped); i++ {
		if escaped[i] != '%' {
			continue
		}
		value, err := strconv.ParseUint(escaped[i+1:i+3], 16, 8)
		if err != nil || value == '/' || value == '\\' {
			return fallback
		}
		i += 2
	}
	decoded, err := url.PathUnescape(escaped)
	if err != nil || decoded != u.Path || validateRequestPathString(decoded) != nil {
		return fallback
	}
	if !gatePathMatches(gatePrefix, decoded) {
		return fallback
	}
	return raw
}

// --- the wall ---------------------------------------------------------------------

// authWallActiveKey marks requests which passed through an enabled auth wall.
// The data-plane response hook uses it to scrub reserved cookies before even
// an upgraded or streaming upstream response can commit its headers.
type authWallActiveKey struct{}

func authWallActive(ctx context.Context) bool {
	active, _ := ctx.Value(authWallActiveKey{}).(bool)
	return active
}

// authGatedUserKey carries the user of a request that passed a gate.
type authGatedUserKey struct{}

// authGatedUser returns the user a gate admitted this request as, or "".
func authGatedUser(ctx context.Context) string {
	user, _ := ctx.Value(authGatedUserKey{}).(string)
	return user
}

// serveAuthWall runs the wall for a site with effective auth on.
// handled=true means the wall wrote the response (login door, 302/401,
// or plain-HTTP 421); otherwise the returned request — cookies stripped,
// Remote-User cleared or injected — proceeds to hub or the data plane.
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
	user, authorized := "", false
	if gate != nil {
		// Validate, authorize, and refresh in one store transaction. Open paths
		// and disallowed gates never extend the idle lifetime.
		user, authorized = h.authSessionAllowed(r, gate.allow)
	}

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
	// every proxied request when auth is on. The site handler already
	// stripped client identity headers; the wall repeats it so its own
	// injection is the only Remote-User whatever the caller.
	stripClientIdentityHeaders(r.Header)
	stripAuthCookies(r)
	ctx := context.WithValue(r.Context(), authWallActiveKey{}, true)
	if authorized {
		r.Header.Set("Remote-User", user)
		authSetUserID(r, user)
		ctx = context.WithValue(ctx, authGatedUserKey{}, user)
	}
	r = r.WithContext(ctx)
	return r, false, nil
}

// stripClientIdentityHeaders deletes every client-sent header that names
// an identity Janus asserts: Remote-User (set only by a passed gate) and
// Rip-Site (set only from a site registration). A name matches when it
// equals one of those case-insensitively after mapping _ to -, because
// CGI-style upstreams (Rack, PHP, WSGI) fold Remote_User and Remote-User
// into the same HTTP_REMOTE_USER variable.
func stripClientIdentityHeaders(h http.Header) {
	for name := range h {
		switch strings.ToLower(strings.ReplaceAll(name, "_", "-")) {
		case "remote-user", "rip-site":
			delete(h, name)
		}
	}
}

// authSessionAllowed resolves and refreshes a session only when its user is
// authorized by the winning gate.
func (h *Handler) authSessionAllowed(r *http.Request, allow map[string]bool) (string, bool) {
	cfg := h.authCfg
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return "", false
	}
	user, _, ok := cfg.store.lookupAllowed(c.Value, normalizeHostHeader(r.Host), cfg.ttl, true, allow, cfg.users)
	return user, ok
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
	for _, line := range r.Header.Values("Cookie") {
		for _, pair := range strings.Split(line, ";") {
			pair = strings.TrimSpace(pair)
			if pair == "" {
				continue
			}
			name := pair
			if i := strings.IndexByte(pair, '='); i >= 0 {
				name = strings.TrimSpace(pair[:i])
			}
			if authCookieNameReserved(name) {
				continue
			}
			// Keep the original pair bytes, including quoted values. Parsing
			// through Request.Cookies and rebuilding name=value would silently
			// remove valid quoting from unrelated tenant cookies.
			kept = append(kept, pair)
		}
	}
	r.Header.Del("Cookie")
	if len(kept) == 0 {
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
	stripReservedAuthCookies(w.Header())
}

func stripReservedAuthCookies(h http.Header) {
	vals := h.Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	h.Del("Set-Cookie")
	for _, v := range vals {
		name := v
		if i := strings.IndexByte(v, '='); i >= 0 {
			name = strings.TrimSpace(v[:i])
		}
		if authCookieNameReserved(name) {
			continue
		}
		h.Add("Set-Cookie", v)
	}
}

func (w *authRespStrip) WriteHeader(status int) {
	w.strip()
	// A 1xx informational write (e.g. 103 Early Hints) does not commit the
	// final header set: the scrub must still run on the real status later.
	if status >= http.StatusOK {
		w.wroteHeader = true
	}
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
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
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
		Door: gateDoor(g.prefix),
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
// whose user is in this site's users table with the credential the
// session was minted against).
func (h *Handler) authPageSession(r *http.Request) (string, time.Time, bool) {
	cfg := h.authCfg
	c, err := r.Cookie(authCookieName)
	if err != nil || c.Value == "" {
		return "", time.Time{}, false
	}
	return cfg.store.lookupAllowed(c.Value, normalizeHostHeader(r.Host), cfg.ttl, false, nil, cfg.users)
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
	host := normalizeHostHeader(r.Host)
	clientKey := authClientKey(authClientIP(r))
	scope := authThrottleScope(host, g.prefix)

	blocked, retryAfter := st.throttleCheck(scope, clientKey, name)
	if !blocked {
		blocked, retryAfter = st.accountCheck(host, name)
	}
	log := h.authLog()
	if blocked {
		st.throttled.Add(1)
		secs := int(retryAfter/time.Second) + 1
		if secs < 1 {
			secs = 1
		}
		log.Warn("janus auth: login throttled",
			append(authAuditFields(r, g, name), zap.Int("retry_after_s", secs))...)
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		http.Error(w, "too many attempts", http.StatusTooManyRequests)
		return nil
	}
	if !st.acquireVerify() {
		st.throttled.Add(1)
		log.Warn("janus auth: login throttled (verification queue full)",
			append(authAuditFields(r, g, name), zap.Int("retry_after_s", 1))...)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "verification queue is full", http.StatusTooManyRequests)
		return nil
	}
	blob, known := cfg.users[name]
	ok := st.verifyCredential(password, blob, known)
	st.releaseVerify()

	// Password OK is not enough: the user must be on this gate's allow list.
	if !ok || !g.allow[name] {
		client := st.throttleFail(scope, clientKey, name)
		account := st.accountFail(host, name)
		st.loginFailures.Add(1)
		// One message for every failure cause: the log never tells an
		// unknown user from a wrong password or a disallowed gate.
		log.Warn("janus auth: login failed", authAuditFields(r, g, name)...)
		if client.banned {
			log.Warn("janus auth: client banned",
				append(authAuditFields(r, g, name), zap.Duration("ban", authThrottleBan))...)
		} else if client.wait > 0 {
			log.Warn("janus auth: client throttle engaged",
				append(authAuditFields(r, g, name), zap.Duration("wait", client.wait))...)
		}
		if account.wait > 0 {
			log.Warn("janus auth: account throttle engaged",
				append(authAuditFields(r, g, name), zap.Duration("wait", account.wait))...)
		}
		return h.renderAuthPage(w, http.StatusUnauthorized, authPageData{
			Host:  host,
			CSRF:  r.PostForm.Get("csrf"),
			To:    to,
			Door:  gateDoor(g.prefix),
			Error: "Invalid credentials",
		})
	}

	st.throttleClear(scope, clientKey, name)
	st.accountClear(host, name)
	// A login replaces the host session: the session the request arrived
	// with, if any, ends here rather than living on beside the new one.
	if c, err := r.Cookie(authCookieName); err == nil {
		if ended, live := st.revokeToken(c.Value); live {
			log.Info("janus auth: session revoked",
				append(authAuditFields(r, g, ended.user), zap.String("via", "login"))...)
		}
	}
	token, err := st.mint(name, host, blob)
	if err != nil {
		return caddyhttp.Error(http.StatusInternalServerError, err)
	}
	st.logins.Add(1)
	log.Info("janus auth: login succeeded", authAuditFields(r, g, name)...)
	authSetUserID(r, name)
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
	user := ""
	if c, err := r.Cookie(authCookieName); err == nil {
		if ended, live := st.revokeToken(c.Value); live {
			user = ended.user
		}
	}
	st.signouts.Add(1)
	h.authLog().Info("janus auth: signed out", authAuditFields(r, g, user)...)
	authClearCookie(w, authCookieName)
	authClearCookie(w, authCSRFCookieName)
	http.Redirect(w, r, g.door, http.StatusSeeOther)
	return nil
}

// authSetCookie writes a __Host- shaped cookie: Secure, HttpOnly,
// SameSite=Strict, Path=/, no Domain. maxAge 0 = session-scoped (the
// server's clock is the sole expiry authority).
func authSetCookie(w http.ResponseWriter, name, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		Secure:   true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
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
