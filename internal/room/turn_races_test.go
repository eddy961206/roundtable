package room

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

func TestFailedPartBootstrapKeepsKickoffTurn(t *testing.T) {
	repo := newGitRepo(t)
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claude := &controlledProvider{fakeProvider: fakeProvider{id: "claude"}, callsCh: make(chan *controlledCall, 16)}
	codex := &controlledProvider{fakeProvider: fakeProvider{id: "codex"}, callsCh: make(chan *controlledCall, 16)}
	hub, err := NewHub(store, agent.NewRegistry(claude, codex), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })

	r, err := hub.CreateRoom(RoomSpec{Title: "work", Mode: agent.ModeWrite, Workspace: repo, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}

	postQueueMessage(t, hub, r.ID, "INITIAL_CHAT", "claude")
	running := nextCall(t, claude)

	if _, err := hub.CreateWork(r.ID, workSpec(1, nil)); err != nil {
		t.Fatal(err)
	}

	m1 := postQueueMessage(t, hub, r.ID, "M1_TARGET_MESSAGE", "claude")

	finishCall(running, "initial done")
	bootstrap := nextCall(t, claude)
	if strings.Contains(bootstrap.opts.Prompt, "M1_TARGET_MESSAGE") {
		t.Fatalf("bootstrap turn unexpectedly included m1: %s", bootstrap.opts.Prompt)
	}

	bootstrap.emit(agent.Event{Kind: agent.KindSession, Text: "part-session"})
	bootstrap.result <- controlledResult{err: errors.New("bootstrap failed")}

	// The failed bootstrap left the part unstarted, so its kickoff turn runs as
	// the bootstrap, and m1 stays unseen for the seat's next turn as after any
	// failed turn.
	var kickoff *controlledCall
	select {
	case kickoff = <-claude.callsCh:
	case <-time.After(3 * time.Second):
		snap, _ := hub.Snapshot(r.ID)
		t.Fatalf("the part's kickoff turn did not run after the failed bootstrap (part session=%q, queue=%d)", snap.Work.part("p1").SessionID, len(snap.Queue))
	}
	if !strings.Contains(kickoff.opts.Prompt, "목표") || kickoff.opts.SessionID != "" {
		t.Fatalf("kickoff is not a fresh bootstrap: session=%q prompt=%s", kickoff.opts.SessionID, kickoff.opts.Prompt)
	}
	finishCall(kickoff, "plan read")
	waitIdle(t, hub, r.ID)

	snap, err := hub.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	part := snap.Work.part("p1")
	if part.SessionID != "claude-session" || part.LastSeen >= m1.Seq {
		t.Fatalf("want the kickoff's session and m1 (#%d) still unseen: session=%q last_seen=%d", m1.Seq, part.SessionID, part.LastSeen)
	}
}

// A turn that took a request and failed after output puts the request back to
// pending, and the request's own queued turn has nothing new to read. That
// turn is dropped (6215715: no empty CLI turn for a pending request alone), so
// the request rides the seat's next turn.
func TestPendingRequestRidesTheNextTurn(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	quiet := true // a done request does not call codex back
	if _, err := h.UpdateRoom(r.ID, RoomPatch{QuietDone: &quiet}); err != nil {
		t.Fatal(err)
	}

	postQueueMessage(t, h, r.ID, "start both", "")
	callA := nextCall(t, ps[0])
	callB := nextCall(t, ps[1])

	finishCall(callA, "[[REQUEST:@claude]] 요청 작업 확인")
	queueSnapshot(t, h, r.ID, func(s *Snapshot) bool {
		return len(s.Requests) == 1 && s.Requests[0].Status == "pending"
	})

	postQueueMessage(t, h, r.ID, "PRIORITY_FOR_CLAUDE", "claude")

	finishCall(callB, "initial B done")
	prio := nextCall(t, ps[1])
	prio.result <- controlledResult{text: "partial answer before failure", err: errors.New("stream idle timeout")}

	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	id := snap.Requests[0].ID
	if snap.Requests[0].Status != "pending" || len(snap.Queue) != 0 {
		t.Fatalf("want request %s pending and no empty turn queued: status=%s queue=%d", id, snap.Requests[0].Status, len(snap.Queue))
	}
	postQueueMessage(t, h, r.ID, "NEXT_FOR_CLAUDE", "claude")
	req := nextCall(t, ps[1])
	if !strings.Contains(req.opts.Prompt, "[요청 "+id) {
		t.Fatalf("the turn does not carry request %s: %s", id, req.opts.Prompt)
	}
	finishCall(req, "확인함\n[[REQUEST_DONE:"+id+"]]")
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if snap.Requests[0].Status != "done" {
		t.Fatalf("request %s is %s after the turn that carried it", id, snap.Requests[0].Status)
	}
}

func TestHandoffRefusedWhileSpeechHeld(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")

	postQueueMessage(t, h, r.ID, "initial work", "codex")
	finishCall(nextCall(t, ps[0]), "initial done")
	waitIdle(t, h, r.ID)

	paused := true
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AgentsPaused: &paused}); err != nil {
		t.Fatal(err)
	}

	// pump would drop the handoff turn, and the request lives only in that turn.
	_, err := h.Handoff(r.ID, "codex", "", false)
	if err == nil || !strings.Contains(err.Error(), "발화") {
		t.Fatalf("want the handoff refused for the paused room, got %v", err)
	}
	snap, _ := h.Snapshot(r.ID)
	if len(snap.Queue) != 0 || snap.Messages[len(snap.Messages)-1].Kind == "system" {
		t.Fatalf("a refused handoff left a turn or a notice: queue=%d last=%+v", len(snap.Queue), snap.Messages[len(snap.Messages)-1])
	}
}
