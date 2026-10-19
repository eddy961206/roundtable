package room

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func waitWorktreeCleanup(t *testing.T, f *workFixture) *WorkPlan {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		f.hub.mu.Lock()
		rs := f.hub.rooms[f.room.ID]
		busy := rs.workBusy[busyCleanup]
		plan := cloneWork(rs.room.Work)
		f.hub.mu.Unlock()
		if !busy {
			return plan
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worktree cleanup did not finish")
	return nil
}

func assertWorktreeExists(t *testing.T, path string, want bool) {
	t.Helper()
	_, err := os.Stat(path)
	if want && err != nil || !want && !os.IsNotExist(err) {
		t.Fatalf("worktree %s: exists want %v, stat %v", path, want, err)
	}
}

func assertCleanupNote(t *testing.T, f *workFixture, text string) {
	t.Helper()
	_, msgs, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs[f.room.ID] {
		if msg.Kind == "system" && strings.Contains(msg.Text, text) {
			return
		}
	}
	t.Fatalf("persisted system message missing: %s", text)
}

func TestWorktreeCleanupAfterMergeAndAbandon(t *testing.T) {
	f := newWorkFixture(t, 2, func(s *WorkSpec) { s.MergeMode = "auto" })
	p1, p2 := f.part(t, "p1"), f.part(t, "p2")
	integration := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
	sha := commitT(t, p1.Worktree, "done.txt", "done\n")
	f.passReview(t, p1.ID, sha)
	f.waitPart(t, p1.ID, "merged")
	plan := waitWorktreeCleanup(t, f)
	if !plan.part(p1.ID).WorktreeRemoved || plan.part(p1.ID).Worktree != p1.Worktree || plan.IntegrationWorktreeRemoved {
		t.Fatalf("cleanup state: %+v", plan)
	}
	assertWorktreeExists(t, p1.Worktree, false)
	assertWorktreeExists(t, p2.Worktree, true)
	assertWorktreeExists(t, integration, true)
	if got := gitT(t, f.repo, "rev-parse", p1.Branch); got != sha {
		t.Fatalf("part branch changed: %s", got)
	}
	if diff, err := f.hub.PartDiff(f.room.ID, p1.ID); err != nil || diff.HeadSHA != sha {
		t.Fatalf("removed part diff: %v", err)
	}
	if _, err := f.hub.RetryPart(f.room.ID, p1.ID); !errors.Is(err, ErrWorkConflict) || !strings.Contains(err.Error(), "이미 정리됨") {
		t.Fatalf("removed part retry: %v", err)
	}
	// Historical CLI launch must not resume into the deleted part directory.
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	msg := &Message{ID: "old-part-session", Kind: "agent", Author: p1.Seat, Provider: "claude", SessionID: p1.SessionID, SessionDirectory: p1.Worktree, Mode: "write"}
	rs.msgs = append(rs.msgs, msg)
	f.hub.mu.Unlock()
	if _, _, err := f.hub.NativeSessionOptions(f.room.ID, p1.Seat, msg.ID); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("removed part native session: %v", err)
	}
	if _, err := f.hub.AbandonPart(f.room.ID, p2.ID); err != nil {
		t.Fatal(err)
	}
	plan = waitWorktreeCleanup(t, f)
	if !plan.IntegrationWorktreeRemoved || plan.part(p2.ID).WorktreeRemoved || plan.part(p2.ID).State != "abandoned" {
		t.Fatalf("finished plan cleanup: %+v", plan)
	}
	assertWorktreeExists(t, integration, false)
	assertWorktreeExists(t, p2.Worktree, true)
	gitT(t, f.repo, "show-ref", "--verify", "refs/heads/"+p2.Branch)
	if f.integrationTip(t) != plan.IntegrationSHA {
		t.Fatal("integration branch was removed or changed")
	}
}

func TestWorktreeCleanupPreservesDirtyWorktrees(t *testing.T) {
	for _, dirty := range []string{"part", "integration"} {
		t.Run(dirty, func(t *testing.T) {
			f := newWorkFixture(t, 1, func(s *WorkSpec) { s.MergeMode = "auto" })
			p := f.part(t, "p1")
			integration := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
			sha := commitT(t, p.Worktree, "done.txt", "done\n")
			dir, note := p.Worktree, "파트 p1 worktree 정리 실패"
			if dirty == "integration" {
				dir, note = integration, "통합 worktree 정리 실패"
			}
			writeFileT(t, dir, "uncommitted.txt", "keep this\n")
			f.passReview(t, p.ID, sha)
			f.waitPart(t, p.ID, "merged")
			plan := waitWorktreeCleanup(t, f)
			if plan.part(p.ID).State != "merged" || plan.Halted != nil || (dirty == "part" && plan.part(p.ID).WorktreeRemoved) || (dirty == "integration" && plan.IntegrationWorktreeRemoved) {
				t.Fatalf("cleanup failure changed merge: %+v", plan)
			}
			assertWorktreeExists(t, dir, true)
			if got, err := os.ReadFile(filepath.Join(dir, "uncommitted.txt")); err != nil || string(got) != "keep this\n" {
				t.Fatalf("dirty file lost: %q %v", got, err)
			}
			assertCleanupNote(t, f, note)
			assertCleanupNote(t, f, "git worktree remove")
			gitT(t, f.repo, "show-ref", "--verify", "refs/heads/"+p.Branch)
			if f.integrationTip(t) != plan.IntegrationSHA {
				t.Fatal("cleanup rolled back integration branch")
			}
		})
	}
}

func TestWorktreeCleanupRestart(t *testing.T) {
	for _, alreadyRemoved := range []bool{false, true} {
		t.Run(map[bool]string{false: "leftover", true: "removed_before_flag_saved"}[alreadyRemoved], func(t *testing.T) {
			f := newWorkFixture(t, 2, nil)
			p1, p2 := f.part(t, "p1"), f.part(t, "p2")
			integration := filepath.Join(f.hub.workRoot, f.room.ID, "integration")
			if err := f.hub.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			f.hub.mu.Lock()
			rs := f.hub.rooms[f.room.ID]
			rs.room.Work.part(p1.ID).State = "merged"
			rs.room.Work.part(p1.ID).SubmitSHA = f.base
			rs.room.Work.part(p2.ID).State = "abandoned"
			err := f.store.SaveRoom(rs.room)
			f.hub.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if alreadyRemoved {
				gitT(t, f.repo, "worktree", "remove", p1.Worktree)
				gitT(t, f.repo, "worktree", "remove", integration)
			}
			restarted, err := NewHub(f.store, f.reg, t.TempDir(), log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			defer restarted.Shutdown(context.Background())
			f.hub = restarted
			waitWorktreeCleanup(t, f)
			snap, err := restarted.Snapshot(f.room.ID)
			if err != nil || !snap.Work.part(p1.ID).WorktreeRemoved || !snap.Work.IntegrationWorktreeRemoved || snap.Work.part(p2.ID).WorktreeRemoved {
				t.Fatalf("recovery flags: %+v %v", snap, err)
			}
			assertWorktreeExists(t, p1.Worktree, false)
			assertWorktreeExists(t, integration, false)
			assertWorktreeExists(t, p2.Worktree, true)
			for _, branch := range []string{p1.Branch, p2.Branch, snap.Work.Integration} {
				gitT(t, f.repo, "show-ref", "--verify", "refs/heads/"+branch)
			}
			rooms, _, err := f.store.LoadRooms()
			if err != nil || !rooms[0].Work.part(p1.ID).WorktreeRemoved || !rooms[0].Work.IntegrationWorktreeRemoved {
				t.Fatalf("recovery flags not persisted: %v", err)
			}
		})
	}
}

func TestWorktreeCleanupRejectsWrongPlanPath(t *testing.T) {
	f := newWorkFixture(t, 2, nil)
	p1, p2 := f.part(t, "p1"), f.part(t, "p2")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	rs.room.Work.part(p1.ID).State = "merged"
	rs.room.Work.part(p1.ID).Worktree = p2.Worktree
	f.hub.startWorktreeCleanup(rs)
	f.hub.mu.Unlock()
	plan := waitWorktreeCleanup(t, f)
	if plan.part(p1.ID).WorktreeRemoved || plan.part(p1.ID).State != "merged" {
		t.Fatal("path mismatch marked worktree removed or changed state")
	}
	assertWorktreeExists(t, p1.Worktree, true)
	assertWorktreeExists(t, p2.Worktree, true)
	assertCleanupNote(t, f, "계획의 worktree 경로 또는 branch와 일치하지 않음")
}
