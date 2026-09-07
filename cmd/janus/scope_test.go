package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Absent means localhost; a stored state comes back as written; anything
// that does not parse or cannot be realized is refused, not repaired.
func TestScopeStateRoundTrip(t *testing.T) {
	p := isolatedHome(t)
	st, err := readScope(p)
	if err != nil || st.Scope != ScopeLocalhost {
		t.Fatalf("absent scope.json: %+v %v", st, err)
	}
	lan := scopeState{Scope: ScopeLAN, Interface: "en0", LANV4: "10.0.0.211", OnLinkV4: "10.0.0.0/24"}
	if err := writeScope(p, lan); err != nil {
		t.Fatal(err)
	}
	got, err := readScope(p)
	if err != nil || got != lan {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	if p.scope != filepath.Join(p.state, "scope.json") {
		t.Errorf("scope path %q", p.scope)
	}
	for name, body := range map[string]string{
		"not json":        "{",
		"unknown scope":   `{"scope":"public"}`,
		"unknown field":   `{"scope":"localhost","bind":"0.0.0.0"}`,
		"lan no iface":    `{"scope":"lan","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.0/24"}`,
		"lan no address":  `{"scope":"lan","interface":"en0"}`,
		"prefix mismatch": `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"192.168.0.0/24"}`,
		"lan loopback":    `{"scope":"lan","interface":"en0","lan_v4":"127.0.0.1","onlink_v4":"127.0.0.0/8"}`,
		"lan ipv6":        `{"scope":"lan","interface":"en0","lan_v4":"2601:680:8000:2330::906e","onlink_v4":"2601:680:8000:2330::/64"}`,
		"lan public":      `{"scope":"lan","interface":"en0","lan_v4":"34.22.36.78","onlink_v4":"34.22.36.0/24"}`,
		"lan host route":  `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.211/32"}`,
		"lan no prefix":   `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211"}`,
		"iface on wan":    `{"scope":"wan","interface":"en0"}`,
		"pinned on wan":   `{"scope":"wan","interface_pinned":true}`,
		"trailing value":  `{"scope":"localhost"} {"scope":"wan"}`,
		"trailing text":   `{"scope":"localhost"} garbage`,
		"repeated key":    `{"scope":"localhost","scope":"wan"}`,
		"padded scope":    `{"scope":" lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.0/24"}`,
		"upper scope":     `{"scope":"LAN","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.0/24"}`,
		"onlink everyone": `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"0.0.0.0/0"}`,
		"onlink too wide": `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.0/7"}`,
		"onlink unmasked": `{"scope":"lan","interface":"en0","lan_v4":"10.0.0.211","onlink_v4":"10.0.0.211/24"}`,
	} {
		if err := os.WriteFile(p.scope, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readScope(p); err == nil {
			t.Errorf("%s: accepted %s", name, body)
		}
	}
	if err := writeScope(p, scopeState{Scope: "LAN"}); err == nil {
		t.Error("writeScope accepted an unnormalized scope")
	}
	pinned := lanState()
	pinned.Pinned = true
	if err := writeScope(p, pinned); err != nil {
		t.Fatal(err)
	}
	if got, err := readScope(p); err != nil || got != pinned || !strings.Contains(readFile(t, p.scope), `"interface_pinned": true`) {
		t.Errorf("pinned round trip: %+v %v", got, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// setBindEnv puts the mode's bind for this OS in the environment; a
// broken scope.json stops it (and so every run/reload/validate).
func TestSetBindEnv(t *testing.T) {
	p := isolatedHome(t)
	t.Setenv(bindEnvKey, "stale")
	st, err := setBindEnv(p)
	if err != nil || st.Scope != ScopeLocalhost {
		t.Fatal(err)
	}
	want := "127.0.0.1 ::1"
	if runtime.GOOS == "darwin" {
		want = "0.0.0.0 ::"
	}
	if got := os.Getenv(bindEnvKey); got != want {
		t.Errorf("%s = %q, want %q", bindEnvKey, got, want)
	}
	if err := os.MkdirAll(p.state, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.scope, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := setBindEnv(p); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("broken scope.json: %v", err)
	}
	if err := validateInProcess(p.config); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("validate with a broken scope.json: %v", err)
	}
}

// The env file loads the way Caddy's --envfile does.
func TestLoadEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "env")
	if err := loadEnvFile(path); err != nil {
		t.Fatalf("absent env file: %v", err)
	}
	body := "# comment\nexport JT_A=one\nJT_B=\"two words\"\nJT_C=three # trailing\nJT_D=x=y\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"JT_A", "JT_B", "JT_C", "JT_D"} {
		t.Setenv(k, "")
	}
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"JT_A": "one", "JT_B": "two words", "JT_C": "three", "JT_D": "x=y"} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if err := os.WriteFile(path, []byte("no equals sign\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err == nil {
		t.Error("a malformed line loaded")
	}
	// The bind is the mode's: an env file cannot set it.
	if err := os.WriteFile(path, []byte("JANUS_BIND=0.0.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err == nil || !strings.Contains(err.Error(), "janus mode") {
		t.Errorf("JANUS_BIND in the env file: %v", err)
	}
}
