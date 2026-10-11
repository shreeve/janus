package janus

// A vista session: one stream to one em process, one request at a time.
// One reader goroutine owns every read of the stream, so an idle close is
// seen at once and bytes nobody asked for end the session; a request's
// body is written whole once it leaves the queue, whatever its client
// does, and its reply is read to its end by the framing rule (vista_xwb.go)
// even when the client has gone, so the stream stays aligned.

import (
	"net"
	"net/http"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

const (
	vistaDialTimeout  = 3 * time.Second
	vistaWriteTimeout = 30 * time.Second
	vistaReadChunk    = 32 << 10
	// vistaCauseRejected ends a session whose TCPConnect em answered
	// reject: no token was minted, so it leaves no tombstone that matters.
	vistaCauseRejected = "rejected"
)

type vistaSession struct {
	id        string
	route     *vistaRouteState
	logger    *zap.Logger
	conn      net.Conn
	clientKey netip.Prefix
	client    string
	started   time.Time
	mac       string // guarded by route.mu

	chunks chan []byte   // reader → the running request
	eof    chan struct{} // closed when the stream reads EOF or an error
	done   chan struct{} // closed when the session ends
	turn   chan struct{} // holds one token while no request runs

	pending  atomic.Int32 // requests queued or running
	runSince atomic.Int64 // unix nanoseconds the running request began; 0 when idle

	mu        sync.Mutex
	active    bool
	starting  bool // TCPConnect's reply is being decided; a close waits for vistaStart
	ended     bool
	cause     string
	signed    bool
	changing  bool
	endAfter  string // a cause to end with when the running reply ends
	timer     *time.Timer
	timerGen  int
	timeout   time.Duration
	requests  atomic.Int64
	bytesIn   atomic.Int64
	bytesOut  atomic.Int64
	eofClosed atomic.Bool
}

func newVistaSession(rs *vistaRouteState, conn net.Conn, key netip.Prefix, client string, logger *zap.Logger) *vistaSession {
	s := &vistaSession{
		id: vistaSessionID(), route: rs, logger: logger, conn: conn,
		clientKey: key, client: client, started: time.Now(),
		chunks: make(chan []byte, 1), eof: make(chan struct{}), done: make(chan struct{}),
		turn: make(chan struct{}, 1), starting: true,
	}
	go s.readLoop()
	return s
}

// readLoop is the stream's only reader. While a request runs it hands
// each read to it; a read while idle is a protocol violation, and EOF or
// an error while idle ends the session.
func (s *vistaSession) readLoop() {
	buf := make([]byte, vistaReadChunk)
	for {
		n, err := s.conn.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			s.mu.Lock()
			if !s.active {
				s.mu.Unlock()
				s.end(vistaEndProtocol)
				return
			}
			select {
			case s.chunks <- chunk:
				s.mu.Unlock()
			default:
				s.mu.Unlock()
				select {
				case s.chunks <- chunk:
				case <-s.done:
					return
				}
			}
		}
		if err != nil {
			s.eofClosed.Store(true)
			close(s.eof)
			s.mu.Lock()
			idle := !s.active && !s.starting
			s.mu.Unlock()
			if idle {
				s.end(vistaEndClosed)
			}
			return
		}
	}
}

// end ends the session with cause, once: the stream closes (so em exits),
// the token becomes a tombstone, and the route's counts are released.
func (s *vistaSession) end(cause string) {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return
	}
	s.ended, s.cause = true, cause
	signed := s.signed
	if s.timer != nil {
		s.timer.Stop()
	}
	s.mu.Unlock()
	close(s.done)
	_ = s.conn.Close()
	s.route.forget(s, cause, signed)
	if cause == vistaCauseRejected {
		return
	}
	s.logger.Info("janus vista: session ended",
		zap.String("route", s.route.key), zap.String("session", s.id), zap.String("client", s.client),
		zap.String("cause", cause), zap.Duration("duration", time.Since(s.started)),
		zap.Int64("requests", s.requests.Load()), zap.Int64("bytes_in", s.bytesIn.Load()),
		zap.Int64("bytes_out", s.bytesOut.Load()))
}

func (s *vistaSession) endedCause() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cause, s.ended
}

func (s *vistaSession) runningSince() time.Time {
	if ns := s.runSince.Load(); ns != 0 {
		return time.Unix(0, ns)
	}
	return time.Time{}
}

// armSignon starts (or restarts) the sign-on deadline.
func (s *vistaSession) armSignon(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armSignonLocked(d)
}

func (s *vistaSession) armSignonLocked(d time.Duration) {
	if s.timer != nil {
		s.timer.Stop()
	}
	s.timerGen++
	gen := s.timerGen
	s.timeout = d
	s.timer = time.AfterFunc(d, func() { s.signonExpired(gen) })
}

func (s *vistaSession) signonExpired(gen int) {
	s.mu.Lock()
	if s.signed || s.ended || gen != s.timerGen {
		s.mu.Unlock()
		return
	}
	if s.active {
		s.endAfter = vistaEndSignonTimeout
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.end(vistaEndSignonTimeout)
}

// applySignon acts on a judged sign-on reply and reports whether the
// session has just become signed on (so its token rotates).
func (s *vistaSession) applySignon(v signonVerdict) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signed || s.ended {
		return false
	}
	switch v {
	case signonOn:
	case signonCVC:
		if !s.changing {
			return false
		}
	case signonExpired:
		s.changing = true
		s.armSignonLocked(s.timeout)
		return false
	default:
		return false
	}
	s.signed = true
	if s.timer != nil {
		s.timer.Stop()
	}
	s.route.signedOn(s)
	return true
}

// vistaResponder writes a reply to the client as it is read: headers with
// the first byte (or, while holding, once released), each write under a
// deadline and flushed. A client that fails a write is gone; the reply
// still drains.
type vistaResponder struct {
	w          http.ResponseWriter
	rc         *http.ResponseController
	hold       bool
	held       []byte
	headersOut bool
	gone       bool
	setCookie  string
}

func newVistaResponder(w http.ResponseWriter) *vistaResponder {
	return &vistaResponder{w: w, rc: http.NewResponseController(w)}
}

func (o *vistaResponder) writeHeaders() {
	if o.headersOut {
		return
	}
	o.headersOut = true
	h := o.w.Header()
	h.Set("Content-Type", "application/x-xwb")
	h.Set("Cache-Control", "no-store")
	if o.setCookie != "" {
		h.Add("Set-Cookie", o.setCookie)
	}
	o.w.WriteHeader(http.StatusOK)
}

func (o *vistaResponder) write(b []byte) {
	if len(b) == 0 {
		return
	}
	if o.hold {
		o.held = append(o.held, b...)
		return
	}
	o.send(b)
}

func (o *vistaResponder) send(b []byte) {
	if o.gone {
		return
	}
	o.writeHeaders()
	_ = o.rc.SetWriteDeadline(time.Now().Add(vistaWriteTimeout))
	if _, err := o.w.Write(b); err != nil {
		o.gone = true
		return
	}
	if err := o.rc.Flush(); err != nil {
		o.gone = true
	}
}

// release stops holding and sends what was held.
func (o *vistaResponder) release() {
	if !o.hold {
		return
	}
	o.hold = false
	held := o.held
	o.held = nil
	o.send(held)
}

// vistaExchange is what one request's exchange came to.
type vistaExchange struct {
	complete bool   // the reply ended by the framing rule
	cause    string // the session's end cause when the exchange failed
	reply    []byte // the whole reply, kept only when keep is set
}

// exchange writes body and reads its reply into o. The caller holds the
// session's turn. judge names the sign-on RPC whose reply is judged, ""
// for none; keep keeps the whole reply (TCPConnect's, a few bytes); bye
// marks the #BYE# command, whose completed reply ends the session.
func (s *vistaSession) exchange(body []byte, judge string, keep, bye bool, l *vistaLimits, deadline time.Time, o *vistaResponder) vistaExchange {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		c, _ := s.endedCause()
		return vistaExchange{cause: c}
	}
	select {
	case <-s.chunks:
		// Bytes left from an earlier reply would become this one's.
		s.mu.Unlock()
		s.end(vistaEndProtocol)
		return vistaExchange{cause: vistaEndProtocol}
	default:
	}
	s.active = true
	s.mu.Unlock()
	s.runSince.Store(time.Now().UnixNano())
	defer s.runSince.Store(0)

	_ = s.conn.SetWriteDeadline(time.Now().Add(vistaWriteTimeout))
	if _, err := s.conn.Write(body); err != nil {
		s.end(vistaEndClosed)
		return vistaExchange{cause: vistaEndClosed}
	}
	s.bytesIn.Add(int64(len(body)))
	s.route.c.bytesIn.Add(int64(len(body)))

	var sc xwbReplyScanner
	var kept []byte
	judged := judge == ""
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	leftover := false
	feed := func(chunk []byte) (bool, string) {
		n, done := sc.Feed(chunk)
		part := chunk[:n]
		if sc.Total() > l.maxReply {
			return false, vistaEndReplyTooLarge
		}
		s.bytesOut.Add(int64(n))
		s.route.c.bytesOut.Add(int64(n))
		if keep {
			kept = append(kept, part...)
		}
		o.write(part)
		if !judged && sc.PastShorts() {
			if v, decided := judgeSignon(judge, sc.Clean(), sc.MError(), sc.DataPrefix(), done); decided {
				judged = true
				if s.applySignon(v) {
					token := vistaMintToken()
					s.route.bind(s, token, vistaEndRotated)
					o.setCookie = vistaSetCookie(token)
				}
				o.release()
			}
		}
		if done && n < len(chunk) {
			leftover = true
		}
		return done, ""
	}
	for {
		select {
		case chunk := <-s.chunks:
			done, cause := feed(chunk)
			if cause != "" {
				s.end(cause)
				return vistaExchange{cause: cause}
			}
			if done {
				return s.finish(sc, judge, judged, leftover, bye, kept, o)
			}
		case <-s.eof:
			// The reader hands over its last read before it closes eof.
			for {
				select {
				case chunk := <-s.chunks:
					done, cause := feed(chunk)
					if cause != "" {
						s.end(cause)
						return vistaExchange{cause: cause}
					}
					if done {
						return s.finish(sc, judge, judged, leftover, bye, kept, o)
					}
					continue
				default:
				}
				break
			}
			s.end(vistaEndClosed)
			return vistaExchange{cause: vistaEndClosed}
		case <-s.done:
			c, _ := s.endedCause()
			return vistaExchange{cause: c}
		case <-timer.C:
			s.end(vistaEndRPCTimeout)
			return vistaExchange{cause: vistaEndRPCTimeout}
		}
	}
}

// finish closes a complete exchange: the last sign-on judgement, the
// leftover-bytes rule, and an end the sign-on deadline deferred.
func (s *vistaSession) finish(sc xwbReplyScanner, judge string, judged, leftover, bye bool, kept []byte, o *vistaResponder) vistaExchange {
	if !judged {
		v, _ := judgeSignon(judge, sc.Clean(), sc.MError(), sc.DataPrefix(), true)
		if s.applySignon(v) {
			token := vistaMintToken()
			s.route.bind(s, token, vistaEndRotated)
			o.setCookie = vistaSetCookie(token)
		}
		// A sign-on reply held its headers for the judgement; any other
		// hold (TCPConnect's) is the caller's to release.
		o.release()
	}
	s.mu.Lock()
	select {
	case <-s.chunks:
		leftover = true
	default:
	}
	s.active = false
	endAfter := s.endAfter
	starting := s.starting
	s.mu.Unlock()
	s.requests.Add(1)
	switch {
	case bye:
		// The #BYE# command's reply completed: em logs out and exits.
		s.end(vistaEndBye)
	case leftover:
		s.end(vistaEndProtocol)
	case endAfter != "":
		s.end(endAfter)
	case s.eofClosed.Load() && !starting:
		s.end(vistaEndClosed)
	}
	return vistaExchange{complete: true, reply: kept}
}

// startedOK ends the TCPConnect phase of an accepted session, which holds
// its token by now: a stream that closed meanwhile ends it (closed) here,
// and from now on the reader ends it at once. While starting, a close is
// left to vistaStart, so a reject is never counted as an ended session.
func (s *vistaSession) startedOK() {
	s.mu.Lock()
	s.starting = false
	closed := s.eofClosed.Load()
	s.mu.Unlock()
	if closed {
		s.end(vistaEndClosed)
	}
}

func vistaSetCookie(token string) string {
	return vistaCookieName + "=" + token + "; Path=/; Secure; HttpOnly; SameSite=Strict"
}

const vistaClearCookie = vistaCookieName + "=; Path=/; Max-Age=0; Secure; HttpOnly; SameSite=Strict"
