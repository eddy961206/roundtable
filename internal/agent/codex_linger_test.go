package agent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// 2026-10-28: codex exec stayed alive 1h45m after task_complete because a
// Gradle daemon started in the turn held its output handles; the turn never closed.
func TestCodexRunClosesAfterCompletedProcessLingers(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := codexExitGrace
	codexExitGrace = 200 * time.Millisecond
	t.Cleanup(func() { codexExitGrace = prev })
	t.Setenv("CODEX_HOME", t.TempDir())
	spec, _ := json.Marshal(fakeOpenCode{SleepMs: 20000, Stdout: []string{
		`{"type":"thread.started","thread_id":"th_1"}`,
		`{"type":"item.completed","item":{"id":"i1","type":"agent_message","text":"판정: 완료"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`,
	}})
	t.Setenv(fakeOpenCodeEnv, string(spec))
	start := time.Now()
	res, err := NewCodex(exe).Run(context.Background(), RunOptions{Mode: ModeChat, ScratchDir: t.TempDir()}, func(Event) {})
	if err != nil {
		t.Fatalf("error = %v, want nil after completed turn", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %s, want it to close soon after the grace", elapsed)
	}
	if res.Text != "판정: 완료" || !strings.Contains(res.Stderr, "끝나지 않아 프로세스를 정리함") {
		t.Fatalf("result = %q / stderr %q", res.Text, res.Stderr)
	}
}
