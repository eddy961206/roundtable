package room

import (
	"strings"
	"testing"
)

// Cancelling one running turn keeps the seat on, so other seats can still
// [[REQUEST]] it; stopping the seat turns it off and such requests are refused.
func TestCancelTurnKeepsSeatRequestable(t *testing.T) {
	for _, stopSeat := range []bool{false, true} {
		h, r, ps := controlledHub(t, "codex", "claude")
		if _, err := h.PostUser(r.ID, "duplicate work", "claude"); err != nil {
			t.Fatal(err)
		}
		dup := nextCall(t, ps[1])
		snap, _ := h.Snapshot(r.ID)
		var turn string
		for _, q := range snap.Queue {
			if q.Agent == "claude" && q.Status == "running" {
				turn = q.ID
			}
		}
		if stopSeat {
			if _, err := h.StopParticipant(r.ID, "claude"); err != nil {
				t.Fatal(err)
			}
		} else if err := h.CancelTurn(r.ID, turn); err != nil {
			t.Fatal(err)
		}
		<-dup.ctx.Done()
		waitIdle(t, h, r.ID)
		if _, err := h.PostUser(r.ID, "ask claude", "codex"); err != nil {
			t.Fatal(err)
		}
		finishCall(nextCall(t, ps[0]), "[[REQUEST:@claude]] re-check")
		if !stopSeat {
			req := nextCall(t, ps[1]) // the request reaches the still-enabled seat
			if !strings.Contains(req.opts.Prompt, "re-check") {
				t.Fatalf("request not delivered: %s", req.opts.Prompt)
			}
			finishCall(req, "checked")
		}
		waitIdle(t, h, r.ID)
		snap, _ = h.Snapshot(r.ID)
		refused := systemSays(snap, "마커 거절")
		if stopSeat != refused {
			t.Fatalf("stopSeat=%v refused=%v requests=%+v", stopSeat, refused, snap.Requests)
		}
		if !stopSeat {
			if seat := snap.Room.participant("claude"); seat == nil || !seat.Enabled || len(snap.Requests) != 1 || !strings.Contains(snap.Requests[0].Text, "re-check") {
				t.Fatalf("cancel-turn must keep the seat requestable: %+v %+v", seat, snap.Requests)
			}
		}
	}
}
