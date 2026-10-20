package room

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestDiscussionFailuresAreExcludedAndPersisted(t *testing.T) {
	for _, state := range []string{"error", "cancelled", "all_failed"} {
		t.Run(state, func(t *testing.T) {
			h, r, ps := controlledHub(t, "claude", "codex")
			if err := h.StartDiscussion(r.ID, 1, nil); err != nil {
				t.Fatal(err)
			}
			a, b := nextCall(t, ps[0]), nextCall(t, ps[1])
			if state == "cancelled" {
				s, _ := h.Snapshot(r.ID)
				for _, turn := range s.Queue {
					if turn.Agent == "claude" {
						if err := h.CancelTurn(r.ID, turn.ID); err != nil {
							t.Fatal(err)
						}
					}
				}
			} else {
				a.result <- controlledResult{text: "partial\n[[DISCUSSION:CONCLUDE]]", err: os.ErrPermission}
			}
			if state == "all_failed" {
				b.result <- controlledResult{err: os.ErrPermission}
			} else {
				finishCall(b, "[[DISCUSSION:AGREE]]")
			}
			queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return s.Discussion == nil && len(s.Queue) == 0 })
			restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
			if err != nil {
				t.Fatal(err)
			}
			s, _ := restarted.Snapshot(r.ID)
			failed := roundMessage(s, "claude", 1)
			if failed == nil || failed.Status == "done" || failed.Signal != "" {
				t.Fatalf("failed signal persisted: %+v", failed)
			}
			var ending string
			for _, m := range s.Messages {
				if m.Kind == "system" && strings.Contains(m.Text, "토론 종료") {
					ending = m.Text
				}
			}
			if !strings.Contains(ending, "@claude 실패로 제외") || strings.Contains(ending, "전원 동의") || strings.Contains(ending, "결론 제시") {
				t.Fatal(ending)
			}
			if state == "all_failed" && (strings.Contains(ending, "동의/패스") || !strings.Contains(ending, "@codex 실패로 제외")) {
				t.Fatal(ending)
			}
		})
	}
}

func TestMissingDiscussionSignalCreatesDurableSeatNotice(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "no signal")
	finishCall(nextCall(t, ps[1]), "[[DISCUSSION:AGREE]]")
	waitIdle(t, h, r.ID)
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := restarted.Snapshot(r.ID)
	reply := roundMessage(s, "claude", 1)
	if reply == nil || reply.Signal != "CONTINUE" || reply.Status != "done" || reply.ReplyTo != "" {
		t.Fatal(reply)
	}
	found := false
	for _, m := range s.Messages {
		if m.NoticeFor == "claude" && m.NoticeMarker == "discussion" && m.NoticeMsg == reply.ID && strings.Contains(m.Text, "토론 신호가 없어 CONTINUE로 처리함") {
			if strings.Contains(m.Text, "마커 거절") {
				t.Fatal(m.Text)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing durable discussion notice")
	}
}

func TestDiscussionRecoveredSeatParticipatesInFinalAgreement(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 3, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{err: os.ErrPermission}
	finishCall(nextCall(t, ps[1]), "[[DISCUSSION:CONTINUE]]")
	finishCall(nextCall(t, ps[0]), "[[DISCUSSION:CONTINUE]]")
	finishCall(nextCall(t, ps[1]), "[[DISCUSSION:AGREE]]")
	finishCall(nextCall(t, ps[0]), "[[DISCUSSION:AGREE]]")
	finishCall(nextCall(t, ps[1]), "[[DISCUSSION:AGREE]]")
	waitIdle(t, h, r.ID)
	_, saved, err := h.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	var ending string
	for _, m := range saved[r.ID] {
		if m.Kind == "system" && strings.Contains(m.Text, "토론 종료") {
			ending = m.Text
		}
	}
	if !strings.Contains(ending, "3라운드에서 전원 동의/패스") || strings.Contains(ending, "실패로 제외") {
		t.Fatal(ending)
	}
}

func TestDiscussionMissingSignalNoticeWaitsForCompletionHandlers(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	// The provider succeeds, but a later completion handler cannot save its card.
	if err := os.Mkdir(filepath.Join(h.store.roomDir(r.ID), "decisions.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	finishCall(call, "answer without discussion signal\n[[DECISION: proposal | 근거: source | 반대: 없음 | 미확인: 없음]]")
	finishCall(nextCall(t, ps[1]), "[[DISCUSSION:AGREE]]")
	waitIdle(t, h, r.ID)
	_, saved, err := h.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range saved[r.ID] {
		if m.Kind == "agent" && m.Author == "claude" {
			found = true
			if m.Status != "error" || m.Signal != "" {
				t.Fatalf("failed completion retained a vote: %+v", m)
			}
		}
		if m.NoticeMarker == "discussion" && m.NoticeFor == "claude" {
			t.Fatalf("failed completion retained CONTINUE feedback: %+v", m)
		}
	}
	if !found {
		t.Fatal("missing failed reply")
	}
}

// sequentialFirst keeps the pre-blind behaviour for tests about sequential rounds.
var sequentialFirst = func() *bool { v := false; return &v }()

func roundMessage(s *Snapshot, author string, round int) *Message {
	for _, m := range s.Messages {
		if m.Kind == "agent" && m.Author == author && m.Reason == "discussion" && m.Round == round {
			return m
		}
	}
	return nil
}

func TestBlindFirstRoundHidesPeersUntilRoundTwo(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	// codex is busy when the discussion starts, so its blind turn is built
	// only after claude's round-1 answer is already in the room.
	postQueueMessage(t, h, r.ID, "busy first", "codex")
	busy := nextCall(t, ps[1])
	if err := h.StartDiscussion(r.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	a1 := nextCall(t, ps[0])
	if !strings.Contains(a1.opts.Prompt, "1라운드는 블라인드") {
		t.Fatal(a1.opts.Prompt)
	}
	finishCall(a1, "claude-blind-answer\n[[DECISION: claude-blind-decision | 근거: x | 반대: 없음 | 미확인: 없음]]\n[[DISCUSSION:CONTINUE]]")
	answered := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool {
		m := roundMessage(s, "claude", 1)
		return m != nil && m.Status == "done"
	})
	// A seat's card is only proposed; confirm it so the cap is what hides it.
	if _, err := h.UpdateDecision(r.ID, answered.Decisions[0].ID, DecisionInput{Status: ptr("active")}); err != nil {
		t.Fatal(err)
	}
	finishCall(busy, "busy reply")
	b1 := nextCall(t, ps[1])
	// Neither the history nor the standing brief's current decisions may carry
	// claude's round-1 answer or the decision it recorded.
	if strings.Contains(b1.opts.Prompt, "claude-blind-answer") || strings.Contains(b1.opts.Prompt, "claude-blind-decision") || !strings.Contains(b1.opts.Prompt, "1라운드는 블라인드") {
		t.Fatalf("blind round leaked a peer answer:\n%s", b1.opts.Prompt)
	}
	finishCall(b1, "codex-blind-answer\n[[DISCUSSION:CONTINUE]]")

	a2 := nextCall(t, ps[0])
	if !strings.Contains(a2.opts.Prompt, "codex-blind-answer") || !strings.Contains(a2.opts.Prompt, "1라운드 답들이 공개됐습니다") {
		t.Fatalf("round 2 must reveal round 1:\n%s", a2.opts.Prompt)
	}
	finishCall(a2, "claude-round-two\n[[DISCUSSION:AGREE]]")
	b2 := nextCall(t, ps[1])
	// Round 2 is sequential again: codex reads claude's round-1 and round-2 answers.
	if !strings.Contains(b2.opts.Prompt, "claude-blind-answer") || !strings.Contains(b2.opts.Prompt, "claude-round-two") || !strings.Contains(b2.opts.Prompt, "1라운드 답들이 공개됐습니다") || !strings.Contains(b2.opts.Prompt, "현재 결정(최신순):\n- claude-blind-decision\n") {
		t.Fatalf("round 2 prompt:\n%s", b2.opts.Prompt)
	}
	finishCall(b2, "[[DISCUSSION:AGREE]]")
	snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return s.Discussion == nil && len(s.Queue) == 0 })
	var started, ended bool
	for _, m := range snap.Messages {
		started = started || (m.Kind == "system" && strings.Contains(m.Text, "1라운드 블라인드"))
		ended = ended || (m.Kind == "system" && strings.Contains(m.Text, "검증은 아님"))
	}
	if !started || !ended {
		t.Fatal("blind start or agreement-is-not-verification notice missing")
	}
}

func TestBlindFirstRoundRunsConcurrentlyFromOneCap(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex", "agy")
	postQueueMessage(t, h, r.ID, "topic", "claude")
	finishCall(nextCall(t, ps[0]), "earlier claude reply")
	waitIdle(t, h, r.ID)
	if err := h.StartDiscussion(r.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	// All three start before any answers: same call count as sequential, run at once.
	calls := []*controlledCall{nextCall(t, ps[0]), nextCall(t, ps[1]), nextCall(t, ps[2])}
	snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 3 })
	var caps []int64
	for _, turn := range snap.Queue {
		if !turn.Blind || turn.Status != "running" {
			t.Fatalf("%+v", turn)
		}
		caps = append(caps, turn.Cap)
	}
	if caps[0] != caps[1] || caps[1] != caps[2] || snap.Discussion == nil || !snap.Discussion.Blind {
		t.Fatalf("caps %v discussion %+v", caps, snap.Discussion)
	}
	for i, call := range calls {
		finishCall(call, "answer "+ps[i].id+"\n[[DISCUSSION:CONTINUE]]")
	}
	snap = queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return s.Discussion == nil && len(s.Queue) == 0 })
	for _, p := range []string{"claude", "codex", "agy"} {
		if m := roundMessage(snap, p, 1); m == nil || m.Status != "done" || m.Signal != "CONTINUE" {
			t.Fatalf("%s round 1: %+v", p, m)
		}
	}
}

func TestSequentialDiscussionKeepsOrderWhenBlindIsOff(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 1, sequentialFirst); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, ps[0])
	if len(ps[1].callsCh) != 0 {
		t.Fatal("sequential round started the second seat early")
	}
	if strings.Contains(first.opts.Prompt, "블라인드") {
		t.Fatal(first.opts.Prompt)
	}
	finishCall(first, "seq-one\n[[DISCUSSION:CONTINUE]]")
	second := nextCall(t, ps[1])
	if !strings.Contains(second.opts.Prompt, "seq-one") {
		t.Fatal(second.opts.Prompt)
	}
	finishCall(second, "[[DISCUSSION:AGREE]]")
	waitIdle(t, h, r.ID)
}

func TestBlindFirstDefaultSetting(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"})
	if !r.BlindFirstDefault {
		t.Fatal("new rooms start discussions blind by default")
	}
	off := false
	other, err := h.CreateRoom(RoomSpec{Title: "seq", Mode: agent.ModeChat, Participants: []string{"claude", "codex"}, BlindFirstDefault: &off})
	if err != nil || other.BlindFirstDefault {
		t.Fatal(other, err)
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{BlindFirstDefault: &off}); err != nil {
		t.Fatal(err)
	}
	if err := h.StartDiscussion(r.ID, 1, nil); err != nil {
		t.Fatal(err)
	}
	snap, _ := h.Snapshot(r.ID)
	if snap.Discussion == nil || snap.Discussion.Blind {
		t.Fatalf("room default off must start sequentially: %+v", snap.Discussion)
	}
	waitIdle(t, h, r.ID)

	// A room.json written before the setting existed keeps the default (true);
	// an explicit false survives a restart.
	path := filepath.Join(h.store.roomDir(other.ID), "room.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "blind_first_default")
	data, _ = json.Marshal(raw)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := restarted.Snapshot(other.ID)
	kept, _ := restarted.Snapshot(r.ID)
	if !legacy.Room.BlindFirstDefault || kept.Room.BlindFirstDefault {
		t.Fatalf("legacy=%v explicit=%v", legacy.Room.BlindFirstDefault, kept.Room.BlindFirstDefault)
	}
}

func TestDiscussionPromptAsksForCheckedAndUncheckedClaims(t *testing.T) {
	r := &Room{Title: "t", Participants: []*Participant{{ID: "a", Label: "A", Enabled: true}, {ID: "b", Label: "B", Enabled: true}}}
	prompt := BuildPrompt(r, r.Participants[0], nil, turnContext{Reason: "discussion", Round: 3, TotalRounds: 3, BlindFirst: true})
	for _, want := range []string{"확인한 근거: …", "확인하지 못한 주장: …", "AGREE는 의견이 같다는 뜻일 뿐 검증이 아닙니다"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "블라인드") || strings.Contains(prompt, "공개됐습니다") {
		t.Fatalf("round 3 is an ordinary sequential round:\n%s", prompt)
	}
}

func TestBlindFirstRoundConcludeStillReveals(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if err := h.StartDiscussion(r.ID, 2, nil); err != nil {
		t.Fatal(err)
	}
	a1, b1 := nextCall(t, ps[0]), nextCall(t, ps[1])
	finishCall(a1, "claude-blind\n[[DISCUSSION:CONCLUDE]]")
	finishCall(b1, "codex-blind\n[[DISCUSSION:AGREE]]")
	a2 := nextCall(t, ps[0])
	if !strings.Contains(a2.opts.Prompt, "codex-blind") {
		t.Fatalf("a blind round-1 CONCLUDE must not skip the reveal round:\n%s", a2.opts.Prompt)
	}
	finishCall(a2, "[[DISCUSSION:CONCLUDE]]")
	b2 := nextCall(t, ps[1])
	finishCall(b2, "[[DISCUSSION:AGREE]]")
	snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return s.Discussion == nil && len(s.Queue) == 0 })
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, "1라운드에서 결론") {
			t.Fatal("discussion ended in the blind round")
		}
	}
}
