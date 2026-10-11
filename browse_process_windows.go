//go:build windows

package janus

import (
	"os"
	"os/exec"
	"strings"
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

// browseRendererPlatformEnv names the Windows variables a child process
// needs to locate the system, its temporary directory, and executables.
var browseRendererPlatformEnv = []string{"SYSTEMROOT", "TEMP", "TMP", "PATHEXT", "USERPROFILE"}

// browseEnvKeyEqual compares environment keys case-insensitively, as Windows
// does.
func browseEnvKeyEqual(key, name string) bool { return strings.EqualFold(key, name) }
