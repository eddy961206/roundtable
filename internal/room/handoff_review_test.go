package room

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// Counterexamples from the Argon review of ac5988b..11effa1 (room f83ad4e3c6d6 #2).

// withSession gives seat ps[i] a resumable session.
func withSession(t *testing.T, h *Hub, room string, p *controlledProvider, seat string) {
	t.Helper()
	postQueueMessage(t, h, room, "준비 "+seat, seat)
	finishCall(nextCall(t, p), "R0")
	waitIdle(t, h, room)
}

func waitSeat(t *testing.T, h *Hub, room, seat string, ok func(Participant) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok(seatOf(t, h, room, seat)) {
		if time.Now().After(deadline) {
			t.Fatalf("seat %s: %+v", seat, seatOf(t, h, room, seat))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func handedOff(t *testing.T, h *Hub, r *Room, p *controlledProvider, seat, summary string) {
	t.Helper()
	if _, err := h.Handoff(r.ID, seat, "", false); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, p), summary)
	waitSeat(t, h, r.ID, seat, func(s Participant) bool { return s.HistoryFrom > 0 })
}

// 1: another seat's reply still streaming at the switch reaches the new session,
// and a turn it chains (cap below the summary) still runs with the summary.
func TestHandoffKeepsReplyStreamingAtSwitch(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	withSession(t, h, r.ID, ps[0], "codex")
	postQueueMessage(t, h, r.ID, "오래 걸리는 일", "claude")
	slow := nextCall(t, ps[1])
	handedOff(t, h, r, ps[0], "codex", "SUMMARY")
	finishCall(slow, "CLAUDE-ANSWER @codex 확인해")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "" || !strings.Contains(next.opts.Prompt, "CLAUDE-ANSWER") || !strings.Contains(next.opts.Prompt, "SUMMARY") {
		t.Fatalf("%q\n%s", next.opts.SessionID, next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}

// 2: the first turn after a handoff reports a session and fails before any
// output; the next turn still starts fresh from the summary.
func TestHandoffSurvivesFirstTurnFailingAfterSession(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	handedOff(t, h, r, ps[0], "codex", "SUMMARY")
	postQueueMessage(t, h, r.ID, "첫 일", "codex")
	c := nextCall(t, ps[0])
	c.emit(agent.Event{Kind: agent.KindSession, Text: "partial"})
	c.result <- controlledResult{err: errors.New("boom")}
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "둘째 일", "codex")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "" || !strings.Contains(next.opts.Prompt, "SUMMARY") || !strings.Contains(next.opts.Prompt, "인계 요약(#") {
		t.Fatalf("%q\n%s", next.opts.SessionID, next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
	if s := seatOf(t, h, r.ID, "codex"); s.HistoryFrom != 0 || s.SessionID != "codex-session" {
		t.Fatalf("a delivered turn ends the handoff: %+v", s)
	}
}

// 3: any unfinished part owns the seat's context.
func TestHandoffRefusesSeatWithUnfinishedPart(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	h.mu.Lock()
	h.rooms[r.ID].room.Work = &WorkPlan{Parts: []*Part{{ID: "p1", Seat: "codex", State: "reviewing"}}}
	h.mu.Unlock()
	_, err := h.Handoff(r.ID, "codex", "", false)
	h.mu.Lock()
	h.rooms[r.ID].room.Work = nil
	h.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "파트") {
		t.Fatalf("%v", err)
	}
}

// 4: a message sent during the handoff turn waits for the new session.
func TestHandoffTurnIsNotSteered(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	ps[0].steer = true
	withSession(t, h, r.ID, ps[0], "codex")
	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	hc := nextCall(t, ps[0])
	m, err := h.PostUser(r.ID, "후속 지시", "codex")
	if err != nil || m.Live {
		t.Fatalf("%v live=%v", err, m != nil && m.Live)
	}
	finishCall(hc, "SUMMARY")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "" || !strings.Contains(next.opts.Prompt, "후속 지시") {
		t.Fatalf("%q\n%s", next.opts.SessionID, next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}

// 5: the handoff turn takes no requests, runs no markers, offers no marker
// guidance, and its markers cannot be applied later.
func TestHandoffTurnLeavesRequestsAndMarkersAlone(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	withSession(t, h, r.ID, ps[0], "codex")
	img, err := h.UploadAttachment(r.ID, "image.png", bytes.NewReader(pngImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(r.ID, "@claude 이미지 봐", "", img.ID); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[1]), "봤어")
	waitIdle(t, h, r.ID)
	h.mu.Lock()
	rs := h.rooms[r.ID]
	if err := h.saveRequest(rs, Request{ID: "rq1", From: "claude", To: "codex", Text: "봐줘", SourceMsg: rs.msgs[0].ID, Status: "pending", At: nowMs()}); err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	h.mu.Unlock()

	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	hc := nextCall(t, ps[0])
	if strings.Contains(hc.opts.Prompt, "REQUEST_DONE:rq1") || strings.Contains(hc.opts.Prompt, "[[REQUEST:@좌석ID]]") {
		t.Fatalf("handoff prompt offers requests or markers:\n%s", hc.opts.Prompt)
	}
	hc.result <- controlledResult{text: "SUMMARY @claude\n[[DECISION: 인용 결정 | 근거: a.go:1 | 반대: 없음 | 미확인: 없음]]", err: errors.New("stream idle timeout")}
	waitIdle(t, h, r.ID)
	noMoreCalls(t, ps[1])
	snap, _ := h.Snapshot(r.ID)
	failed := agentMsg(t, snap, "codex", "error")
	if _, err := h.ApplyMarkers(r.ID, failed.Seq); err == nil {
		t.Fatal("a handoff summary's markers must not be applied")
	}
	if len(snap.Decisions) != 0 || len(snap.Requests) != 1 || snap.Requests[0].Status != "pending" {
		t.Fatalf("%+v %+v", snap.Decisions, snap.Requests)
	}

	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "SUMMARY\n[[DECISION: 인용 결정 | 근거: a.go:1 | 반대: 없음 | 미확인: 없음]]")
	waitIdle(t, h, r.ID)
	noMoreCalls(t, ps[1])
	if snap, _ := h.Snapshot(r.ID); len(snap.Decisions) != 0 {
		t.Fatalf("quoted decision became a card: %+v", snap.Decisions)
	}
}

// 6: a handoff that could not run now is refused, not silently dropped.
func TestHandoffRefusedDuringDiscussionOrBudgetHold(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	withSession(t, h, r.ID, ps[0], "codex")
	withSession(t, h, r.ID, ps[1], "claude")
	h.mu.Lock()
	h.rooms[r.ID].room.SeatInputBudget = 1
	h.rooms[r.ID].room.participant("codex").InputAll = 10
	h.mu.Unlock()
	if _, err := h.Handoff(r.ID, "codex", "", false); err == nil || !strings.Contains(err.Error(), "예산") {
		t.Fatalf("budget hold: %v", err)
	}
	h.mu.Lock()
	h.rooms[r.ID].room.SeatInputBudget = 0
	h.mu.Unlock()
	blind := false
	if err := h.StartDiscussion(r.ID, 1, &blind); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, ps[0])
	if _, err := h.Handoff(r.ID, "claude", "", false); err == nil || !strings.Contains(err.Error(), "토론") {
		t.Fatalf("discussion: %v", err)
	}
	finishCall(first, "a\n[[DISCUSSION:AGREE]]")
	finishCall(nextCall(t, ps[1]), "b\n[[DISCUSSION:AGREE]]")
	waitIdle(t, h, r.ID)
}

// 7a: after a failed handoff, continuing does not carry the handoff request.
func TestFailedHandoffLeavesNoRequestBehind(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{err: errors.New("boom")}
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "NEXT", "codex")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "codex-session" || strings.Contains(next.opts.Prompt, "인계 요약을 써") {
		t.Fatalf("%q\n%s", next.opts.SessionID, next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}

// 7b: a session reset during the handoff turn wins.
func TestResetDuringHandoffWins(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	if _, err := h.Handoff(r.ID, "codex", "", false); err != nil {
		t.Fatal(err)
	}
	hc := nextCall(t, ps[0])
	if err := h.ResetSessions(r.ID); err != nil {
		t.Fatal(err)
	}
	finishCall(hc, "SUMMARY")
	waitIdle(t, h, r.ID)
	if s := seatOf(t, h, r.ID, "codex"); s.HistoryFrom != 0 {
		t.Fatalf("reset promised the full history: %+v", s)
	}
}

// 8: a fork seat whose base room is gone can only hand off fresh.
func TestHandoffRefusesDeletedForkBase(t *testing.T) {
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
	if err := h.DeleteRoom(base.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Handoff(r.ID, "codex", "", false); err == nil || !strings.Contains(err.Error(), "fresh") {
		t.Fatalf("%v", err)
	}
	if _, err := h.Handoff(r.ID, "codex", "", true); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "SUMMARY")
	waitIdle(t, h, r.ID)
}

// 10: part sessions run the longest builds; they get the wait rule too.
func TestPartPromptCarriesWaitRule(t *testing.T) {
	plan := &WorkPlan{Goal: "g"}
	part := &Part{ID: "p1", Title: "t", Branch: "b"}
	if !strings.Contains(BuildPartPrompt(plan, part, "", "s", "codex"), "yield_time_ms") || strings.Contains(BuildPartPrompt(plan, part, "", "s", "claude"), "yield_time_ms") {
		t.Fatal("wait rule per provider")
	}
}

// 11: the summary survives the history trim of a long backlog.
func TestHandoffSummarySurvivesLongBacklog(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	handedOff(t, h, r, ps[0], "codex", "SUMMARY-LONG")
	h.mu.Lock()
	for i := 0; i < maxHistoryMessages+5; i++ {
		h.addSystem(h.rooms[r.ID], "잡담")
	}
	h.mu.Unlock()
	postQueueMessage(t, h, r.ID, "NEXT", "codex")
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "SUMMARY-LONG") {
		t.Fatalf("%s", next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}

// 6b: a seat with a waiting turn hands off only after that turn; the queued
// turn would otherwise run in whichever session is current.
func TestHandoffRefusedWithQueuedTurn(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	withSession(t, h, r.ID, ps[0], "codex")
	h.mu.Lock()
	h.rooms[r.ID].room.participant("codex").HoldUntil = time.Now().Add(time.Hour).UnixMilli()
	h.mu.Unlock()
	if _, err := h.PostUser(r.ID, "모두에게", ""); err != nil { // waits behind the hold
		t.Fatal(err)
	}
	if _, err := h.Handoff(r.ID, "codex", "", false); err == nil || !strings.Contains(err.Error(), "대기 중인 턴") {
		t.Fatalf("%v", err)
	}
	h.mu.Lock()
	h.rooms[r.ID].room.participant("codex").HoldUntil = 0
	h.pump(h.rooms[r.ID])
	h.mu.Unlock()
	finishCall(nextCall(t, ps[0]), "ok")
	waitIdle(t, h, r.ID)
}
