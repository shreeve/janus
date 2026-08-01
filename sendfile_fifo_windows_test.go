//go:build windows

package janus

func makeSendfileFIFO(string) (bool, error) {
	return false, nil
}
