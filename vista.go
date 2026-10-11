package janus

// Capability 11, vista: the pooled state that survives reloads. Each route
// (host and path) holds its live sessions, keyed by an HMAC of their
// tokens, the tombstones of ended ones, its caps' counts, and counters.
// Routes are created on first use and removed when a committed generation
// no longer declares them (docs/20261010-232539-capability-vista.md).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	vistaCookieName   = "__Host-ov"
	vistaTombstoneTTL = 10 * time.Minute
	vistaTokenLen     = 43 // 32 bytes, base64url without padding
)

// Session end causes, as OV-Session-Ended names them.
const (
	vistaEndBye           = "bye"
	vistaEndClosed        = "closed"
	vistaEndSignonTimeout = "signon-timeout"
	vistaEndRPCTimeout    = "rpc-timeout"
	vistaEndReplyTooLarge = "reply-too-large"
	vistaEndProtocol      = "protocol"
	vistaEndReplaced      = "replaced"
	vistaEndRotated       = "rotated"
	vistaEndRouteRemoved  = "route-removed"
	vistaEndUnknown       = "unknown"
)

var vistaEndCauses = []string{vistaEndBye, vistaEndClosed, vistaEndSignonTimeout, vistaEndRPCTimeout,
	vistaEndReplyTooLarge, vistaEndProtocol, vistaEndReplaced, vistaEndRouteRemoved}

// vistaPool is the process-wide holder in janusState.
type vistaPool struct {
	logger *zap.Logger
	key    []byte // per-process HMAC key: a memory image holds no usable token

	mu        sync.Mutex
	routes    map[string]*vistaRouteState
	committed map[string]bool // the keys the serving generation declares
	gen       any             // the generation that committed them
}

func newVistaPool(logger *zap.Logger) *vistaPool {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic("janus vista: crypto/rand: " + err.Error())
	}
	return &vistaPool{logger: logger, key: key, routes: map[string]*vistaRouteState{}}
}

func vistaRouteKey(host, path string) string { return host + path }

// route returns the route's state, creating it on first use.
func (p *vistaPool) route(key, target string, gen any) *vistaRouteState {
	p.mu.Lock()
	defer p.mu.Unlock()
	rs := p.routes[key]
	if rs == nil {
		rs = newVistaRouteState(p, key, target, gen)
		p.routes[key] = rs
	}
	return rs
}

// commit makes keys the serving generation's routes: every other route's
// sessions end (route-removed) and the route is forgotten.
func (p *vistaPool) commit(gen any, keys map[string]bool) {
	p.mu.Lock()
	p.committed, p.gen = keys, gen
	var gone []*vistaRouteState
	for k, rs := range p.routes {
		if !keys[k] {
			gone = append(gone, rs)
			delete(p.routes, k)
		}
	}
	p.mu.Unlock()
	for _, rs := range gone {
		if n := rs.endAll(vistaEndRouteRemoved); n > 0 {
			p.logger.Warn("janus vista: a reload removed a route; its sessions ended",
				zap.String("route", rs.key), zap.Int("sessions", n))
		}
	}
}

// retire forgets the routes an aborted generation created and the serving
// one does not declare.
func (p *vistaPool) retire(gen any) {
	p.mu.Lock()
	if p.gen == gen {
		p.mu.Unlock()
		return
	}
	var gone []*vistaRouteState
	for k, rs := range p.routes {
		if rs.createdBy == gen && !p.committed[k] {
			gone = append(gone, rs)
			delete(p.routes, k)
		}
	}
	p.mu.Unlock()
	for _, rs := range gone {
		rs.endAll(vistaEndRouteRemoved)
	}
}

// closeAll ends every session: process shutdown.
func (p *vistaPool) closeAll() {
	p.mu.Lock()
	routes := make([]*vistaRouteState, 0, len(p.routes))
	for _, rs := range p.routes {
		routes = append(routes, rs)
	}
	p.mu.Unlock()
	for _, rs := range routes {
		rs.endAll(vistaEndClosed)
	}
}

func (p *vistaPool) mac(token string) string {
	m := hmac.New(sha256.New, p.key)
	m.Write([]byte(token))
	return string(m.Sum(nil))
}

func vistaMintToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("janus vista: crypto/rand: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func vistaSessionID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic("janus vista: crypto/rand: " + err.Error())
	}
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 16)
	for i, c := range b {
		out[2*i], out[2*i+1] = hexDigits[c>>4], hexDigits[c&15]
	}
	return string(out)
}

type vistaTomb struct {
	cause string
	at    time.Time
}

// vistaRouteState is one route's sessions, caps, tombstones, and counters.
type vistaRouteState struct {
	pool      *vistaPool
	key       string
	target    string
	createdBy any

	mu                sync.Mutex
	sessions          map[string]*vistaSession // token HMAC → session
	all               map[*vistaSession]bool   // every live session, including one still starting
	total             int                      // sessions, including those still starting
	unsigned          int
	perClient         map[netip.Prefix]int
	perClientUnsigned map[netip.Prefix]int
	tombs             map[string]vistaTomb
	tombOrder         []string
	tombCap           int

	lim atomic.Pointer[vistaLimits] // the serving generation's limits, for status

	c vistaCounters
}

type vistaCounters struct {
	started, signedOn, rejected                atomic.Int64
	requests, bytesIn, bytesOut                atomic.Int64
	clientGone, aborted, queueTimeout, running atomic.Int64
	queued, draining                           atomic.Int64
	ended                                      sync.Map // cause → *atomic.Int64
	refused                                    sync.Map // row → *atomic.Int64
	refusedLogAt                               sync.Map // row → *vistaLogGate
}

func newVistaRouteState(p *vistaPool, key, target string, gen any) *vistaRouteState {
	return &vistaRouteState{
		pool: p, key: key, target: target, createdBy: gen,
		sessions: map[string]*vistaSession{}, all: map[*vistaSession]bool{},
		perClient: map[netip.Prefix]int{}, perClientUnsigned: map[netip.Prefix]int{},
		tombs: map[string]vistaTomb{}, tombCap: vistaDefaultMaxSessions * 4,
	}
}

func counterOf(m *sync.Map, name string) *atomic.Int64 {
	if v, ok := m.Load(name); ok {
		return v.(*atomic.Int64)
	}
	v, _ := m.LoadOrStore(name, new(atomic.Int64))
	return v.(*atomic.Int64)
}

// reserve claims a new session's slots under every cap, or names the cap
// that is full. The caller releases with unreserve when the start fails.
func (rs *vistaRouteState) reserve(l *vistaLimits, key netip.Prefix, exempt bool) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.tombCap = l.maxSessions * 4
	switch {
	case rs.total >= l.maxSessions:
		return "cap_route"
	case rs.unsigned >= l.maxUnsigned:
		return "cap_unsigned"
	case !exempt && rs.perClient[key] >= l.perClient:
		return "cap_client"
	case !exempt && rs.perClientUnsigned[key] >= l.perClientUnsigned:
		return "cap_client_unsigned"
	}
	rs.total++
	rs.unsigned++
	rs.perClient[key]++
	rs.perClientUnsigned[key]++
	return ""
}

// add records a reserved session as live (not yet holding a token).
func (rs *vistaRouteState) add(s *vistaSession) {
	rs.mu.Lock()
	rs.all[s] = true
	rs.mu.Unlock()
}

func (rs *vistaRouteState) unreserve(key netip.Prefix, signed bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.total--
	if !signed {
		rs.unsigned--
		decPrefix(rs.perClientUnsigned, key)
	}
	decPrefix(rs.perClient, key)
}

func decPrefix(m map[netip.Prefix]int, k netip.Prefix) {
	if m[k] <= 1 {
		delete(m, k)
		return
	}
	m[k]--
}

// bind maps a token to a session, retiring the session's previous token as
// a tombstone with cause when it had one.
func (rs *vistaRouteState) bind(s *vistaSession, token, cause string) {
	mac := rs.pool.mac(token)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if s.mac != "" {
		delete(rs.sessions, s.mac)
		rs.tombLocked(s.mac, cause)
	}
	s.mac = mac
	rs.sessions[mac] = s
}

// signedOn moves a session out of the unsigned counts.
func (rs *vistaRouteState) signedOn(s *vistaSession) {
	rs.mu.Lock()
	rs.unsigned--
	decPrefix(rs.perClientUnsigned, s.clientKey)
	rs.mu.Unlock()
	rs.c.signedOn.Add(1)
}

// lookup returns the live session for token, or the cause it ended with
// (unknown when Janus never minted it or its tombstone has gone).
func (rs *vistaRouteState) lookup(token string) (*vistaSession, string) {
	mac := rs.pool.mac(token)
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if s := rs.sessions[mac]; s != nil {
		return s, ""
	}
	if t, ok := rs.tombs[mac]; ok {
		if time.Since(t.at) < vistaTombstoneTTL {
			return nil, t.cause
		}
		delete(rs.tombs, mac)
	}
	return nil, vistaEndUnknown
}

// forget removes an ended session and leaves its token's tombstone.
func (rs *vistaRouteState) forget(s *vistaSession, cause string, signed bool) {
	rs.mu.Lock()
	if !rs.all[s] {
		rs.mu.Unlock()
		return
	}
	delete(rs.all, s)
	if s.mac != "" {
		delete(rs.sessions, s.mac)
		rs.tombLocked(s.mac, cause)
	}
	rs.mu.Unlock()
	rs.unreserve(s.clientKey, signed)
	counterOf(&rs.c.ended, cause).Add(1)
}

func (rs *vistaRouteState) tombLocked(mac, cause string) {
	if _, ok := rs.tombs[mac]; !ok {
		rs.tombOrder = append(rs.tombOrder, mac)
	}
	rs.tombs[mac] = vistaTomb{cause: cause, at: time.Now()}
	for len(rs.tombOrder) > rs.tombCap {
		old := rs.tombOrder[0]
		rs.tombOrder = rs.tombOrder[1:]
		delete(rs.tombs, old)
	}
	// Expired tombstones leave from the front as they age.
	for len(rs.tombOrder) > 0 {
		t, ok := rs.tombs[rs.tombOrder[0]]
		if ok && time.Since(t.at) < vistaTombstoneTTL {
			break
		}
		delete(rs.tombs, rs.tombOrder[0])
		rs.tombOrder = rs.tombOrder[1:]
	}
}

// endAll ends every session on the route with cause and returns how many.
func (rs *vistaRouteState) endAll(cause string) int {
	rs.mu.Lock()
	live := make([]*vistaSession, 0, len(rs.all))
	for s := range rs.all {
		live = append(live, s)
	}
	rs.mu.Unlock()
	for _, s := range live {
		s.end(cause)
	}
	return len(live)
}

// refuse counts a refusal and logs it at warn, at most one line per route
// and row each 10 s, with how many were suppressed since the last line.
func (rs *vistaRouteState) refuse(row string, warn bool, fields ...zap.Field) {
	counterOf(&rs.c.refused, row).Add(1)
	if !warn {
		rs.pool.logger.Debug("janus vista: refused", append(fields, zap.String("route", rs.key), zap.String("reason", row))...)
		return
	}
	v, _ := rs.c.refusedLogAt.LoadOrStore(row, &vistaLogGate{})
	gate := v.(*vistaLogGate)
	if suppressed, ok := gate.allow(10 * time.Second); ok {
		rs.pool.logger.Warn("janus vista: refused",
			append(fields, zap.String("route", rs.key), zap.String("reason", row), zap.Int64("suppressed", suppressed))...)
	}
}

// vistaLogGate rate-limits one kind of log line.
type vistaLogGate struct {
	mu         sync.Mutex
	last       time.Time
	suppressed int64
}

func (g *vistaLogGate) allow(every time.Duration) (int64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.last.IsZero() && time.Since(g.last) < every {
		g.suppressed++
		return 0, false
	}
	n := g.suppressed
	g.suppressed, g.last = 0, time.Now()
	return n, true
}

// status is the route's row on GET /1.0/vista.
func (rs *vistaRouteState) status(l *vistaLimits) vistaRouteStatus {
	rs.mu.Lock()
	st := vistaRouteStatus{
		Target:   rs.target,
		Sessions: rs.total,
		Unsigned: rs.unsigned,
	}
	var oldest time.Time
	for s := range rs.all {
		if t := s.runningSince(); !t.IsZero() && (oldest.IsZero() || t.Before(oldest)) {
			oldest = t
		}
	}
	rs.mu.Unlock()
	if !oldest.IsZero() {
		st.OldestRunningMS = time.Since(oldest).Milliseconds()
	}
	if l != nil {
		st.MaxSessions, st.MaxUnsigned = l.maxSessions, l.maxUnsigned
	}
	c := &rs.c
	st.Running, st.Queued, st.Draining = c.running.Load(), c.queued.Load(), c.draining.Load()
	st.Started, st.SignedOn, st.Rejected = c.started.Load(), c.signedOn.Load(), c.rejected.Load()
	st.Requests, st.BytesIn, st.BytesOut = c.requests.Load(), c.bytesIn.Load(), c.bytesOut.Load()
	st.ClientGone, st.Aborted, st.QueueTimeout = c.clientGone.Load(), c.aborted.Load(), c.queueTimeout.Load()
	st.Ended = map[string]int64{}
	for _, cause := range vistaEndCauses {
		st.Ended[vistaCounterName(cause)] = counterOf(&c.ended, cause).Load()
	}
	st.Refused = map[string]int64{}
	for _, row := range vistaRefusalRows {
		st.Refused[row] = counterOf(&c.refused, row).Load()
	}
	return st
}

func vistaCounterName(cause string) string {
	out := []byte(cause)
	for i, b := range out {
		if b == '-' {
			out[i] = '_'
		}
	}
	return string(out)
}

var vistaRefusalRows = []string{"method", "no_tls", "early_data", "origin", "content_type", "message", "token",
	"unknown", "cap_route", "cap_unsigned", "cap_client", "cap_client_unsigned", "dial", "queue"}

type vistaRouteStatus struct {
	Target          string           `json:"target"`
	Sessions        int              `json:"sessions"`
	Unsigned        int              `json:"unsigned"`
	Running         int64            `json:"running"`
	Queued          int64            `json:"queued"`
	Draining        int64            `json:"draining"`
	OldestRunningMS int64            `json:"oldest_running_ms"`
	MaxSessions     int              `json:"max_sessions"`
	MaxUnsigned     int              `json:"max_unsigned"`
	Started         int64            `json:"started"`
	SignedOn        int64            `json:"signed_on"`
	Rejected        int64            `json:"rejected"`
	Requests        int64            `json:"requests"`
	BytesIn         int64            `json:"bytes_in"`
	BytesOut        int64            `json:"bytes_out"`
	ClientGone      int64            `json:"client_gone"`
	Aborted         int64            `json:"aborted"`
	QueueTimeout    int64            `json:"queue_timeout"`
	Ended           map[string]int64 `json:"ended"`
	Refused         map[string]int64 `json:"refused"`
}
