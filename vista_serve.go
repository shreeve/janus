package janus

// A vista route's request path: admission in the contract's order, the
// token, a new session on TCPConnect, and every later request through its
// session's queue (docs/20261010-232539-capability-vista.md, Admission),
// with a tcp target's message guard (vista_guard.go).

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strconv"
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
	// A non-browser client presents Authorization: Bearer; everything else
	// is a browser (cookie) connection. The class is the namespace the token
	// resolves in, decided here and never re-derived mid-connection.
	bearer := r.Header.Get("Authorization") != ""
	// The walk records fields for a tcp target's guard and for a browser
	// connection's patient binding (ORWPT SELECT's first parameter).
	var detail *xwbDetail
	if l.tcp || !bearer {
		detail = &xwbDetail{}
	}
	msg, err := walkXWBInto(body, detail)
	if err != nil {
		return fail(http.StatusBadRequest, "message", true, "bad request: not one broker message", zap.String("why", err.Error()))
	}
	// A stock broker parses what it is sent as VA wrote it: the shape
	// rules judge every message, commands included, before anything else.
	if l.tcp {
		if rule := vistaShapeGuard(msg, detail); rule != "" {
			rs.guardRefuse(rule)
			vistaWriteSecurity(w, vistaGuardMessage(rule))
			return nil
		}
	}

	// Row 7: the token's form.
	if tok.err != nil {
		return fail(http.StatusBadRequest, "token", true, "bad request: "+tok.err.Error())
	}

	// A connection's class is fixed here and never re-derived: browser (the
	// groups namespace) or non-browser (the sessions namespace). A TCPConnect
	// is browser when it carries a cookie or an Origin — the first tab has no
	// cookie yet, so Origin is its only mark; a bootstrap TCPConnect with
	// neither, or one carrying a Bearer token, is non-browser (CPRS). A later
	// request is non-browser only when it presents a Bearer token.
	var browser bool
	if msg.Command == "TCPConnect" {
		browser = tok.viaCookie || origin != ""
	} else {
		browser = !bearer
	}

	// Rows 8–10: TCPConnect starts a new connection (a browser one joins its
	// cookie's group; a non-browser one is a lone session as today).
	if msg.Command == "TCPConnect" {
		return h.vistaStart(w, r, rt, rs, body, tok, browser)
	}

	// A non-browser later request: the Bearer token names its one session,
	// and no OV-* header applies.
	if !browser {
		if r.Header.Get("OV-Session") != "" || r.Header.Get("OV-Patient") != "" || r.Header.Get("OV-End") != "" {
			return fail(http.StatusBadRequest, "ov_header", true, "bad request: OV-* headers are for browser connections")
		}
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
		return h.vistaRun(w, r, rs, s, l, body, msg, detail, tok, "")
	}

	// A browser later request: OV-* header forms, then end-all, then the
	// group + OV-Session lookup.
	ovSession := r.Header.Get("OV-Session")
	ovPatient := r.Header.Get("OV-Patient")
	ovEnd := r.Header.Get("OV-End")
	if ovEnd != "" {
		if ovEnd != "all" || ovSession != "" || msg.Command != "#BYE#" {
			return fail(http.StatusBadRequest, "ov_header", true, "bad request: OV-End must be 'all' on a #BYE# with no OV-Session")
		}
		n := rs.endGroupAll(tok.value)
		w.Header().Set("OV-Ended", strconv.Itoa(n))
		w.Header().Add("Set-Cookie", vistaClearCookie)
		vistaWriteBye(w)
		return nil
	}
	if ovSession == "" || !vistaOVWellFormed(ovSession) {
		return fail(http.StatusBadRequest, "ov_session", true, "bad request: a well-formed OV-Session is required")
	}
	if ovPatient != "" && !vistaValidDFN(ovPatient) {
		return fail(http.StatusBadRequest, "ov_header", true, "bad request: a malformed OV-Patient")
	}
	if tok.value == "" {
		w.Header().Set(vistaEndedHeader, vistaEndUnknown)
		return fail(http.StatusUnauthorized, "unknown", false, "no session")
	}
	s, cause, groupLive := rs.lookupBrowser(tok.value, ovSession)
	if s == nil {
		w.Header().Set(vistaEndedHeader, cause)
		rs.refuse(refusalForGroup(groupLive), false)
		// Clear the cookie only when it names no live group at all; a dead
		// OV-Session in a live group leaves the other tabs' cookie alone.
		if !groupLive {
			w.Header().Add("Set-Cookie", vistaClearCookie)
		}
		http.Error(w, "the session has ended", http.StatusUnauthorized)
		return nil
	}
	vistaLogField(r, zap.String("vista_session", s.id))
	return h.vistaRun(w, r, rs, s, l, body, msg, detail, tok, ovPatient)
}

// refusalForGroup names the refusal row for a browser lookup miss.
func refusalForGroup(groupLive bool) string {
	if groupLive {
		return "connection"
	}
	return "unknown"
}

// vistaWriteBye answers end-all with a 200 and the broker's #BYE# reply.
func vistaWriteBye(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/x-xwb")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(vistaBrokerBye))
}

// vistaWriteSecurity answers a refused message with a broker security
// reply (200): the message was never sent and the session goes on.
func vistaWriteSecurity(w http.ResponseWriter, msg string) {
	h := w.Header()
	h.Set("Content-Type", "application/x-xwb")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(vistaSecurityReply(msg))
}

// vistaDialer dials a route's target: 3 s, and for a tcp target TCP
// keepalive every 15 s, so a broker host that vanishes is noticed.
func vistaDialer(l *vistaLimits) *net.Dialer {
	d := &net.Dialer{Timeout: vistaDialTimeout}
	if l.tcp {
		d.KeepAliveConfig = net.KeepAliveConfig{Enable: true, Idle: vistaKeepAlive, Interval: vistaKeepAlive}
	}
	return d
}

// vistaStart admits, dials, and opens a session for a TCPConnect. A browser
// connection joins its cookie's group (or a fresh one); a non-browser one is
// a lone session whose own token, if live, it replaces.
func (h *Handler) vistaStart(w http.ResponseWriter, r *http.Request, rt *vistaSiteRoute, rs *vistaRouteState, body []byte, tok vistaToken, browser bool) error {
	l := rt.limits
	// A non-browser TCPConnect that carries a live token (CPRS reconnecting
	// over Bearer) replaces its own prior session. A browser TCPConnect never
	// reaches here with a token: a cookie makes it browser, and browser joins
	// its group rather than replacing. So no cookie is cleared on this path.
	if !browser && tok.value != "" {
		if old, _ := rs.lookup(tok.value); old != nil {
			old.end(vistaEndReplaced)
		}
	}
	fail := func(code int, msg string) error {
		http.Error(w, msg, code)
		return nil
	}
	peer, ok := vistaPeer(r)
	local, lok := vistaLocal(r)
	if !ok || (!lok && !l.tcp) {
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
	conn, err := vistaDialer(l).Dial(l.network, l.address)
	if err != nil {
		rs.unreserve(key, false)
		rs.refuse("dial", true, zap.Error(err))
		return fail(http.StatusBadGateway, "bad gateway")
	}
	s := newVistaSession(rs, conn, key, peer.Addr().String(), h.app.state.vista.logger)
	s.tcp = l.tcp
	s.browser = browser
	rs.add(s)
	s.start()
	vistaLogField(r, zap.String("vista_session", s.id))
	// em reads the PROXY line first; a stock broker closes a connection
	// that does not begin with TCPConnect.
	if !l.tcp {
		_ = conn.SetWriteDeadline(time.Now().Add(vistaWriteTimeout))
		if _, err := conn.Write([]byte(vistaProxyLine(peer, local))); err != nil {
			s.end(vistaEndClosed)
			return fail(http.StatusBadGateway, "bad gateway")
		}
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
		if browser {
			// Join the cookie's group (or open a fresh one). The new cookie
			// is set only when a group was opened; a joining tab keeps it.
			ov, newToken := rs.attachBrowser(s, tok.value)
			w.Header().Set("OV-Session", ov)
			if newToken != "" {
				o.setCookie = vistaSetCookie(newToken)
			}
		} else {
			token := vistaMintToken()
			rs.bind(s, token, vistaEndRotated)
			o.setCookie = vistaSetCookie(token)
		}
		s.armSignon(l.signonTimeout)
		rs.c.started.Add(1)
		fields := []zap.Field{zap.String("route", rs.key), zap.String("session", s.id),
			zap.String("client", s.client), zap.String("network", l.network)}
		// The broker sees Janus's address for every session; the dial's
		// local port joins this line to the broker job's record.
		if ta, ok := conn.LocalAddr().(*net.TCPAddr); ok {
			fields = append(fields, zap.Int("local_port", ta.Port))
		}
		s.logger.Info("janus vista: session started", fields...)
		o.release()
		s.turn <- struct{}{}
		s.startedOK()
		return nil
	case string(ex.reply) == "\x00\x00reject\x04":
		// Sign-ons inhibited or the job limit reached: em ends the stream.
		rs.c.rejected.Add(1)
		s.end(vistaCauseRejected)
		o.release()
		return nil
	default:
		s.end(vistaEndClosed)
		s.logger.Warn("janus vista: TCPConnect answered neither accept nor reject",
			zap.String("route", rs.key), zap.Int("bytes", len(ex.reply)))
		return fail(http.StatusBadGateway, "bad gateway")
	}
}

// vistaRun queues a request on its session and runs it. ovPatient is the
// request's OV-Patient header (browser only, "" otherwise).
func (h *Handler) vistaRun(w http.ResponseWriter, r *http.Request, rs *vistaRouteState, s *vistaSession, l *vistaLimits, body []byte, msg xwbMessage, detail *xwbDetail, tok vistaToken, ovPatient string) error {
	ended := func(cause string) error {
		w.Header().Set(vistaEndedHeader, cause)
		// Clear the shared cookie only when this is the group's last tab;
		// a sibling tab still needs it (a non-browser cookie is always solo).
		if tok.viaCookie && rs.groupSolo(s) {
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
	// A tcp target's session rules, judged under the turn on the state
	// the requests before this one left; then the failed sign-on hold.
	if l.tcp {
		if rule := vistaSessionGuard(msg, detail, s.isSigned()); rule != "" {
			rs.guardRefuse(rule)
			vistaWriteSecurity(w, vistaGuardMessage(rule))
			return nil
		}
		if msg.RPC == "XUS AV CODE" && rs.failures.held(s.clientKey, time.Now()) {
			rs.c.failedSignonHolds.Add(1)
			vistaWriteSecurity(w, vistaMsgFailedSignon)
			return nil
		}
	}
	// A browser connection's advisory patient binding, judged under the turn
	// on the state earlier requests left: the first type-0-literal ORWPT
	// SELECT binds; a select of a different DFN, an OV-Patient naming a
	// different DFN, or an ORWPT SELECT whose first parameter is not a literal
	// DFN is refused unsent (a 200 security reply; the connection goes on).
	if s.browser {
		if h.vistaBindPatient(w, rs, s, msg, detail, ovPatient) {
			return nil
		}
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
	// A per-tab #BYE# clears the shared cookie only when no sibling tab is
	// left in the group; end-all (handled in serveVista) clears it outright.
	if bye && tok.viaCookie && rs.groupSolo(s) {
		o.setCookie = vistaClearCookie
	}
	ex := s.exchange(body, judge, false, bye, l, deadline, o)
	if o.gone {
		rs.c.clientGone.Add(1)
	}
	// An XUS AV CODE reply that signs nothing on and is not an expired
	// verify code is a failed sign-on from this client key.
	if l.tcp && ex.complete && msg.RPC == "XUS AV CODE" && ex.verdict != signonOn && ex.verdict != signonExpired {
		rs.failedSignon(s.clientKey, l)
	}
	if ex.complete {
		return nil
	}
	if !o.headersOut {
		o.hold = false
		w.Header().Del("Set-Cookie")
		w.Header().Set(vistaEndedHeader, ex.cause)
		if tok.viaCookie && rs.groupSolo(s) {
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

// Broker security messages for a refused browser patient-binding request.
const (
	vistaMsgOtherPatient = "This connection is open for another patient"
	vistaMsgSelectForm   = "This connection cannot select a patient this way"
)

// vistaBindPatient applies a browser connection's patient binding to one
// request and reports that it answered (a security reply, unsent). It is
// called under the session's turn, so the binding is read and set in order.
func (h *Handler) vistaBindPatient(w http.ResponseWriter, rs *vistaRouteState, s *vistaSession, msg xwbMessage, detail *xwbDetail, ovPatient string) bool {
	bound, isBound := rs.patientBound(s)
	// OV-Patient is advisory: a declared patient that differs from the
	// binding is refused before the RPC is sent; unbound, it is allowed.
	if ovPatient != "" && isBound && ovPatient != bound {
		rs.refuse("patient", false)
		vistaWriteSecurity(w, vistaMsgOtherPatient)
		return true
	}
	if msg.RPC != "ORWPT SELECT" {
		return false
	}
	dfn, ok := vistaFirstLiteralDFN(detail)
	if !ok {
		// Fail closed: em accepts a type-1 reference from a signed-on client,
		// so a hand-made page could select a patient without a literal DFN.
		// A browser ORWPT SELECT must name its patient as a type-0 literal.
		rs.refuse("patient", false)
		vistaWriteSecurity(w, vistaMsgSelectForm)
		return true
	}
	if isBound {
		if dfn != bound {
			rs.refuse("patient", false)
			vistaWriteSecurity(w, vistaMsgOtherPatient)
			return true
		}
		return false
	}
	rs.bindPatient(s, dfn)
	return false
}

// vistaFirstLiteralDFN returns a walked message's first parameter when it is
// a type-0 literal of a valid DFN, and whether it was.
func vistaFirstLiteralDFN(d *xwbDetail) (string, bool) {
	if d == nil || len(d.Params) == 0 {
		return "", false
	}
	p := d.Params[0]
	if p.Type != '0' {
		return "", false
	}
	dfn := string(p.Value)
	if !vistaValidDFN(dfn) {
		return "", false
	}
	return dfn, true
}

// vistaValidDFN reports whether v is a syntactic DFN: ^[1-9][0-9]{0,15}$.
func vistaValidDFN(v string) bool {
	if len(v) < 1 || len(v) > 16 || v[0] < '1' || v[0] > '9' {
		return false
	}
	for i := 1; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return false
		}
	}
	return true
}

// stripVistaCookie removes __Host-ov and the OV-* request headers from a
// request the site forwards anywhere but its vista routes; only the vista
// handler reads them. X-OV stays — it is not sensitive.
func stripVistaCookie(r *http.Request) {
	r.Header.Del("OV-Session")
	r.Header.Del("OV-Patient")
	r.Header.Del("OV-End")
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
