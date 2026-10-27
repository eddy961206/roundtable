package room

import (
	"strings"
	"testing"
	"time"
)

// 2026-10-27 19:46: two Opus writer turns hit the fixed 45-minute limit and the
// room showed them as "cancelled · 중단됨", the same as a user stop. The limit is
// now a room setting and a turn that runs past it ends as a timeout error.
func TestTurnTimeoutIsAnErrorNotACancel(t *testing.T) {
	old := turnTimeoutUnit
	turnTimeoutUnit = 20 * time.Millisecond // 5 "minutes" = 100ms in this test
	defer func() { turnTimeoutUnit = old }()
	h, r, ps := controlledHub(t, "codex", "claude")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{TurnTimeoutMinutes: ref(5)}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(r.ID, "long work", "codex"); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]) // never answers: the room limit ends it
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	m := agentMsg(t, snap, "codex", "error")
	if m.ErrorKind != "turn_timeout" || !strings.Contains(m.Error, "턴 시간 상한 5분") {
		t.Fatalf("%q %q", m.ErrorKind, m.Error)
	}

	// A user stop stays a cancel.
	if _, err := h.PostUser(r.ID, "stop me", "claude"); err != nil {
		t.Fatal(err)
	}
	stopped := nextCall(t, ps[1])
	snap, _ = h.Snapshot(r.ID)
	for _, q := range snap.Queue {
		if q.Agent == "claude" && q.Status == "running" {
			if err := h.CancelTurn(r.ID, q.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	<-stopped.ctx.Done()
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if c := agentMsg(t, snap, "claude", "cancelled"); c.ErrorKind != "" || c.Error != "중단됨" {
		t.Fatalf("%+v", c)
	}
}

func TestTurnTimeoutSetting(t *testing.T) {
	h, r, _ := controlledHub(t, "codex")
	if got := r.turnTimeout(); got != 45*time.Minute {
		t.Fatalf("default %v", got)
	}
	for _, bad := range []int{4, 721, -1} {
		_, err := h.UpdateRoom(r.ID, RoomPatch{TurnTimeoutMinutes: ref(bad)})
		if err == nil || !strings.Contains(err.Error(), "5~720") {
			t.Errorf("%d: %v", bad, err)
		}
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{TurnTimeoutMinutes: ref(180)}); err != nil {
		t.Fatal(err)
	}
	if got := h.rooms[r.ID].room.turnTimeout(); got != 3*time.Hour {
		t.Fatal(got)
	}
	created, err := h.CreateRoom(RoomSpec{Title: "long", Mode: "chat", Participants: []string{"codex"}, TurnTimeoutMinutes: 90})
	if err != nil || created.TurnTimeoutMinutes != 90 {
		t.Fatalf("%v %+v", err, created)
	}
	if _, err := h.CreateRoom(RoomSpec{Title: "bad", Mode: "chat", Participants: []string{"codex"}, TurnTimeoutMinutes: 3}); err == nil {
		t.Fatal("create accepted 3 minutes")
	}
}
