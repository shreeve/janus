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
