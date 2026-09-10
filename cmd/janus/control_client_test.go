package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestControlClientReusesAndClosesConnections(t *testing.T) {
	var mu sync.Mutex
	accepted, open := 0, 0
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	srv.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		if state == http.StateNew {
			accepted++
			open++
		}
		if state == http.StateClosed {
			open--
		}
	}
	srv.Start()
	defer srv.Close()
	c := &edgeControl{client: controlClient(nil), base: srv.URL}
	defer c.Close()
	for range 12 {
		if _, status, err := c.Do("POST", "/heartbeat", nil); err != nil || status != 204 {
			t.Fatalf("%d: %v", status, err)
		}
	}
	mu.Lock()
	got := accepted
	mu.Unlock()
	if got != 1 {
		t.Fatalf("12 sequential requests opened %d connections", got)
	}
	c.Close()
	for deadline := time.Now().Add(time.Second); ; {
		mu.Lock()
		n := open
		mu.Unlock()
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d connections survived command close", n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestControlClientRejectsTruncatedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	c := &edgeControl{client: controlClient(nil), base: srv.URL}
	defer c.Close()
	if _, _, err := c.Do("POST", "/apps", nil); err == nil {
		t.Fatal("truncated response accepted")
	}
}
