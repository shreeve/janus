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
