package janus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
)

// The bridge contract (docs/20260720-162350-hub-design.md "The bridge
// contract").
//
// Every socket event POSTs to the tenant's hot-registered bridge_path with
// Sec-WebSocket-Frame: open|text|close, riding the existing data plane
// (upstream selection, passive health, marked-503 retry, doorbell — with a
// separate, lower-priority holder budget). Open is synchronous admission;
// text is at-most-once observation; close is bounded best-effort. Bridge
// failure is never client-visible except at open.

const (
	// hubBridgeTimeout bounds one bridge POST: one full ring hold (17s)
	// plus margin.
	hubBridgeTimeout = 20 * time.Second

	// hubBridgeQueueMsgCap bounds the per-connection text FIFO; byte cap
	// is 32 × max_frame. Overflow drops the OLDEST queued text (counted),
	// never blocks the reader, never closes the socket.
	hubBridgeQueueMsgCap = 32

	// hubBridgeRespCap bounds a directive response body read.
	hubBridgeRespCap = 1 << 20

	// hubBridgeWaiterCap is the bridge class's doorbell holder budget per
	// app, separate from the 64-client budget.
	hubBridgeWaiterCap = 16

	// Bridge request headers.
	hubFrameHeader  = "Sec-WebSocket-Frame"
	hubClientHeader = "Janus-Hub-Client"
	hubAppHeader    = "Janus-Hub-App"
)

// hubBridgeResult is one bridge POST's outcome.
type hubBridgeResult struct {
	ok     bool // a response landed (any status)
	status int
	header http.Header
	body   []byte
	over   bool // response body exceeded the read cap
	errMsg string
}

// hubBridgePost sends one synthetic data-plane POST to the app's
// registered bridge_path and captures the response. The request is fully
// buffered and replayable across marked 503s; the doorbell hold uses the
// bridge holder class.
func hubBridgePost(st *janusState, host, appID, connID, kind, remoteAddr string, snapshot http.Header, body []byte) hubBridgeResult {
	rec, ok := st.registry.resolveHost(host)
	if !ok || rec.ID != appID {
		return hubBridgeResult{errMsg: fmt.Sprintf("host %q no longer resolves to app %q", host, appID)}
	}
	if rec.BridgePath == "" {
		return hubBridgeResult{errMsg: "app has no registered bridge_path"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), hubBridgeTimeout)
	defer cancel()
	ctx = withRingClass(ctx, ringClassBridge)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+host+rec.BridgePath, bytes.NewReader(body))
	if err != nil {
		return hubBridgeResult{errMsg: err.Error()}
	}
	req.Host = host
	req.RemoteAddr = remoteAddr
	req.ContentLength = int64(len(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.Header = make(http.Header, len(snapshot)+4)
	for k, vs := range snapshot {
		req.Header[k] = vs
	}
	req.Header.Set(hubFrameHeader, kind)
	req.Header.Set(hubClientHeader, connID)
	req.Header.Set(hubAppHeader, appID)
	if kind == "open" {
		req.Header.Del("Content-Type")
	} else {
		req.Header.Set("Content-Type", "application/json")
	}

	cw := &hubCaptureWriter{header: http.Header{}}
	if err := st.dp.serveResolved(cw, req, host, rec); err != nil {
		return hubBridgeResult{errMsg: err.Error()}
	}
	if cw.status == 0 {
		return hubBridgeResult{errMsg: "bridge attempt produced no response (timeout or client context)"}
	}
	return hubBridgeResult{ok: true, status: cw.status, header: cw.header, body: cw.buf.Bytes(), over: cw.over}
}

// hubCaptureWriter records a synthetic request's response, capping the
// buffered body at the directive read cap (an oversized 2xx body is
// tenant garbage, not a streaming case).
type hubCaptureWriter struct {
	header http.Header
	status int
	buf    bytes.Buffer
	over   bool
}

func (w *hubCaptureWriter) Header() http.Header { return w.header }

func (w *hubCaptureWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *hubCaptureWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	room := hubBridgeRespCap - w.buf.Len()
	if room <= 0 {
		w.over = true
		return len(p), nil
	}
	if len(p) > room {
		w.over = true
		w.buf.Write(p[:room])
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// --- per-connection bridge FIFO -----------------------------------------------

// hubBridge serializes one connection's bridge POSTs: open (sent inline by
// admission, before this queue starts), then texts in read order, then
// close. The queue is bounded; overflow drops the oldest queued text and
// counts it. Local close stops new text admission, discards queued texts,
// waits at most for the in-flight POST, then gives the close notification
// one bounded attempt (whole post-close drainer ≤ 40s: one 20s in-flight
// bound plus one 20s close bound).
type hubBridge struct {
	c      *hubConn
	st     *janusState
	logger *zap.Logger

	byteCap int64 // 32 × max_frame

	mu        sync.Mutex
	texts     [][]byte
	bytes     int64
	closed    bool   // close pending or done: no new texts
	closeBody []byte // set once by notifyClose
	wake      chan struct{}
	done      chan struct{}

	// lastFailLog throttles per-failure warns (drainer-goroutine-local):
	// during a tenant outage a chatty client would otherwise emit one
	// warn per bridged frame. Counters stay exact; logs sample.
	lastFailLog time.Time
	failsMuted  int64
}

func newHubBridge(c *hubConn, st *janusState, logger *zap.Logger, maxFrame int64) *hubBridge {
	return &hubBridge{
		c:       c,
		st:      st,
		logger:  logger,
		byteCap: hubBridgeQueueMsgCap * maxFrame,
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
	}
}

// enqueueText queues one client frame (verbatim bytes) for observation.
// Never blocks the reader; overflow drops the oldest queued text.
func (b *hubBridge) enqueueText(frame []byte) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	for len(b.texts) >= hubBridgeQueueMsgCap || (len(b.texts) > 0 && b.bytes+int64(len(frame)) > b.byteCap) {
		oldest := b.texts[0]
		b.texts = b.texts[1:]
		b.bytes -= int64(len(oldest))
		b.c.hub.ctr.bridgeDropped.Add(1)
	}
	b.texts = append(b.texts, frame)
	b.bytes += int64(len(frame))
	b.mu.Unlock()
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// notifyClose stops text admission, discards and counts queued texts, and
// hands the drainer its one bounded close attempt.
func (b *hubBridge) notifyClose(code int, reason string) {
	body, _ := json.Marshal(map[string]any{"code": code, "reason": reason})
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	dropped := len(b.texts)
	b.texts = nil
	b.bytes = 0
	b.closeBody = body
	b.mu.Unlock()
	if dropped > 0 {
		b.c.hub.ctr.bridgeDropped.Add(int64(dropped))
	}
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// run is the connection's one bridge drainer: texts in read order, close
// last. close is never silently lost behind text and never reordered
// before an in-flight text; each POST is individually bounded (20s), so
// the post-close tail is bounded by design (≤ 40s).
func (b *hubBridge) run() {
	defer close(b.done)
	for {
		b.mu.Lock()
		if len(b.texts) > 0 {
			frame := b.texts[0]
			b.texts = b.texts[1:]
			b.bytes -= int64(len(frame))
			b.mu.Unlock()
			b.postText(frame)
			continue
		}
		closed, closeBody := b.closed, b.closeBody
		b.mu.Unlock()
		if closed {
			if closeBody != nil {
				b.postClose(closeBody)
			}
			return
		}
		<-b.wake
	}
}

// postText sends one text bridge and processes the tenant's answer.
// Failure is at-most-once observation: counted, logged, never retried,
// nothing client-visible.
func (b *hubBridge) postText(frame []byte) {
	c := b.c
	res := hubBridgePost(b.st, c.host, c.appID, c.id, "text", c.remoteAddr, c.snapshot, frame)
	b.processResponse("text", res)
}

// postClose gives the close notification its one bounded attempt.
func (b *hubBridge) postClose(body []byte) {
	c := b.c
	res := hubBridgePost(b.st, c.host, c.appID, c.id, "close", c.remoteAddr, c.snapshot, body)
	if !res.ok || res.status < 200 || res.status > 299 {
		c.hub.ctr.bridgeFailed.Add(1)
		b.logger.Warn("janus hub close bridge undelivered",
			zap.String("app", c.appID),
			zap.String("conn", c.id),
			zap.String("reason", bridgeFailReason(res)),
		)
		return
	}
	c.hub.ctr.bridgeSent.Add(1)
	b.processDirectives("close", res)
}

// processResponse applies the per-frame-type failure policy and, on 2xx,
// executes any directive body on the bridge-response plane.
func (b *hubBridge) processResponse(kind string, res hubBridgeResult) {
	c := b.c
	if !res.ok || res.status < 200 || res.status > 299 {
		c.hub.ctr.bridgeFailed.Add(1)
		// Sample failure logs to one per second per connection: a tenant
		// outage under chatty senders must not flood the log (the
		// bridge_failed counter stays exact).
		if now := time.Now(); now.Sub(b.lastFailLog) >= time.Second {
			b.logger.Warn("janus hub bridge failed",
				zap.String("app", c.appID),
				zap.String("conn", c.id),
				zap.String("frame", kind),
				zap.String("reason", bridgeFailReason(res)),
				zap.Int64("muted_since_last", b.failsMuted),
			)
			b.lastFailLog = now
			b.failsMuted = 0
		} else {
			b.failsMuted++
		}
		return
	}
	c.hub.ctr.bridgeSent.Add(1)
	b.processDirectives(kind, res)
}

// processDirectives executes a 2xx bridge response's body in the bridged
// connection's context. A malformed body is tenant garbage: dropped
// whole, counted, logged; the client is unaffected.
func (b *hubBridge) processDirectives(kind string, res hubBridgeResult) {
	c := b.c
	body := bytes.TrimSpace(res.body)
	if len(body) == 0 || res.status == http.StatusNoContent {
		return
	}
	if res.over {
		b.garbage(kind, "response body exceeds "+strconv.Itoa(hubBridgeRespCap)+" bytes")
		return
	}
	objs, verr := parseHubFrame(body, hubPlaneBridge)
	if verr != nil {
		b.garbage(kind, verr.msg)
		return
	}
	if _, verr := c.hub.execute(objs, hubPlaneBridge, c); verr != nil {
		b.garbage(kind, verr.msg)
	}
}

func (b *hubBridge) garbage(kind, reason string) {
	c := b.c
	c.hub.ctr.bridgeGarbage.Add(1)
	b.logger.Warn("janus hub bridge garbage",
		zap.String("app", c.appID),
		zap.String("conn", c.id),
		zap.String("frame", kind),
		zap.String("reason", reason),
	)
}

func bridgeFailReason(res hubBridgeResult) string {
	if res.errMsg != "" {
		return res.errMsg
	}
	return "tenant answered " + strconv.Itoa(res.status)
}

// --- handshake header snapshot --------------------------------------------------

// hubSnapshotMax caps the serialized filtered snapshot; over it the
// handshake rejects 431 — never truncated.
const hubSnapshotMax = 32 << 10

// hubHopHeaders are dropped from the snapshot: hop-by-hop and WebSocket
// mechanics. The rest — Cookie, Authorization, everything the tenant
// authenticates with — rides along for the connection's lifetime.
var hubHopHeaders = map[string]bool{
	"Connection":        true,
	"Upgrade":           true,
	"Keep-Alive":        true,
	"Te":                true,
	"Trailer":           true,
	"Transfer-Encoding": true,
}

// hubHeaderSnapshot filters and freezes the upgrade request's headers.
// ok=false means the filtered snapshot serializes over the cap.
func hubHeaderSnapshot(h http.Header) (http.Header, bool) {
	out := make(http.Header, len(h))
	size := 0
	for name, vals := range h {
		if hubHopHeaders[name] || len(name) >= 14 && name[:14] == "Sec-Websocket-" {
			continue
		}
		for _, v := range vals {
			size += len(name) + len(v) + 4 // name: value\r\n
		}
		out[name] = vals
	}
	if size > hubSnapshotMax {
		return nil, false
	}
	return out, true
}
