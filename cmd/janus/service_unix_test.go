//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRootServiceRejectsReplaceableExecutablePath(t *testing.T) {
	const target = "/usr/bin/true"
	if !rootOwnedAndPrivate(target) {
		t.Skip("no protected root-owned fixture executable")
	}
	dir := t.TempDir()
	// Also exercise the writable-directory check when tests run as root.
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "janus")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if rootOwnedAndPrivate(path) {
		t.Fatal("accepted replaceable link to a protected executable")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o755); err != nil {
		t.Fatal(err)
	}
	if rootOwnedAndPrivate(path) {
		t.Fatal("accepted executable under a writable directory")
	}
}

func TestExecutablePathFollowsFilesystemSymlinkSemantics(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"protected", "external/subdir"} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"protected/payload", "external/payload"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	links := map[string]string{
		"protected/bridge":   "../external/subdir",
		"protected/absolute": filepath.Join(dir, "protected") + "/bridge/../payload",
		"protected/relative": "bridge/../payload",
		"protected/safe":     "../protected/payload",
		"protected/chain":    "relative",
		"protected/cycle":    "cycle",
	}
	for name, target := range links {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Model protected ownership without requiring chown in a developer's
	// test run; filesystem resolution and link contents remain real.
	protected := func(st os.FileInfo) bool { return st.Name() != "external" }
	for _, name := range []string{"absolute", "relative", "chain", "cycle"} {
		if protectedExecutablePath(filepath.Join(dir, "protected", name), protected) {
			t.Errorf("accepted unsafe %s", name)
		}
	}
	if !protectedExecutablePath(filepath.Join(dir, "protected", "safe"), protected) {
		t.Fatal("rejected protected relative parent link")
	}
}
