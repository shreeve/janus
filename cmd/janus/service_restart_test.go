package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"os"
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
