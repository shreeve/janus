package janus

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
)

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
	alice, err := auth.mint("alice", "app.test", "unused")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := auth.mint("bob", "app.test", "unused")
	if err != nil {
		t.Fatal(err)
	}
	staleAlice, err := auth.mint("alice", "removed.test", "unused")
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

// withActiveJanusApp stands in for Caddy's active configuration for one test.
func withActiveJanusApp(t *testing.T, active any, err error) {
	t.Helper()
	saved := activeJanusApp
	activeJanusApp = func() (any, error) { return active, err }
	t.Cleanup(func() { activeJanusApp = saved })
}

func reconcilePendingOf(a *App) bool {
	a.reconcileMu.Lock()
	defer a.reconcileMu.Unlock()
	return a.reconcilePending
}

// TestActivePredecessorRequiresSharedState pins which serving generation a
// starting app defers to: only another app bound to the same pooled state.
func TestActivePredecessorRequiresSharedState(t *testing.T) {
	st, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Destruct() }()
	other, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Destruct() }()

	starting := &App{state: st}
	old := &App{state: st}
	cases := []struct {
		name   string
		active any
		err    error
		want   *App
	}{
		{"lookup error", old, errors.New("no active config"), nil},
		{"no janus app", nil, nil, nil},
		{"foreign app type", "not a janus app", nil, nil},
		{"itself", starting, nil, nil},
		{"stateless app", &App{}, nil, nil},
		{"other pooled state", &App{state: other}, nil, nil},
		{"old generation", old, nil, old},
	}
	for _, tc := range cases {
		withActiveJanusApp(t, tc.active, tc.err)
		if got := starting.activePredecessor(); got != tc.want {
			t.Errorf("%s: activePredecessor = %p, want %p", tc.name, got, tc.want)
		}
		if got := starting.hasActivePredecessor(); got != (tc.want != nil) {
			t.Errorf("%s: hasActivePredecessor = %v", tc.name, got)
		}
	}
}

// TestStopCommitsTheServingSuccessor pins the reload hand-off: the old
// generation's Stop is the commit signal for the new generation's staged
// reconciliation, and nothing else commits it.
func TestStopCommitsTheServingSuccessor(t *testing.T) {
	st, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Destruct() }()
	other, err := newJanusState(zap.NewNop(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Destruct() }()

	next := &App{state: st, logger: zap.NewNop(), hubs: st.hubs}
	next.stageReconciliation()
	old := &App{state: st, logger: zap.NewNop(), hubs: st.hubs}

	// No serving config, a foreign app, or a generation over other state:
	// Stop tears down and commits nothing.
	for name, active := range map[string]any{
		"lookup error":       nil,
		"foreign app type":   "not a janus app",
		"other pooled state": &App{state: other, logger: zap.NewNop(), hubs: other.hubs},
	} {
		var lookupErr error
		if name == "lookup error" {
			lookupErr = errors.New("no active config")
		}
		withActiveJanusApp(t, active, lookupErr)
		if err := old.Stop(); err != nil {
			t.Fatalf("%s: Stop: %v", name, err)
		}
		if !reconcilePendingOf(next) {
			t.Fatalf("%s: Stop committed a generation that is not serving", name)
		}
	}

	// An aborted reload: the old generation is still the one serving, so
	// its own Stop (it is not stopping) never commits the rejected one.
	withActiveJanusApp(t, old, nil)
	if err := next.Stop(); err != nil {
		t.Fatal(err)
	}
	if !reconcilePendingOf(next) {
		t.Fatal("an aborted generation's Stop committed itself")
	}

	// A successful reload: the new generation is serving and the old one
	// stops — exactly one commit.
	withActiveJanusApp(t, next, nil)
	if err := old.Stop(); err != nil {
		t.Fatal(err)
	}
	if reconcilePendingOf(next) {
		t.Fatal("the old generation's Stop did not commit the serving successor")
	}
	if next.commitReconciliation() {
		t.Fatal("the successor's reconciliation committed twice")
	}
}
