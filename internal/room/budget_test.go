package room

import (
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func countSystem(snap *Snapshot, part string) int {
	n := 0
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, part) {
			n++
		}
	}
	return n
}

func ref[T any](v T) *T { return &v }

func runOnce(t *testing.T, h *Hub, r *Room, p *controlledProvider, text string, usage map[string]any) *controlledCall {
	t.Helper()
	if _, err := h.PostUser(r.ID, text, "codex"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, p)
	call.emit(agent.Event{Kind: agent.KindUsage, Meta: usage}) // as the CLI parsers do
	call.result <- controlledResult{text: "ok", usage: usage}
	waitIdle(t, h, r.ID)
	return call
}

// 2026-10-27: an agy seat kept one session for the whole day and read over 80M
// input tokens before anyone noticed. A room can now warn at a level and cap a
// seat or the whole room; a capped seat does not start turns until the cap rises.
func TestSeatInputWarnAndBudgetHold(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{InputWarnTokens: ref[int64](1000), SeatInputBudget: ref[int64](1500)}); err != nil {
		t.Fatal(err)
	}
	runOnce(t, h, r, ps[0], "first", map[string]any{"input_tokens": 1200.0, "cached_input_tokens": 1000.0, "output_tokens": 1.0})
	snap, _ := h.Snapshot(r.ID)
	if got := snap.Room.participant("codex").InputAll; got != 1200 {
		t.Fatalf("normalized input (codex input includes its cache): %v", got)
	}
	if countSystem(snap, "누적 입력") != 1 {
		t.Fatalf("no warning at 1,000: %+v", snap.Messages)
	}
	runOnce(t, h, r, ps[0], "second", map[string]any{"input_tokens": 400.0})
	snap, _ = h.Snapshot(r.ID)
	if countSystem(snap, "누적 입력") != 1 {
		t.Fatal("warned twice for the same level")
	}
	if _, err := h.PostUser(r.ID, "third task", "codex"); err != nil {
		t.Fatal(err)
	}
	noMoreCalls(t, ps[0])
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if countSystem(snap, "예산 상한") != 1 || countSystem(snap, "1,600") != 1 {
		t.Fatalf("hold must be explained with the numbers: %+v", snap.Messages)
	}
	if _, err := h.PostUser(r.ID, "still held", "codex"); err != nil {
		t.Fatal(err)
	}
	noMoreCalls(t, ps[0])
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if countSystem(snap, "예산 상한") != 1 {
		t.Fatal("the same hold must not be announced on every message")
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{SeatInputBudget: ref[int64](5000)}); err != nil {
		t.Fatal(err)
	}
	call := runOnce(t, h, r, ps[0], "fourth", map[string]any{"input_tokens": 10.0})
	for _, s := range []string{"third task", "still held", "fourth"} {
		if !strings.Contains(call.opts.Prompt, s) {
			t.Fatalf("held messages must be delivered once the cap rises; missing %q: %s", s, call.opts.Prompt)
		}
	}
}

func TestRoomInputBudgetHoldsEverySeat(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{RoomInputBudget: ref[int64](100)}); err != nil {
		t.Fatal(err)
	}
	runOnce(t, h, r, ps[0], "first", map[string]any{"input_tokens": 150.0})
	if _, err := h.PostUser(r.ID, "to claude", "claude"); err != nil {
		t.Fatal(err)
	}
	noMoreCalls(t, ps[1])
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if countSystem(snap, "방 누적 입력") != 1 {
		t.Fatalf("%+v", snap.Messages)
	}
}

func TestContextWarnOncePerCrossing(t *testing.T) {
	h, r, ps := controlledHub(t, "codex") // fake context window: 1000 tokens
	for i, ctx := range []float64{800, 850, 300, 750} {
		runOnce(t, h, r, ps[0], "turn "+string(rune('a'+i)), map[string]any{"input_tokens": 1.0, "context_tokens": ctx})
	}
	snap, _ := h.Snapshot(r.ID)
	if got := countSystem(snap, "문맥이 모델 한도의"); got != 2 {
		t.Fatalf("want a warning at 80%% and again at 75%% after compaction to 30%%, got %d: %+v", got, snap.Messages)
	}
	if countSystem(snap, "80%") != 1 || countSystem(snap, "75%") != 1 {
		t.Fatal("the warning names the ratio")
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{ContextWarnPercent: ref(-1)}); err != nil {
		t.Fatal(err)
	}
	runOnce(t, h, r, ps[0], "after off", map[string]any{"input_tokens": 1.0, "context_tokens": 300.0})
	runOnce(t, h, r, ps[0], "after off 2", map[string]any{"input_tokens": 1.0, "context_tokens": 990.0})
	snap, _ = h.Snapshot(r.ID)
	if got := countSystem(snap, "문맥이 모델 한도의"); got != 2 {
		t.Fatalf("-1 turns the warning off: %d", got)
	}
}

func TestBudgetSettingsValidated(t *testing.T) {
	h, r, _ := controlledHub(t, "codex")
	for _, p := range []RoomPatch{
		{SeatInputBudget: ref[int64](-5)},
		{RoomInputBudget: ref[int64](-1)},
		{InputWarnTokens: ref[int64](-2)},
		{ContextWarnPercent: ref(100)},
		{ContextWarnPercent: ref(-3)},
	} {
		if _, err := h.UpdateRoom(r.ID, p); err == nil {
			t.Errorf("accepted %+v", p)
		}
	}
}

func TestInputAllBackfilledFromHistory(t *testing.T) {
	h, r, _ := controlledHub(t, "codex")
	rs := h.rooms[r.ID]
	m := rs.nextMessage("agent", "codex", "old")
	m.Usage = map[string]any{"input_tokens": 70.0}
	m2 := rs.nextMessage("agent", "codex", "old2")
	m2.Usage = map[string]any{"input_tokens": 30.0}
	p := rs.room.participant("codex")
	p.InputAll = 0
	backfillInput(rs)
	if p.InputAll != 100 {
		t.Fatalf("rooms saved before input_all existed are counted from history: %v", p.InputAll)
	}
}
