package janus

import (
	"bufio"
	"context"
	"errors"
	"io"
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
// A short MkdirTemp pattern keeps the path under the darwin 104-byte limit.
func startUnixHTTP(t testing.TB, handler http.Handler) string {
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

	path, st, ok := dp.acquireUpstream(ups, nil, nil)
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
	path, _, ok = dp.acquireUpstream(ups, nil, nil)
	if !ok || path != "a" {
		t.Fatalf("want a (b unhealthy), got %q ok=%v", path, ok)
	}

	// Tried entries are skipped.
	_, _, ok = dp.acquireUpstream(ups, map[string]bool{"a": true, "b": true}, nil)
	if ok {
		t.Fatal("acquired an already-tried upstream")
	}

	// Doorbells are never acquired.
	_, _, ok = dp.acquireUpstream([]Upstream{{Path: "bell", Doorbell: true}}, nil, nil)
	if ok {
		t.Fatal("acquired a doorbell as a worker")
	}
}

func TestAcquireUpstreamTieBreakUniform(t *testing.T) {
	dp, _ := newTestDataPlane(t)
	ups := []Upstream{{Path: "a"}, {Path: "b"}, {Path: "c"}}
	picks := map[string]int{}
	for range 300 {
		path, st, ok := dp.acquireUpstream(ups, nil, nil)
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
		_, st, ok := dp.acquireUpstream([]Upstream{{Path: "blocked"}}, nil, blockedApp)
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
		_, st, ok := dp.acquireUpstream([]Upstream{{Path: "other"}}, nil, otherApp)
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

// startUnixHTTPServer is startUnixHTTP with the server handed back so a test
// can kill the worker mid-hold.
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
