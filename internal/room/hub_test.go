package room

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// fakeProvider scripts replies and records what it was asked.
type fakeProvider struct {
	id      string
	mu      sync.Mutex
	prompts []string
	steers  []string
	reply   func(n int, prompt string) string
	delay   time.Duration
	steer   bool
	drain   bool // read steer messages while delaying (a real CLI would)
	fail    error
}

func (f *fakeProvider) ID() string      { return f.id }
func (f *fakeProvider) Label() string   { return strings.ToUpper(f.id) }
func (f *fakeProvider) Command() string { return f.id }
func (f *fakeProvider) Models() []agent.ModelOption {
	return []agent.ModelOption{{ID: "m", Label: "m"}}
}
func (f *fakeProvider) Available() bool          { return true }
func (f *fakeProvider) CanSteer() bool           { return f.steer }
func (f *fakeProvider) ContextWindow(string) int { return 1000 }
func (f *fakeProvider) calls() int               { f.mu.Lock(); defer f.mu.Unlock(); return len(f.prompts) }
func (f *fakeProvider) Run(ctx context.Context, o agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, o.Prompt)
	n := len(f.prompts)
	f.mu.Unlock()
	steered := 0
	emit(agent.Event{Kind: agent.KindSession, Text: f.id + "-session"})
	emit(agent.Event{Kind: agent.KindLimits, Meta: map[string]any{"limits": agent.Limits{Provider: f.id, At: 1, Windows: []agent.LimitWindow{{Label: "5시간", UsedPercent: 42}}}}})
	if f.delay > 0 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return agent.Result{}, ctx.Err()
			case s := <-steerCh(o.Steer, f.drain):
				f.mu.Lock()
				f.steers = append(f.steers, s)
				f.mu.Unlock()
				steered++
				continue
			case <-timer.C:
			}
			break
		}
	}
	if f.fail != nil {
		return agent.Result{}, f.fail
	}
	text := "답변 " + f.id
	if f.reply != nil {
		text = f.reply(n, o.Prompt)
	}
	emit(agent.Event{Kind: agent.KindTextDelta, Text: text})
	emit(agent.Event{Kind: agent.KindUsage, Meta: map[string]any{"input_tokens": 100.0, "output_tokens": 10.0, "context_tokens": 300.0}})
	return agent.Result{Text: text, SessionID: f.id + "-session", Usage: map[string]any{"input_tokens": 100.0, "output_tokens": 10.0}, Steered: steered}, nil
}

func steerCh(ch <-chan string, drain bool) <-chan string {
	if drain {
		return ch
	}
	return nil
}

func newTestHub(t *testing.T, provs ...*fakeProvider) (*Hub, *Room) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var ps []agent.Provider
	var ids []string
	for _, p := range provs {
		ps = append(ps, p)
		ids = append(ids, p.id)
	}
	hub, err := NewHub(store, agent.NewRegistry(ps...), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	r, err := hub.CreateRoom(RoomSpec{Title: "t", Mode: agent.ModeChat, Participants: ids, MaxChain: 2, DiscussionRounds: 2})
	if err != nil {
		t.Fatal(err)
	}
	return hub, r
}

func waitIdle(t *testing.T, hub *Hub, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := hub.Snapshot(id)
		busy := len(snap.Queue) > 0
		for _, m := range snap.Messages {
			if m.Status == "streaming" {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("hub did not become idle")
}

func agentTexts(snap *Snapshot, author string) []string {
	var out []string
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Author == author {
			out = append(out, m.Status+":"+m.Text)
		}
	}
	return out
}

func TestBroadcastAndDelta(t *testing.T) {
	a, b := &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	if _, err := hub.PostUser(r.ID, "첫 질문", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	if got := agentTexts(snap, "claude"); len(got) != 1 || got[0] != "done:답변 claude" {
		t.Fatalf("claude messages %v", got)
	}
	if got := agentTexts(snap, "codex"); len(got) != 1 {
		t.Fatalf("codex messages %v", got)
	}
	if !strings.Contains(a.prompts[0], "[User] 첫 질문") || !strings.Contains(a.prompts[0], "지금까지의 대화") {
		t.Fatalf("first prompt should carry the whole transcript: %s", a.prompts[0])
	}
	p := snap.Room.participant("claude")
	if p.SessionID != "claude-session" || p.Totals.Input != 100 || p.ContextTokens != 300 || p.ContextWindow != 1000 {
		t.Fatalf("participant bookkeeping wrong: %+v", p)
	}

	// second message: only new lines are delivered, and the agent's own reply is excluded
	hub.PostUser(r.ID, "둘째 질문", "")
	waitIdle(t, hub, r.ID)
	second := a.prompts[1]
	_, hist, ok := strings.Cut(second, "새로 올라온 메시지 ===")
	if !ok || strings.Contains(hist, "[User] 첫 질문") || strings.Count(hist, "[@codex] 답변 codex") != 1 || strings.Contains(hist, "[@claude]") || !strings.Contains(hist, "[User] 둘째 질문") {
		t.Fatalf("delta prompt wrong:\n%s", second)
	}
}

func TestMentionRoutingAndChainLimit(t *testing.T) {
	a := &fakeProvider{id: "claude", reply: func(n int, _ string) string { return "@codex 확인해줘" }}
	b := &fakeProvider{id: "codex", reply: func(n int, _ string) string { return "@claude 다시 봐" }}
	c := &fakeProvider{id: "agy"}
	hub, r := newTestHub(t, a, b, c)
	hub.PostUser(r.ID, "@claude 만 답해", "")
	waitIdle(t, hub, r.ID)
	if c.calls() != 0 {
		t.Fatal("agy was not mentioned and must not run")
	}
	// claude(depth0) -> codex(depth1) -> claude(depth2) -> stop at max_chain=2
	if a.calls() != 2 || b.calls() != 1 {
		t.Fatalf("chain calls claude=%d codex=%d", a.calls(), b.calls())
	}
	snap, _ := hub.Snapshot(r.ID)
	var limited bool
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, "연쇄 한도") {
			limited = true
		}
	}
	if !limited {
		t.Fatal("chain limit notice missing")
	}
}

func TestDisabledParticipantSkipped(t *testing.T) {
	a, b := &fakeProvider{id: "claude"}, &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	off := false
	hub.UpdateRoom(r.ID, RoomPatch{Participants: []ParticipantPatch{{ID: "codex", Enabled: &off}}})
	hub.PostUser(r.ID, "모두 답해", "")
	waitIdle(t, hub, r.ID)
	if b.calls() != 0 || a.calls() != 1 {
		t.Fatalf("calls claude=%d codex=%d", a.calls(), b.calls())
	}
	hub.PostUser(r.ID, "@codex 너는?", "")
	waitIdle(t, hub, r.ID)
	if b.calls() != 0 {
		t.Fatal("disabled agent must ignore mentions too")
	}
}

func TestDiscussionEndsOnAgreement(t *testing.T) {
	a := &fakeProvider{id: "claude", reply: func(n int, _ string) string { return "의견 A\n[[DISCUSSION:CONTINUE]]" }}
	b := &fakeProvider{id: "codex", reply: func(n int, _ string) string {
		if n == 1 {
			return "반박\n[[DISCUSSION:CONTINUE]]"
		}
		return "[[DISCUSSION:AGREE]]"
	}}
	hub, r := newTestHub(t, a, b)
	a.reply = func(n int, _ string) string {
		if n == 1 {
			return "의견 A\n[[DISCUSSION:CONTINUE]]"
		}
		return "동의\n[[DISCUSSION:AGREE]]"
	}
	if err := hub.StartDiscussion(r.ID, 3); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	if snap.Discussion != nil {
		t.Fatal("discussion should be over")
	}
	if a.calls() != 2 || b.calls() != 2 {
		t.Fatalf("expected two rounds, got claude=%d codex=%d", a.calls(), b.calls())
	}
	var ended bool
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Round == 2 && m.Signal != "AGREE" {
			t.Fatalf("round-2 signal %q text %q", m.Signal, m.Text)
		}
		if m.Kind == "agent" && strings.Contains(m.Text, "[[DISCUSSION") {
			t.Fatal("marker must be stripped from the chat text")
		}
		if m.Kind == "system" && strings.Contains(m.Text, "전원 동의") {
			ended = true
		}
	}
	if !ended {
		t.Fatal("end-of-discussion notice missing")
	}
	// discussion turns are sequential: codex's round-1 prompt must contain claude's round-1 reply
	if !strings.Contains(b.prompts[0], "[@claude] 의견 A") {
		t.Fatalf("sequential delivery broken:\n%s", b.prompts[0])
	}
}

func TestStopCancelsRunning(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 2 * time.Second}
	b := &fakeProvider{id: "codex"}
	hub, r := newTestHub(t, a, b)
	hub.PostUser(r.ID, "오래 걸리는 일", "")
	time.Sleep(100 * time.Millisecond)
	if err := hub.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	got := agentTexts(snap, "claude")
	if len(got) != 1 || !strings.HasPrefix(got[0], "cancelled:") {
		t.Fatalf("claude message should be cancelled: %v", got)
	}
}

func TestSteerLiveAndQueued(t *testing.T) {
	a := &fakeProvider{id: "claude", delay: 600 * time.Millisecond, steer: true, drain: true}
	b := &fakeProvider{id: "codex", delay: 600 * time.Millisecond}
	hub, r := newTestHub(t, a, b)
	hub.PostUser(r.ID, "시작", "")
	time.Sleep(100 * time.Millisecond)
	m1, err := hub.PostUser(r.ID, "방향 바꿔", "claude")
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := hub.PostUser(r.ID, "너도 바꿔", "codex")
	if !m1.Live || m1.Target != "claude" {
		t.Fatalf("claude steer should be live: %+v", m1)
	}
	if m2.Live {
		t.Fatal("codex cannot steer live; must be queued")
	}
	waitIdle(t, hub, r.ID)
	if len(a.steers) != 1 || a.steers[0] != "방향 바꿔" {
		t.Fatalf("steer not delivered: %v", a.steers)
	}
	if a.calls() != 1 {
		t.Fatalf("live steer must not create another claude turn, calls=%d", a.calls())
	}
	if b.calls() != 2 || !strings.Contains(b.prompts[1], "[User] 너도 바꿔") || strings.Contains(b.prompts[1], "방향 바꿔") {
		t.Fatalf("queued steer wrong: calls=%d prompt=%s", b.calls(), b.prompts[len(b.prompts)-1])
	}
}

func TestErrorTurnIsRecorded(t *testing.T) {
	a := &fakeProvider{id: "claude", fail: context.DeadlineExceeded}
	hub, r := newTestHub(t, a)
	hub.PostUser(r.ID, "x", "")
	waitIdle(t, hub, r.ID)
	snap, _ := hub.Snapshot(r.ID)
	got := agentTexts(snap, "claude")
	if len(got) != 1 || !strings.HasPrefix(got[0], "error:") {
		t.Fatalf("got %v", got)
	}
}

func TestFailedTurnRedeliversMessages(t *testing.T) {
	a := &fakeProvider{id: "claude", fail: context.DeadlineExceeded}
	hub, r := newTestHub(t, a)
	hub.PostUser(r.ID, "첫 질문", "")
	waitIdle(t, hub, r.ID)
	a.fail = nil
	hub.PostUser(r.ID, "둘째 질문", "")
	waitIdle(t, hub, r.ID)
	if a.calls() != 2 {
		t.Fatalf("calls %d", a.calls())
	}
	second := a.prompts[1]
	if !strings.Contains(second, "[User] 첫 질문") || !strings.Contains(second, "[User] 둘째 질문") {
		t.Fatalf("messages of the failed turn must be redelivered: %s", second)
	}
	snap, _ := hub.Snapshot(r.ID)
	if got := agentTexts(snap, "claude"); len(got) != 2 || !strings.HasPrefix(got[0], "error:") || got[1] != "done:답변 claude" {
		t.Fatalf("got %v", got)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	a := &fakeProvider{id: "claude"}
	store, _ := NewStore(t.TempDir())
	hub, _ := NewHub(store, agent.NewRegistry(a), t.TempDir(), log.New(io.Discard, "", 0))
	r, _ := hub.CreateRoom(RoomSpec{Title: "p", Mode: agent.ModeChat, Participants: []string{"claude"}})
	hub.PostUser(r.ID, "저장되나", "")
	waitIdle(t, hub, r.ID)
	hub2, err := NewHub(store, agent.NewRegistry(a), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := hub2.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 2 || snap.Messages[1].Text != "답변 claude" {
		t.Fatalf("messages after reload: %+v", snap.Messages)
	}
	if snap.Room.participant("claude").SessionID != "claude-session" {
		t.Fatal("session id must persist")
	}
	if len(hub2.Trace(r.ID, snap.Messages[1].ID)) == 0 {
		t.Fatal("trace must persist")
	}
	if l := hub2.Limits()["claude"]; len(l.Windows) != 1 || l.Windows[0].UsedPercent != 42 {
		t.Fatalf("limits must persist across restarts: %+v", l)
	}
	md, _ := hub2.ExportMarkdown(r.ID)
	if !strings.Contains(md, "저장되나") || !strings.Contains(md, "답변 claude") {
		t.Fatal("export missing content")
	}
}

func TestSubscribeReceivesEvents(t *testing.T) {
	a := &fakeProvider{id: "claude"}
	hub, r := newTestHub(t, a)
	ch, unsub, err := hub.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	hub.PostUser(r.ID, "이벤트", "")
	waitIdle(t, hub, r.ID)
	types := map[string]int{}
	timeout := time.After(time.Second)
	want := []string{"message", "delta", "trace", "participant", "queue", "limits"}
	have := func() bool {
		for _, w := range want {
			if types[w] == 0 {
				return false
			}
		}
		return true
	}
	for !have() {
		select {
		case b := <-ch:
			var ev struct{ Type string }
			_ = json.Unmarshal(b, &ev)
			types[ev.Type]++
		case <-timeout:
			t.Fatalf("event types so far: %v", types)
		}
	}
	for _, w := range want {
		if types[w] == 0 {
			t.Errorf("missing event type %s (got %v)", w, types)
		}
	}
}
