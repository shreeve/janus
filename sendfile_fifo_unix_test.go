//go:build !windows

package janus

import "golang.org/x/sys/unix"

func makeSendfileFIFO(name string) (bool, error) {
	return true, unix.Mkfifo(name, 0o600)
}
