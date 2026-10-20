package agent

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestNativeAndWorkerPermissionPolicies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{ModeRead, ModeWrite, ModeChat} {
		o := RunOptions{Mode: mode, Workspace: t.TempDir(), ScratchDir: t.TempDir(), SessionID: "specific-session"}
		for _, sandbox := range []string{"", "unelevated", "elevated"} {
			c := NewCodex(exe)
			c.SetWindowsSandbox(sandbox)
			plan, err := NativeSessionCommand(c, o)
			if err != nil {
				t.Fatal(err)
			}
			worker := codexArgs(o, sandbox)
			for _, setting := range codexPermissionArgs(mode, sandbox) {
				if setting != "-c" && (!slices.Contains(plan.Args, setting) || !slices.Contains(worker, setting)) {
					t.Fatalf("native/worker policy differs: %v / %v", plan.Args, worker)
				}
			}
			if strings.Contains(strings.Join(plan.Args, " "), "windows.sandbox=") != (sandbox != "") || slices.Contains(plan.Args, "--sandbox") {
				t.Fatal(plan.Args)
			}
		}
		for _, p := range []Provider{NewClaude(exe), NewAgy(exe, "")} {
			plan, err := NativeSessionCommand(p, o)
			if err != nil {
				t.Fatal(err)
			}
			var worker []string
			if p.ID() == "claude" {
				worker = claudeArgs(o)
			} else {
				worker = agyArgs(o)
			}
			for _, args := range [][]string{plan.Args, worker} {
				if slices.Contains(args, "--sandbox") || slices.Contains(args, "--dangerously-skip-permissions") {
					t.Fatal(args)
				}
				joined := strings.Join(args, " ")
				if p.ID() == "claude" {
					if !slices.Contains(args, claudeSandboxSettings) || strings.Contains(joined, "Bash,PowerShell") != (mode == ModeWrite) {
						t.Fatal(args)
					}
					if mode == ModeRead && !strings.Contains(joined, "--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch") {
						t.Fatal(args)
					}
				} else if !strings.Contains(joined, map[bool]string{true: "--mode accept-edits", false: "--mode plan"}[mode == ModeWrite]) {
					t.Fatal(args)
				}
			}
		}
	}
}
