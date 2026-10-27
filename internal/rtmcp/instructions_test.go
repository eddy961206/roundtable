package rtmcp

import (
	"os"
	"strings"
	"testing"
)

// An agent connected only through MCP must be able to find the agent guide,
// and the guide must exist where the instructions point.
func TestInstructionsPointToAgentGuide(t *testing.T) {
	for _, want := range []string{"docs/AGENT_GUIDE.md", "방 하나", "[[REQUEST:@좌석ID]]", "rt_wait", "HTTP 전용"} {
		if !strings.Contains(instructions, want) {
			t.Errorf("instructions에 %q 없음", want)
		}
	}
	if _, err := os.Stat("../../docs/AGENT_GUIDE.md"); err != nil {
		t.Fatal(err)
	}
}
