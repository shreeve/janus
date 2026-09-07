package main

import (
	"encoding/json"
	"errors"
	"github.com/spf13/cobra"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestServeName(t *testing.T) {
	for in, want := range map[string]string{
		"janus": "janus", "My Docs": "my-docs", "Q3_Reports (final)": "q3-reports-final", "...": "files", "Ünïcode": "n-code",
	} {
		if got := serveName(in); got != want {
			t.Errorf("serveName(%q) = %q, want %q", in, got, want)
		}
	}
	if !validServeName("a-b") || validServeName("-a") || validServeName("A") || validServeName("a.b") {
		t.Error("label validation")
	}
}

// A control server that accepts one browse registration, counts its
// heartbeats, and records its deletion.
type serveControl struct {
	mu       sync.Mutex
	browse   bool
	created  []byte
	beats    int
	deleted  bool
	conflict bool
}

func (c *serveControl) start(t *testing.T, p servicePaths) {
	t.Helper()
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case r.URL.Path == "/1.0":
			_, _ = w.Write([]byte(`{"type":"janus","browse":` + map[bool]string{true: "true", false: "false"}[c.browse] + `,"control":[{"mode":"internal","listen":"` + p.sock + `"}]}`))
		case r.URL.Path == "/1.0/apps" && r.Method == http.MethodPost:
			if c.conflict {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"host docs.local is claimed by app d1"}`))
				return
			}
			var buf strings.Builder
			b := make([]byte, 4096)
			n, _ := r.Body.Read(b)
			buf.Write(b[:n])
			c.created = []byte(buf.String())
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"s1"}`))
		case r.URL.Path == "/1.0/apps/s1/heartbeat" && r.Method == http.MethodPost:
			c.beats++
			w.WriteHeader(http.StatusNoContent)
		case r.URL.Path == "/1.0/apps/s1" && r.Method == http.MethodDelete:
			c.deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

// serve registers the directory as a browse-only heartbeat app under
// both local names, heartbeats while it runs, prints the URL, and deletes
// the registration when stopped.
func TestServeRegistersAndCleansUp(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	dir := filepath.Join(t.TempDir(), "My Docs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No edge: exit 3.
	_, err := run(t, "serve", dir)
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 {
		t.Fatalf("no edge: %v", err)
	}
	c := &serveControl{}
	c.start(t, p)
	if _, err := run(t, "serve", dir); err == nil || !strings.Contains(err.Error(), "browse off") {
		t.Fatalf("browse off: %v", err)
	}
	c.mu.Lock()
	c.browse = true
	c.mu.Unlock()
	prevHS, prevHB, prevReload := serveHandshake, serveHeartbeatPeriod(), reloadEdge
	serveHandshake = func(string) string { return "" }
	setServeHeartbeat(50 * time.Millisecond)
	reloads := 0
	reloadEdge = func(*cobra.Command, servicePaths) error { reloads++; return nil }
	t.Cleanup(func() { serveHandshake = prevHS; setServeHeartbeat(prevHB); reloadEdge = prevReload })
	serveStop = make(chan struct{})
	t.Cleanup(func() { serveStop = nil })
	go func() {
		time.Sleep(400 * time.Millisecond)
		close(serveStop)
	}()
	out, err := run(t, "serve", dir)
	if err != nil {
		t.Fatalf("serve: %v\n%s", err, out)
	}
	for _, want := range []string{"added " + localhostSitePath(p), "serving " + dir, "https://my-docs.localhost/", "janus trust", "closed " + dir} {
		if !strings.Contains(out, want) {
			t.Errorf("serve output lacks %q:\n%s", want, out)
		}
	}
	// The drop-in is janus's: written once with the *.localhost site and
	// applied by one reload; a later serve finds it and reloads nothing.
	if b, err := os.ReadFile(localhostSitePath(p)); err != nil || !strings.Contains(string(b), "*.localhost {") || reloads != 1 {
		t.Errorf("drop-in: %v reloads=%d\n%s", err, reloads, b)
	}
	if strings.Contains(out, "my-docs.local/") {
		t.Errorf("localhost mode advertised the network name:\n%s", out)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var reg struct {
		Name  string   `json:"name"`
		Hosts []string `json:"hosts"`
		Lease string   `json:"lease"`
		Files struct {
			Roots []struct {
				Path   string `json:"path"`
				Browse bool   `json:"browse"`
			} `json:"roots"`
		} `json:"files"`
		Upstreams []any `json:"upstreams"`
	}
	if err := json.Unmarshal(c.created, &reg); err != nil {
		t.Fatalf("registration body: %v\n%s", err, c.created)
	}
	if reg.Name != "my-docs" || strings.Join(reg.Hosts, " ") != "my-docs.localhost my-docs.local" || reg.Lease != "heartbeat" || len(reg.Files.Roots) != 1 || reg.Files.Roots[0].Path != dir || !reg.Files.Roots[0].Browse || len(reg.Upstreams) != 0 {
		t.Errorf("registration: %+v", reg)
	}
	if c.beats < 3 || !c.deleted {
		t.Errorf("beats=%d deleted=%v", c.beats, c.deleted)
	}
	// A taken name is refused with the holder and the way out.
	c.conflict = true
	c.mu.Unlock()
	_, err = run(t, "serve", dir)
	c.mu.Lock()
	if err == nil || !strings.Contains(err.Error(), "--name") || !strings.Contains(err.Error(), "d1") {
		t.Errorf("conflict: %v", err)
	}
	if _, err := run(t, "serve", dir, "--name", "Bad Name"); err == nil {
		t.Error("bad --name accepted")
	}
	if _, err := run(t, "serve", filepath.Join(dir, "missing")); err == nil {
		t.Error("missing directory accepted")
	}
}
