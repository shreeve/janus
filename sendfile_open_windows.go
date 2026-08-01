//go:build windows

package janus

import "os"

func openSendfile(name string) (*os.File, error) {
	return os.Open(name)
}
