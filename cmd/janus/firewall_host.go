package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

func goosForFirewall() string { return runtime.GOOS }

// runCmd runs a command, stderr folded into the error like runOut.
func runCmd(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
