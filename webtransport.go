package janus

// webtransport (capability 10): the pooled relay. One process-wide set of
// UDP sockets, each with its own QUIC transport, HTTP/3 server, and
// WebTransport server; a committed table of routes; and the sessions
// relaying datagrams between browsers and fixed UDP targets. The state
// lives in janusState so a config reload never rebinds a socket or
// touches a session it does not have to. The contract is
// docs/20260928-200621-capability-webtransport.md.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"go.uber.org/zap"
)

// wtHandshakeRate is the number of new handshakes per second per socket
// above which unvalidated sources get a Retry instead of a handshake
// reply: Caddy's own HTTP/3 listener uses the same figure.
const wtHandshakeRate = 1000

// wtRelay is the pooled relay. Sockets bind once and live until the
// address leaves the plan or the process ends; the table is replaced
// atomically at each generation's commit.
type wtRelay struct {
	logger *zap.Logger

	mu      sync.Mutex
	sockets map[netip.AddrPort]*wtSocket
	staged  *wtStaged // the generation that has started but not committed
	live    atomic.Pointer[wtTable]
	tlsConf atomic.Pointer[tls.Config]

	tickets *wtTicketSet

	// notify wakes the mDNS advertiser after the route set changes, so a
	// new relay host is announced (and a removed one withdrawn) at once.
	notify func()

	sessMu   sync.Mutex
	sessions map[*wtSession]struct{}
	sessionN atomic.Int64

	// Process-wide counters the routes do not own.
	refusedHost     atomic.Int64
	refusedPath     atomic.Int64
	refusedFronting atomic.Int64
	refusedMethod   atomic.Int64
	retriesSent     atomic.Int64
}

// wtSocket is one bound UDP socket with its QUIC transport and the
// servers that speak HTTP/3 and WebTransport over it.
type wtSocket struct {
	addr   netip.AddrPort
	pc     *net.UDPConn
	tr     *quic.Transport
	ln     *quic.EarlyListener
	h3     *http3.Server
	wt     *webtransport.Server
	cancel context.CancelFunc
	// stagedBy is the generation that bound the socket while it is not yet
	// committed; nil once live. An aborted generation closes only these.
	stagedBy any
	limiter  *wtRateLimiter
}

// wtTable is one committed configuration: the tuned limits and the routes,
// keyed by host and path.
type wtTable struct {
	maxSessions int
	maxDatagram int
	idleTimeout time.Duration
	routes      map[string]*wtRoute // key: host + path
	hosts       map[string]bool
	listen      []netip.AddrPort
}

// wtStaged is a generation's configuration between its Start and its
// commit.
type wtStaged struct {
	gen   any
	table *wtTable
	tls   *tls.Config
	bound []*wtSocket // sockets this generation bound; nothing else may close them before commit
}

// wtRouteConfig is a route's policy, published as an immutable value behind
// an atomic pointer so a reload can swap it while a CONNECT reads it without
// a lock (the counters below stay on the stable route object).
type wtRouteConfig struct {
	origin      []string
	maxSessions int
	gated       bool
}

// wtRoute is one relay route with its live counters. Counters survive a
// reload that keeps the route; a route removed and re-added starts over.
type wtRoute struct {
	host   string
	path   string
	target netip.AddrPort
	cfg    atomic.Pointer[wtRouteConfig]

	sessions atomic.Int64
	counters wtRouteCounters
}

func (rt *wtRoute) config() *wtRouteConfig { return rt.cfg.Load() }

// wtRouteCounters are the per-route monotonic counters on
// /1.0/webtransport.
type wtRouteCounters struct {
	accepted           atomic.Int64
	refusedOrigin      atomic.Int64
	refusedCap         atomic.Int64
	refusedUpstream    atomic.Int64
	refusedSettings    atomic.Int64
	refusedTicket      atomic.Int64
	datagramsIn        atomic.Int64
	datagramsOut       atomic.Int64
	bytesIn            atomic.Int64
	bytesOut           atomic.Int64
	droppedQueue       atomic.Int64
	droppedOversizeIn  atomic.Int64
	droppedOversizeOut atomic.Int64
	upstreamRefused    atomic.Int64
	streamsReset       atomic.Int64
	panics             atomic.Int64
}

func wtRouteKey(host, path string) string { return host + path }

func newWtRelay(logger *zap.Logger) *wtRelay {
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &wtRelay{
		logger:   logger,
		sockets:  map[netip.AddrPort]*wtSocket{},
		sessions: map[*wtSession]struct{}{},
		tickets:  newWtTicketSet(),
	}
	// An empty live table refuses everything until the first commit, and
	// a handshake before then has no certificate to present.
	r.live.Store(&wtTable{routes: map[string]*wtRoute{}, hosts: map[string]bool{}})
	return r
}

// stage records a generation's configuration and binds any address not
// yet bound. A bind failure closes the sockets bound for this generation
// and returns the error; nothing live changes. cfg nil stages a disable.
func (r *wtRelay) stage(gen any, table *wtTable, tlsConf *tls.Config) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staged != nil && r.staged.gen != gen {
		// A generation that started and was neither committed nor retired
		// is a Caddy sequencing error; refusing is louder than guessing.
		return fmt.Errorf("janus webtransport: another config generation is staged and not committed")
	}
	st := &wtStaged{gen: gen, table: table, tls: tlsConf}
	if table != nil {
		for _, addr := range table.listen {
			if _, ok := r.sockets[addr]; ok {
				continue
			}
			sock, err := r.bind(addr, table.idleTimeout)
			if err != nil {
				for _, s := range st.bound {
					s.close()
					delete(r.sockets, s.addr)
				}
				return err
			}
			sock.stagedBy = gen
			r.sockets[addr] = sock
			st.bound = append(st.bound, sock)
		}
	}
	r.staged = st
	return nil
}

// commit makes the staged generation live: routes swap atomically,
// sessions on routes no longer present close, sockets whose address left
// the plan close with their sessions, and the TLS config swaps. An
// identical configuration changes nothing observable.
func (r *wtRelay) commit(gen any) {
	r.mu.Lock()
	st := r.staged
	if st == nil || st.gen != gen {
		r.mu.Unlock()
		return
	}
	r.staged = nil
	old := r.live.Load()
	next := st.table
	if next == nil {
		next = &wtTable{routes: map[string]*wtRoute{}, hosts: map[string]bool{}}
	}
	// A kept route (same host+path+target) keeps its counters, its session
	// count, and its object identity — only its policy is re-published,
	// atomically, so a concurrent CONNECT never reads a half-written field.
	for key, route := range next.routes {
		if prev, ok := old.routes[key]; ok && prev.target == route.target {
			prev.cfg.Store(route.cfg.Load())
			next.routes[key] = prev
		}
	}
	for _, s := range st.bound {
		s.stagedBy = nil
	}
	var closing []*wtSocket
	for addr, s := range r.sockets {
		if !next.hasListen(addr) {
			closing = append(closing, s)
			delete(r.sockets, addr)
		}
	}
	if st.tls != nil {
		r.tlsConf.Store(st.tls)
	}
	r.live.Store(next)
	r.mu.Unlock()

	// Sessions on routes that left close from outside their goroutines by
	// cancelling; each session's own goroutine performs the close.
	r.sessMu.Lock()
	for sess := range r.sessions {
		// Survival is by object identity, not path: a kept route reuses its
		// object (above), so its sessions live; a route that was removed or
		// whose target changed gets a fresh object, so its sessions close
		// rather than keep relaying to a stale target.
		if next.routes[wtRouteKey(sess.route.host, sess.route.path)] != sess.route || !next.hasListen(sess.local) {
			sess.end(wtCloseNormal, "route removed")
		}
	}
	r.sessMu.Unlock()
	for _, s := range closing {
		s.close()
		r.logger.Info("janus webtransport listener closed", zap.String("addr", s.addr.String()))
	}
	r.notifyMdns()
}

// retire discards a generation's staging: the aborted reload's table is
// dropped and the sockets it alone bound are closed. Nothing of it went
// live, so nothing is restored.
func (r *wtRelay) retire(gen any) {
	r.mu.Lock()
	st := r.staged
	if st == nil || st.gen != gen {
		r.mu.Unlock()
		return
	}
	r.staged = nil
	for _, s := range st.bound {
		delete(r.sockets, s.addr)
	}
	r.mu.Unlock()
	for _, s := range st.bound {
		s.close()
	}
	if st.table != nil {
		r.logger.Warn("janus webtransport: discarded the configuration of an aborted reload",
			zap.Int("routes", len(st.table.routes)), zap.Int("sockets_closed", len(st.bound)))
	}
}

// closeAll ends every session and closes every socket: process shutdown.
func (r *wtRelay) closeAll() {
	r.sessMu.Lock()
	for sess := range r.sessions {
		sess.end(wtCloseNormal, "relay shutting down")
	}
	r.sessMu.Unlock()
	r.mu.Lock()
	socks := make([]*wtSocket, 0, len(r.sockets))
	for addr, s := range r.sockets {
		socks = append(socks, s)
		delete(r.sockets, addr)
	}
	r.staged = nil
	r.mu.Unlock()
	for _, s := range socks {
		s.close()
	}
}

func (t *wtTable) hasListen(addr netip.AddrPort) bool {
	for _, a := range t.listen {
		if a == addr {
			return true
		}
	}
	return false
}

// bind opens one UDP socket with a plain listen (no SO_REUSEPORT, so a
// second binder fails loudly) and starts its QUIC transport, HTTP/3
// server, WebTransport server, and accept loop. The TLS config the
// transport holds is fixed and delegates every handshake to the current
// committed config; quic-go wraps that hook itself and forces TLS 1.3.
func (r *wtRelay) bind(addr netip.AddrPort, idleTimeout time.Duration) (*wtSocket, error) {
	pc, err := net.ListenUDP(udpNetwork(addr.Addr()), net.UDPAddrFromAddrPort(addr))
	if err != nil {
		return nil, fmt.Errorf("janus webtransport: listen %s: %w", addr, err)
	}
	limiter := newWtRateLimiter(wtHandshakeRate)
	tr := &quic.Transport{
		Conn: pc,
		VerifySourceAddress: func(net.Addr) bool {
			if limiter.exceeded(time.Now()) {
				r.retriesSent.Add(1)
				return true
			}
			return false
		},
	}
	tlsConf := &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{http3.NextProtoH3},
		GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
			// quic-go holds one fixed config for the socket, but the
			// certificate a generation serves lives in the committed Caddy
			// config, swapped under the atomic pointer. Delegate to that
			// config's own hook (Caddy chooses its connection policy there),
			// and hand back a config that carries a real GetCertificate:
			// Caddy's top-level config exposes certificates only through its
			// hook, so returning it directly leaves quic-go with no cert and
			// the client sees an unrecognized-name alert.
			cur := r.tlsConf.Load()
			if cur == nil {
				return nil, errors.New("janus webtransport: no committed TLS configuration")
			}
			chosen := cur
			if cur.GetConfigForClient != nil {
				c, err := cur.GetConfigForClient(hello)
				if err != nil {
					return nil, err
				}
				if c != nil {
					chosen = c
				}
			}
			return chosen, nil
		},
	}
	quicConf := &quic.Config{
		EnableDatagrams:                  true,
		EnableStreamResetPartialDelivery: true,
		Allow0RTT:                        false,
	}
	if idleTimeout <= 0 {
		idleTimeout = wtDefaultIdleTimeout
	}
	quicConf.MaxIdleTimeout = idleTimeout
	quicConf.KeepAlivePeriod = idleTimeout / 4
	ln, err := tr.ListenEarly(tlsConf, quicConf)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("janus webtransport: quic listen %s: %w", addr, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var sock *wtSocket
	h3 := &http3.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { r.serveHTTP(sock, w, req) }), EnableDatagrams: true}
	webtransport.ConfigureHTTP3Server(h3)
	wt := &webtransport.Server{
		H3: h3,
		// Janus judges Origin before Upgrade, so the 403 and its counter
		// are Janus's; the library's check is out of the way.
		CheckOrigin: func(*http.Request) bool { return true },
	}
	sock = &wtSocket{addr: addr, pc: pc, tr: tr, ln: ln, h3: h3, wt: wt, cancel: cancel, limiter: limiter}
	go r.accept(ctx, sock)
	r.logger.Info("janus webtransport listening", zap.String("addr", addr.String()))
	return sock, nil
}

// accept hands each QUIC connection to the WebTransport server, which
// demultiplexes HTTP/3 requests from WebTransport streams and datagrams.
func (r *wtRelay) accept(ctx context.Context, sock *wtSocket) {
	for {
		conn, err := sock.ln.Accept(ctx)
		if err != nil {
			return
		}
		go func() {
			defer wtRecover(r.logger, "connection", nil)
			if err := sock.wt.ServeQUICConn(conn); err != nil && ctx.Err() == nil {
				r.logger.Debug("janus webtransport connection ended", zap.Error(err))
			}
		}()
	}
}

func (s *wtSocket) close() {
	s.cancel()
	_ = s.wt.Close()
	_ = s.ln.Close()
	_ = s.tr.Close()
	_ = s.pc.Close()
}

// udpNetwork picks the UDP network for an address: IPv4, dual-stack for a
// bare unspecified IPv6 (a listen wildcard), else IPv6. A relay target is
// never unspecified, so it resolves to udp4/udp6.
func udpNetwork(a netip.Addr) string {
	switch {
	case a.Is4():
		return "udp4"
	case a.IsUnspecified():
		return "udp"
	default:
		return "udp6"
	}
}

// wtRateLimiter counts events in the current second; exceeded reports
// whether this second's count passed the limit, so the transport can
// demand address validation only under a handshake flood.
type wtRateLimiter struct {
	limit  int64
	mu     sync.Mutex
	second int64
	count  int64
}

func newWtRateLimiter(limit int64) *wtRateLimiter { return &wtRateLimiter{limit: limit} }

func (l *wtRateLimiter) exceeded(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	sec := now.Unix()
	if sec != l.second {
		l.second, l.count = sec, 0
	}
	l.count++
	return l.count > l.limit
}

// wtRecover turns a panic in a relay goroutine into a logged, counted
// failure of that session alone (or of one connection, before a session
// exists).
func wtRecover(logger *zap.Logger, where string, sess *wtSession) {
	if p := recover(); p != nil {
		logger.Error("janus webtransport: recovered panic", zap.String("where", where), zap.Any("panic", p), zap.Stack("stack"))
		if sess != nil {
			// Count the death once even if several of the session's goroutines
			// panic on the way down.
			if sess.panicked.CompareAndSwap(false, true) {
				sess.route.counters.panics.Add(1)
			}
			sess.end(wtCloseInternal, "internal error")
		}
	}
}

// newTicketKey draws the per-process HMAC key for tickets.
func newTicketKey() [32]byte {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic("janus webtransport: crypto/rand failed: " + err.Error())
	}
	return k
}

// wtMdnsApp labels a relay-route host in the mDNS snapshot, so the
// dashboard shows why a name that is not a registered app is advertised.
const wtMdnsApp = "webtransport"

// hostList returns the committed relay routes' hosts, unique and sorted:
// the names the mDNS advertiser announces so a browser can resolve the
// relay. Empty when the relay is disabled.
func (r *wtRelay) hostList() []string {
	table := r.live.Load()
	if table == nil || len(table.hosts) == 0 {
		return nil
	}
	out := make([]string, 0, len(table.hosts))
	for h := range table.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// notifyMdns wakes the advertiser after a route-set change, best effort.
func (r *wtRelay) notifyMdns() {
	if r.notify != nil {
		r.notify()
	}
}

// wtHashMaxValidity is the longest leaf validity a browser accepts for
// WebTransport serverCertificateHashes (draft: 14 days). A short-lived
// internal leaf qualifies; a public 90-day leaf does not (and does not need
// to — it is CA-trusted and CT-compliant).
const wtHashMaxValidity = 14 * 24 * time.Hour

// leafHash returns the base64 SHA-256 of the DER leaf the relay would
// present for host, but only when that leaf is short enough to be pinned
// with serverCertificateHashes. It returns ("", nil) when the leaf does
// not qualify (e.g. a public CA cert), so the descriptor falls back to
// plain CA validation. Chrome enforces Certificate Transparency on the
// WebTransport QUIC handshake, which a local CA cannot satisfy, so pinning
// the leaf hash is how a local-CA relay is reached from a browser.
func (r *wtRelay) leafHash(host string) (string, error) {
	cfg := r.tlsConf.Load()
	if cfg == nil {
		return "", errors.New("no committed TLS configuration")
	}
	hello := &tls.ClientHelloInfo{
		ServerName:        host,
		SupportedProtos:   []string{http3.NextProtoH3},
		SupportedVersions: []uint16{tls.VersionTLS13},
		CipherSuites:      []uint16{tls.TLS_AES_128_GCM_SHA256, tls.TLS_CHACHA20_POLY1305_SHA256},
	}
	chosen := cfg
	if cfg.GetConfigForClient != nil {
		c, err := cfg.GetConfigForClient(hello)
		if err != nil {
			return "", err
		}
		if c != nil {
			chosen = c
		}
	}
	if chosen.GetCertificate == nil {
		return "", errors.New("TLS config has no certificate source")
	}
	cert, err := chosen.GetCertificate(hello)
	if err != nil {
		return "", err
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return "", errors.New("no certificate for " + host)
	}
	leaf := cert.Leaf
	if leaf == nil {
		leaf, err = x509.ParseCertificate(cert.Certificate[0])
		if err != nil {
			return "", err
		}
	}
	if time.Until(leaf.NotAfter) > wtHashMaxValidity {
		return "", nil // too long-lived to pin; CA validation is the path
	}
	// serverCertificateHashes requires an ECDSA P-256 leaf. A non-conforming
	// leaf would be published as a hash the browser rejects, so fall back to
	// CA validation instead.
	if pub, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok || pub.Curve != elliptic.P256() {
		return "", nil
	}
	sum := sha256.Sum256(cert.Certificate[0])
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}
