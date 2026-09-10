package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
)

// The load-bearing fact behind default_bind {$JANUS_BIND}: Caddy replaces
// {$VAR} in the text before lexing, so a value with spaces becomes several
// arguments — one listener per address, never one dual-stack socket.
func TestBindPlaceholderExpandsToTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "Caddyfile")
	body := "{\n\tadmin off\n\tauto_https off\n\tdefault_bind {$JANUS_BIND}\n}\nhttp://example.test:8099 {\n\trespond ok\n}\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bindEnvKey, "127.0.0.1 ::1")
	listen := adaptedListen(t, path)
	if len(listen) != 2 || !strings.Contains(listen[0], "127.0.0.1:8099") || !strings.Contains(listen[1], "[::1]:8099") {
		t.Fatalf("listen = %v, want one listener per address", listen)
	}
	// Caddy's own answer to an empty placeholder is its default, the
	// wildcard — which is why every janus verb that adapts the service
	// Caddyfile sets JANUS_BIND from scope.json first, and why the edge
	// checks its own sockets against the mode (verifyExposure).
	os.Unsetenv(bindEnvKey)
	listen = adaptedListen(t, path)
	if len(listen) != 1 || listen[0] != ":8099" {
		t.Fatalf("unset placeholder: listen = %v", listen)
	}
}

func adaptedListen(t *testing.T, path string) []string {
	t.Helper()
	cfgJSON, _, _, err := caddycmd.LoadConfig(path, "caddyfile")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Apps struct {
			HTTP struct {
				Servers map[string]struct {
					Listen []string `json:"listen"`
				} `json:"servers"`
			} `json:"http"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(cfgJSON, &cfg); err != nil {
		t.Fatal(err)
	}
	var listen []string
	for _, s := range cfg.Apps.HTTP.Servers {
		listen = append(listen, s.Listen...)
	}
	return listen
}

// validate and adapt of the service Caddyfile get the mode's bind; validate
// also loads the env file beside it.
func TestValidateLoadsScopeAndEnv(t *testing.T) {
	p := isolatedHome(t)
	seedAt(t, p)
	t.Setenv("JT_SITE_PORT", "")
	os.Unsetenv("JT_SITE_PORT")
	if err := os.WriteFile(p.env, []byte("JT_SITE_PORT=8098\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	site := filepath.Join(p.sites, "t.caddy")
	if err := os.WriteFile(site, []byte("http://example.test:{$JT_SITE_PORT} {\n\trespond ok\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(bindEnvKey, "")
	if out, err := run(t, "validate"); err != nil {
		t.Fatalf("validate: %v\n%s", err, out)
	}
	if os.Getenv("JT_SITE_PORT") != "8098" {
		t.Error("env file not loaded for validate")
	}
	want := "127.0.0.1 ::1"
	if runtime.GOOS == "darwin" {
		want = "0.0.0.0 ::"
	}
	if got := os.Getenv(bindEnvKey); got != want {
		t.Errorf("%s = %q after validate, want %q", bindEnvKey, got, want)
	}
	t.Setenv(bindEnvKey, "")
	if out, err := run(t, "adapt", "--config", p.config); err != nil {
		t.Fatalf("adapt: %v\n%s", err, out)
	}
	if got := os.Getenv(bindEnvKey); got != want {
		t.Errorf("%s = %q after adapt", bindEnvKey, got)
	}
}

func TestStatusReportsExposure(t *testing.T) {
	p := isolatedHome(t)
	fakeHostPf(t).bare()
	withFakeItem(t, &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()})
	out, err := run(t, "status", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var st edgeStatus
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatal(err)
	}
	if st.Scope != "localhost" || st.Interface != "" || len(st.Listeners) != 4 || st.ScopeError != "" {
		t.Errorf("localhost status: %+v", st)
	}
	wantBind := []string{"127.0.0.1", "::1"}
	wantFW := "none"
	if runtime.GOOS == "darwin" {
		wantBind = []string{"0.0.0.0", "::"}
		wantFW = "missing: " + pfAnchorPath + " is absent"
	}
	if strings.Join(st.Bind, " ") != strings.Join(wantBind, " ") || !strings.HasPrefix(st.Firewall, wantFW) {
		t.Errorf("bind %v firewall %q", st.Bind, st.Firewall)
	}
	for _, l := range st.Listeners {
		if l.Reach != ReachLoopback {
			t.Errorf("listener %+v", l)
		}
	}
	// lan: the private address is host-enforced by addressing alone; the
	// firewall line says whether pf was verified, and nothing overclaims.
	if err := writeScope(p, lanState()); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "status")
	wantFW = "firewall none"
	if runtime.GOOS == "darwin" {
		wantFW = "firewall missing: " + pfAnchorPath + " is absent"
	}
	for _, want := range []string{"scope    lan (en0)", wantFW, "10.0.0.211:443", string(ReachRFC1918), "http     the same addresses on port 80"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "2601") || strings.Contains(out, "*:443") {
		t.Errorf("status overclaims:\n%s", out)
	}
	// A broken scope.json is reported, not repaired.
	if err := os.WriteFile(p.scope, []byte(`{"scope":"public"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ = run(t, "status")
	if !strings.Contains(out, "scope    UNUSABLE") {
		t.Errorf("broken scope.json:\n%s", out)
	}
	out, _ = run(t, "status", "--json")
	if !strings.Contains(out, `"bind": []`) || !strings.Contains(out, `"scope_error"`) {
		t.Errorf("broken scope.json as JSON must carry an empty bind and the error:\n%s", out)
	}
}

func TestModeRefusals(t *testing.T) {
	isolatedHome(t)
	withFakeItem(t, &fakeItem{})
	t.Setenv("JANUS_NO_SUDO", "1")
	if _, err := run(t, "mode", "public"); err == nil || !strings.Contains(err.Error(), "unknown exposure mode") {
		t.Errorf("mode public: %v", err)
	}
	// No argument shows the mode; options without a mode are refused.
	if out, err := run(t, "mode"); err != nil || !strings.Contains(out, "scope    localhost\n") || !strings.Contains(out, "bind     ") || !strings.Contains(out, "firewall ") {
		t.Errorf("bare mode: %v\n%s", err, out)
	}
	if _, err := run(t, "mode", "--interface", "en0"); err == nil || !strings.Contains(err.Error(), "mode to set") {
		t.Errorf("bare mode with options: %v", err)
	}
	if _, err := run(t, "mode", "localhost", "--interface", "en0"); err == nil || !strings.Contains(err.Error(), "belong to lan") {
		t.Errorf("interface on localhost: %v", err)
	}
	for _, args := range [][]string{{"start", "--scope", "lan"}, {"reload", "--interface", "en0"}} {
		if _, err := run(t, args...); err == nil || !strings.Contains(err.Error(), "belongs to the edge's exposure mode") {
			t.Errorf("%v: %v", args, err)
		}
	}
	// A host with no default route and no --interface has nothing to guess.
	prev := defaultInterface
	defaultInterface = func() (string, error) {
		return "", errors.New("no IPv4 default route; name the interface with --interface")
	}
	t.Cleanup(func() { defaultInterface = prev })
	if _, err := run(t, "mode", "lan"); err == nil || !strings.Contains(err.Error(), "--interface") {
		t.Errorf("lan without a default route: %v", err)
	}
}

// wan from a fresh install needs no firewall step anywhere, so it goes
// through unprivileged: the scope is stored and an installed edge starts.
// Narrowing on macOS is a pf step: without a terminal to ask on it refuses
// with the command, exit 4, before changing anything.
func TestModeWANStoresAndStarts(t *testing.T) {
	p := isolatedHome(t)
	fakeHostPf(t).bare()
	seedAt(t, p)
	f := &fakeItem{reg: true}
	withFakeItem(t, f)
	t.Setenv("JANUS_NO_SUDO", "1")
	out, err := run(t, "mode", "wan")
	if err != nil {
		t.Fatalf("mode wan: %v\n%s", err, out)
	}
	st, err := readScope(p)
	if err != nil || st.Scope != ScopeWAN {
		t.Errorf("stored %+v %v", st, err)
	}
	if !strings.Contains(out, "wildcard exposure active") || !strings.Contains(out, "bind     0.0.0.0 ::") || f.loads != 1 {
		t.Errorf("out=%q loads=%d", out, f.loads)
	}
	out, err = run(t, "mode", "localhost")
	switch runtime.GOOS {
	case "darwin":
		var ee *exitError
		if !errors.As(err, &ee) || ee.code != exitNeedsRoot {
			t.Errorf("darwin localhost without root: %v", err)
		}
		if !strings.Contains(out, "no terminal to ask on") || !strings.Contains(out, "sudo ") || !strings.Contains(out, " firewall") {
			t.Errorf("exit 4 without the command to run:\n%s", out)
		}
		if st, _ := readScope(p); st.Scope != ScopeWAN {
			t.Error("scope changed before the firewall step")
		}
	default:
		if err != nil {
			t.Fatalf("linux localhost: %v\n%s", err, out)
		}
		if st, _ := readScope(p); st.Scope != ScopeLocalhost {
			t.Error("scope not stored")
		}
	}
}

// lan takes the default route's interface; --interface pins another, and
// setting lan again keeps a pinned interface while re-resolving an
// automatic one.
func TestModeLANDefaultInterfaceAndAgain(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("lan on macOS is a pf step; the resolution is covered through autostart")
	}
	p := isolatedHome(t)
	seedAt(t, p)
	withFakeItem(t, &fakeItem{reg: true})
	fakeInterfaces(t)
	t.Setenv("JANUS_NO_SUDO", "1")
	out, err := run(t, "mode", "lan")
	if err != nil {
		t.Fatalf("mode lan: %v\n%s", err, out)
	}
	if st, _ := readScope(p); st != lanState() || !strings.Contains(out, "scope    lan (en0)\n") {
		t.Errorf("auto: %+v\n%s", st, out)
	}
	// The default route moved to an interface that is not up: lan again
	// says so, and the stored scope stays.
	prevDefault := defaultInterface
	defaultInterface = func() (string, error) { return "en1", nil }
	t.Cleanup(func() { defaultInterface = prevDefault })
	if out, err := run(t, "mode", "lan"); err == nil || !strings.Contains(err.Error(), "en1") {
		t.Errorf("lan again onto a missing interface: %v\n%s", err, out)
	}
	if st, _ := readScope(p); st != lanState() {
		t.Errorf("a failed re-resolution changed the scope: %+v", st)
	}
	// Pinned: named once, kept by a bare 'mode lan' even though the
	// default route is elsewhere.
	out, err = run(t, "mode", "lan", "--interface", "en0")
	if err != nil {
		t.Fatalf("pin: %v\n%s", err, out)
	}
	pinned := lanState()
	pinned.Pinned = true
	if st, _ := readScope(p); st != pinned || !strings.Contains(out, "(en0, pinned)") {
		t.Errorf("pinned: %+v\n%s", st, out)
	}
	if out, err := run(t, "mode", "lan"); err != nil || !strings.Contains(out, "(en0, pinned)") {
		t.Errorf("lan again on a pinned interface: %v\n%s", err, out)
	}
	// auto unpins: back to the default route, which is en1 now, and down.
	if _, err := run(t, "mode", "lan", "--interface", "auto"); err == nil || !strings.Contains(err.Error(), "en1") {
		t.Errorf("unpin: %v", err)
	}
}

func fakeInterfaces(t *testing.T) {
	t.Helper()
	prev := interfaceFacts
	interfaceFacts = func(name string) (ifaceFacts, error) {
		if name != "en0" {
			return ifaceFacts{}, errors.New("interface " + name + ": no such interface")
		}
		return popFacts(), nil
	}
	t.Cleanup(func() { interfaceFacts = prev })
}

// autostart --scope lan resolves the default route's interface and stores
// it. On macOS a user's install then says exactly what applies pf; on
// Linux lan needs no firewall at all, so a user's edge is simply installed
// on it.
func TestAutostartScopeLAN(t *testing.T) {
	p := isolatedHome(t)
	fakeHostPf(t).bare()
	f := &fakeItem{}
	withFakeItem(t, f)
	fakeInterfaces(t)
	if _, err := run(t, "autostart", "--scope", "lan", "--interface", "en9"); err == nil || !strings.Contains(err.Error(), "no such interface") {
		t.Fatalf("unknown interface: %v", err)
	}
	if fileExists(p.scope) {
		t.Error("a failed autostart wrote scope.json")
	}
	out, err := run(t, "autostart", "--scope", "lan")
	if err != nil {
		t.Fatalf("autostart lan: %v\n%s", err, out)
	}
	st, err := readScope(p)
	if err != nil || st != lanState() {
		t.Errorf("stored %+v %v", st, err)
	}
	if !strings.Contains(out, "scope  lan (en0)") {
		t.Errorf("out=%q", out)
	}
	switch runtime.GOOS {
	case "darwin":
		// No terminal, no root: the rule is not in place, so the edge is
		// registered but not started, and the output says what to run.
		if !strings.Contains(out, "sudo janus firewall") || !strings.Contains(out, "refuses to serve") {
			t.Errorf("darwin did not say what applies pf:\n%s", out)
		}
		if f.loads != 0 {
			t.Errorf("darwin started a wildcard edge without pf: loads=%d", f.loads)
		}
	default:
		if strings.Contains(out, "firewall") || !strings.Contains(out, "bind   127.0.0.1 ::1 10.0.0.211") || f.loads != 1 {
			t.Errorf("linux lan (loads=%d):\n%s", f.loads, out)
		}
	}
	// autostart again without --scope keeps lan.
	if _, err := run(t, "autostart"); err != nil {
		t.Fatal(err)
	}
	if st, _ := readScope(p); st.Scope != ScopeLAN {
		t.Error("autostart without --scope changed the scope")
	}
	if _, err := run(t, "autostart", "--interface", "en1"); err == nil || !strings.Contains(err.Error(), "--scope lan") {
		t.Errorf("interface without scope: %v", err)
	}
}

func TestAutostartDefaultsToLocalhost(t *testing.T) {
	p := isolatedHome(t)
	fakeHostPf(t).bare()
	withFakeItem(t, &fakeItem{})
	out, err := run(t, "autostart")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !fileExists(p.scope) {
		t.Error("scope.json not written")
	}
	if !fileExists(localhostSitePath(p)) {
		t.Error("autostart did not seed the *.localhost drop-in")
	}
	if st, _ := readScope(p); st.Scope != ScopeLocalhost {
		t.Errorf("scope %+v", st)
	}
	if !strings.Contains(out, "scope  localhost") {
		t.Errorf("out=%q", out)
	}
	if runtime.GOOS == "darwin" && !strings.Contains(out, "sudo janus firewall") {
		t.Errorf("darwin user install did not say what applies pf:\n%s", out)
	}
	if runtime.GOOS == "linux" && strings.Contains(out, "firewall") {
		t.Errorf("linux localhost mentions a firewall:\n%s", out)
	}
}

// On macOS the service edge refuses to run a scoped mode until the pf
// files are in place: an unprivileged autostart is never a wildcard edge
// with nothing scoping it.
func TestServiceEdgeReadyNeedsPfFiles(t *testing.T) {
	isolatedHome(t)
	h := fakeHostPf(t)
	h.bare()
	st := scopeState{Scope: ScopeLocalhost}
	err := serviceEdgeReady(st)
	switch runtime.GOOS {
	case "darwin":
		if err == nil || !strings.Contains(err.Error(), "sudo janus firewall") {
			t.Fatalf("no pf files: %v", err)
		}
		h.installed(st)
		if err := serviceEdgeReady(st); err != nil {
			t.Errorf("files in place: %v", err)
		}
	default:
		if err != nil {
			t.Errorf("linux localhost: %v", err)
		}
	}
	if err := serviceEdgeReady(scopeState{Scope: ScopeWAN}); err != nil {
		t.Errorf("wan needs no rule: %v", err)
	}
}

// Without root, status reports the firewall from the files: missing when
// they are absent, unverified once they are in place (pf itself is root's
// to see). A bogus firewall argument is refused.
func TestFirewallVerdictUnprivileged(t *testing.T) {
	isolatedHome(t)
	h := fakeHostPf(t)
	h.bare()
	withFakeItem(t, &fakeItem{reg: true, isLoaded: true, pid: os.Getpid()})
	out, _ := run(t, "status")
	if runtime.GOOS == "darwin" {
		if !strings.Contains(out, "firewall missing: "+pfAnchorPath+" is absent") {
			t.Errorf("darwin, nothing installed:\n%s", out)
		}
		h.installed(scopeState{Scope: ScopeLocalhost})
		if out, _ := run(t, "status"); !strings.Contains(out, "firewall unverified (run: sudo janus status)") {
			t.Errorf("darwin, files in place:\n%s", out)
		}
	} else if !strings.Contains(out, "firewall none") {
		t.Errorf("linux localhost:\n%s", out)
	}
	if _, err := run(t, "firewall", "check"); err == nil {
		t.Error("firewall took an argument")
	}
}

func TestRootHasExposureVerbs(t *testing.T) {
	root := newRootCommand()
	for _, name := range []string{"mode", "firewall"} {
		if cmd, _, err := root.Find([]string{name}); err != nil || cmd == nil || cmd.Name() != name {
			t.Errorf("no %s command", name)
		}
	}
	autostart, _, _ := root.Find([]string{"autostart"})
	for _, flag := range []string{"scope", "interface", "v4"} {
		if autostart.Flags().Lookup(flag) == nil {
			t.Errorf("autostart lacks --%s", flag)
		}
	}
	for _, gone := range []string{"v6", "confirm-wan"} {
		if autostart.Flags().Lookup(gone) != nil {
			t.Errorf("autostart takes --%s", gone)
		}
	}
	if cmd, _, _ := root.Find([]string{"repair"}); cmd != nil && cmd.Name() == "repair" {
		t.Error("repair exists; setting the mode again is the repair")
	}
}
