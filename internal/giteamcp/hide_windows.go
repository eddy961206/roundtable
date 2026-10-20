//go:build windows

package giteamcp

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func hideWindow(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true} }

func stopTea(cmd *exec.Cmd) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill.exe", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	hideWindow(kill)
	if err := kill.Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
