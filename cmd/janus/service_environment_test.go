package main

import (
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
)

func TestStopExplicitAddressWithBrokenScope(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	os.MkdirAll(p.run, 0755)
	os.WriteFile(p.scope, []byte("broken"), 0644)
	ln, e := net.Listen("unix", p.admin)
	if e != nil {
		t.Fatal(e)
	}
	var calls atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(200) })}
	go srv.Serve(ln)
	defer srv.Close()
	output, e := run(t, "stop", "--address", "unix/"+p.admin)
	t.Logf("requests=%d error=%v output=%s", calls.Load(), e, output)
	if e != nil || calls.Load() != 1 {
		t.Fatal("explicit-address stop was blocked by unrelated broken scope")
	}
}
func TestCAEnvironmentPrecedence(t *testing.T) {
	p := isolatedHome(t)
	seedAt(t, p)
	actual := t.TempDir()
	fileValue := t.TempDir()
	t.Setenv("XDG_DATA_HOME", actual)
	os.WriteFile(p.env, []byte("XDG_DATA_HOME="+fileValue+"\n"), 0600)
	reported := serviceCARoot(p)
	if e := loadEnvFile(p.env); e != nil {
		t.Fatal(e)
	}
	running := localCARoot()
	t.Logf("status path=%s service environment path=%s", reported, running)
	if reported != running {
		t.Fatal("status overrides inherited environment although service loader preserves it")
	}
}
func TestCAHomePrecedence(t *testing.T) {
	p := isolatedHome(t)
	seedAt(t, p)
	t.Setenv("XDG_DATA_HOME", "")
	os.Unsetenv("XDG_DATA_HOME")
	fileValue := t.TempDir()
	os.WriteFile(p.env, []byte("HOME="+fileValue+"\n"), 0600)
	reported := serviceCARoot(p)
	if e := loadEnvFile(p.env); e != nil {
		t.Fatal(e)
	}
	running := localCARoot()
	t.Logf("status path=%s service environment path=%s", reported, running)
	if reported != running {
		t.Fatal("status overrides HOME although supervisor always provides it")
	}
}
