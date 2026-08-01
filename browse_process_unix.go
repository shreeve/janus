//go:build !windows

package janus

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func configureBrowseProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		pgid, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			return err
		}
		termErr := syscall.Kill(-pgid, syscall.SIGTERM)
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		<-timer.C
		killErr := syscall.Kill(-pgid, syscall.SIGKILL)
		if errors.Is(killErr, syscall.ESRCH) {
			killErr = nil
		}
		if termErr != nil && !errors.Is(termErr, syscall.ESRCH) {
			return termErr
		}
		return killErr
	}
	cmd.WaitDelay = 300 * time.Millisecond
}
