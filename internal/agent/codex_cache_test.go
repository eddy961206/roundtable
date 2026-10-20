package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCodexCacheFreshAndResumeArguments(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "server scratch 한글")
	cache := filepath.Join(scratch, "codex", "go-build")
	for _, session := range []string{"", "existing-session"} {
		for _, mode := range []string{ModeWrite, ModeRead, ModeChat} {
			for _, sandbox := range []string{"", "unelevated", "elevated"} {
				args := codexArgs(RunOptions{Mode: mode, ScratchDir: scratch, Workspace: t.TempDir(), SessionID: session}, sandbox)
				if slices.Contains(args, "shell_environment_policy.set.GOCACHE="+tomlString(cache)) != (mode == ModeWrite) {
					t.Fatal(args)
				}
				if slices.Contains(args, "sandbox_workspace_write.writable_roots=["+tomlString(cache)+"]") != (mode == ModeWrite && sandbox != "") {
					t.Fatal(args)
				}
				policy := "danger-full-access"
				if sandbox != "" {
					policy = "read-only"
					if mode == ModeWrite {
						policy = "workspace-write"
					}
				}
				if !slices.Contains(args, `sandbox_mode="`+policy+`"`) || !slices.Contains(args, `approval_policy="never"`) {
					t.Fatal(args)
				}
				if strings.Contains(strings.Join(args, " "), "windows.sandbox=") != (sandbox != "") {
					t.Fatal(args)
				}
				if session != "" && (args[1] != "resume" || args[len(args)-2] != session) {
					t.Fatal(args)
				}
			}
		}
	}
}

func TestCodexCacheSetupFailureAndReadMode(t *testing.T) {
	// An invalid executable lets setup run, then fails at launch without
	// starting a vendor or a child test process that can lock the test binary.
	executable := filepath.Join(t.TempDir(), "codex.exe")
	if err := os.WriteFile(executable, []byte("not an executable"), 0600); err != nil {
		t.Fatal(err)
	}
	c := NewCodex(executable)
	scratch := filepath.Join(t.TempDir(), "scratch")
	ctx := context.Background()
	for _, mode := range []string{ModeRead, ModeChat} {
		if _, err := c.Run(ctx, RunOptions{Mode: mode, Workspace: t.TempDir(), ScratchDir: scratch}, func(Event) {}); err == nil {
			t.Fatal("invalid executable unexpectedly started")
		}
		if _, err := os.Stat(scratch); !os.IsNotExist(err) {
			t.Fatalf("%s created scratch: %v", mode, err)
		}
	}
	if err := os.WriteFile(scratch, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := c.Run(ctx, RunOptions{Mode: ModeWrite, Workspace: t.TempDir(), ScratchDir: scratch}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "Go 빌드 캐시 생성") {
		t.Fatalf("setup error hidden: %v", err)
	}
	_, err = c.Run(ctx, RunOptions{Mode: ModeWrite, ScratchDir: "relative"}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "절대 경로") {
		t.Fatalf("relative cache accepted: %v", err)
	}
}
