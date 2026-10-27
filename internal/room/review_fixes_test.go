package room

import (
	"strings"
	"testing"
	"time"
)

// Findings of the Argon cross-review (room 505eff128e48) on the folder lock,
// limit hold and forward changes. Each test fails on the reviewed commits.

func TestWorkspaceRivalDeletedWakesWaiters(t *testing.T) {
	h, _, ps := controlledHub(t, "codex")
	dir := t.TempDir()
	writer := roomIn(t, h, "작성 방", "write", dir)
	reviewer := roomIn(t, h, "검수 방", "read", dir)
	if _, err := h.PostUser(writer.ID, "write it", ""); err != nil {
		t.Fatal(err)
	}
	running := nextCall(t, ps[0])
	h.mu.Lock()
	rs := h.rooms[reviewer.ID]
	m := rs.nextMessage("user", "user", "queued review")
	rs.enqueue(&Turn{ID: newID(), Agent: "codex", Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
	h.pump(rs)
	h.mu.Unlock()
	noMoreCalls(t, ps[0])
	if err := h.DeleteRoom(writer.ID); err != nil {
		t.Fatal(err)
	}
	<-running.ctx.Done()
	review := nextCall(t, ps[0]) // the folder is free once the busy room is gone
	finishCall(review, "reviewed")
	waitIdle(t, h, reviewer.ID)
}

func TestWorkspaceRivalIgnoresTurnsOutsideTheRoomFolder(t *testing.T) {
	h, _, _ := controlledHub(t, "codex")
	dir := t.TempDir()
	a := roomIn(t, h, "A", "write", dir)
	b := roomIn(t, h, "B", "read", dir)
	h.mu.Lock()
	defer h.mu.Unlock()
	ra, rb := h.rooms[a.ID], h.rooms[b.ID]
	for reason, busy := range map[string]bool{"part": false, "review": false, "mention": true} {
		ra.running = map[string]*running{"t": {turn: &Turn{ID: "t", Agent: "codex", Reason: reason}}}
		if got := h.workspaceRival(rb) != nil; got != busy {
			t.Errorf("%s turn in the other room: busy=%v", reason, got)
		}
	}
	ra.running = map[string]*running{}
}

func TestLimitHoldRestoredAfterRestart(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	h.mu.Lock()
	rs := h.rooms[r.ID]
	p := rs.room.participant("codex")
	p.HoldUntil, p.HoldReason = time.Now().Add(300*time.Millisecond).UnixMilli(), "429 earlier"
	m := rs.nextMessage("user", "user", "waiting")
	rs.enqueue(&Turn{ID: newID(), Agent: "codex", Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
	h.pump(rs)
	h.restoreHolds(rs) // what NewHub does for every loaded room
	h.mu.Unlock()
	finishCall(nextCall(t, ps[0]), "ok") // started by the restored timer
	waitIdle(t, h, r.ID)

	h.mu.Lock()
	p.HoldUntil, p.HoldReason = time.Now().Add(-time.Minute).UnixMilli(), "old"
	h.restoreHolds(rs)
	cleared := p.HoldUntil == 0 && p.HoldReason == ""
	h.mu.Unlock()
	if !cleared {
		t.Fatal("an expired hold must be cleared on start")
	}
}

func TestLimitHoldClearsWithEmptyQueue(t *testing.T) {
	h, r, _ := controlledHub(t, "codex")
	h.mu.Lock()
	rs := h.rooms[r.ID]
	p := rs.room.participant("codex")
	h.holdSeat(rs, p, time.Now().Add(150*time.Millisecond), "429 quick", false)
	h.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		done := p.HoldUntil == 0 && p.HoldReason == ""
		h.mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("an ended hold must be cleared even when nothing waits")
}

func TestSequentialDiscussionWaitsForHeldSeat(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	h.mu.Lock()
	p := h.rooms[r.ID].room.participant("codex")
	p.HoldUntil, p.HoldReason = time.Now().Add(400*time.Millisecond).UnixMilli(), "429"
	h.restoreHolds(h.rooms[r.ID])
	h.mu.Unlock()
	blind := false
	if err := h.StartDiscussion(r.ID, 1, &blind); err != nil {
		t.Fatal(err)
	}
	noMoreCalls(t, ps[1]) // claude speaks after codex, even while codex is held
	finishCall(nextCall(t, ps[0]), "first\nAGREE")
	finishCall(nextCall(t, ps[1]), "second\nAGREE")
	waitIdle(t, h, r.ID)
}

func TestClassifyLimitEdges(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		text  string
		hold  bool
		after time.Duration
	}{
		{"claude 실행 실패: exit status 1: foo.go:429: unexpected token", false, 0},
		{"pid 429 exited", false, 0},
		{"context limit reached", false, 0},
		{"max_turns limit reached", false, 0},
		{"HTTP 429 Too Many Requests", true, defaultLimitHold},
		{"status code 429", true, defaultLimitHold},
		{"daily usage limit reached", true, defaultLimitHold},
		{"rate limited, try again in 1.5s", true, 1500 * time.Millisecond},
		{"rate limited, try again in 500ms", true, minLimitHold},
		{"rate limited, try again in 2m30s", true, 150 * time.Second},
		{"사용 한도 초과(429): 3분 후 다시 시도", true, 3 * time.Minute},
		{"rate limit; retry-after: 0", true, minLimitHold},
		{"rate limit; retry after 99999999999999 hours", true, maxLimitHold},
	} {
		hold, until, _, _ := classifyLimit(tc.text, now)
		if hold != tc.hold || (hold && !until.Equal(now.Add(tc.after))) {
			t.Errorf("%q: hold=%v after=%v", tc.text, hold, until.Sub(now))
		}
	}
}

// Forwarding to a seat in the middle of a turn must not steer the bare text in
// (losing the source and note); the forward goes to the seat's next turn.
func TestForwardToRunningSeatKeepsSource(t *testing.T) {
	h, src, ps := controlledHub(t, "codex", "claude")
	ps[1].steer, ps[1].drain = true, true
	dst, _ := h.CreateRoom(RoomSpec{Title: "받는 방", Mode: "chat", Participants: []string{"claude"}})
	if _, err := h.PostUser(src.ID, "검수", "codex"); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "판정: 승인")
	waitIdle(t, h, src.ID)
	snap, _ := h.Snapshot(src.ID)
	seq := agentMsg(t, snap, "codex", "done").Seq
	if _, err := h.PostUser(dst.ID, "busy", "claude"); err != nil {
		t.Fatal(err)
	}
	busy := nextCall(t, ps[1])
	m, err := h.Forward(dst.ID, src.ID, seq, "claude", "참고해")
	if err != nil {
		t.Fatal(err)
	}
	if m.Live {
		t.Fatal("a forward must not be steered into the running turn")
	}
	finishCall(busy, "done")
	next := nextCall(t, ps[1])
	if !strings.Contains(next.opts.Prompt, "전달: 방") || !strings.Contains(next.opts.Prompt, "참고해") || !strings.Contains(next.opts.Prompt, "판정: 승인") {
		t.Fatalf("%s", next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, dst.ID)
}

// Without a target the forward goes to every active seat; a quoted @mention
// must not narrow it.
func TestForwardBroadcastIgnoresQuotedMentions(t *testing.T) {
	h, src, ps := controlledHub(t, "codex", "claude")
	dst, _ := h.CreateRoom(RoomSpec{Title: "받는 방", Mode: "chat", Participants: []string{"codex", "claude"}})
	if _, err := h.PostUser(src.ID, "검수", "codex"); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "@claude 만 봐 줘")
	finishCall(nextCall(t, ps[1]), "봤어") // the source room's own mention
	waitIdle(t, h, src.ID)
	snap, _ := h.Snapshot(src.ID)
	seq := agentMsg(t, snap, "codex", "done").Seq
	m, err := h.Forward(dst.ID, src.ID, seq, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Mentions == nil || len(*m.Mentions) != 0 {
		t.Fatalf("quoted mentions must not be recorded: %v", m.Mentions)
	}
	finishCall(nextCall(t, ps[0]), "codex got it")
	finishCall(nextCall(t, ps[1]), "claude got it")
	waitIdle(t, h, dst.ID)
}
