package janus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}

func contextBackground() context.Context { return context.Background() }

// --- harness -------------------------------------------------------------------

// bridgeFixture is the recording tenant: every bridge POST it receives
// (headers, frame type, body, order) plus a scriptable answer.
type bridgeFixture struct {
	mu     sync.Mutex
	posts  []bridgePost
	answer func(kind string, body []byte) (int, string) // status, response body
	block  chan struct{}                                // when non-nil, handler waits
}

type bridgePost struct {
	kind   string
	path   string
	header http.Header
	body   []byte
}

func (f *bridgeFixture) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.posts = append(f.posts, bridgePost{
			kind:   r.Header.Get(hubFrameHeader),
			path:   r.URL.Path,
			header: r.Header.Clone(),
			body:   body,
		})
		answer := f.answer
		block := f.block
		f.mu.Unlock()
		if block != nil {
			<-block
		}
		status, resp := http.StatusNoContent, ""
		if answer != nil {
			status, resp = answer(r.Header.Get(hubFrameHeader), body)
		}
		if resp != "" {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, resp)
	})
}

func (f *bridgeFixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.posts)
}

func (f *bridgeFixture) post(i int) bridgePost {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts[i]
}

// newBridgeHarness stands up a janusState with one registered app whose
// bridge_path points at the fixture over a unix socket.
func newBridgeHarness(t *testing.T, f *bridgeFixture) (*janusState, AppRecord) {
	t.Helper()
	reg := newAppRegistry()
	st := &janusState{
		registry: reg,
		hubs:     newHubSet(),
		dp:       newDataPlane(reg, nil),
		logger:   zap.NewNop(),
	}
	reg.hubTeardown = st.hubs.teardownApp
	reg.hubHostsRemoved = st.hubs.hostsRemoved
	rec, err := reg.create("brtest", []string{"hubtest.example.com"}, "/rt/bridge")
	if err != nil {
		t.Fatal(err)
	}
	sock := startUnixHTTP(t, f.handler())
	if _, err := reg.setUpstreams(rec.ID, []Upstream{{Path: sock}}); err != nil {
		t.Fatal(err)
	}
	rec, _ = reg.get(rec.ID)
	return st, rec
}

// --- request shapes --------------------------------------------------------------

func TestHubBridgeRequestShapes(t *testing.T) {
	f := &bridgeFixture{}
	st, rec := newBridgeHarness(t, f)

	snapshot := http.Header{
		"Cookie":        {"sid=42"},
		"Authorization": {"Bearer tok"},
		"User-Agent":    {"test-agent"},
	}

	// open: no body, no Content-Type.
	res := hubBridgePost(st, "hubtest.example.com", rec.ID, "conn1conn1conn1c", "open", "1.2.3.4:5", snapshot, nil)
	if !res.ok || res.status != http.StatusNoContent {
		t.Fatalf("open: %+v", res)
	}
	p := f.post(0)
	if p.kind != "open" || p.path != "/rt/bridge" {
		t.Fatalf("open shape: %+v", p)
	}
	if p.header.Get("Content-Type") != "" || len(p.body) != 0 {
		t.Fatalf("open must carry no body/Content-Type: %q %q", p.header.Get("Content-Type"), p.body)
	}
	if p.header.Get(hubClientHeader) != "conn1conn1conn1c" || p.header.Get(hubAppHeader) != rec.ID {
		t.Fatalf("identity headers: %+v", p.header)
	}
	if p.header.Get("Cookie") != "sid=42" || p.header.Get("Authorization") != "Bearer tok" {
		t.Fatal("snapshot headers must ride along")
	}
	if p.header.Get("X-Forwarded-For") != "1.2.3.4" {
		t.Fatalf("X-Forwarded-For: %q", p.header.Get("X-Forwarded-For"))
	}

	// text: verbatim body bytes, application/json.
	frame := []byte(`{"@": ["/room"],  "chat!": {"hi": 1}}`)
	res = hubBridgePost(st, "hubtest.example.com", rec.ID, "conn1conn1conn1c", "text", "1.2.3.4:5", snapshot, frame)
	if !res.ok {
		t.Fatalf("text: %+v", res)
	}
	p = f.post(1)
	if string(p.body) != string(frame) {
		t.Fatalf("text body must be verbatim: %q", p.body)
	}
	if p.header.Get("Content-Type") != "application/json" {
		t.Fatalf("text Content-Type: %q", p.header.Get("Content-Type"))
	}
}

func TestHubBridgePostFailureModes(t *testing.T) {
	f := &bridgeFixture{}
	st, rec := newBridgeHarness(t, f)

	// Empty upstreams: down on purpose — fail fast as a 503 (non-2xx per
	// the failure table), nothing to wake, no hang.
	if _, err := st.registry.setUpstreams(rec.ID, []Upstream{}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	res := hubBridgePost(st, "hubtest.example.com", rec.ID, "c", "text", "", http.Header{}, []byte(`{}`))
	if !res.ok || res.status != http.StatusServiceUnavailable {
		t.Fatalf("empty upstreams must land 503: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("empty upstreams must fail immediately")
	}

	// Host gone.
	res = hubBridgePost(st, "nope.example.com", rec.ID, "c", "text", "", http.Header{}, []byte(`{}`))
	if res.ok || !strings.Contains(res.errMsg, "no longer resolves") {
		t.Fatalf("gone host: %+v", res)
	}

	// bridge_path cleared mid-life.
	empty := ""
	if _, err := st.registry.patch(rec.ID, nil, nil, &empty); err != nil {
		t.Fatal(err)
	}
	res = hubBridgePost(st, "hubtest.example.com", rec.ID, "c", "text", "", http.Header{}, []byte(`{}`))
	if res.ok || !strings.Contains(res.errMsg, "bridge_path") {
		t.Fatalf("cleared bridge_path: %+v", res)
	}
}

// TestHubBridgeMarked503Replay pins that a buffered bridge POST retries
// across a worker's marked 503 (GetBody replay) and lands intact.
func TestHubBridgeMarked503Replay(t *testing.T) {
	f := &bridgeFixture{}
	st, rec := newBridgeHarness(t, f)

	busy := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set(workerBusyHeader, "1")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	good := f.handler()
	goodSock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		good.ServeHTTP(w, r)
	}))
	// Order matters to least-conn only statistically; retry covers both.
	if _, err := st.registry.setUpstreams(rec.ID, []Upstream{{Path: busy}, {Path: goodSock}}); err != nil {
		t.Fatal(err)
	}

	frame := []byte(`{"chat":{"n":1}}`)
	res := hubBridgePost(st, "hubtest.example.com", rec.ID, "c", "text", "", http.Header{}, frame)
	if !res.ok || res.status != http.StatusNoContent {
		t.Fatalf("marked-503 retry failed: %+v", res)
	}
	if f.count() != 1 || string(f.post(0).body) != string(frame) {
		t.Fatalf("replayed body must land intact once: %d posts", f.count())
	}
}

// --- the per-connection FIFO --------------------------------------------------------

// newBridgeConn wires a registered hubConn + hubBridge into the harness.
func newBridgeConn(t *testing.T, st *janusState, rec AppRecord) (*hubConn, *hubBridge) {
	t.Helper()
	hub := st.hubs.getOrCreate(rec.ID, nil)
	c, _ := dialHubConn(t, hub, hubDefaultMaxChannels)
	b := newHubBridge(c, st, zap.NewNop(), hubDefaultMaxFrame)
	c.bridge = b
	return c, b
}

func TestHubBridgeFIFOOrder(t *testing.T) {
	f := &bridgeFixture{}
	st, rec := newBridgeHarness(t, f)
	_, b := newBridgeConn(t, st, rec)
	go b.run()

	for i := 0; i < 3; i++ {
		b.enqueueText([]byte(fmt.Sprintf(`{"seq":{"n":%d}}`, i)))
	}
	waitFor(t, "three texts", func() bool { return f.count() == 3 })
	for i := 0; i < 3; i++ {
		if want := fmt.Sprintf(`{"seq":{"n":%d}}`, i); string(f.post(i).body) != want {
			t.Fatalf("order: post %d = %q", i, f.post(i).body)
		}
	}
	if st.hubs.getOrCreate(rec.ID, nil).ctr.bridgeSent.Load() != 3 {
		t.Fatal("bridge_sent must count 2xx posts")
	}
}

func TestHubBridgeDropOldestOverflow(t *testing.T) {
	f := &bridgeFixture{block: make(chan struct{})}
	st, rec := newBridgeHarness(t, f)
	c, b := newBridgeConn(t, st, rec)
	go b.run()

	// First text goes in-flight and blocks at the fixture.
	b.enqueueText([]byte(`{"inflight":0}`))
	waitFor(t, "in-flight post", func() bool { return f.count() == 1 })

	// Fill the queue to its cap, then two more: the two OLDEST queued
	// texts drop, the reader is never blocked, the socket stays open.
	for i := 1; i <= hubBridgeQueueMsgCap+2; i++ {
		b.enqueueText([]byte(fmt.Sprintf(`{"q":%d}`, i)))
	}
	if got := c.hub.ctr.bridgeDropped.Load(); got != 2 {
		t.Fatalf("bridge_dropped: want 2, got %d", got)
	}
	close(f.block)
	waitFor(t, "drain", func() bool { return f.count() == 1+hubBridgeQueueMsgCap })
	// The survivors are the newest 32: q=3 … q=34.
	if want := `{"q":3}`; string(f.post(1).body) != want {
		t.Fatalf("oldest must drop: second post = %q", f.post(1).body)
	}
}

func TestHubBridgeClosePriorityAndDrainer(t *testing.T) {
	f := &bridgeFixture{block: make(chan struct{})}
	st, rec := newBridgeHarness(t, f)
	c, b := newBridgeConn(t, st, rec)
	go b.run()

	b.enqueueText([]byte(`{"inflight":0}`))
	waitFor(t, "in-flight post", func() bool { return f.count() == 1 })
	b.enqueueText([]byte(`{"queued":1}`))
	b.enqueueText([]byte(`{"queued":2}`))

	// Local close: queued texts discard (counted), the in-flight POST
	// finishes, then exactly one close POST.
	b.notifyClose(1001, "test close")
	if got := c.hub.ctr.bridgeDropped.Load(); got != 2 {
		t.Fatalf("discarded queued texts must count: %d", got)
	}
	close(f.block)
	waitFor(t, "drainer exit", func() bool {
		select {
		case <-b.done:
			return true
		default:
			return false
		}
	})
	if f.count() != 2 {
		t.Fatalf("want in-flight text + close, got %d posts", f.count())
	}
	last := f.post(1)
	if last.kind != "close" {
		t.Fatalf("final post must be the close: %+v", last.kind)
	}
	var closeBody map[string]any
	if err := json.Unmarshal(last.body, &closeBody); err != nil || closeBody["code"] != float64(1001) || closeBody["reason"] != "test close" {
		t.Fatalf("close body: %s", last.body)
	}

	// After close: no new texts admitted.
	b.enqueueText([]byte(`{"late":1}`))
	time.Sleep(50 * time.Millisecond)
	if f.count() != 2 {
		t.Fatal("text admitted after close")
	}

	// notifyClose is idempotent.
	b.notifyClose(1002, "again")
}

// --- response processing --------------------------------------------------------------

func TestHubBridgeResponseProcessing(t *testing.T) {
	f := &bridgeFixture{}
	st, rec := newBridgeHarness(t, f)
	c, b := newBridgeConn(t, st, rec)
	hub := c.hub

	exec := func(status int, body string) {
		f.mu.Lock()
		f.answer = func(string, []byte) (int, string) { return status, body }
		f.mu.Unlock()
		b.postText([]byte(`{"observed":1}`))
	}

	// 2xx empty → nothing; the frame was observed.
	exec(http.StatusOK, "")
	if hub.ctr.bridgeGarbage.Load() != 0 || hub.ctr.bridgeFailed.Load() != 0 {
		t.Fatal("2xx empty must be clean")
	}

	// 2xx directives → executed in the bridged connection's context:
	// @-absent defaults to the originating connection.
	exec(http.StatusOK, `{"+":["/enrolled"]}`)
	if hubChannelCount(hub, "/enrolled") != 1 {
		t.Fatal("bridge-response join must apply to the bridged connection")
	}

	// Garbage: non-JSON → dropped whole, counted, client unaffected.
	exec(http.StatusOK, `this is not json`)
	if hub.ctr.bridgeGarbage.Load() != 1 {
		t.Fatalf("bridge_garbage: %d", hub.ctr.bridgeGarbage.Load())
	}

	// Garbage: plane-violating sigil (? is client-only).
	exec(http.StatusOK, `{"?":"t"}`)
	if hub.ctr.bridgeGarbage.Load() != 2 {
		t.Fatal("plane-violating response must be garbage")
	}

	// Garbage: valid grammar, stateful violation (max_channels) — dropped
	// whole and counted as garbage.
	joins := make([]string, hubDefaultMaxChannels+1)
	for i := range joins {
		joins[i] = fmt.Sprintf("/c/%d", i)
	}
	jb, _ := json.Marshal(joins)
	// One frame joining max+1 channels needs a list (64-per-op cap):
	// split into three objects of ~43.
	exec(http.StatusOK, fmt.Sprintf(`[{"+":%s},{"+":%s},{"+":%s}]`,
		string(mustSlice(jb, 0, 43)), string(mustSlice(jb, 43, 86)), string(mustSlice(jb, 86, hubDefaultMaxChannels+1))))
	if hub.ctr.bridgeGarbage.Load() != 3 {
		t.Fatalf("stateful violation must be garbage: %d", hub.ctr.bridgeGarbage.Load())
	}

	// Non-2xx → bridge_failed, logged, never retried, nothing else.
	exec(http.StatusInternalServerError, "boom")
	if hub.ctr.bridgeFailed.Load() != 1 {
		t.Fatalf("bridge_failed: %d", hub.ctr.bridgeFailed.Load())
	}

	// 2xx status is otherwise uninterpreted: 299 with directives works.
	exec(299, `{"+":["/further"]}`)
	if hubChannelCount(hub, "/further") != 1 {
		t.Fatal("299 must process directives")
	}
}

// mustSlice re-marshals a subrange of a JSON string array.
func mustSlice(arr []byte, from, to int) []byte {
	var all []string
	if err := json.Unmarshal(arr, &all); err != nil {
		panic(err)
	}
	out, err := json.Marshal(all[from:to])
	if err != nil {
		panic(err)
	}
	return out
}

// --- snapshot filtering ------------------------------------------------------------------

func TestHubHeaderSnapshot(t *testing.T) {
	in := http.Header{
		"Cookie":                   {"sid=1"},
		"Authorization":            {"Bearer x"},
		"User-Agent":               {"ua"},
		"Connection":               {"Upgrade"},
		"Upgrade":                  {"websocket"},
		"Keep-Alive":               {"300"},
		"Te":                       {"trailers"},
		"Trailer":                  {"X-T"},
		"Transfer-Encoding":        {"chunked"},
		"Sec-Websocket-Key":        {"abc"},
		"Sec-Websocket-Version":    {"13"},
		"Sec-Websocket-Extensions": {"permessage-deflate"},
	}
	out, ok := hubHeaderSnapshot(in)
	if !ok {
		t.Fatal("snapshot must fit")
	}
	for _, kept := range []string{"Cookie", "Authorization", "User-Agent"} {
		if out.Get(kept) == "" {
			t.Fatalf("%s must survive the filter", kept)
		}
	}
	for name := range out {
		if hubHopHeaders[name] || strings.HasPrefix(name, "Sec-Websocket-") {
			t.Fatalf("%s must be filtered", name)
		}
	}

	// Over 32 KiB after filtering → rejected, never truncated.
	big := http.Header{"Cookie": {strings.Repeat("x", hubSnapshotMax)}}
	if _, ok := hubHeaderSnapshot(big); ok {
		t.Fatal("oversized snapshot must reject")
	}
}

// --- the separate doorbell holder budget ------------------------------------------------

func TestHubBridgeSeparateRingBudget(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	f := &ringFlight{done: make(chan struct{})}
	dp.mu.Lock()
	dp.flights["app-x"] = f
	f.waiters = dp.waiterCap // client budget exhausted
	dp.mu.Unlock()

	// Bridge class rides its own budget: not rejected by full client cap.
	ctx, cancel := contextWithTimeout(t, 50*time.Millisecond)
	defer cancel()
	out := dp.awaitRing(withRingClass(ctx, ringClassBridge), "app-x", "/nope.sock")
	if out.kind != ringClientGone {
		t.Fatalf("bridge class with free budget must hold, got %v", out.kind)
	}

	// Bridge budget exhausted → overflow, regardless of client headroom.
	dp.mu.Lock()
	f.waiters = 0
	f.bridgeWaiters = hubBridgeWaiterCap
	dp.mu.Unlock()
	out = dp.awaitRing(withRingClass(contextBackground(), ringClassBridge), "app-x", "/nope.sock")
	if out.kind != ringOverflow {
		t.Fatalf("bridge budget overflow: got %v", out.kind)
	}

	// Client class never consumes or observes the bridge budget.
	ctx2, cancel2 := contextWithTimeout(t, 50*time.Millisecond)
	defer cancel2()
	out = dp.awaitRing(ctx2, "app-x", "/nope.sock")
	if out.kind != ringClientGone {
		t.Fatalf("client class must be unaffected by bridge budget, got %v", out.kind)
	}
}
