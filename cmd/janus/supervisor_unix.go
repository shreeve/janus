//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// reopenSupervisorLog opens path (append, created at perm) and makes it
// this process's stdout and/or stderr, in place of the descriptors the
// manager handed over, which name the rolled file.
func reopenSupervisorLog(path string, perm os.FileMode, stdout, stderr bool) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	fd := int(f.Fd())
	if stdout {
		if err := unix.Dup2(fd, 1); err != nil {
			return err
		}
	}
	if stderr {
		if err := unix.Dup2(fd, 2); err != nil {
			return err
		}
	}
	return nil
}
