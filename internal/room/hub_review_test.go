package room

import (
	"strings"
	"testing"
	"time"
)

// Tests for the defects found by the cross-vendor review (Codex, 2026-10-16).

func TestMentionOfDisabledParticipantDoesNotBroadcast(t *testing.T) {
	a, b := &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	off := false
	hub.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: "codex", Enabled: &off}}})
	hub.PostUser(r.ID, "@codex 너만 봐줘", "")
	waitIdle(t, hub, r.ID)
	if a.calls() != 0 || b.calls() != 0 {
		t.Fatalf("nobody should run: claude=%d codex=%d", a.calls(), b.calls())
	}
	snap, _ := hub.Snapshot(r.ID)
	last := snap.Messages[len(snap.Messages)-1]
	if last.Kind != "system" || !strings.Contains(last.Text, "@codex 는 비활성 또는 삭제 상태라 아무도 답하지 않아") {
		t.Fatalf("expected a notice, got %+v", last)
	}
}

func TestInvalidTargetLeavesNoMessage(t *testing.T) {
	a := &fakeProvider{id: "claude"}
	hub, r := newTestHub(t, a)
	if _, err := hub.PostUser(r.ID, "누구?", "ghost"); err == nil {
		t.Fatal("expected error")
	}
	snap, _ := hub.Snapshot(r.ID)
	if len(snap.Messages) != 0 {
		t.Fatalf("rejected message must not be stored: %+v", snap.Messages)
	}
}

func TestCancelQueuedDiscussionTurnEndsRound(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 400 * time.Millisecond, reply: func(int, string) string { return "x\n[[DISCUSSION:AGREE]]" }}
	b := &fakeProvider{id: "codex", reply: func(int, string) string { return "y\n[[DISCUSSION:AGREE]]" }}
	hub, r := newTestHub(t, a, b)
	if err := hub.StartDiscussion(r.ID, 3); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	snap, _ := hub.Snapshot(r.ID)
	var queued string
	for _, q := range snap.Queue {
		if q.Status == "queued" {
			queued = q.ID
		}
	}
	if queued == "" {
		t.Fatal("codex turn should be waiting")
	}
	if err := hub.CancelTurn(r.ID, queued); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	snap, _ = hub.Snapshot(r.ID)
	if snap.Discussion != nil {
		t.Fatal("discussion must end when the last turn of the round was cancelled and the rest agreed")
	}
	if b.calls() != 0 {
		t.Fatal("cancelled turn must not run")
	}
}

func TestResetDuringRunDoesNotRestoreOldSession(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 300 * time.Millisecond}
	hub, r := newTestHub(t, a)
	hub.PostUser(r.ID, "첫", "")
	waitIdle(t, hub, r.ID)
	hub.PostUser(r.ID, "둘", "")
	time.Sleep(50 * time.Millisecond)
	if err := hub.ResetSessions(r.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	if sid := snap.Room.participant("claude").SessionID; sid != "" {
		t.Fatalf("session must stay empty after a reset issued mid-turn, got %q", sid)
	}
}

func TestUndeliveredSteerIsRedelivered(t *testing.T) {
	// claims it can steer but never drains the channel, so Result.Steered stays 0
	a := &fakeProvider{id: "claude", steer: true, delay: 300 * time.Millisecond}
	hub, r := newTestHub(t, a)
	hub.PostUser(r.ID, "시작", "")
	time.Sleep(50 * time.Millisecond)
	m, _ := hub.PostUser(r.ID, "조정", "claude")
	if !m.Live {
		t.Fatal("steer should be accepted live while running")
	}
	waitIdle(t, hub, r.ID)
	if a.calls() != 2 || !strings.Contains(a.prompts[1], "[User] 조정") {
		t.Fatalf("undelivered steer must immediately get a retry turn: %v", a.prompts)
	}
	hub.PostUser(r.ID, "셋째", "")
	waitIdle(t, hub, r.ID)
	last := a.prompts[len(a.prompts)-1]
	if strings.Contains(last, "[User] 조정") {
		t.Fatalf("retried steer must not be delivered again:\n%s", last)
	}
}

func TestDisablingParticipantDropsQueuedTurn(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 300 * time.Millisecond}
	b := &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	if err := hub.StartDiscussion(r.ID, 1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	off := false
	hub.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: "codex", Enabled: &off}}})
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	if b.calls() != 0 || snap.Discussion != nil {
		t.Fatalf("disabled participant ran (%d) or discussion stuck (%v)", b.calls(), snap.Discussion)
	}
}
