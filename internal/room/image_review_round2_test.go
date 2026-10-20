package room

import (
	"fmt"
	"strings"
	"testing"
)

func TestQ1R2RejectRequestsToUnfinishedPart(t *testing.T) {
	for _, state := range []string{"working", "fixing", "reviewing", "queued", "awaiting_approval", "blocked", "merging"} {
		for _, mention := range []string{"", "@codex", "@all"} {
			t.Run(state+"/"+mention, func(t *testing.T) {
				h, r := newTestHub(t, &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"})
				rs := h.rooms[r.ID]
				part := &Part{ID: "p1", Seat: "codex", State: state, SessionID: "part-session", Worktree: t.TempDir()}
				r.Work = &WorkPlan{Parts: []*Part{part}}
				text := strings.TrimSpace(mention + "\n[[REQUEST:@codex]] inspect this")
				msg := rs.nextMessage("agent", "claude", text)
				turn := &Turn{ID: newID(), Agent: "claude", Reason: "mention", Cap: msg.Seq}
				pr := &prepared{p: r.participant("claude"), t: turn, msg: msg}
				if err := h.finishMarkers(rs, pr, text); err != nil {
					t.Fatal(err)
				}
				if len(rs.requests) != 0 || len(msg.Requests) != 0 {
					t.Errorf("request created for unfinished part: %+v", rs.requests)
				}
				h.chainMentions(rs, msg, turn)
				want := 0
				if mention != "" {
					want = 1
				}
				if len(rs.queue) != want {
					t.Errorf("ordinary mention routing changed: got %d want %d", len(rs.queue), want)
				}
				if mention != "" && (state == "working" || state == "fixing") && len(rs.queue) == 1 {
					partTurn := h.buildTurn(rs, rs.queue[0])
					if partTurn == nil || !strings.Contains(partTurn.opts.Prompt, mention) {
						t.Error("mixed ordinary mention disappeared")
					}
				}
				next := &prepared{p: r.participant("claude"), t: &Turn{Cap: msg.Seq}}
				rs.addTurnNotices(next)
				if !strings.Contains(next.opts.Prompt, "@codex은 파트 p1 작업 중이라 요청을 받을 수 없음") || !strings.Contains(next.opts.Prompt, "파트가 끝난 뒤 다시 요청하거나 사용자에게 알려") {
					t.Errorf("missing rejection in sender's next prompt: %s", next.opts.Prompt)
				}
			})
		}
	}
}

func TestQ1R2NoticeStaysInOriginSession(t *testing.T) {
	for _, origin := range []string{"", "p1"} {
		for _, other := range []struct {
			name, part          string
			reviewer, bootstrap bool
		}{
			{"reviewer", "p2", true, false},
			{"other-part", "p2", false, false},
			{"bootstrap", "p1", false, true},
			{"other-session", map[bool]string{true: "p1", false: ""}[origin == ""], false, false},
		} {
			t.Run(fmt.Sprintf("origin=%s/%s", origin, other.name), func(t *testing.T) {
				h, r := newTestHub(t, &fakeProvider{id: "codex"})
				rs := h.rooms[r.ID]
				p := r.Participants[0]
				created := &prepared{p: p, t: &Turn{Part: origin}, msg: &Message{ID: "source"}}
				if origin != "" {
					created.part = &Part{ID: origin}
				}
				if err := h.markerError(rs, created, "request", "[[REQUEST invalid]]", "ORIGIN-FEEDBACK"); err != nil {
					t.Fatal(err)
				}
				notice := rs.msgs[len(rs.msgs)-1]
				foreign := &prepared{p: p, t: &Turn{Part: other.part}, reviewer: other.reviewer, bootstrap: other.bootstrap}
				if other.part != "" {
					foreign.part = &Part{ID: other.part}
				}
				rs.addTurnNotices(foreign)
				if strings.Contains(foreign.opts.Prompt, "ORIGIN-FEEDBACK") {
					t.Error("notice leaked into a different or disposable session")
				}
				if err := h.acknowledgeNotices(rs, foreign); err != nil {
					t.Fatal(err)
				}
				if notice.NoticeDelivered {
					t.Error("foreign session consumed the receipt")
				}
				original := &prepared{p: p, t: &Turn{Part: origin}}
				if origin != "" {
					original.part = &Part{ID: origin}
				}
				rs.addTurnNotices(original)
				if !strings.Contains(original.opts.Prompt, "ORIGIN-FEEDBACK") {
					t.Error("origin session lost its notice")
				}
				if err := h.acknowledgeNotices(rs, original); err != nil {
					t.Fatal(err)
				}
				if !notice.NoticeDelivered {
					t.Error("origin session did not record successful delivery")
				}
			})
		}
	}
}

func TestQ1R2ReleasedPartAcceptsRequests(t *testing.T) {
	for _, state := range []string{"merged", "abandoned"} {
		h, r := newTestHub(t, &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"})
		rs := h.rooms[r.ID]
		r.Work = &WorkPlan{Parts: []*Part{{ID: "p1", Seat: "codex", State: state}}}
		msg := rs.nextMessage("agent", "claude", "[[REQUEST:@codex]] inspect")
		pr := &prepared{p: r.participant("claude"), t: &Turn{Reason: "mention"}, msg: msg}
		if err := h.finishMarkers(rs, pr, msg.Text); err != nil {
			t.Fatal(err)
		}
		if len(rs.requests) != 1 || rs.requests[0].Status != "pending" {
			t.Fatal("released seat cannot accept a request", rs.requests)
		}
	}
}

func TestQ1R2NoticeScopeSurvivesRestart(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	rs := h.rooms[r.ID]
	if err := h.markerError(rs, &prepared{p: r.Participants[0], t: &Turn{Part: "p1"}, part: &Part{ID: "p1"}, msg: &Message{ID: "source"}}, "request", "[[REQUEST invalid]]", "PART-ONLY-NOTICE"); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveRoom(r); err != nil {
		t.Fatal(err)
	}
	h, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs = h.rooms[r.ID]
	p := rs.room.Participants[0]
	p.SessionID = "room-session"
	p.LastSeen = rs.room.Seq
	if pr := h.buildTurn(rs, &Turn{Agent: p.ID, Reason: "mention", Cap: rs.room.Seq}); pr != nil {
		t.Fatal("part notice started a room turn")
	}
	for _, partID := range []string{"", "p2", "p1"} {
		pr := &prepared{p: p, t: &Turn{Part: partID}}
		if partID != "" {
			pr.part = &Part{ID: partID}
		}
		rs.addTurnNotices(pr)
		if strings.Contains(pr.opts.Prompt, "PART-ONLY-NOTICE") != (partID == "p1") {
			t.Fatalf("notice routed to %q after restart", partID)
		}
		if err := h.acknowledgeNotices(rs, pr); err != nil {
			t.Fatal(err)
		}
	}
	h, err = NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.rooms[r.ID].pendingNotices("codex", "p1")) != 0 {
		t.Fatal("scoped delivery receipt was lost")
	}
}

func TestQ1R2NoticeOriginUsesActualSession(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	rs := h.rooms[r.ID]
	// A queued turn can retain its former part tag after that part finishes.
	// The prepared invocation now runs in the room session (part == nil).
	pr := &prepared{p: r.Participants[0], t: &Turn{Part: "finished-part"}, msg: &Message{ID: "source"}}
	if err := h.markerError(rs, pr, "request", "[[REQUEST invalid]]", "ROOM-NOTICE"); err != nil {
		t.Fatal(err)
	}
	rs.addTurnNotices(pr)
	if !strings.Contains(pr.opts.Prompt, "ROOM-NOTICE") {
		t.Fatal("stale queue tag misclassified a room notice")
	}
}
