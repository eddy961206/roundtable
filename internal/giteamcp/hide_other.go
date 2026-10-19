//go:build !windows

package giteamcp

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}
