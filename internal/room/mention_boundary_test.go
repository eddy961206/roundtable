package room

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestMentionIdentityPrecedesLabels(t *testing.T) {
	ps := []*Participant{
		{ID: "codex-a", Provider: "codex", Label: "Astra", Enabled: true},
		{ID: "codex-b", Provider: "codex", Label: "Sol", Enabled: true},
		{ID: "claude-a", Provider: "claude", Label: "codex-a", Enabled: true},
		{ID: "claude-b", Provider: "claude", Label: "codex", Enabled: true},
	}
	for text, want := range map[string][]string{
		"@codex-a":  {"codex-a"},
		"@codex-a야": {"codex-a"},
		"@codex":    {"codex-a", "codex-b"},
		"@codex에게":  {"codex-a", "codex-b"},
	} {
		t.Run(text, func(t *testing.T) {
			if got := ParseParticipantMentions(text, ps); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %v want %v", got, want)
			}
		})
	}
}

func TestMentionLabelParticleBoundary(t *testing.T) {
	ps := []*Participant{{ID: "a", Label: "민수"}, {ID: "b", Label: "민수진"}}
	for _, suffix := range []string{"", "야", "아", "님", "에게", "한테", "께", "이", "가", "은", "는", "을", "를", "도", "의", "와", "과", "랑", "이랑"} {
		if got := ParseParticipantMentions("@민수진"+suffix, ps); !reflect.DeepEqual(got, []string{"b"}) {
			t.Errorf("suffix %q: got %v want [b]", suffix, got)
		}
	}
	for _, suffix := range []string{"진", "님들", "에게서", "です", "x"} {
		if got := ParseParticipantMentions("@민수"+suffix, ps[:1]); len(got) != 0 {
			t.Errorf("unlisted suffix %q called %v", suffix, got)
		}
	}
}

func TestMentionWhitespaceLabelPolicy(t *testing.T) {
	for _, label := range []string{"Astra Prime", "Astra\tPrime", "Astra\u3000Prime"} {
		rs := &roomState{room: &Room{Participants: []*Participant{{ID: "seat-a", Label: label, Enabled: true}}}}
		if got := ParseParticipantMentions("@"+label, rs.room.Participants); len(got) != 0 {
			t.Errorf("whitespace label called %v", got)
		}
		if p, _ := rs.requestTarget(label); p != nil {
			t.Errorf("whitespace request label resolved to %s", p.ID)
		}
	}
	brief := standingBrief(&Room{}, &Participant{ID: "seat-a"}, nil)
	if !strings.Contains(brief, "공백 없는 이름만 @이름으로 부를 수 있고, 확실하게는 @좌석ID") {
		t.Error("standing brief must explain whitespace and stable IDs")
	}
}

func TestRequestInactiveIDDoesNotResolveLabel(t *testing.T) {
	for _, removed := range []bool{false, true} {
		rs := &roomState{room: &Room{Participants: []*Participant{
			{ID: "seat-a", Enabled: removed, Removed: removed},
			{ID: "seat-b", Label: "seat-a", Enabled: true},
		}}}
		if p, why := rs.requestTarget("seat-a"); p != nil || why == "" {
			t.Errorf("removed=%v: ID resolved to %v, reason=%q", removed, p, why)
		}
	}
}

func TestMentionSnapshotSurvivesRenameAndCancel(t *testing.T) {
	for _, mentioned := range []bool{true, false} {
		t.Run(map[bool]string{true: "mentioned", false: "unresolved"}[mentioned], func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex", "claude")
			label := "Astra"
			if !mentioned {
				label = "Other"
			}
			h.mu.Lock()
			r.participant("codex").Label = label
			h.mu.Unlock()
			postQueueMessage(t, h, r.ID, "start together", "")
			busy, sender := nextCall(t, ps[0]), nextCall(t, ps[1])
			finishCall(sender, "@Astra also explain\n[[REQUEST:@codex]] inspect")
			snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return len(s.Requests) == 1 })
			req := snap.Requests[0]
			newLabel := "Renamed"
			if !mentioned {
				newLabel = "Astra"
			}
			if _, err := h.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: "codex", Label: &newLabel}}}); err != nil {
				t.Fatal(err)
			}
			if _, err := h.CancelRequest(r.ID, req.ID); err != nil {
				t.Fatal(err)
			}
			snap, _ = h.Snapshot(r.ID)
			queued := false
			for _, turn := range snap.Queue {
				queued = queued || turn.Group == req.SourceMsg && turn.Agent == "codex"
			}
			if queued != mentioned {
				t.Fatalf("after rename and cancel: queued=%v want %v", queued, mentioned)
			}
			rooms, history, _, err := h.store.LoadAll()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range history[r.ID] {
				if m.ID != req.SourceMsg {
					continue
				}
				found = true
				data, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(data, &fields); err != nil {
					t.Fatal(err)
				}
				want := `[]`
				if mentioned {
					want = `["codex"]`
				}
				if string(fields["mentions"]) != want {
					t.Fatalf("persisted mentions=%s want %s", fields["mentions"], want)
				}
				replay := &roomState{room: rooms[0]}
				h.chainMentions(replay, m, &Turn{})
				if (len(replay.queue) == 1) != mentioned {
					t.Fatalf("replayed chain reinterpreted renamed label: %+v", replay.queue)
				}
			}
			if !found {
				t.Fatal("request source was not persisted")
			}
			finishCall(busy, "initial answer")
			if mentioned {
				finishCall(nextCall(t, ps[0]), "ordinary mention answer")
			}
			waitIdle(t, h, r.ID)
		})
	}
}

func TestMentionLegacyMessageStillResolvesText(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"})
	r.participant("codex").Label = "Astra"
	var m Message
	if err := json.Unmarshal([]byte(`{"id":"old","kind":"agent","author":"claude","text":"@Astra explain"}`), &m); err != nil {
		t.Fatal(err)
	}
	rs := h.rooms[r.ID]
	h.chainMentions(rs, &m, &Turn{})
	if len(rs.queue) != 1 || rs.queue[0].Agent != "codex" {
		t.Fatalf("legacy message did not resolve text: %+v", rs.queue)
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"mentions"`) {
		t.Fatalf("legacy message unexpectedly acquired a snapshot: %s", data)
	}
}

func TestMentionUserSnapshotAndUnknownBroadcast(t *testing.T) {
	for _, known := range []bool{true, false} {
		h, r, ps := controlledHub(t, "codex", "claude")
		label := "Astra Prime"
		text := "@Astra Prime check"
		want := []string{}
		if known {
			label, text, want = "Astra", "@Astra check", []string{"codex"}
		}
		h.mu.Lock()
		r.participant("codex").Label = label
		h.mu.Unlock()
		m := postQueueMessage(t, h, r.ID, text, "")
		finishCall(nextCall(t, ps[0]), "answer")
		if !known {
			finishCall(nextCall(t, ps[1]), "broadcast answer")
		}
		waitIdle(t, h, r.ID)
		_, history, _, err := h.store.LoadAll()
		if err != nil {
			t.Fatal(err)
		}
		for _, stored := range history[r.ID] {
			if stored.ID == m.ID && (stored.Mentions == nil || !reflect.DeepEqual(*stored.Mentions, want)) {
				t.Fatalf("known=%v: stored user mentions=%v want %v", known, stored.Mentions, want)
			}
		}
	}
}

func TestMentionChainReservesAuthorAndInactiveIDs(t *testing.T) {
	for _, target := range []string{"claude", "codex"} {
		h, r := newTestHub(t, &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"}, &fakeProvider{id: "agy"})
		r.participant("codex").Enabled = false
		r.participant("agy").Label = target
		rs := h.rooms[r.ID]
		m := rs.nextMessage("agent", "claude", "@"+target+" check")
		h.chainMentions(rs, m, &Turn{})
		if len(rs.queue) != 0 {
			t.Fatalf("reserved ID %q fell through to a label: %+v", target, rs.queue)
		}
	}
}
