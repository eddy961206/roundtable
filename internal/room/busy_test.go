package room

import "testing"

// 2026-11-04: while a part's submit check or its merge ran outside h.mu the
// room had no running or queued turn. The room list and the snapshot said
// idle, so rt wait returned before the review turn was queued, and restart's
// idle check did not wait for the merge.
func TestWorkStepKeepsRoomActive(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	rs := h.rooms[r.ID]
	check := func(want bool, what string) {
		t.Helper()
		h.mu.Lock()
		idle := h.idleLocked()
		h.mu.Unlock()
		listed := false
		for _, s := range h.Rooms() {
			if s.ID == r.ID {
				listed = s.Active
			}
		}
		snap, err := h.Snapshot(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if listed != want || snap.Active != want || idle == want {
			t.Fatalf("%s: room list active %v, snapshot active %v, hub idle %v; want active %v", what, listed, snap.Active, idle, want)
		}
	}
	check(false, "no work")
	h.mu.Lock()
	rs.workBusy["p1"] = true
	h.mu.Unlock()
	check(true, "submit check")
	h.mu.Lock()
	delete(rs.workBusy, "p1")
	rs.room.Work = &WorkPlan{Parts: []*Part{{ID: "p1", State: "merging"}}}
	h.mu.Unlock()
	check(true, "merge")
	h.mu.Lock()
	rs.room.Work.Parts[0].State = "merged"
	h.mu.Unlock()
	check(false, "merged")
}

// A work step that starts without a room event left the published room list
// at active false (the reply's event came first). When the step then failed
// back to idle the list did not change, so no SSE signal went out and an
// rt wait that had read the list during the step slept forever.
func TestWorkStepSignalsTheRoomList(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	rs := h.rooms[r.ID]
	feed, stop, err := h.SubscribeRooms()
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	drain := func() {
		for {
			select {
			case <-feed:
			default:
				return
			}
		}
	}
	signaled := func(what string) {
		t.Helper()
		select {
		case <-feed:
		default:
			t.Fatalf("%s: no room-list signal", what)
		}
	}
	h.mu.Lock()
	seat := rs.room.Participants[0].ID
	rs.room.Work = &WorkPlan{Parts: []*Part{{ID: "p1", Seat: seat, State: "working", Worktree: t.TempDir(), Branch: "rt/p1"}}}
	part := rs.room.Work.Parts[0]
	h.mu.Unlock()
	drain()
	h.mu.Lock()
	err = h.reserveSubmit(rs, part)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	signaled("submit check starts")
	if _, err := h.submitReserved(rs, part); err != nil {
		t.Fatal(err)
	}
	if part.State != "blocked" {
		t.Fatalf("want the check to fail without a git worktree: %+v", part)
	}
	signaled("submit check fails back to idle")
}
