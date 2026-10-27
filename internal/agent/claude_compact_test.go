package agent

import (
	"slices"
	"testing"
)

// 2026-10-28 A/B (docs/토큰효율_측정_20261028.md): a review+fix turn forked
// from a 120k session used 38% less input when the CLI compacted at a 150k
// window, with the same defects found and fixed. The window is passed only
// when the room sets one; the user's own CLI settings are never changed.
func TestClaudeEnvCarriesCompactWindow(t *testing.T) {
	t.Setenv("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "999")
	if env := claudeEnv(RunOptions{}); slices.Contains(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=999") {
		t.Fatal("without a room window the CLI keeps its own default, not the server's environment")
	}
	env := claudeEnv(RunOptions{CompactWindow: 150000})
	if !slices.Contains(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=150000") || slices.Contains(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW=999") {
		t.Fatalf("%v", env)
	}
}
