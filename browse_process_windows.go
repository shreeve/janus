//go:build windows

package janus

import (
	"os"
	"os/exec"
	"time"
)

func configureBrowseProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Kill()
	}
	cmd.WaitDelay = 300 * time.Millisecond
}
