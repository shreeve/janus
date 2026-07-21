package janus

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is an injectable heartbeat clock for deterministic TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 7, 19, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClockedRegistry(t *testing.T, ttl time.Duration) (*appRegistry, *fakeClock) {
	t.Helper()
	clk := newFakeClock()
	r := newAppRegistry()
	r.now = clk.now
	r.ttl = ttl
	return r, clk
}

func TestHeartbeatStampsClock(t *testing.T) {
	r, clk := newClockedRegistry(t, 15*time.Second)
	rec, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Without the re-stamp the app would be 20s stale; the heartbeat at
	// +10s keeps it 10s fresh at +20s.
	clk.advance(10 * time.Second)
	if err := r.heartbeat(rec.ID); err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Second)
	if reaped := r.sweepExpired(); len(reaped) != 0 {
		t.Fatalf("fresh heartbeat reaped: %v", reaped)
	}
	// 16s past the last stamp → stale → reaped.
	clk.advance(6 * time.Second)
	if reaped := r.sweepExpired(); len(reaped) != 1 || reaped[0] != rec.ID {
		t.Fatalf("want %s reaped, got %v", rec.ID, reaped)
	}
}

func TestHeartbeatUnknownID404(t *testing.T) {
	r, _ := newClockedRegistry(t, 15*time.Second)
	err := r.heartbeat("nope-000000")
	var ae *apiError
	if err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("want 404, got %v", err)
	}
}

func TestRegistrationCountsAsFirstHeartbeat(t *testing.T) {
	r, clk := newClockedRegistry(t, 15*time.Second)
	rec, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// A slow cold boot inside the TTL is never mistaken for dead.
	clk.advance(14 * time.Second)
	if reaped := r.sweepExpired(); len(reaped) != 0 {
		t.Fatalf("app reaped within TTL of registration: %v", reaped)
	}
	if _, err := r.get(rec.ID); err != nil {
		t.Fatalf("app gone within TTL: %v", err)
	}
	clk.advance(2 * time.Second)
	if reaped := r.sweepExpired(); len(reaped) != 1 {
		t.Fatalf("want reap past TTL, got %v", reaped)
	}
}

func TestTTLExpiryReapsEntryAndFreesHosts(t *testing.T) {
	r, clk := newClockedRegistry(t, 15*time.Second)
	rec, _ := r.create("shop", []string{"shop.example.com"}, "")
	if _, err := r.setUpstreams(rec.ID, []Upstream{{Path: "/run/w1.sock"}}); err != nil {
		t.Fatal(err)
	}
	clk.advance(16 * time.Second)
	if reaped := r.sweepExpired(); len(reaped) != 1 {
		t.Fatalf("want reap, got %v", reaped)
	}
	// Entry gone → 404; host no longer resolves; host reusable by a new app.
	var ae *apiError
	if _, err := r.get(rec.ID); err == nil || !asAPIError(err, &ae) || ae.Status != http.StatusNotFound {
		t.Fatalf("reaped app get: want 404, got %v", err)
	}
	if _, ok := r.resolveHost("shop.example.com"); ok {
		t.Fatal("reaped app's host still resolves")
	}
	if _, err := r.create("shop2", []string{"shop.example.com"}, ""); err != nil {
		t.Fatalf("host not freed by reap: %v", err)
	}
}

func TestFreshHeartbeatWithEmptyUpstreamsStaysRegistered(t *testing.T) {
	// Heartbeat ≠ readiness: alive but not routable keeps the registration.
	r, clk := newClockedRegistry(t, 15*time.Second)
	rec, _ := r.create("shop", []string{"shop.example.com"}, "")
	if _, err := r.setUpstreams(rec.ID, []Upstream{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ { // 50s of 5s heartbeats, over three TTLs
		clk.advance(5 * time.Second)
		if err := r.heartbeat(rec.ID); err != nil {
			t.Fatal(err)
		}
		if reaped := r.sweepExpired(); len(reaped) != 0 {
			t.Fatalf("heartbeating app reaped: %v", reaped)
		}
	}
	got, err := r.get(rec.ID)
	if err != nil {
		t.Fatalf("alive-but-not-routable app vanished: %v", err)
	}
	if len(got.Upstreams) != 0 {
		t.Fatalf("upstreams: %+v", got.Upstreams)
	}
}

func TestTTLExpiryIsPerApp(t *testing.T) {
	r, clk := newClockedRegistry(t, 15*time.Second)
	stale, _ := r.create("stale", []string{"stale.example.com"}, "")
	fresh, _ := r.create("fresh", []string{"fresh.example.com"}, "")
	if _, err := r.setUpstreams(fresh.ID, []Upstream{{Path: "/run/f.sock"}}); err != nil {
		t.Fatal(err)
	}

	clk.advance(10 * time.Second)
	if err := r.heartbeat(fresh.ID); err != nil {
		t.Fatal(err)
	}
	clk.advance(10 * time.Second) // stale is 20s old; fresh is 10s old
	reaped := r.sweepExpired()
	if len(reaped) != 1 || reaped[0] != stale.ID {
		t.Fatalf("want only %s reaped, got %v", stale.ID, reaped)
	}
	got, err := r.get(fresh.ID)
	if err != nil {
		t.Fatalf("fresh app touched by neighbor's expiry: %v", err)
	}
	if len(got.Upstreams) != 1 || got.Upstreams[0].Path != "/run/f.sock" {
		t.Fatalf("fresh app upstreams mutated: %+v", got.Upstreams)
	}
	if _, ok := r.resolveHost("fresh.example.com"); !ok {
		t.Fatal("fresh app's host stopped resolving")
	}
}

func TestSweeperReapsInBackground(t *testing.T) {
	r := newAppRegistry()
	r.ttl = 60 * time.Millisecond // sweep ticks every 20ms
	rec, err := r.create("shop", []string{"shop.example.com"}, "")
	if err != nil {
		t.Fatal(err)
	}
	r.startSweeper(nil)
	defer r.stopSweeper()

	waitFor(t, "sweeper to reap the silent app", func() bool {
		_, err := r.get(rec.ID)
		return err != nil
	})
	if _, ok := r.resolveHost("shop.example.com"); ok {
		t.Fatal("host survived the sweep")
	}
}

func TestSweeperShutdownIsClean(t *testing.T) {
	r := newAppRegistry()
	r.ttl = 30 * time.Millisecond
	r.startSweeper(nil)
	time.Sleep(50 * time.Millisecond) // let it tick at least once

	done := make(chan struct{})
	go func() {
		r.stopSweeper()
		r.stopSweeper() // second stop is a no-op
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stopSweeper did not return; sweeper goroutine leaked")
	}
	// Stopping before any Start is also a no-op.
	newAppRegistry().stopSweeper()
}

// --- HTTP layer ------------------------------------------------------------

func TestHeartbeatHTTP(t *testing.T) {
	mux := newTestControlMux(t)
	code, body := doJSON(t, mux, http.MethodPost, "/1.0/apps",
		`{"name":"shop","hosts":["shop.example.com"]}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, body)
	}
	id, _ := body["id"].(string)

	// Empty body → 204 with no payload.
	req := httptest.NewRequest(http.MethodPost, "/1.0/apps/"+id+"/heartbeat", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent || rr.Body.Len() != 0 {
		t.Fatalf("heartbeat: want empty 204, got %d %q", rr.Code, rr.Body.String())
	}

	// Unknown id → 404.
	code, _ = doJSON(t, mux, http.MethodPost, "/1.0/apps/shop-zzzzzz/heartbeat", "")
	if code != http.StatusNotFound {
		t.Fatalf("heartbeat unknown id: want 404, got %d", code)
	}

	// A body is not part of the protocol → 400.
	req = httptest.NewRequest(http.MethodPost, "/1.0/apps/"+id+"/heartbeat",
		strings.NewReader(`{"beat":true}`))
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("heartbeat with body: want 400, got %d", rr.Code)
	}
}

func TestHeartbeatTTLFromEnv(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
		ok   bool
	}{
		{"", defaultHeartbeatTTL, true},
		{"2s", 2 * time.Second, true},
		{"150ms", 150 * time.Millisecond, true},
		{"0", 0, false},
		{"-5s", 0, false},
		{"fifteen", 0, false},
		{"15", 0, false}, // bare number is not a Go duration
	}
	for _, tt := range cases {
		t.Setenv(heartbeatTTLEnv, tt.val)
		got, err := heartbeatTTLFromEnv()
		if tt.ok {
			if err != nil || got != tt.want {
				t.Fatalf("%q: want %v, got %v err %v", tt.val, tt.want, got, err)
			}
		} else if err == nil {
			t.Fatalf("%q: want loud rejection, got %v", tt.val, got)
		}
	}
}
