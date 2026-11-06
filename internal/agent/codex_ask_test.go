package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCodexAsyncQuestionHasOneDeliveryPath(t *testing.T) {
	const thread = "01a181f0-afe5-7d43-8019-6b4ec62cd031"
	since := time.Now().Add(-time.Minute)
	day := time.Now()
	root := t.TempDir()
	dir := filepath.Join(root, day.Format("2006"), day.Format("01"), day.Format("02"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	questions := []AskQuestion{{Title: "어떤 항목을 사용할까?", Options: []string{"A", "B"}}}
	args, _ := json.Marshal(map[string]any{"questions": questions})
	call, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": "response_item", "payload": map[string]any{"type": "function_call", "name": "request_user_input_async", "call_id": "call-question", "arguments": string(args)}})
	path := filepath.Join(dir, "rollout-2026-11-06T14-00-00-"+thread+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"session_meta","payload":{"id":"`+thread+`"}}`+"\n"+string(call)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := newCodexParser()
	p.thread, p.turn = thread, "turn-question"
	packet, _ := json.Marshal(map[string]any{"method": "item/completed", "params": map[string]any{"threadId": thread, "turnId": p.turn, "item": map[string]any{"id": "call-question", "type": "agentMessage", "phase": "final_answer", "delivery": "async", "text": "어떤 항목을 사용할까?", "questions": questions}}})
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	p.Line(string(packet), emit)
	p.Line(string(packet), emit)
	w := newRolloutWatcher(root, since)
	w.scan(thread, emit)
	w.scan(thread, emit)
	asks := kinds(events, KindAsk)
	if len(asks) != 1 || asks[0].ID != "call-question" || !reflect.DeepEqual(asks[0].Meta["questions"], questions) {
		t.Fatalf("questions: %+v", events)
	}
	if p.completed {
		t.Fatal("an async question with phase final_answer is not turn completion")
	}
}

func TestCodexInvalidAsyncQuestionIsVisible(t *testing.T) {
	p := newCodexParser()
	p.turn = "t"
	var events []Event
	p.Line(`{"method":"item/completed","params":{"turnId":"t","item":{"id":"q","type":"agentMessage","delivery":"async","questions":[]}}}`, func(e Event) { events = append(events, e) })
	if len(events) != 1 || events[0].Kind != KindStatus || events[0].Text == "" {
		t.Fatal(events)
	}
}
