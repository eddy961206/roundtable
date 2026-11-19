package room

import (
	"strings"
	"testing"
	"time"
)

// 2026-11-20: a usage watcher posted a one-line report to the orchestrator
// seat every two minutes, and each post started a turn on a huge context that
// only answered "확인". PostNotice records the line without waking the seat.
func TestNoticeStartsNoTurnAndIsReadNextTurn(t *testing.T) {
	a, b := &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	n1, err := hub.PostNotice(r.ID, "사용량 41%", "claude")
	if err != nil {
		t.Fatal(err)
	}
	// An untargeted notice with an @mention calls nobody either.
	if _, err := hub.PostNotice(r.ID, "@codex 참고: 빌드 통과", ""); err != nil {
		t.Fatal(err)
	}
	time.Sleep(150 * time.Millisecond)
	waitIdle(t, hub, r.ID)
	if a.calls() != 0 || b.calls() != 0 {
		t.Fatalf("notice must not start a turn: claude=%d codex=%d", a.calls(), b.calls())
	}
	snap, _ := hub.Snapshot(r.ID)
	if len(snap.Queue) != 0 {
		t.Fatalf("notice queued a turn: %+v", snap.Queue)
	}
	var kept *Message
	for _, m := range snap.Messages {
		if m.ID == n1.ID {
			kept = m
		}
	}
	if kept == nil || !kept.Notice || kept.Kind != "user" || kept.Target != "claude" || kept.Delivery != nil || kept.Live {
		t.Fatalf("notice not recorded as a plain record: %+v", kept)
	}

	// The next ordinary turn reads both notices through the usual history.
	if _, err := hub.PostUser(r.ID, "다음 질문", "claude"); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	if a.calls() != 1 || b.calls() != 0 {
		t.Fatalf("ordinary post must start exactly the target turn: claude=%d codex=%d", a.calls(), b.calls())
	}
	p := a.prompts[0]
	if !strings.Contains(p, "User (알림, 답 불필요)] 사용량 41%") || !strings.Contains(p, "참고: 빌드 통과") || !strings.Contains(p, "] 다음 질문") {
		t.Fatalf("next turn should read the notices:\n%s", p)
	}

	// Persisted: a reloaded hub still sees the flag.
	reloaded, err := NewHub(hub.store, hub.reg, t.TempDir(), hub.Log)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := reloaded.Snapshot(r.ID)
	found := false
	for _, m := range again.Messages {
		found = found || (m.ID == n1.ID && m.Notice)
	}
	if !found {
		t.Fatal("notice flag not persisted")
	}
}

// A running seat that can steer gets ordinary posts injected; a notice must
// neither be injected nor queue a follow-up turn.
func TestNoticeIsNotInjectedIntoRunningTurn(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 500 * time.Millisecond, steer: true, drain: true}
	hub, r := newTestHub(t, a)
	if _, err := hub.PostUser(r.ID, "시작", "claude"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	n, err := hub.PostNotice(r.ID, "사용량 42%", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if n.Delivery != nil || n.Live || n.Part != "" {
		t.Fatalf("notice must carry no delivery: %+v", n)
	}
	waitIdle(t, hub, r.ID)
	if len(a.steers) != 0 || a.calls() != 1 {
		t.Fatalf("notice woke the seat: steers=%v calls=%d", a.steers, a.calls())
	}
	if strings.Contains(a.prompts[0], "사용량 42%") {
		t.Fatal("notice after the turn's cap leaked into the running turn")
	}
	// The ordinary post is unchanged: it still injects into a running turn.
	if _, err := hub.PostUser(r.ID, "다음", "claude"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	steer, err := hub.PostUser(r.ID, "방향 바꿔", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if steer.Delivery == nil || steer.Delivery.State != deliveryPending {
		t.Fatalf("ordinary post during a turn should be injected: %+v", steer)
	}
	waitIdle(t, hub, r.ID)
	if a.calls() != 2 || len(a.steers) != 1 || a.steers[0] != "방향 바꿔" {
		t.Fatalf("ordinary post changed: calls=%d steers=%v", a.calls(), a.steers)
	}
	if !strings.Contains(a.prompts[1], "사용량 42%") {
		t.Fatalf("next turn should read the notice:\n%s", a.prompts[1])
	}
}

func TestNoticeRejectsRemovedTargetAndEmptyText(t *testing.T) {
	hub, r := newTestHub(t, &fakeProvider{id: "claude"})
	if _, err := hub.PostNotice(r.ID, "x", "nobody"); err == nil {
		t.Fatal("unknown target accepted")
	}
	if _, err := hub.PostNotice(r.ID, "  ", ""); err == nil {
		t.Fatal("empty notice accepted")
	}
}
