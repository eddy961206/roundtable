package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func appendTranscript(t *testing.T, path, data string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAgyTranscriptPollingPreservesPartialAndFinalLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	first := `{"type":"PLANNER_RESPONSE","content":"first"}` + "\n"
	large := strings.Repeat("한", 100_000) // larger than the shared reader buffer
	partial := `{"type":"PLANNER_RESPONSE","content":"` + large
	appendTranscript(t, path, first+partial)
	p := newAgyParser()
	sub := &agySub{logPath: path}
	p.subs["child"] = sub
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	p.readTranscripts(emit, false)
	if len(events) != 1 || events[0].Text != "first" || sub.offset != int64(len(first)) {
		t.Fatalf("partial line must wait: events=%d offset=%d", len(events), sub.offset)
	}
	p.readTranscripts(emit, false)
	if len(events) != 1 || sub.offset != int64(len(first)) {
		t.Fatal("an unchanged partial line advanced or duplicated events")
	}

	// Give the reusable reader a different file between polls. Its buffered
	// partial line must not leak to another transcript or be lost on returning.
	otherPath := filepath.Join(t.TempDir(), "other.jsonl")
	appendTranscript(t, otherPath, `{"type":"PLANNER_RESPONSE","content":"other"}`+"\n")
	other := newAgyParser()
	other.subs["other"] = &agySub{logPath: otherPath}
	other.readTranscripts(func(e Event) {
		if e.Text != "other" || e.Parent != "other" {
			t.Fatalf("cross-transcript data: %+v", e)
		}
	}, false)

	completed := `"}` + "\n"
	last := `{"type":"PLANNER_RESPONSE","content":"last without newline"}`
	appendTranscript(t, path, completed+last)
	p.readTranscripts(emit, false)
	if len(events) != 2 || events[1].Text != large || sub.offset != int64(len(first+partial+completed)) {
		t.Fatalf("completed large line was lost/duplicated: events=%d offset=%d", len(events), sub.offset)
	}
	p.Finish(emit)
	if len(events) != 4 || events[2].Text != "last without newline" || events[3].Kind != KindSubagentEnd || !sub.ended || sub.offset != int64(len(first+partial+completed+last)) {
		t.Fatalf("final tail/end contract: events=%d ended=%v offset=%d", len(events), sub.ended, sub.offset)
	}
	p.Finish(emit)
	if len(events) != 4 {
		t.Fatalf("final transcript was replayed: %d events", len(events))
	}
}

func TestCodexRolloutPollingPreservesPartialAndResumedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	first := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"first"}}` + "\n"
	large := strings.Repeat("x", 300_000)
	partial := `{"type":"response_item","payload":{"type":"function_call","call_id":"c1","name":"tool","arguments":"` + large
	appendTranscript(t, path, first+partial)
	w := &rolloutWatcher{}
	rf := &rolloutFile{path: path, id: "child"}
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	w.tail(rf, emit)
	w.tail(rf, emit)
	if len(events) != 1 || events[0].Text != "first" || rf.offset != int64(len(first)) {
		t.Fatalf("partial line must wait: events=%d offset=%d", len(events), rf.offset)
	}
	completed := `"}}` + "\n"
	end := `{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"done"}}` + "\n"
	appendTranscript(t, path, completed+end)
	w.tail(rf, emit)
	if len(events) != 3 || events[1].Input != large || events[2].Kind != KindSubagentEnd || !rf.ended {
		t.Fatalf("large line/end lost: events=%d ended=%v", len(events), rf.ended)
	}
	// An ended subagent may receive another turn. Polling must keep following
	// that same file, including withholding a complete JSON value until newline.
	resume := `{"type":"response_item","payload":{"type":"message","role":"assistant","content":"resumed"}}`
	appendTranscript(t, path, resume)
	w.tail(rf, emit)
	if len(events) != 3 {
		t.Fatal("Codex incomplete trailing line must wait for newline")
	}
	appendTranscript(t, path, "\n")
	w.tail(rf, emit)
	if len(events) != 4 || events[3].Text != "resumed" || rf.offset != int64(len(first+partial+completed+end+resume+"\n")) {
		t.Fatalf("resumed line was lost/duplicated: events=%d offset=%d", len(events), rf.offset)
	}
}

func TestTranscriptPollingConcurrent(t *testing.T) {
	for i := 0; i < 32; i++ {
		t.Run(fmt.Sprintf("seat_%d", i), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			id := fmt.Sprintf("child-%d", i)
			p := newAgyParser()
			p.subs[id] = &agySub{logPath: path}
			w := &rolloutWatcher{}
			rf := &rolloutFile{path: path, id: id}
			var events []Event
			emit := func(e Event) { events = append(events, e) }
			poll := func() {
				if i%2 == 0 {
					p.readTranscripts(emit, false)
				} else {
					w.tail(rf, emit)
				}
			}
			poll() // an unavailable file must remain retryable
			for j := 0; j < 16; j++ {
				want := fmt.Sprintf("%s-message-%d", id, j)
				line := fmt.Sprintf(`{"type":"PLANNER_RESPONSE","content":%q}`, want)
				if i%2 != 0 {
					line = fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":%q}}`, want)
				}
				appendTranscript(t, path, line+"\n")
				poll()
				poll() // EOF must not repeat the last accepted line
				if len(events) != j+1 || events[j].Parent != id || events[j].Text != want {
					t.Fatalf("crossed or duplicated events after poll %d: %+v", j, events)
				}
			}
		})
	}
}
