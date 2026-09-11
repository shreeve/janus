package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"time"
)

func TestServicePathsUser(t *testing.T) {
	p := servicePathsFor(false, "/home/ann", func(string) string { return "" })
	want := map[string]string{
		"config": "/home/ann/.config/janus/Caddyfile",
		"sites":  "/home/ann/.config/janus/sites",
		"env":    "/home/ann/.config/janus/env",
		"state":  "/home/ann/.local/state/janus",
		"log":    "/home/ann/.local/state/janus/log/janus.log",
		"sock":   "/home/ann/.local/state/janus/run/janus.sock",
		"admin":  "/home/ann/.local/state/janus/run/admin.sock",
		"pid":    "/home/ann/.local/state/janus/run/janus.pid",
		"home":   "/home/ann",
	}
	got := map[string]string{"config": p.config, "sites": p.sites, "env": p.env, "state": p.state, "log": p.log, "sock": p.sock, "admin": p.admin, "pid": p.pid, "home": p.home}
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

// The item's environment: HOME, an absolute-only PATH that leads with the
// binary's directory, and relocated XDG roots.
func TestServiceEnv(t *testing.T) {
	t.Setenv("PATH", ".:/Users/ann/bin:relative/bin:/usr/bin:/Users/ann/bin")
	t.Setenv("XDG_DATA_HOME", "/data")
	t.Setenv("XDG_CACHE_HOME", "")
	p := servicePathsFor(false, "/Users/ann", func(string) string { return "" })
	env := serviceEnv(p, "/Users/ann/.local/bin/janus")
	if env["PATH"] != "/Users/ann/.local/bin:/Users/ann/bin:/usr/bin" {
		t.Errorf("PATH = %q", env["PATH"])
	}
	if env["HOME"] != "/Users/ann" || env["XDG_DATA_HOME"] != "/data" {
		t.Errorf("env = %v", env)
	}
	if _, ok := env["XDG_CACHE_HOME"]; ok {
		t.Error("empty XDG_CACHE_HOME carried")
	}
	root := serviceEnv(servicePathsFor(true, "/var/root", func(string) string { return "" }), "/usr/local/bin/janus")
	if root["HOME"] != "/var/lib/janus" || !strings.HasPrefix(root["PATH"], "/usr/local/bin:") || strings.Contains(root["PATH"], "/Users/") {
		t.Errorf("root env = %v", root)
	}
}

// The seed is a runnable Caddyfile: it adapts and validates, control, admin,
// and log pointed at the service's paths, sites imported from beside it.
func TestSeedConfigValidates(t *testing.T) {
	p := isolatedHome(t)
	for _, dir := range []string{filepath.Dir(p.config), p.run, filepath.Dir(p.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	seed := seedConfig(p)
	for _, want := range []string{"control internal " + p.sock, "output file " + p.log, "import " + p.sites + "/*.caddy", "admin unix/" + p.admin,
		"permission janus", "mode bridge", "origin same", "precompressed", "output file " + accessLogPath(p), "/trust"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed lacks %q", want)
		}
	}
	for _, reject := range []string{"hub off", "control local", "ask http"} {
		if strings.Contains(seed, reject) {
			t.Errorf("seed carries %q", reject)
		}
	}
	if site := localhostSite(p); !strings.Contains(site, "output file "+accessLogPath(p)) || strings.Contains(site, "hub off") {
		t.Errorf("localhost drop-in:\n%s", site)
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
	start, _, _ := root.Find([]string{"start"})
	if !strings.Contains(start.Long, "autostart") {
		t.Errorf("start help does not mention autostart:\n%s", start.Long)
	}
	// reload/validate show --config as optional and say what the default is.
	for _, name := range []string{"reload", "validate"} {
		cmd, _, _ := root.Find([]string{name})
		if !strings.Contains(cmd.Use, "[--config <path>]") || !strings.Contains(cmd.Long, "service Caddyfile") {
			t.Errorf("%s: Use %q Long lacks the service default", name, cmd.Use)
		}
	}
	autostart, _, _ := root.Find([]string{"autostart"})
	if autostart.Flags().Lookup("config") != nil {
		t.Error("autostart still takes --config")
	}
}

// validate/stop/reload default --config to the service Caddyfile when it
// exists; a --config given wins.
func TestValidateDefaultsToServiceConfig(t *testing.T) {
	p := isolatedHome(t)
	seedAt(t, p)
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
	loads         int
	removed       bool
	body          string
}

func (f *fakeItem) name() string        { return "fake" }
func (f *fakeItem) file() string        { return "/fake/item" }
func (f *fakeItem) registered() bool    { return f.reg }
func (f *fakeItem) loaded() (bool, int) { return f.isLoaded, f.pid }
func (f *fakeItem) register(p servicePaths, exe string) (bool, error) {
	body := exe + " " + p.config
	changed := body != f.body
	f.body, f.reg = body, true
	return changed, nil
}
func (f *fakeItem) load() error { f.loads++; f.isLoaded = true; return nil }
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

// isolatedHome points the service at fresh directories. The state root is
// under /tmp rather than the test temp dir: unix socket paths are short
// by law, and the seed puts two under state/run.
func isolatedHome(t *testing.T) servicePaths {
	t.Helper()
	home := t.TempDir()
	state, err := os.MkdirTemp("/tmp", "janus-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(state) })
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", state)
	// Nothing answers here, so a foreign edge on the real port cannot
	// pass as this one during tests.
	prev := localControlURL
	localControlURL = "http://127.0.0.1:1"
	t.Cleanup(func() { localControlURL = prev })
	fakeHostPf(t)
	// Nothing holds a front-door port as far as a verb under test can
	// tell, whatever this machine's real edge is doing.
	prevDial := dialTCP
	dialTCP = func(string, time.Duration) (net.Conn, error) { return nil, errors.New("connection refused") }
	t.Cleanup(func() { dialTCP = prevDial })
	return currentPaths()
}

// fakeHostPf stands a fake host in for this machine's pf and default
// route, so no verb under test reads /etc or the routing table: the pf
// files for localhost in place (what a finished install looks like, so
// the edge may start), and en0 as the default route's interface.
func fakeHostPf(t *testing.T) *fakeHost {
	t.Helper()
	h, ops := newFakeHost(runtime.GOOS)
	h.installed(scopeState{Scope: ScopeLocalhost})
	prevOps, prevIface := hostFwOps, defaultInterface
	hostFwOps = func() fwOps { return ops }
	defaultInterface = func() (string, error) { return "en0", nil }
	t.Cleanup(func() { hostFwOps, defaultInterface = prevOps, prevIface })
	return h
}

func seedAt(t *testing.T, p servicePaths) {
	t.Helper()
	for _, dir := range []string{filepath.Dir(p.config), p.sites, p.run, filepath.Dir(p.log)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(p.config, []byte(seedConfig(p)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	root := newRootCommand()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestStartUnderItem(t *testing.T) {
	isolatedHome(t)
	f := &fakeItem{reg: true}
	withFakeItem(t, f)

	// Not loaded: start loads it.
	out, err := run(t, "start")
	if err != nil {
		t.Fatal(err)
	}
	if f.loads != 1 || !strings.Contains(out, "started under fake") {
		t.Errorf("loads=%d out=%q", f.loads, out)
	}

	// Loaded but exited (after a stop): start loads again, which the
	// item does by replacing the job it still holds.
	f.pid = 0
	if _, err := run(t, "start"); err != nil {
		t.Fatal(err)
	}
	if f.loads != 2 {
		t.Errorf("loads=%d", f.loads)
	}

	// Running: start says so and does nothing.
	f.pid = os.Getpid()
	out, err = run(t, "start")
	if err != nil {
		t.Fatal(err)
	}
	if f.loads != 2 || !strings.Contains(out, "already running (pid "+itoa(os.Getpid())+")") {
		t.Errorf("loads=%d out=%q", f.loads, out)
	}

	// Start options belong to the item's Caddyfile.
	for _, flag := range []string{"--config=/elsewhere/Caddyfile", "--watch", "--pidfile=/x"} {
		_, err = run(t, "start", flag)
		if err == nil || !strings.Contains(err.Error(), "belong to the autostart item") {
			t.Errorf("start %s under an item: %v", flag, err)
		}
	}
}

func itoa(n int) string { return fmtInt(n) }

func fmtInt(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A bare start with no Caddyfile anywhere is refused rather than starting
// an empty Caddy.
func TestBareStartNeedsConfig(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	cwd, _ := os.Getwd()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	_, err := run(t, "start")
	if err == nil || !strings.Contains(err.Error(), "no Caddyfile at "+p.config) {
		t.Fatalf("bare start without a Caddyfile: %v", err)
	}
}

func TestAutostartInstalls(t *testing.T) {
	p := isolatedHome(t)
	f := &fakeItem{}
	withFakeItem(t, f)
	out, err := run(t, "autostart")
	if err != nil {
		t.Fatalf("autostart: %v\n%s", err, out)
	}
	if !fileExists(p.config) || !strings.Contains(out, "seeded "+p.config) {
		t.Errorf("seed missing: %q", out)
	}
	if st, err := os.Stat(p.sites); err != nil || !st.IsDir() {
		t.Error("sites dir missing")
	}
	if !f.reg || f.loads != 1 || !strings.Contains(out, "autostart on: /fake/item") || !strings.Contains(out, "started under fake") {
		t.Errorf("item: %+v out=%q", f, out)
	}

	// Again while running and unchanged: says so, loads nothing.
	f.pid = os.Getpid()
	out, err = run(t, "autostart")
	if err != nil {
		t.Fatal(err)
	}
	if f.loads != 1 || !strings.Contains(out, "already running") {
		t.Errorf("idempotent autostart: loads=%d out=%q", f.loads, out)
	}

	// The item file changed (a new binary path) while running: the
	// operator is told a restart applies it.
	f.body = "old"
	out, _ = run(t, "autostart")
	if !strings.Contains(out, "under the previous item; 'janus restart' applies") {
		t.Errorf("changed item while running: %q", out)
	}
}

func TestAutostartOffLeavesEdgeRunning(t *testing.T) {
	isolatedHome(t)
	f := &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()}
	withFakeItem(t, f)
	out, err := run(t, "autostart", "off")
	if err != nil {
		t.Fatal(err)
	}
	if !f.removed || !strings.Contains(out, "autostart off: removed /fake/item; a running edge is left alone") {
		t.Errorf("removed=%v out=%q", f.removed, out)
	}

	// Off again is a no-op, not an error.
	out, err = run(t, "autostart", "off")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "was not on") {
		t.Errorf("out=%q", out)
	}

	for _, bad := range [][]string{{"autostart", "on"}, {"autostart", "off", "now"}, {"autostart", "stop"}} {
		if _, err := run(t, bad...); err == nil {
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
	_, err := run(t, "autostart")
	if err == nil || !strings.Contains(err.Error(), "does not validate") {
		t.Fatalf("autostart with a broken config: %v", err)
	}
	if f.reg || f.loads != 0 {
		t.Errorf("installed anyway: %+v", f)
	}
}

// A state root deep enough to push the sockets past the unix path limit
// is refused up front, not discovered as a restart loop.
func TestAutostartRefusesLongSocketPath(t *testing.T) {
	isolatedHome(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), strings.Repeat("deep/", 30)))
	f := &fakeItem{}
	withFakeItem(t, f)
	_, err := run(t, "autostart")
	if err == nil || !strings.Contains(err.Error(), "unix sockets allow") {
		t.Fatalf("long socket path: %v", err)
	}
	if f.reg {
		t.Error("installed anyway")
	}
}

// stop and reload with nothing running: stop is a quiet no-op, reload
// says what to do instead.
func TestStopAndReloadWhenStopped(t *testing.T) {
	p := isolatedHome(t)
	seedAt(t, p)
	withFakeItem(t, &fakeItem{})
	out, err := run(t, "stop")
	if err != nil || !strings.Contains(out, "janus was not running") {
		t.Errorf("stop when stopped: err=%v out=%q", err, out)
	}
	_, err = run(t, "reload")
	if err == nil || !strings.Contains(err.Error(), "janus is not running") {
		t.Errorf("reload when stopped: %v", err)
	}
	// Naming another edge's config or admin address bypasses the check:
	// the verb goes to Caddy, which reports what it finds there.
	other := filepath.Join(t.TempDir(), "Caddyfile")
	if err := os.WriteFile(other, []byte("{\n\tadmin unix//tmp/janus-test-nothing.sock\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"reload", "--config", other}, {"stop", "--config", other}, {"stop", "--address", "unix//tmp/janus-test-nothing.sock"}} {
		out, err := run(t, args...)
		if err == nil || strings.Contains(out, "janus was not running") || strings.Contains(err.Error(), "janus is not running") {
			t.Errorf("%v treated as the service edge: err=%v out=%q", args, err, out)
		}
	}
}

// A control server on the service's socket, answering /1.0 and /1.0/apps.
func controlServer(t *testing.T, p servicePaths, apps string) {
	t.Helper()
	var records []json.RawMessage
	if err := json.Unmarshal([]byte(apps), &records); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0":
			fmt.Fprintf(w, `{"type":"janus","app_count":%d,"control":[{"mode":"internal","listen":%q}]}`, len(records), p.sock)
		case "/1.0/apps":
			_, _ = w.Write([]byte(apps))
		case "/1.0/mdns":
			_, _ = w.Write([]byte(`{"dashboard_url":"http://janus-2.local/","trust_url":"http://janus-2.local/trust"}`))
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

// trust and untrust default --address to the service edge's admin socket
// when it exists, and leave Caddy's default alone otherwise.
func TestTrustDefaultsToAdminSocket(t *testing.T) {
	p := isolatedHome(t)
	address := func(verb string) string {
		t.Helper()
		root := newRootCommand()
		cmd, _, _ := root.Find([]string{verb})
		got := "unset"
		cmd.RunE = func(cmd *cobra.Command, _ []string) error {
			got, _ = cmd.Flags().GetString("address")
			return nil
		}
		root.SetArgs([]string{verb})
		if err := root.Execute(); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := address("trust"); got != "" {
		t.Errorf("no socket: address %q", got)
	}
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	for _, verb := range []string{"trust", "untrust"} {
		if got := address(verb); got != "unix/"+p.admin {
			t.Errorf("%s with socket: address %q", verb, got)
		}
	}
}

// status reports whether this machine trusts the local CA, and where a
// phone trusts it: the mdns front door by its effective name.
func TestStatusShowsCA(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	controlServer(t, p, `[]`)
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	root := localCARoot()
	if !strings.HasPrefix(root, data) {
		t.Fatalf("root %s outside the isolated data dir", root)
	}
	out, err := run(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "\nca ") {
		t.Errorf("no root on disk, yet a ca line:\n%s", out)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, testRootPEM(t), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := verifyCA
	t.Cleanup(func() { verifyCA = prev })
	verifyCA = func(*x509.Certificate) error { return nil }
	out, err = run(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "ca       trusted on this machine") || !strings.Contains(out, "http://janus-2.local/trust") {
		t.Errorf("trusted:\n%s", out)
	}
	verifyCA = func(*x509.Certificate) error { return errors.New("unknown authority") }
	out, _ = run(t, "status")
	if !strings.Contains(out, "ca       not trusted on this machine") || !strings.Contains(out, "'janus trust'") {
		t.Errorf("untrusted:\n%s", out)
	}
	out, _ = run(t, "status", "--json")
	if !strings.Contains(out, `"ca_trusted": false`) || !strings.Contains(out, `"trust_url": "http://janus-2.local/trust"`) {
		t.Errorf("json:\n%s", out)
	}
}

func testRootPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Caddy Local Authority"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// trust --export writes the root certificate the admin API hands out, over
// the service edge's admin socket, instead of trusting it here.
func TestTrustExport(t *testing.T) {
	p := isolatedHome(t)
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	rootPEM := string(testRootPEM(t))
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pki/ca/local" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "local", "root_certificate": rootPEM})
	}))
	ln, err := net.Listen("unix", p.admin)
	if err != nil {
		t.Fatal(err)
	}
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
	dest := filepath.Join(t.TempDir(), "ca.crt")
	out, err := run(t, "trust", "--export", dest)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != rootPEM {
		t.Errorf("exported %q (%v), want the root", got, err)
	}
	if !strings.Contains(out, "wrote "+dest) {
		t.Errorf("out: %s", out)
	}
}

// status reads the control plane for the app count, over the socket.
func TestStatusCountsApps(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	controlServer(t, p, `[{"id":"a"},{"id":"b"}]`)

	n, at := probeControl(p)
	if n != 2 || at != "unix "+p.sock {
		t.Fatalf("probe: n=%d at=%q", n, at)
	}
	out, err := run(t, "status")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 apps registered") || !strings.Contains(out, "edge     running") {
		t.Errorf("status:\n%s", out)
	}
}

func TestStatusRejectsInvalidControlMetadata(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.sock)
	if err != nil {
		t.Fatal(err)
	}
	body := "not JSON"
	requests := []string{}
	var mu sync.Mutex
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests = append(requests, r.URL.Path)
		fmt.Fprint(w, body)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	if st := gatherStatus(p); st.Running || st.Control != "" {
		t.Fatalf("malformed metadata reported a running edge: %+v", st)
	}
	mu.Lock()
	body = fmt.Sprintf(`{"type":"janus","control":[{"mode":"internal","listen":%q}]}`, p.sock)
	requests = nil
	mu.Unlock()
	st := gatherStatus(p)
	if !st.Running || st.Apps != nil {
		t.Fatalf("root without count: %+v", st)
	}
	var out bytes.Buffer
	printStatus(&out, p, st)
	if strings.Contains(out.String(), "control  unreachable") {
		t.Fatal(out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(requests, ",") != "/1.0,/1.0/mdns" {
		t.Fatalf("status fetched more than compact metadata: %v", requests)
	}
}

// Over loopback, only an edge that also listens on this service's socket
// is this edge; another Janus with 'control local' is not.
func TestProbeControlLoopbackIdentity(t *testing.T) {
	p := isolatedHome(t)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0":
			_, _ = w.Write([]byte(`{"type":"janus","control":[{"mode":"internal","listen":"/elsewhere/janus.sock"},{"mode":"local","listen":"http://127.0.0.1:7600/"}]}`))
		case "/1.0/apps":
			_, _ = w.Write([]byte(`[{"id":"x"}]`))
		}
	}))
	defer other.Close()
	prev := localControlURL
	localControlURL = other.URL
	defer func() { localControlURL = prev }()
	if _, at := probeControl(p); at != "" {
		t.Fatalf("a foreign edge passed as this one: %q", at)
	}
	// The same server claiming this socket is accepted.
	mine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/1.0":
			_, _ = w.Write([]byte(`{"type":"janus","control":[{"mode":"internal","listen":"` + p.sock + `"}]}`))
		case "/1.0/apps":
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer mine.Close()
	localControlURL = mine.URL
	if n, at := probeControl(p); at != mine.URL || n != 0 {
		t.Fatalf("own edge over loopback: n=%d at=%q", n, at)
	}
}

func TestStatusJSON(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()})
	out, err := run(t, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st edgeStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if !st.Running || st.PID != os.Getpid() || !st.Autostart || !st.Loaded || st.Supervisor != "fake" || st.Config != p.config || st.Sites != p.sites {
		t.Errorf("status: %+v", st)
	}
	// Control did not answer: no apps count, rather than a misleading 0.
	if st.Apps != nil || st.Control != "" || strings.Contains(out, `"apps"`) {
		t.Errorf("apps reported without control: %s", out)
	}
	if st.Janus == "" || st.Caddy == "" {
		t.Errorf("version fields: %+v", st)
	}
	if st.Socket != p.sock || st.Admin != p.admin || st.Env != p.env || st.State != p.state {
		t.Errorf("path fields: %+v", st)
	}
}

// A pidfile edge while the item is registered but not running: status
// says which is which. A pidfile naming a process that is not a janus is
// stale and goes away.
func TestStatusPidfileUnderRegisteredItem(t *testing.T) {
	p := isolatedHome(t)
	withFakeItem(t, &fakeItem{reg: true, isLoaded: true})
	if err := os.MkdirAll(p.run, 0o755); err != nil {
		t.Fatal(err)
	}
	// The test binary is janus.test, which is janus enough for ps.
	if err := os.WriteFile(p.pid, []byte(fmtInt(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "status")
	if err != nil || !strings.Contains(out, "under    pidfile "+p.pid+" (autostart on, but the item is not running it") {
		t.Errorf("pidfile edge under a registered item: err=%v\n%s", err, out)
	}
	// pid 1 is alive (init) and is not a janus: stale.
	if err := os.WriteFile(p.pid, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "status")
	if !strings.Contains(out, "edge     stopped") || fileExists(p.pid) {
		t.Errorf("stale pidfile kept: exists=%v\n%s", fileExists(p.pid), out)
	}
}

func TestStatusStoppedExits3(t *testing.T) {
	isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	out, err := run(t, "status")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 3 || exitCode(err) != 3 {
		t.Fatalf("stopped status: %v", err)
	}
	if !strings.Contains(out, "edge     stopped") || strings.Contains(out, "Error:") {
		t.Errorf("status:\n%s", out)
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
