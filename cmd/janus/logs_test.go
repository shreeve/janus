package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The last lines come out in order, whatever the chunking; a log shorter
// than asked prints whole; an empty one prints nothing.
func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "janus.log")
	var b strings.Builder
	for i := 1; i <= 3000; i++ {
		fmt.Fprintf(&b, `{"level":"info","n":%d,"pad":"%s"}`+"\n", i, strings.Repeat("x", 40))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	off, err := printTail(&out, path, 3)
	if err != nil || off != int64(b.Len()) {
		t.Fatalf("printTail: %d %v", off, err)
	}
	if got := out.String(); !strings.HasPrefix(got, `{"level":"info","n":2998,`) || !strings.HasSuffix(got, `{"level":"info","n":3000,"pad":"`+strings.Repeat("x", 40)+`"}`+"\n") || strings.Count(got, "\n") != 3 {
		t.Errorf("tail 3:\n%s", got)
	}
	out.Reset()
	if _, err := printTail(&out, path, 5000); err != nil || strings.Count(out.String(), "\n") != 3000 {
		t.Errorf("tail beyond length: %d lines, %v", strings.Count(out.String(), "\n"), err)
	}
	empty := filepath.Join(t.TempDir(), "empty.log")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if _, err := printTail(&out, empty, 10); err != nil || out.Len() != 0 {
		t.Errorf("empty: %q %v", out.String(), err)
	}
}

// A follow prints what arrives, whole lines only, and continues from the
// start of the new file when the log rolls over.
func TestFollowFileAcrossRollover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "janus.log")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- followFile(ctx, &out, path, 4) }()
	appendTo := func(s string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprint(f, s)
		f.Close()
	}
	appendTo("two\nthr")
	waitFor(t, func() bool { return out.String() == "two\n" })
	appendTo("ee\n")
	waitFor(t, func() bool { return out.String() == "two\nthree\n" })
	// Roll: the file is renamed away and a new one appears at the path.
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("four\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return out.String() == "two\nthree\nfour\n" })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// The verb reads the service log, falls back to the supervisor's capture
// when the edge has not written its own, and says so.
func TestLogsVerb(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	if _, err := run(t, "logs"); err == nil || !strings.Contains(err.Error(), "has not run here") {
		t.Errorf("no logs at all: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(p.sup), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.sup, []byte("Error: the host firewall for localhost is not in place\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "logs")
	if err != nil || !strings.Contains(out, "supervisor's capture") || !strings.Contains(out, "host firewall for localhost") {
		t.Errorf("supervisor fallback: %v\n%s", err, out)
	}
	if err := os.WriteFile(p.log, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := run(t, "logs", "-n", "2"); err != nil || out != "b\nc\n" {
		t.Errorf("logs -n 2: %v %q", err, out)
	}
	if out, err := run(t, "logs", "--supervisor"); err != nil || !strings.HasPrefix(out, "Error: the host firewall") {
		t.Errorf("logs --supervisor: %v %q", err, out)
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
