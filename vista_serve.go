package janus

// A vista route's request path: admission in the contract's order, the
// token, a new session on TCPConnect, and every later request through its
// session's queue (docs/20261010-232539-capability-vista.md, Admission).

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
)

const (
	vistaBodyReadTimeout = 30 * time.Second
	vistaEndedHeader     = "OV-Session-Ended"
)

// vistaToken is where a request's token came from, if anywhere.
type vistaToken struct {
	value     string
	viaCookie bool
	err       error // two tokens, a malformed one, or a non-Bearer Authorization
}

func vistaRequestToken(r *http.Request) vistaToken {
	var t vistaToken
	cookies := 0
	for _, c := range r.Cookies() {
		if c.Name == vistaCookieName {
			cookies++
			t.value, t.viaCookie = c.Value, true
		}
	}
	if cookies > 1 {
		t.err = errors.New("two session cookies")
		return t
	}
	if auths := r.Header.Values("Authorization"); len(auths) > 0 {
		if len(auths) > 1 {
			t.err = errors.New("two Authorization headers")
			return t
		}
		scheme, value, ok := strings.Cut(auths[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			t.err = errors.New("an Authorization header that is not Bearer")
			return t
		}
		if t.viaCookie {
			t.err = errors.New("a session cookie and a Bearer token together")
			return t
		}
		t.value = strings.TrimSpace(value)
	}
	if t.value != "" && !vistaTokenWellFormed(t.value) {
		t.err = errors.New("a malformed token")
	}
	return t
}

func vistaTokenWellFormed(v string) bool {
	if len(v) != vistaTokenLen {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// vistaOriginAllowed applies the route's origin policy to a present Origin.
func vistaOriginAllowed(l *vistaLimits, origin, host string) bool {
	if origin == "null" {
		return false
	}
	n, err := normalizeVistaOrigin(origin)
	if err != nil {
		return false
	}
	if l.originSame {
		h, port, err := net.SplitHostPort(host)
		if err != nil {
			h, port = host, ""
		}
		if same, err := normalizeVistaOrigin("https://" + netJoinHost(strings.ToLower(h), port)); err == nil && same == n {
			return true
		}
	}
	for _, o := range l.origins {
		if o == n {
			return true
		}
	}
	return false
}

// vistaPeer returns the client address the PROXY line and the caps use:
// the TCP peer, or Caddy's client IP when trusted_proxies trusts the peer.
func vistaPeer(r *http.Request) (netip.AddrPort, bool) {
	if trusted, _ := caddyhttp.GetVar(r.Context(), caddyhttp.TrustedProxyVarKey).(bool); trusted {
		if ip, _ := caddyhttp.GetVar(r.Context(), caddyhttp.ClientIPVarKey).(string); ip != "" {
			if a, err := netip.ParseAddr(ip); err == nil {
				return netip.AddrPortFrom(a.WithZone(""), 0), true
			}
		}
	}
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

func vistaLocal(r *http.Request) (netip.AddrPort, bool) {
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return netip.AddrPort{}, false
	}
	ap, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return netip.AddrPort{}, false
	}
	return ap, true
}

func vistaLogField(r *http.Request, f zap.Field) {
	if extra, ok := r.Context().Value(caddyhttp.ExtraLogFieldsCtxKey).(*caddyhttp.ExtraLogFields); ok {
		extra.Add(f)
	}
}

// serveVista answers one request to a route.
func (h *Handler) serveVista(w http.ResponseWriter, r *http.Request, rt *vistaSiteRoute) error {
	if h.app == nil || h.app.state == nil {
		return caddyhttp.Error(http.StatusServiceUnavailable, errors.New("janus vista: no pooled state"))
	}
	l := rt.limits
	host := normalizeHostHeader(r.Host)
	rs := h.app.state.vista.route(vistaRouteKey(host, rt.path), rt.target, h.app)
	w.Header().Set("Cache-Control", "no-store")
	tok := vistaRequestToken(r)
	fail := func(code int, row string, warn bool, msg string, fields ...zap.Field) error {
		rs.refuse(row, warn, fields...)
		if tok.viaCookie && code == http.StatusUnauthorized {
			w.Header().Add("Set-Cookie", vistaClearCookie)
		}
		http.Error(w, msg, code)
		return nil
	}

	// Rows 1–5: method, TLS, early data, the browser rule, content type.
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		return fail(http.StatusMethodNotAllowed, "method", true, "method not allowed")
	}
	if r.TLS == nil {
		return fail(http.StatusMisdirectedRequest, "no_tls", true, "vista requires TLS ending at Janus")
	}
	trusted, _ := caddyhttp.GetVar(r.Context(), caddyhttp.TrustedProxyVarKey).(bool)
	if !r.TLS.HandshakeComplete || (trusted && r.Header.Get("Early-Data") == "1") {
		return fail(http.StatusTooEarly, "early_data", true, "too early")
	}
	origin := r.Header.Get("Origin")
	if origin != "" && !vistaOriginAllowed(l, origin, r.Host) {
		return fail(http.StatusForbidden, "origin", true, "forbidden", zap.String("origin", origin))
	}
	if (origin != "" || tok.viaCookie) && r.Header.Get("X-OV") != "1" {
		return fail(http.StatusForbidden, "origin", true, "forbidden: X-OV: 1 required", zap.String("origin", origin))
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-xwb" {
		return fail(http.StatusUnsupportedMediaType, "content_type", true, "want application/x-xwb")
	}

	// Row 6: one whole message, read within the limit and 30 s, walked.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(vistaBodyReadTimeout))
	body, err := io.ReadAll(io.LimitReader(r.Body, l.maxRequest+1))
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		return fail(http.StatusBadRequest, "message", true, "bad request: the body did not arrive", zap.Error(err))
	}
	if int64(len(body)) > l.maxRequest {
		return fail(http.StatusBadRequest, "message", true, "bad request: the body exceeds the limit")
	}
	msg, err := walkXWB(body)
	if err != nil {
		return fail(http.StatusBadRequest, "message", true, "bad request: not one broker message", zap.String("why", err.Error()))
	}

	// Row 7: the token's form.
	if tok.err != nil {
		return fail(http.StatusBadRequest, "token", true, "bad request: "+tok.err.Error())
	}

	// Rows 8–10: TCPConnect starts a new session, whatever token it carries.
	if msg.Command == "TCPConnect" {
		return h.vistaStart(w, r, rt, rs, body, tok)
	}

	// Rows 11–12: a token naming a live session.
	if tok.value == "" {
		w.Header().Set(vistaEndedHeader, vistaEndUnknown)
		return fail(http.StatusUnauthorized, "unknown", false, "no session")
	}
	s, cause := rs.lookup(tok.value)
	if s == nil {
		w.Header().Set(vistaEndedHeader, cause)
		return fail(http.StatusUnauthorized, "unknown", false, "the session has ended")
	}
	vistaLogField(r, zap.String("vista_session", s.id))
	return h.vistaRun(w, r, rs, s, l, body, msg, tok)
}

// vistaStart admits, dials, and opens a session for a TCPConnect.
func (h *Handler) vistaStart(w http.ResponseWriter, r *http.Request, rt *vistaSiteRoute, rs *vistaRouteState, body []byte, tok vistaToken) error {
	l := rt.limits
	replaced := false
	if tok.value != "" {
		if old, _ := rs.lookup(tok.value); old != nil {
			old.end(vistaEndReplaced)
			replaced = true
		}
	}
	// A browser whose session this TCPConnect replaced holds a dead
	// cookie; any answer without a new token clears it at once.
	clearReplaced := func() {
		if replaced && tok.viaCookie {
			w.Header().Add("Set-Cookie", vistaClearCookie)
		}
	}
	fail := func(code int, msg string) error {
		clearReplaced()
		http.Error(w, msg, code)
		return nil
	}
	peer, ok := vistaPeer(r)
	local, lok := vistaLocal(r)
	if !ok || !lok {
		rs.refuse("dial", true, zap.String("why", "the connection's addresses are unknown"))
		return fail(http.StatusBadGateway, "bad gateway")
	}
	key := vistaClientKey(peer.Addr())
	exempt := false
	for _, p := range l.perClientExempt {
		if p.Contains(peer.Addr().Unmap()) {
			exempt = true
			break
		}
	}
	if capName := rs.reserve(l, key, exempt); capName != "" {
		rs.refuse(capName, true, zap.String("client", key.String()))
		w.Header().Set("Retry-After", "5")
		return fail(http.StatusServiceUnavailable, "service unavailable: "+strings.ReplaceAll(capName, "_", " "))
	}
	conn, err := net.DialTimeout("unix", l.socket, vistaDialTimeout)
	if err != nil {
		rs.unreserve(key, false)
		rs.refuse("dial", true, zap.Error(err))
		return fail(http.StatusBadGateway, "bad gateway")
	}
	s := newVistaSession(rs, conn, key, peer.Addr().String(), h.app.state.vista.logger)
	rs.add(s)
	s.start()
	vistaLogField(r, zap.String("vista_session", s.id))
	_ = conn.SetWriteDeadline(time.Now().Add(vistaWriteTimeout))
	if _, err := conn.Write([]byte(vistaProxyLine(peer, local))); err != nil {
		s.end(vistaEndClosed)
		return fail(http.StatusBadGateway, "bad gateway")
	}
	o := newVistaResponder(w)
	o.hold = true // the token rides in the headers, decided by the reply
	ex := s.exchange(body, "", true, false, l, time.Now().Add(l.rpcTimeout), o)
	switch {
	case !ex.complete:
		w.Header().Set(vistaEndedHeader, ex.cause)
		status := http.StatusBadGateway
		if ex.cause == vistaEndRPCTimeout {
			status = http.StatusGatewayTimeout
		}
		return fail(status, "bad gateway")
	case string(ex.reply) == "\x00\x00accept\x04":
		token := vistaMintToken()
		rs.bind(s, token, vistaEndRotated)
		o.setCookie = vistaSetCookie(token)
		s.armSignon(l.signonTimeout)
		rs.c.started.Add(1)
		s.logger.Info("janus vista: session started",
			zap.String("route", rs.key), zap.String("session", s.id), zap.String("client", s.client))
		o.release()
		s.turn <- struct{}{}
		s.startedOK()
		return nil
	case string(ex.reply) == "\x00\x00reject\x04":
		// Sign-ons inhibited or the job limit reached: em ends the stream.
		rs.c.rejected.Add(1)
		s.end(vistaCauseRejected)
		if replaced && tok.viaCookie {
			o.setCookie = vistaClearCookie
		}
		o.release()
		return nil
	default:
		s.end(vistaEndClosed)
		s.logger.Warn("janus vista: TCPConnect answered neither accept nor reject",
			zap.String("route", rs.key), zap.Int("bytes", len(ex.reply)))
		return fail(http.StatusBadGateway, "bad gateway")
	}
}

// vistaRun queues a request on its session and runs it.
func (h *Handler) vistaRun(w http.ResponseWriter, r *http.Request, rs *vistaRouteState, s *vistaSession, l *vistaLimits, body []byte, msg xwbMessage, tok vistaToken) error {
	ended := func(cause string) error {
		w.Header().Set(vistaEndedHeader, cause)
		if tok.viaCookie {
			w.Header().Add("Set-Cookie", vistaClearCookie)
		}
		http.Error(w, "the session has ended", http.StatusUnauthorized)
		return nil
	}
	if s.pending.Add(1) > int32(l.queue)+1 {
		s.pending.Add(-1)
		rs.refuse("queue", true)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "service unavailable: the session's queue is full", http.StatusServiceUnavailable)
		return nil
	}
	defer s.pending.Add(-1)
	// The wait in the queue has a limit of its own; the request's
	// rpc_timeout runs from leaving the queue.
	wait := time.NewTimer(l.rpcTimeout)
	rs.c.queued.Add(1)
	select {
	case <-s.turn:
		wait.Stop()
		rs.c.queued.Add(-1)
	case <-r.Context().Done():
		wait.Stop()
		rs.c.queued.Add(-1)
		rs.c.clientGone.Add(1)
		return nil
	case <-wait.C:
		rs.c.queued.Add(-1)
		rs.c.queueTimeout.Add(1)
		http.Error(w, "gateway timeout: the request waited past its limit and was never sent", http.StatusGatewayTimeout)
		return nil
	case <-s.done:
		wait.Stop()
		rs.c.queued.Add(-1)
		c, _ := s.endedCause()
		return ended(c)
	}
	defer func() { s.turn <- struct{}{} }()
	deadline := time.Now().Add(l.rpcTimeout)
	if c, done := s.endedCause(); done {
		return ended(c)
	}
	rs.c.running.Add(1)
	defer rs.c.running.Add(-1)
	rs.c.requests.Add(1)

	o := newVistaResponder(w)
	draining := false
	o.onGone = func() {
		draining = true
		rs.c.draining.Add(1)
	}
	defer func() {
		if draining {
			rs.c.draining.Add(-1)
		}
	}()
	judge := ""
	if msg.RPC == "XUS AV CODE" || msg.RPC == "XUS CVC" {
		judge = msg.RPC
		o.hold = true
	}
	bye := msg.Command == "#BYE#"
	if bye && tok.viaCookie {
		o.setCookie = vistaClearCookie
	}
	ex := s.exchange(body, judge, false, bye, l, deadline, o)
	if o.gone {
		rs.c.clientGone.Add(1)
	}
	if ex.complete {
		return nil
	}
	if !o.headersOut {
		o.hold = false
		w.Header().Del("Set-Cookie")
		w.Header().Set(vistaEndedHeader, ex.cause)
		if tok.viaCookie {
			w.Header().Add("Set-Cookie", vistaClearCookie)
		}
		status := http.StatusBadGateway
		if ex.cause == vistaEndRPCTimeout {
			status = http.StatusGatewayTimeout
		}
		http.Error(w, fmt.Sprintf("the session ended (%s)", ex.cause), status)
		return nil
	}
	// The 200 is out: the response cannot become an error, so it is cut.
	rs.c.aborted.Add(1)
	vistaLogField(r, zap.Bool("vista_aborted", true))
	if o.gone {
		return nil
	}
	panic(http.ErrAbortHandler)
}

// stripVistaCookie removes __Host-ov from a request the site forwards
// anywhere but its vista routes; only the vista handler reads the token.
func stripVistaCookie(r *http.Request) {
	if !strings.Contains(r.Header.Get("Cookie"), vistaCookieName) && len(r.Header.Values("Cookie")) < 2 {
		return
	}
	var kept []string
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
			if strings.HasPrefix(name, vistaCookieName) {
				continue
			}
			kept = append(kept, pair)
		}
	}
	r.Header.Del("Cookie")
	if len(kept) > 0 {
		r.Header.Set("Cookie", strings.Join(kept, "; "))
	}
}

// vistaRespStrip removes any __Host-ov Set-Cookie from a response the
// site sends from anywhere but its vista routes: no upstream or file can
// plant a session token (fixation).
type vistaRespStrip struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *vistaRespStrip) strip() {
	if w.wroteHeader {
		return
	}
	vals := w.Header().Values("Set-Cookie")
	if len(vals) == 0 {
		return
	}
	w.Header().Del("Set-Cookie")
	for _, v := range vals {
		name, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(strings.TrimSpace(name), vistaCookieName) {
			continue
		}
		w.Header().Add("Set-Cookie", v)
	}
}

func (w *vistaRespStrip) WriteHeader(status int) {
	w.strip()
	if status >= http.StatusOK {
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *vistaRespStrip) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *vistaRespStrip) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *vistaRespStrip) Unwrap() http.ResponseWriter { return w.ResponseWriter }
