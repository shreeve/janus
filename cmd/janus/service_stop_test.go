package main

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/caddyserver/caddy/v2"
)

func TestStopLoadsConfigEnvironment(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "service", true: "explicit"}[explicit], func(t *testing.T) {
			p := isolatedHome(t)
			withFakeItem(t, &fakeItem{})
			seedAt(t, p)
			controlServer(t, p, `[]`)
			const key = "JANUS_TEST_STOP_ADMIN"
			t.Setenv(key, "")
			os.Unsetenv(key)
			if err := os.WriteFile(p.config, []byte("{\n admin {$"+key+"}\n}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			env := p.env
			args := []string{"stop"}
			if explicit {
				env = filepath.Join(p.run, "explicit.env")
				args = append(args, "--envfile", env)
			}
			if err := os.WriteFile(env, []byte(key+"=unix/"+p.admin+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			old := caddy.DefaultAdminListen
			caddy.DefaultAdminListen = "unix/" + filepath.Join(p.run, "wrong.sock")
			t.Cleanup(func() { caddy.DefaultAdminListen = old })
			ln, err := net.Listen("unix", p.admin)
			if err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/stop" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				calls.Add(1)
			})}
			go srv.Serve(ln)
			t.Cleanup(func() { srv.Close() })
			if out, err := run(t, args...); err != nil {
				t.Fatalf("stop: %v (%s)", err, out)
			}
			if calls.Load() != 1 {
				t.Fatalf("service admin received %d requests", calls.Load())
			}
		})
	}
}
