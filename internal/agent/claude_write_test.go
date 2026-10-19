package agent

import (
	"strings"
	"testing"
)

func TestClaudeWriteModeAllowsBash(t *testing.T) {
	m := &MCPServer{Name: "gitea", Command: "rt", Args: []string{"gitea-mcp"}}
	count := func(args []string) int {
		n := 0
		for _, a := range args {
			if a == "--allowedTools" {
				n++
			}
		}
		return n
	}
	w := claudeArgs(RunOptions{Mode: ModeWrite, Workspace: "ws"})
	if j := strings.Join(w, " "); !strings.Contains(j, "--allowedTools Bash,PowerShell") || !strings.Contains(j, "--permission-mode acceptEdits") {
		t.Fatalf("write mode must pre-approve Bash: %s", j)
	}
	wm := claudeArgs(RunOptions{Mode: ModeWrite, Workspace: "ws", MCP: m})
	if j := strings.Join(wm, " "); count(wm) != 1 || !strings.Contains(j, "--allowedTools mcp__gitea,Bash,PowerShell") {
		t.Fatalf("one --allowedTools must carry MCP and Bash: %s", j)
	}
	for _, mode := range []string{ModeRead, ModeChat} {
		if j := strings.Join(claudeArgs(RunOptions{Mode: mode, Workspace: "ws", MCP: m}), " "); strings.Contains(j, "Bash") || strings.Contains(j, "PowerShell") {
			t.Fatalf("%s mode must not allow Bash: %s", mode, j)
		}
	}
}
