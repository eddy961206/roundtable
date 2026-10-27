package room

import (
	"errors"
	"strings"
	"testing"
)

func seatOf(t *testing.T, h *Hub, room, id string) Participant {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.rooms[room].room.participant(id)
}

// 2026-10-28 A/B (docs/토큰효율_측정_20261028.md): a 120k-context seat that
// wrote a handoff and moved to a new session used 43% less input on the next
// review+fix turns, with the same defects found and fixed. A handoff runs in
// the old session, then the seat's next turn starts a new session holding only
// the summary and later messages.
func TestHandoffStartsNewSessionFromSummary(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	postQueueMessage(t, h, r.ID, "OLD-HISTORY 긴 작업", "codex")
	finishCall(nextCall(t, ps[0]), "RESULT-1")
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "MIDDLE 두 번째", "codex")
	if c := nextCall(t, ps[0]); c.opts.SessionID != "codex-session" {
		t.Fatalf("second turn resumes: %q", c.opts.SessionID)
	} else {
		finishCall(c, "RESULT-2")
	}
	waitIdle(t, h, r.ID)

	req, err := h.Handoff(r.ID, "codex", "파트 2 결정도 넣어", false)
	if err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	if call.opts.SessionID != "codex-session" || !strings.Contains(call.opts.Prompt, "인계 요약") || !strings.Contains(call.opts.Prompt, "파트 2 결정도 넣어") {
		t.Fatalf("the handoff is written in the old session: %q\n%s", call.opts.SessionID, call.opts.Prompt)
	}
	finishCall(call, "SUMMARY 결정 X · @claude 확인 필요")
	waitIdle(t, h, r.ID)
	noMoreCalls(t, ps[1]) // a mention inside the summary does not start a turn

	p := seatOf(t, h, r.ID, "codex")
	if p.SessionID != "" || p.HistoryFrom <= req.Seq || p.ContextTokens != 0 {
		t.Fatalf("seat after handoff: %+v", p)
	}
	postQueueMessage(t, h, r.ID, "NEXT 다음 작업", "codex")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "" || next.opts.ForkSession != "" {
		t.Fatalf("new session expected: %+v", next.opts)
	}
	for _, want := range []string{"SUMMARY 결정 X", "NEXT 다음 작업", "인계 요약"} {
		if !strings.Contains(next.opts.Prompt, want) {
			t.Fatalf("new session misses %q:\n%s", want, next.opts.Prompt)
		}
	}
	for _, gone := range []string{"OLD-HISTORY", "RESULT-2", "MIDDLE"} {
		if strings.Contains(next.opts.Prompt, gone) {
			t.Fatalf("history before the handoff must stay out (%q):\n%s", gone, next.opts.Prompt)
		}
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}

func TestHandoffRefusalsAndFailedSummaryKeepSession(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.Handoff(r.ID, "codex", "", false); err == nil || !strings.Contains(err.Error(), "세션") {
		t.Fatalf("no session yet: %v", err)
	}
	postQueueMessage(t, h, r.ID, "작업", "codex")
	running := nextCall(t, ps[0])
	if _, err := h.Handoff(r.ID, "codex", "", false); err == nil || !strings.Contains(err.Error(), "실행 중") {
		t.Fatalf("running seat: %v", err)
	}
	if _, err := h.Handoff(r.ID, "codex", "두 줄\n메모", false); err == nil {
		t.Fatal("a note is one line")
	}
	finishCall(running, "done")
	waitIdle(t, h, r.ID)

	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{err: errors.New("stream idle timeout")}
	waitIdle(t, h, r.ID)
	if p := seatOf(t, h, r.ID, "codex"); p.SessionID != "codex-session" || p.HistoryFrom != 0 {
		t.Fatalf("a failed handoff keeps the session: %+v", p)
	}
	snap, _ := h.Snapshot(r.ID)
	if last := snap.Messages[len(snap.Messages)-1]; last.Kind != "system" || !strings.Contains(last.Text, "세션을 그대로") {
		t.Fatalf("%+v", last)
	}
}

// A seat started from a base session forks the base again after a handoff,
// so the common documents stay read; fresh drops the base too.
func TestHandoffReforksBaseUnlessFresh(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		h, r, ps := controlledHub(t, "codex")
		base, err := h.CreateRoom(RoomSpec{Title: "기반", Mode: r.Mode, Workspace: r.Workspace, Participants: []string{"codex"}})
		if err != nil {
			t.Fatal(err)
		}
		h.mu.Lock()
		h.rooms[r.ID].room.participant("codex").Fork = &ForkSource{Room: base.ID, Seat: "codex", Session: "base-s", Dir: sessionDir(r)}
		h.mu.Unlock()
		postQueueMessage(t, h, r.ID, "작업", "codex")
		finishCall(nextCall(t, ps[0]), "done")
		waitIdle(t, h, r.ID)
		if _, err := h.Handoff(r.ID, "codex", "", fresh); err != nil {
			t.Fatal(err)
		}
		finishCall(nextCall(t, ps[0]), "SUMMARY")
		waitIdle(t, h, r.ID)
		postQueueMessage(t, h, r.ID, "다음", "codex")
		next := nextCall(t, ps[0])
		if want := map[bool]string{false: "base-s", true: ""}[fresh]; next.opts.ForkSession != want {
			t.Fatalf("fresh=%v fork=%q", fresh, next.opts.ForkSession)
		}
		finishCall(next, "ok")
		waitIdle(t, h, r.ID)
	}
}
