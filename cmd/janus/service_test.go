package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestServicePathsUser(t *testing.T) {
	p := servicePathsFor(false, "/home/ann", func(string) string { return "" })
	want := map[string]string{
		"config": "/home/ann/.config/janus/Caddyfile",
		"state":  "/home/ann/.local/state/janus",
		"log":    "/home/ann/.local/state/janus/log/janus.log",
		"sock":   "/home/ann/.local/state/janus/run/janus.sock",
		"pid":    "/home/ann/.local/state/janus/run/janus.pid",
		"home":   "/home/ann",
	}
	got := map[string]string{"config": p.config, "state": p.state, "log": p.log, "sock": p.sock, "pid": p.pid, "home": p.home}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	// XDG overrides move the config and state roots.
	env := map[string]string{"XDG_CONFIG_HOME": "/cfg", "XDG_STATE_HOME": "/st"}
	p = servicePathsFor(false, "/home/ann", func(k string) string { return env[k] })
	if p.config != "/cfg/janus/Caddyfile" || p.state != "/st/janus" {
		t.Errorf("xdg: config %q state %q", p.config, p.state)
	}
}

func TestServicePathsRoot(t *testing.T) {
	p := servicePathsFor(true, "/var/root", func(string) string { return "" })
	if p.config != "/etc/janus/Caddyfile" || p.state != "/var/lib/janus" || p.log != "/var/log/janus/janus.log" || p.home != "/var/lib/janus" {
		t.Errorf("root paths: %+v", p)
	}
}

// The seed is a runnable Caddyfile: it adapts and validates, control and
// log pointed at the service's paths.
func TestSeedConfigValidates(t *testing.T) {
	home := t.TempDir()
	p := servicePathsFor(false, home, func(string) string { return "" })
	for _, dir := range []string{filepath.Dir(p.config), p.run, filepath.Dir(p.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seed := seedConfig(p)
	if !strings.Contains(seed, "control internal "+p.sock) || !strings.Contains(seed, "output file "+p.log) || !strings.Contains(seed, "import "+p.sites+"/*.caddy") {
		t.Fatalf("seed does not name the service paths:\n%s", seed)
	}
	if err := os.WriteFile(p.config, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := validateInProcess(p.config); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestRootHasServiceVerbs(t *testing.T) {
	root := newRootCommand()
	for _, name := range []string{"start", "stop", "restart", "autostart", "status", "reload", "validate", "run"} {
		if cmd, _, err := root.Find([]string{name}); err != nil || cmd == nil || cmd.Name() != name {
			t.Errorf("no %s command", name)
		}
	}
	// start under the service is described as such; the Long text is
	// Janus's, not Caddy's "keep the terminal open" advice.
	start, _, _ := root.Find([]string{"start"})
	if !strings.Contains(start.Long, "autostart") {
		t.Errorf("start help does not mention autostart:\n%s", start.Long)
	}
}

// validate/stop/reload default --config to the service Caddyfile when it
// exists; a --config given wins.
func TestValidateDefaultsToServiceConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	p := currentPaths()
	if !strings.HasPrefix(p.config, home) {
		t.Fatalf("paths not under the test HOME: %s", p.config)
	}
	for _, dir := range []string{filepath.Dir(p.config), p.run, filepath.Dir(p.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p.config, []byte(seedConfig(p)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	root := newRootCommand()
	root.SetOut(&out)
	root.SetArgs([]string{"validate"})
	if err := root.Execute(); err != nil {
		t.Fatalf("validate with the service config as default: %v", err)
	}
	validate, _, _ := root.Find([]string{"validate"})
	if got, _ := validate.Flags().GetString("config"); got != p.config {
		t.Errorf("--config defaulted to %q, want %q", got, p.config)
	}
	if got, _ := validate.Flags().GetString("adapter"); got != "caddyfile" {
		t.Errorf("--adapter defaulted to %q, want caddyfile", got)
	}
}

// fakeItem stands in for launchd/systemd.
type fakeItem struct {
	reg, isLoaded bool
	pid           int
	loads, kicks  int
	removed       bool
}

func (f *fakeItem) name() string        { return "fake" }
func (f *fakeItem) file() string        { return "/fake/item" }
func (f *fakeItem) registered() bool    { return f.reg }
func (f *fakeItem) loaded() (bool, int) { return f.isLoaded, f.pid }
func (f *fakeItem) register(servicePaths, string) error {
	f.reg = true
	return nil
}
func (f *fakeItem) load() error { f.loads++; f.isLoaded = true; return nil }
func (f *fakeItem) kick() error { f.kicks++; return nil }
func (f *fakeItem) unregister() (bool, error) {
	had := f.reg
	f.reg, f.removed = false, true
	return had, nil
}

func withFakeItem(t *testing.T, f *fakeItem) {
	t.Helper()
	prev, prevValidate := itemFor, validateConfig
	itemFor = func(servicePaths) serviceItem { return f }
	// Never re-exec the test binary: as 'validate' it would run the tests.
	validateConfig = validateInProcess
	t.Cleanup(func() { itemFor, validateConfig = prev, prevValidate })
}

func isolatedHome(t *testing.T) servicePaths {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	return currentPaths()
}

func TestStartUnderItem(t *testing.T) {
	isolatedHome(t)
	f := &fakeItem{reg: true}
	withFakeItem(t, f)

	// Not loaded: start loads it.
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"start"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if f.loads != 1 || f.kicks != 0 || !strings.Contains(out.String(), "started under fake") {
		t.Errorf("loads=%d kicks=%d out=%q", f.loads, f.kicks, out.String())
	}

	// Loaded but exited (after a stop): start kicks it.
	f.pid = 0
	root = newRootCommand()
	root.SetOut(&out)
	root.SetArgs([]string{"start"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if f.loads != 1 || f.kicks != 1 {
		t.Errorf("loads=%d kicks=%d", f.loads, f.kicks)
	}

	// Running: start says so and does nothing.
	f.pid = 4242
	out.Reset()
	root = newRootCommand()
	root.SetOut(&out)
	root.SetArgs([]string{"start"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if f.kicks != 1 || !strings.Contains(out.String(), "already running (pid 4242)") {
		t.Errorf("kicks=%d out=%q", f.kicks, out.String())
	}

	// Start options belong to the item's Caddyfile.
	root = newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"start", "--config", "/elsewhere/Caddyfile"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "autostart item runs the edge") {
		t.Errorf("start --config under an item: %v", err)
	}
}

func TestAutostartOffLeavesEdgeRunning(t *testing.T) {
	isolatedHome(t)
	f := &fakeItem{reg: true, isLoaded: true, pid: 4242}
	withFakeItem(t, f)
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"autostart", "off"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !f.removed || !strings.Contains(out.String(), "autostart off: removed /fake/item") {
		t.Errorf("removed=%v out=%q", f.removed, out.String())
	}

	// Off again is a no-op, not an error.
	out.Reset()
	root = newRootCommand()
	root.SetOut(&out)
	root.SetArgs([]string{"autostart", "off"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "was not on") {
		t.Errorf("out=%q", out.String())
	}

	for _, bad := range [][]string{{"autostart", "on"}, {"autostart", "off", "now"}} {
		root = newRootCommand()
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs(bad)
		if err := root.Execute(); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// autostart validates before installing: a broken Caddyfile installs
// nothing.
func TestAutostartRefusesBrokenConfig(t *testing.T) {
	p := isolatedHome(t)
	f := &fakeItem{}
	withFakeItem(t, f)
	if err := os.MkdirAll(filepath.Dir(p.config), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.config, []byte("{\n\tjanus {\n\t\tno-such-capability\n\t}\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"autostart"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "does not validate") {
		t.Fatalf("autostart with a broken config: %v", err)
	}
	if f.reg || f.loads != 0 {
		t.Errorf("installed anyway: %+v", f)
	}
}

// A state root deep enough to push the control socket past the unix
// path limit is refused up front, not discovered as a restart loop.
func TestAutostartRefusesLongSocketPath(t *testing.T) {
	isolatedHome(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), strings.Repeat("deep/", 30)))
	f := &fakeItem{}
	withFakeItem(t, f)
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"autostart"})
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "unix sockets allow") {
		t.Fatalf("long socket path: %v", err)
	}
	if f.reg {
		t.Error("installed anyway")
	}
}

// status reads the control plane for the app count.
func TestStatusCountsApps(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/1.0/apps" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`[{"id":"a"},{"id":"b"}]`))
	}))
	ln, err := listenUnix(p.sock)
	if err != nil {
		t.Skipf("unix listener: %v", err)
	}
	srv.Listener = ln
	srv.Start()
	defer srv.Close()

	n, at := probeControl(p)
	if n != 2 || !strings.HasPrefix(at, "unix ") {
		t.Fatalf("probe: n=%d at=%q", n, at)
	}
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"status"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 apps registered") || !strings.Contains(out.String(), "edge     running") {
		t.Errorf("status:\n%s", out.String())
	}
}

func TestStatusJSON(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()})
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"status", "--json"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var st edgeStatus
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if !st.Running || st.PID != os.Getpid() || !st.Autostart || !st.Loaded || st.Supervisor != "fake" || st.Config != p.config || st.Sites != p.sites {
		t.Errorf("status: %+v", st)
	}
}

func TestStatusStoppedExits3(t *testing.T) {
	isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"status"})
	err := root.Execute()
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 || exitCode(err) != 3 {
		t.Fatalf("stopped status: %v", err)
	}
	if !strings.Contains(out.String(), "edge     stopped") || strings.Contains(out.String(), "Error:") {
		t.Errorf("status:\n%s", out.String())
	}
}

func TestParseElapsed(t *testing.T) {
	cases := map[string]time.Duration{
		"":            0,
		"05:03":       5*time.Minute + 3*time.Second,
		"01:02:03":    time.Hour + 2*time.Minute + 3*time.Second,
		"2-01:02:03":  49*time.Hour + 2*time.Minute + 3*time.Second,
		"  00:09\n":   9 * time.Second,
		"not a value": 0,
	}
	for in, want := range cases {
		if got := parseElapsed(in); got != want {
			t.Errorf("parseElapsed(%q) = %v, want %v", in, got, want)
		}
	}
}

var _ = cobra.Command{}
