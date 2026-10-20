package room

import (
	"encoding/json"
	"errors"
	"roundtable/internal/agent"
	"strings"
	"testing"
)

// Review 1A: the later participant must read the already completed first answer.
func TestQueueReviewComparisonReadsCompletedReply(t *testing.T) {
	for _, earlier := range []int{0, 1} {
		t.Run([]string{"claude-first", "codex-first"}[earlier], func(t *testing.T) {
			hub, r, ps := controlledHub(t, "claude", "codex")
			postQueueMessage(t, hub, r.ID, "first question", "")
			first := []*controlledCall{nextCall(t, ps[0]), nextCall(t, ps[1])}
			for _, call := range first {
				call.emit(agent.Event{Kind: agent.KindTextDelta, Text: "unfinished first answer"})
			}
			postQueueMessage(t, hub, r.ID, "compare first answers", "")
			later := 1 - earlier
			finishCall(first[earlier], "completed first answer")
			secondEarly := nextCall(t, ps[earlier])
			finishCall(first[later], "other first answer")
			secondLate := nextCall(t, ps[later])
			if strings.Contains(secondLate.opts.Prompt, "unfinished first answer") ||
				!strings.Contains(secondLate.opts.Prompt, "@"+ps[earlier].id+" ("+r.participant(ps[earlier].id).Label+")] completed first answer") {
				t.Fatalf("completed answer was frozen as streaming: %s", secondLate.opts.Prompt)
			}
			s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant(ps[later].id).Turns == 1 })
			for _, m := range s.Messages {
				if m.Author == ps[later].id {
					for _, seq := range s.Room.participant(ps[later].id).Unseen {
						if seq == m.Seq {
							t.Fatalf("own answer retained as an unseen hole: %d", seq)
						}
					}
				}
			}
			finishCall(secondEarly, "compared early")
			finishCall(secondLate, "compared late")
		})
	}
}

// Review 1B: queue first, then accept a Live correction that the CLI never reads.
func TestQueueReviewLateUndeliveredLiveIsDelivered(t *testing.T) {
	for _, ending := range []string{"success", "error", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			hub, r, ps := controlledHub(t, "claude")
			ps[0].steer = true
			postQueueMessage(t, hub, r.ID, "first", "claude")
			first := nextCall(t, ps[0])
			postQueueMessage(t, hub, r.ID, "@claude next question", "")
			m := postQueueMessage(t, hub, r.ID, "late correction", "claude")
			if !m.Live {
				t.Fatal("correction must be accepted live")
			}
			switch ending {
			case "success":
				finishCall(first, "first done")
			case "error":
				first.result <- controlledResult{err: errors.New("failed")}
			case "cancel":
				s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 2 })
				for _, turn := range s.Queue {
					if turn.Status == "running" {
						if err := hub.CancelTurn(r.ID, turn.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			correction := nextCall(t, ps[0])
			if !strings.Contains(correction.opts.Prompt, "] late correction") ||
				!strings.Contains(correction.opts.Prompt, "] @claude next question") {
				t.Fatalf("late correction or queued question missing: %s", correction.opts.Prompt)
			}
			hub.mu.Lock()
			_, msgs, _, err := hub.store.LoadAll()
			hub.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, saved := range msgs[r.ID] {
				if saved.ID == m.ID {
					found = true
					if saved.Live {
						t.Fatal("undelivered correction still live on disk")
					}
				}
			}
			if !found {
				t.Fatal("correction not persisted")
			}
			finishCall(correction, "correction handled")
		})
	}
}

func TestQueueReviewUndeliveredLiveCreatesItsOwnTurn(t *testing.T) {
	for _, ending := range []string{"success", "error", "cancel"} {
		t.Run(ending, func(t *testing.T) {
			hub, r, ps := controlledHub(t, "claude")
			ps[0].steer = true
			postQueueMessage(t, hub, r.ID, "first", "claude")
			first := nextCall(t, ps[0])
			m := postQueueMessage(t, hub, r.ID, "correction without another question", "claude")
			if !m.Live {
				t.Fatal("correction must be accepted live")
			}
			switch ending {
			case "success":
				finishCall(first, "done")
			case "error":
				first.result <- controlledResult{err: errors.New("failed")}
			case "cancel":
				s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 1 })
				if err := hub.CancelTurn(r.ID, s.Queue[0].ID); err != nil {
					t.Fatal(err)
				}
			}
			next := nextCall(t, ps[0])
			if !strings.Contains(next.opts.Prompt, "] correction without another question") {
				t.Fatalf("correction was not retried: %s", next.opts.Prompt)
			}
			finishCall(next, "received")
		})
	}
}

// Review 2, B/C: priority may consume both inputs; no empty Run follows.
func TestQueueReviewPriorityConsumesOrdinaryWithoutEmptyRun(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "ordinary broadcast", "")
	postQueueMessage(t, hub, r.ID, "priority target", "codex")
	finishCall(first, "first reply")
	urgent := nextCall(t, ps[0])
	if !strings.Contains(urgent.opts.Prompt, "] ordinary broadcast") ||
		!strings.Contains(urgent.opts.Prompt, "] priority target") {
		t.Fatalf("ordinary or target input was lost: %s", urgent.opts.Prompt)
	}
	finishCall(urgent, "urgent reply")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool {
		for _, m := range s.Messages {
			if m.Text == "urgent reply" && m.Status == "done" {
				return true
			}
		}
		return false
	})
	if len(s.Queue) != 0 {
		t.Fatalf("empty ordinary turn started after priority consumed its input: %+v", s.Queue)
	}
}

// Review 3: ordinary broadcast, mention and priority must wait for discussion.
func TestQueueReviewOrdinaryWaitsForDiscussion(t *testing.T) {
	for _, route := range []string{"broadcast", "mention", "target"} {
		t.Run(route, func(t *testing.T) {
			hub, r, ps := controlledHub(t, "claude", "codex", "agy")
			if err := hub.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
				t.Fatal(err)
			}
			claude := nextCall(t, ps[0])
			if route == "target" {
				postQueueMessage(t, hub, r.ID, "codex target during discussion", "codex")
			} else {
				finishCall(claude, "claude view\n[[DISCUSSION:AGREE]]")
				codex := nextCall(t, ps[1])
				text := "ordinary during discussion"
				if route == "mention" {
					text = "@claude ordinary during discussion"
				}
				postQueueMessage(t, hub, r.ID, text, "")
				s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) >= 2 })
				for _, turn := range s.Queue {
					if turn.Reason != "discussion" && turn.Status == "running" {
						t.Fatal("ordinary turn started during discussion")
					}
				}
				finishCall(codex, "codex view\n[[DISCUSSION:AGREE]]")
				agy := nextCall(t, ps[2])
				if route == "broadcast" && !strings.Contains(agy.opts.Prompt, "] ordinary during discussion") {
					t.Fatalf("broadcast was not delivered in the discussion: %s", agy.opts.Prompt)
				}
				finishCall(agy, "agy view\n[[DISCUSSION:AGREE]]")
				ordinary := nextCall(t, ps[0])
				finishCall(ordinary, "ordinary after discussion")
				if route == "broadcast" {
					finishCall(nextCall(t, ps[1]), "codex ordinary")
				}
				return
			}
			s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 4 })
			for _, turn := range s.Queue {
				if turn.Reason != "discussion" && turn.Status == "running" {
					t.Fatal("target overtook the discussion")
				}
			}
			finishCall(claude, "claude view\n[[DISCUSSION:AGREE]]")
			codex := nextCall(t, ps[1])
			if !strings.Contains(codex.opts.Prompt, "] codex target during discussion") {
				t.Fatalf("target was not delivered to the discussion speaker: %s", codex.opts.Prompt)
			}
			if !strings.Contains(codex.opts.Prompt, "자율 토론") {
				t.Fatal("target ran instead of the next discussion speaker")
			}
			finishCall(codex, "codex view\n[[DISCUSSION:AGREE]]")
			finishCall(nextCall(t, ps[2]), "agy view\n[[DISCUSSION:AGREE]]")
			queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
		})
	}
}

func TestQueueReviewCancelRetainsQueuedStatus(t *testing.T) {
	hub, r, _ := controlledHub(t, "claude", "codex")
	if err := hub.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	m := postQueueMessage(t, hub, r.ID, "@codex later question", "")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 3 })
	for _, turn := range s.Queue {
		if turn.Group == m.ID {
			if err := hub.CancelTurn(r.ID, turn.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	s = queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 2 })
	if s.Room.participant("codex").Status != "queued" {
		t.Fatalf("codex has a queued turn but status=%s", s.Room.participant("codex").Status)
	}
}

func TestQueueReviewDisabledTargetReturnsToIdle(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	seatID := r.Participants[0].ID
	postQueueMessage(t, hub, r.ID, "running target", seatID)
	first := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "waiting target", seatID)
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 2 })
	off := false
	if _, err := hub.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: seatID, Enabled: &off}}}); err != nil {
		t.Fatal(err)
	}
	before := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 1 })
	// Explicit targets require an enabled seat. Rejection must not add a message or turn.
	if m, err := hub.PostUser(r.ID, "disabled target", seatID); err == nil || err.Error() != "활성 대상 없음: "+seatID || m != nil {
		t.Fatalf("disabled target was not rejected: message=%+v err=%v", m, err)
	}
	after := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 1 })
	if after.Room.Seq != before.Room.Seq || len(after.Messages) != len(before.Messages) {
		t.Fatal("rejected target changed the transcript")
	}
	finishCall(first, "running target completed")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	if s.Room.participant(seatID).Status != "idle" {
		t.Fatalf("disabled participant stuck at %s", s.Room.participant(seatID).Status)
	}
	select {
	case <-ps[0].callsCh:
		t.Fatal("disabled participant started its waiting target")
	default:
	}
}

// Review 5: an unrelated completion must not capture the old session's partial reply.
func TestQueueReviewResetDoesNotRefreezeWaitingTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, hub, r.ID, "first", "")
	firstClaude, firstCodex := nextCall(t, ps[0]), nextCall(t, ps[1])
	firstClaude.emit(agent.Event{Kind: agent.KindTextDelta, Text: "unfinished before reset"})
	firstCodex.emit(agent.Event{Kind: agent.KindTextDelta, Text: "unfinished before reset"})
	postQueueMessage(t, hub, r.ID, "codex waiting question", "codex")
	if err := hub.ResetSessions(r.ID); err != nil {
		t.Fatal(err)
	}
	finishCall(firstClaude, "claude before codex finishes")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("claude").Status == "idle" })
	finishCall(firstCodex, "old codex result")
	next := nextCall(t, ps[1])
	if next.opts.SessionID != "" || strings.Contains(next.opts.Prompt, "unfinished before reset") ||
		!strings.Contains(next.opts.Prompt, "@claude (CLAUDE 1)] claude before codex finishes") {
		t.Fatalf("queued reset session did not read current completed content: %+v", next.opts)
	}
	finishCall(next, "fresh result")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	for _, m := range s.Messages {
		if m.Author == "codex" {
			for _, seq := range s.Room.participant("codex").Unseen {
				if seq == m.Seq {
					t.Fatalf("reset prompt froze its own prior reply as an unseen hole: %d", seq)
				}
			}
		}
	}
}

// Review 6: no runnable participants means no queued prompts are rendered.
func TestQueueReviewBlockedPumpDoesNotRenderWaitingPrompts(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "long work", "codex")
	_ = nextCall(t, ps[0])
	for i := 0; i < 32; i++ {
		postQueueMessage(t, hub, r.ID, "waiting question", "codex")
	}
	hub.mu.Lock()
	rs := hub.rooms[r.ID]
	rs.room.Note = strings.Repeat("large standing note ", 2048)
	allocs := testing.AllocsPerRun(5, func() { hub.pump(rs) })
	hub.mu.Unlock()
	t.Logf("blocked pump allocations: %.0f", allocs)
	if allocs > 100 {
		t.Fatalf("blocked pump rendered waiting prompts: %.0f allocations", allocs)
	}
}

func TestQueueReviewStopPersistsUndeliveredLiveWithoutRestart(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude")
	ps[0].steer = true
	postQueueMessage(t, hub, r.ID, "first", "claude")
	_ = nextCall(t, ps[0])
	m := postQueueMessage(t, hub, r.ID, "correction before stop", "claude")
	if err := hub.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	_, msgs, _, err := hub.store.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, saved := range msgs[r.ID] {
		if saved.ID == m.ID {
			found = true
			if saved.Live {
				t.Fatal("undelivered stopped correction still marked Live on disk")
			}
		}
	}
	if !found {
		t.Fatal("stopped correction not saved")
	}
	select {
	case <-ps[0].callsCh:
		t.Fatal("Stop restarted a Live retry")
	default:
	}
	postQueueMessage(t, hub, r.ID, "explicit restart", "claude")
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "] correction before stop") {
		t.Fatal("stopped correction was lost")
	}
	finishCall(next, "handled after explicit restart")
}

func TestQueueReviewLiveSteerStillAllowedDuringDiscussion(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	ps[0].steer = true
	if err := hub.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	claude := nextCall(t, ps[0])
	live := postQueueMessage(t, hub, r.ID, "discussion correction", "claude")
	if !live.Live {
		t.Fatal("discussion rejected live steering")
	}
	select {
	case text := <-claude.opts.Steer:
		if text != "discussion correction" {
			t.Fatalf("unexpected live text: %s", text)
		}
	default:
		t.Fatal("live text was not written to running discussion's channel")
	}
	claude.result <- controlledResult{text: "corrected\n[[DISCUSSION:AGREE]]", steered: 1}
	finishCall(nextCall(t, ps[1]), "agree\n[[DISCUSSION:AGREE]]")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 && s.Discussion == nil })
	select {
	case <-ps[0].callsCh:
		t.Fatal("delivered live correction was retried")
	default:
	}
}

func TestQueueReviewRunningTextResetsForNextTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	first.emit(agent.Event{Kind: agent.KindToolCall, Name: "old-tool"})
	postQueueMessage(t, hub, r.ID, "second", "codex")
	finishCall(first, "first reply")
	second := nextCall(t, ps[0])
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Turns == 1 })
	if s.Room.participant("codex").StatusText != "생각 중" {
		t.Fatalf("new running turn retained old status text: %s", s.Room.participant("codex").StatusText)
	}
	finishCall(second, "second reply")
}

func TestQueueReviewStatusAndLastQueueEventMatchSnapshot(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	seatID := r.Participants[0].ID
	postQueueMessage(t, hub, r.ID, "running target", seatID)
	first := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "waiting target", seatID)
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 2 })
	ch, unsub, err := hub.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	// Disable after enqueueing: new explicit targets are rejected before reaching pump.
	off := false
	if _, err := hub.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: seatID, Enabled: &off}}}); err != nil {
		t.Fatal(err)
	}
	finishCall(first, "running target completed")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	if s.Room.participant(seatID).Status != "idle" {
		t.Fatalf("disabled participant stuck at %s", s.Room.participant(seatID).Status)
	}
	var lastQueue []*Turn
	sawQueue := false
	for {
		select {
		case data := <-ch:
			var event struct {
				Type string
				Data json.RawMessage
			}
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == "queue" {
				sawQueue = true
				if err := json.Unmarshal(event.Data, &lastQueue); err != nil {
					t.Fatal(err)
				}
			}
		default:
			if !sawQueue || len(lastQueue) != len(s.Queue) {
				t.Fatalf("final queue event disagrees with empty snapshot: %+v", lastQueue)
			}
			return
		}
	}
}

func TestQueueReviewUnchangedStatusIsNotRebroadcast(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "long work", "codex")
	first := nextCall(t, ps[0])
	ch, unsub, err := hub.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	if _, err := hub.UpdateRoom(r.ID, RoomPatch{}); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case data := <-ch:
			var event struct{ Type string }
			if err := json.Unmarshal(data, &event); err != nil {
				t.Fatal(err)
			}
			if event.Type == "participant" {
				t.Fatalf("unchanged participant state was rebroadcast: %s", data)
			}
		default:
			finishCall(first, "done")
			return
		}
	}
}
