//go:build windows

package agyhook

import (
	"os/exec"
	"syscall"
)

// setRawCmdLine passes the chained command to cmd.exe verbatim; Go's default
// argument quoting would escape the user's own quotes.
func setRawCmdLine(cmd *exec.Cmd, args string) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: "cmd.exe " + args, HideWindow: true}
}
