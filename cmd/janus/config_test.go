package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEquivalentServiceConfigLoadsEnvironment(t *testing.T) {
	p := isolatedHome(t)
	if err := os.MkdirAll(filepath.Dir(p.config), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.config, []byte("http://example.test:{$JT_PORT} {\n respond ok\n}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.env, []byte("JT_PORT=8198\n"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(p.config), "alias.caddy")
	if err := os.Symlink(p.config, alias); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(mustWorkingDir(t), p.config)
	if err != nil {
		t.Fatal(err)
	}
	for _, cfg := range []string{alias, relative} {
		t.Setenv("JT_PORT", "")
		os.Unsetenv("JT_PORT")
		if !sameConfigPath(cfg, p.config) {
			t.Fatalf("equivalent path not recognized: %s", cfg)
		}
		if out, err := run(t, "validate", "--config", cfg); err != nil {
			t.Fatalf("validate alias: %v %s", err, out)
		}
		if os.Getenv("JT_PORT") != "8198" {
			t.Fatal("equivalent path skipped envfile")
		}
	}
	other := filepath.Join(filepath.Dir(p.config), "other")
	if err := os.WriteFile(other, []byte("different"), 0600); err != nil {
		t.Fatal(err)
	}
	if sameConfigPath(other, p.config) {
		t.Fatal("different file treated as service config")
	}
}

func mustWorkingDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestInternalReloadDoesNotMutateSiblingCommand(t *testing.T) {
	p := isolatedHome(t)
	var source, adapter, body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/load" {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		source = r.Header.Get("Caddy-Config-Source-File")
		adapter = r.Header.Get("Caddy-Config-Source-Adapter")
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		body = string(b)
	}))
	defer server.Close()
	if err := os.MkdirAll(filepath.Dir(p.config), 0755); err != nil {
		t.Fatal(err)
	}
	config := "{\n admin " + strings.TrimPrefix(server.URL, "http://") + "\n}\nhttp://example.test:8098 {\n respond ok\n}\n"
	if err := os.WriteFile(p.config, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().String("config", "unrelated", "")
	cmd.RunE = func(*cobra.Command, []string) error { t.Fatal("invoked sibling command"); return nil }
	if err := reloadEdge(cmd, p); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := cmd.Flags().GetString("config"); cfg != "unrelated" {
		t.Fatal("mutated sibling command flags")
	}
	if source != p.config || adapter != "caddyfile" || !strings.Contains(body, `"body":"ok"`) {
		t.Fatalf("reload request: %s %s %s", source, adapter, body)
	}
}
