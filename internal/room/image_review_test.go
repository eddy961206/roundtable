package room

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestQ1ReviewSignalBlock(t *testing.T) {
	for _, tail := range []string{
		"[[REQUEST_DONE:r1]]\n[[REQUEST_REJECT:r2]] unavailable\nWORK:DONE\n[[DISCUSSION:AGREE]]",
		"[[DISCUSSION:AGREE]]\nWORK:DONE\n[[REQUEST_REJECT:r2]] unavailable\n[[REQUEST_DONE:r1]]",
	} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		for _, id := range []string{"r1", "r2"} {
			if err := h.saveRequest(rs, Request{ID: id, To: "codex", Status: "in_progress"}); err != nil {
				t.Fatal(err)
			}
		}
		raw := "answer\n" + tail
		msg := rs.nextMessage("agent", "codex", raw)
		pr := &prepared{t: &Turn{}, p: r.Participants[0], msg: msg, requests: []string{"r1", "r2"}}
		if err := h.finishMarkers(rs, pr, raw); err != nil {
			t.Fatal(err)
		}
		if rs.request("r1").Status != "done" || rs.request("r2").Status != "rejected" {
			t.Errorf("not all terminal signals applied: %+v", rs.requests)
		}
		clean, sig := ParseSignal(raw)
		if sig != "AGREE" || strings.Contains(clean, "[[DISCUSSION:") {
			t.Errorf("discussion signal lost: %q %q", clean, sig)
		}
		clean, done := ParseWorkDone(raw)
		if !done || strings.Contains(clean, "WORK:DONE") {
			t.Errorf("work signal lost: %q %v", clean, done)
		}
	}
}

func TestQ1ReviewNoticeRetryKeepsImageCap(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	postQueueMessage(t, h, r.ID, "next before the image", "codex")
	a, err := h.UploadAttachment(r.ID, "later.png", bytes.NewReader(pngImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(r.ID, "later image", "codex", a.ID); err != nil {
		t.Fatal(err)
	}
	first.result <- controlledResult{text: "[[REQUEST_DONE:notice-cap-test]]"}
	second := nextCall(t, ps[0])
	if !strings.Contains(second.opts.Prompt, "마커 거절") || len(second.opts.Images) != 0 {
		t.Fatal("notice advanced image cap", second.opts)
	}
	second.result <- controlledResult{err: errors.New("provider failed")}
	third := nextCall(t, ps[0])
	if !strings.Contains(third.opts.Prompt, "마커 거절") || len(third.opts.Images) != 1 {
		t.Fatal("notice not retried with next input", third.opts)
	}
	third.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "after success", "codex")
	fourth := nextCall(t, ps[0])
	if strings.Contains(fourth.opts.Prompt, "마커 거절") || len(fourth.opts.Images) != 0 {
		t.Fatal("successful delivery repeated", fourth.opts)
	}
	fourth.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
}

func TestQ1ReviewNoticeReplayAndOtherSeats(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
	rs := h.rooms[r.ID]
	pr := &prepared{p: r.Participants[0], t: &Turn{}}
	if err := h.markerError(rs, pr, "[[REQUEST broken]]", "fix this marker"); err != nil {
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
	other, _, _ := rs.deltaFor(rs.room.participant("claude"), true, rs.room.Seq)
	if len(other) != 1 || !strings.Contains(other[0].Text, "fix this marker") {
		t.Fatal("other seats cannot see the notice", other)
	}
	pr = h.buildTurn(rs, &Turn{ID: newID(), Agent: "codex", Reason: "mention", Cap: 0})
	if pr == nil {
		t.Fatal("pending notice lost at restart")
	}
	rs.addTurnNotices(pr)
	if !strings.Contains(pr.opts.Prompt, "fix this marker") {
		t.Fatal(pr.opts.Prompt)
	}
	if err := h.acknowledgeNotices(rs, pr); err != nil {
		t.Fatal(err)
	}
	h, err = NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	rs = h.rooms[r.ID]
	if len(rs.pendingNotices("codex", "")) != 0 {
		t.Fatal("notice receipt lost at restart")
	}
	hist, _, _ := rs.deltaFor(rs.room.participant("codex"), true, rs.room.Seq)
	if len(hist) != 0 {
		t.Fatal("notice repeated through normal history", hist)
	}
}

func TestQ1ReviewDiscussionCompletesSeveralRequests(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	h.mu.Lock()
	rs := h.rooms[r.ID]
	source := rs.nextMessage("agent", "claude", "two pending tasks")
	for _, id := range []string{"r1", "r2"} {
		source.Requests = append(source.Requests, id)
		if err := h.saveRequest(rs, Request{ID: id, From: "claude", To: "codex", Text: "task " + id, SourceMsg: source.ID, Status: "pending"}); err != nil {
			h.mu.Unlock()
			t.Fatal(err)
		}
	}
	if err := h.store.AppendMessage(r.ID, source); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()
	if err := h.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "[요청 r1") || !strings.Contains(call.opts.Prompt, "[요청 r2") {
		t.Fatal(call.opts.Prompt)
	}
	call.result <- controlledResult{text: "handled both\n[[REQUEST_DONE:r1]]\n[[DISCUSSION:AGREE]]\n[[REQUEST_REJECT:r2]] cannot do this"}
	other := nextCall(t, ps[1])
	other.result <- controlledResult{text: "[[DISCUSSION:AGREE]]"}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if snap.Requests[0].Status != "done" || snap.Requests[1].Status != "rejected" {
		t.Fatal(snap.Requests)
	}
	for _, m := range snap.Messages {
		if m.Author == "codex" && m.Kind == "agent" && m.Signal != "AGREE" {
			t.Fatal("discussion signal lost", m)
		}
	}
}

func TestQ1ReviewPartRequestIsolation(t *testing.T) {
	for _, state := range []string{"working", "fixing"} {
		for _, customPrompt := range []bool{false, true} {
			h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
			rs := h.rooms[r.ID]
			p := r.Participants[0]
			source := rs.nextMessage("agent", "claude", "[[REQUEST:@codex]] OLD-ROOM-REQUEST")
			source.Requests = []string{"r1"}
			if err := h.saveRequest(rs, Request{ID: "r1", To: p.ID, Status: "pending", SourceMsg: source.ID, Text: "OLD-ROOM-REQUEST"}); err != nil {
				t.Fatal(err)
			}
			part := &Part{ID: "p1", Seat: p.ID, State: state, SessionID: "part-session", Worktree: t.TempDir(), LastSeen: source.Seq}
			r.Work = &WorkPlan{Parts: []*Part{part}}
			next := rs.nextMessage("user", "user", "part-only task")
			turn := &Turn{ID: newID(), Agent: p.ID, Reason: "mention", Cap: next.Seq}
			if customPrompt {
				turn.prompt = "fix only the accepted review findings"
			}
			pr := h.buildTurn(rs, turn)
			if pr == nil {
				t.Fatal("missing part turn")
			}
			h.beginRequests(rs, pr)
			if rs.request("r1").Status != "pending" || len(pr.requests) != 0 || strings.Contains(pr.opts.Prompt, "OLD-ROOM-REQUEST") {
				t.Errorf("%s custom=%v: room request leaked into part: %s", state, customPrompt, pr.opts.Prompt)
			}
		}
	}
}

func TestQ1ReviewNoticeCrossesQueuedCap(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	queued := postQueueMessage(t, h, r.ID, "already queued", "codex")
	first.result <- controlledResult{text: "[[REQUEST_DONE:unknown-review-id]]"}
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "마커 거절") || !strings.Contains(next.opts.Prompt, "unknown-review-id") {
		t.Errorf("notice above cap=%d missing from next prompt: %s", queued.Seq, next.opts.Prompt)
	}
	next.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "one more", "codex")
	third := nextCall(t, ps[0])
	if strings.Contains(third.opts.Prompt, "마커 거절") {
		t.Error("notice redelivered")
	}
	third.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
}

func TestQ1ReviewRequestRejection(t *testing.T) {
	for _, tc := range []struct{ reason, text string }{
		{"mention", "[[REQUEST @claude]] missing colon"},
		{"mention", "[[REQUEST]] missing target"},
		{"discussion", "[[REQUEST:@claude]] forbidden"},
		{"review", "[[REQUEST:@claude]] forbidden"},
	} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		msg := rs.nextMessage("agent", "codex", tc.text)
		pr := &prepared{t: &Turn{Reason: tc.reason}, p: r.Participants[0], msg: msg, opts: agent.RunOptions{Mode: agent.ModeChat}}
		if err := h.finishMarkers(rs, pr, tc.text); err != nil {
			t.Fatal(err)
		}
		if len(rs.requests) != 0 {
			t.Errorf("%s created a request", tc.reason)
		}
		found := false
		for _, m := range rs.msgs {
			if m.Kind == "system" && strings.Contains(m.Text, "마커 거절") {
				found = true
				if m.Target != "" {
					t.Error("feedback hidden from other seats")
				}
			}
		}
		if !found {
			t.Errorf("silently discarded: %s %s", tc.reason, tc.text)
		}
	}
}

func TestQ1ReviewStaleQueueDoesNotStartPendingRequest(t *testing.T) {
	for _, inPart := range []bool{false, true} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		p := r.Participants[0]
		source := rs.nextMessage("agent", "claude", "request already delivered")
		source.Requests = []string{"r1"}
		if err := h.saveRequest(rs, Request{ID: "r1", To: p.ID, Status: "pending", SourceMsg: source.ID}); err != nil {
			t.Fatal(err)
		}
		user := rs.nextMessage("user", "user", "priority turn already delivered")
		p.SessionID, p.LastSeen = "room-session", user.Seq
		if inPart {
			r.Work = &WorkPlan{Parts: []*Part{{ID: "p1", Seat: p.ID, State: "working", SessionID: "part-session", LastSeen: user.Seq, Worktree: t.TempDir()}}}
		}
		if pr := h.buildTurn(rs, &Turn{ID: newID(), Agent: p.ID, Reason: "mention", Cap: source.Seq}); pr != nil {
			t.Errorf("part=%v stale queue started an empty turn: %s", inPart, pr.opts.Prompt)
		}
	}
}
