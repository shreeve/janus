package janus

// One WebTransport session is one connected UDP socket toward the route's
// target and one goroutine pair per direction. Browser → target writes
// each datagram at once; target → browser goes through a bounded ring that
// drops the oldest rather than queue. Every end closes the UDP socket
// first, then the session from its own goroutine.

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/webtransport-go"
	"go.uber.org/zap"
)

// Session close codes, sent as the WebTransport close-session capsule.
const (
	wtCloseNormal   webtransport.SessionErrorCode = 0 // relay-initiated: shutdown or route removal
	wtCloseInternal webtransport.SessionErrorCode = 1 // a panic in this session's goroutines
	wtCloseUpstream webtransport.SessionErrorCode = 2 // the UDP socket failed after admission
)

// wtStreamUnsupported resets an incoming stream: datagrams only.
const wtStreamUnsupported webtransport.StreamErrorCode = 3

const (
	wtRingSize   = 256
	wtRingMaxAge = 50 * time.Millisecond
	wtReadBuffer = 2048 // above max_datagram so an oversize target datagram is seen, not truncated
	wtRecvBuffer = 1 << 20
)

type wtSession struct {
	relay       *wtRelay
	route       *wtRoute
	sess        *webtransport.Session
	udp         *net.UDPConn
	local       netip.AddrPort
	user        string
	maxDatagram int
	ring        *wtRing
	started     time.Time
	ctx         context.Context
	cancel      context.CancelFunc

	endOnce sync.Once
	endCode webtransport.SessionErrorCode
	endMsg  string
	done    chan struct{}

	dgramsIn, dgramsOut, bytesIn, bytesOut, drops atomic.Int64
	oversizeInLogged, oversizeOutLogged           atomic.Bool
	panicked                                      atomic.Bool
}

// wtDialTarget opens the session's connected UDP socket: a fixed ephemeral
// source port for the session's life, delivery from the target only.
func wtDialTarget(target netip.AddrPort) (*net.UDPConn, error) {
	c, err := net.DialUDP(udpNetwork(target.Addr()), nil, net.UDPAddrFromAddrPort(target))
	if err != nil {
		return nil, err
	}
	_ = c.SetReadBuffer(wtRecvBuffer) // best effort
	return c, nil
}

func newWtSession(relay *wtRelay, route *wtRoute, sess *webtransport.Session, udp *net.UDPConn, local netip.AddrPort, user string, maxDatagram int) *wtSession {
	ctx, cancel := context.WithCancel(context.Background())
	s := &wtSession{
		relay: relay, route: route, sess: sess, udp: udp, local: local, user: user,
		maxDatagram: maxDatagram, started: time.Now(), ctx: ctx, cancel: cancel,
		done: make(chan struct{}),
	}
	s.ring = newWtRing(wtRingSize, wtRingMaxAge, time.Now, func() {
		s.drops.Add(1)
		route.counters.droppedQueue.Add(1)
	})
	return s
}

// run starts the session's goroutines and returns at once.
func (s *wtSession) run() {
	s.relay.sessMu.Lock()
	s.relay.sessions[s] = struct{}{}
	s.relay.sessMu.Unlock()
	s.relay.logger.Info("janus webtransport session open",
		zap.String("route", wtRouteKey(s.route.host, s.route.path)), zap.String("user", s.user))
	go s.fromBrowser()
	go s.fromTarget()
	go s.toBrowser()
	go s.refuseStreams()
	go s.closer()
}

// end records the outcome once and unblocks every loop: the UDP socket
// closes first, then the closer performs the session close.
func (s *wtSession) end(code webtransport.SessionErrorCode, msg string) {
	s.endOnce.Do(func() {
		s.endCode, s.endMsg = code, msg
		_ = s.udp.Close()
		s.cancel()
		s.ring.close()
		close(s.done)
	})
}

// closer waits for the end signal or the browser's own close, then folds
// the session out of the relay and closes it from this goroutine
// (CloseWithError blocks until the session context ends).
func (s *wtSession) closer() {
	defer wtRecover(s.relay.logger, "closer", nil)
	select {
	case <-s.done:
	case <-s.sess.Context().Done():
		s.end(wtCloseNormal, "peer closed")
	}
	s.relay.sessMu.Lock()
	delete(s.relay.sessions, s)
	s.relay.sessMu.Unlock()
	s.route.sessions.Add(-1)
	s.relay.sessionN.Add(-1)
	s.relay.logger.Info("janus webtransport session closed",
		zap.String("route", wtRouteKey(s.route.host, s.route.path)), zap.String("user", s.user),
		zap.Duration("duration", time.Since(s.started).Round(time.Millisecond)),
		zap.Int64("datagrams_in", s.dgramsIn.Load()), zap.Int64("datagrams_out", s.dgramsOut.Load()),
		zap.Int64("bytes_in", s.bytesIn.Load()), zap.Int64("bytes_out", s.bytesOut.Load()),
		zap.Int64("dropped", s.drops.Load()), zap.Uint32("code", uint32(s.endCode)), zap.String("reason", s.endMsg))
	if s.sess.Context().Err() == nil {
		_ = s.sess.CloseWithError(s.endCode, s.endMsg)
	}
}

// fromBrowser relays each received datagram to the target at once. Its
// only blocking call besides the receive is the UDP write, which does not
// wait; quic-go's and http3's own receive queues sit below it.
func (s *wtSession) fromBrowser() {
	defer wtRecover(s.relay.logger, "from-browser", s)
	for {
		b, err := s.sess.ReceiveDatagram(s.ctx)
		if err != nil {
			if s.ctx.Err() == nil {
				s.end(wtCloseNormal, "peer closed")
			}
			return
		}
		if len(b) > s.maxDatagram {
			s.route.counters.droppedOversizeIn.Add(1)
			if s.oversizeInLogged.CompareAndSwap(false, true) {
				s.relay.logger.Warn("janus webtransport: dropped an oversize datagram from the browser",
					zap.String("route", wtRouteKey(s.route.host, s.route.path)), zap.Int("bytes", len(b)), zap.Int("max_datagram", s.maxDatagram))
			}
			continue
		}
		if _, err := s.udp.Write(b); err != nil {
			if errors.Is(err, syscall.ECONNREFUSED) {
				s.route.counters.upstreamRefused.Add(1)
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.end(wtCloseUpstream, "udp write: "+err.Error())
			return
		}
		s.dgramsIn.Add(1)
		s.bytesIn.Add(int64(len(b)))
		s.route.counters.datagramsIn.Add(1)
		s.route.counters.bytesIn.Add(int64(len(b)))
	}
}

// fromTarget reads the connected socket into the ring.
func (s *wtSession) fromTarget() {
	defer wtRecover(s.relay.logger, "from-target", s)
	for {
		buf := wtBufPool.Get().(*[]byte)
		n, err := s.udp.Read((*buf)[:wtReadBuffer])
		if err != nil {
			wtBufPool.Put(buf)
			if errors.Is(err, syscall.ECONNREFUSED) {
				s.route.counters.upstreamRefused.Add(1)
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			s.end(wtCloseUpstream, "udp read: "+err.Error())
			return
		}
		if n > s.maxDatagram {
			wtBufPool.Put(buf)
			s.route.counters.droppedOversizeOut.Add(1)
			if s.oversizeOutLogged.CompareAndSwap(false, true) {
				s.relay.logger.Warn("janus webtransport: dropped an oversize datagram from the target",
					zap.String("route", wtRouteKey(s.route.host, s.route.path)), zap.Int("bytes", n), zap.Int("max_datagram", s.maxDatagram))
			}
			continue
		}
		s.ring.push(wtItem{buf: buf, n: n, at: time.Now()})
	}
}

// toBrowser sends ring entries; it is the only caller of SendDatagram for
// this session, because SendDatagram blocks once 32 frames are queued.
func (s *wtSession) toBrowser() {
	defer wtRecover(s.relay.logger, "to-browser", s)
	for {
		item, ok := s.ring.pop()
		if !ok {
			return
		}
		err := s.sess.SendDatagram((*item.buf)[:item.n])
		n := item.n
		wtBufPool.Put(item.buf)
		if err != nil {
			var tooLarge *quic.DatagramTooLargeError
			if errors.As(err, &tooLarge) {
				s.route.counters.droppedOversizeOut.Add(1)
				if s.oversizeOutLogged.CompareAndSwap(false, true) {
					s.relay.logger.Warn("janus webtransport: the QUIC path refused a datagram as too large",
						zap.String("route", wtRouteKey(s.route.host, s.route.path)), zap.Int("bytes", n))
				}
				continue
			}
			if s.ctx.Err() == nil {
				s.end(wtCloseNormal, "peer closed")
			}
			return
		}
		s.dgramsOut.Add(1)
		s.bytesOut.Add(int64(n))
		s.route.counters.datagramsOut.Add(1)
		s.route.counters.bytesOut.Add(int64(n))
	}
}

// refuseStreams resets every incoming stream: datagrams only in v1.
func (s *wtSession) refuseStreams() {
	defer wtRecover(s.relay.logger, "streams", s)
	go func() {
		defer wtRecover(s.relay.logger, "uni-streams", s)
		for {
			str, err := s.sess.AcceptUniStream(s.ctx)
			if err != nil {
				return
			}
			str.CancelRead(wtStreamUnsupported)
			s.route.counters.streamsReset.Add(1)
		}
	}()
	for {
		str, err := s.sess.AcceptStream(s.ctx)
		if err != nil {
			return
		}
		str.CancelRead(wtStreamUnsupported)
		str.CancelWrite(wtStreamUnsupported)
		s.route.counters.streamsReset.Add(1)
	}
}

var wtBufPool = sync.Pool{New: func() any { b := make([]byte, wtReadBuffer); return &b }}

// wtItem is one target datagram waiting for the browser.
type wtItem struct {
	buf *[]byte
	n   int
	at  time.Time
}

// wtRing is the bounded target → browser queue: full drops the oldest,
// and an entry older than maxAge is dropped when it reaches the front.
// Both drops count. The clock is injected for tests.
type wtRing struct {
	mu      sync.Mutex
	cond    *sync.Cond
	items   []wtItem
	head, n int
	closed  bool
	maxAge  time.Duration
	now     func() time.Time
	dropped func()
}

func newWtRing(size int, maxAge time.Duration, now func() time.Time, dropped func()) *wtRing {
	r := &wtRing{items: make([]wtItem, size), maxAge: maxAge, now: now, dropped: dropped}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *wtRing) push(item wtItem) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		wtBufPool.Put(item.buf)
		return
	}
	if r.n == len(r.items) {
		oldest := r.items[r.head]
		r.head = (r.head + 1) % len(r.items)
		r.n--
		wtBufPool.Put(oldest.buf)
		r.dropped()
	}
	r.items[(r.head+r.n)%len(r.items)] = item
	r.n++
	r.mu.Unlock()
	r.cond.Signal()
}

// pop returns the oldest entry younger than maxAge; ok is false once the
// ring is closed and drained.
func (r *wtRing) pop() (wtItem, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for {
		for r.n == 0 && !r.closed {
			r.cond.Wait()
		}
		if r.n == 0 {
			return wtItem{}, false
		}
		item := r.items[r.head]
		r.items[r.head] = wtItem{}
		r.head = (r.head + 1) % len(r.items)
		r.n--
		if r.now().Sub(item.at) > r.maxAge {
			wtBufPool.Put(item.buf)
			r.dropped()
			continue
		}
		return item, true
	}
}

func (r *wtRing) close() {
	r.mu.Lock()
	r.closed = true
	for r.n > 0 {
		wtBufPool.Put(r.items[r.head].buf)
		r.items[r.head] = wtItem{}
		r.head = (r.head + 1) % len(r.items)
		r.n--
	}
	r.mu.Unlock()
	r.cond.Broadcast()
}
