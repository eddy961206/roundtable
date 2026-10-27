package room

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClassifyLimit(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		text      string
		hold      bool
		after     time.Duration
		estimated bool
	}{
		{"OpenCode 오류(provider.free-limit, 429): Free usage limit reached. Try again in 2 minutes", true, 2 * time.Minute, false},
		{"API error: 429 Too Many Requests; retry-after: 30", true, 30 * time.Second, false},
		{"rate_limit_error: Rate limit exceeded, please retry after 90s", true, 90 * time.Second, false},
		{"You've hit your usage limit. Try again in 1 hour", true, time.Hour, false},
		{"Quota exceeded for model gemini", true, defaultLimitHold, true},
		{"RESOURCE_EXHAUSTED: try again in 48h", true, maxLimitHold, false},
		{"stream idle timeout (5m0s): context deadline exceeded", false, 0, false},
		{"exit status 1: permission denied", false, 0, false},
		{"", false, 0, false},
	} {
		hold, until, _, estimated := classifyLimit(tc.text, now)
		if hold != tc.hold || (hold && (!until.Equal(now.Add(tc.after)) || estimated != tc.estimated)) {
			t.Errorf("%q: hold=%v until=%v estimated=%v", tc.text, hold, until.Sub(now), estimated)
		}
	}
}

// 2026-10-27: an OpenCode seat hit its free limit and every queued turn failed
// the same way. A limit error now holds only that seat until it may retry.
func TestLimitErrorHoldsOnlyThatSeat(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	if _, err := h.PostUser(r.ID, "first", "codex"); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{err: errors.New("429 Too Many Requests: usage limit reached, try again in 2 minutes")}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	p := snap.Room.participant("codex")
	left := time.Until(time.UnixMilli(p.HoldUntil))
	if left < 100*time.Second || left > 2*time.Minute || !strings.Contains(p.HoldReason, "429") {
		t.Fatalf("hold: %v %q", left, p.HoldReason)
	}
	if countSystem(snap, "한도 오류로") != 1 {
		t.Fatalf("no notice: %+v", snap.Messages)
	}
	if _, err := h.PostUser(r.ID, "everyone", ""); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[1]), "claude answers") // the other seat is not held
	noMoreCalls(t, ps[0])
	snap, _ = h.Snapshot(r.ID)
	if len(snap.Queue) != 1 || snap.Queue[0].Agent != "codex" {
		t.Fatalf("the held seat's turn waits in the queue: %+v", snap.Queue)
	}
	// A person targeting the seat retries now.
	if _, err := h.PostUser(r.ID, "retry now", "codex"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "everyone") || !strings.Contains(call.opts.Prompt, "retry now") {
		t.Fatalf("%s", call.opts.Prompt)
	}
	finishCall(call, "ok")
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if p := snap.Room.participant("codex"); p.HoldUntil != 0 || p.HoldReason != "" {
		t.Fatalf("a targeted retry clears the hold: %+v", p)
	}
}

func TestLimitHoldEndsByItself(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "first", "codex"); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{err: errors.New("rate limit: retry after 1s")}
	waitIdle(t, h, r.ID)
	if _, err := h.PostUser(r.ID, "later", ""); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	call := nextCall(t, ps[0]) // started by the hold timer, not by a new message
	if waited := time.Since(start); waited < 500*time.Millisecond {
		t.Fatalf("ran before the retry time: %v", waited)
	}
	finishCall(call, "ok")
	waitIdle(t, h, r.ID)
}
