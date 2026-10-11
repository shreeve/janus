//go:build !windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// After a roll, what the edge writes to stdout and stderr lands in the
// fresh file at the path, not the rolled one. Run in a child process: the
// reopen replaces the process's own descriptors 1 and 2.
func TestReopenSupervisorLog(t *testing.T) {
	if path := os.Getenv("JANUS_TEST_REOPEN"); path != "" {
		if err := reopenSupervisorLog(path, 0o600, true, true); err != nil {
			os.Exit(3)
		}
		fmt.Fprintln(os.Stdout, "to stdout")
		fmt.Fprintln(os.Stderr, "to stderr")
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "supervisor.log")
	rolled, err := os.Create(path + ".1")
	if err != nil {
		t.Fatal(err)
	}
	defer rolled.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReopenSupervisorLog$")
	cmd.Env = append(os.Environ(), "JANUS_TEST_REOPEN="+path)
	cmd.Stdout, cmd.Stderr = rolled, rolled
	if err := cmd.Run(); err != nil {
		t.Fatalf("child: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "to stdout\nto stderr\n" {
		t.Errorf("fresh log holds %q", got)
	}
	if old, _ := os.ReadFile(path + ".1"); len(old) != 0 {
		t.Errorf("the rolled log was written: %q", old)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("fresh log mode: %v %v", st, err)
	}
}
