package room

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// controlledProvider makes completion order explicit instead of relying on
// vendor speeds or sleeps. Every Run has its own release channel.
type controlledProvider struct {
	fakeProvider
	callsCh chan *controlledCall
}

type controlledCall struct {
	opts   agent.RunOptions
	ctx    context.Context
	emit   func(agent.Event)
	result chan controlledResult
}

type controlledResult struct {
	text    string
	err     error
	steered int
	usage   map[string]any // parsers emit this map as the usage event and return it
}

func (p *controlledProvider) Run(ctx context.Context, opts agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	call := &controlledCall{opts: opts, ctx: ctx, emit: emit, result: make(chan controlledResult, 1)}
	p.callsCh <- call
	select {
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	case result := <-call.result:
		return agent.Result{Text: result.text, SessionID: p.id + "-session", Steered: result.steered, Usage: result.usage}, result.err
	}
}

func controlledHub(t *testing.T, ids ...string) (*Hub, *Room, []*controlledProvider) {
	t.Helper()
	var fakes []*fakeProvider
	var providers []agent.Provider
	var controlled []*controlledProvider
	for _, id := range ids {
		p := &controlledProvider{fakeProvider: fakeProvider{id: id}, callsCh: make(chan *controlledCall, 16)}
		fakes = append(fakes, &p.fakeProvider)
		providers = append(providers, p)
		controlled = append(controlled, p)
	}
	hub, r := newTestHub(t, fakes...)
	hub.reg = agent.NewRegistry(providers...)
	t.Cleanup(func() {
		_ = hub.Stop(r.ID)
		queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	})
	return hub, r, controlled
}

func nextCall(t *testing.T, p *controlledProvider) *controlledCall {
	t.Helper()
	select {
	case call := <-p.callsCh:
		return call
	case <-time.After(3 * time.Second):
		t.Fatalf("%s did not start", p.id)
		return nil
	}
}

func queueSnapshot(t *testing.T, hub *Hub, id string, ready func(*Snapshot) bool) *Snapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		data, err := hub.SnapshotJSON(id)
		if err != nil {
			t.Fatal(err)
		}
		var s Snapshot
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		if ready(&s) {
			return &s
		}
		select {
		case <-deadline:
			t.Fatalf("room did not reach expected state: %s", data)
		case <-time.After(time.Millisecond):
		}
	}
}

func postQueueMessage(t *testing.T, hub *Hub, id, text, target string) *Message {
	t.Helper()
	m, err := hub.PostUser(id, text, target)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func finishCall(call *controlledCall, text string) {
	call.result <- controlledResult{text: text}
}

func TestReplyToPersistsAcrossParallelAndChainedTurns(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	trigger := postQueueMessage(t, h, r.ID, "start", "")
	first, second := nextCall(t, ps[0]), nextCall(t, ps[1])
	finishCall(first, "@codex follow up")
	finishCall(second, "parallel reply")
	chained := nextCall(t, ps[1])
	finishCall(chained, "follow up completed")
	waitIdle(t, h, r.ID)
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := restarted.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var source string
	for _, m := range s.Messages {
		if m.Text == "@codex follow up" {
			source = m.ID
		}
	}
	if source == "" {
		t.Fatal("missing chain source")
	}
	for _, m := range s.Messages {
		want := ""
		if m.Kind == "agent" {
			want = trigger.ID
			if m.Text == "follow up completed" {
				want = source
			}
		}
		if m.ReplyTo != want {
			t.Fatalf("message %s reply_to=%q, want %q", m.Text, m.ReplyTo, want)
		}
	}
}

func TestSessionMetadataRenameFailureKeepsInvocationRunning(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "start", "codex")
	call := nextCall(t, ps[0])
	// Force SaveRoom's rename, not the already successful message append, to fail.
	path := filepath.Join(h.store.roomDir(r.ID), "room.json")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	call.emit(agent.Event{Kind: agent.KindSession, Text: "codex-session"})
	if call.ctx.Err() != nil {
		t.Fatal("metadata failure cancelled an invocation with a durable session message")
	}
	s, err := h.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, m := range s.Messages {
		if m.Kind == "agent" && (m.Status != "streaming" || m.Error != "") {
			t.Fatalf("metadata failure became a turn error: %+v", m)
		}
		warned = warned || (m.Kind == "system" && m.Status == "error" && strings.Contains(m.Text, "방 저장 실패"))
	}
	if !warned {
		t.Fatal("metadata failure was not visible")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	rooms, saved, err := h.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	sessionSaved := false
	for _, m := range saved[r.ID] {
		sessionSaved = sessionSaved || (m.Kind == "agent" && m.SessionID == "codex-session")
	}
	if !sessionSaved || rooms[0].Participants[0].SessionID != "" {
		t.Fatal("failure fixture did not separate message and room persistence")
	}
	finishCall(call, "answer survived metadata failure")
	waitIdle(t, h, r.ID)
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Room.Participants[0].SessionID != "codex-session" || replay.Room.Participants[0].Turns != 1 {
		t.Fatalf("completion did not save updated room metadata: %+v", replay.Room.Participants[0])
	}
	var reply *Message
	for _, m := range replay.Messages {
		if m.Kind == "agent" {
			reply = m
		}
	}
	if reply == nil || reply.Status != "done" || reply.Error != "" || reply.Text != "answer survived metadata failure" {
		t.Fatalf("answer was discarded: %+v", reply)
	}
}

func TestSessionMessagePersistenceFailureStillCancelsInvocation(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "start", "codex")
	call := nextCall(t, ps[0])
	path := filepath.Join(h.store.roomDir(r.ID), "messages.jsonl")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	call.emit(agent.Event{Kind: agent.KindSession, Text: "codex-session"})
	waitIdle(t, h, r.ID)
	if call.ctx.Err() == nil {
		t.Fatal("session message persistence failure did not cancel the invocation")
	}
	s, err := h.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var reply *Message
	for _, m := range s.Messages {
		if m.Kind == "agent" {
			reply = m
		}
	}
	if reply == nil || reply.Status != "error" {
		t.Fatalf("missing turn failure: %+v", reply)
	}
	warned := false
	for _, m := range s.Messages {
		warned = warned || (m.Kind == "system" && strings.Contains(m.Text, "세션 메시지 저장 실패"))
	}
	if !warned {
		t.Fatal("session message failure was not reported")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
}

func TestStopSucceedsWhenItsNoticeCannotBeSaved(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	a, b := nextCall(t, ps[0]), nextCall(t, ps[1])
	postQueueMessage(t, h, r.ID, "queued input", "codex")
	path := filepath.Join(h.store.roomDir(r.ID), "messages.jsonl")
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := h.Stop(r.ID); err != nil {
		t.Fatalf("completed cancellation returned a notice error: %v", err)
	}
	if a.ctx.Err() == nil || b.ctx.Err() == nil {
		t.Fatal("stop did not cancel both running invocations")
	}
	s := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 && s.Discussion == nil })
	warned := false
	for _, m := range s.Messages {
		warned = warned || (m.Kind == "system" && m.Status == "error" && strings.Contains(m.Text, "시스템 메시지 저장 실패") && strings.Contains(m.Text, "메모리에만"))
	}
	if !warned {
		t.Fatal("failed stop notice was not reported visibly")
	}
	if len(ps[0].callsCh) != 0 || len(ps[1].callsCh) != 0 {
		t.Fatal("stop started pending work")
	}
	if err := h.Stop("missing-room"); err == nil {
		t.Fatal("stop accepted a nonexistent room")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
}

func TestLiveInputAcceptanceAndPersistenceFailures(t *testing.T) {
	for _, failedFile := range []string{"room.json.tmp", "messages.jsonl"} {
		t.Run(failedFile, func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex")
			ps[0].steer = true
			postQueueMessage(t, h, r.ID, "start", "codex")
			call := nextCall(t, ps[0])
			path := filepath.Join(h.store.roomDir(r.ID), failedFile)
			if failedFile == "messages.jsonl" {
				if err := os.Rename(path, path+".saved"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			m, err := h.PostUser(r.ID, "live correction", "codex")
			if failedFile == "room.json.tmp" {
				if err != nil || m == nil || !m.Live {
					t.Fatalf("accepted live input returned failure: %+v %v", m, err)
				}
				select {
				case text := <-call.opts.Steer:
					if text != "live correction" {
						t.Fatal(text)
					}
				default:
					t.Fatal("accepted correction was not delivered")
				}
				_, saved, e := h.store.LoadRooms()
				if e != nil {
					t.Fatal(e)
				}
				found := false
				for _, stored := range saved[r.ID] {
					if stored.ID == m.ID {
						found = stored.Live
					}
				}
				if !found {
					t.Fatal("delivered steer was not persisted as live")
				}
			} else {
				if err == nil || m != nil {
					t.Fatalf("unsaved input was accepted: %+v %v", m, err)
				}
			}
			select {
			case extra := <-call.opts.Steer:
				t.Fatalf("unsaved or duplicate steer delivered: %q", extra)
			default:
			}
			if e := os.Remove(path); e != nil {
				t.Fatal(e)
			}
			if failedFile == "messages.jsonl" {
				if e := os.Rename(path+".saved", path); e != nil {
					t.Fatal(e)
				}
			}
			call.result <- controlledResult{text: "finished", steered: 1}
			waitIdle(t, h, r.ID)
			if len(ps[0].callsCh) != 0 {
				t.Fatal("accepted input was replayed as another turn")
			}
		})
	}
}

func TestTargetRunsWhileOtherParticipantIsBusy(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, hub, r.ID, "start together", "")
	slow, initial := nextCall(t, ps[0]), nextCall(t, ps[1])
	finishCall(initial, "initial codex reply")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Status == "idle" })

	postQueueMessage(t, hub, r.ID, "codex target while claude runs", "codex")
	targeted := nextCall(t, ps[1])
	if !strings.Contains(targeted.opts.Prompt, "] codex target while claude runs") {
		t.Fatalf("target behind streaming reply was lost: %s", targeted.opts.Prompt)
	}
	finishCall(targeted, "target completed")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Status == "idle" })
	if got := agentTexts(s, "codex"); len(got) != 2 || got[1] != "done:target completed" {
		t.Fatalf("target was not completed: %v", got)
	}
	if s.Room.participant("claude").Status != "running" {
		t.Fatal("slow participant must still be running")
	}
	finishCall(slow, "slow reply after target")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })

	// The unfinished reply was skipped, not acknowledged. It arrives exactly
	// once on the next turn, while the already delivered target stays excluded.
	postQueueMessage(t, hub, r.ID, "read the late reply", "codex")
	later := nextCall(t, ps[1])
	if strings.Count(later.opts.Prompt, "@claude (CLAUDE 1)] slow reply after target") != 1 ||
		strings.Contains(later.opts.Prompt, "] codex target while claude runs") {
		t.Fatalf("out-of-order delta is wrong: %s", later.opts.Prompt)
	}
	finishCall(later, "read")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	postQueueMessage(t, hub, r.ID, "one more", "codex")
	final := nextCall(t, ps[1])
	if strings.Contains(final.opts.Prompt, "slow reply after target") {
		t.Fatalf("late reply delivered twice: %s", final.opts.Prompt)
	}
	finishCall(final, "done")
}

func TestParticipantQueuedTargetsRunInOrder(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	secondMsg := postQueueMessage(t, hub, r.ID, "second", "codex")
	thirdMsg := postQueueMessage(t, hub, r.ID, "third", "codex")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 3 })
	var queued []*Turn
	for _, turn := range s.Queue {
		if turn.Status == "queued" {
			queued = append(queued, turn)
		}
	}
	if len(queued) != 2 || queued[0].Group != secondMsg.ID || queued[1].Group != thirdMsg.ID {
		t.Fatalf("target FIFO reversed: %+v", queued)
	}
	select {
	case <-ps[0].callsCh:
		t.Fatal("same participant ran concurrently")
	default:
	}
	finishCall(first, "first reply")
	second := nextCall(t, ps[0])
	if second.opts.SessionID != "codex-session" || !strings.Contains(second.opts.Prompt, "] second") ||
		strings.Contains(second.opts.Prompt, "] third") {
		t.Fatalf("second turn wrong: %+v", second.opts)
	}
	select {
	case <-ps[0].callsCh:
		t.Fatal("third turn ran before second completed")
	default:
	}
	finishCall(second, "second reply")
	third := nextCall(t, ps[0])
	if third.opts.SessionID != "codex-session" || !strings.Contains(third.opts.Prompt, "] third") ||
		strings.Contains(third.opts.Prompt, "] second") {
		t.Fatalf("third turn wrong: %+v", third.opts)
	}
	finishCall(third, "third reply")
}

func TestBroadcastSnapshotSurvivesBusyParticipant(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, hub, r.ID, "claude first", "claude")
	slow := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "shared broadcast", "")
	fast := nextCall(t, ps[1])
	finishCall(fast, "fast broadcast answer")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Status == "idle" })
	finishCall(slow, "prior claude answer")
	delayed := nextCall(t, ps[0])
	if !strings.Contains(fast.opts.Prompt, "] shared broadcast") ||
		!strings.Contains(delayed.opts.Prompt, "] shared broadcast") ||
		strings.Contains(delayed.opts.Prompt, "fast broadcast answer") {
		t.Fatalf("broadcast snapshot changed:\nfast=%s\ndelayed=%s", fast.opts.Prompt, delayed.opts.Prompt)
	}
	finishCall(delayed, "delayed broadcast answer")
}

func TestStopDoesNotStartQueuedTurns(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, hub, r.ID, "start", "")
	_ = nextCall(t, ps[0])
	_ = nextCall(t, ps[1])
	postQueueMessage(t, hub, r.ID, "queued claude", "claude")
	postQueueMessage(t, hub, r.ID, "queued codex", "codex")
	if err := hub.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	for _, m := range s.Messages {
		if m.Kind == "agent" && m.Status != "cancelled" {
			t.Fatalf("unexpected turn after stop: %+v", m)
		}
	}
	for _, p := range ps {
		select {
		case <-p.callsCh:
			t.Fatalf("%s started a queued turn after stop", p.id)
		default:
		}
	}
}

func TestStopSkipsPreparedTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	// Hold the exact boundary between reservation and the goroutine entering Run.
	hub.mu.Lock()
	rs := hub.rooms[r.ID]
	rs.nextMessage("user", "user", "reserved before stop")
	turn := &Turn{ID: newID(), Agent: "codex", Reason: "mention", Cap: rs.room.Seq, Status: "queued"}
	pr := hub.buildTurn(rs, turn)
	hub.prepareTurn(rs, pr)
	hub.turnWG.Add(1) // Match pump: register the reserved turn before launching it.
	hub.mu.Unlock()
	if err := hub.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	go hub.runTurn(rs, pr)
	hub.turnWG.Wait()
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	if got := agentTexts(s, "codex"); len(got) != 1 || got[0] != "cancelled:" {
		t.Fatalf("prepared turn did not finish as cancelled: %v", got)
	}
	select {
	case <-ps[0].callsCh:
		t.Fatal("provider called for a turn stopped before Run")
	default:
	}
}

func TestQueuedTargetRetriesFailedInput(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "retry first input", "codex")
	first := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "second input", "codex")
	first.result <- controlledResult{err: errors.New("first failed")}
	second := nextCall(t, ps[0])
	if !strings.Contains(second.opts.Prompt, "] retry first input") ||
		!strings.Contains(second.opts.Prompt, "] second input") {
		t.Fatalf("failed input lost from already queued turn: %s", second.opts.Prompt)
	}
	finishCall(second, "retried")
}

func TestQueuedTurnReceivesUndeliveredLiveSteer(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude")
	ps[0].steer = true
	postQueueMessage(t, hub, r.ID, "first", "claude")
	first := nextCall(t, ps[0])
	live := postQueueMessage(t, hub, r.ID, "undelivered live correction", "claude")
	if !live.Live {
		t.Fatal("correction was not accepted as live")
	}
	postQueueMessage(t, hub, r.ID, "@claude follow up", "")
	// This provider never writes steering to a CLI and reports Steered == 0.
	finishCall(first, "first reply")
	queued := nextCall(t, ps[0])
	if !strings.Contains(queued.opts.Prompt, "] undelivered live correction") {
		t.Fatalf("frozen Live flag lost an undelivered correction: %s", queued.opts.Prompt)
	}
	finishCall(queued, "correction received")
}

func TestResetRefreshesQueuedSessionAndPrompt(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first before reset", "codex")
	first := nextCall(t, ps[0])
	postQueueMessage(t, hub, r.ID, "queued before reset", "codex")
	if err := hub.ResetSessions(r.ID); err != nil {
		t.Fatal(err)
	}
	finishCall(first, "old session result")
	next := nextCall(t, ps[0])
	if next.opts.SessionID != "" ||
		!strings.Contains(next.opts.Prompt, "지금까지의 대화") ||
		!strings.Contains(next.opts.Prompt, "] first before reset") ||
		!strings.Contains(next.opts.Prompt, "] queued before reset") ||
		strings.Contains(next.opts.Prompt, "] 세션을 초기화했어.") {
		t.Fatalf("queued turn did not refresh after reset: %+v", next.opts)
	}
	finishCall(next, "fresh session")
}

func TestCancelQueuedTargetPreservesFollowingTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	second := postQueueMessage(t, hub, r.ID, "second", "codex")
	postQueueMessage(t, hub, r.ID, "third", "codex")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 3 })
	for _, turn := range s.Queue {
		if turn.Group == second.ID {
			if err := hub.CancelTurn(r.ID, turn.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	finishCall(first, "first reply")
	third := nextCall(t, ps[0])
	if !strings.Contains(third.opts.Prompt, "] third") {
		t.Fatalf("following target lost: %s", third.opts.Prompt)
	}
	finishCall(third, "third reply")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	select {
	case <-ps[0].callsCh:
		t.Fatal("cancelled target started")
	default:
	}
}

func TestUnfinishedReplyGapPersistsAcrossReload(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, hub, r.ID, "start", "")
	slow, fast := nextCall(t, ps[0]), nextCall(t, ps[1])
	finishCall(fast, "fast")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Status == "idle" })
	postQueueMessage(t, hub, r.ID, "past gap", "codex")
	targeted := nextCall(t, ps[1])
	finishCall(targeted, "past gap done")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return s.Room.participant("codex").Status == "idle" })
	finishCall(slow, "late reply persisted")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	restored, err := NewHub(hub.store, hub.reg, hub.scratch, hub.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = restored.Stop(r.ID)
		queueSnapshot(t, restored, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	})
	postQueueMessage(t, restored, r.ID, "after reload", "codex")
	next := nextCall(t, ps[1])
	if !strings.Contains(next.opts.Prompt, "@claude (CLAUDE 1)] late reply persisted") ||
		strings.Contains(next.opts.Prompt, "] past gap") {
		t.Fatalf("persisted out-of-order delivery wrong: %s", next.opts.Prompt)
	}
	finishCall(next, "read after reload")
}

func TestLiveSteerCountBelongsToCurrentTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "claude")
	ps[0].steer = true
	postQueueMessage(t, hub, r.ID, "start", "claude")
	first := nextCall(t, ps[0])
	firstLive := postQueueMessage(t, hub, r.ID, "first live", "claude")
	postQueueMessage(t, hub, r.ID, "@claude next", "")
	first.result <- controlledResult{text: "first reply", steered: 1}
	second := nextCall(t, ps[0])
	secondLive := postQueueMessage(t, hub, r.ID, "second live", "claude")
	second.result <- controlledResult{text: "second reply", steered: 1}
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	for _, m := range s.Messages {
		if (m.ID == firstLive.ID || m.ID == secondLive.ID) && !m.Live {
			t.Fatalf("delivered live message lost its acknowledgement: %+v", m)
		}
	}
	postQueueMessage(t, hub, r.ID, "last", "claude")
	last := nextCall(t, ps[0])
	if strings.Contains(last.opts.Prompt, "] first live") || strings.Contains(last.opts.Prompt, "] second live") {
		t.Fatalf("delivered steer repeated: %s", last.opts.Prompt)
	}
	finishCall(last, "done")
}

func TestTargetKeepsPriorityOverOrdinaryQueuedTurn(t *testing.T) {
	hub, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, hub, r.ID, "first", "codex")
	first := nextCall(t, ps[0])
	ordinary := postQueueMessage(t, hub, r.ID, "ordinary broadcast", "")
	target := postQueueMessage(t, hub, r.ID, "priority target", "codex")
	s := queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 3 })
	var queued []*Turn
	for _, turn := range s.Queue {
		if turn.Status == "queued" {
			queued = append(queued, turn)
		}
	}
	if len(queued) != 2 || queued[0].Group != target.ID || queued[1].Group != ordinary.ID {
		t.Fatalf("target lost priority over ordinary turn: %+v", queued)
	}
	finishCall(first, "first reply")
	urgent := nextCall(t, ps[0])
	if !strings.Contains(urgent.opts.Prompt, "] priority target") ||
		!strings.Contains(urgent.opts.Prompt, "] ordinary broadcast") {
		t.Fatalf("ordinary turn ran before target: %s", urgent.opts.Prompt)
	}
	finishCall(urgent, "urgent reply")
	queueSnapshot(t, hub, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 })
	select {
	case <-ps[0].callsCh:
		t.Fatal("empty ordinary turn ran after priority delivered both inputs")
	default:
	}
}
