package room

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"roundtable/internal/agent"
)

func TestRetryStartsAPartThatNeverStarted(t *testing.T) {
	repo := newGitRepo(t)
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	author := &controlledProvider{fakeProvider: fakeProvider{id: "claude"}, callsCh: make(chan *controlledCall, 16)}
	reviewer := &controlledProvider{fakeProvider: fakeProvider{id: "codex"}, callsCh: make(chan *controlledCall, 16)}
	reg := agent.NewRegistry(author, reviewer)
	hub, err := NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })

	r, err := hub.CreateRoom(RoomSpec{Title: "work", Mode: agent.ModeWrite, Workspace: repo, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}

	// Hold the author seat via a provider rate-limit error before creating the work plan.
	postQueueMessage(t, hub, r.ID, "hold author", "claude")
	holdCall := nextCall(t, author)
	holdCall.result <- controlledResult{err: errors.New("429 Too Many Requests: usage limit reached, try again in 2 minutes")}
	waitIdle(t, hub, r.ID)

	if _, err := hub.CreateWork(r.ID, workSpec(1, nil)); err != nil {
		t.Fatal(err)
	}
	snap, err := hub.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Queue) != 1 || snap.Queue[0].Reason != "part" || snap.Queue[0].Part != "p1" {
		t.Fatalf("expected queued part turn while author seat is held, got queue=%+v", snap.Queue)
	}

	// SubmitPart fails the submit check (no commits) and blocks p1 (Kind "submit")
	// while its kickoff turn still waits for the held seat.
	plan, err := hub.SubmitPart(r.ID, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if p := plan.part("p1"); p.State != "blocked" || p.Block == nil || p.Block.Kind != "submit" {
		t.Fatalf("expected submit block, got %+v", p)
	}

	// RetryPart returns p1 to "working", so its part turn should be queued or running.
	if _, err := hub.RetryPart(r.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	snap, err = hub.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := snap.Work.part("p1")
	hasPartTurn := p.SessionID != "" || len(author.callsCh) > 0
	for _, turn := range snap.Queue {
		if turn.Reason == "part" && turn.Part == "p1" {
			hasPartTurn = true
		}
	}
	if !hasPartTurn {
		t.Fatalf("part p1 is %s after RetryPart, but has no part turn queued or running (queue=%d, session=%q)", p.State, len(snap.Queue), p.SessionID)
	}
}

func TestBootstrapReplayCountsDiscussionOnce(t *testing.T) {
	repo := newGitRepo(t)
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	author := &controlledProvider{fakeProvider: fakeProvider{id: "claude"}, callsCh: make(chan *controlledCall, 16)}
	reviewer := &controlledProvider{fakeProvider: fakeProvider{id: "codex"}, callsCh: make(chan *controlledCall, 16)}
	reg := agent.NewRegistry(author, reviewer)
	hub, err := NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })

	r, err := hub.CreateRoom(RoomSpec{Title: "work", Mode: agent.ModeWrite, Workspace: repo, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}

	// Keep claude busy with a chat turn so CreateWork's kickoff "part" turn stays queued
	// and part.SessionID remains empty when StartDiscussion is called.
	postQueueMessage(t, hub, r.ID, "busy before plan", "claude")
	preCall := nextCall(t, author)

	if _, err := hub.CreateWork(r.ID, workSpec(1, nil)); err != nil {
		t.Fatal(err)
	}
	if err := hub.StartDiscussion(r.ID, 2, sequentialFirst); err != nil {
		t.Fatal(err)
	}

	// Finishing preCall lets pump run the discussion. Because part.SessionID == "",
	// claude's round-1 discussion turn runs as a part bootstrap first and then replays.
	finishCall(preCall, "pre done")

	bootCall := nextCall(t, author)
	finishCall(bootCall, "부트스트랩 완료\n[[DISCUSSION:CONTINUE]]")

	replayCall := nextCall(t, author)
	finishCall(replayCall, "1라운드 의견\n[[DISCUSSION:CONTINUE]]")

	// Wait until codex's round-1 turn starts, which happens after replayCall's runTurn finishes.
	_ = nextCall(t, reviewer)

	snap, err := hub.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Discussion == nil || snap.Discussion.Round != 1 {
		round := 0
		if snap.Discussion != nil {
			round = snap.Discussion.Round
		}
		t.Fatalf("discussion advanced to round %d before codex finished round 1", round)
	}
}

func TestSubmitCheckAfterShutdownChangesNothing(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	wt := f.part(t, "p1").Worktree
	commitT(t, wt, "done.txt", "done\n")

	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	hook := filepath.Join(dir, "fsmonitor")
	script := fmt.Sprintf("#!/bin/sh\n: > %s\nwhile [ ! -f %s ]; do sleep 0.05; done\nprintf 'token\\000'\n", shellPath(entered), shellPath(release))
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, f.repo, "config", "core.fsmonitor", filepath.ToSlash(hook))
	t.Cleanup(func() { _ = os.WriteFile(release, []byte("release"), 0o600) })

	type submitResult struct {
		plan *WorkPlan
		err  error
	}
	done := make(chan submitResult, 1)
	go func() {
		plan, err := f.hub.SubmitPart(f.room.ID, "p1")
		done <- submitResult{plan, err}
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("SubmitPart did not reach git status fsmonitor hook")
		}
		time.Sleep(10 * time.Millisecond)
	}

	if err := f.hub.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := <-done

	afterSnap, err := f.hub.Snapshot(f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	rooms, _, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	diskState := rooms[0].Work.part("p1").State
	if res.err == nil || afterSnap.Work.part("p1").State != "working" || len(afterSnap.Queue) != 0 || diskState != "working" {
		t.Fatalf("closed hub mutated state on SubmitPart: err=%v state=%s queue=%d diskState=%s", res.err, afterSnap.Work.part("p1").State, len(afterSnap.Queue), diskState)
	}
}
