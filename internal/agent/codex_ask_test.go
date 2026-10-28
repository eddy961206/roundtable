package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// askLine is a rollout line as Codex 0.164.0 writes a request_user_input_async
// call (2026-10-28 SOW installer room); exec --json shows only its text.
func askLine(t *testing.T, at time.Time, callID string, questions []AskQuestion) string {
	t.Helper()
	args, err := json.Marshal(map[string]any{"questions": questions})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{"timestamp": at.UTC().Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{
		"type": "function_call", "status": "completed", "name": "request_user_input_async", "arguments": string(args), "call_id": callID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(line) + "\n"
}

func TestCodexRootRolloutAsks(t *testing.T) {
	const thread = "01a181f0-afe5-7d43-8019-6b4ec62cd031"
	since := time.Now().Add(-time.Minute)
	day := time.Now()
	dir := filepath.Join(t.TempDir(), day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "rollout-2026-10-28T18-56-37-"+thread+".jsonl")
	restore := []AskQuestion{{Title: "누락 파일만 복원해도 될까?", Options: []string{"누락 파일만 복원", "메인은 그대로 두기"}}}
	body := `{"timestamp":"2026-10-28T09:56:41.148Z","type":"session_meta","payload":{"id":"` + thread + `","originator":"codex_exec"}}` + "\n" +
		askLine(t, since.Add(-time.Hour), "call_earlier_turn", restore) +
		`{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"function_call","name":"exec","arguments":"{}","call_id":"call_exec"}}` + "\n" +
		askLine(t, time.Now(), "call_now", restore)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	w := newRolloutWatcher(filepath.Dir(filepath.Dir(filepath.Dir(dir))), since)
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	w.scan(thread, emit)
	asks := kinds(events, KindAsk)
	if len(events) != 1 || len(asks) != 1 || asks[0].ID != "call_now" || !reflect.DeepEqual(asks[0].Meta["questions"], restore) {
		t.Fatalf("only this turn's question should surface: %+v", events)
	}
	// The next scan reads only what was appended.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	free := []AskQuestion{{Title: "다음 단계는?"}}
	if _, err := f.WriteString(askLine(t, time.Now(), "call_next", free)); err != nil {
		t.Fatal(err)
	}
	f.Close()
	events = nil
	w.scan(thread, emit)
	if len(events) != 1 || events[0].ID != "call_next" || !reflect.DeepEqual(events[0].Meta["questions"], free) {
		t.Fatalf("appended question: %+v", events)
	}
}

func TestCodexRolloutAskBadArguments(t *testing.T) {
	line := `{"timestamp":"` + time.Now().UTC().Format(time.RFC3339Nano) + `","type":"response_item","payload":{"type":"function_call","name":"request_user_input_async","arguments":"{\"questions\":[]}","call_id":"call_bad"}}`
	x, ok := rolloutAsk(line, time.Now().Add(-time.Minute))
	if !ok || x.Kind != KindStatus || x.Text == "" {
		t.Fatalf("an accepted question that cannot be read must show in the trace: %+v", x)
	}
}
