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
	prev := exitGrace
	exitGrace = 200 * time.Millisecond
	t.Cleanup(func() { exitGrace = prev })
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

// 2026-11-04: agy sent its result (its log: "stream input closed after 1
// turn(s)", completion signal) and then never reached "Language server shutting
// down"; the idle process held the turn open 25+ minutes. The lines are the
// shapes agy 1.5.0 printed in a measured run, including a failed tool call.
func TestAgyRunClosesAfterResultProcessLingers(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	prev := exitGrace
	exitGrace = 200 * time.Millisecond
	t.Cleanup(func() { exitGrace = prev })
	spec, _ := json.Marshal(fakeOpenCode{SleepMs: 20000, Stdout: []string{
		`{"event":"init","conversation_id":"c1","init":{"model":"gemini-3.8-flash"}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"ACTIVE","step_type":"tool","tool_name":"edit_file","tool_info":{"name":"edit_file","parameters":{"Path":"x.txt"}}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":2,"state":"ERROR","step_type":"tool","tool_name":"edit_file","tool_info":{"name":"edit_file","parameters":{"Path":"x.txt"},"error":{"type":"TOOL_ERROR","message":"target content not found in the file"}}}}`,
		`{"event":"step_update","step_update":{"conversation_id":"c1","step_index":3,"state":"DONE","step_type":"agent_response","text_delta":"끝\n","usage":{"input_tokens":1,"output_tokens":1}}}`,
		`{"event":"result","result":{"conversation_id":"c1","status":"SUCCESS","response":"끝\n","num_turns":1}}`,
	}})
	t.Setenv(fakeOpenCodeEnv, string(spec))
	var results []Event
	start := time.Now()
	res, err := NewAgy(exe, "").Run(context.Background(), RunOptions{Mode: ModeChat, ScratchDir: t.TempDir()}, func(e Event) {
		if e.Kind == KindToolResult {
			results = append(results, e)
		}
	})
	if err != nil {
		t.Fatalf("error = %v, want nil after the result", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Run took %s, want it to close soon after the grace", elapsed)
	}
	if res.Text != "끝" || res.SessionID != "c1" || !strings.Contains(res.Stderr, "끝나지 않아 프로세스를 정리함") {
		t.Fatalf("result = %+v", res)
	}
	// A failed call closes like a finished one, carrying its error.
	if len(results) != 1 || results[0].ID != "s2" || results[0].Meta["error"] != "target content not found in the file" {
		t.Fatalf("tool results = %+v", results)
	}
}
