//go:build !windows

package main

import (
	"os"
	"syscall"
)

// rootOwnedAndPrivate reports whether the file is owned by root and
// writable by no one else: what a system service may run.
func rootOwnedAndPrivate(path string) bool {
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	return ok && sys.Uid == 0 && st.Mode().Perm()&0o022 == 0
}
