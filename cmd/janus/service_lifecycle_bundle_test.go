package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A command that is a symlink into Janus.app registers the bundle's own
// executable; anything else registers the path it was invoked as.
func TestBundleExecutable(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "Applications", "Janus.app", "Contents", "MacOS", "janus")
	bare := filepath.Join(dir, "bin", "janus-bare")
	for _, p := range []string{inside, bare} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "bin", "janus")
	if err := os.Symlink(inside, link); err != nil {
		t.Fatal(err)
	}
	resolvedInside, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ in, want string }{
		{link, resolvedInside},
		{inside, resolvedInside},
		{bare, bare},
		{filepath.Join(dir, "missing"), filepath.Join(dir, "missing")},
	} {
		if got := bundleExecutable(tc.in); got != tc.want {
			t.Errorf("bundleExecutable(%s) = %s, want %s", tc.in, got, tc.want)
		}
	}
	if got := bundlePath(resolvedInside); filepath.Base(got) != "Janus.app" {
		t.Errorf("bundlePath = %s", got)
	}
	for path, want := range map[string]bool{
		"/Applications/Janus.app/Contents/MacOS/janus": true,
		"/Users/x/.local/bin/janus":                    false,
		"/x/Janus.app/janus":                           false,
		"/x/Janus.app/Contents/Resources/janus":        false,
	} {
		if inBundle(path) != want {
			t.Errorf("inBundle(%s) = %v", path, !want)
		}
	}
}
