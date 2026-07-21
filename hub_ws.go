package janus

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

// The WebSocket edge (docs/20260720-162350-hub-design.md "Identity and
// connection lifecycle", "Security"). Connections terminate at Janus; the
// tenant participates over the HTTP bridge. Client frames execute at the
// edge — delivery never gates on the bridge.

var hubUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Origin policy is Janus's own admission check (step 2), enforced
	// before any bridge is sent; the library check is disabled.
	CheckOrigin: func(*http.Request) bool { return true },
}

// serveHub handles one intercepted upgrade request on a hub-enabled site.
func (h *Handler) serveHub(w http.ResponseWriter, r *http.Request) error {
	cfg := h.hubCfg
	app := h.app
	st := app.state

	// Host resolution runs as always: unknown host → 404, worker untouched.
	host := normalizeHostHeader(r.Host)
	rec, ok := st.registry.resolveHost(host)
	if !ok {
		return caddyhttp.Error(http.StatusNotFound, fmt.Errorf("janus: unknown host %q", host))
	}

	// Origin check: fails → 403, no upgrade, no bridge.
	if !hubOriginAllowed(cfg, r, host) {
		app.logger.Warn("janus hub origin rejected",
			zap.String("app", rec.ID),
			zap.String("host", host),
			zap.String("origin", r.Header.Get("Origin")),
		)
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil
	}

	// Hub-enabled site, hub-unready tenant: loud, not a hang.
	if rec.BridgePath == "" {
		return hubUnavailable(w, "app has no registered bridge_path")
	}

	// Admission checks and slot reservation, atomically: reserving before
	// the open bridge closes the concurrent-handshake race.
	floor, enabled := app.hubMaxConnsFloor(rec)
	if !enabled {
		// The arriving site is hub-enabled (we are here through it), so
		// the table must agree; a mismatch means the table is stale.
		floor = cfg.maxConns
	}
	hub := st.hubs.getOrCreate(rec.ID)
	if !hub.reserveSlot(floor) {
		return hubUnavailable(w, fmt.Sprintf("connection cap reached (%d)", floor))
	}

	// The filtered handshake-header snapshot: frozen at open, replayed on
	// every bridge POST for the connection's lifetime. Over the cap → 431,
	// never truncated.
	snapshot, fits := hubHeaderSnapshot(r.Header)
	if !fits {
		hub.releaseSlot()
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "handshake headers exceed the 32 KiB bridge snapshot cap",
			http.StatusRequestHeaderFieldsTooLarge)
		return nil
	}

	connID, err := mintHubConnID()
	if err != nil {
		hub.releaseSlot()
		return caddyhttp.Error(http.StatusInternalServerError, fmt.Errorf("janus hub: minting connection id: %w", err))
	}

	// The open bridge: the tenant's admission decision, synchronous, while
	// the handshake holds unanswered. Nothing is buffered; the socket is
	// not yet upgraded.
	res := hubBridgePost(st, host, rec.ID, connID, "open", r.RemoteAddr, snapshot, nil)
	if !res.ok {
		hub.releaseSlot()
		app.logger.Warn("janus hub open bridge failed",
			zap.String("app", rec.ID),
			zap.String("conn", connID),
			zap.String("reason", res.errMsg),
		)
		return hubUnavailable(w, "open bridge failed: "+res.errMsg)
	}
	if res.status < 200 || res.status > 299 {
		// The tenant refused admission: forward that status and body
		// honestly (marker headers are already stripped on the data
		// plane). A data-plane-generated 503 (empty upstreams, all
		// unhealthy, ring failure) carries Retry-After; keep it.
		hub.releaseSlot()
		if ct := res.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if ra := res.header.Get("Retry-After"); ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(res.status)
		_, werr := w.Write(res.body)
		return werr
	}

	// 2xx → recheck the hub tombstone while converting the reserved slot
	// into a registration. Teardown during the bridge → 503, no upgrade.
	c := newHubConn(connID, hub, host, r.RemoteAddr, snapshot, cfg.maxChannels, cfg.maxFrame)
	if !hub.registerConn(c) {
		return hubUnavailable(w, "app deregistered during the open bridge")
	}

	ws, err := hubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		// Client departure before 101: no connection, no close bridge.
		hub.removeConn(c)
		return nil
	}
	if !c.attachWS(ws) {
		// A close (teardown, host removal, kick) raced the upgrade:
		// cleanup already ran, possibly before the socket existed — make
		// sure the raw socket dies either way (double close is harmless).
		_ = ws.Close()
		return nil
	}
	ws.SetReadLimit(cfg.maxFrame)
	ws.SetPongHandler(func(string) error {
		c.pongReceived()
		return nil
	})
	c.bridge = newHubBridge(c, st, app.logger, cfg.maxFrame)

	go c.writeLoop()
	go c.pingLoop()
	go c.bridge.run()

	// A close that began after attachWS but before the bridge existed
	// could not deliver its close notification; recheck now that the
	// drainer is running, so the drainer always terminates.
	select {
	case <-c.closedCh:
		c.qmu.Lock()
		code, reason := c.closeCode, c.closeReason
		c.qmu.Unlock()
		c.bridge.notifyClose(code, reason)
		return nil
	default:
	}

	// The tenant's open-response directives execute in the new
	// connection's context before the inbound reader starts: enrollment
	// and greeting precede all client frames.
	c.bridge.processDirectives("open", res)

	go h.hubReadLoop(c)
	return nil
}

func hubUnavailable(w http.ResponseWriter, reason string) error {
	w.Header().Set("Retry-After", retryAfter)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	_ = reason // carried in logs by callers that have context
	return nil
}

// hubReadLoop reads, validates, and executes client frames at the edge,
// forwarding each valid frame to the text bridge (observation only).
func (h *Handler) hubReadLoop(c *hubConn) {
	hub := c.hub
	logger := h.app.logger
	for {
		mt, data, err := c.ws.ReadMessage()
		if err != nil {
			code, reason := hubCloseDetail(err, c)
			c.closeWith(code, reason)
			return
		}
		if mt != websocket.TextMessage {
			hub.ctr.rejectedFrames.Add(1)
			c.closeWith(hubCloseUnsupported, "binary frames are not supported")
			return
		}
		hub.ctr.framesIn.Add(1)

		objs, verr := parseHubFrame(data, hubPlaneClient)
		if verr == nil {
			_, verr = hub.execute(objs, hubPlaneClient, c)
		}
		if verr != nil {
			// A malformed client frame closes the connection: executing
			// well-formed frames while erroring malformed ones is exactly
			// the drift-tolerance the repo rules forbid.
			hub.ctr.rejectedFrames.Add(1)
			logger.Warn("janus hub frame rejected",
				zap.String("app", c.appID),
				zap.String("conn", c.id),
				zap.Int("close", verr.code),
				zap.String("reason", verr.msg),
			)
			c.closeWith(verr.code, verr.msg)
			return
		}

		// Observation: verbatim wire bytes, at-most-once, never gating
		// the edge execution above.
		c.bridge.enqueueText(data)
	}
}

// hubCloseDetail maps a read error to the close code and reason reported
// to the tenant's close bridge.
func hubCloseDetail(err error, c *hubConn) (int, string) {
	if ce, ok := err.(*websocket.CloseError); ok {
		return ce.Code, ce.Text
	}
	if err == websocket.ErrReadLimit {
		return hubCloseTooBig, fmt.Sprintf("frame exceeds %d bytes", c.maxFrame)
	}
	return websocket.CloseAbnormalClosure, "read failed: " + err.Error()
}

// hubOriginAllowed applies the cold origin policy: `any` disables the
// check (a deliberate, spelled-out opt-out); otherwise the Origin header
// must be present and its host must equal the request's Host (`same`) or
// be explicitly allowlisted. Scheme is not compared.
func hubOriginAllowed(cfg *hubSite, r *http.Request, host string) bool {
	if cfg.originAny {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false // non-browser clients need an allowlist or `any` posture
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	oh := strings.ToLower(u.Hostname())
	if cfg.originSame && oh == host {
		return true
	}
	return cfg.originHosts[oh]
}
