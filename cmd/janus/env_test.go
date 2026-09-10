package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigCommandsLoadEnvironmentConsistently(t *testing.T) {
	for _, verb := range []string{"run", "adapt", "validate", "reload"} {
		for _, reserved := range []bool{false, true} {
			t.Run(verb+"/reserved="+map[bool]string{true: "yes", false: "no"}[reserved], func(t *testing.T) {
				p := isolatedHome(t)
				if err := os.MkdirAll(filepath.Dir(p.env), 0755); err != nil {
					t.Fatal(err)
				}
				body := "JT_MULTILINE=\"first\nsecond\"\nJT_OVERRIDE=file\nJT_EMPTY=file\n"
				if reserved {
					body += "JANUS_BIND=0.0.0.0\n"
				}
				if err := os.WriteFile(p.env, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("JT_MULTILINE", "")
				os.Unsetenv("JT_MULTILINE")
				t.Setenv("JT_OVERRIDE", "shell")
				t.Setenv("JT_EMPTY", "")
				// An invalid config stops run/reload before they can open listeners or
				// contact an admin endpoint, after the actual command loads its envfile.
				cfg := filepath.Join(filepath.Dir(p.config), "invalid.caddy")
				if err := os.WriteFile(cfg, []byte("{\n invalid_directive\n}\n"), 0600); err != nil {
					t.Fatal(err)
				}
				out, err := run(t, verb, "--config", cfg, "--envfile", p.env)
				if err == nil {
					t.Fatal("invalid config accepted")
				}
				if reserved {
					if !strings.Contains(err.Error(), "janus mode") {
						t.Fatalf("reserved bind not rejected: %v %s", err, out)
					}
					if _, exists := os.LookupEnv("JT_MULTILINE"); exists {
						t.Fatal("partially applied rejected file")
					}
				} else {
					if got := os.Getenv("JT_MULTILINE"); got != "first\nsecond" {
						t.Fatalf("multiline value: %q (%v %s)", got, err, out)
					}
					if os.Getenv("JT_OVERRIDE") != "shell" || os.Getenv("JT_EMPTY") != "" {
						t.Fatal("envfile overwrote process environment")
					}
				}
			})
		}
	}
}

func TestEnvFilesPreserveFirstLoadedValue(t *testing.T) {
	t.Setenv("JT_ORDER", "")
	os.Unsetenv("JT_ORDER")
	dir := t.TempDir()
	for _, value := range []string{"first", "second"} {
		path := filepath.Join(dir, value)
		if err := os.WriteFile(path, []byte("JT_ORDER="+value+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := loadEnvFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("JT_ORDER") != "first" {
		t.Fatal("later envfile overwrote first value")
	}
}
