package janus

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
)

// --- helpers ----------------------------------------------------------------

func newTestDataPlane(t testing.TB) (*dataPlane, *appRegistry) {
	t.Helper()
	reg := newAppRegistry()
	return newDataPlane(reg, nil), reg
}

// startUnixHTTP serves handler on a fresh unix socket and returns its path.
func startUnixHTTP(t testing.TB, handler http.Handler) string {
	t.Helper()
	sock, _ := startUnixHTTPServer(t, handler)
	return sock
}

func registerApp(t testing.TB, reg *appRegistry, host string, ups ...Upstream) string {
	t.Helper()
	rec, err := reg.create("app", []string{host}, "")
	if err != nil {
		t.Fatal(err)
	}
	if ups == nil {
		ups = []Upstream{}
	}
	if _, err := reg.setUpstreams(rec.ID, ups); err != nil {
		t.Fatal(err)
	}
	return rec.ID
}

func doServe(dp *dataPlane, method, host, path, body string) (*httptest.ResponseRecorder, error) {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://"+host+path, rd)
	rr := httptest.NewRecorder()
	err := dp.serve(rr, r)
	return rr, err
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (dp *dataPlane) testWaiters(appID string) int {
	dp.mu.Lock()
	defer dp.mu.Unlock()
	if f := dp.flights[appID]; f != nil {
		return f.waiters
	}
	return 0
}

func (dp *dataPlane) testHasState(path string) bool {
	dp.stateMu.RLock()
	defer dp.stateMu.RUnlock()
	_, ok := dp.state[path]
	return ok
}

// TestPruneStateDropsUnreferencedPaths pins the reload-epoch growth fix:
// the pooled data plane sheds per-socket state once no registered app
// references the path (the pool protocol never reuses paths, so without
// pruning every publish grows the map for the process lifetime).
func TestPruneStateDropsUnreferencedPaths(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	reg.pruneUpstreams = dp.pruneState
	id := registerApp(t, reg, "prune.test", Upstream{Path: "/run/old1.sock"}, Upstream{Path: "/run/old2.sock"})

	dp.stateMu.Lock()
	dp.state["/run/old1.sock"] = &upstreamState{}
	dp.state["/run/old2.sock"] = &upstreamState{}
	dp.stateMu.Unlock()

	// The swap retires both old paths; their state must go with them.
	if _, err := reg.setUpstreams(id, []Upstream{{Path: "/run/new1.sock"}}); err != nil {
		t.Fatal(err)
	}
	if dp.testHasState("/run/old1.sock") || dp.testHasState("/run/old2.sock") {
		t.Fatal("retired socket paths must be pruned")
	}

	// A still-referenced path survives pruning.
	dp.stateMu.Lock()
	dp.state["/run/new1.sock"] = &upstreamState{}
	dp.stateMu.Unlock()
	dp.pruneState()
	if !dp.testHasState("/run/new1.sock") {
		t.Fatal("referenced socket path must survive pruning")
	}

	// DELETE retires everything the app referenced.
	if err := reg.delete(id); err != nil {
		t.Fatal(err)
	}
	if dp.testHasState("/run/new1.sock") {
		t.Fatal("deleted app's socket paths must be pruned")
	}
}

// stateWithInflight seeds an upstreamState carrying n in-flight requests.
func stateWithInflight(n int64) *upstreamState {
	st := &upstreamState{}
	st.inflight.Store(n)
	return st
}

// unhealthyNow reports whether the state is inside its unhealthy window.
func (st *upstreamState) unhealthyNow() bool {
	until := st.unhealthyUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}

// echoUpstream answers GET / with its name and POST with received:<body>.
func echoUpstream(name string, hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			b, _ := io.ReadAll(r.Body)
			if hits != nil {
				hits.Add(1)
			}
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("received:" + string(b)))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream:" + name))
	})
}

// --- decision table ----------------------------------------------------------

func TestDataPlaneUnknownHost404(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	_, err := doServe(dp, "GET", "nope.test", "/", "")
	var he caddyhttp.HandlerError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Fatalf("want HandlerError 404, got %v", err)
	}
}

func TestDataPlaneEmptyUpstreams503(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	registerApp(t, reg, "app.test")
	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	if got := rr.Header().Get("Retry-After"); got != retryAfter {
		t.Fatalf("want Retry-After %q, got %q", retryAfter, got)
	}
}

func TestDataPlaneProxiesToWorker(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	var gotHost string
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		echoUpstream("w1", nil).ServeHTTP(w, r)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:w1" {
		t.Fatalf("want 200 upstream:w1, got %d %q", rr.Code, rr.Body.String())
	}
	if gotHost != "app.test" {
		t.Fatalf("worker saw Host %q, want app.test", gotHost)
	}

	rr, err = doServe(dp, "POST", "app.test", "/submit", "hello-body")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "received:hello-body" {
		t.Fatalf("want 200 received:hello-body, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestDataPlaneAllUnhealthy503(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dead := filepath.Join(t.TempDir(), "gone.sock") // never listened on
	registerApp(t, reg, "app.test", Upstream{Path: dead})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
	dp.stateMu.RLock()
	st := dp.state[dead]
	dp.stateMu.RUnlock()
	if st == nil || !st.unhealthyNow() {
		t.Fatal("failed dial did not mark upstream unhealthy")
	}
}

func TestDataPlaneDialFailoverToHealthyWorker(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dead := filepath.Join(t.TempDir(), "gone.sock")
	live := startUnixHTTP(t, echoUpstream("w2", nil))
	registerApp(t, reg, "app.test", Upstream{Path: dead}, Upstream{Path: live})

	// A dead dial must fail over — including for requests with a body.
	for range 4 {
		rr, err := doServe(dp, "POST", "app.test", "/submit", "payload")
		if err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusOK || rr.Body.String() != "received:payload" {
			t.Fatalf("want 200 received:payload, got %d %q", rr.Code, rr.Body.String())
		}
	}
}

func TestDataPlane502WhenWorkerMisbehavesAfterDial(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dir, err := os.MkdirTemp("", "janus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "u.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close() // dial succeeds, then the "worker" hangs up
		}
	}()
	registerApp(t, reg, "app.test", Upstream{Path: sock})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rr.Code)
	}
}

// TestWorkerDiesMidResponseMarksUnhealthy pins the health accounting for a
// worker dying after response headers landed: the body copy fails (the
// proxy aborts with http.ErrAbortHandler — ErrorHandler never runs), the
// client's partial response is already out the door, and the one required
// effect is that the socket is deselected for the unhealthy window.
func TestWorkerDiesMidResponseMarksUnhealthy(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	// Declares Content-Length 1000 but sends 500 bytes: Go's server slams
	// the connection and the proxy's body copy fails mid-response.
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.Write(make([]byte, 500))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})

	func() {
		defer func() { recover() }() // the abort panic is the server's business
		doServe(dp, "GET", "app.test", "/", "")
	}()

	dp.stateMu.RLock()
	st := dp.state[sock]
	dp.stateMu.RUnlock()
	if st == nil || !st.unhealthyNow() {
		t.Fatal("mid-response worker death did not mark the socket unhealthy")
	}
}

// --- marked 503s (worker busy / draining) --------------------------------------

// busyUpstream answers every request 503 + Rip-Worker-Busy, like a c:1
// worker at capacity.
func busyUpstream(hits *atomic.Int32) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		w.Header().Set(workerBusyHeader, "1")
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("busy"))
	})
}

func TestMarkedBusy503TriesNextUpstream(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	var bounces atomic.Int32
	busy := startUnixHTTP(t, busyUpstream(&bounces))
	free := startUnixHTTP(t, echoUpstream("w2", nil))
	registerApp(t, reg, "app.test", Upstream{Path: busy}, Upstream{Path: free})

	// Bias least_conn toward the busy worker so the bounce path runs.
	dp.state[free] = stateWithInflight(1)

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:w2" {
		t.Fatalf("want 200 upstream:w2 via retry, got %d %q", rr.Code, rr.Body.String())
	}
	if bounces.Load() != 1 {
		t.Fatalf("want exactly one bounce off the busy worker, got %d", bounces.Load())
	}
	// The marked 503 never counts toward health.
	dp.stateMu.RLock()
	st := dp.state[busy]
	dp.stateMu.RUnlock()
	if st != nil && st.unhealthyNow() {
		t.Fatal("marked busy 503 poisoned the worker's health")
	}
}

func TestAllWorkersBusy503RetryAfter(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.busyWait = 120 * time.Millisecond
	b1 := startUnixHTTP(t, busyUpstream(nil))
	b2 := startUnixHTTP(t, busyUpstream(nil))
	registerApp(t, reg, "app.test", Upstream{Path: b1}, Upstream{Path: b2})

	started := time.Now()
	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	// Workers that stay busy for the whole wait: the hold runs to its
	// deadline, then the capacity 503 — never the unhealthy one.
	if held := time.Since(started); held < dp.busyWait {
		t.Fatalf("want the request held for the busy wait (%v), answered after %v", dp.busyWait, held)
	}
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "all workers busy\n" {
		t.Fatalf("want 503 all workers busy when every worker stays busy, got %d %q", rr.Code, rr.Body.String())
	}
	if rr.Header().Get("Retry-After") != retryAfter {
		t.Fatalf("want Retry-After %q, got %q", retryAfter, rr.Header().Get("Retry-After"))
	}
	if n := capacityWaiters(reg, "app.test"); n != 0 {
		t.Fatalf("waiter left counted after its 503: %d", n)
	}
	// Busy workers stay healthy: the next request tries them again.
	for _, p := range []string{b1, b2} {
		dp.stateMu.RLock()
		st := dp.state[p]
		dp.stateMu.RUnlock()
		if st != nil && st.unhealthyNow() {
			t.Fatalf("busy bounce marked %s unhealthy", p)
		}
	}
}

func TestMarkedBusy503WithBodyForwardsToClient(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	busy := startUnixHTTP(t, busyUpstream(nil))
	registerApp(t, reg, "app.test", Upstream{Path: busy})

	// A request whose body was already streamed must not be replayed; the
	// bounce goes to the client with the internal markers stripped.
	rr, err := doServe(dp, "POST", "app.test", "/submit", "payload")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want the 503 forwarded, got %d", rr.Code)
	}
	if rr.Header().Get(workerBusyHeader) != "" || rr.Header().Get(workerDrainingHeader) != "" {
		t.Fatal("internal marker headers leaked to the client")
	}
}

func TestRipMarkScrubbedFromClientResponses(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	marked := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(ripMarkHeader, "abc")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: marked})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get(ripMarkHeader); got != "" {
		t.Fatalf("rip-mark leaked to the client: %q", got)
	}
}

func TestAuthCookieScrubRunsBeforeUpgradeHandling(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	rp := dp.newProxy("/run/test.sock")
	req := httptest.NewRequest(http.MethodGet, "http://app.test/socket", nil)
	req = req.WithContext(context.WithValue(req.Context(), authWallActiveKey{}, true))
	resp := &http.Response{
		StatusCode: http.StatusSwitchingProtocols,
		Header:     make(http.Header),
		Request:    req,
	}
	resp.Header.Add("Set-Cookie", "__Host-janus=evil")
	resp.Header.Add("Set-Cookie", "sid=ok")
	if err := rp.ModifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	got := resp.Header.Values("Set-Cookie")
	if len(got) != 1 || got[0] != "sid=ok" {
		t.Fatalf("upgrade Set-Cookie after modify: %v", got)
	}
}

func TestUnmarked503PassesThrough(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	app503 := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("app-level 503"))
	}))
	other := startUnixHTTP(t, echoUpstream("w2", nil))
	registerApp(t, reg, "app.test", Upstream{Path: app503}, Upstream{Path: other})
	dp.state[other] = stateWithInflight(1) // bias selection to app503

	// An application 503 without the marker is a real response — no retry.
	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "app-level 503" {
		t.Fatalf("want the app's own 503 verbatim, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestAcquireUpstreamLeastConn(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	ups := []Upstream{{Path: "a"}, {Path: "b"}}
	dp.state["a"] = stateWithInflight(2)
	dp.state["b"] = stateWithInflight(1)

	path, st, ok, _ := dp.acquireUpstream(ups, nil, nil)
	if !ok || path != "b" {
		t.Fatalf("want b (least conn), got %q ok=%v", path, ok)
	}
	if st != dp.state["b"] {
		t.Fatal("acquire returned a state other than the charged upstream's")
	}
	if got := dp.state["b"].inflight.Load(); got != 2 {
		t.Fatalf("want inflight charged to 2, got %d", got)
	}
	if st.proxy == nil {
		t.Fatal("acquire did not build the upstream's proxy")
	}

	// Unhealthy entries are skipped even when least loaded.
	dp.markUnhealthy(dp.state["b"])
	path, _, ok, _ = dp.acquireUpstream(ups, nil, nil)
	if !ok || path != "a" {
		t.Fatalf("want a (b unhealthy), got %q ok=%v", path, ok)
	}

	// Tried entries are skipped.
	_, _, ok, _ = dp.acquireUpstream(ups, map[string]bool{"a": true, "b": true}, nil)
	if ok {
		t.Fatal("acquired an already-tried upstream")
	}

	// Doorbells are never acquired.
	_, _, ok, _ = dp.acquireUpstream([]Upstream{{Path: "bell", Doorbell: true}}, nil, nil)
	if ok {
		t.Fatal("acquired a doorbell as a worker")
	}
}

func TestAcquireUpstreamTieBreakUniform(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	ups := []Upstream{{Path: "a"}, {Path: "b"}, {Path: "c"}}
	picks := map[string]int{}
	for range 300 {
		path, st, ok, _ := dp.acquireUpstream(ups, nil, nil)
		if !ok {
			t.Fatal("acquire failed on all-healthy ties")
		}
		st.inflight.Add(-1) // release so every round stays a three-way tie
		picks[path]++
	}
	for _, p := range []string{"a", "b", "c"} {
		// Uniform expectation is 100 each; 300 rounds put ~5σ at ±41.
		if picks[p] < 59 || picks[p] > 141 {
			t.Fatalf("tie-break not uniform: picks=%v", picks)
		}
	}
}

func TestAcquireUpstreamSelectionIsPerApp(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	blockedApp := new(sync.Mutex)
	otherApp := new(sync.Mutex)
	blockedApp.Lock()
	blockedDone := make(chan struct{})
	go func() {
		_, st, ok, _ := dp.acquireUpstream([]Upstream{{Path: "blocked"}}, nil, blockedApp)
		if ok {
			st.inflight.Add(-1)
		}
		close(blockedDone)
	}()
	defer func() {
		blockedApp.Unlock()
		<-blockedDone
	}()

	done := make(chan struct{})
	go func() {
		_, st, ok, _ := dp.acquireUpstream([]Upstream{{Path: "other"}}, nil, otherApp)
		if ok {
			st.inflight.Add(-1)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("one app's selection lock blocked an unrelated app")
	}
}

// --- the ring -----------------------------------------------------------------

func TestRingSingleFlight(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))

	var rings atomic.Int32
	var appID string
	release := make(chan struct{})
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ring" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		rings.Add(1)
		<-release
		// PUT completes before the 204, per protocol.
		if _, err := reg.setUpstreams(appID, []Upstream{{Path: worker}}); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	const n = 6
	var wg sync.WaitGroup
	codes := make([]int, n)
	bodies := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr, err := doServe(dp, "GET", "app.test", "/", "")
			if err != nil {
				t.Error(err)
				return
			}
			codes[i] = rr.Code
			bodies[i] = rr.Body.String()
		}()
	}
	waitFor(t, "all requests holding", func() bool { return dp.testWaiters(appID) == n })
	close(release)
	wg.Wait()

	if got := rings.Load(); got != 1 {
		t.Fatalf("want exactly 1 ring for %d concurrent requests, got %d", n, got)
	}
	for i := range n {
		if codes[i] != http.StatusOK || bodies[i] != "upstream:fresh" {
			t.Fatalf("holder %d: want 200 upstream:fresh, got %d %q", i, codes[i], bodies[i])
		}
	}
}

func TestRingWithholdsExpectContinueUntilWorkerReadsBody(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))
	release := make(chan struct{})
	var appID string
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		if _, err := reg.setUpstreams(appID, []Upstream{{Path: worker}}); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = dp.serve(w, r)
	}))
	t.Cleanup(srv.Close)
	address := strings.TrimPrefix(srv.URL, "http://")
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := io.WriteString(conn,
		"POST /upload HTTP/1.1\r\nHost: app.test\r\nContent-Length: 7\r\nExpect: 100-continue\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	waitFor(t, "expect request holding at doorbell", func() bool { return dp.testWaiters(appID) == 1 })
	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if line, err := reader.ReadString('\n'); err == nil {
		t.Fatalf("server solicited body before doorbell release: %q", line)
	} else if netErr, ok := err.(net.Error); !ok || !netErr.Timeout() {
		t.Fatalf("pre-release read: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	close(release)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "HTTP/1.1 100 Continue\r\n" {
		t.Fatalf("first response after release=%q", line)
	}
	if blank, err := reader.ReadString('\n'); err != nil || blank != "\r\n" {
		t.Fatalf("100 terminator=%q err=%v", blank, err)
	}
	if _, err := io.WriteString(conn, "payload"); err != nil {
		t.Fatal(err)
	}
	var response *http.Response
	for {
		response, err = http.ReadResponse(reader, &http.Request{Method: http.MethodPost})
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusContinue {
			break
		}
		_ = response.Body.Close()
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "received:payload" {
		t.Fatalf("final response=%d %q", response.StatusCode, body)
	}
}

// TestRingFlightRetiredAtomicallyWithClose pins the waiter-cap overshoot
// fix: the moment a flight leaves dp.flights, its done channel is already
// closed (both happen in one critical section, outcome published first).
// The retired ordering — delete under the lock, close after unlocking —
// left a gap where a new arrival started flight #2 while up to a full
// waiter cap of holders was still parked on flight #1.
func TestRingFlightRetiredAtomicallyWithClose(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))

	var appID string
	release := make(chan struct{})
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		reg.setUpstreams(appID, []Upstream{{Path: worker}})
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	done := make(chan struct{})
	go func() {
		defer close(done)
		rr, _ := doServe(dp, "GET", "app.test", "/", "")
		if rr.Code != http.StatusOK {
			t.Errorf("holder: want 200, got %d", rr.Code)
		}
	}()
	waitFor(t, "the holder", func() bool { return dp.testWaiters(appID) == 1 })
	dp.mu.Lock()
	f := dp.flights[appID]
	dp.mu.Unlock()
	if f == nil {
		t.Fatal("no flight in progress")
	}

	close(release)
	// Spin under the lock until the flight leaves the map; at that exact
	// observation its done channel must already be closed and the outcome
	// published — otherwise the overshoot gap is open.
	waitFor(t, "the flight to retire", func() bool {
		dp.mu.Lock()
		defer dp.mu.Unlock()
		if dp.flights[appID] == f {
			return false
		}
		select {
		case <-f.done:
		default:
			t.Error("flight left dp.flights before its done channel closed")
		}
		return true
	})
	if f.outcome.kind != ringWoke {
		t.Fatalf("outcome not published before close: %+v", f.outcome)
	}
	<-done
}

func TestRingWaiterCap(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.waiterCap = 2
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))

	var appID string
	release := make(chan struct{})
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		reg.setUpstreams(appID, []Upstream{{Path: worker}})
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rr, _ := doServe(dp, "GET", "app.test", "/", "")
			if rr.Code != http.StatusOK {
				t.Errorf("holder: want 200, got %d", rr.Code)
			}
		}()
	}
	waitFor(t, "two holders", func() bool { return dp.testWaiters(appID) == 2 })

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow: want immediate 503, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("overflow 503 missing Retry-After")
	}

	close(release)
	wg.Wait()
}

func TestRingRetryCap(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	var rings atomic.Int32
	// Answers 204 but never publishes workers: re-resolve finds the doorbell
	// again, so the holder rings again — up to the cap.
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rings.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 past ring cap, got %d", rr.Code)
	}
	if rr.Header().Get("Retry-After") == "" {
		t.Fatal("503 missing Retry-After")
	}
	if got := rings.Load(); got != int32(dp.maxRings) {
		t.Fatalf("want %d rings, got %d", dp.maxRings, got)
	}
}

func TestRingTimeout503AndHealthExclusion(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.ringTimeout = 100 * time.Millisecond
	dp.maxRings = 1
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // never answer
	}))
	registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	start := time.Now()
	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 on ring timeout, got %d", rr.Code)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("ring timeout took %v, want ~100ms", elapsed)
	}
	// Doorbell failures never enter health accounting.
	if dp.testHasState(bell) {
		t.Fatal("doorbell acquired health state; it must be excluded")
	}
}

func TestRingBootError503PassThrough(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("boot failed: kaboom on line 3"))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "kaboom on line 3") {
		t.Fatalf("boot error not forwarded, body %q", rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("content type not forwarded, got %q", got)
	}
	if dp.testHasState(bell) {
		t.Fatal("doorbell acquired health state; it must be excluded")
	}
}

func TestRingClientDisconnectAbandonsOnlyThatHolder(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))

	var appID string
	release := make(chan struct{})
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		reg.setUpstreams(appID, []Upstream{{Path: worker}})
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan struct{})
	go func() {
		defer close(gone)
		r := httptest.NewRequest("GET", "http://app.test/", nil).WithContext(ctx)
		rr := httptest.NewRecorder()
		dp.serve(rr, r)
	}()
	var survivorCode atomic.Int32
	var survivorBody atomic.Value
	done := make(chan struct{})
	go func() {
		defer close(done)
		rr, _ := doServe(dp, "GET", "app.test", "/", "")
		survivorCode.Store(int32(rr.Code))
		survivorBody.Store(rr.Body.String())
	}()
	waitFor(t, "two holders", func() bool { return dp.testWaiters(appID) == 2 })

	cancel()
	<-gone
	waitFor(t, "abandoned holder released", func() bool { return dp.testWaiters(appID) == 1 })

	close(release)
	<-done
	if survivorCode.Load() != http.StatusOK || survivorBody.Load() != "upstream:fresh" {
		t.Fatalf("survivor: want 200 upstream:fresh, got %d %v",
			survivorCode.Load(), survivorBody.Load())
	}
}

// --- plumbing ------------------------------------------------------------------

func TestNormalizeHostHeader(t *testing.T) {
	cases := map[string]string{
		"App.Example.COM":      "app.example.com",
		"app.example.com:8443": "app.example.com",
		"app.example.com.":     "app.example.com",
		"[::1]:8443":           "::1",
		"[::1]":                "[::1]", // no port: passes through whole
		"::1":                  "::1",   // bare IPv6: no single colon to strip
		"app.test:":            "app.test",
	}
	for in, want := range cases {
		if got := normalizeHostHeader(in); got != want {
			t.Errorf("normalizeHostHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSockHostRoundTrip(t *testing.T) {
	path := "/run/app/w1.sock"
	u := url.URL{Scheme: "http", Host: sockHost(path)}
	host, _, err := net.SplitHostPort(u.Host + ":80")
	if err != nil {
		t.Fatal(err)
	}
	if host != sockHost(path) {
		t.Fatalf("host mangled: %q", host)
	}
}

// A client-supplied X-Forwarded-For must never survive the hop.
//
// It does not today: ReverseProxy's Rewrite hook clears the inbound
// X-Forwarded-* from the outbound request, so SetXForwarded has nothing
// to append to and the app sees exactly one hop — the peer Janus saw.
// That is a property of the stdlib rather than of this file, which is
// precisely why it is pinned here: apps behind Janus key rate limits
// and audit trails on this header, and if a future refactor moved to a
// Director, or re-added the inbound values, the header would silently
// start carrying "<client's text>, <real peer>". The conventional read
// takes the FIRST entry, so such a regression would let an attacker
// aim any app's throttle at any address they named.
func TestInboundXForwardedIsReplacedNotAppended(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	seen := make(chan http.Header, 1)
	up := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: up})

	r := httptest.NewRequest("GET", "http://app.test/", nil)
	r.Header.Set("X-Forwarded-For", "198.51.100.9")
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "evil.example")
	r.RemoteAddr = "203.0.113.4:5555"
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, r); err != nil {
		t.Fatal(err)
	}

	h := <-seen
	if got := h.Get("X-Forwarded-For"); got != "203.0.113.4" {
		t.Fatalf("X-Forwarded-For should be the peer alone, got %q", got)
	}
	if got := h.Get("X-Forwarded-Host"); got != "app.test" {
		t.Fatalf("X-Forwarded-Host should be the request host, got %q", got)
	}
	if got := h.Get("X-Forwarded-Proto"); got == "https" {
		t.Fatalf("X-Forwarded-Proto should reflect our own hop, got %q", got)
	}
}

// --- capacity hold (every selectable worker busy) --------------------------

// startUnixHTTPServer serves handler on a fresh unix socket and returns its
// path and the server, so a test can kill the worker mid-hold. A short
// MkdirTemp pattern keeps the path under the darwin 104-byte limit.
func startUnixHTTPServer(t testing.TB, handler http.Handler) (string, *http.Server) {
	t.Helper()
	dir, err := os.MkdirTemp("", "janus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "u.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return sock, srv
}

// slotUpstream behaves like a c:1 worker: the first request in holds its one
// slot until release is closed, then answers 200; every request that finds
// the slot taken bounces a marked busy 503.
func slotUpstream(name string, release <-chan struct{}, entered chan<- struct{}, bounces *atomic.Int32) http.Handler {
	slot := make(chan struct{}, 1)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case slot <- struct{}{}:
		default:
			bounces.Add(1)
			w.Header().Set(workerBusyHeader, "1")
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte("busy"))
			return
		}
		defer func() { <-slot }()
		if entered != nil {
			entered <- struct{}{}
		}
		<-release
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("upstream:" + name))
	})
}

// capacityWaiters reports how many requests are parked on the app's gate.
func capacityWaiters(reg *appRegistry, host string) int32 {
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.apps[reg.hosts[host]].capacity.waiters.Load()
}

func TestAllWorkersBusyHoldsUntilASlotFrees(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	// With the poll parked past the wait, only A's completion can release
	// B before the deadline; a poll-only wake would surface as B's 503.
	dp.busyPoll = 10 * time.Second
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var bounces atomic.Int32
	w1 := startUnixHTTP(t, slotUpstream("w1", release, entered, &bounces))
	w2 := startUnixHTTP(t, busyUpstream(nil))
	registerApp(t, reg, "app.test", Upstream{Path: w1}, Upstream{Path: w2})

	// Request A takes w1's only slot and holds it.
	type result struct {
		rr  *httptest.ResponseRecorder
		err error
	}
	a := make(chan result, 1)
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/a", "")
		a <- result{rr, err}
	}()
	<-entered

	// Request B finds w1 busy and w2 busy: the fail-fast answer was 503.
	// It must park instead, and complete on w1 once A releases the slot.
	b := make(chan result, 1)
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/b", "")
		b <- result{rr, err}
	}()
	waitFor(t, "request B parked on the capacity gate", func() bool {
		return capacityWaiters(reg, "app.test") == 1
	})
	select {
	case <-b:
		t.Fatal("request B answered while every worker was busy")
	case <-time.After(30 * time.Millisecond):
	}

	released := time.Now()
	close(release)
	for _, ch := range []chan result{a, b} {
		res := <-ch
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.rr.Code != http.StatusOK || res.rr.Body.String() != "upstream:w1" {
			t.Fatalf("want 200 upstream:w1, got %d %q", res.rr.Code, res.rr.Body.String())
		}
	}
	if woke := time.Since(released); woke >= dp.busyWait {
		t.Fatalf("B took %v after A's release: not woken by the completion", woke)
	}
	if bounces.Load() == 0 {
		t.Fatal("B never bounced off the busy slot: the hold was not exercised")
	}
	if n := capacityWaiters(reg, "app.test"); n != 0 {
		t.Fatalf("waiters still counted after both requests finished: %d", n)
	}
}

func TestBusyHoldPollsWhenNoCompletionSignals(t *testing.T) {
	// A worker that frees without any Janus-side completion (it was busy
	// with something Janus never proxied): only the poll can notice.
	dp, reg := newTestDataPlane(t)
	freeAt := time.Now().Add(20 * time.Millisecond)
	w := startUnixHTTP(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if time.Now().Before(freeAt) {
			busyUpstream(nil).ServeHTTP(rw, r)
			return
		}
		rw.WriteHeader(http.StatusOK)
		rw.Write([]byte("upstream:w"))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: w})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:w" {
		t.Fatalf("want 200 after the worker freed, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestBusyHoldFollowsAPoolSwap(t *testing.T) {
	// The tenant replaces its workers while a request is parked (an API
	// edit under watch): the hold must re-select against the new pool.
	dp, reg := newTestDataPlane(t)
	old := startUnixHTTP(t, busyUpstream(nil))
	fresh := startUnixHTTP(t, echoUpstream("w2", nil))
	id := registerApp(t, reg, "app.test", Upstream{Path: old})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/", "")
		if err != nil {
			t.Error(err)
		}
		done <- rr
	}()
	waitFor(t, "request parked on the capacity gate", func() bool {
		return capacityWaiters(reg, "app.test") == 1
	})
	if _, err := reg.setUpstreams(id, []Upstream{{Path: fresh}}); err != nil {
		t.Fatal(err)
	}
	rr := <-done
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:w2" {
		t.Fatalf("want 200 from the swapped-in worker, got %d %q", rr.Code, rr.Body.String())
	}
}

func TestBusyHoldWaiterCapIsImmediate503(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.waiterCap = 1
	dp.busyWait = 300 * time.Millisecond
	b := startUnixHTTP(t, busyUpstream(nil))
	registerApp(t, reg, "app.test", Upstream{Path: b})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr, _ := doServe(dp, "GET", "app.test", "/", "")
		first <- rr
	}()
	waitFor(t, "first request parked", func() bool {
		return capacityWaiters(reg, "app.test") == 1
	})

	started := time.Now()
	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "all workers busy\n" {
		t.Fatalf("want the overflow 503, got %d %q", rr.Code, rr.Body.String())
	}
	if took := time.Since(started); took >= dp.busyWait {
		t.Fatalf("overflow must not wait: took %v", took)
	}
	if rr := <-first; rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("parked request past its deadline: want 503, got %d", rr.Code)
	}
}

func TestBusyHoldEndsWhenClientLeaves(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	b := startUnixHTTP(t, busyUpstream(nil))
	registerApp(t, reg, "app.test", Upstream{Path: b})

	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "http://app.test/", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	go func() {
		// Cancel once the request is parked (a deadline guards the loop;
		// the assertions below fail on their own if it never parks).
		for until := time.Now().Add(2 * time.Second); time.Now().Before(until); time.Sleep(2 * time.Millisecond) {
			if capacityWaiters(reg, "app.test") == 1 {
				break
			}
		}
		cancel()
	}()
	started := time.Now()
	if err := dp.serve(rr, r); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(started); took >= dp.busyWait {
		t.Fatalf("a departed client held its slot for the whole wait (%v)", took)
	}
	if rr.Body.Len() != 0 || rr.Header().Get("Retry-After") != "" {
		t.Fatalf("nothing should be written to a departed client, got %q", rr.Body.String())
	}
	if n := capacityWaiters(reg, "app.test"); n != 0 {
		t.Fatalf("departed client still counted as a waiter: %d", n)
	}
}

func TestBusyHoldKeepsUnhealthyDistinct(t *testing.T) {
	// The only worker bounces busy, then dies mid-hold. The next
	// re-selection fails its dial, marks it unhealthy, and the request
	// gets the logged unhealthy 503 — not the capacity one, and not the
	// whole busy wait.
	dp, reg := newTestDataPlane(t)
	dp.busyWait = time.Second
	var bounces atomic.Int32
	b, srv := startUnixHTTPServer(t, busyUpstream(&bounces))
	registerApp(t, reg, "app.test", Upstream{Path: b})

	done := make(chan *httptest.ResponseRecorder, 1)
	started := time.Now()
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/", "")
		if err != nil {
			t.Error(err)
		}
		done <- rr
	}()
	// The request bounces once, counts itself as a waiter, and re-selects
	// once more before parking. Kill the worker only after that second
	// bounce has been answered, so the close hits a parked request rather
	// than an in-flight bounce (which would read as a worker dying after
	// the dial — a 502, not the unhealthy 503 under test).
	waitFor(t, "request parked after its re-selection", func() bool {
		return capacityWaiters(reg, "app.test") == 1 && bounces.Load() >= 2
	})
	time.Sleep(10 * time.Millisecond)
	srv.Close()
	rr := <-done
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "service unavailable\n" {
		t.Fatalf("want the unhealthy 503, got %d %q", rr.Code, rr.Body.String())
	}
	if took := time.Since(started); took >= dp.busyWait {
		t.Fatalf("a dead pool should not be held for the busy wait: %v", took)
	}
	dp.stateMu.RLock()
	st := dp.state[b]
	dp.stateMu.RUnlock()
	if st == nil || !st.unhealthyNow() {
		t.Fatal("the dead worker was not marked unhealthy")
	}
}

func TestBusyHoldWakesWhenWorkerDiesMidResponse(t *testing.T) {
	// A worker dying after its headers landed never returns from the
	// proxy attempt: the copy aborts with a panic. The slot it held is
	// free all the same, so the completion signal must ride the unwind
	// and wake a parked request — with the poll parked past the wait,
	// only that signal can end B's hold before the deadline.
	dp, reg := newTestDataPlane(t)
	dp.busyPoll = 10 * time.Second
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	slot := make(chan struct{}, 1)
	var bounces atomic.Int32
	w := startUnixHTTP(t, http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		select {
		case slot <- struct{}{}:
		default:
			busyUpstream(&bounces).ServeHTTP(rw, r)
			return
		}
		defer func() { <-slot }()
		entered <- struct{}{}
		<-release
		// Declares 1000 bytes, sends 500: the server slams the connection.
		rw.Header().Set("Content-Length", "1000")
		rw.WriteHeader(http.StatusOK)
		rw.Write(make([]byte, 500))
	}))
	registerApp(t, reg, "app.test", Upstream{Path: w})

	// ReverseProxy aborts a failed body copy with a panic only when it is
	// serving under a real http.Server (it suppresses the panic for direct
	// handler calls), so the dying request must arrive through one for the
	// unwind path to run at all.
	edge := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		r.Host = "app.test"
		dp.serve(rw, r)
	}))
	edge.Config.ErrorLog = log.New(io.Discard, "", 0) // the abort panic is the server's business
	t.Cleanup(edge.Close)
	go func() {
		resp, err := http.Get(edge.URL + "/a")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	<-entered

	b := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rr, _ := doServe(dp, "GET", "app.test", "/b", "")
		b <- rr
	}()
	// B bounces once, counts itself as a waiter, re-selects once more and
	// only then parks; release the slot after that second bounce, so the
	// death lands on a parked request rather than on B's own re-selection
	// (which would find the socket unhealthy on its own, proving nothing).
	waitFor(t, "request B parked after its re-selection", func() bool {
		return capacityWaiters(reg, "app.test") == 1 && bounces.Load() >= 2
	})
	time.Sleep(10 * time.Millisecond)

	released := time.Now()
	close(release)
	rr := <-b
	// The death marked the only worker unhealthy, so the woken request
	// gets the logged unhealthy 503 — promptly, not at the busy deadline.
	if took := time.Since(released); took >= dp.busyWait {
		t.Fatalf("B ran to the busy deadline (%v): the dying worker's completion never signaled", took)
	}
	if rr.Code != http.StatusServiceUnavailable || rr.Body.String() != "service unavailable\n" {
		t.Fatalf("want the unhealthy 503 after the pool died, got %d %q", rr.Code, rr.Body.String())
	}
}

// --- published concurrency ---------------------------------------------------

func TestAcquireUpstreamSkipsSocketAtPublishedCap(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	ups := []Upstream{{Path: "a", Concurrency: 2}, {Path: "b", Concurrency: 2}}
	dp.state["a"] = stateWithInflight(2) // full
	dp.state["b"] = stateWithInflight(1) // one slot left

	path, st, ok, atCap := dp.acquireUpstream(ups, nil, nil)
	if !ok || path != "b" {
		t.Fatalf("want b (a is at its cap), got %q ok=%v", path, ok)
	}
	if !atCap {
		t.Fatal("a was passed over for capacity but atCap was not reported")
	}
	st.inflight.Add(-1)

	// Both full: nothing selectable, and the reason is capacity, not health.
	dp.state["b"].inflight.Store(2)
	_, _, ok, atCap = dp.acquireUpstream(ups, nil, nil)
	if ok || !atCap {
		t.Fatalf("want no selection with atCap=true when every socket is full, got ok=%v atCap=%v", ok, atCap)
	}

	// Unknown cap (zero) never skips: the bounce is how Janus learns.
	_, _, ok, atCap = dp.acquireUpstream([]Upstream{{Path: "a"}}, nil, nil)
	if !ok || atCap {
		t.Fatalf("an upstream with no published cap must stay selectable, got ok=%v atCap=%v", ok, atCap)
	}
	// A socket never touched has no state and no load: selectable.
	_, _, ok, _ = dp.acquireUpstream([]Upstream{{Path: "fresh", Concurrency: 1}}, nil, nil)
	if !ok {
		t.Fatal("a fresh socket with a published cap must be selectable")
	}
}

func TestPublishedCapRoutesAroundSaturatedWorkerWithoutDialing(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	var fullHits atomic.Int32
	full := startUnixHTTP(t, echoUpstream("full", &fullHits))
	free := startUnixHTTP(t, echoUpstream("free", nil))
	registerApp(t, reg, "app.test",
		Upstream{Path: full, Concurrency: 1}, Upstream{Path: free, Concurrency: 1})
	// The first worker is at its published cap; least-conn alone would
	// still consider it (and on main it would be dialed and bounce).
	dp.state[full] = stateWithInflight(1)

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:free" {
		t.Fatalf("want 200 from the worker with a free slot, got %d %q", rr.Code, rr.Body.String())
	}
	if fullHits.Load() != 0 {
		t.Fatalf("the saturated worker was dialed %d time(s); the cap should have skipped it", fullHits.Load())
	}
}

func TestPublishedCapHoldsWithoutDialing(t *testing.T) {
	// One worker, c=1, its slot taken by request A. Request B must park
	// on the capacity gate without ever reaching the worker, then land
	// on it when A's completion frees the slot.
	dp, reg := newTestDataPlane(t)
	dp.busyPoll = 10 * time.Second
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	var bounces atomic.Int32
	w := startUnixHTTP(t, slotUpstream("w", release, entered, &bounces))
	registerApp(t, reg, "app.test", Upstream{Path: w, Concurrency: 1})

	type result struct {
		rr  *httptest.ResponseRecorder
		err error
	}
	a := make(chan result, 1)
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/a", "")
		a <- result{rr, err}
	}()
	<-entered

	b := make(chan result, 1)
	go func() {
		rr, err := doServe(dp, "GET", "app.test", "/b", "")
		b <- result{rr, err}
	}()
	waitFor(t, "request B parked on the capacity gate", func() bool {
		return capacityWaiters(reg, "app.test") == 1
	})
	select {
	case <-b:
		t.Fatal("request B answered while the only slot was taken")
	case <-time.After(30 * time.Millisecond):
	}

	released := time.Now()
	close(release)
	for _, ch := range []chan result{a, b} {
		res := <-ch
		if res.err != nil {
			t.Fatal(res.err)
		}
		if res.rr.Code != http.StatusOK || res.rr.Body.String() != "upstream:w" {
			t.Fatalf("want 200 upstream:w, got %d %q", res.rr.Code, res.rr.Body.String())
		}
	}
	if woke := time.Since(released); woke >= dp.busyWait {
		t.Fatalf("B took %v after A's release: not woken by the completion", woke)
	}
	if bounces.Load() != 0 {
		t.Fatalf("the worker bounced %d request(s); with the cap published it should never have been dialed while full", bounces.Load())
	}
	if n := capacityWaiters(reg, "app.test"); n != 0 {
		t.Fatalf("waiters still counted after both requests finished: %d", n)
	}
}

func TestPublishedCapSurvivesTheControlPlane(t *testing.T) {
	// The field round-trips through the registry: a PUT that names caps
	// is what the data plane selects against.
	_, reg := newTestDataPlane(t)
	id := registerApp(t, reg, "app.test", Upstream{Path: "/run/a.sock", Concurrency: 3})
	rec, ok := reg.resolveHost("app.test")
	if !ok || len(rec.Upstreams) != 1 || rec.Upstreams[0].Concurrency != 3 {
		t.Fatalf("published concurrency did not survive the registry: %+v", rec.Upstreams)
	}
	if _, err := reg.setUpstreams(id, []Upstream{{Path: "/run/a.sock", Concurrency: -2}}); err == nil {
		t.Fatal("a negative concurrency was accepted")
	}
}

// --- response-header timeout --------------------------------------------------

// silentUpstream accepts connections, reads the request, and never answers:
// the worker that would pin a client (and its socket's slot) forever without
// a response-header bound.
func silentUpstream(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "janus")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "u.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		for _, c := range conns {
			c.Close()
		}
		mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go io.Copy(io.Discard, c) // read forever, answer never
		}
	}()
	return sock
}

// TestUpstreamResponseHeaderTimeout504 pins the bound on a worker that
// accepts and never answers: 504 with no-store, the in-flight charge
// released (a published concurrency of 1 would otherwise hold the socket at
// its cap and 503 the whole app), and the socket left healthy: one slow
// request is not a broken worker.
func TestUpstreamResponseHeaderTimeout504(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.transport.ResponseHeaderTimeout = 100 * time.Millisecond
	sock := silentUpstream(t)
	registerApp(t, reg, "app.test", Upstream{Path: sock, Concurrency: 1})

	for _, method := range []string{"GET", "POST"} {
		body := ""
		if method == "POST" {
			body = "payload"
		}
		start := time.Now()
		rr, err := doServe(dp, method, "app.test", "/", body)
		if err != nil {
			t.Fatal(err)
		}
		if rr.Code != http.StatusGatewayTimeout {
			t.Fatalf("%s: want 504, got %d %q", method, rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s: 504 Cache-Control = %q, want no-store", method, got)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("%s: timeout took %v, want ~100ms", method, elapsed)
		}
		dp.stateMu.RLock()
		st := dp.state[sock]
		dp.stateMu.RUnlock()
		if st == nil || st.unhealthyNow() {
			t.Fatalf("%s: a header timeout marked the socket unhealthy", method)
		}
		if got := st.inflight.Load(); got != 0 {
			t.Fatalf("%s: in-flight charge leaked: %d", method, got)
		}
	}
}

// TestResponseHeaderTimeoutSparesStreamsAfterHeaders pins that the bound
// covers only the wait for headers: a response whose body trickles out for
// longer than the timeout completes, and the socket stays healthy.
func TestResponseHeaderTimeoutSparesStreamsAfterHeaders(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.transport.ResponseHeaderTimeout = 100 * time.Millisecond
	sock := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		for i := range 3 {
			time.Sleep(100 * time.Millisecond)
			io.WriteString(w, "chunk"+string(rune('0'+i))+";")
			w.(http.Flusher).Flush()
		}
	}))
	registerApp(t, reg, "app.test", Upstream{Path: sock})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "chunk0;chunk1;chunk2;" {
		t.Fatalf("want the whole slow stream, got %d %q", rr.Code, rr.Body.String())
	}
	dp.stateMu.RLock()
	st := dp.state[sock]
	dp.stateMu.RUnlock()
	if st == nil || st.unhealthyNow() {
		t.Fatal("a slow body after prompt headers marked the worker unhealthy")
	}
}

// TestResponseHeaderTimeoutSparesTheRing pins that the doorbell keeps its
// own bound: a ring answered after the worker header timeout still wakes the
// held request.
func TestResponseHeaderTimeoutSparesTheRing(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	dp.transport.ResponseHeaderTimeout = 50 * time.Millisecond
	worker := startUnixHTTP(t, echoUpstream("fresh", nil))
	var appID string
	bell := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		if _, err := reg.setUpstreams(appID, []Upstream{{Path: worker}}); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	appID = registerApp(t, reg, "app.test", Upstream{Path: bell, Doorbell: true})

	rr, err := doServe(dp, "GET", "app.test", "/", "")
	if err != nil {
		t.Fatal(err)
	}
	if rr.Code != http.StatusOK || rr.Body.String() != "upstream:fresh" {
		t.Fatalf("want 200 upstream:fresh after a slow ring, got %d %q", rr.Code, rr.Body.String())
	}
}

// TestResponseHeaderTimeoutOrdering pins the relation between the bounds:
// the worker header wait outlasts the ring hold and the hub bridge POST
// deadline, so neither of those ever meets it, and the production value is
// the documented 120s.
func TestResponseHeaderTimeoutOrdering(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	if got := dp.transport.ResponseHeaderTimeout; got != 120*time.Second {
		t.Fatalf("ResponseHeaderTimeout = %v, want 120s", got)
	}
	if upstreamResponseHeaderTimeout <= hubBridgeTimeout || upstreamResponseHeaderTimeout <= defaultRingTimeout {
		t.Fatalf("header timeout %v must exceed the bridge (%v) and ring (%v) bounds",
			upstreamResponseHeaderTimeout, hubBridgeTimeout, defaultRingTimeout)
	}
}

// --- Janus-generated responses are never cacheable -----------------------------

func TestJanusGeneratedResponsesAreNoStore(t *testing.T) {
	dp, reg := newTestDataPlane(t)
	noStore := func(what string, rr *httptest.ResponseRecorder) {
		t.Helper()
		if got := rr.Header().Get("Cache-Control"); got != "no-store" {
			t.Fatalf("%s Cache-Control = %q, want no-store", what, got)
		}
	}

	// Unknown host: a 404 that must not outlive an empty registry.
	rr, err := doServe(dp, "GET", "nope.test", "/", "")
	var he caddyhttp.HandlerError
	if !errors.As(err, &he) || he.StatusCode != http.StatusNotFound {
		t.Fatalf("want HandlerError 404, got %v", err)
	}
	noStore("unknown-host 404", rr)

	// Empty upstreams (down on purpose).
	registerApp(t, reg, "empty.test")
	rr, err = doServe(dp, "GET", "empty.test", "/", "")
	if err != nil || rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("empty: want 503, got %d %v", rr.Code, err)
	}
	noStore("empty-upstreams 503", rr)

	// All upstreams unhealthy.
	registerApp(t, reg, "dead.test", Upstream{Path: filepath.Join(t.TempDir(), "gone.sock")})
	rr, err = doServe(dp, "GET", "dead.test", "/", "")
	if err != nil || rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("dead: want 503, got %d %v", rr.Code, err)
	}
	noStore("all-unhealthy 503", rr)

	// Ring failure (a doorbell nobody listens on).
	dp.maxRings = 1
	registerApp(t, reg, "bell.test", Upstream{Path: filepath.Join(t.TempDir(), "bell.sock"), Doorbell: true})
	rr, err = doServe(dp, "GET", "bell.test", "/", "")
	if err != nil || rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("ring: want 503, got %d %v", rr.Code, err)
	}
	noStore("ring-failure 503", rr)

	// Capacity: the one worker sits at its published cap past the busy wait.
	dp.busyWait = 50 * time.Millisecond
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	sock := startUnixHTTP(t, slotUpstream("w", release, entered, nil))
	registerApp(t, reg, "busy.test", Upstream{Path: sock, Concurrency: 1})
	first := make(chan struct{})
	go func() {
		defer close(first)
		_, _ = doServe(dp, "GET", "busy.test", "/", "")
	}()
	<-entered
	rr, err = doServe(dp, "GET", "busy.test", "/", "")
	close(release)
	<-first
	if err != nil || rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy: want 503, got %d %v", rr.Code, err)
	}
	noStore("capacity 503", rr)
}

// --- X-Forwarded-* and trusted_proxies -----------------------------------------

// forwardedFor runs one request through the data plane with the given peer,
// trust verdict (nil: no Caddy vars at all), and inbound headers, and
// returns the headers the worker saw.
func forwardedFor(t *testing.T, trusted *bool, remote string, overTLS bool, inbound http.Header) http.Header {
	t.Helper()
	dp, reg := newTestDataPlane(t)
	seen := make(chan http.Header, 1)
	up := startUnixHTTP(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	registerApp(t, reg, "app.test", Upstream{Path: up})

	target := "http://app.test/"
	if overTLS {
		target = "https://app.test/"
	}
	r := httptest.NewRequest("GET", target, nil)
	for k, vs := range inbound {
		r.Header[k] = vs
	}
	r.RemoteAddr = remote
	if trusted != nil {
		r = r.WithContext(context.WithValue(r.Context(), caddyhttp.VarsCtxKey, map[string]any{
			caddyhttp.TrustedProxyVarKey: *trusted,
		}))
	}
	rr := httptest.NewRecorder()
	if err := dp.serve(rr, r); err != nil {
		t.Fatal(err)
	}
	return <-seen
}

// TestTrustedProxyXForwardedIsAppended pins Caddy's reverse_proxy rule for a
// peer inside the server's trusted_proxies: the inbound chain is kept and
// the peer appended; inbound Proto and Host are kept; X-Real-IP never is.
func TestTrustedProxyXForwardedIsAppended(t *testing.T) {
	trusted := true
	h := forwardedFor(t, &trusted, "10.0.0.2:4444", false, http.Header{
		"X-Forwarded-For":   {"198.51.100.9", "192.0.2.7"},
		"X-Forwarded-Proto": {"https"},
		"X-Forwarded-Host":  {"shop.example.com"},
		"X-Real-Ip":         {"198.51.100.9"},
	})
	if got := h.Get("X-Forwarded-For"); got != "198.51.100.9, 192.0.2.7, 10.0.0.2" {
		t.Fatalf("trusted peer: X-Forwarded-For = %q, want the chain plus the peer", got)
	}
	if got := h.Get("X-Forwarded-Proto"); got != "https" {
		t.Fatalf("trusted peer: X-Forwarded-Proto = %q, want the inbound https", got)
	}
	if got := h.Get("X-Forwarded-Host"); got != "shop.example.com" {
		t.Fatalf("trusted peer: X-Forwarded-Host = %q, want the inbound host", got)
	}
	if got := h.Values("X-Real-Ip"); len(got) != 0 {
		t.Fatalf("X-Real-IP survived a trusted hop: %q", got)
	}

	// A trusted peer that sent nothing gets Janus's own hop.
	h = forwardedFor(t, &trusted, "10.0.0.2:4444", true, nil)
	if h.Get("X-Forwarded-For") != "10.0.0.2" || h.Get("X-Forwarded-Proto") != "https" || h.Get("X-Forwarded-Host") != "app.test" {
		t.Fatalf("trusted peer without inbound headers: %v", h)
	}
}

// TestUntrustedPeerXForwardedIsReplaced pins the default: without a trusted
// verdict (explicitly untrusted, or no Caddy vars at all) every inbound
// X-Forwarded-* claim and X-Real-IP is discarded.
func TestUntrustedPeerXForwardedIsReplaced(t *testing.T) {
	untrusted := false
	for name, trust := range map[string]*bool{"untrusted": &untrusted, "no-caddy-vars": nil} {
		h := forwardedFor(t, trust, "[2001:db8::5%en0]:4444", false, http.Header{
			"X-Forwarded-For":   {"198.51.100.9"},
			"X-Forwarded-Proto": {"https"},
			"X-Forwarded-Host":  {"evil.example"},
			"X-Real-Ip":         {"198.51.100.9"},
		})
		if got := h.Get("X-Forwarded-For"); got != "2001:db8::5" {
			t.Fatalf("%s: X-Forwarded-For = %q, want the peer alone", name, got)
		}
		if got := h.Get("X-Forwarded-Proto"); got != "http" {
			t.Fatalf("%s: X-Forwarded-Proto = %q, want our own hop's http", name, got)
		}
		if got := h.Get("X-Forwarded-Host"); got != "app.test" {
			t.Fatalf("%s: X-Forwarded-Host = %q, want the request host", name, got)
		}
		if got := h.Values("X-Real-Ip"); len(got) != 0 {
			t.Fatalf("%s: client X-Real-IP survived: %q", name, got)
		}
	}
}
