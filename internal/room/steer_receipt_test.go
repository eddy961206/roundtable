package room

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// A channel enqueue is not a provider receipt. The old hub immediately set
// Live and the UI called this "즉시 주입됨", even if Run never read the input.
func TestSteerIsNotDeliveredBeforeReceipt(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	ps[0].steer = true
	postQueueMessage(t, h, r.ID, "start", "codex")
	nextCall(t, ps[0])
	m := postQueueMessage(t, h, r.ID, "correction waiting in channel", "codex")
	if m.Live {
		t.Fatal("input is displayed as delivered before the provider has read or acknowledged it")
	}
}

func TestRemoteWorkerKeepsNextTurnDelivery(t *testing.T) {
	h, r, token := remoteHub(t)
	seat := r.Participants[0].ID
	h.mu.Lock()
	h.rooms[r.ID].room.participant(seat).RemoteSeenAt = nowMs()
	h.mu.Unlock()
	postQueueMessage(t, h, r.ID, "REMOTE_FIRST", seat)
	first, err := h.RemoteNext(context.Background(), token, "worker", 3*time.Second)
	if err != nil || first == nil {
		t.Fatalf("first job: %+v %v", first, err)
	}
	m := postQueueMessage(t, h, r.ID, "REMOTE_NEXT_INPUT", seat)
	if m.Live || m.Delivery == nil || m.Delivery.State != deliveryQueued {
		t.Fatalf("remote input: %+v", m)
	}
	if err := h.RemoteFinish(token, "worker", first.Turn, RemoteResult{Text: "first remote answer", SessionID: "remote-session"}); err != nil {
		t.Fatal(err)
	}
	second, err := h.RemoteNext(context.Background(), token, "worker", 3*time.Second)
	if err != nil || second == nil || second.Turn == first.Turn || strings.Count(second.Prompt, "REMOTE_NEXT_INPUT") != 1 {
		t.Fatalf("second job: %+v %v", second, err)
	}
	if err := h.RemoteFinish(token, "worker", second.Turn, RemoteResult{Text: "second remote answer", SessionID: "remote-session"}); err != nil {
		t.Fatal(err)
	}
	queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return !s.Active })
	if extra, err := h.RemoteNext(context.Background(), token, "worker", 0); err != nil || extra != nil {
		t.Fatalf("duplicate remote job: %+v %v", extra, err)
	}
}

type recoveryProvider struct {
	*controlledProvider
	resolved string
}

func (p *recoveryProvider) ResolveSteer(_ context.Context, r agent.SteerReceipt) (agent.SteerReceipt, error) {
	if p.resolved == "error" {
		return r, errors.New("history unavailable")
	}
	r.State, r.Source = p.resolved, "test_history"
	return r, nil
}

func TestSteerRecoveryDoesNotLeaveSendingOrReplayAcceptedInput(t *testing.T) {
	for _, tc := range []struct {
		name, state, result string
		halted, replay      bool
	}{
		{"not written", deliveryPending, "", false, true},
		{"written and received", agent.InputSending, agent.InputAccepted, false, false},
		{"written but absent", agent.InputSending, agent.InputRejected, false, true},
		{"history unavailable", agent.InputUncertain, "error", false, false},
		{"explicit stop", deliveryPending, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := &recoveryProvider{controlledProvider: &controlledProvider{fakeProvider: fakeProvider{id: "codex", steer: true}, callsCh: make(chan *controlledCall, 8)}, resolved: tc.result}
			reg := agent.NewRegistry(p)
			initial, err := NewHub(store, reg, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			r, err := initial.CreateRoom(RoomSpec{Title: "recovery", Mode: agent.ModeChat, Participants: []string{"codex"}})
			if err != nil {
				t.Fatal(err)
			}
			seat := r.Participants[0].ID
			initial.mu.Lock()
			rs := initial.rooms[r.ID]
			first := rs.nextMessage("user", "user", "original")
			_ = store.AppendMessage(r.ID, first)
			reply := rs.nextMessage("agent", seat, "")
			reply.Status, reply.TurnID, reply.SessionID = "streaming", "old-turn", "old-session"
			_ = store.AppendMessage(r.ID, reply)
			m := rs.nextMessage("user", "user", "RECOVER_INPUT")
			m.Target = seat
			m.Delivery = &InputDelivery{State: tc.state, TurnID: reply.TurnID, SessionID: reply.SessionID, ProviderTurnID: "provider-turn", ClientID: m.ID, Halted: tc.halted}
			_ = store.AppendMessage(r.ID, m)
			rs.room.participant(seat).LastSeen, rs.room.participant(seat).SessionID = first.Seq, reply.SessionID
			if err := store.SaveRoom(rs.room); err != nil {
				t.Fatal(err)
			}
			initial.mu.Unlock()
			restored, err := NewHub(store, reg, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			if tc.replay {
				call := nextCall(t, p.controlledProvider)
				if strings.Count(call.opts.Prompt, "RECOVER_INPUT") != 1 {
					t.Fatal(call.opts.Prompt)
				}
				finishCall(call, "recovered answer")
			}
			s := queueSnapshot(t, restored, r.ID, func(s *Snapshot) bool { return !s.Active })
			for _, saved := range s.Messages {
				if saved.ID != m.ID {
					continue
				}
				if saved.Delivery.State == deliveryPending || saved.Delivery.State == agent.InputSending || saved.Delivery.State == agent.InputUncertain {
					t.Fatalf("stuck delivery: %+v", saved)
				}
				if tc.result == agent.InputAccepted && !saved.Live {
					t.Fatalf("accepted receipt lost: %+v", saved)
				}
				if tc.result == "error" && saved.Delivery.State != deliveryFailed {
					t.Fatalf("unknown receipt hidden: %+v", saved)
				}
			}
			if len(p.callsCh) != 0 {
				t.Fatal("recovery replayed an accepted or stopped input")
			}
		})
	}
}

func TestSteerReceiptsIdentifyTheMessageNotASuccessCount(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	ps[0].steer = true
	postQueueMessage(t, h, r.ID, "start", "codex")
	first := nextCall(t, ps[0])
	a := postQueueMessage(t, h, r.ID, "FIRST_NOT_ACCEPTED", "codex")
	b := postQueueMessage(t, h, r.ID, "SECOND_ACCEPTED", "codex")
	first.result <- controlledResult{text: "first answer", receipts: receivedInput(b.ID)}
	next := nextCall(t, ps[0])
	if strings.Count(next.opts.Prompt, "FIRST_NOT_ACCEPTED") != 1 || strings.Contains(next.opts.Prompt, "SECOND_ACCEPTED") {
		t.Fatalf("wrong retry: %s", next.opts.Prompt)
	}
	finishCall(next, "retry answer")
	s := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return !s.Active })
	for _, m := range s.Messages {
		if m.ID == a.ID && (m.Live || m.Delivery.State != deliveryDelivered) {
			t.Fatalf("first input: %+v", m)
		}
		if m.ID == b.ID && (!m.Live || m.Delivery.State != agent.InputAccepted) {
			t.Fatalf("second input: %+v", m)
		}
	}
	if len(ps[0].callsCh) != 0 {
		t.Fatal("input was delivered more than once")
	}
}

func TestAcceptedSteerIsNotReplayedWhenTheTurnLaterFails(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	ps[0].steer = true
	postQueueMessage(t, h, r.ID, "start", "codex")
	first := nextCall(t, ps[0])
	m := postQueueMessage(t, h, r.ID, "ALREADY_RECEIVED", "codex")
	first.result <- controlledResult{text: "partial answer", err: errors.New("later tool failed"), receipts: receivedInput(m.ID)}
	queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return !s.Active })
	if len(ps[0].callsCh) != 0 {
		t.Fatal("accepted input was automatically replayed after a turn failure")
	}
	postQueueMessage(t, h, r.ID, "continue", "codex")
	next := nextCall(t, ps[0])
	if strings.Contains(next.opts.Prompt, "ALREADY_RECEIVED") {
		t.Fatal("accepted input was repeated in the next prompt")
	}
	finishCall(next, "done")
}

func TestSteerPostedWhileCompletionWaitsForTheLockIsQueuedOnce(t *testing.T) {
	for i := 0; i < 12; i++ {
		h, r, ps := controlledHub(t, "codex")
		ps[0].steer = true
		postQueueMessage(t, h, r.ID, "start", "codex")
		first := nextCall(t, ps[0])
		h.mu.Lock()
		finishCall(first, "first done")
		_, err := h.acceptUser(h.rooms[r.ID], "BOUNDARY_INPUT", "codex", nil, nil, nil, nil, "")
		h.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		next := nextCall(t, ps[0])
		if strings.Count(next.opts.Prompt, "BOUNDARY_INPUT") != 1 {
			t.Fatalf("boundary input: %s", next.opts.Prompt)
		}
		finishCall(next, "next done")
		queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return !s.Active })
		if len(ps[0].callsCh) != 0 {
			t.Fatal("duplicate boundary delivery")
		}
	}
}
