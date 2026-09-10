//go:build !windows

package janus

import (
	"os"

	"golang.org/x/sys/unix"
)

func openSendfile(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

// Open before inspecting the descriptor so replacement cannot evade root
// confinement or the caller's type check. O_NONBLOCK prevents a FIFO from
// waiting for a writer before that check can run.
func openRootFile(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|unix.O_NONBLOCK, 0)
}
