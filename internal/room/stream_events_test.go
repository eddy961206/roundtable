package room

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"roundtable/internal/agent"
)

func TestTypedStreamEnvelopesPreserveWireBytes(t *testing.T) {
	participant := &Participant{ID: "seat", Label: "한글 <>&", Usage: map[string]any{"input_tokens": 12, "nested": map[string]any{"text": "line\nnext"}}}
	event := agent.Event{Kind: agent.KindTextDelta, Text: "한글 <>&\n\"quoted\""}
	for _, tc := range []struct {
		kind     string
		new, old any
	}{
		{"delta", textDeltaEvent{ID: "m", Text: event.Text}, map[string]any{"id": "m", "text": event.Text}},
		{"delta", textDeltaEvent{ID: "m", Text: event.Text, Block: true}, map[string]any{"id": "m", "text": event.Text, "block": true}},
		{"trace", traceEvent{MsgID: "m", Index: 1, Event: event}, map[string]any{"msg_id": "m", "index": 1, "event": event}},
		{"participant", participant, participant},
		{"discussion", nil, nil},
	} {
		ch := make(chan []byte, 1)
		rs := &roomState{room: &Room{ID: "r"}, subs: map[chan []byte]struct{}{ch: {}}}
		rs.broadcast(tc.kind, tc.new)
		want, err := json.Marshal(map[string]any{"type": tc.kind, "data": tc.old})
		if err != nil {
			t.Fatal(err)
		}
		if got := <-ch; !bytes.Equal(got, want) {
			t.Fatalf("%s wire format changed:\n%s\n%s", tc.kind, got, want)
		}
	}
}

func TestStreamPublishesParticipantTransitionsAndEveryTrace(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "stream", "codex")
	call := nextCall(t, ps[0])
	ch, unsub, err := h.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	events := []agent.Event{
		{Kind: agent.KindSession, Text: "session"},
		{Kind: agent.KindTextDelta, Text: "first"},
		{Kind: agent.KindTextDelta, Text: " second"},
		{Kind: agent.KindThinking, Text: "reasoning"},
		{Kind: agent.KindThinking, Text: "continued reasoning"},
		{Kind: agent.KindToolCall, Name: "read", ID: "tool"},
		{Kind: agent.KindToolResult, ID: "tool", Output: "full result"},
		{Kind: agent.KindSubagentStart, Parent: "child", Name: "review"},
		{Kind: agent.KindSubagentMeta, Parent: "child", Meta: map[string]any{"model": "m"}},
		{Kind: agent.KindSubagentMsg, Parent: "child", Text: "nested message"},
		{Kind: agent.KindSubagentEnd, Parent: "child", Output: "nested result"},
		{Kind: agent.KindTextDelta, Text: " final"},
		{Kind: agent.KindUsage, Meta: map[string]any{"input_tokens": 12.0, "output_tokens": 5.0, "context_tokens": 20.0}},
		{Kind: agent.KindStatus, Text: "status record"},
		{Kind: agent.KindError, Text: "visible record"},
	}
	var messageID string
	for i, event := range events {
		before, err := h.Snapshot(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		oldParticipant, _ := json.Marshal(before.Room.Participants[0])
		call.emit(event)
		after, err := h.Snapshot(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		newParticipant, _ := json.Marshal(after.Room.Participants[0])
		participantCount, traceCount := 0, 0
		for len(ch) > 0 {
			var got struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(<-ch, &got); err != nil {
				t.Fatal(err)
			}
			switch got.Type {
			case "trace":
				traceCount++
				var trace traceEvent
				if err := json.Unmarshal(got.Data, &trace); err != nil {
					t.Fatal(err)
				}
				if trace.Index != i+1 || !reflect.DeepEqual(trace.Event, event) {
					t.Fatalf("trace missing/reordered: %+v want %+v", trace, event)
				}
				messageID = trace.MsgID
			case "participant":
				participantCount++
				if traceCount != 1 || !bytes.Equal(got.Data, newParticipant) {
					t.Fatalf("participant emitted out of order or stale: %s", got.Data)
				}
			}
		}
		wantParticipant := 0
		if !bytes.Equal(oldParticipant, newParticipant) {
			wantParticipant = 1
		}
		if traceCount != 1 || participantCount != wantParticipant {
			t.Fatalf("event %s: traces=%d participants=%d want=%d", event.Kind, traceCount, participantCount, wantParticipant)
		}
	}
	saved, err := h.ReadTrace(r.ID, messageID)
	if err != nil || !reflect.DeepEqual(saved, events) {
		t.Fatalf("trace persistence changed: %v (%v)", saved, err)
	}
	finishCall(call, "first second final")
	waitIdle(t, h, r.ID)
}

func TestBroadcastSlowSubscriberClosesWithoutReorderingOtherClients(t *testing.T) {
	slow, healthy := make(chan []byte, 1), make(chan []byte, 1)
	rs := &roomState{room: &Room{ID: "r"}, subs: map[chan []byte]struct{}{slow: {}, healthy: {}}}
	rs.broadcast("delta", textDeltaEvent{ID: "m", Text: "first"})
	first := <-healthy
	rs.broadcast("delta", textDeltaEvent{ID: "m", Text: "second"})
	second := <-healthy
	if len(rs.subs) != 1 {
		t.Fatal("slow subscriber remained connected")
	}
	if got := <-slow; !bytes.Equal(got, first) {
		t.Fatal("queued event changed before disconnection")
	}
	if _, ok := <-slow; ok {
		t.Fatal("slow subscriber must reconnect after an event gap")
	}
	if bytes.Equal(first, second) || !bytes.Contains(second, []byte("second")) {
		t.Fatal("healthy subscriber lost ordered data")
	}
}

func TestSnapshotDetachesLiveTurnTiming(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "timing", "codex")
	call := nextCall(t, ps[0])
	snapshot, err := h.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	reply := snapshot.Messages[len(snapshot.Messages)-1]
	if reply.Timing == nil {
		t.Fatal("missing live turn timing")
	}
	want := *reply.Timing
	// Live emit/completion updates these fields while HTTP serializes the
	// detached snapshot after releasing h.mu. The snapshot must remain stable.
	h.mu.Lock()
	for _, run := range h.rooms[r.ID].running {
		run.msg.Timing.FirstOutputMs = 123
		run.msg.Timing.TotalMs = 456
	}
	h.mu.Unlock()
	if *reply.Timing != want {
		t.Fatalf("live turn mutated an earlier snapshot: %+v, want %+v", reply.Timing, want)
	}
	finishCall(call, "done")
	waitIdle(t, h, r.ID)
}
