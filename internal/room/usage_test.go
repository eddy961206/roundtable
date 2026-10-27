package room

import (
	"context"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// Codex input_tokens includes its cached part; Claude reports input, cache
// writes and cache reads separately. Both normalize to the same totals.
func TestRoomUsageNormalizesProviders(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	postQueueMessage(t, h, r.ID, "질문", "claude")
	claude := nextCall(t, ps[0])
	cu := map[string]any{"input_tokens": 10.0, "cache_creation_input_tokens": 990.0, "cache_read_input_tokens": 9000.0, "output_tokens": 50.0, "context_tokens": 10000.0, "session_cost_usd": 0.2}
	claude.emit(agent.Event{Kind: agent.KindUsage, Meta: cu})
	claude.result <- controlledResult{text: "a", usage: cu}
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "질문", "codex")
	codex := nextCall(t, ps[1])
	xu := map[string]any{"input_tokens": 30000.0, "cached_input_tokens": 24000.0, "output_tokens": 70.0, "context_tokens": 18000.0}
	codex.emit(agent.Event{Kind: agent.KindUsage, Meta: xu})
	codex.result <- controlledResult{text: "b", usage: xu}
	waitIdle(t, h, r.ID)

	u, err := h.RoomUsage(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	seat := map[string]SeatUsage{}
	for _, s := range u.Seats {
		seat[s.ID] = s
	}
	c, x := seat["claude"], seat["codex"]
	if c.Input != 10000 || c.Cached != 9000 || c.Uncached != 1000 || c.Output != 50 || c.MaxContext != 10000 || c.Turns != 1 || c.CostUSD < 0.1999 || c.CostUSD > 0.2001 {
		t.Fatalf("claude usage %+v", c.UsageTotals)
	}
	if x.Input != 30000 || x.Cached != 24000 || x.Uncached != 6000 || x.Output != 70 || x.MaxContext != 18000 {
		t.Fatalf("codex usage %+v", x.UsageTotals)
	}
	if u.Totals.Input != 40000 || u.Totals.Uncached != 7000 || u.Totals.Turns != 2 || u.Totals.MaxContext != 18000 {
		t.Fatalf("room totals %+v", u.Totals)
	}
	if _, err := h.RoomUsage("nope"); err == nil {
		t.Fatal("unknown room")
	}
}

func TestCloseWhenIdleWaitsForTurnsAndRefusesLaterPosts(t *testing.T) {
	h, r, ps := controlledHub(t, "claude")
	postQueueMessage(t, h, r.ID, "긴 작업", "claude")
	running := nextCall(t, ps[0])

	short, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	closed, err := h.CloseWhenIdle(short)
	cancel()
	if closed || err == nil || !strings.Contains(err.Error(), "종료하지 않음") {
		t.Fatalf("busy hub must keep serving: %v %v", closed, err)
	}
	if _, err := h.PostUser(r.ID, "아직 받음", "claude"); err != nil {
		t.Fatalf("a timed-out close must not refuse posts: %v", err)
	}

	result := make(chan error, 1)
	go func() {
		closed, err := h.CloseWhenIdle(context.Background())
		if !closed && err == nil {
			err = context.Canceled
		}
		result <- err
	}()
	finishCall(running, "끝")
	finishCall(nextCall(t, ps[0]), "둘째 끝") // the queued post still runs before closing
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idle hub did not close")
	}
	if _, err := h.PostUser(r.ID, "늦은 질문", ""); err == nil || !strings.Contains(err.Error(), "종료 중") {
		t.Fatalf("posts after close: %v", err)
	}
	snap, _ := h.Snapshot(r.ID)
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Status != "done" {
			t.Fatalf("idle close cancelled a turn: %+v", m)
		}
	}
}
