package agent

import (
	"slices"
	"strings"
	"testing"
)

func TestCodexGiteaFreshAndResume(t *testing.T) {
	m := &MCPServer{Name: "gitea", Command: `C:\server path\roundtable.exe`, Args: []string{"gitea-mcp"}}
	for _, mode := range []string{ModeRead, ModeWrite, ModeChat} {
		for _, session := range []string{"", "saved-session"} {
			o := RunOptions{Mode: mode, SessionID: session, MCP: m, Prompt: "PR #11 상태를 알려줘"}
			args := codexArgs(o, "unelevated")
			for _, setting := range []string{
				"mcp_servers.gitea.enabled=true", "mcp_servers.gitea.required=true",
				"mcp_servers.gitea.startup_timeout_sec=10", "mcp_servers.gitea.tool_timeout_sec=25",
			} {
				if slices.Contains(args, setting) != (mode != ModeChat) {
					t.Fatalf("%s/%s missing or unexpected %s: %v", mode, session, setting, args)
				}
			}
			prompt := codexPrompt(o)
			first := mode != ModeChat && session == ""
			if (prompt != o.Prompt) != first || !strings.HasSuffix(prompt, o.Prompt) {
				t.Fatalf("%s/%s prompt: %s", mode, session, prompt)
			}
			if first && !strings.Contains(prompt, "gitea_pull") {
				t.Fatal("Gitea tools must be named for deferred discovery")
			}
			for _, implementation := range []string{"functions.exec", "ALL_TOOLS", "slice", "샌드박스"} {
				if strings.Contains(prompt, implementation) {
					t.Fatalf("implementation detail in Gitea guidance: %s", prompt)
				}
			}
		}
	}
	o := RunOptions{Mode: ModeRead, Prompt: "no server"}
	if codexPrompt(o) != o.Prompt {
		t.Fatal("unconfigured tools must not be advertised")
	}
}
