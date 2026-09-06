package main

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"

	caddycmd "github.com/caddyserver/caddy/v2/cmd"
	"github.com/spf13/cobra"
)

// caddyAsCommand matches help text that tells the reader to type caddy, or
// calls the running process Caddy. Caddy's nouns (Caddyfile, Caddy's native
// JSON, Caddy modules, caddyserver.com) are not matched: they are accurate.
var caddyAsCommand = regexp.MustCompile(
	`(['"$>] ?|\bwhich )caddy\b|\bCaddy (process|instances?)\b|\b(runs|stop) Caddy\b`)

func helpOf(t *testing.T, root *cobra.Command, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(append([]string{"help"}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("help %v: %v", args, err)
	}
	return out.String()
}

func walk(cmd *cobra.Command, fn func(path []string, c *cobra.Command)) {
	var rec func(c *cobra.Command, path []string)
	rec = func(c *cobra.Command, path []string) {
		fn(path, c)
		for _, sub := range c.Commands() {
			rec(sub, append(append([]string{}, path...), sub.Name()))
		}
	}
	rec(cmd, nil)
}

func TestHelpNamesJanusAsTheCommand(t *testing.T) {
	root := newRootCommand()
	walk(root, func(path []string, c *cobra.Command) {
		if c.Hidden {
			return
		}
		text := helpOf(t, newRootCommand(), path...)
		if m := caddyAsCommand.FindString(text); m != "" {
			t.Errorf("help for %q still says %q:\n%s", strings.Join(path, " "), m, text)
		}
	})
	rootHelp := helpOf(t, newRootCommand())
	for _, want := range []string{"$ janus run", "'janus start'", "which janus", "janus janus-auth-hash", "caddyserver.com/docs/command-line"} {
		if !strings.Contains(rootHelp, want) {
			t.Errorf("root help lacks %q", want)
		}
	}
}

func TestEveryRegisteredCaddyCommandIsPresent(t *testing.T) {
	root := newRootCommand()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for name := range caddycmd.Commands() {
		if !have[name] {
			t.Errorf("registered command %q missing from the janus tree", name)
		}
	}
	for _, name := range []string{"manpage", "completion", "janus-auth-hash", "run", "adapt", "validate"} {
		if !have[name] {
			t.Errorf("expected command %q missing", name)
		}
	}
}

func TestJanusifyKeepsCaddyNouns(t *testing.T) {
	in := "Adapts a configuration to Caddy's native JSON. Formats a Caddyfile with the caddyfile adapter; see https://caddyserver.com/docs. Use 'caddy adapt' on the Caddy process, or $ caddy run; then $(which caddy)."
	got := janusify(in)
	for _, keep := range []string{"Caddy's native JSON", "Caddyfile", "caddyfile adapter", "caddyserver.com/docs"} {
		if !strings.Contains(got, keep) {
			t.Errorf("janusify dropped %q: %s", keep, got)
		}
	}
	for _, want := range []string{"'janus adapt'", "Janus process", "$ janus run", "which janus"} {
		if !strings.Contains(got, want) {
			t.Errorf("janusify missed %q: %s", want, got)
		}
	}
}

func TestExitCodeReadsCaddysAndOurs(t *testing.T) {
	// Caddy's exitError is unexported; obtain a real one through the
	// exported wrapper, as every Caddy command does.
	run := caddycmd.WrapCommandFuncForCobra(func(caddycmd.Flags) (int, error) { return 3, errors.New("boom") })
	err := run(&cobra.Command{}, nil)
	if got := exitCode(err); got != 3 {
		t.Fatalf("caddy exit code: want 3, got %d", got)
	}
	if got := exitCode(&exitError{code: 2, err: errors.New("x")}); got != 2 {
		t.Fatalf("own exit code: want 2, got %d", got)
	}
	if got := exitCode(errors.New("plain")); got != 1 {
		t.Fatalf("plain error: want 1, got %d", got)
	}
}

func TestPackageCommandsAreRefused(t *testing.T) {
	for _, name := range []string{"upgrade", "add-package", "remove-package"} {
		root := newRootCommand()
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&bytes.Buffer{})
		root.SetArgs([]string{name})
		if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "rebuild Janus from source") {
			t.Errorf("%s: want the refusal, got %v", name, err)
		}
	}
}
