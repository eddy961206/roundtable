//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
)

// hideWindow puts the CLI in its own process group so killTree can take the
// whole tree down with it.
func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killGroup(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) == nil
}
