//go:build !windows

package janus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallerPreservesOldBinaryOnFailure(t *testing.T) {
	for _, failure := range []string{"copy", "capability", "none"} {
		t.Run(failure, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			mocks := filepath.Join(dir, "mocks")
			for _, path := range []string{bin, mocks} {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			source, dest := filepath.Join(dir, "new-janus"), filepath.Join(bin, "janus")
			write(source, "#!/bin/sh\necho new\n")
			write(dest, "#!/bin/sh\necho old\n")
			switch failure {
			case "copy":
				write(filepath.Join(mocks, "install"), "#!/bin/sh\nprintf partial >\"$4\"\nexit 9\n")
			case "capability":
				write(filepath.Join(mocks, "uname"), "#!/bin/sh\necho Linux\n")
				write(filepath.Join(mocks, "id"), "#!/bin/sh\necho 0\n")
				write(filepath.Join(mocks, "getcap"), "#!/bin/sh\necho \"$1 cap_net_bind_service=ep\"\n")
				write(filepath.Join(mocks, "setcap"), "#!/bin/sh\nexit 9\n")
			}
			cmd := exec.Command("bash", "scripts/release-install.sh", source)
			cmd.Env = append(os.Environ(), "BIN="+bin, "PATH="+mocks+string(os.PathListSeparator)+os.Getenv("PATH"), "NO_COLOR=1")
			out, err := cmd.CombinedOutput()
			if (err == nil) != (failure == "none") {
				t.Fatalf("install result: %v %s", err, out)
			}
			out, err = exec.Command(dest).Output()
			if err != nil {
				t.Fatal(err)
			}
			want := "old"
			if failure == "none" {
				want = "new"
			}
			if strings.TrimSpace(string(out)) != want {
				t.Fatalf("installed executable = %s, want %s", out, want)
			}
			stages, err := filepath.Glob(filepath.Join(bin, ".janus.install.*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("staging leak: %v %v", stages, err)
			}
		})
	}
}

// The installer gives every macOS install the one code-signing identifier
// Local Network privacy files its grant under, signing the staged copy (a
// fresh file) when the identifier differs and leaving a correctly named
// binary alone.
func TestInstallerSignsWithStableIdentifier(t *testing.T) {
	const identifier = "com.github.shreeve.janus"
	for _, current := range []string{"a.out", identifier} {
		t.Run(current, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			mocks := filepath.Join(dir, "mocks")
			for _, path := range []string{bin, mocks} {
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			}
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			source, dest := filepath.Join(dir, "new-janus"), filepath.Join(bin, "janus")
			write(source, "#!/bin/sh\necho new\n")
			signed := filepath.Join(dir, "codesign.log")
			write(filepath.Join(mocks, "uname"), "#!/bin/sh\necho Darwin\n")
			write(filepath.Join(mocks, "codesign"), "#!/bin/sh\ncase \"$1\" in\n-dv) echo 'Identifier="+current+"' >&2 ;;\n-s) printf '%s\\n' \"$*\" >> '"+signed+"' ;;\nesac\n")
			cmd := exec.Command("bash", "scripts/release-install.sh", source)
			cmd.Env = append(os.Environ(), "BIN="+bin, "PATH="+mocks+string(os.PathListSeparator)+os.Getenv("PATH"), "NO_COLOR=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("install: %v %s", err, out)
			}
			out, err := exec.Command(dest).Output()
			if err != nil || strings.TrimSpace(string(out)) != "new" {
				t.Fatalf("installed executable = %q, %v", out, err)
			}
			log, _ := os.ReadFile(signed)
			// The installer stages beside the resolved destination directory.
			resolved, err := filepath.EvalSymlinks(bin)
			if err != nil {
				t.Fatal(err)
			}
			switch {
			case current == identifier && len(log) != 0:
				t.Fatalf("re-signed a correctly named binary: %s", log)
			case current != identifier && !strings.Contains(string(log), "-s - -f -i "+identifier+" "+resolved+"/.janus.install."):
				t.Fatalf("expected the staged copy signed as %s, got %q", identifier, log)
			}
		})
	}
}

// On macOS the installer lays down Janus.app and links the command into it:
// from the bundle path (make install) or through the archive's symlink; an
// existing bare binary becomes the symlink; an upgrade swaps the whole
// bundle; something else at the bundle's place is refused.
func TestInstallerInstallsBundleOnDarwin(t *testing.T) {
	const identifier = "com.github.shreeve.janus"
	setup := func(t *testing.T, body string) (dir, bin, mocks, app string, env []string) {
		t.Helper()
		dir = t.TempDir()
		bin = filepath.Join(dir, "bin")
		mocks = filepath.Join(dir, "mocks")
		home := filepath.Join(dir, "home")
		app = filepath.Join(dir, "src", "Janus.app")
		for _, p := range []string{bin, mocks, home, filepath.Join(app, "Contents", "MacOS")} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(path, body string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(app, "Contents", "Info.plist"), "<plist/>\n")
		write(filepath.Join(app, "Contents", "MacOS", "janus"), "#!/bin/sh\necho "+body+"\n")
		write(filepath.Join(mocks, "uname"), "#!/bin/sh\necho Darwin\n")
		write(filepath.Join(mocks, "codesign"), "#!/bin/sh\ncase \"$1\" in\n-dv) echo 'Identifier="+identifier+"' >&2 ;;\n--verify) exit 0 ;;\nesac\n")
		write(filepath.Join(mocks, "ditto"), "#!/bin/sh\ncp -R \"$1\" \"$2\"\n")
		write(filepath.Join(mocks, "lsregister"), "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+filepath.Join(dir, "lsregister.log")+"'\n")
		env = append(os.Environ(), "BIN="+bin, "HOME="+home, "PATH="+mocks+string(os.PathListSeparator)+os.Getenv("PATH"), "NO_COLOR=1")
		return
	}
	run := func(t *testing.T, env []string, source string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command("bash", "scripts/release-install.sh", source)
		cmd.Env = env
		return cmd.CombinedOutput()
	}
	check := func(t *testing.T, dir, bin, want string) {
		t.Helper()
		installed := filepath.Join(dir, "home", "Applications", "Janus.app", "Contents", "MacOS", "janus")
		link := filepath.Join(bin, "janus")
		target, err := os.Readlink(link)
		if err != nil || target != installed {
			t.Fatalf("%s -> %q, %v; want %s", link, target, err, installed)
		}
		out, err := exec.Command(link).Output()
		if err != nil || strings.TrimSpace(string(out)) != want {
			t.Fatalf("installed command = %q, %v; want %s", out, err, want)
		}
		for _, glob := range []string{filepath.Join(bin, ".janus.install.*"), filepath.Join(dir, "home", "Applications", ".Janus.app.*")} {
			if stale, _ := filepath.Glob(glob); len(stale) != 0 {
				t.Fatalf("staging leak: %v", stale)
			}
		}
	}

	t.Run("from bundle path, then upgrade", func(t *testing.T) {
		dir, bin, _, app, env := setup(t, "new")
		if out, err := run(t, env, app); err != nil {
			t.Fatalf("install: %v %s", err, out)
		}
		check(t, dir, bin, "new")
		if log, _ := os.ReadFile(filepath.Join(dir, "lsregister.log")); !strings.Contains(string(log), "-f "+filepath.Join(dir, "home", "Applications", "Janus.app")) {
			t.Fatalf("bundle not registered with LaunchServices: %q", log)
		}
		if err := os.WriteFile(filepath.Join(app, "Contents", "MacOS", "janus"), []byte("#!/bin/sh\necho newer\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, env, app); err != nil {
			t.Fatalf("upgrade: %v %s", err, out)
		}
		check(t, dir, bin, "newer")
	})
	t.Run("through the archive symlink over a bare binary", func(t *testing.T) {
		dir, bin, _, app, env := setup(t, "new")
		link := filepath.Join(dir, "src", "janus")
		if err := os.Symlink("Janus.app/Contents/MacOS/janus", link); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "janus"), []byte("#!/bin/sh\necho old\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := run(t, env, link); err != nil {
			t.Fatalf("install: %v %s", err, out)
		}
		check(t, dir, bin, "new")
		_ = app
	})
	t.Run("refuses a non-bundle at the destination", func(t *testing.T) {
		dir, _, _, app, env := setup(t, "new")
		bad := filepath.Join(dir, "home", "Applications", "Janus.app")
		if err := os.MkdirAll(bad, 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, env, app)
		if err == nil || !strings.Contains(string(out), "not an application bundle") {
			t.Fatalf("expected refusal, got %v %s", err, out)
		}
	})
	t.Run("refuses a foreign identifier", func(t *testing.T) {
		_, _, mocks, app, env := setup(t, "new")
		if err := os.WriteFile(filepath.Join(mocks, "codesign"), []byte("#!/bin/sh\necho 'Identifier=a.out' >&2\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		out, err := run(t, env, app)
		if err == nil || !strings.Contains(string(out), "signed as 'a.out'") {
			t.Fatalf("expected refusal, got %v %s", err, out)
		}
	})
}

// A quarantined bundle is refused with the command that clears it; root
// installs the bare executable from the bundle, re-signed on its own.
func TestInstallerBundleEdgeCases(t *testing.T) {
	const identifier = "com.github.shreeve.janus"
	setup := func(t *testing.T) (dir, bin, mocks, app string, env []string) {
		t.Helper()
		dir = t.TempDir()
		bin = filepath.Join(dir, "bin")
		mocks = filepath.Join(dir, "mocks")
		home := filepath.Join(dir, "home")
		app = filepath.Join(dir, "src", "Janus.app")
		for _, p := range []string{bin, mocks, home, filepath.Join(app, "Contents", "MacOS")} {
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write := func(path, body string) {
			t.Helper()
			if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		write(filepath.Join(app, "Contents", "Info.plist"), "<plist/>\n")
		write(filepath.Join(app, "Contents", "MacOS", "janus"), "#!/bin/sh\necho new\n")
		write(filepath.Join(mocks, "uname"), "#!/bin/sh\necho Darwin\n")
		write(filepath.Join(mocks, "codesign"), "#!/bin/sh\ncase \"$1\" in\n-dv) echo 'Identifier="+identifier+"' >&2 ;;\n--verify) exit 0 ;;\n-s) printf '%s\\n' \"$*\" >> '"+filepath.Join(dir, "codesign.log")+"' ;;\nesac\n")
		write(filepath.Join(mocks, "ditto"), "#!/bin/sh\ncp -R \"$1\" \"$2\"\n")
		write(filepath.Join(mocks, "lsregister"), "#!/bin/sh\n")
		env = append(os.Environ(), "BIN="+bin, "HOME="+home, "PATH="+mocks+string(os.PathListSeparator)+os.Getenv("PATH"), "NO_COLOR=1")
		return
	}
	t.Run("quarantined bundle is refused", func(t *testing.T) {
		dir, bin, mocks, app, env := setup(t)
		// xattr -p com.apple.quarantine succeeds: the flag is present.
		if err := os.WriteFile(filepath.Join(mocks, "xattr"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "scripts/release-install.sh", app)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "xattr -dr com.apple.quarantine") {
			t.Fatalf("expected refusal naming the fix, got %v %s", err, out)
		}
		if _, err := os.Lstat(filepath.Join(bin, "janus")); err == nil {
			t.Fatal("command installed despite refusal")
		}
		if stale, _ := filepath.Glob(filepath.Join(dir, "home", "Applications", ".Janus.app.*")); len(stale) != 0 {
			t.Fatalf("staging leak: %v", stale)
		}
	})
	t.Run("root installs the bare executable", func(t *testing.T) {
		dir, bin, mocks, app, env := setup(t)
		if err := os.WriteFile(filepath.Join(mocks, "id"), []byte("#!/bin/sh\necho 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "scripts/release-install.sh", app)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("install: %v %s", err, out)
		}
		dest := filepath.Join(bin, "janus")
		if st, err := os.Lstat(dest); err != nil || st.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("root command should be a regular file: %v %v", st, err)
		}
		if out, err := exec.Command(dest).Output(); err != nil || strings.TrimSpace(string(out)) != "new" {
			t.Fatalf("installed command = %q, %v", out, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "home", "Applications", "Janus.app")); err == nil {
			t.Fatal("root must not receive a bundle")
		}
		if log, _ := os.ReadFile(filepath.Join(dir, "codesign.log")); !strings.Contains(string(log), "-i "+identifier) {
			t.Fatalf("bare executable from the bundle must be re-signed: %q", log)
		}
	})
}
