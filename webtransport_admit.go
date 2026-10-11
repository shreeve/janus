package janus

// Admission on the relay listener: Janus's own HTTP/3 handler, judged in
// the contract's order, first match wins. Every refusal is counted and
// logged; the path is logged without its query, and a ticket is never
// logged.

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/quic-go/quic-go/http3"

	"go.uber.org/zap"
)

// wtProtocol reports whether an extended CONNECT names WebTransport:
// `webtransport-h3` (draft-ietf-webtrans-http3-15 and later) or the
// earlier drafts' `webtransport`, which browsers still send.
func wtProtocol(p string) bool { return p == "webtransport-h3" || p == "webtransport" }

// serveHTTP is the HTTP/3 handler for one socket.
func (r *wtRelay) serveHTTP(sock *wtSocket, w http.ResponseWriter, req *http.Request) {
	table := r.live.Load()
	if req.Method != http.MethodConnect || !wtProtocol(req.Proto) {
		r.refusedMethod.Add(1)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := normalizeHostHeader(req.Host)
	path := req.URL.Path
	if !table.hosts[host] {
		r.refusedHost.Add(1)
		r.refuse(w, http.StatusNotFound, "no relay routes for this host", host, path, "")
		return
	}
	// A legitimate WebTransport/QUIC (TLS 1.3) handshake always carries SNI,
	// so an empty or mismatched server name against Host fails closed.
	if req.TLS != nil && strings.ToLower(req.TLS.ServerName) != host {
		r.refusedFronting.Add(1)
		r.refuse(w, http.StatusMisdirectedRequest, "TLS server name does not match Host", host, path, req.TLS.ServerName)
		return
	}
	if wanExposure() && localFamilyHost(host) {
		r.refuse(w, http.StatusMisdirectedRequest, "local name in wan mode", host, path, "")
		return
	}
	route := table.routes[wtRouteKey(host, path)]
	if route == nil {
		r.refusedPath.Add(1)
		r.refuse(w, http.StatusNotFound, "no relay route at this path", host, path, "")
		return
	}
	cfg := route.config()
	origin := req.Header.Get("Origin")
	if !wtOriginAllowed(cfg.origin, origin, host) {
		route.counters.refusedOrigin.Add(1)
		r.refuse(w, http.StatusForbidden, "origin not allowed", host, path, origin)
		return
	}
	// A ticket is verified here and spent only after every later check
	// passes, so a refusal for capacity, the target, or the client's
	// SETTINGS leaves it good for a retry inside its lifetime.
	var claim wtTicketClaim
	if cfg.gated {
		c, ok := r.tickets.verify(req.URL.Query().Get(wtTicketQuery), wtRouteKey(host, path))
		if !ok {
			route.counters.refusedTicket.Add(1)
			r.refuse(w, http.StatusForbidden, "ticket missing, invalid, expired, or already used", host, path, "")
			return
		}
		claim = c
	}
	if !r.reserve(route, cfg.maxSessions, table.maxSessions) {
		route.counters.refusedCap.Add(1)
		w.Header().Set("Retry-After", "1")
		r.refuse(w, http.StatusServiceUnavailable, "session cap reached", host, path, "")
		return
	}
	udp, err := wtDialTarget(route.target)
	if err != nil {
		r.release(route)
		route.counters.refusedUpstream.Add(1)
		w.Header().Set("Retry-After", "5")
		r.refuse(w, http.StatusServiceUnavailable, "target socket: "+err.Error(), host, path, "")
		return
	}
	if err := wtAwaitSettings(w, req); err != nil {
		_ = udp.Close()
		r.release(route)
		route.counters.refusedSettings.Add(1)
		r.refuse(w, http.StatusBadRequest, "webtransport upgrade: "+err.Error(), host, path, "")
		return
	}
	if cfg.gated && !r.tickets.consume(claim) {
		// A concurrent CONNECT spent the same ticket first.
		_ = udp.Close()
		r.release(route)
		route.counters.refusedTicket.Add(1)
		r.refuse(w, http.StatusForbidden, "ticket missing, invalid, expired, or already used", host, path, "")
		return
	}
	sess, err := sock.wt.Upgrade(w, req)
	if err != nil {
		// Upgrade has written nothing on this path. Its SETTINGS checks
		// already passed above, so what is left is the connection closing
		// under it; the spent ticket stays spent.
		_ = udp.Close()
		r.release(route)
		route.counters.refusedSettings.Add(1)
		r.refuse(w, http.StatusBadRequest, "webtransport upgrade: "+err.Error(), host, path, "")
		return
	}
	route.counters.accepted.Add(1)
	s := newWtSession(r, route, sess, udp, sock.addr, claim.user, table.maxDatagram)
	s.run()
}

// wtSettingsWait bounds the wait for the client's HTTP/3 SETTINGS before
// a session is admitted: the library's reordering timeout, which bounds
// the same wait inside Upgrade.
const wtSettingsWait = 5 * time.Second

// wtAwaitSettings waits for the client's SETTINGS and requires datagram
// support, the checks Upgrade makes before it writes 200, so they run
// before the ticket is spent rather than after.
func wtAwaitSettings(w http.ResponseWriter, req *http.Request) error {
	s, ok := w.(http3.Settingser)
	if !ok {
		return errors.New("not an HTTP/3 request")
	}
	t := time.NewTimer(wtSettingsWait)
	defer t.Stop()
	select {
	case <-s.ReceivedSettings():
	case <-req.Context().Done():
		return context.Cause(req.Context())
	case <-t.C:
		return errors.New("the client's SETTINGS did not arrive in time")
	}
	if set := s.Settings(); set == nil || !set.EnableDatagrams {
		return errors.New("the client's SETTINGS lack datagram support")
	}
	return nil
}

// reserve takes a session slot on the route and in the process, or
// neither; two racing CONNECTs cannot both pass the cap.
func (r *wtRelay) reserve(route *wtRoute, routeMax, processMax int) bool {
	if route.sessions.Add(1) > int64(routeMax) {
		route.sessions.Add(-1)
		return false
	}
	if r.sessionN.Add(1) > int64(processMax) {
		r.sessionN.Add(-1)
		route.sessions.Add(-1)
		return false
	}
	return true
}

func (r *wtRelay) release(route *wtRoute) {
	route.sessions.Add(-1)
	r.sessionN.Add(-1)
}

func (r *wtRelay) refuse(w http.ResponseWriter, status int, reason, host, path, detail string) {
	fields := []zap.Field{zap.Int("status", status), zap.String("reason", reason), zap.String("host", host), zap.String("path", path)}
	if detail != "" {
		fields = append(fields, zap.String("detail", detail))
	}
	r.logger.Warn("janus webtransport refused", fields...)
	http.Error(w, reason, status)
}

// descriptor is the JSON a page reads at the route's path over TCP.
type wtDescriptor struct {
	URL               string              `json:"url"`
	MaxDatagram       int                 `json:"max_datagram"`
	CertificateHashes []wtCertificateHash `json:"certificate_hashes"`
}

type wtCertificateHash struct {
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}

// serveWebtransportDescriptor answers GET and HEAD at a route's path on
// the site (over TCP), after the auth wall. On a gated site the URL
// carries a ticket for the signed-in user; a request the wall let through
// without a user (a path outside every gate) gets no ticket and no URL.
func (h *Handler) serveWebtransportDescriptor(w http.ResponseWriter, r *http.Request, route *WebtransportRoute) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	host := normalizeHostHeader(r.Host)
	desc := wtDescriptor{URL: "https://" + host + route.Path, CertificateHashes: []wtCertificateHash{}}
	desc.MaxDatagram = wtDefaultMaxDatagram
	if h.app != nil && h.app.Webtransport != nil {
		desc.MaxDatagram = h.app.Webtransport.maxDatagram()
	}
	// Chrome enforces Certificate Transparency on the WebTransport QUIC
	// handshake even for a locally-trusted root, so a local-CA leaf is
	// reached by pinning its hash (serverCertificateHashes), not CA trust.
	// Publish the current short-lived leaf hash; empty means "validate by CA"
	// (a public, CT-compliant cert).
	if h.app != nil && h.app.state != nil && h.app.state.webtransport != nil {
		if hash, err := h.app.state.webtransport.leafHash(host); err == nil && hash != "" {
			desc.CertificateHashes = []wtCertificateHash{{Algorithm: "sha-256", Value: hash}}
		}
	}
	if h.authCfg != nil {
		user := r.Header.Get("Remote-User")
		if user == "" {
			http.Error(w, "the relay on a gated site needs a signed-in user; put this path under a gate", http.StatusForbidden)
			return
		}
		if h.app == nil || h.app.state == nil {
			http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
			return
		}
		ticket := h.app.state.webtransport.tickets.mint(wtRouteKey(host, route.Path), user)
		if ticket == "" {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "too many outstanding tickets", http.StatusServiceUnavailable)
			return
		}
		desc.URL += "?" + wtTicketQuery + "=" + ticket
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	writeJSON(w, http.StatusOK, desc)
}

// wtRouteFor finds this site's route at an exact path.
func (h *Handler) wtRouteFor(path string) *WebtransportRoute {
	for _, route := range h.Webtransport {
		if route.Path == path {
			return route
		}
	}
	return nil
}
