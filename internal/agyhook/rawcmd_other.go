//go:build !windows

package agyhook

import "os/exec"

func setRawCmdLine(cmd *exec.Cmd, args string) {}
