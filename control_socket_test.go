//go:build unix && !solaris

package janus

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// shortSockDir returns a short-lived temp dir with a SHORT path:
// t.TempDir() embeds the full test name and blows through sun_path's
// 104-byte limit on darwin ("bind: invalid argument").
func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "jctl")
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// acceptOne proves a listener's accept queue is live: dial the path and
// wait for the accept on the given listener.
func acceptOne(t *testing.T, ln net.Listener, path string) {
	t.Helper()
	accepted := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
		accepted <- err
	}()
	conn, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		t.Fatalf("dial %s: %v", path, err)
	}
	_ = conn.Close()
	select {
	case err := <-accepted:
		if err != nil {
			t.Fatalf("accept on %s: %v", path, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("accept on %s never fired", path)
	}
}

// TestControlSocketPoolSurvivesAbortedGeneration pins the aborted-reload
// contract: a generation that acquires the socket and then dies (reload
// abort) must leave the surviving generation serving AND later
// generations able to acquire. Caddy's own unix reuse map fails the
// third acquire here with "use of closed network connection" — one
// aborted reload wedged every reload after it until process restart.
func TestControlSocketPoolSurvivesAbortedGeneration(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "ctl.sock")
	pool := &controlSocketPool{}

	gen1, err := pool.acquire(path)
	if err != nil {
		t.Fatalf("gen1 acquire: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("socket file after first bind: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o200 {
		t.Fatalf("socket mode = %o, want 200", perm)
	}

	gen2, err := pool.acquire(path)
	if err != nil {
		t.Fatalf("gen2 acquire: %v", err)
	}
	// The aborted generation tears down; net/http's layered closes mean
	// Close can fire more than once — the second must not double-release.
	if err := gen2.Close(); err != nil {
		t.Fatalf("gen2 close: %v", err)
	}
	_ = gen2.Close()

	// The survivor still accepts and the file is intact.
	acceptOne(t, gen1, path)

	// The next generation acquires cleanly — the poisoned-map wedge.
	gen3, err := pool.acquire(path)
	if err != nil {
		t.Fatalf("gen3 acquire after aborted generation: %v", err)
	}
	acceptOne(t, gen3, path)

	// Retire the survivor (successful handoff); gen3 keeps the socket.
	if err := gen1.Close(); err != nil {
		t.Fatalf("gen1 close: %v", err)
	}
	acceptOne(t, gen3, path)

	// Last release unlinks the file.
	if err := gen3.Close(); err != nil {
		t.Fatalf("gen3 close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("socket file after last release: %v, want ErrNotExist", err)
	}

	// A fresh acquire after total teardown rebinds from scratch.
	gen4, err := pool.acquire(path)
	if err != nil {
		t.Fatalf("gen4 acquire after teardown: %v", err)
	}
	acceptOne(t, gen4, path)
	_ = gen4.Close()
}

// TestControlSocketPoolStaleFile pins unclean-shutdown recovery: a socket
// file left by a dead process must not block the first bind.
func TestControlSocketPoolStaleFile(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "stale.sock")
	stale, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("stale bind: %v", err)
	}
	// Simulate a kill -9: the file stays, the listener is gone.
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = stale.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stale file missing before test: %v", err)
	}

	pool := &controlSocketPool{}
	ln, err := pool.acquire(path)
	if err != nil {
		t.Fatalf("acquire over stale file: %v", err)
	}
	acceptOne(t, ln, path)
	_ = ln.Close()
}

// TestControlSocketPoolPermissionBits pins the "path|bits" address form.
func TestControlSocketPoolPermissionBits(t *testing.T) {
	path := filepath.Join(shortSockDir(t), "perm.sock")
	pool := &controlSocketPool{}
	ln, err := pool.acquire(path + "|0666")
	if err != nil {
		t.Fatalf("acquire with permission bits: %v", err)
	}
	defer ln.Close()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o666 {
		t.Fatalf("socket mode = %o, want 666", perm)
	}

	// A later acquire with different bits re-chmods the live socket.
	ln2, err := pool.acquire(path + "|0222")
	if err != nil {
		t.Fatalf("re-acquire with new bits: %v", err)
	}
	defer ln2.Close()
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatalf("stat after re-acquire: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o222 {
		t.Fatalf("socket mode after re-acquire = %o, want 222", perm)
	}

	// Owner-write-less bits would lock every client out — rejected loudly.
	if _, err := pool.acquire(path + "|0444"); err == nil {
		t.Fatal("want acquire to reject permissions without owner write")
	}
}
