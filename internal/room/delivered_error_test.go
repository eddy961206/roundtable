package room

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// noMoreCalls fails if the provider starts another turn within a short window.
func noMoreCalls(t *testing.T, p *controlledProvider) {
	t.Helper()
	select {
	case call := <-p.callsCh:
		t.Fatalf("%s ran again with: %s", p.id, call.opts.Prompt)
	case <-time.After(300 * time.Millisecond):
	}
}

// 2026-10-27, room 8a864d16ac29: #25 waited behind a running turn, the targeted
// #26 jumped ahead and its turn (#27) received #25 and #26, did the work, then
// ended with "stream idle timeout". Because the error left the seat's cursor
// behind, #25's own queued turn (#28) ran the same work again.
func TestErroredTurnWithOutputCountsAsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name       string
		text       string // what the failing turn wrote before its error
		lateUser   bool   // a user message arrives while the failing turn runs
		wantRerun  bool
		wantInNext []string
		notInNext  []string
	}{
		{name: "output then error: no rerun", text: "both tasks done"},
		// The waiting turn for "second task" runs again; its cap (#3) ends before "third task".
		{name: "error before any output: messages are delivered again", wantRerun: true, wantInNext: []string{"second task"}},
		{name: "later message still gets its turn", text: "both tasks done", lateUser: true, wantRerun: true, wantInNext: []string{"fourth task"}, notInNext: []string{"second task", "third task"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex")
			if _, err := h.PostUser(r.ID, "first task", ""); err != nil {
				t.Fatal(err)
			}
			first := nextCall(t, ps[0])
			if _, err := h.PostUser(r.ID, "second task", ""); err != nil { // waits as an ordinary turn
				t.Fatal(err)
			}
			if _, err := h.PostUser(r.ID, "third task", "codex"); err != nil { // targeted: jumps ahead
				t.Fatal(err)
			}
			finishCall(first, "first done")
			running := nextCall(t, ps[0])
			if !strings.Contains(running.opts.Prompt, "second task") || !strings.Contains(running.opts.Prompt, "third task") {
				t.Fatalf("the jumping turn must carry both waiting messages: %s", running.opts.Prompt)
			}
			if tc.lateUser {
				if _, err := h.PostUser(r.ID, "fourth task", ""); err != nil {
					t.Fatal(err)
				}
			}
			running.result <- controlledResult{text: tc.text, err: errors.New("stream idle timeout (5m0s)")}
			if !tc.wantRerun {
				noMoreCalls(t, ps[0])
				waitIdle(t, h, r.ID)
				return
			}
			next := nextCall(t, ps[0])
			for _, s := range tc.wantInNext {
				if !strings.Contains(next.opts.Prompt, s) {
					t.Fatalf("next turn lost %q: %s", s, next.opts.Prompt)
				}
			}
			for _, s := range tc.notInNext {
				if strings.Contains(next.opts.Prompt, s) {
					t.Fatalf("next turn repeated %q: %s", s, next.opts.Prompt)
				}
			}
			finishCall(next, "ok")
			noMoreCalls(t, ps[0])
			waitIdle(t, h, r.ID)
		})
	}
}

// A user stop is not a delivery: the stopped turn's messages stay unseen.
func TestCancelledTurnStaysUndelivered(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "first task", ""); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, ps[0])
	snap, _ := h.Snapshot(r.ID)
	var turn string
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Status == "streaming" {
			turn = m.TurnID
		}
	}
	if err := h.CancelTurn(r.ID, turn); err != nil {
		t.Fatal(err)
	}
	<-first.ctx.Done()
	waitIdle(t, h, r.ID)
	if _, err := h.PostUser(r.ID, "second task", ""); err != nil {
		t.Fatal(err)
	}
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "first task") {
		t.Fatalf("a stopped turn's message must be delivered again: %s", next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)
}
