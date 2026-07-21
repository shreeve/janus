package janus

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// --- harness ------------------------------------------------------------------

func newTestAppHub() *appHub {
	return newHubSet().getOrCreate("app-test01")
}

// dialHubConn stands up one real WebSocket pair: the server side becomes a
// registered hubConn (writer running, reader NOT running — tests drive the
// executor directly), the client side reads deliveries.
func dialHubConn(t *testing.T, hub *appHub, maxChannels int) (*hubConn, *websocket.Conn) {
	t.Helper()
	connCh := make(chan *hubConn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := hubUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		id, err := mintHubConnID()
		if err != nil {
			t.Error(err)
			return
		}
		c := newHubConn(id, hub, "hubtest.example.com", r.RemoteAddr, http.Header{}, maxChannels, hubDefaultMaxFrame)
		c.ws = ws
		if !hub.registerConn(c) {
			t.Error("registerConn refused")
			return
		}
		go c.writeLoop()
		go func() { // absorb client-sent control frames
			for {
				if _, _, err := ws.ReadMessage(); err != nil {
					return
				}
			}
		}()
		connCh <- c
	}))
	t.Cleanup(srv.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	c := <-connCh
	return c, client
}

// clientExec parses and executes one client-plane frame from c.
func clientExec(t *testing.T, hub *appHub, c *hubConn, frame string) (hubExecOutcome, *hubViolation) {
	t.Helper()
	objs, verr := parseHubFrame([]byte(frame), hubPlaneClient)
	if verr != nil {
		return hubExecOutcome{}, verr
	}
	return hub.execute(objs, hubPlaneClient, c)
}

func mustExec(t *testing.T, hub *appHub, plane hubPlane, origin *hubConn, frame string) hubExecOutcome {
	t.Helper()
	objs, verr := parseHubFrame([]byte(frame), plane)
	if verr != nil {
		t.Fatalf("parse: %s", verr.msg)
	}
	out, verr := hub.execute(objs, plane, origin)
	if verr != nil {
		t.Fatalf("execute: %s", verr.msg)
	}
	return out
}

func readWS(t *testing.T, ws *websocket.Conn) []byte {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, data, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("expected a delivery: %v", err)
	}
	return data
}

func expectNoWS(t *testing.T, ws *websocket.Conn) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
	_, data, err := ws.ReadMessage()
	if err == nil {
		t.Fatalf("expected no delivery, got %s", data)
	}
}

func expectWSClose(t *testing.T, ws *websocket.Conn, code int, reasonSub string) {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, _, err := ws.ReadMessage()
		if err == nil {
			continue // drain deliveries ahead of the close
		}
		ce, ok := err.(*websocket.CloseError)
		if !ok {
			t.Fatalf("want close %d, got %v", code, err)
		}
		if ce.Code != code {
			t.Fatalf("close code: want %d, got %d (%s)", code, ce.Code, ce.Text)
		}
		if !strings.Contains(ce.Text, reasonSub) {
			t.Fatalf("close reason: want %q, got %q", reasonSub, ce.Text)
		}
		return
	}
}

// checkMirror asserts the bidirectional bookkeeping invariant:
// conns[c].channels contains ch IFF channels[ch] contains c.
func checkMirror(t *testing.T, hub *appHub) {
	t.Helper()
	hub.mu.Lock()
	defer hub.mu.Unlock()
	for id, c := range hub.conns {
		for ch := range c.channels {
			if set := hub.channels[ch]; set == nil || set[id] != c {
				t.Fatalf("conn %s claims %s but channel side disagrees", id, ch)
			}
		}
	}
	for ch, set := range hub.channels {
		if len(set) == 0 {
			t.Fatalf("empty channel %s not deleted", ch)
		}
		for id, c := range set {
			if hub.conns[id] != c {
				t.Fatalf("channel %s holds unregistered conn %s", ch, id)
			}
			if _, ok := c.channels[ch]; !ok {
				t.Fatalf("channel %s holds %s but conn side disagrees", ch, id)
			}
		}
	}
}

func hubChannelCount(hub *appHub, ch string) int {
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return len(hub.channels[ch])
}

// --- membership bookkeeping -----------------------------------------------------

func TestHubMembershipBookkeeping(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, _ := dialHubConn(t, hub, hubDefaultMaxChannels)

	mustExec(t, hub, hubPlaneClient, a, `{"+":["/lobby","/game/42"]}`)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/lobby"]}`)
	checkMirror(t, hub)
	if n := hubChannelCount(hub, "/lobby"); n != 2 {
		t.Fatalf("/lobby members: want 2, got %d", n)
	}

	// Joining an existing membership is an idempotent no-op.
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/lobby"]}`)
	checkMirror(t, hub)
	if n := hubChannelCount(hub, "/lobby"); n != 2 {
		t.Fatalf("idempotent join changed member count: %d", n)
	}

	// Leaving a missing membership is an idempotent no-op.
	mustExec(t, hub, hubPlaneClient, b, `{"-":["/game/42"]}`)
	checkMirror(t, hub)

	// The last leave deletes the channel.
	mustExec(t, hub, hubPlaneClient, a, `{"-":["/game/42"]}`)
	checkMirror(t, hub)
	if n := hubChannelCount(hub, "/game/42"); n != 0 {
		t.Fatalf("emptied channel must be deleted, has %d", n)
	}

	// One frame may join and leave; object-list order applies.
	mustExec(t, hub, hubPlaneClient, a, `[{"+":["/x"]},{"-":["/x"]}]`)
	checkMirror(t, hub)
	if n := hubChannelCount(hub, "/x"); n != 0 {
		t.Fatalf("join-then-leave must end absent, has %d", n)
	}
}

func TestHubMaxChannelsRejectsWholeFrameBeforeAnyEffect(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, 2)

	mustExec(t, hub, hubPlaneClient, a, `{"+":["/one"]}`)

	// Two more would exceed max_channels 2: the WHOLE frame rejects and
	// nothing applies — not even the first channel of the list.
	_, verr := clientExec(t, hub, a, `{"+":["/two","/three"]}`)
	if verr == nil || !strings.Contains(verr.msg, "join would exceed 2 channels") {
		t.Fatalf("want max_channels rejection, got %v", verr)
	}
	if n := hubChannelCount(hub, "/two"); n != 0 {
		t.Fatal("rejected frame partially applied /two")
	}
	checkMirror(t, hub)

	// A leave inside the same frame makes room: [-one, +two,+three] fits.
	mustExec(t, hub, hubPlaneClient, a, `[{"-":["/one"]},{"+":["/two","/three"]}]`)
	checkMirror(t, hub)
}

// --- target resolution and authorization -----------------------------------------

func TestHubClientMembershipIsSenderOnly(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)

	// @ naming another connection with + rejects; neither joins.
	_, verr := clientExec(t, hub, a, fmt.Sprintf(`{"@":[%q],"+":["/x"]}`, b.id))
	if verr == nil || !strings.Contains(verr.msg, "may mutate only the sending connection") {
		t.Fatalf("want sender-only rejection, got %v", verr)
	}
	if hubChannelCount(hub, "/x") != 0 {
		t.Fatal("rejected frame mutated membership")
	}

	// @ resolving to exactly the sender is legal, any spelling.
	mustExec(t, hub, hubPlaneClient, a, fmt.Sprintf(`{"@":[%q],"+":["/x"]}`, a.id))
	if hubChannelCount(hub, "/x") != 1 {
		t.Fatal("self-@ join failed")
	}

	// A channel that expands to only the sender also counts as exactly
	// the sender.
	mustExec(t, hub, hubPlaneClient, a, `{"@":["/x"],"+":["/y"]}`)
	if hubChannelCount(hub, "/y") != 1 {
		t.Fatal("self-channel-@ join failed")
	}

	// The trusted publish plane CAN enroll b.
	mustExec(t, hub, hubPlanePublish, nil, fmt.Sprintf(`{"@":[%q],"+":["/x"]}`, b.id))
	if hubChannelCount(hub, "/x") != 2 {
		t.Fatal("trusted cross-connection join failed")
	}
	_ = bws
	checkMirror(t, hub)
}

func TestHubTrustedChannelExpansionAsSubjects(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	// Publish: everyone in /room joins /vip (channel expands as subjects).
	mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"+":["/vip"]}`)
	if hubChannelCount(hub, "/vip") != 2 {
		t.Fatalf("/vip members: want 2, got %d", hubChannelCount(hub, "/vip"))
	}

	// Post-prior-mutation expansion: object 1 enrolls b into /stage,
	// object 2 expands /stage and moves its members to /live — b included.
	mustExec(t, hub, hubPlanePublish, nil, fmt.Sprintf(
		`[{"@":[%q],"+":["/stage"]},{"@":["/stage"],"+":["/live"]}]`, b.id))
	if hubChannelCount(hub, "/live") != 1 {
		t.Fatalf("/live members: want 1, got %d", hubChannelCount(hub, "/live"))
	}
	checkMirror(t, hub)
}

func TestHubTargetResolution(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	// Dedupe: b reached through the channel AND its id receives one bundle.
	out := mustExec(t, hub, hubPlaneClient, a, fmt.Sprintf(`{"@":["/room",%q],"chat":{"n":1}}`, b.id))
	if out.deliveries != 1 {
		t.Fatalf("dedupe: want 1 delivery, got %d", out.deliveries)
	}
	readWS(t, bws)
	// A sentinel proves exactly one bundle was queued: the next read is
	// the sentinel, not a duplicate chat.
	mustExec(t, hub, hubPlanePublish, nil, fmt.Sprintf(`{"@":[%q],"sentinel":1}`, b.id))
	if got := string(readWS(t, bws)); !strings.Contains(got, "sentinel") {
		t.Fatalf("dedupe: duplicate bundle before sentinel: %s", got)
	}

	// Missing channels and dead ids skip and count, never error.
	out = mustExec(t, hub, hubPlaneClient, a, `{"@":["/nope","deadbeefdeadbeef"],"chat":{}}`)
	if out.deliveries != 0 || out.unknownTargets != 2 {
		t.Fatalf("want 0 deliveries / 2 unknown, got %d / %d", out.deliveries, out.unknownTargets)
	}

	// Channel vs id discrimination: /-prefix expands, other strings are ids.
	out = mustExec(t, hub, hubPlaneClient, a, fmt.Sprintf(`{"@":[%q],"whisper":{}}`, b.id))
	if out.deliveries != 1 {
		t.Fatalf("direct id: want 1 delivery, got %d", out.deliveries)
	}
	readWS(t, bws)
}

func TestHubNamingOnlyHierarchy(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/a/b"]}`)

	// Targeting /a does NOT fan out to /a/b members.
	out := mustExec(t, hub, hubPlanePublish, nil, `{"@":["/a"],"chat":{}}`)
	if out.deliveries != 0 || out.unknownTargets != 1 {
		t.Fatalf("naming-only: want 0 deliveries / 1 unknown, got %d / %d", out.deliveries, out.unknownTargets)
	}
}

// --- the ! suffix ×4 --------------------------------------------------------------

func TestHubBareNameExcludesOriginatingConnectionOnly(t *testing.T) {
	hub := newTestAppHub()
	a1, a1ws := dialHubConn(t, hub, hubDefaultMaxChannels) // one "user",
	a2, a2ws := dialHubConn(t, hub, hubDefaultMaxChannels) // two tabs
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	for _, c := range []*hubConn{a1, a2, b} {
		mustExec(t, hub, hubPlaneClient, c, `{"+":["/room"]}`)
	}

	out := mustExec(t, hub, hubPlaneClient, a1, `{"@":["/room"],"chat":{"m":"hi"}}`)
	if out.deliveries != 2 {
		t.Fatalf("bare name: want 2 deliveries, got %d", out.deliveries)
	}
	readWS(t, a2ws) // the sender's other tab is a different connection
	readWS(t, bws)
	expectNoWS(t, a1ws)
}

func TestHubSuffixIncludesSenderAndIsStripped(t *testing.T) {
	hub := newTestAppHub()
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	out := mustExec(t, hub, hubPlaneClient, a, `{"@":["/room"],"chat!":{"m":"hi"}}`)
	if out.deliveries != 2 {
		t.Fatalf("!: want 2 deliveries, got %d", out.deliveries)
	}
	for _, ws := range []*websocket.Conn{aws, bws} {
		var got map[string]json.RawMessage
		if err := json.Unmarshal(readWS(t, ws), &got); err != nil {
			t.Fatal(err)
		}
		if _, has := got["chat!"]; has {
			t.Fatal("suffix must be stripped on delivery")
		}
		if _, has := got["chat"]; !has {
			t.Fatalf("recipient must see the bare key, got %v", got)
		}
	}
}

func TestHubPublishIgnoresSpelling(t *testing.T) {
	hub := newTestAppHub()
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)

	// No originating connection → bare and ! deliver identically.
	out := mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"chat":{}}`)
	if out.deliveries != 1 {
		t.Fatalf("publish bare: want 1, got %d", out.deliveries)
	}
	readWS(t, aws)
	out = mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"chat!":{}}`)
	if out.deliveries != 1 {
		t.Fatalf("publish !: want 1, got %d", out.deliveries)
	}
	readWS(t, aws)
}

func TestHubExclusionNeverKeysOffProvenance(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	// A bridge response (origin a) attributing < to b does not exclude b.
	out := mustExec(t, hub, hubPlaneBridge, a, fmt.Sprintf(`{"<":[%q],"@":["/room"],"chat":{}}`, b.id))
	if out.deliveries != 1 {
		t.Fatalf("want b delivered despite < naming it, got %d", out.deliveries)
	}
	readWS(t, bws)
}

func TestHubLoopbackSemantics(t *testing.T) {
	hub := newTestAppHub()
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)

	// No @: the sender itself is the target set. Bare name → nobody
	// (asserted by count; the sentinel below proves the queue is empty).
	out := mustExec(t, hub, hubPlaneClient, a, `{"echo":{}}`)
	if out.deliveries != 0 {
		t.Fatalf("bare self-target: want 0 deliveries, got %d", out.deliveries)
	}
	mustExec(t, hub, hubPlanePublish, nil, fmt.Sprintf(`{"@":[%q],"sentinel":1}`, a.id))
	if got := string(readWS(t, aws)); !strings.Contains(got, "sentinel") {
		t.Fatalf("bare self-target leaked a delivery: %s", got)
	}

	// {"echo!": …} is the protocol's built-in loopback probe.
	out = mustExec(t, hub, hubPlaneClient, a, `{"echo!":{"n":1}}`)
	if out.deliveries != 1 {
		t.Fatalf("loopback: want 1 delivery, got %d", out.deliveries)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(readWS(t, aws), &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["echo"]; !has {
		t.Fatalf("loopback bundle: %v", got)
	}
}

// --- < stamping --------------------------------------------------------------------

func TestHubProvenanceStamping(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	// Client-originated: Janus stamps < as [sending-connection-id].
	mustExec(t, hub, hubPlaneClient, a, `{"@":["/room"],"chat":{}}`)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(readWS(t, bws), &got); err != nil {
		t.Fatal(err)
	}
	var prov []string
	if err := json.Unmarshal(got["<"], &prov); err != nil {
		t.Fatalf("< must be an array: %v", err)
	}
	if len(prov) != 1 || prov[0] != a.id {
		t.Fatalf("< stamp: want [%s], got %v", a.id, prov)
	}

	// Trusted plane: array-valued < passes through unmodified (a relay
	// may append); absent < stays absent.
	mustExec(t, hub, hubPlanePublish, nil, `{"<":["origin","relay-1"],"@":["/room"],"chat":{}}`)
	if err := json.Unmarshal(readWS(t, bws), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(got["<"], &prov); err != nil || len(prov) != 2 || prov[1] != "relay-1" {
		t.Fatalf("trusted < passthrough: got %v", prov)
	}

	mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"chat":{}}`)
	got = nil // Unmarshal merges into a live map; start fresh
	if err := json.Unmarshal(readWS(t, bws), &got); err != nil {
		t.Fatal(err)
	}
	if _, has := got["<"]; has {
		t.Fatal("absent < must stay absent on trusted deliveries")
	}
}

// --- ? / ! liveness -----------------------------------------------------------------

func TestHubPongEchoesVerbatim(t *testing.T) {
	hub := newTestAppHub()
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)

	mustExec(t, hub, hubPlaneClient, a, `{"?":"t1721512345"}`)
	if got := string(readWS(t, aws)); got != `{"!":"t1721512345"}` {
		t.Fatalf("pong: got %s", got)
	}

	// ? may ride with other directives; the pong is sent after the frame
	// executes — the sender's own chat! delivery lands first.
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)
	mustExec(t, hub, hubPlaneClient, a, `{"@":["/room"],"chat!":{},"?":"t2"}`)
	readWS(t, bws)
	first := readWS(t, aws)  // own chat! delivery
	second := readWS(t, aws) // then the pong
	if !strings.Contains(string(first), "chat") || string(second) != `{"!":"t2"}` {
		t.Fatalf("pong ordering: %s then %s", first, second)
	}
	checkMirror(t, hub)
}

// --- kick ---------------------------------------------------------------------------

func TestHubTrustedKick(t *testing.T) {
	hub := newTestAppHub()
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)

	// Events in the object deliver first, then the * frame, then close 1000.
	mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"notice!":{"bye":1},"*":"kicked"}`)
	if got := string(readWS(t, aws)); !strings.Contains(got, "notice") {
		t.Fatalf("event before kick: %s", got)
	}
	if got := string(readWS(t, aws)); got != `{"*":"kicked"}` {
		t.Fatalf("kick frame: %s", got)
	}
	expectWSClose(t, aws, hubCloseNormal, "kicked")

	waitFor(t, "kicked conn cleanup", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.conns) == 0 && len(hub.channels) == 0
	})
}

// --- backpressure ---------------------------------------------------------------------

func TestHubSlowConsumerMessageCap(t *testing.T) {
	hub := newTestAppHub()
	// No writer: the queue only fills.
	c := newHubConn("cccccccccccccccc", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	srvWS, clientWS := wsPair(t)
	c.ws = srvWS
	hub.registerConn(c)

	for i := 0; i < hubQueueMsgCap; i++ {
		if !c.enqueue([]byte(`{"n":1}`)) {
			t.Fatalf("enqueue %d refused below the cap", i)
		}
	}
	if c.enqueue([]byte(`{"n":1}`)) {
		t.Fatal("enqueue past the message cap must refuse")
	}
	// The closer writes the Close frame — it is never enqueued.
	expectWSClose(t, clientWS, hubCloseTryLater, "slow consumer")
	if hub.ctr.slowCloses.Load() != 1 {
		t.Fatalf("slow_closes: want 1, got %d", hub.ctr.slowCloses.Load())
	}
	waitFor(t, "slow conn cleanup", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.conns) == 0
	})
}

func TestHubSlowConsumerByteCap(t *testing.T) {
	hub := newTestAppHub()
	c := newHubConn("cccccccccccccccc", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	srvWS, clientWS := wsPair(t)
	c.ws = srvWS
	hub.registerConn(c)

	big := make([]byte, 600<<10)
	if !c.enqueue(big) {
		t.Fatal("first 600KB must fit")
	}
	if c.enqueue(big) {
		t.Fatal("second 600KB must trip the 1MiB byte cap")
	}
	expectWSClose(t, clientWS, hubCloseTryLater, "slow consumer")
}

// TestHubSlowConsumerOverflowClosesOnce pins that racing enqueues to one
// wedged connection trip exactly one slow-consumer close: the overflow
// branch marks the queue closed under its own lock, so followers take the
// closed fast path instead of each spawning a close and inflating the
// counter.
func TestHubSlowConsumerOverflowClosesOnce(t *testing.T) {
	hub := newTestAppHub()
	c := newHubConn("cccccccccccccccc", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	srvWS, clientWS := wsPair(t)
	c.ws = srvWS
	hub.registerConn(c)

	for i := 0; i < hubQueueMsgCap; i++ {
		if !c.enqueue([]byte(`{"n":1}`)) {
			t.Fatalf("enqueue %d refused below the cap", i)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c.enqueue([]byte(`{"n":1}`)) {
				t.Error("enqueue past the cap must refuse")
			}
		}()
	}
	wg.Wait()
	if got := hub.ctr.slowCloses.Load(); got != 1 {
		t.Fatalf("slow_closes: want exactly 1, got %d", got)
	}
	expectWSClose(t, clientWS, hubCloseTryLater, "slow consumer")
}

// TestHubCloseBeforeUpgrade pins the open/teardown race: a connection
// registered but not yet upgraded (ws == nil) can be closed by teardown
// without panicking, cleanup runs, and a late attachWS reports the close
// so the admission path keeps ownership of the raw socket.
func TestHubCloseBeforeUpgrade(t *testing.T) {
	hs := newHubSet()
	hub := hs.getOrCreate("app-race")
	c := newHubConn("rrrrrrrrrrrrrrrr", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	if !hub.registerConn(c) {
		t.Fatal("registerConn refused")
	}

	// Teardown lands in the registration→upgrade window: must not panic,
	// must clean up, must return promptly (never waiting on a socket).
	done := make(chan struct{})
	go func() {
		hs.teardownApp("app-race")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("teardown blocked on an unupgraded connection")
	}
	hub.mu.Lock()
	remaining := len(hub.conns)
	hub.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("cleanup must run without a socket: %d conns remain", remaining)
	}

	// The upgrade completes after the close: attachWS must report it.
	srvWS, _ := wsPair(t)
	if c.attachWS(srvWS) {
		t.Fatal("attachWS after close must report false")
	}
	c.qmu.Lock()
	code, reason := c.closeCode, c.closeReason
	c.qmu.Unlock()
	if code != hubCloseGoingAway || !strings.Contains(reason, "app deregistered") {
		t.Fatalf("recorded close: %d %q", code, reason)
	}
}

// wsPair returns a raw server/client websocket pair with no hub wiring.
func wsPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	ch := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := hubUpgrader.Upgrade(w, r, nil)
		if err == nil {
			ch <- ws
		}
	}))
	t.Cleanup(srv.Close)
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return <-ch, client
}

// --- lifecycle ---------------------------------------------------------------------

func TestHubCloseCleansBothMapDirections(t *testing.T) {
	hub := newTestAppHub()
	a, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room","/solo"]}`)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	a.closeWith(hubCloseGoingAway, "test")
	checkMirror(t, hub)
	hub.mu.Lock()
	_, aLive := hub.conns[a.id]
	soloMembers := len(hub.channels["/solo"])
	roomMembers := len(hub.channels["/room"])
	hub.mu.Unlock()
	if aLive || soloMembers != 0 || roomMembers != 1 {
		t.Fatalf("cleanup: live=%v solo=%d room=%d", aLive, soloMembers, roomMembers)
	}

	// Idempotent: a second close is a no-op.
	a.closeWith(hubCloseGoingAway, "again")

	// Deliveries targeting the dead id skip and count.
	out := mustExec(t, hub, hubPlaneClient, b, fmt.Sprintf(`{"@":[%q],"chat":{}}`, a.id))
	if out.deliveries != 0 || out.unknownTargets != 1 {
		t.Fatalf("dead target: want 0/1, got %d/%d", out.deliveries, out.unknownTargets)
	}
}

func TestHubTeardownClosesEverything(t *testing.T) {
	hs := newHubSet()
	hub := hs.getOrCreate("app-x")
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)

	hs.teardownApp("app-x")
	expectWSClose(t, aws, hubCloseGoingAway, "app deregistered")
	waitFor(t, "teardown cleanup", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.conns) == 0 && len(hub.channels) == 0
	})

	// The tombstone rejects in-flight opens after teardown.
	if hub.reserveSlot(100) {
		t.Fatal("tombstoned hub must not reserve slots")
	}
	c := newHubConn("dddddddddddddddd", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	if hub.registerConn(c) {
		t.Fatal("tombstoned hub must not register connections")
	}
}

func TestHubHostsRemovedClosesOnlyBoundConns(t *testing.T) {
	hs := newHubSet()
	hub := hs.getOrCreate("app-x")
	a, aws := dialHubConn(t, hub, hubDefaultMaxChannels)
	b, bws := dialHubConn(t, hub, hubDefaultMaxChannels)
	b.host = "kept.example.com"
	mustExec(t, hub, hubPlaneClient, a, `{"+":["/room"]}`)
	mustExec(t, hub, hubPlaneClient, b, `{"+":["/room"]}`)

	hs.hostsRemoved("app-x", map[string]bool{"hubtest.example.com": true})
	expectWSClose(t, aws, hubCloseGoingAway, "host removed")
	waitFor(t, "removed-host cleanup", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.conns) == 1
	})
	// b keeps its socket and membership.
	mustExec(t, hub, hubPlanePublish, nil, `{"@":["/room"],"still":{}}`)
	readWS(t, bws)
}

func TestHubSlotReservation(t *testing.T) {
	hub := newTestAppHub()
	if !hub.reserveSlot(2) {
		t.Fatal("first reservation must fit a floor of 2")
	}
	if !hub.reserveSlot(2) {
		t.Fatal("second reservation must fit a floor of 2")
	}
	if hub.reserveSlot(2) {
		t.Fatal("third reservation must fail at floor 2")
	}
	hub.releaseSlot()
	if !hub.reserveSlot(2) {
		t.Fatal("released slot must be reusable")
	}
	// registerConn converts a reservation.
	c := newHubConn("eeeeeeeeeeeeeeee", hub, "h", "", http.Header{}, 8, hubDefaultMaxFrame)
	if !hub.registerConn(c) {
		t.Fatal("registerConn must succeed")
	}
	hub.mu.Lock()
	reserved, conns := hub.reserved, len(hub.conns)
	hub.mu.Unlock()
	if reserved != 1 || conns != 1 {
		t.Fatalf("after convert: reserved=%d conns=%d", reserved, conns)
	}
}

// TestHubFanoutRacingClose exercises fan-out against concurrent closes
// under -race: never a panic, never a delivery after cleanup.
func TestHubFanoutRacingClose(t *testing.T) {
	hub := newTestAppHub()
	publisher, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	var conns []*hubConn
	for i := 0; i < 8; i++ {
		c, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
		mustExec(t, hub, hubPlaneClient, c, `{"+":["/room"]}`)
		conns = append(conns, c)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_, _ = clientExec(t, hub, publisher, `{"@":["/room"],"tick":{}}`)
		}
	}()
	go func() {
		defer wg.Done()
		for _, c := range conns {
			c.closeWith(hubCloseGoingAway, "race test")
		}
	}()
	wg.Wait()
	checkMirror(t, hub)
}

// --- mint ---------------------------------------------------------------------------

func TestMintHubConnID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := mintHubConnID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != hubConnIDLen {
			t.Fatalf("id length: %q", id)
		}
		for _, r := range id {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				t.Fatalf("id alphabet: %q", id)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate id %q in 100 mints", id)
		}
		seen[id] = true
	}
}
