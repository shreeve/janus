package janus

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"
	"go.uber.org/zap"
)

const wtTestHost = "relay.test"

// wtTestCert is a self-signed leaf for relay.test and the pool that
// trusts it.
func wtTestCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: wtTestHost},
		DNSNames: []string{wtTestHost}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool
}

// udpEcho answers every datagram with its own bytes and reports the peer
// it last heard from.
type udpEcho struct {
	conn  *net.UDPConn
	peers chan netip.AddrPort
	count atomic.Int64
}

func newUDPEcho(t *testing.T) *udpEcho {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	e := &udpEcho{conn: conn, peers: make(chan netip.AddrPort, 4096)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			e.count.Add(1)
			select {
			case e.peers <- addr:
			default:
			}
			_, _ = conn.WriteToUDPAddrPort(buf[:n], addr)
		}
	}()
	t.Cleanup(func() { conn.Close() })
	return e
}

func (e *udpEcho) addr() netip.AddrPort { return e.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

// freeUDPPort reserves and releases an ephemeral port.
func freeUDPPort(t *testing.T) uint16 {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	p := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return uint16(p)
}

type wtFixture struct {
	relay  *wtRelay
	listen netip.AddrPort
	table  *wtTable
	echo   *udpEcho
	pool   *x509.CertPool
	gen    *App
}

// newWtFixture stages and commits a relay with one route at /r toward
// the echo, bound on a loopback port.
func newWtFixture(t *testing.T, route func(*wtRouteConfig), table func(*wtTable)) *wtFixture {
	t.Helper()
	cert, pool := wtTestCert(t)
	echo := newUDPEcho(t)
	relay := newWtRelay(zap.NewNop())
	listen := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freeUDPPort(t))
	rt := &wtRoute{host: wtTestHost, path: "/r", target: echo.addr()}
	rc := &wtRouteConfig{maxSessions: 4}
	if route != nil {
		route(rc)
	}
	rt.cfg.Store(rc)
	tb := &wtTable{
		maxSessions: 256, maxDatagram: 1200, idleTimeout: 30 * time.Second,
		routes: map[string]*wtRoute{wtRouteKey(rt.host, rt.path): rt}, hosts: map[string]bool{rt.host: true},
		listen: []netip.AddrPort{listen},
	}
	if table != nil {
		table(tb)
	}
	tlsConf := http3.ConfigureTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	gen := &App{}
	if err := relay.stage(gen, tb, tlsConf); err != nil {
		t.Fatal(err)
	}
	relay.commit(gen)
	t.Cleanup(relay.closeAll)
	return &wtFixture{relay: relay, listen: listen, table: tb, echo: echo, pool: pool, gen: gen}
}

func (f *wtFixture) route() *wtRoute { return f.table.routes[wtRouteKey(wtTestHost, "/r")] }

// dial opens a session to /r with the given header; the server's path MTU
// discovery is off in these tests, so what passes is the first-flight
// budget.
func (f *wtFixture) dial(t *testing.T, path string, hdr http.Header) (*http.Response, *webtransport.Session, error) {
	t.Helper()
	d := &webtransport.Dialer{
		TLSClientConfig: &tls.Config{RootCAs: f.pool, ServerName: wtTestHost, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{EnableDatagrams: true, EnableStreamResetPartialDelivery: true, DisablePathMTUDiscovery: true},
		DialAddr: func(ctx context.Context, _ string, tlsCfg *tls.Config, cfg *quic.Config) (*quic.Conn, error) {
			return quic.DialAddrEarly(ctx, f.listen.String(), tlsCfg, cfg)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, sess, err := d.Dial(ctx, "https://"+wtTestHost+path, hdr)
	if sess != nil {
		t.Cleanup(func() { _ = sess.CloseWithError(0, "") })
	}
	return res, sess, err
}

func sameOrigin() http.Header { return http.Header{"Origin": []string{"https://" + wtTestHost}} }

func recvWithin(t *testing.T, sess *webtransport.Session, d time.Duration) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	b, err := sess.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	return b
}

func wtWaitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWebtransportRelaysByteExactBothWays(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	res, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d", res.StatusCode)
	}
	for _, size := range []int{1, 64, 1152, 1200} {
		payload := make([]byte, size)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		if err := sess.SendDatagram(payload); err != nil {
			t.Fatalf("send %d: %v", size, err)
		}
		got := recvWithin(t, sess, 2*time.Second)
		if string(got) != string(payload) {
			t.Fatalf("size %d: echo differs (%d bytes back)", size, len(got))
		}
	}
	rt := f.route()
	wtWaitFor(t, "counters", func() bool { return rt.counters.datagramsIn.Load() == 4 && rt.counters.datagramsOut.Load() == 4 })
	if rt.counters.bytesIn.Load() != 1+64+1152+1200 || rt.counters.bytesOut.Load() != 1+64+1152+1200 {
		t.Fatalf("bytes: in %d out %d", rt.counters.bytesIn.Load(), rt.counters.bytesOut.Load())
	}
	if rt.counters.accepted.Load() != 1 || rt.sessions.Load() != 1 || f.relay.sessionN.Load() != 1 {
		t.Fatalf("accounting: accepted %d route %d process %d", rt.counters.accepted.Load(), rt.sessions.Load(), f.relay.sessionN.Load())
	}
}

func TestWebtransportPreservesBoundaries(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	// Sizes cycle through the range; each must come back as one datagram
	// of the same size, in order (loopback does not reorder).
	sizes := []int{7, 300, 1152, 33, 900, 1, 1200, 512}
	for i := 0; i < 1000; i++ {
		size := sizes[i%len(sizes)]
		p := make([]byte, size)
		p[0] = byte(i)
		if size > 1 {
			p[size-1] = byte(i >> 8)
		}
		if err := sess.SendDatagram(p); err != nil {
			t.Fatal(err)
		}
		got := recvWithin(t, sess, 2*time.Second)
		if len(got) != size || got[0] != byte(i) {
			t.Fatalf("datagram %d: sent %d bytes, got %d (first byte %d)", i, size, len(got), got[0])
		}
	}
}

func TestWebtransportOneConnectedSocketPerSession(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	var source netip.AddrPort
	for i := 0; i < 20; i++ {
		if err := sess.SendDatagram([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
		recvWithin(t, sess, 2*time.Second)
		peer := <-f.echo.peers
		if i == 0 {
			source = peer
		} else if peer != source {
			t.Fatalf("source tuple changed mid-session: %s then %s", source, peer)
		}
	}
	var s *wtSession
	f.relay.sessMu.Lock()
	for x := range f.relay.sessions {
		s = x
	}
	f.relay.sessMu.Unlock()
	if s == nil || s.udp.LocalAddr().(*net.UDPAddr).AddrPort() != source {
		t.Fatalf("the session's socket is not the echo's peer")
	}
	// A second session has its own socket with another port.
	_, sess2, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess2.SendDatagram([]byte{9}); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, sess2, 2*time.Second)
	if peer := <-f.echo.peers; peer == source {
		t.Fatal("two sessions shared a source port")
	}
	// Closing the session closes its socket at once.
	_ = sess.CloseWithError(0, "bye")
	wtWaitFor(t, "socket closed", func() bool {
		_, err := s.udp.Write([]byte{0})
		return errors.Is(err, net.ErrClosed)
	})
	wtWaitFor(t, "session gone", func() bool { return f.route().sessions.Load() == 1 })
}

func TestWebtransportRefusesOrigin(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	for _, hdr := range []http.Header{nil, {"Origin": []string{"https://evil.test"}}, {"Origin": []string{"null"}}} {
		res, _, err := f.dial(t, "/r", hdr)
		if err == nil || res == nil || res.StatusCode != http.StatusForbidden {
			t.Fatalf("Origin %v: want 403 refusal, got %v %v", hdr, res, err)
		}
	}
	if got := f.route().counters.refusedOrigin.Load(); got != 3 {
		t.Fatalf("refused_origin = %d", got)
	}
	// A named policy admits the named host and still refuses a stranger.
	f2 := newWtFixture(t, func(rc *wtRouteConfig) { rc.origin = []string{"viewer.test"} }, nil)
	if res, _, err := f2.dial(t, "/r", http.Header{"Origin": []string{"https://viewer.test"}}); err != nil || res.StatusCode != 200 {
		t.Fatalf("named origin: %v %v", res, err)
	}
	if res, _, err := f2.dial(t, "/r", sameOrigin()); err == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("same host under a named-only policy: want 403, got %v %v", res, err)
	}
}

func TestWebtransportRefusesUnknownHostAndPath(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	// A known host, unknown path: 404, counted as a path miss (not a host miss).
	if res, _, err := f.dial(t, "/nope", sameOrigin()); err == nil || res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path: want 404, got %v %v", res, err)
	}
	if f.relay.refusedPath.Load() != 1 || f.relay.refusedHost.Load() != 0 {
		t.Fatalf("path miss: refused_path=%d refused_host=%d", f.relay.refusedPath.Load(), f.relay.refusedHost.Load())
	}
}

func TestWebtransportSessionCaps(t *testing.T) {
	f := newWtFixture(t, func(rc *wtRouteConfig) { rc.maxSessions = 1 }, nil)
	if _, _, err := f.dial(t, "/r", sameOrigin()); err != nil {
		t.Fatal(err)
	}
	res, _, err := f.dial(t, "/r", sameOrigin())
	if err == nil || res.StatusCode != http.StatusServiceUnavailable || res.Header.Get("Retry-After") != "1" {
		t.Fatalf("at cap: want 503 Retry-After 1, got %v %v", res, err)
	}
	if f.route().counters.refusedCap.Load() != 1 {
		t.Fatal("refused_cap not counted")
	}
	// The process cap refuses too, and a refusal releases what it reserved.
	f2 := newWtFixture(t, nil, func(tb *wtTable) { tb.maxSessions = 1 })
	if _, _, err := f2.dial(t, "/r", sameOrigin()); err != nil {
		t.Fatal(err)
	}
	if res, _, err := f2.dial(t, "/r", sameOrigin()); err == nil || res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("process cap: want 503, got %v %v", res, err)
	}
	if f2.route().sessions.Load() != 1 || f2.relay.sessionN.Load() != 1 {
		t.Fatalf("a refusal leaked a slot: route %d process %d", f2.route().sessions.Load(), f2.relay.sessionN.Load())
	}
}

func TestWebtransportConcurrentDialsAtCapAdmitOne(t *testing.T) {
	f := newWtFixture(t, func(rc *wtRouteConfig) { rc.maxSessions = 1 }, nil)
	results := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, _, err := f.dial(t, "/r", sameOrigin())
			if err != nil && res == nil {
				results <- -1
				return
			}
			results <- res.StatusCode
		}()
	}
	a, b := <-results, <-results
	if !((a == 200 && b == 503) || (a == 503 && b == 200)) {
		t.Fatalf("want exactly one admitted, got %d and %d", a, b)
	}
}

func TestWebtransportRefusedTargetKeepsSession(t *testing.T) {
	// A target with nothing listening: loopback answers with ICMP port
	// unreachable, which surfaces as ECONNREFUSED on the connected socket.
	// Closing the echo leaves its address dead.
	f := newWtFixture(t, nil, nil)
	f.echo.conn.Close()
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := sess.SendDatagram([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	wtWaitFor(t, "upstream_refused", func() bool { return f.route().counters.upstreamRefused.Load() >= 1 })
	if sess.Context().Err() != nil {
		t.Fatal("the session closed on a refused target")
	}
	if f.route().sessions.Load() != 1 {
		t.Fatal("session count dropped")
	}
}

func TestWebtransportOversizeFromTargetIsDroppedAndCounted(t *testing.T) {
	f := newWtFixture(t, nil, func(tb *wtTable) { tb.maxDatagram = 100 })
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	// Learn the session's socket, then have the echo send an oversize
	// datagram straight to it.
	if err := sess.SendDatagram(make([]byte, 50)); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, sess, 2*time.Second)
	peer := <-f.echo.peers
	if _, err := f.echo.conn.WriteToUDPAddrPort(make([]byte, 101), peer); err != nil {
		t.Fatal(err)
	}
	wtWaitFor(t, "dropped_oversize_out", func() bool { return f.route().counters.droppedOversizeOut.Load() == 1 })
	// And a browser datagram above the ceiling is dropped, not relayed.
	before := f.echo.count.Load()
	if err := sess.SendDatagram(make([]byte, 101)); err != nil {
		t.Fatal(err)
	}
	wtWaitFor(t, "dropped_oversize_in", func() bool { return f.route().counters.droppedOversizeIn.Load() == 1 })
	if f.echo.count.Load() != before {
		t.Fatal("an oversize browser datagram reached the target")
	}
}

func TestWebtransportResetsIncomingStreams(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	str, err := sess.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = str.Write([]byte("stream"))
	wtWaitFor(t, "streams_reset", func() bool { return f.route().counters.streamsReset.Load() >= 1 })
	if _, err := str.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream was not reset")
	}
}

func TestWebtransportPanicClosesOneSession(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, victim, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	_, bystander, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	// Mark the bystander by traffic so the victim is the session with none.
	if err := bystander.SendDatagram([]byte("mark")); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, bystander, 2*time.Second)
	var target *wtSession
	f.relay.sessMu.Lock()
	for s := range f.relay.sessions {
		if s.dgramsIn.Load() == 0 && target == nil {
			target = s
		}
	}
	f.relay.sessMu.Unlock()
	// Fault injection: a goroutine of the session panics under the
	// relay's recover.
	go func() {
		defer wtRecover(f.relay.logger, "test", target)
		panic("boom")
	}()
	wtWaitFor(t, "victim closed", func() bool { return f.route().counters.panics.Load() == 1 && f.route().sessions.Load() == 1 })
	if err := bystander.SendDatagram([]byte("still here")); err != nil {
		t.Fatal(err)
	}
	if got := recvWithin(t, bystander, 2*time.Second); string(got) != "still here" {
		t.Fatal("bystander session stopped relaying")
	}
	_ = victim
}

func TestWebtransportTicketGate(t *testing.T) {
	f := newWtFixture(t, func(rc *wtRouteConfig) { rc.gated = true }, nil)
	key := wtRouteKey(wtTestHost, "/r")
	if res, _, err := f.dial(t, "/r", sameOrigin()); err == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("no ticket: want 403, got %v %v", res, err)
	}
	if res, _, err := f.dial(t, "/r?t=forged", sameOrigin()); err == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("forged ticket: want 403, got %v %v", res, err)
	}
	other := f.relay.tickets.mint(wtRouteKey(wtTestHost, "/other"), "steve")
	if res, _, err := f.dial(t, "/r?t="+other, sameOrigin()); err == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("another route's ticket: want 403, got %v %v", res, err)
	}
	good := f.relay.tickets.mint(key, "steve")
	res, _, err := f.dial(t, "/r?t="+good, sameOrigin())
	if err != nil || res.StatusCode != http.StatusOK {
		t.Fatalf("valid ticket: %v %v", res, err)
	}
	if res, _, err := f.dial(t, "/r?t="+good, sameOrigin()); err == nil || res.StatusCode != http.StatusForbidden {
		t.Fatalf("replayed ticket: want 403, got %v %v", res, err)
	}
	if f.route().counters.refusedTicket.Load() != 4 {
		t.Fatalf("refused_ticket = %d", f.route().counters.refusedTicket.Load())
	}
	var user string
	f.relay.sessMu.Lock()
	for s := range f.relay.sessions {
		user = s.user
	}
	f.relay.sessMu.Unlock()
	if user != "steve" {
		t.Fatalf("session user = %q", user)
	}
}

func TestWebtransportTicketExpiry(t *testing.T) {
	set := newWtTicketSet()
	now := time.Unix(1_800_000_000, 0)
	set.now = func() time.Time { return now }
	tk := set.mint("h/p", "u")
	tk2 := set.mint("h/p", "u")
	now = now.Add(wtTicketTTL - time.Second)
	if u, ok := set.redeem(tk, "h/p"); !ok || u != "u" {
		t.Fatal("a ticket inside its lifetime must redeem")
	}
	now = now.Add(2 * time.Second)
	if _, ok := set.redeem(tk2, "h/p"); ok {
		t.Fatal("an expired ticket redeemed")
	}
	if _, ok := set.redeem(strings.Repeat("A", 80), "h/p"); ok {
		t.Fatal("garbage redeemed")
	}
	if _, ok := set.redeem("", "h/p"); ok {
		t.Fatal("empty ticket redeemed")
	}
}

func TestWebtransportRingDropsOldest(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var drops int
	r := newWtRing(4, 50*time.Millisecond, func() time.Time { return now }, func() { drops++ })
	mk := func(n int) wtItem {
		b := make([]byte, wtReadBuffer)
		b[0] = byte(n)
		return wtItem{buf: &b, n: 1, at: now}
	}
	for i := 1; i <= 6; i++ {
		r.push(mk(i))
	}
	if drops != 2 {
		t.Fatalf("drops = %d, want 2 (oldest two)", drops)
	}
	item, ok := r.pop()
	if !ok || (*item.buf)[0] != 3 {
		t.Fatalf("first pop = %v %v, want entry 3", ok, (*item.buf)[0])
	}
	// Age: entries older than 50 ms are dropped at the front.
	now = now.Add(60 * time.Millisecond)
	r.push(mk(7)) // fresh
	item, ok = r.pop()
	if !ok || (*item.buf)[0] != 7 || drops != 5 {
		t.Fatalf("aged pop = entry %d drops %d, want entry 7 and 5 drops", (*item.buf)[0], drops)
	}
	r.close()
	if _, ok := r.pop(); ok {
		t.Fatal("pop after close must report closed")
	}
}

func TestWebtransportReloadSemantics(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	sockBefore := f.relay.sockets[f.listen]
	// Identical reload: same table contents, staged and committed by a new
	// generation; the socket and the session are untouched.
	gen2 := &App{}
	same := *f.table
	same.routes = map[string]*wtRoute{}
	for k, v := range f.table.routes {
		cp := &wtRoute{host: v.host, path: v.path, target: v.target}
		cp.cfg.Store(v.config())
		same.routes[k] = cp
	}
	if err := f.relay.stage(gen2, &same, f.relay.tlsConf.Load()); err != nil {
		t.Fatal(err)
	}
	if f.relay.live.Load().routes[wtRouteKey(wtTestHost, "/r")].counters.accepted.Load() != 1 {
		t.Fatal("staging must not touch the live table")
	}
	f.relay.commit(gen2)
	if f.relay.sockets[f.listen] != sockBefore {
		t.Fatal("identical reload rebound the socket")
	}
	if err := sess.SendDatagram([]byte("x")); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, sess, 2*time.Second)
	if f.relay.live.Load().routes[wtRouteKey(wtTestHost, "/r")].counters.accepted.Load() != 1 {
		t.Fatal("a kept route lost its counters")
	}
	// Route removal: staged, the session lives; committed, it closes.
	gen3 := &App{}
	empty := *f.table
	empty.routes = map[string]*wtRoute{}
	empty.hosts = map[string]bool{}
	if err := f.relay.stage(gen3, &empty, nil); err != nil {
		t.Fatal(err)
	}
	if err := sess.SendDatagram([]byte("y")); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, sess, 2*time.Second)
	f.relay.commit(gen3)
	wtWaitFor(t, "session closed at commit", func() bool { return sess.Context().Err() != nil })
	if f.relay.sockets[f.listen] != sockBefore {
		t.Fatal("removing a route must keep the socket")
	}
	// Aborted reload: a new address bound for a generation that retires
	// is closed; the live socket stays.
	gen4 := &App{}
	extra := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freeUDPPort(t))
	wider := empty
	wider.listen = []netip.AddrPort{f.listen, extra}
	if err := f.relay.stage(gen4, &wider, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.relay.sockets[extra]; !ok {
		t.Fatal("staging must bind the new address")
	}
	f.relay.retire(gen4)
	if _, ok := f.relay.sockets[extra]; ok {
		t.Fatal("retire must close the aborted generation's socket")
	}
	if f.relay.sockets[f.listen] != sockBefore {
		t.Fatal("retire must not touch the live socket")
	}
	// Disable: everything closes at commit.
	gen5 := &App{}
	if err := f.relay.stage(gen5, nil, nil); err != nil {
		t.Fatal(err)
	}
	f.relay.commit(gen5)
	if len(f.relay.sockets) != 0 {
		t.Fatal("disable must close the sockets")
	}
}

func TestWebtransportBindFailureIsLoud(t *testing.T) {
	taken, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	relay := newWtRelay(zap.NewNop())
	addr := taken.LocalAddr().(*net.UDPAddr).AddrPort()
	tb := &wtTable{routes: map[string]*wtRoute{}, hosts: map[string]bool{}, listen: []netip.AddrPort{addr}}
	err = relay.stage(&App{}, tb, &tls.Config{})
	if err == nil || !strings.Contains(err.Error(), addr.String()) {
		t.Fatalf("want a bind error naming %s, got %v", addr, err)
	}
	if len(relay.sockets) != 0 || relay.staged != nil {
		t.Fatal("a failed stage must leave nothing behind")
	}
}

func TestWebtransportStatusShape(t *testing.T) {
	f := newWtFixture(t, func(rc *wtRouteConfig) { rc.gated = true }, nil)
	st := f.relay.status()
	if !st.Enabled || len(st.Listen) != 1 || st.MaxDatagram != 1200 || st.IdleTimeout != "30s" {
		t.Fatalf("status: %+v", st)
	}
	rs, ok := st.Routes[wtRouteKey(wtTestHost, "/r")]
	if !ok || rs.Target != "udp/"+f.echo.addr().String() || !rs.Gated || rs.MaxSessions != 4 || len(rs.Origin) != 1 || rs.Origin[0] != "same" {
		t.Fatalf("route status: %+v", rs)
	}
}

// TestWebtransportTLSHookCallsThrough pins the shape Caddy hands the relay:
// a config with no certificates of its own whose GetConfigForClient picks
// the one that has them. Go consults that hook only on the top-level
// config, so the relay's delegating hook must call through.
func TestWebtransportTLSHookCallsThrough(t *testing.T) {
	cert, pool := wtTestCert(t)
	echo := newUDPEcho(t)
	relay := newWtRelay(zap.NewNop())
	listen := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), freeUDPPort(t))
	rt := &wtRoute{host: wtTestHost, path: "/r", target: echo.addr()}
	rt.cfg.Store(&wtRouteConfig{maxSessions: 4})
	tb := &wtTable{maxSessions: 4, maxDatagram: 1200, idleTimeout: 30 * time.Second,
		routes: map[string]*wtRoute{wtRouteKey(rt.host, rt.path): rt}, hosts: map[string]bool{rt.host: true}, listen: []netip.AddrPort{listen}}
	withCert := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{http3.NextProtoH3}}
	caddyShaped := http3.ConfigureTLSConfig(&tls.Config{
		MinVersion:         tls.VersionTLS13,
		GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) { return withCert, nil },
	})
	gen := &App{}
	if err := relay.stage(gen, tb, caddyShaped); err != nil {
		t.Fatal(err)
	}
	relay.commit(gen)
	t.Cleanup(relay.closeAll)
	f := &wtFixture{relay: relay, listen: listen, table: tb, echo: echo, pool: pool, gen: gen}
	res, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("dial through a Caddy-shaped config: %v %v", res, err)
	}
	if err := sess.SendDatagram([]byte("cert via hook")); err != nil {
		t.Fatal(err)
	}
	if got := recvWithin(t, sess, 2*time.Second); string(got) != "cert via hook" {
		t.Fatalf("echo %q", got)
	}
}

// TestWebtransportLeafHash pins that the relay publishes a pinnable hash for
// a short-lived leaf and declines for a long-lived one (a public cert that
// should be validated by CA instead).
func TestWebtransportLeafHash(t *testing.T) {
	mk := func(validity time.Duration) *tls.Config {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: wtTestHost}, DNSNames: []string{wtTestHost},
			NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(validity),
			KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		leaf, _ := x509.ParseCertificate(der)
		cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
		return &tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }}
	}

	r := newWtRelay(zap.NewNop())
	if _, err := r.leafHash(wtTestHost); err == nil {
		t.Fatal("no committed TLS config must be an error")
	}
	// Short-lived leaf: a hash is published, and it is the base64 SHA-256 of
	// the DER the config presents.
	short := mk(12 * time.Hour)
	r.tlsConf.Store(short)
	got, err := r.leafHash(wtTestHost)
	if err != nil || got == "" {
		t.Fatalf("short-lived leaf: want a hash, got %q %v", got, err)
	}
	cert, _ := short.GetCertificate(nil)
	sum := sha256.Sum256(cert.Certificate[0])
	if got != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("hash is not the base64 SHA-256 of the DER leaf")
	}
	// Long-lived leaf (public-cert shape): no hash — validate by CA.
	r.tlsConf.Store(mk(90 * 24 * time.Hour))
	if got, err := r.leafHash(wtTestHost); err != nil || got != "" {
		t.Fatalf("long-lived leaf: want no hash, got %q %v", got, err)
	}
}

// wtConnect builds a CONNECT/webtransport request with the given SNI, Host,
// path and Origin, for driving serveHTTP's admission directly.
func wtConnect(host, path, sni, origin string) *http.Request {
	req := httptest.NewRequest(http.MethodConnect, "https://"+host+path, nil)
	req.Method = http.MethodConnect
	req.Proto = "webtransport"
	req.Host = host
	req.TLS = &tls.ConnectionState{ServerName: sni}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	return req
}

// wtRefusalRelay builds a relay whose live table has one route on host, with
// no bound sockets — enough to exercise serveHTTP's refusal rows (which read
// only r.live and the counters, never the socket).
func wtRefusalRelay(host, path string) *wtRelay {
	r := newWtRelay(zap.NewNop())
	rt := &wtRoute{host: host, path: path, target: netip.MustParseAddrPort("127.0.0.1:41151")}
	rt.cfg.Store(&wtRouteConfig{maxSessions: 4})
	r.live.Store(&wtTable{
		maxSessions: 256, maxDatagram: 1200,
		routes: map[string]*wtRoute{wtRouteKey(host, path): rt}, hosts: map[string]bool{host: true},
	})
	return r
}

func TestWebtransportAdmissionRefusals(t *testing.T) {
	r := wtRefusalRelay(wtTestHost, "/r")

	// Non-CONNECT method → 405, counted.
	rec := httptest.NewRecorder()
	get := httptest.NewRequest(http.MethodGet, "https://"+wtTestHost+"/r", nil)
	get.TLS = &tls.ConnectionState{ServerName: wtTestHost}
	r.serveHTTP(nil, rec, get)
	if rec.Code != http.StatusMethodNotAllowed || r.refusedMethod.Load() != 1 {
		t.Fatalf("method: code=%d refused_method=%d", rec.Code, r.refusedMethod.Load())
	}

	// Mismatched SNI → 421, refused_fronting.
	rec = httptest.NewRecorder()
	r.serveHTTP(nil, rec, wtConnect(wtTestHost, "/r", "other.test", "https://"+wtTestHost))
	if rec.Code != http.StatusMisdirectedRequest || r.refusedFronting.Load() != 1 {
		t.Fatalf("mismatched SNI: code=%d refused_fronting=%d", rec.Code, r.refusedFronting.Load())
	}

	// Empty SNI (with TLS present) also fails closed → 421.
	rec = httptest.NewRecorder()
	r.serveHTTP(nil, rec, wtConnect(wtTestHost, "/r", "", "https://"+wtTestHost))
	if rec.Code != http.StatusMisdirectedRequest || r.refusedFronting.Load() != 2 {
		t.Fatalf("empty SNI: code=%d refused_fronting=%d", rec.Code, r.refusedFronting.Load())
	}

	// Unknown host → 404, refused_host (distinct from path).
	rec = httptest.NewRecorder()
	r.serveHTTP(nil, rec, wtConnect("nope.test", "/r", "nope.test", "https://nope.test"))
	if rec.Code != http.StatusNotFound || r.refusedHost.Load() != 1 {
		t.Fatalf("unknown host: code=%d refused_host=%d", rec.Code, r.refusedHost.Load())
	}
}

func TestWebtransportWanLocalRefused(t *testing.T) {
	SetExposure("wan", "en0", netip.MustParseAddr("203.0.113.7"))
	t.Cleanup(func() { SetExposure("", "", netip.Addr{}) })
	r := wtRefusalRelay("lyte.local", "/r")
	rec := httptest.NewRecorder()
	r.serveHTTP(nil, rec, wtConnect("lyte.local", "/r", "lyte.local", "https://lyte.local"))
	if rec.Code != http.StatusMisdirectedRequest {
		t.Fatalf("wan + .local host: want 421, got %d", rec.Code)
	}
}

func TestWebtransportTicketPerRouteCap(t *testing.T) {
	set := newWtTicketSet()
	set.maxLive = 2
	// Fill route A's used-set to its cap by redeeming A's tickets.
	for i := 0; i < 2; i++ {
		tk := set.mint("host/a", "u")
		if tk == "" {
			t.Fatalf("mint A #%d unexpectedly refused", i)
		}
		if _, ok := set.redeem(tk, "host/a"); !ok {
			t.Fatalf("redeem A #%d failed", i)
		}
	}
	// A is now full → mint on A refuses, but a different route B is unaffected.
	if set.mint("host/a", "u") != "" {
		t.Fatal("route A over cap should refuse minting")
	}
	if set.mint("host/b", "u") == "" {
		t.Fatal("route B must still mint while A is full (per-route scoping)")
	}
}

func TestWebtransportLeafHashRejectsNonP256(t *testing.T) {
	// An RSA leaf (short-lived) must NOT be published as a hash — it fails the
	// P-256 gate, so the descriptor falls back to CA validation.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: wtTestHost}, DNSNames: []string{wtTestHost},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(12 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	leaf, _ := x509.ParseCertificate(der)
	cert := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
	r := newWtRelay(zap.NewNop())
	r.tlsConf.Store(&tls.Config{GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return cert, nil }})
	if got, err := r.leafHash(wtTestHost); err != nil || got != "" {
		t.Fatalf("RSA leaf: want no hash, got %q %v", got, err)
	}
}

// TestWebtransportTargetChangeClosesSession pins that a reload which changes a
// route's target (same host+path) closes the sessions bound to the old target
// rather than leaving them relaying to it (correctness fix: survival is by
// object identity, not path).
func TestWebtransportTargetChangeClosesSession(t *testing.T) {
	f := newWtFixture(t, nil, nil)
	_, sess, err := f.dial(t, "/r", sameOrigin())
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.SendDatagram([]byte("x")); err != nil {
		t.Fatal(err)
	}
	recvWithin(t, sess, 2*time.Second)

	// A new generation keeps /r but points it at a different target.
	echo2 := newUDPEcho(t)
	gen2 := &App{}
	changed := *f.table
	changed.routes = map[string]*wtRoute{}
	rt := &wtRoute{host: wtTestHost, path: "/r", target: echo2.addr()}
	rt.cfg.Store(&wtRouteConfig{maxSessions: 4})
	changed.routes[wtRouteKey(wtTestHost, "/r")] = rt
	if err := f.relay.stage(gen2, &changed, f.relay.tlsConf.Load()); err != nil {
		t.Fatal(err)
	}
	f.relay.commit(gen2)
	wtWaitFor(t, "session closed on target change", func() bool { return sess.Context().Err() != nil })
}
