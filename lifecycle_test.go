package janus

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestRegistryPurgeHooksSurviveAbortedGeneration(t *testing.T) {
	reg := newAppRegistry()
	oldOwner, rejectedOwner := new(purgeOwner), new(purgeOwner)
	var oldCalls, rejectedCalls atomic.Int32
	reg.bindPurge(oldOwner, func(string) { oldCalls.Add(1) })
	reg.bindPurge(rejectedOwner, func(string) { rejectedCalls.Add(1) })

	reg.purgeApp("app-test")
	if oldCalls.Load() != 1 || rejectedCalls.Load() != 1 {
		t.Fatalf("overlap purge calls old=%d rejected=%d, want 1 each", oldCalls.Load(), rejectedCalls.Load())
	}

	// Cleanup of a rejected reload removes only that generation. The old
	// generation remains wired for every later registry mutation.
	reg.unbindPurge(rejectedOwner)
	reg.purgeApp("app-test")
	if oldCalls.Load() != 2 || rejectedCalls.Load() != 1 {
		t.Fatalf("post-abort purge calls old=%d rejected=%d, want 2/1", oldCalls.Load(), rejectedCalls.Load())
	}
}

func TestRegistryPurgeHookReloadOverlapIsRaceSafe(t *testing.T) {
	reg := newAppRegistry()
	stable := new(purgeOwner)
	var stableCalls atomic.Int32
	reg.bindPurge(stable, func(string) { stableCalls.Add(1) })

	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		owner := new(purgeOwner)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				reg.bindPurge(owner, func(string) {})
				reg.purgeApp("app-test")
				reg.unbindPurge(owner)
			}
		}()
	}
	wg.Wait()
	reg.purgeApp("app-test")
	if stableCalls.Load() == 0 {
		t.Fatal("stable generation's purge hook was lost")
	}
}

func TestReconciliationWaitsForCommit(t *testing.T) {
	auth, err := newAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	hubs := newHubSet()
	app := &App{
		logger: zap.NewNop(),
		state:  &janusState{auth: auth, hubs: hubs},
		hubs:   hubs,
		authSites: []authSiteEntry{{
			patterns: []string{"app.test"},
			cfg: &authSite{
				users: map[string]string{"alice": "unused"},
				ttl:   2 * time.Hour,
			},
		}},
	}
	alice, err := auth.mint("alice", "app.test")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := auth.mint("bob", "app.test")
	if err != nil {
		t.Fatal(err)
	}
	staleAlice, err := auth.mint("alice", "removed.test")
	if err != nil {
		t.Fatal(err)
	}
	hub := hubs.getOrCreate("app-test", nil)
	conn := newHubConn("aaaaaaaaaaaaaaaa", hub, "app.test", "", nil, 8, hubDefaultMaxFrame)
	if !hub.registerConn(conn) {
		t.Fatal("register hub connection")
	}

	app.stageReconciliation()
	if _, _, ok := auth.lookup(bob, "app.test", authDefaultTTL, false); !ok {
		t.Fatal("staging an uncommitted generation revoked bob")
	}
	select {
	case <-conn.closedCh:
		t.Fatal("staging an uncommitted generation closed the hub")
	default:
	}

	old := &App{state: app.state}
	if !old.commitSuccessor(app) {
		t.Fatal("staged reconciliation did not commit")
	}
	if _, _, ok := auth.lookup(bob, "app.test", authDefaultTTL, false); ok {
		t.Fatal("committed reconciliation kept removed user bob")
	}
	if _, _, ok := auth.lookup(alice, "app.test", authDefaultTTL, false); !ok {
		t.Fatal("committed reconciliation removed retained user alice")
	}
	if _, _, ok := auth.lookup(staleAlice, "removed.test", authDefaultTTL, false); ok {
		t.Fatal("same-name user on another site kept a removed-host session")
	}
	select {
	case <-conn.closedCh:
	case <-time.After(time.Second):
		t.Fatal("committed reconciliation did not close disabled hub")
	}
	if app.commitReconciliation() {
		t.Fatal("reconciliation committed more than once")
	}
}

func TestAbortedGenerationStopCannotCommitItself(t *testing.T) {
	st, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Destruct() }()

	active := &App{state: st}
	rejected := &App{state: st, logger: zap.NewNop(), hubs: st.hubs}
	rejected.stageReconciliation()
	if rejected.commitSuccessor(active) {
		t.Fatal("rejected generation committed while the old generation remained active")
	}
	rejected.reconcileMu.Lock()
	pending := rejected.reconcilePending
	rejected.reconcileMu.Unlock()
	if !pending {
		t.Fatal("rejected generation unexpectedly consumed its pending reconciliation")
	}
}

func TestStateDestructClosesDataPlaneIdleConnections(t *testing.T) {
	closed := make(chan struct{})
	idle := make(chan struct{})
	var idleOnce, closeOnce sync.Once
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	origin.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateIdle:
			idleOnce.Do(func() { close(idle) })
		case http.StateClosed:
			closeOnce.Do(func() { close(closed) })
		}
	}
	origin.Start()
	defer origin.Close()

	st, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	st.dp.transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", origin.Listener.Addr().String())
	}
	rec, err := st.registry.create("idle", []string{"idle.test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.registry.setUpstreams(rec.ID, []Upstream{{Path: "/unused/by/test.sock"}}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://idle.test/", nil)
	rr := httptest.NewRecorder()
	if err := st.dp.serve(rr, req); err != nil || rr.Code != http.StatusOK {
		t.Fatalf("proxy response code=%d err=%v", rr.Code, err)
	}
	select {
	case <-idle:
	case <-time.After(time.Second):
		t.Fatal("transport connection never became idle")
	}
	if err := st.Destruct(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("state destruction left the data-plane idle connection open")
	}
}

func TestStopMdnsClosesListenerOnceBeforeServeTracks(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	counted := &closeCountingListener{Listener: raw}
	ln := &idempotentListener{Listener: counted}
	srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	app := &App{mdnsSrv: srv, mdnsLn: ln}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()

	if err := app.stopMdns(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("mDNS Serve did not return during early stop")
	}
	if got := counted.closes.Load(); got != 1 {
		t.Fatalf("mDNS underlying listener closed %d times, want 1", got)
	}
}

func TestCleanupStopsStartedGenerationWithoutStop(t *testing.T) {
	newServer := func() (*http.Server, *idempotentListener, *closeCountingListener, chan error) {
		raw, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = raw.Close() })
		counted := &closeCountingListener{Listener: raw}
		ln := &idempotentListener{Listener: counted}
		srv := &http.Server{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
		done := make(chan error, 1)
		go func() { done <- srv.Serve(ln) }()
		return srv, ln, counted, done
	}

	controlSrv, controlLn, controlCounted, controlDone := newServer()
	mdnsSrv, mdnsLn, mdnsCounted, mdnsDone := newServer()
	browseCtx, browseCancel := context.WithCancel(context.Background())
	bridge, err := newAccessBridge(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	accessState := bridge.newState()
	sub := &accessSubscriber{state: accessState, lines: make(chan *accessLine, 1), done: make(chan struct{})}
	accessState.subscribers[sub] = struct{}{}
	bridge.subscribers = 1

	app := &App{
		logger:       zap.NewNop(),
		controlSrvs:  []*controlServer{{mode: "local", server: controlSrv, ln: controlLn, conns: map[net.Conn]struct{}{}}},
		mdnsSrv:      mdnsSrv,
		mdnsLn:       mdnsLn,
		browseCtx:    browseCtx,
		browseCancel: browseCancel,
		accessStreams: map[*accessSubscriber]struct{}{
			sub: {},
		},
	}
	app.accessStreamsWG.Add(1)
	go func() {
		<-sub.done
		app.accessStreamsMu.Lock()
		delete(app.accessStreams, sub)
		app.accessStreamsMu.Unlock()
		app.accessStreamsWG.Done()
	}()

	// Model Caddy rejecting a candidate after Start: Cleanup is invoked
	// directly, with no preceding Stop.
	if err := app.Cleanup(); err != nil {
		t.Fatal(err)
	}
	for name, done := range map[string]<-chan error{"control": controlDone, "mDNS": mdnsDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatalf("%s Serve did not return", name)
		}
	}
	select {
	case <-browseCtx.Done():
	default:
		t.Fatal("Cleanup did not cancel browse work")
	}
	select {
	case <-sub.done:
	default:
		t.Fatal("Cleanup did not wake access subscriber")
	}
	if !sub.detached || sub.reason != "generation_stop" {
		t.Fatalf("Cleanup left access subscriber attached: %+v", sub)
	}
	if got := bridge.subscribers; got != 0 {
		t.Fatalf("Cleanup left %d access subscribers accounted", got)
	}
	if got := controlCounted.closes.Load(); got != 1 {
		t.Fatalf("control listener closed %d times, want 1", got)
	}
	if got := mdnsCounted.closes.Load(); got != 1 {
		t.Fatalf("mDNS listener closed %d times, want 1", got)
	}

	if err := app.Cleanup(); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if got := controlCounted.closes.Load(); got != 1 {
		t.Fatalf("second Cleanup reclosed control listener: %d", got)
	}
	if got := mdnsCounted.closes.Load(); got != 1 {
		t.Fatalf("second Cleanup reclosed mDNS listener: %d", got)
	}
}
