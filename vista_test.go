package janus

// Capability 11, vista: the handler, sessions, pool, and grammar against
// the fake em (vista_fakeem_test.go), served over real TLS so requests
// carry a completed handshake, their peer, and the local address
// (docs/20261010-232539-capability-vista.md).

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// --- the rig -------------------------------------------------------------------

// vistaRig is one route served over TLS (HTTP/2) to a fake em.
type vistaRig struct {
	t      *testing.T
	em     *fakeEm
	h      *Handler
	pool   *vistaPool
	srv    *httptest.Server
	c      *http.Client
	remote atomic.Value // the last request's RemoteAddr, as the server saw it
}

func newVistaRig(t *testing.T, logger *zap.Logger, edit func(*vistaLimits)) *vistaRig {
	t.Helper()
	em := newFakeEm(t)
	return newVistaRigTarget(t, em, "unix/"+em.Path(), logger, edit)
}

func newVistaRigTarget(t *testing.T, em *fakeEm, target string, logger *zap.Logger, edit func(*vistaLimits)) *vistaRig {
	t.Helper()
	if logger == nil {
		logger = zap.NewNop()
	}
	pool := newVistaPool(logger)
	h := &Handler{
		app:    &App{state: &janusState{vista: pool}},
		logger: logger,
		Vista:  []*VistaRoute{{Path: "/rpc", Target: target}},
	}
	if err := h.provisionVista(); err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(h.vistaRoutes[0].limits)
	}
	g := &vistaRig{t: t, em: em, h: h, pool: pool}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.remote.Store(r.RemoteAddr)
		_ = h.serveVista(w, r, h.vistaRoutes[0])
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	// Sessions end first, so no handler is left waiting on the em.
	t.Cleanup(pool.closeAll)
	g.srv, g.c = srv, srv.Client()
	return g
}

// vistaResp is a response read whole; err is a read error (an aborted
// response) or the request's own error, with code 0.
type vistaResp struct {
	code int
	h    http.Header
	body []byte
	err  error
}

func (g *vistaRig) do(body []byte, kv ...string) vistaResp {
	return g.doCtx(context.Background(), body, kv...)
}

func (g *vistaRig) doCtx(ctx context.Context, body []byte, kv ...string) vistaResp {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.srv.URL+"/rpc", bytes.NewReader(body))
	if err != nil {
		return vistaResp{err: err}
	}
	req.Header.Set("Content-Type", "application/x-xwb")
	for i := 0; i+1 < len(kv); i += 2 {
		req.Header.Set(kv[i], kv[i+1])
	}
	resp, err := g.c.Do(req)
	if err != nil {
		return vistaResp{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return vistaResp{code: resp.StatusCode, h: resp.Header, body: b, err: err}
}

// connect starts a session and returns its token.
func (g *vistaRig) connect() string {
	g.t.Helper()
	r := g.do(xwbTCPConnectMsg())
	if r.code != http.StatusOK || !bytes.Equal(r.body, emReplyAccept) || r.err != nil {
		g.t.Fatalf("TCPConnect: %d %q %v", r.code, r.body, r.err)
	}
	tok := vistaSetToken(r.h)
	if tok == "" {
		g.t.Fatalf("TCPConnect: no token in %q", r.h.Values("Set-Cookie"))
	}
	return tok
}

// status is the rig's one route's row on /1.0/vista.
func (g *vistaRig) status() vistaRouteStatus {
	for _, st := range g.pool.status() {
		return st
	}
	return vistaRouteStatus{}
}

func vistaWait(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// vistaSetToken is the token a response's Set-Cookie carries, or "".
func vistaSetToken(h http.Header) string {
	for _, v := range h.Values("Set-Cookie") {
		if rest, ok := strings.CutPrefix(v, vistaCookieName+"="); ok {
			if tok, _, _ := strings.Cut(rest, ";"); tok != "" {
				return tok
			}
		}
	}
	return ""
}

func bearer(tok string, kv ...string) []string {
	return append([]string{"Authorization", "Bearer " + tok}, kv...)
}

func cookie(tok string, kv ...string) []string {
	return append([]string{"Cookie", vistaCookieName + "=" + tok}, kv...)
}

func echoMsg(s string) []byte   { return xwbRPCMsg("ECHO", s) }
func echoReply(s string) []byte { return []byte("\x00\x00" + s + "\x04") }

var imHereMsg = xwbRPCMsg("XWB IM HERE")

func wantEnded(t *testing.T, what string, r vistaResp, code int, cause string) {
	t.Helper()
	if r.code != code || r.h.Get(vistaEndedHeader) != cause {
		t.Fatalf("%s: %d %s=%q, want %d %q (body %q, err %v)", what, r.code, vistaEndedHeader,
			r.h.Get(vistaEndedHeader), code, cause, r.body, r.err)
	}
}

func wantReply(t *testing.T, what string, r vistaResp, reply []byte) {
	t.Helper()
	if r.code != http.StatusOK || !bytes.Equal(r.body, reply) || r.err != nil {
		t.Fatalf("%s: %d %q (err %v), want 200 %q", what, r.code, r.body, r.err, reply)
	}
}

// vistaServeDirect calls serveVista without a server, reporting an abort.
func vistaServeDirect(h *Handler, r *http.Request) (rr *httptest.ResponseRecorder, aborted bool) {
	rr = httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			if v != http.ErrAbortHandler {
				panic(v)
			}
			aborted = true
		}
	}()
	_ = h.serveVista(rr, r, h.vistaRoutes[0])
	return rr, false
}

func vistaDirectReq(method string, body []byte, kv ...string) *http.Request {
	r := httptest.NewRequest(method, "https://vista.test/rpc", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/x-xwb")
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}
	return r
}

// --- 1. lifecycle ---------------------------------------------------------------

func TestVistaLifecycle(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	r := g.do(xwbTCPConnectMsg())
	wantReply(t, "TCPConnect", r, emReplyAccept)
	sc := r.h.Values("Set-Cookie")
	tok := vistaSetToken(r.h)
	if len(sc) != 1 || len(tok) != vistaTokenLen ||
		sc[0] != vistaCookieName+"="+tok+"; Path=/; Secure; HttpOnly; SameSite=Strict" {
		t.Fatalf("TCPConnect Set-Cookie: %q", sc)
	}
	if !vistaTokenWellFormed(tok) {
		t.Fatalf("token %q is not 43 base64url characters", tok)
	}
	for name, vals := range r.h {
		for _, v := range vals {
			if name != "Set-Cookie" && strings.Contains(v, tok) {
				t.Fatalf("header %s carries the token", name)
			}
		}
	}
	if r.h.Get("Content-Type") != "application/x-xwb" || r.h.Get("Cache-Control") != "no-store" {
		t.Fatalf("TCPConnect headers: %v", r.h)
	}

	g.em.WaitConns(t, 1)
	conn := g.em.Conns()[0]
	_, cport, _ := net.SplitHostPort(g.remote.Load().(string))
	_, sport, _ := net.SplitHostPort(g.srv.Listener.Addr().String())
	if want := "PROXY TCP4 127.0.0.1 127.0.0.1 " + cport + " " + sport; conn.Proxy() != want {
		t.Fatalf("PROXY line %q, want %q", conn.Proxy(), want)
	}

	r = g.do(imHereMsg, bearer(tok)...)
	wantReply(t, "IM HERE by Bearer", r, emReplyIMHere)
	if len(r.h.Values("Set-Cookie")) != 0 {
		t.Fatalf("IM HERE set a cookie: %q", r.h.Values("Set-Cookie"))
	}
	wantReply(t, "IM HERE by cookie and X-OV", g.do(imHereMsg, cookie(tok, "X-OV", "1")...), emReplyIMHere)
	if r := g.do(imHereMsg, cookie(tok)...); r.code != http.StatusForbidden {
		t.Fatalf("cookie without X-OV: %d, want 403", r.code)
	}

	r = g.do(xwbByeMsg(), cookie(tok, "X-OV", "1")...)
	wantReply(t, "#BYE#", r, emReplyBye)
	if sc := r.h.Values("Set-Cookie"); len(sc) != 1 || sc[0] != vistaClearCookie || !strings.Contains(sc[0], "Max-Age=0") {
		t.Fatalf("#BYE# Set-Cookie: %q, want the clearing cookie", sc)
	}
	r = g.do(imHereMsg, cookie(tok, "X-OV", "1")...)
	wantEnded(t, "after #BYE#", r, http.StatusUnauthorized, vistaEndBye)
	if sc := r.h.Values("Set-Cookie"); len(sc) != 1 || sc[0] != vistaClearCookie {
		t.Fatalf("401 to a cookie: Set-Cookie %q, want the clearing cookie", sc)
	}
	conn.WaitClosed(t)

	var got []string
	for _, m := range conn.Received() {
		got = append(got, m.Command+m.RPC)
	}
	if want := "TCPConnect,XWB IM HERE,XWB IM HERE,#BYE#"; strings.Join(got, ",") != want {
		t.Fatalf("the em received %q, want %q", got, want)
	}
	vistaWait(t, "the session's entry to go", func() bool { return g.status().Sessions == 0 })
	if st := g.status(); st.Started != 1 || st.Ended["bye"] != 1 || st.Requests != 3 {
		t.Fatalf("status: %+v", st)
	}
}

// The PROXY line names Caddy's client IP (port 0) only when trusted_proxies
// trusts the peer; an untrusted X-Forwarded-For is never read.
func TestVistaProxyLineClient(t *testing.T) {
	cases := []struct {
		name    string
		trusted bool
		want    string
	}{
		{"untrusted peer", false, "PROXY TCP4 192.0.2.1 127.0.0.1 1234 443"},
		{"trusted proxy", true, "PROXY TCP4 203.0.113.9 127.0.0.1 0 443"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newVistaRig(t, nil, nil)
			r := vistaDirectReq(http.MethodPost, xwbTCPConnectMsg(), "X-Forwarded-For", "198.51.100.7")
			ctx := context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443})
			ctx = context.WithValue(ctx, caddyhttp.VarsCtxKey, map[string]any{
				caddyhttp.TrustedProxyVarKey: tc.trusted,
				caddyhttp.ClientIPVarKey:     "203.0.113.9",
			})
			rr, _ := vistaServeDirect(g.h, r.WithContext(ctx))
			if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), emReplyAccept) {
				t.Fatalf("TCPConnect: %d %q", rr.Code, rr.Body.Bytes())
			}
			g.em.WaitConns(t, 1)
			if got := g.em.Conns()[0].Proxy(); got != tc.want {
				t.Fatalf("PROXY line %q, want %q", got, tc.want)
			}
		})
	}
}

// --- 2. admission ------------------------------------------------------------

func TestVistaAdmissionRows(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()
	same := "https://" + g.srv.Listener.Addr().String()
	bad4 := append([]byte(nil), imHereMsg...)
	bad4[8] = '1'
	random := vistaMintToken()

	cases := []struct {
		name   string
		method string
		body   []byte
		kv     []string
		code   int
		row    string
	}{
		{"GET", http.MethodGet, nil, bearer(tok), http.StatusMethodNotAllowed, "method"},
		{"foreign Origin", "", imHereMsg, bearer(tok, "Origin", "https://evil.example", "X-OV", "1"), http.StatusForbidden, "origin"},
		{"Origin null", "", imHereMsg, bearer(tok, "Origin", "null", "X-OV", "1"), http.StatusForbidden, "origin"},
		{"same Origin without X-OV", "", imHereMsg, bearer(tok, "Origin", same), http.StatusForbidden, "origin"},
		{"same Origin with X-OV", "", imHereMsg, bearer(tok, "Origin", same, "X-OV", "1"), http.StatusOK, ""},
		{"cookie without X-OV", "", imHereMsg, cookie(tok), http.StatusForbidden, "origin"},
		{"content type", "", imHereMsg, bearer(tok, "Content-Type", "text/plain"), http.StatusUnsupportedMediaType, "content_type"},
		{"not [XWB]", "", []byte("GET / HTTP/1.1\r\n"), bearer(tok), http.StatusBadRequest, "message"},
		{"fourth digit 1", "", bad4, bearer(tok), http.StatusBadRequest, "message"},
		{"malformed token", "", imHereMsg, bearer("short"), http.StatusBadRequest, "token"},
		{"two cookies", "", imHereMsg, []string{"Cookie", vistaCookieName + "=" + tok + "; " + vistaCookieName + "=" + random, "X-OV", "1"}, http.StatusBadRequest, "token"},
		{"cookie and Bearer", "", imHereMsg, cookie(tok, "Authorization", "Bearer "+tok, "X-OV", "1"), http.StatusBadRequest, "token"},
		{"Basic", "", imHereMsg, []string{"Authorization", "Basic YTpi"}, http.StatusBadRequest, "token"},
		{"no token", "", imHereMsg, nil, http.StatusUnauthorized, "unknown"},
		{"unknown token", "", imHereMsg, bearer(random), http.StatusUnauthorized, "unknown"},
	}
	for _, tc := range cases {
		before := g.status().Refused
		var r vistaResp
		start := time.Now()
		if tc.method != "" {
			req, _ := http.NewRequest(tc.method, g.srv.URL+"/rpc", nil)
			for i := 0; i+1 < len(tc.kv); i += 2 {
				req.Header.Set(tc.kv[i], tc.kv[i+1])
			}
			resp, err := g.c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			r = vistaResp{code: resp.StatusCode, h: resp.Header}
		} else {
			r = g.do(tc.body, tc.kv...)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s: took %v", tc.name, took)
		}
		if r.code != tc.code {
			t.Errorf("%s: %d, want %d (%q)", tc.name, r.code, tc.code, r.body)
			continue
		}
		if tc.code == http.StatusMethodNotAllowed && r.h.Get("Allow") != http.MethodPost {
			t.Errorf("%s: Allow %q", tc.name, r.h.Get("Allow"))
		}
		if tc.code == http.StatusUnauthorized && r.h.Get(vistaEndedHeader) != vistaEndUnknown {
			t.Errorf("%s: %s %q, want unknown", tc.name, vistaEndedHeader, r.h.Get(vistaEndedHeader))
		}
		if tc.row != "" {
			if got := g.status().Refused[tc.row]; got != before[tc.row]+1 {
				t.Errorf("%s: refused[%s] %d, want %d", tc.name, tc.row, got, before[tc.row]+1)
			}
		}
	}
	// The em saw only the TCPConnect and the one admitted IM HERE.
	if got := len(g.em.Conns()[0].Received()); got != 2 {
		t.Fatalf("the em received %d messages, want 2", got)
	}

	// Row 6's size limit, read only to the limit.
	small := newVistaRig(t, nil, func(l *vistaLimits) { l.maxRequest = 1024 })
	stok := small.connect()
	if r := small.do(echoMsg(strings.Repeat("a", 2000)), bearer(stok)...); r.code != http.StatusBadRequest {
		t.Fatalf("oversize body: %d, want 400", r.code)
	}
	wantReply(t, "a body within the limit", small.do(echoMsg("ok"), bearer(stok)...), echoReply("ok"))
}

// Rows 1–8 judged first match wins, including the rows a test server
// cannot produce: no TLS, early data, and Early-Data from a trusted proxy.
func TestVistaAdmissionOrder(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	noTLS := func(r *http.Request) { r.TLS = nil }
	early := func(r *http.Request) { r.TLS = &tls.ConnectionState{HandshakeComplete: false} }
	trusted := func(r *http.Request) {
		*r = *r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey,
			map[string]any{caddyhttp.TrustedProxyVarKey: true}))
	}
	cases := []struct {
		name   string
		method string
		body   []byte
		kv     []string
		edit   func(*http.Request)
		code   int
		row    string
	}{
		{"GET without TLS", http.MethodGet, nil, nil, noTLS, http.StatusMethodNotAllowed, "method"},
		{"no TLS", http.MethodPost, imHereMsg, []string{"Origin", "https://evil.example"}, noTLS, http.StatusMisdirectedRequest, "no_tls"},
		{"TLS early data", http.MethodPost, imHereMsg, []string{"Origin", "https://evil.example"}, early, http.StatusTooEarly, "early_data"},
		{"Early-Data from a trusted proxy", http.MethodPost, imHereMsg, []string{"Early-Data", "1"}, trusted, http.StatusTooEarly, "early_data"},
		{"Early-Data from an untrusted peer", http.MethodPost, imHereMsg, []string{"Early-Data", "1"}, nil, http.StatusUnauthorized, "unknown"},
		{"foreign Origin before content type", http.MethodPost, []byte("x"), []string{"Origin", "https://evil.example", "Content-Type", "text/plain"}, nil, http.StatusForbidden, "origin"},
		{"content type before the body", http.MethodPost, []byte("x"), []string{"Content-Type", "text/plain"}, nil, http.StatusUnsupportedMediaType, "content_type"},
		{"the body before the token", http.MethodPost, []byte("x"), []string{"Authorization", "Basic YTpi"}, nil, http.StatusBadRequest, "message"},
		{"the token before TCPConnect", http.MethodPost, xwbTCPConnectMsg(), []string{"Authorization", "Bearer short"}, nil, http.StatusBadRequest, "token"},
	}
	key := vistaRouteKey("vista.test", "/rpc")
	for _, tc := range cases {
		r := vistaDirectReq(tc.method, tc.body, tc.kv...)
		if tc.edit != nil {
			tc.edit(r)
		}
		before := g.pool.status()[key].Refused[tc.row]
		rr, _ := vistaServeDirect(g.h, r)
		if rr.Code != tc.code {
			t.Errorf("%s: %d, want %d", tc.name, rr.Code, tc.code)
			continue
		}
		if got := g.pool.status()[key].Refused[tc.row]; got != before+1 {
			t.Errorf("%s: refused[%s] %d, want %d", tc.name, tc.row, got, before+1)
		}
	}
	if n := len(g.em.Conns()); n != 0 {
		t.Fatalf("refused requests dialed the em %d times", n)
	}
}

// --- 3–4. sign-on and rotation --------------------------------------------------

func vistaStatusJSON(t *testing.T, pool *vistaPool) map[string]map[string]any {
	t.Helper()
	a := &App{state: &janusState{vista: pool}}
	rr := httptest.NewRecorder()
	a.handleVistaState(rr, httptest.NewRequest(http.MethodGet, "/1.0/vista", nil))
	var out struct {
		Routes map[string]map[string]any `json:"routes"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("/1.0/vista: %v: %s", err, rr.Body.Bytes())
	}
	return out.Routes
}

func TestVistaStatusEmpty(t *testing.T) {
	rr := httptest.NewRecorder()
	(&App{}).handleVistaState(rr, httptest.NewRequest(http.MethodGet, "/1.0/vista", nil))
	if strings.TrimSpace(rr.Body.String()) != `{"routes":{}}` {
		t.Fatalf("no state: %s", rr.Body.String())
	}
	rr = httptest.NewRecorder()
	(&App{state: &janusState{vista: newVistaPool(zap.NewNop())}}).handleVistaState(rr, httptest.NewRequest(http.MethodGet, "/1.0/vista", nil))
	if strings.TrimSpace(rr.Body.String()) != `{"routes":{}}` {
		t.Fatalf("no routes: %s", rr.Body.String())
	}
}

func TestVistaSignonRotates(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()

	r := g.do(xwbRPCMsg("XUS AV CODE", " BAD;CODE "), bearer(tok)...)
	wantReply(t, "a bad code", r, emReplyAVBad)
	if vistaSetToken(r.h) != "" {
		t.Fatal("a bad code rotated the token")
	}
	if st := g.status(); st.Unsigned != 1 || st.SignedOn != 0 {
		t.Fatalf("after a bad code: unsigned %d signed_on %d", st.Unsigned, st.SignedOn)
	}

	r = g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), bearer(tok)...)
	wantReply(t, "a good code", r, emReplyAVGood)
	fresh := vistaSetToken(r.h)
	if fresh == "" || fresh == tok || len(r.h.Values("Set-Cookie")) != 1 {
		t.Fatalf("sign-on Set-Cookie %q: want one new token", r.h.Values("Set-Cookie"))
	}
	wantEnded(t, "the old token", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndRotated)
	wantReply(t, "the new token", g.do(xwbRPCMsg("ORWU USERINFO"), bearer(fresh)...), emReplyUserInfo)

	routes := vistaStatusJSON(t, g.pool)
	if len(routes) != 1 {
		t.Fatalf("/1.0/vista routes: %v", routes)
	}
	for _, row := range routes {
		if row["signed_on"] != float64(1) || row["unsigned"] != float64(0) || row["sessions"] != float64(1) {
			t.Fatalf("/1.0/vista row: %v", row)
		}
		raw, _ := json.Marshal(row)
		if strings.Contains(string(raw), fresh) || strings.Contains(string(raw), "127.0.0.1") {
			t.Fatalf("/1.0/vista publishes a token or client: %s", raw)
		}
	}

	// Signed-on is one-way: a later good code mints nothing.
	r = g.do(xwbRPCMsg("XUS AV CODE", "GOOD;CODE"), bearer(fresh)...)
	if r.code != http.StatusOK || vistaSetToken(r.h) != "" {
		t.Fatalf("a second sign-on: %d %q", r.code, r.h.Values("Set-Cookie"))
	}
}

func TestVistaExpiredThenCVC(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()
	r := g.do(xwbRPCMsg("XUS AV CODE", "EXPIRED;CODE"), bearer(tok)...)
	wantReply(t, "an expired code", r, emReplyAVExpired)
	if vistaSetToken(r.h) != "" {
		t.Fatal("an expired code rotated the token")
	}
	r = g.do(xwbRPCMsg("XUS CVC", "NEW;CODE"), bearer(tok)...)
	wantReply(t, "XUS CVC", r, emReplyCVC)
	if vistaSetToken(r.h) == "" {
		t.Fatal("XUS CVC after an expired code did not rotate the token")
	}
	if st := g.status(); st.SignedOn != 1 || st.Unsigned != 0 {
		t.Fatalf("status: %+v", st)
	}

	// XUS CVC with no expired step signs nothing on.
	tok2 := g.connect()
	r = g.do(xwbRPCMsg("XUS CVC", "NEW;CODE"), bearer(tok2)...)
	wantReply(t, "XUS CVC alone", r, emReplyCVC)
	if vistaSetToken(r.h) != "" {
		t.Fatal("XUS CVC without an expired code rotated the token")
	}
	if st := g.status(); st.SignedOn != 1 || st.Unsigned != 1 {
		t.Fatalf("status: %+v", st)
	}
}

// --- 5. signon_timeout ----------------------------------------------------------

func TestVistaSignonTimeout(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) {
		l.signonTimeout = 300 * time.Millisecond
		l.maxUnsigned = 1
	})
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" && fakeEmParam(m, 0) == "slow" {
			return []fakeEmAction{{Delay: 600 * time.Millisecond}, {Write: echoReply("slow")}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	vistaWait(t, "the unsigned session to end", func() bool { return g.status().Sessions == 0 })
	g.em.Conns()[0].WaitClosed(t)
	wantEnded(t, "after the deadline", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndSignonTimeout)

	// The unsigned slot is free again; a deadline falling during a request
	// ends the session only once that request's reply has ended.
	tok = g.connect()
	wantReply(t, "a request across the deadline", g.do(echoMsg("slow"), bearer(tok)...), echoReply("slow"))
	vistaWait(t, "the session to end after its reply", func() bool { return g.status().Sessions == 0 })
	wantEnded(t, "after the reply", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndSignonTimeout)
	if st := g.status(); st.Ended["signon_timeout"] != 2 {
		t.Fatalf("ended: %v", st.Ended)
	}
}

// A deadline that falls during the reply that signs the session on ends
// nothing: the session is signed on, its token rotated.
func TestVistaSignonAcrossTheDeadline(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) { l.signonTimeout = 300 * time.Millisecond })
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "XUS AV CODE" {
			return append([]fakeEmAction{{Delay: 600 * time.Millisecond}}, fakeEmDefault(c, m)...)
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	r := g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), bearer(tok)...)
	wantReply(t, "sign-on across the deadline", r, emReplyAVGood)
	fresh := vistaSetToken(r.h)
	if fresh == "" || fresh == tok {
		t.Fatalf("no rotated token: %q", fresh)
	}
	wantReply(t, "after the deadline", g.do(imHereMsg, bearer(fresh)...), emReplyIMHere)
	if st := g.status(); st.Sessions != 1 || st.SignedOn != 1 || st.Ended["signon_timeout"] != 0 {
		t.Fatalf("status: %+v", st)
	}
}

// --- 6. caps ----------------------------------------------------------------------

func TestVistaCaps(t *testing.T) {
	cases := []struct {
		name  string
		edit  func(*vistaLimits)
		admit int
		row   string
	}{
		{"route", func(l *vistaLimits) { l.maxSessions, l.maxUnsigned, l.perClient, l.perClientUnsigned = 1, 1, 1, 1 }, 1, "cap_route"},
		{"unsigned", func(l *vistaLimits) { l.maxSessions, l.maxUnsigned = 4, 1 }, 1, "cap_unsigned"},
		{"per client", func(l *vistaLimits) { l.maxSessions, l.maxUnsigned, l.perClient, l.perClientUnsigned = 4, 4, 1, 1 }, 1, "cap_client"},
		{"per client unsigned", func(l *vistaLimits) { l.maxSessions, l.maxUnsigned, l.perClient, l.perClientUnsigned = 4, 4, 4, 1 }, 1, "cap_client_unsigned"},
		{"exempt skips per client, not the route", func(l *vistaLimits) {
			l.maxSessions, l.maxUnsigned, l.perClient, l.perClientUnsigned = 3, 3, 1, 1
			l.perClientExempt = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}
		}, 3, "cap_route"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newVistaRig(t, nil, tc.edit)
			var toks []string
			for i := 0; i < tc.admit; i++ {
				toks = append(toks, g.connect())
			}
			r := g.do(xwbTCPConnectMsg())
			if r.code != http.StatusServiceUnavailable || r.h.Get("Retry-After") != "5" || vistaSetToken(r.h) != "" {
				t.Fatalf("over the cap: %d Retry-After %q", r.code, r.h.Get("Retry-After"))
			}
			if st := g.status(); st.Refused[tc.row] != 1 || st.Sessions != tc.admit {
				t.Fatalf("status: sessions %d refused %v", st.Sessions, st.Refused)
			}
			if len(g.em.Conns()) != tc.admit {
				t.Fatalf("a refused TCPConnect dialed the em")
			}
			if tc.row == "cap_unsigned" {
				// Signing on frees the unsigned slot.
				wantReply(t, "sign-on", g.do(xwbRPCMsg("XUS AV CODE", "GOOD;CODE"), bearer(toks[0])...), emReplyAVGood)
				g.connect()
			}
		})
	}
}

// --- 7. serial, FIFO, never interleaved; the queue cap -----------------------

func TestVistaSerialUnderConcurrency(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) { l.queue = 32 })
	var overlap atomic.Int32
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" {
			// Another message already on the stream means Janus wrote it
			// before this one's reply: interleaving.
			_ = c.c.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if _, err := c.r.Peek(1); err == nil {
				overlap.Add(1)
			}
			_ = c.c.SetReadDeadline(time.Time{})
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	const n = 20
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("p%02d-%s", i, strings.Repeat("x", i*37))
			r := g.do(echoMsg(p), bearer(tok)...)
			if r.code != http.StatusOK || !bytes.Equal(r.body, echoReply(p)) {
				errs <- fmt.Sprintf("request %d: %d %q", i, r.code, r.body)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	if overlap.Load() != 0 {
		t.Fatalf("%d messages reached the em before the previous reply ended", overlap.Load())
	}
	if got := len(g.em.Conns()[0].Received()); got != n+1 {
		t.Fatalf("the em received %d messages, want %d", got, n+1)
	}
}

func TestVistaQueueFIFO(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" && fakeEmParam(m, 0) == "q0" {
			return []fakeEmAction{{Delay: 300 * time.Millisecond}, {Write: echoReply("q0")}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := fmt.Sprintf("q%d", i)
			if r := g.do(echoMsg(p), bearer(tok)...); r.code != http.StatusOK || !bytes.Equal(r.body, echoReply(p)) {
				t.Errorf("%s: %d %q", p, r.code, r.body)
			}
		}(i)
		if i == 0 {
			vistaWait(t, "the first request to run", func() bool { return g.status().Running == 1 })
		} else {
			vistaWait(t, "a request to queue", func() bool { return g.status().Queued == int64(i) })
		}
	}
	wg.Wait()
	var order []string
	for _, m := range g.em.Conns()[0].Received()[1:] {
		order = append(order, fakeEmParam(m, 0))
	}
	if want := "q0,q1,q2,q3,q4,q5"; strings.Join(order, ",") != want {
		t.Fatalf("the em ran %v, want %s", order, want)
	}
}

func TestVistaQueueCap(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) { l.queue = 1 })
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" && fakeEmParam(m, 0) == "slow" {
			return []fakeEmAction{{Delay: 400 * time.Millisecond}, {Write: echoReply("slow")}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	var wg sync.WaitGroup
	results := make([]vistaResp, 2)
	wg.Add(1)
	go func() { defer wg.Done(); results[0] = g.do(echoMsg("slow"), bearer(tok)...) }()
	vistaWait(t, "the first request to run", func() bool { return g.status().Running == 1 })
	wg.Add(1)
	go func() { defer wg.Done(); results[1] = g.do(echoMsg("next"), bearer(tok)...) }()
	vistaWait(t, "the second request to queue", func() bool { return g.status().Queued == 1 })
	r := g.do(echoMsg("over"), bearer(tok)...)
	if r.code != http.StatusServiceUnavailable || r.h.Get("Retry-After") != "1" {
		t.Fatalf("over the queue: %d Retry-After %q", r.code, r.h.Get("Retry-After"))
	}
	wg.Wait()
	wantReply(t, "the running request", results[0], echoReply("slow"))
	wantReply(t, "the queued request", results[1], echoReply("next"))
	if st := g.status(); st.Refused["queue"] != 1 {
		t.Fatalf("refused: %v", st.Refused)
	}
	for _, m := range g.em.Conns()[0].Received() {
		if fakeEmParam(m, 0) == "over" {
			t.Fatal("the refused request reached the em")
		}
	}
}

// A queued request whose client goes away is never sent.
func TestVistaQueuedClientGoneNeverSent(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" && fakeEmParam(m, 0) == "slow" {
			return []fakeEmAction{{Delay: 400 * time.Millisecond}, {Write: echoReply("slow")}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	done := make(chan vistaResp, 1)
	go func() { done <- g.do(echoMsg("slow"), bearer(tok)...) }()
	vistaWait(t, "the first request to run", func() bool { return g.status().Running == 1 })
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{})
	go func() { defer close(gone); g.doCtx(ctx, echoMsg("gone"), bearer(tok)...) }()
	vistaWait(t, "the second request to queue", func() bool { return g.status().Queued == 1 })
	cancel()
	<-gone
	wantReply(t, "the running request", <-done, echoReply("slow"))
	wantReply(t, "the next request", g.do(echoMsg("after"), bearer(tok)...), echoReply("after"))
	for _, m := range g.em.Conns()[0].Received() {
		if fakeEmParam(m, 0) == "gone" {
			t.Fatal("a request whose client left the queue reached the em")
		}
	}
	if st := g.status(); st.ClientGone != 1 {
		t.Fatalf("client_gone %d, want 1", st.ClientGone)
	}
}

// --- 8. client gone mid-reply ---------------------------------------------------

func TestVistaClientGoneMidReplyStaysAligned(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "BIG" {
			pieces := fakeEmPieces(fakeEmBigReply(2000000))
			pieces[0].Delay = 300 * time.Millisecond
			return pieces
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, g.srv.URL+"/rpc", bytes.NewReader(xwbRPCMsg("BIG", "2000000")))
	req.Header.Set("Content-Type", "application/x-xwb")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := g.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(resp.Body, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()

	wantReply(t, "the next request", g.do(echoMsg("aligned"), bearer(tok)...), echoReply("aligned"))
	st := g.status()
	if st.ClientGone != 1 || st.Sessions != 1 || st.Aborted != 0 {
		t.Fatalf("status: client_gone %d sessions %d aborted %d", st.ClientGone, st.Sessions, st.Aborted)
	}
}

// --- 9–11. idle close, unsolicited bytes, leftover bytes ------------------------

func TestVistaIdleCloseNoticed(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()
	g.em.Conns()[0].Close()
	start := time.Now()
	vistaWait(t, "the closed session to go", func() bool { return g.status().Sessions == 0 })
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the close was noticed after %v", took)
	}
	wantEnded(t, "after the close", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndClosed)
	if st := g.status(); st.Ended["closed"] != 1 {
		t.Fatalf("ended: %v", st.Ended)
	}
}

func TestVistaUnsolicitedBytesEndTheSession(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()
	conn := g.em.Conns()[0]
	conn.Send([]byte("\x00\x00stray\x04"))
	vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
	conn.WaitClosed(t)
	wantEnded(t, "after stray bytes", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndProtocol)
}

func TestVistaLeftoverBytesEndTheSession(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "ECHO" && fakeEmParam(m, 0) == "extra" {
			return fakeEmWrite(append(echoReply("extra"), "\x00\x00next\x04"...))
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	wantReply(t, "the reply before the leftover", g.do(echoMsg("extra"), bearer(tok)...), echoReply("extra"))
	vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
	wantEnded(t, "after leftover bytes", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndProtocol)
}

// --- 12–13. rpc_timeout and max_reply ------------------------------------------

func TestVistaRPCTimeout(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) { l.rpcTimeout = 300 * time.Millisecond })
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		switch fakeEmParam(m, 0) {
		case "stall":
			return []fakeEmAction{{Stall: true}}
		case "partial":
			return []fakeEmAction{{Write: []byte("\x00\x00part")}, {Stall: true}}
		}
		return fakeEmDefault(c, m)
	})

	tok := g.connect()
	r := g.do(echoMsg("stall"), cookie(tok, "X-OV", "1")...)
	wantEnded(t, "before the first byte", r, http.StatusGatewayTimeout, vistaEndRPCTimeout)
	if sc := r.h.Values("Set-Cookie"); len(sc) != 1 || sc[0] != vistaClearCookie {
		t.Fatalf("a session-ending 504 to a cookie: Set-Cookie %q", sc)
	}
	g.em.Conns()[0].WaitClosed(t)

	tok = g.connect()
	r = g.do(echoMsg("partial"), bearer(tok)...)
	if r.code != http.StatusOK || r.err == nil {
		t.Fatalf("after the first byte: %d %q err %v, want an aborted 200", r.code, r.body, r.err)
	}
	g.em.Conns()[1].WaitClosed(t)
	wantEnded(t, "after the abort", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndRPCTimeout)
	if st := g.status(); st.Ended["rpc_timeout"] != 2 || st.Aborted != 1 {
		t.Fatalf("status: ended %v aborted %d", st.Ended, st.Aborted)
	}
}

func TestVistaMaxReply(t *testing.T) {
	g := newVistaRig(t, nil, func(l *vistaLimits) { l.maxReply = 2048 })
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.RPC == "BIG" {
			b := fakeEmBigReply(100000)
			return []fakeEmAction{{Write: b[:1000], Delay: 100 * time.Millisecond}, {Write: b[1000:]}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	r := g.do(xwbRPCMsg("BIG", "100000"), bearer(tok)...)
	if r.code != http.StatusOK || r.err == nil || len(r.body) > 2048 {
		t.Fatalf("over max_reply: %d (%d bytes) err %v, want an aborted 200", r.code, len(r.body), r.err)
	}
	g.em.Conns()[0].WaitClosed(t)
	wantEnded(t, "after the abort", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndReplyTooLarge)

	// A reply that passes the limit in its first read is refused before
	// its 200: a 502 naming the cause.
	g.em.Script(fakeEmDefault)
	tok = g.connect()
	r = g.do(xwbRPCMsg("BIG", "100000"), bearer(tok)...)
	if r.code == http.StatusBadGateway {
		wantEnded(t, "over max_reply at once", r, http.StatusBadGateway, vistaEndReplyTooLarge)
	} else if r.code != http.StatusOK || r.err == nil {
		t.Fatalf("over max_reply at once: %d err %v", r.code, r.err)
	}
	if st := g.status(); st.Ended["reply_too_large"] != 2 {
		t.Fatalf("ended: %v", st.Ended)
	}
}

// --- 14–17. reject, replaced, M error, dial failure ------------------------------

func TestVistaTCPConnectReject(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	r := g.do(xwbTCPConnectNamedMsg("reject"))
	wantReply(t, "a rejected TCPConnect", r, emReplyReject)
	if len(r.h.Values("Set-Cookie")) != 0 {
		t.Fatalf("reject set a cookie: %q", r.h.Values("Set-Cookie"))
	}
	g.em.Conns()[0].WaitClosed(t)
	vistaWait(t, "the slot to be released", func() bool { return g.status().Sessions == 0 })
	st := g.status()
	if st.Rejected != 1 || st.Started != 0 || st.Unsigned != 0 {
		t.Fatalf("status: %+v", st)
	}
	for cause, n := range st.Ended {
		if n != 0 {
			t.Fatalf("a rejected TCPConnect counted as a session ended %s", cause)
		}
	}
}

func TestVistaTCPConnectReplaces(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	old := g.connect()
	r := g.do(xwbTCPConnectMsg(), bearer(old)...)
	wantReply(t, "TCPConnect with a live token", r, emReplyAccept)
	fresh := vistaSetToken(r.h)
	if fresh == "" || fresh == old {
		t.Fatalf("no new token: %q", r.h.Values("Set-Cookie"))
	}
	g.em.Conns()[0].WaitClosed(t)
	wantEnded(t, "the replaced token", g.do(imHereMsg, bearer(old)...), http.StatusUnauthorized, vistaEndReplaced)
	wantReply(t, "the new session", g.do(imHereMsg, bearer(fresh)...), emReplyIMHere)
	if st := g.status(); st.Sessions != 1 || st.Ended["replaced"] != 1 || st.Started != 2 {
		t.Fatalf("status: %+v", st)
	}
}

func TestVistaMErrorWhole(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	tok := g.connect()
	wantReply(t, "an M error", g.do(xwbRPCMsg("MERR"), bearer(tok)...), emReplyMErr)
	wantReply(t, "the next request", g.do(echoMsg("after"), bearer(tok)...), echoReply("after"))
}

func TestVistaDialFailure(t *testing.T) {
	g := newVistaRigTarget(t, nil, "unix/"+filepath.Join(t.TempDir(), "absent.sock"), nil,
		func(l *vistaLimits) { l.maxUnsigned = 1 })
	for i := 0; i < 2; i++ {
		r := g.do(xwbTCPConnectMsg())
		if r.code != http.StatusBadGateway || vistaSetToken(r.h) != "" {
			t.Fatalf("attempt %d: %d, want 502", i, r.code)
		}
	}
	if st := g.status(); st.Sessions != 0 || st.Unsigned != 0 || st.Refused["dial"] != 2 || st.Refused["cap_unsigned"] != 0 {
		t.Fatalf("status: %+v", st)
	}
}

// --- 18. reload lifecycle -------------------------------------------------------

func TestVistaCommitRemovesRoute(t *testing.T) {
	core, logs := observer.New(zapcore.WarnLevel)
	g := newVistaRig(t, zap.New(core), nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		switch fakeEmParam(m, 0) {
		case "stall":
			return []fakeEmAction{{Stall: true}}
		case "partial":
			return []fakeEmAction{{Write: []byte("\x00\x00part")}, {Stall: true}}
		}
		return fakeEmDefault(c, m)
	})
	idle, before, after := g.connect(), g.connect(), g.connect()

	// One request running before its first byte, one after it.
	early := make(chan vistaResp, 1)
	go func() { early <- g.do(echoMsg("stall"), bearer(before)...) }()
	req, _ := http.NewRequest(http.MethodPost, g.srv.URL+"/rpc", bytes.NewReader(echoMsg("partial")))
	req.Header.Set("Content-Type", "application/x-xwb")
	req.Header.Set("Authorization", "Bearer "+after)
	resp, err := g.c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	vistaWait(t, "both requests to run", func() bool { return g.status().Running == 2 })

	g.pool.commit(&App{}, map[string]bool{"other.test/rpc": true})
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Fatal("a running request with its 200 out was not aborted")
	}
	wantEnded(t, "a running request before its first byte", <-early, http.StatusBadGateway, vistaEndRouteRemoved)
	for _, c := range g.em.Conns() {
		c.WaitClosed(t)
	}
	if n := len(g.pool.status()); n != 0 {
		t.Fatalf("the removed route is still in the pool (%d routes)", n)
	}
	warns := logs.FilterMessage("janus vista: a reload removed a route; its sessions ended").All()
	if len(warns) != 1 || warns[0].ContextMap()["sessions"] != int64(3) {
		t.Fatalf("warn lines: %v", warns)
	}
	_ = idle
}

func TestVistaRetire(t *testing.T) {
	p := newVistaPool(zap.NewNop())
	served, staged := &App{}, &App{}
	p.route("a.test/rpc", "unix//a", staged)
	p.route("b.test/rpc", "unix//b", staged)
	p.commit(served, map[string]bool{"a.test/rpc": true})
	p.route("c.test/rpc", "unix//c", staged)
	p.retire(served) // the serving generation: nothing
	if len(p.status()) != 2 {
		t.Fatalf("retiring the serving generation removed routes: %v", p.status())
	}
	p.retire(staged)
	st := p.status()
	if _, ok := st["a.test/rpc"]; !ok || len(st) != 1 {
		t.Fatalf("after retire: %v", st)
	}

	// A retired route's sessions end and their streams close.
	g := newVistaRig(t, nil, nil)
	g.connect()
	g.pool.mu.Lock()
	g.pool.committed, g.pool.gen = map[string]bool{}, &App{}
	g.pool.mu.Unlock()
	g.pool.retire(g.h.app)
	g.em.Conns()[0].WaitClosed(t)
	if len(g.pool.status()) != 0 {
		t.Fatal("the retired route is still in the pool")
	}
}

// --- 19. routing through Handler.ServeHTTP ------------------------------------

func vistaAuthHandler(t *testing.T, gates map[string][]string, route *VistaRoute) (*authHarness, error) {
	t.Helper()
	ah := newAuthHarnessGates(t, aliceUsers(t), 0, gates)
	ah.h.app = &App{state: &janusState{vista: newVistaPool(zap.NewNop())}}
	ah.h.authExactHosts = map[string]struct{}{"vista.test": {}}
	ah.h.Vista = []*VistaRoute{route}
	return ah, ah.h.provisionVista()
}

func TestVistaRoutingUnderAuth(t *testing.T) {
	em := newFakeEm(t)
	target := "unix/" + em.Path()
	ah, err := vistaAuthHandler(t, map[string][]string{"/": {"alice"}}, &VistaRoute{Path: "/rpc", Target: target, AuthExempt: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ah.h.app.state.vista.closeAll)

	// The exempt route is served without a session, before the wall.
	rr := serveSite(t, ah.h, vistaDirectReq(http.MethodPost, imHereMsg))
	if rr.Code != http.StatusUnauthorized || rr.Header().Get(vistaEndedHeader) != vistaEndUnknown {
		t.Fatalf("exempt route: %d %s=%q", rr.Code, vistaEndedHeader, rr.Header().Get(vistaEndedHeader))
	}
	r := vistaDirectReq(http.MethodPost, xwbTCPConnectMsg())
	r = r.WithContext(context.WithValue(r.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443}))
	rr = serveSite(t, ah.h, r)
	if rr.Code != http.StatusOK || !bytes.Equal(rr.Body.Bytes(), emReplyAccept) || vistaSetToken(rr.Header()) == "" {
		t.Fatalf("exempt TCPConnect: %d %q", rr.Code, rr.Body.Bytes())
	}

	// Every other spelling is not the route: the gate answers.
	for _, target := range []string{"/RPC", "/rpc/", "/%72pc", "/rpc/.", "/rpc;x"} {
		r := httptest.NewRequest(http.MethodPost, "https://vista.test"+target, bytes.NewReader(imHereMsg))
		r.Header.Set("Content-Type", "application/x-xwb")
		rr := serveSite(t, ah.h, r)
		if rr.Code != http.StatusUnauthorized || rr.Header().Get(vistaEndedHeader) != "" {
			t.Errorf("%s: %d %s=%q, want the gate's 401", target, rr.Code, vistaEndedHeader, rr.Header().Get(vistaEndedHeader))
		}
	}

	// An exact gate puts the route behind the wall.
	ah, err = vistaAuthHandler(t, map[string][]string{"/rpc": {"alice"}}, &VistaRoute{Path: "/rpc", Target: target})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ah.h.app.state.vista.closeAll)
	if !ah.h.vistaRoutes[0].limits.gated {
		t.Fatal("the exact gate did not mark the route gated")
	}
	rr = serveSite(t, ah.h, vistaDirectReq(http.MethodPost, imHereMsg))
	if rr.Code != http.StatusUnauthorized || rr.Header().Get(vistaEndedHeader) != "" {
		t.Fatalf("gated route without a login: %d %s=%q, want the gate's 401", rr.Code, vistaEndedHeader, rr.Header().Get(vistaEndedHeader))
	}
	session := ah.loginDoor(t, "vista.test", "/rpc/auth", "alice", "open-sesame")
	r = vistaDirectReq(http.MethodPost, imHereMsg)
	r.AddCookie(&http.Cookie{Name: authCookieName, Value: session})
	rr = serveSite(t, ah.h, r)
	if rr.Code != http.StatusUnauthorized || rr.Header().Get(vistaEndedHeader) != vistaEndUnknown {
		t.Fatalf("gated route past the wall: %d %s=%q, want vista's 401", rr.Code, vistaEndedHeader, rr.Header().Get(vistaEndedHeader))
	}
}

func TestVistaProvisionStance(t *testing.T) {
	route := func(path string, exempt bool) *VistaRoute {
		return &VistaRoute{Path: path, Target: "unix//run/em.sock", AuthExempt: exempt}
	}
	cases := []struct {
		name  string
		gates map[string][]string
		route *VistaRoute
		want  string
	}{
		{"covering gate without exempt", map[string][]string{"/": {"alice"}}, route("/rpc", false), "lies under gate /; add `auth exempt`"},
		{"exempt with no covering gate", map[string][]string{"/admin": {"alice"}}, route("/rpc", true), "`auth exempt` with no gate covering the route"},
		{"exact gate and exempt", map[string][]string{"/rpc": {"alice"}}, route("/rpc", true), "contradicts gate /rpc/"},
		{"the login door", map[string][]string{"/": {"alice"}}, route("/auth", true), "login door"},
		{"/ping", map[string][]string{"/": {"alice"}}, route("/ping", true), "/ping's"},
		{"exact gate", map[string][]string{"/rpc": {"alice"}}, route("/rpc", false), ""},
		{"covering gate with exempt", map[string][]string{"/": {"alice"}}, route("/rpc", true), ""},
		{"an unrelated gate", map[string][]string{"/admin": {"alice"}}, route("/rpc", false), ""},
	}
	for _, tc := range cases {
		_, err := vistaAuthHandler(t, tc.gates, tc.route)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
	// A site without auth refuses `auth exempt`.
	h := &Handler{Vista: []*VistaRoute{route("/rpc", true)}}
	if err := h.provisionVista(); err == nil || !strings.Contains(err.Error(), "site without auth") {
		t.Fatalf("exempt without auth: %v", err)
	}
	h = &Handler{Vista: []*VistaRoute{route(browseAssetPrefix+"x", false)}}
	if err := h.provisionVista(); err == nil || !strings.Contains(err.Error(), "asset prefix") {
		t.Fatalf("under the browse asset prefix: %v", err)
	}
}

func TestVistaRouteForIsByteExact(t *testing.T) {
	h := &Handler{Vista: []*VistaRoute{{Path: "/rpc", Target: "unix//run/em.sock"}}}
	if err := h.provisionVista(); err != nil {
		t.Fatal(err)
	}
	if h.vistaRouteFor("/rpc") == nil {
		t.Fatal("/rpc is the route")
	}
	for _, p := range []string{"/RPC", "/rpc/", "/rpc/.", "/%72pc", "//rpc", "/rpc;x", "/rp"} {
		if h.vistaRouteFor(p) != nil {
			t.Errorf("%s matched the route", p)
		}
	}
}

// --- 20. cookie isolation -----------------------------------------------------

func TestVistaCookieIsolation(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a=1; " + vistaCookieName + "=tok; b=2", "a=1; b=2"},
		{vistaCookieName + "=tok", ""},
		{"a=1;b=2", "a=1;b=2"}, // untouched when there is no session cookie
		{"__Host-janus=s; " + vistaCookieName + "=tok", "__Host-janus=s"},
	}
	for _, tc := range cases {
		r := httptest.NewRequest(http.MethodGet, "https://vista.test/", nil)
		r.Header.Set("Cookie", tc.in)
		stripVistaCookie(r)
		if got := r.Header.Get("Cookie"); got != tc.want {
			t.Errorf("%q: %q, want %q", tc.in, got, tc.want)
		}
	}

	rr := httptest.NewRecorder()
	w := &vistaRespStrip{ResponseWriter: rr}
	w.Header().Add("Set-Cookie", vistaCookieName+"=planted; Path=/; Secure")
	w.Header().Add("Set-Cookie", "app=1; Path=/")
	w.WriteHeader(http.StatusOK)
	if got := rr.Result().Header.Values("Set-Cookie"); len(got) != 1 || got[0] != "app=1; Path=/" {
		t.Fatalf("upstream Set-Cookie: %q", got)
	}
	rr = httptest.NewRecorder()
	w = &vistaRespStrip{ResponseWriter: rr}
	w.Header().Add("Set-Cookie", vistaCookieName+"=planted")
	_, _ = w.Write([]byte("body"))
	if got := rr.Result().Header.Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("an implicit WriteHeader kept %q", got)
	}
}

// --- 21. the grammar ----------------------------------------------------------

func parseVistaInput(input string) (*VistaRoute, error) {
	d := caddyfile.NewTestDispenser(input)
	d.Next()
	return parseVistaRoute(d)
}

func TestVistaParseLegal(t *testing.T) {
	r, err := parseVistaInput(`vista /rpc unix//run/openvista/rpc.sock`)
	if err != nil {
		t.Fatal(err)
	}
	l, err := r.limits()
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "/rpc" || l.socket != "/run/openvista/rpc.sock" || l.maxSessions != 256 || l.maxUnsigned != 32 ||
		l.perClient != 64 || l.perClientUnsigned != 8 || l.signonTimeout != 180*time.Second ||
		l.rpcTimeout != 10*time.Minute || l.queue != 8 || l.maxRequest != 1<<20 || l.maxReply != 64<<20 ||
		!l.originSame || len(l.origins) != 0 || r.AuthExempt {
		t.Fatalf("defaults: %+v %+v", r, l)
	}

	r, err = parseVistaInput(`vista /api/rpc unix//run/em.sock {
		max_sessions 100
		max_unsigned 20
		per_client 10
		per_client_unsigned 5
		per_client_exempt 10.20.0.0/24 fd00::/64
		signon_timeout 10s
		rpc_timeout 1h
		queue 64
		max_request 1KiB
		max_reply 1GiB
		origin same https://Web.Example.org https://web.example.org:8443
		auth exempt
	}`)
	if err != nil {
		t.Fatal(err)
	}
	l, err = r.limits()
	if err != nil {
		t.Fatal(err)
	}
	if l.maxSessions != 100 || l.maxUnsigned != 20 || l.perClient != 10 || l.perClientUnsigned != 5 ||
		len(l.perClientExempt) != 2 || l.signonTimeout != 10*time.Second || l.rpcTimeout != time.Hour ||
		l.queue != 64 || l.maxRequest != 1024 || l.maxReply != 1<<30 || !l.originSame ||
		strings.Join(l.origins, ",") != "https://web.example.org,https://web.example.org:8443" || !r.AuthExempt {
		t.Fatalf("every option: %+v", l)
	}

	// An empty block takes every default; a lowered max_sessions scales them.
	if _, err := parseVistaInput("vista /rpc unix//run/em.sock {\n}"); err != nil {
		t.Fatal(err)
	}
	r, err = parseVistaInput("vista /rpc unix//run/em.sock {\n max_sessions 4\n}")
	if err != nil {
		t.Fatal(err)
	}
	if l, _ = r.limits(); l.maxUnsigned != 4 || l.perClient != 4 || l.perClientUnsigned != 4 {
		t.Fatalf("scaled defaults: %+v", l)
	}
	if r, err = parseVistaInput("vista /rpc unix//run/em.sock {\n origin https://only.example\n}"); err != nil {
		t.Fatal(err)
	}
	if l, _ = r.limits(); l.originSame {
		t.Fatal("a named origin alone still allows same")
	}
}

func TestVistaParseErrors(t *testing.T) {
	block := func(opts string) string { return "vista /rpc unix//run/em.sock {\n" + opts + "\n}" }
	cases := []struct {
		name, input, want string
	}{
		{"no args", "vista", "want `vista <path> unix/<socket>`"},
		{"one arg", "vista /rpc", "want `vista <path> unix/<socket>`"},
		{"three args", "vista /rpc unix//run/em.sock extra", "want `vista <path> unix/<socket>`"},
		{"relative path", "vista rpc unix//run/em.sock", "absolute path"},
		{"root path", "vista / unix//run/em.sock", "absolute path"},
		{"trailing slash", "vista /rpc/ unix//run/em.sock", "canonical"},
		{"double slash", "vista //rpc unix//run/em.sock", "canonical"},
		{"dot segment", "vista /a/./rpc unix//run/em.sock", "canonical"},
		{"dot-dot segment", "vista /a/../rpc unix//run/em.sock", "canonical"},
		{"percent", "vista /%72pc unix//run/em.sock", "canonical"},
		{"parameter", "vista /rpc;x unix//run/em.sock", "canonical"},
		{"query", "vista /rpc?x unix//run/em.sock", "canonical"},
		{"tcp target", "vista /rpc tcp/127.0.0.1:9430", "want a unix socket"},
		{"bare host target", "vista /rpc 127.0.0.1:9430", "want a unix socket"},
		{"relative socket", "vista /rpc unix/run/em.sock", "absolute and clean"},
		{"unclean socket", "vista /rpc unix//run/../em.sock", "absolute and clean"},
		{"max_sessions 0", block("max_sessions 0"), "want 1 through 65535"},
		{"max_sessions 65536", block("max_sessions 65536"), "want 1 through 65535"},
		{"max_sessions word", block("max_sessions many"), "want 1 through 65535"},
		{"queue 0", block("queue 0"), "want 1 through 64"},
		{"queue 65", block("queue 65"), "want 1 through 64"},
		{"signon_timeout 9s", block("signon_timeout 9s"), "signon_timeout: want 10s through 10m0s"},
		{"signon_timeout 11m", block("signon_timeout 11m"), "signon_timeout: want 10s through 10m0s"},
		{"rpc_timeout 9s", block("rpc_timeout 9s"), "rpc_timeout: want 10s through 1h0m0s"},
		{"rpc_timeout 2h", block("rpc_timeout 2h"), "rpc_timeout: want 10s through 1h0m0s"},
		{"max_request small", block("max_request 1000"), "max_request: want 1024 through"},
		{"max_request large", block("max_request 17MiB"), "max_request: want 1024 through"},
		{"max_reply small", block("max_reply 512KiB"), "max_reply: want 1048576 through"},
		{"max_reply large", block("max_reply 2GiB"), "max_reply: want 1048576 through"},
		{"max_unsigned over max_sessions", block("max_sessions 4\nmax_unsigned 5"), "max_unsigned 5 exceeds max_sessions 4"},
		{"per_client over max_sessions", block("max_sessions 4\nper_client 5"), "per_client 5 exceeds max_sessions 4"},
		{"per_client_unsigned over max_unsigned", block("max_unsigned 4\nper_client_unsigned 5"), "per_client_unsigned 5 exceeds max_unsigned 4"},
		{"per_client_exempt not a CIDR", block("per_client_exempt 10.0.0.1"), "is not a CIDR"},
		{"per_client_exempt empty", block("per_client_exempt"), "want one or more CIDRs"},
		{"origin any", block("origin any"), "not an https:// origin"},
		{"origin null", block("origin null"), "not an https:// origin"},
		{"origin http", block("origin http://web.example.org"), "not an https:// origin"},
		{"origin with a path", block("origin https://web.example.org/app"), "not an https:// origin"},
		{"origin empty", block("origin"), "want same or one or more https:// origins"},
		{"auth other", block("auth required"), "the only form is `auth exempt`"},
		{"auth bare", block("auth"), "the only form is `auth exempt`"},
		{"duplicate option", block("queue 4\nqueue 4"), `duplicate option "queue"`},
		{"unknown option", block("protocol xwb"), "unrecognized vista route option: protocol"},
		{"nested block", block("queue 4 {\n x\n}"), "does not take a nested block"},
	}
	for _, tc := range cases {
		_, err := parseVistaInput(tc.input)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestVistaParseSiteAndGlobal(t *testing.T) {
	h := &Handler{}
	err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`janus {
		vista /rpc unix//run/a.sock
		vista /rpc unix//run/b.sock
	}`))
	if err == nil || !strings.Contains(err.Error(), "path /rpc is declared twice") {
		t.Fatalf("a repeated path: %v", err)
	}
	h = &Handler{}
	if err := h.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`janus {
		vista /rpc unix//run/a.sock
		vista /rpc2 unix//run/b.sock {
			queue 2
		}
	}`)); err != nil || len(h.Vista) != 2 {
		t.Fatalf("two routes: %v %d", err, len(h.Vista))
	}
	a := &App{}
	err = a.UnmarshalCaddyfile(caddyfile.NewTestDispenser(`janus {
		vista /rpc unix//run/a.sock
	}`))
	if err == nil || !strings.Contains(err.Error(), "vista routes are site-scoped") {
		t.Fatalf("vista in the global block: %v", err)
	}
}

// --- 22. no secret in any log line ------------------------------------------

func TestVistaLogsCarryNoSecret(t *testing.T) {
	core, logs := observer.New(zapcore.DebugLevel)
	g := newVistaRig(t, zap.New(core), nil)
	var tokens []string
	tok := g.connect()
	tokens = append(tokens, tok)
	wantReply(t, "IM HERE", g.do(imHereMsg, bearer(tok)...), emReplyIMHere)
	r := g.do(xwbRPCMsg("XUS AV CODE", " GOOD;CODE "), cookie(tok, "X-OV", "1")...)
	fresh := vistaSetToken(r.h)
	if fresh == "" {
		t.Fatal("no rotation")
	}
	tokens = append(tokens, fresh)
	wantReply(t, "USERINFO", g.do(xwbRPCMsg("ORWU USERINFO"), bearer(fresh)...), emReplyUserInfo)
	g.do(imHereMsg, bearer(tok)...)                                                         // rotated, 401
	g.do(imHereMsg, bearer(vistaMintToken())...)                                            // unknown, 401
	g.do(imHereMsg, cookie(fresh)...)                                                       // 403
	g.do(xwbRPCMsg("XUS AV CODE", "GOOD;CODE"), "Authorization", "Bearer GOOD;CODE")        // 400
	g.do([]byte("[XWB]1130"+fakeEmShort("XUS AV CODE")+"GOOD;CODE"), bearer(fresh)...)      // 400
	g.do(xwbRPCMsg("XUS AV CODE", "GOOD;CODE"), bearer(fresh, "Origin", "https://evil")...) // 403
	wantReply(t, "#BYE#", g.do(xwbByeMsg(), bearer(fresh)...), emReplyBye)

	if logs.Len() == 0 {
		t.Fatal("no log lines observed")
	}
	secrets := append([]string{"GOOD;CODE", "XUS AV CODE", "MANAGER,SITE", "ORWU USERINFO"}, tokens...)
	for _, e := range logs.All() {
		text := e.Message
		for k, v := range e.ContextMap() {
			text += " " + k + "=" + fmt.Sprint(v)
		}
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("log line %q carries %q", text, s)
			}
		}
	}
	var started, ended int
	for _, e := range logs.All() {
		switch e.Message {
		case "janus vista: session started":
			started++
		case "janus vista: session ended":
			ended++
		}
	}
	if started != 1 || ended != 1 {
		t.Fatalf("session lines: %d started, %d ended", started, ended)
	}
}

// An em that accepts and at once ends the stream leaves no live entry: the
// session ends (closed) and its token answers 401 closed.
func TestVistaAcceptThenClose(t *testing.T) {
	g := newVistaRig(t, nil, nil)
	g.em.Script(func(c *fakeEmConn, m fakeEmMsg) []fakeEmAction {
		if m.Command == "TCPConnect" {
			return []fakeEmAction{{Write: emReplyAccept, Close: true}}
		}
		return fakeEmDefault(c, m)
	})
	tok := g.connect()
	vistaWait(t, "the session to end", func() bool { return g.status().Sessions == 0 })
	wantEnded(t, "after the close", g.do(imHereMsg, bearer(tok)...), http.StatusUnauthorized, vistaEndClosed)
	g.pool.mu.Lock()
	routes := g.pool.routes
	g.pool.mu.Unlock()
	for _, rs := range routes {
		rs.mu.Lock()
		n := len(rs.sessions)
		rs.mu.Unlock()
		if n != 0 {
			t.Fatalf("%d token entries outlive their session", n)
		}
	}
	if st := g.status(); st.Ended["closed"] != 1 || st.Started != 1 {
		t.Fatalf("status: %+v", st)
	}
}
