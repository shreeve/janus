package janus

import (
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Hub connection (docs/20260720-162350-hub-design.md "Identity and
// connection lifecycle", "Backpressure: the slow consumer", "Close
// mechanisms").
//
// Every connection has one outbound queue and one writer goroutine;
// fan-out enqueues and returns — a sender is never blocked by a
// recipient's socket. The queue is capped by messages and bytes,
// whichever trips first; on overflow the connection is CLOSED (1013
// "slow consumer"), never trimmed: a dropped frame would be a silent gap
// in a protocol with no sequence numbers and no replay.

const (
	// Outbound queue caps (fixed in v1).
	hubQueueMsgCap  = 256
	hubQueueByteCap = 1 << 20

	// Protocol-level ping cadence toward clients (bam.cr's PING interval,
	// kept). A connection that has not answered the previous ping by the
	// time the next one is due is closed: worst case ~60s to detect a
	// silent peer.
	hubPingEvery = 30 * time.Second

	// hubWriteWait bounds one WebSocket write (data or control).
	hubWriteWait = 10 * time.Second

	// hubCloseReasonMax is RFC 6455's cap on a Close frame reason.
	hubCloseReasonMax = 123
)

// hubOutItem is one writer-queue entry: a text frame, or a close
// instruction ordered behind prior deliveries (the trusted * kick sends
// its application frame first, then closes 1000).
type hubOutItem struct {
	data      []byte
	closeCode int
	closeText string
}

// hubConn is one registered WebSocket connection.
type hubConn struct {
	id    string
	appID string
	host  string // bound host at upgrade (host removal closes through it)
	hub   *appHub

	ws         *websocket.Conn
	remoteAddr string

	// snapshot is the frozen, filtered handshake-header snapshot replayed
	// on every bridge POST for this connection's lifetime.
	snapshot http.Header

	// maxChannels and maxFrame are the site's effective caps at admission;
	// membership validation checks each subject against its own
	// connection's cap, and the reader's limit reports against maxFrame.
	maxChannels int
	maxFrame    int64

	// channels is this connection's channel set; guarded by hub.mu (the
	// membership lock), mirroring hub.channels.
	channels map[string]struct{}

	qmu     sync.Mutex
	queue   []hubOutItem
	qbytes  int64
	qclosed bool          // no further enqueue (closing or closed)
	wake    chan struct{} // buffered 1; writer wake-up

	closeOnce sync.Once
	closedCh  chan struct{} // closed when close begins; stops writer and ping loops

	// closeCode/closeReason record what closeWith closed with (guarded by
	// qmu), for the admission path's post-setup recheck: a close racing
	// the upgrade may run before the bridge exists and needs its close
	// notification delivered late.
	closeCode   int
	closeReason string

	pmu         sync.Mutex
	pongPending bool

	bridge *hubBridge
}

func newHubConn(id string, hub *appHub, host, remoteAddr string, snapshot http.Header, maxChannels int, maxFrame int64) *hubConn {
	return &hubConn{
		id:          id,
		appID:       hub.id,
		host:        host,
		hub:         hub,
		remoteAddr:  remoteAddr,
		snapshot:    snapshot,
		maxChannels: maxChannels,
		maxFrame:    maxFrame,
		channels:    map[string]struct{}{},
		wake:        make(chan struct{}, 1),
		closedCh:    make(chan struct{}),
	}
}

// enqueue appends one outbound frame, enforcing both queue caps. On
// overflow the connection is closed (1013), not trimmed; the Close frame
// is written by the closer, never enqueued. Returns false when nothing
// was enqueued (closed or overflowing).
func (c *hubConn) enqueue(data []byte) bool {
	c.qmu.Lock()
	if c.qclosed {
		c.qmu.Unlock()
		return false
	}
	if len(c.queue) >= hubQueueMsgCap || c.qbytes+int64(len(data)) > hubQueueByteCap {
		// Mark the queue closed HERE, under qmu: racing fan-outs to the
		// same wedged connection take the qclosed fast path above instead
		// of each spawning another close and inflating slow_closes.
		c.qclosed = true
		c.queue = nil
		c.qbytes = 0
		c.qmu.Unlock()
		c.hub.ctr.slowCloses.Add(1)
		// Close asynchronously: fan-out to the remaining recipients must
		// never block on one wedged connection's close handshake.
		go c.closeWith(hubCloseTryLater, "slow consumer")
		return false
	}
	c.queue = append(c.queue, hubOutItem{data: data})
	c.qbytes += int64(len(data))
	c.qmu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return true
}

// enqueueAndClose atomically orders one final application frame followed by a
// normal close. Keeping both queue operations under qmu prevents an unrelated
// racing fan-out from landing between the trusted kick and its close now that
// appHub dispatch no longer holds the app-wide membership lock while enqueueing.
func (c *hubConn) enqueueAndClose(data []byte, code int, reason string) bool {
	c.qmu.Lock()
	if c.qclosed {
		c.qmu.Unlock()
		return false
	}
	if len(c.queue) >= hubQueueMsgCap || c.qbytes+int64(len(data)) > hubQueueByteCap {
		c.qclosed = true
		c.queue = nil
		c.qbytes = 0
		c.qmu.Unlock()
		c.hub.ctr.slowCloses.Add(1)
		go c.closeWith(hubCloseTryLater, "slow consumer")
		return false
	}
	c.qclosed = true
	c.queue = append(c.queue,
		hubOutItem{data: data},
		hubOutItem{closeCode: code, closeText: reason},
	)
	c.qbytes += int64(len(data))
	c.qmu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
	return true
}

// writeLoop is the connection's one writer, draining the queue in enqueue
// order (per-connection FIFO delivery).
func (c *hubConn) writeLoop() {
	for {
		c.qmu.Lock()
		if len(c.queue) == 0 {
			c.qmu.Unlock()
			select {
			case <-c.wake:
				continue
			case <-c.closedCh:
				return
			}
		}
		item := c.queue[0]
		c.queue = c.queue[1:]
		c.qbytes -= int64(len(item.data))
		c.qmu.Unlock()

		if item.closeCode != 0 {
			c.closeWith(item.closeCode, item.closeText)
			return
		}
		_ = c.ws.SetWriteDeadline(time.Now().Add(hubWriteWait))
		if err := c.ws.WriteMessage(websocket.TextMessage, item.data); err != nil {
			// The socket is broken mid-write; no Close frame can land.
			c.closeWith(websocket.CloseAbnormalClosure, "write failed")
			return
		}
	}
}

// pingLoop keeps the WebSocket-protocol-level liveness clock: ping every
// interval; an unanswered ping by the next tick closes 1001 "ping
// timeout". Independent of the queue caps on purpose: pings catch the
// silent-dead, the caps catch the alive-but-drowning.
func (c *hubConn) pingLoop() {
	t := time.NewTicker(hubPingEvery)
	defer t.Stop()
	for {
		select {
		case <-c.closedCh:
			return
		case <-t.C:
			if !c.pingOnce() {
				return
			}
		}
	}
}

func (c *hubConn) pingOnce() bool {
	c.pmu.Lock()
	pending := c.pongPending
	c.pongPending = true
	c.pmu.Unlock()
	if pending {
		c.hub.ctr.pingCloses.Add(1)
		c.closeWith(hubCloseGoingAway, "ping timeout")
		return false
	}
	if err := c.ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(hubWriteWait)); err != nil {
		c.hub.ctr.pingCloses.Add(1)
		c.closeWith(websocket.CloseAbnormalClosure, "ping write failed")
		return false
	}
	return true
}

func (c *hubConn) pongReceived() {
	c.pmu.Lock()
	c.pongPending = false
	c.pmu.Unlock()
}

// attachWS binds the upgraded socket to the connection. It reports false
// when a close (teardown, host removal, kick) already began in the window
// between registration and upgrade — the caller owns the raw socket then,
// because closeWith may have run before the socket existed.
func (c *hubConn) attachWS(ws *websocket.Conn) bool {
	c.qmu.Lock()
	c.ws = ws
	c.qmu.Unlock()
	select {
	case <-c.closedCh:
		return false
	default:
		return true
	}
}

// attachBridge publishes the connection's bridge to racing close paths. A
// close that already began cannot have notified a bridge that did not exist,
// so this side completes that notification before reporting false. Otherwise
// closeWith captures the published bridge under the same lock and owns the
// eventual notification.
func (c *hubConn) attachBridge(bridge *hubBridge) bool {
	c.qmu.Lock()
	c.bridge = bridge
	code, reason := c.closeCode, c.closeReason
	c.qmu.Unlock()
	if code == 0 {
		return true
	}
	bridge.notifyClose(code, reason)
	return false
}

// closeWith is the internal close-with-reason mechanism (always present;
// serves teardown, ping timeout, slow consumers, hard protocol errors,
// host removal, hub disable, and write failures). It performs local
// cleanup synchronously (channels left, queue dropped, close bridge
// notified), then sends the Close frame and closes the TCP connection
// from a goroutine of its own: a wedged peer can absorb the full write
// deadline, and teardown loops (DELETE, TTL reap, host removal, reload)
// must never wait on one socket's buffer. Idempotent, and safe before
// the socket exists (the open/teardown race window): ws is read under
// qmu after closedCh closes, so a concurrent attachWS either publishes
// the socket to us or observes the close and keeps ownership.
func (c *hubConn) closeWith(code int, reason string) {
	c.closeOnce.Do(func() {
		if len(reason) > hubCloseReasonMax {
			reason = reason[:hubCloseReasonMax]
		}
		c.qmu.Lock()
		c.qclosed = true
		c.queue = nil
		c.qbytes = 0
		c.closeCode, c.closeReason = code, reason
		bridge := c.bridge
		c.qmu.Unlock()
		close(c.closedCh)

		c.qmu.Lock()
		ws := c.ws
		c.qmu.Unlock()
		if ws != nil {
			go func() {
				msg := websocket.FormatCloseMessage(code, reason)
				_ = ws.WriteControl(websocket.CloseMessage, msg, time.Now().Add(hubWriteWait))
				_ = ws.Close()
			}()
		}

		c.hub.removeConn(c)
		if bridge != nil {
			bridge.notifyClose(code, reason)
		}
	})
}
