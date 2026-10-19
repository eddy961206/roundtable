package room

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func markCleanupReady(t *testing.T, f *workFixture) {
	t.Helper()
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	plan := f.hub.rooms[f.room.ID].room.Work
	p := plan.part("p1")
	p.State, p.SubmitSHA, p.MergedSHA = "merged", f.base, f.base
	for _, other := range plan.Parts[1:] {
		other.State = "abandoned"
	}
	if err := f.hub.saveWork(f.hub.rooms[f.room.ID]); err != nil {
		t.Fatal(err)
	}
}

func retryCleanup(t *testing.T, f *workFixture) *WorkPlan {
	t.Helper()
	f.hub.mu.Lock()
	f.hub.startWorktreeCleanup(f.hub.rooms[f.room.ID])
	f.hub.mu.Unlock()
	return waitWorktreeCleanup(t, f)
}

func TestCleanupReviewMovedDataRoot(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	markCleanupReady(t, f)
	oldIntegration := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
	f.hub.workRoot = filepath.Join(t.TempDir(), "worktrees")
	f.store.root = filepath.Dir(f.hub.workRoot)
	plan := retryCleanup(t, f)
	if plan.IntegrationWorktreeRemoved {
		t.Fatal("unvisited old integration worktree recorded as removed")
	}
	assertWorktreeExists(t, oldIntegration, true)
}

func TestCleanupReviewUnregisteredIntegration(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	markCleanupReady(t, f)
	integration := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
	gitT(t, f.repo, "worktree", "remove", integration)
	if retryCleanup(t, f).IntegrationWorktreeRemoved {
		t.Fatal("missing registration recorded as a successful removal")
	}
}

func TestCleanupReviewStoredIntegrationPath(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	rooms, _, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
	if rooms[0].Work.IntegrationWorktree != want {
		t.Fatal("created integration path was not persisted")
	}
	markCleanupReady(t, f)
	f.hub.mu.Lock()
	f.hub.rooms[f.room.ID].room.Work.IntegrationWorktree = ""
	f.hub.mu.Unlock()
	if retryCleanup(t, f).IntegrationWorktreeRemoved {
		t.Fatal("legacy plan with no saved integration path was removed")
	}
	assertWorktreeExists(t, want, true)
	assertCleanupNote(t, f, "저장된 worktree 경로가 없음")
}

func TestCleanupReviewNativeRelease(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	p := f.part(t, "p1")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	rs.msgs = append(rs.msgs, &Message{ID: "native-part", Kind: "agent", Author: p.Seat, Provider: "claude", SessionID: p.SessionID, SessionDirectory: p.Worktree, Mode: "write"})
	f.hub.mu.Unlock()
	_, opts, _, err := f.hub.DetachNativeSession(f.room.ID, p.Seat, "native-part")
	if err != nil {
		t.Fatal(err)
	}
	markCleanupReady(t, f)
	plan := retryCleanup(t, f)
	if plan.part(p.ID).WorktreeRemoved {
		t.Fatal("open native session's worktree was removed")
	}
	assertWorktreeExists(t, p.Worktree, true)
	retryCleanup(t, f)
	assertCleanupNote(t, f, "CLI 창을 닫으면 정리")
	if got := cleanupNoteCount(t, f, "CLI 창을 닫으면 정리"); got != 1 {
		t.Fatalf("native deferral logged %d times, want 1", got)
	}
	f.hub.ReleaseNativeSession("claude", opts.SessionID)
	if !waitWorktreeCleanup(t, f).part(p.ID).WorktreeRemoved {
		t.Fatal("native release did not retry cleanup")
	}
}

func TestCleanupReviewLegacyNativeReservation(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "claude"})
	defer h.Shutdown(context.Background())
	h.mu.Lock()
	rs := h.rooms[r.ID]
	p := rs.room.participant("claude")
	p.SessionID = "legacy-session"
	h.mu.Unlock()
	if _, _, _, err := h.DetachNativeSession(r.ID, p.ID, ""); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := h.DetachNativeSession(r.ID, p.ID, ""); err == nil {
		t.Fatal("legacy native session with an empty workspace opened twice")
	}
	h.mu.Lock()
	open := h.seatNativeOpen(rs, p)
	h.mu.Unlock()
	if !open {
		t.Fatal("legacy native reservation did not pause the seat")
	}
	h.ReleaseNativeSession("claude", p.SessionID)
}

func TestCleanupReviewSaveFailureAdvancesQueue(t *testing.T) {
	f := newWorkFixture(t, 2, nil)
	plan := f.plan(t)
	p := plan.part("p1")
	sha := commitT(t, p.Worktree, "done.txt", "done\n")
	// Fail only the save after update-ref, after the merge intent was persisted.
	tmp := filepath.Join(f.store.roomDir(f.room.ID), "room.json.tmp")
	hook := fmt.Sprintf("#!/bin/sh\n[ \"$1\" = committed ] || exit 0\nwhile read old new ref; do\n if [ \"$ref\" = '%s' ]; then mkdir %s; fi\ndone\n", "refs/heads/"+plan.Integration, shellPath(tmp))
	hookPath := filepath.Join(f.repo, ".git", "hooks", "reference-transaction")
	if err := os.WriteFile(hookPath, []byte(hook), 0o700); err != nil {
		t.Fatal(err)
	}
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	p = rs.room.Work.part("p1")
	p.State, p.SubmitSHA = "merging", sha
	p2 := rs.room.Work.part("p2")
	p2.State, p2.ReadyMode, p2.SubmitSHA = "queued", "confirm", f.base
	rs.room.Work.Queue = []string{"p1", "p2"}
	f.hub.turnWG.Add(1)
	f.hub.mu.Unlock()
	f.hub.mergePart(rs, p, sha, f.base, plan.Integration, filepath.Join(f.hub.workRoot, f.room.ID, "integration"), nil)
	assertCleanupNote(t, f, "병합 상태 저장 실패")
	if got := f.part(t, "p2").State; got != "awaiting_approval" {
		t.Fatalf("queue stalled after merge save failure: %s", got)
	}
	assertWorktreeExists(t, p.Worktree, true)
}

func TestCleanupReviewPreservesChangedHEAD(t *testing.T) {
	for _, mode := range []string{"detached", "other_branch", "rewound_branch"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkFixture(t, 1, nil)
			markCleanupReady(t, f)
			p := f.part(t, "p1")
			switch mode {
			case "detached":
				gitT(t, p.Worktree, "checkout", "--detach")
				commitT(t, p.Worktree, "orphan.txt", "keep\n")
			case "other_branch":
				gitT(t, p.Worktree, "checkout", "-b", "user-work")
			case "rewound_branch":
				sha := commitT(t, p.Worktree, "submitted.txt", "submitted\n")
				f.hub.mu.Lock()
				f.hub.rooms[f.room.ID].room.Work.part("p1").SubmitSHA = sha
				f.hub.mu.Unlock()
				gitT(t, p.Worktree, "reset", "--hard", f.base)
			}
			if retryCleanup(t, f).part(p.ID).WorktreeRemoved {
				t.Fatal("changed HEAD was removed")
			}
			assertWorktreeExists(t, p.Worktree, true)
		})
	}
}

func TestCleanupReviewDiffWithoutWorktree(t *testing.T) {
	for _, removed := range []bool{false, true} {
		t.Run(fmt.Sprint(removed), func(t *testing.T) {
			f := newWorkFixture(t, 1, nil)
			p := f.part(t, "p1")
			sha := commitT(t, p.Worktree, "done.txt", "done\n")
			if removed {
				gitT(t, f.repo, "worktree", "remove", p.Worktree)
			}
			f.hub.mu.Lock()
			rs := f.hub.rooms[f.room.ID]
			part := rs.room.Work.part(p.ID)
			part.State, part.SubmitSHA, part.WorktreeRemoved = "merged", sha, removed
			rs.workBusy[busyCleanup] = !removed
			f.hub.mu.Unlock()
			diff, err := f.hub.PartDiff(f.room.ID, p.ID)
			if err != nil || len(diff.Files) != 1 || diff.Files[0].Path != "done.txt" {
				t.Fatalf("repository diff blocked by worktree state: %+v %v", diff, err)
			}
		})
	}
}

func TestCleanupReviewFailureDedupAcrossRestart(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	markCleanupReady(t, f)
	p := f.part(t, "p1")
	writeFileT(t, p.Worktree, "dirty.txt", "keep\n")
	retryCleanup(t, f)
	retryCleanup(t, f)
	if err := f.hub.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	h, err := NewHub(f.store, f.reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer h.Shutdown(context.Background())
	f.hub = h
	waitWorktreeCleanup(t, f)
	count := cleanupNoteCount(t, f, "파트 p1 worktree 정리 실패")
	if count != 1 {
		t.Fatalf("same cleanup failure logged %d times, want 1", count)
	}
	assertCleanupNote(t, f, "미커밋 파일")
	if err := os.Remove(filepath.Join(p.Worktree, "dirty.txt")); err != nil {
		t.Fatal(err)
	}
	if !retryCleanup(t, f).part(p.ID).WorktreeRemoved {
		t.Fatal("fixed dirty worktree was not retried")
	}
}

func cleanupNoteCount(t *testing.T, f *workFixture, text string) int {
	t.Helper()
	_, msgs, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, msg := range msgs[f.room.ID] {
		if strings.Contains(msg.Text, text) {
			count++
		}
	}
	return count
}

func TestCleanupReviewUnregisteredDirectory(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	markCleanupReady(t, f)
	p := f.part(t, "p1")
	gitT(t, f.repo, "worktree", "move", p.Worktree, filepath.Join(t.TempDir(), "moved"))
	if err := os.Mkdir(p.Worktree, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, p.Worktree, "keep.txt", "user data\n")
	if retryCleanup(t, f).part(p.ID).WorktreeRemoved {
		t.Fatal("unregistered directory marked removed")
	}
	retryCleanup(t, f)
	assertWorktreeExists(t, filepath.Join(p.Worktree, "keep.txt"), true)
	assertCleanupNote(t, f, "git worktree prune")
	if got := cleanupNoteCount(t, f, "파트 p1 worktree 정리 실패"); got != 1 {
		t.Fatalf("unchanged registration failure logged %d times", got)
	}
}

func shellPath(path string) string {
	return "'" + strings.ReplaceAll(filepath.ToSlash(path), "'", "'\"'\"'") + "'"
}

func TestCleanupReviewRestartDoesNotWaitForGit(t *testing.T) {
	f := newWorkFixture(t, 2, nil)
	markCleanupReady(t, f)
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	p2 := rs.room.Work.part("p2")
	p2.State, p2.ReadyMode, p2.SubmitSHA = "queued", "confirm", f.base
	rs.room.Work.Queue = []string{p2.ID}
	err := f.hub.saveWork(rs)
	f.hub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.hub.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	hook := filepath.Join(dir, "fsmonitor")
	script := fmt.Sprintf("#!/bin/sh\n: > %s\nwhile [ ! -f %s ]; do sleep 0.05; done\nprintf 'token\\000'\n", shellPath(entered), shellPath(release))
	if err := os.WriteFile(hook, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, f.repo, "config", "core.fsmonitor", filepath.ToSlash(hook))
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	type result struct {
		h   *Hub
		err error
	}
	done := make(chan result, 1)
	go func() {
		h, err := NewHub(f.store, f.reg, dir, log.New(io.Discard, "", 0))
		done <- result{h, err}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("git did not reach fsmonitor hook")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var got result
	returned := false
	select {
	case got = <-done:
		returned = true
	case <-time.After(time.Second):
	}
	if returned && got.err == nil {
		got.h.mu.Lock()
		state := got.h.rooms[f.room.ID].room.Work.part("p2").State
		got.h.mu.Unlock()
		if state != "queued" {
			t.Errorf("queue advanced while cleanup git was blocked: %s", state)
		}
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if !returned {
		got = <-done
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	defer got.h.Shutdown(context.Background())
	if !returned {
		t.Fatal("NewHub waited for blocked cleanup git")
	}
	f.hub = got.h
	if !waitWorktreeCleanup(t, f).part("p1").WorktreeRemoved {
		t.Fatal("background cleanup did not finish")
	}
	if f.part(t, "p2").State != "awaiting_approval" {
		t.Fatal("cleanup did not resume the merge queue")
	}
}
