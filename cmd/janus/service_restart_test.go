package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestRestartStopsForegroundControlBeforeStarting(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/1.0/apps" {
			fmt.Fprint(w, `[]`)
		} else {
			fmt.Fprintf(w, `{"type":"janus","control":[{"mode":"internal","listen":%q}]}`, p.sock)
		}
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	calls := []string{}
	stop := &cobra.Command{RunE: func(*cobra.Command, []string) error {
		calls = append(calls, "stop")
		go func() { time.Sleep(50 * time.Millisecond); srv.Close() }()
		return nil
	}}
	start := &cobra.Command{RunE: func(*cobra.Command, []string) error {
		if controlReachable(p) {
			t.Error("start raced the old control listener")
		}
		calls = append(calls, "start")
		return nil
	}}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := restartEdge(stop, start, p, cmd); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(calls) != "[stop start]" || strings.Contains(out.String(), "not running") {
		t.Fatalf("calls=%v output=%s", calls, out.String())
	}
}

// Under its item, restart is the manager's: no stop, no start, one
// restart, and the edge answering on control afterwards.
func TestRestartUnderItemIsTheManagers(t *testing.T) {
	p := isolatedHome(t)
	f := &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()}
	withFakeItem(t, f)
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"type":"janus","control":[{"mode":"internal","listen":%q}]}`, p.sock)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	calls := []string{}
	stop := &cobra.Command{RunE: func(*cobra.Command, []string) error { calls = append(calls, "stop"); return nil }}
	start := &cobra.Command{RunE: func(*cobra.Command, []string) error { calls = append(calls, "start"); return nil }}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := restartEdge(stop, start, p, cmd); err != nil {
		t.Fatal(err)
	}
	if f.restarts != 1 || len(calls) != 0 || !strings.Contains(out.String(), "janus restarted under fake") {
		t.Fatalf("restarts=%d calls=%v output=%s", f.restarts, calls, out.String())
	}
}

// A registered item that is not running the edge (stopped, or a pidfile
// edge to hand over) takes the stop-and-start path, as before.
func TestRestartUnderStoppedItemStartsIt(t *testing.T) {
	p := isolatedHome(t)
	f := &fakeItem{reg: true, isLoaded: true}
	withFakeItem(t, f)
	calls := []string{}
	stop := &cobra.Command{RunE: func(*cobra.Command, []string) error { calls = append(calls, "stop"); return nil }}
	start := &cobra.Command{RunE: func(*cobra.Command, []string) error { calls = append(calls, "start"); return nil }}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := restartEdge(stop, start, p, cmd); err != nil {
		t.Fatal(err)
	}
	if f.restarts != 0 || fmt.Sprint(calls) != "[start]" || !strings.Contains(out.String(), "janus was not running") {
		t.Fatalf("restarts=%d calls=%v output=%s", f.restarts, calls, out.String())
	}
}

// An edge that ignores its stop is ended: SIGTERM, then SIGKILL.
func TestForceStopEndsAProcess(t *testing.T) {
	// A child that ignores SIGTERM stands in for an edge draining forever.
	child := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() })
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	if err := forceStop(child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("child survived forceStop")
	}
	if err := forceStop(0); err == nil {
		t.Error("an unknown pid is an error, not a silent success")
	}
}

// The seed bounds the grace period, so a stop can always finish.
func TestSeedBoundsTheGracePeriod(t *testing.T) {
	p := isolatedHome(t)
	if !strings.Contains(seedConfig(p), "\n\tgrace_period 10s\n") {
		t.Fatalf("seed lacks a bounded grace period:\n%s", seedConfig(p))
	}
}
