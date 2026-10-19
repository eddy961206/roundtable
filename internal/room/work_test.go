package room

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// Work tests use real git repositories in temporary folders. Queue-only tests
// inject a passed summary; lifecycle tests exercise the real review protocol.

type workFixture struct {
	hub   *Hub
	store *Store
	reg   *agent.Registry
	room  *Room
	repo  string
	base  string
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := runGit(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func writeFileT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitT(t *testing.T, dir, name, content string) string {
	t.Helper()
	writeFileT(t, dir, name, content)
	gitT(t, dir, "add", name)
	gitT(t, dir, "commit", "-q", "-m", "change "+name)
	return gitT(t, dir, "rev-parse", "HEAD")
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "Roundtable Test")
	gitT(t, dir, "config", "user.email", "test@example.invalid")
	gitT(t, dir, "config", "commit.gpgsign", "false")
	gitT(t, dir, "config", "core.autocrlf", "false")
	commitT(t, dir, "shared.txt", "base\n")
	return dir
}

func newWorkFixture(t *testing.T, parts int, mutate func(*WorkSpec), beforePlan ...func(*Hub, *Room)) *workFixture {
	t.Helper()
	repo := newGitRepo(t)
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reg := agent.NewRegistry(&fakeProvider{id: "claude"}, &fakeProvider{id: "codex"})
	hub, err := NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	r, err := hub.CreateRoom(RoomSpec{Title: "work", Mode: agent.ModeWrite, Workspace: repo, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, before := range beforePlan {
		before(hub, r)
	}
	if _, err := hub.CreateWork(r.ID, workSpec(parts, mutate)); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, hub, r.ID) // part kickoff turns
	return &workFixture{hub: hub, store: store, reg: reg, room: r, repo: repo, base: gitT(t, repo, "rev-parse", "HEAD")}
}

func workSpec(parts int, mutate func(*WorkSpec)) WorkSpec {
	seats := [][2]string{{"claude", "codex"}, {"codex", "claude"}}
	spec := WorkSpec{Goal: "목표"}
	for i := range parts {
		spec.Parts = append(spec.Parts, PartSpec{Title: fmt.Sprintf("파트 %d", i+1), Brief: "완료 조건", Scope: []string{"."}, Seat: seats[i][0], Reviewer: seats[i][1]})
	}
	if mutate != nil {
		mutate(&spec)
	}
	return spec
}

func (f *workFixture) plan(t *testing.T) *WorkPlan {
	t.Helper()
	snap, err := f.hub.Snapshot(f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Work
}

func (f *workFixture) part(t *testing.T, id string) *Part {
	t.Helper()
	return f.plan(t).part(id)
}

// passReview stands in for a passed review and lets queuePart capture ReadyMode (D6).
func (f *workFixture) passReview(t *testing.T, partID, sha string) {
	t.Helper()
	f.hub.mu.Lock()
	defer f.hub.mu.Unlock()
	rs := f.hub.rooms[f.room.ID]
	part := rs.room.Work.part(partID)
	part.State, part.SubmitSHA = "reviewing", sha
	part.Review = &ReviewState{MaxRounds: 3, Summary: ReviewSummary{Complete: true, Passed: true}}
	f.hub.queuePart(rs, part)
	if err := f.hub.saveWork(rs); err != nil {
		t.Fatal(err)
	}
}

func (f *workFixture) waitPart(t *testing.T, id, state string) *Part {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if part := f.part(t, id); part.State == state {
			return part
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("파트 %s가 %s 상태가 되지 않음: %+v", id, state, f.part(t, id))
	return nil
}

func (f *workFixture) integrationTip(t *testing.T) string {
	t.Helper()
	return gitT(t, f.repo, "rev-parse", "refs/heads/"+f.plan(t).Integration)
}

func parentCount(t *testing.T, repo, sha string) int {
	t.Helper()
	return len(strings.Fields(gitT(t, repo, "rev-list", "--parents", "-n", "1", sha))) - 1
}

func TestWorkCreatesWorktreesOutsideUserCheckout(t *testing.T) {
	f := newWorkFixture(t, 2, nil)
	plan := f.plan(t)
	if plan.BaseSHA != f.base || plan.IntegrationSHA != f.base || plan.MergeMode != "confirm" || plan.ReviewRounds != 3 {
		t.Fatalf("plan defaults: %+v", plan)
	}
	if plan.Integration != "rt/"+f.room.ID+"/integration" || f.integrationTip(t) != f.base {
		t.Fatalf("integration branch: %s", plan.Integration)
	}
	for _, part := range plan.Parts {
		want := filepath.Join(f.store.Root(), "worktrees", f.room.ID, part.ID)
		if part.Worktree != want || part.State != "working" || part.SessionID == "" {
			t.Fatalf("part: %+v (want worktree %s and a part session)", part, want)
		}
		if head := gitT(t, part.Worktree, "symbolic-ref", "--short", "HEAD"); head != part.Branch || part.Branch != "rt/"+f.room.ID+"/"+part.ID {
			t.Fatalf("part %s checks out %s", part.ID, head)
		}
	}
	snap, _ := f.hub.Snapshot(f.room.ID)
	for _, p := range snap.Room.Participants {
		if p.SessionID != "" {
			t.Fatalf("part kickoff must not use the chat session: %+v", p)
		}
	}
	if head := gitT(t, f.repo, "symbolic-ref", "--short", "HEAD"); head != "main" || gitT(t, f.repo, "status", "--porcelain") != "" {
		t.Fatalf("user checkout changed: %s", head)
	}
	if _, err := f.hub.CreateWork(f.room.ID, workSpec(1, nil)); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("second plan: %v", err)
	}

	other, err := f.hub.CreateRoom(RoomSpec{Title: "same provider", Mode: agent.ModeWrite, Workspace: f.repo, Participants: []string{"claude", "codex"}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.hub.CreateWork(other.ID, workSpec(1, func(s *WorkSpec) { s.Parts[0].Reviewer = "claude" }))
	if err == nil || errors.Is(err, ErrWorkConflict) || !strings.Contains(err.Error(), "다른 공급사") {
		t.Fatalf("D3 reviewer check: %v", err)
	}
}

func TestWorkSubmitChecksBlockThePart(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	wt := f.part(t, "p1").Worktree

	plan, err := f.hub.SubmitPart(f.room.ID, "p1")
	if err != nil {
		t.Fatal(err)
	}
	part := plan.part("p1")
	if part.State != "blocked" || part.Block.Kind != "submit" || part.Block.From != "working" || !strings.Contains(part.Block.Message, "커밋이 없음") {
		t.Fatalf("no commits: %+v", part.Block)
	}
	if plan.Halted == nil || plan.Halted.Kind != "submit" {
		t.Fatalf("D5 halt: %+v", plan.Halted)
	}
	plan, err = f.hub.RetryPart(f.room.ID, "p1")
	if err != nil || plan.part("p1").State != "working" || plan.Halted != nil {
		t.Fatalf("retry: %v %+v", err, plan)
	}

	commitT(t, wt, "done.txt", "done\n")
	writeFileT(t, wt, "stray.txt", "untracked\n")
	plan, err = f.hub.SubmitPart(f.room.ID, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if b := plan.part("p1").Block; b == nil || b.Kind != "submit" || !strings.Contains(b.Message, "stray.txt") {
		t.Fatalf("untracked file: %+v", b)
	}
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("submit while blocked: %v", err)
	}
}

func TestWorkConflictBlocksAndHaltsQueue(t *testing.T) {
	f := newWorkFixture(t, 2, func(s *WorkSpec) { s.MergeMode = "auto" })
	sha1 := commitT(t, f.part(t, "p1").Worktree, "shared.txt", "one\n")
	sha2 := commitT(t, f.part(t, "p2").Worktree, "shared.txt", "two\n")

	f.passReview(t, "p1", sha1)
	merged := f.waitPart(t, "p1", "merged").MergedSHA
	f.passReview(t, "p2", sha2)
	part := f.waitPart(t, "p2", "blocked")
	if part.Block.Kind != "conflict" || part.Block.From != "queued" || !slices.Contains(part.Block.Result.Conflicts, "shared.txt") {
		t.Fatalf("conflict block: %+v %+v", part.Block, part.Block.Result)
	}
	plan := f.plan(t)
	if plan.Halted == nil || plan.Halted.Kind != "conflict" || !slices.Equal(plan.Queue, []string{"p2"}) {
		t.Fatalf("halt: %+v queue %v", plan.Halted, plan.Queue)
	}
	if f.integrationTip(t) != merged || plan.IntegrationSHA != merged {
		t.Fatalf("integration moved by a conflicting merge")
	}
	if _, err := runGit(filepath.Join(f.store.Root(), "worktrees", f.room.ID, "integration"), "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		t.Fatal("conflicting merge was not aborted")
	}
	if _, err := f.hub.ApprovePart(f.room.ID, "p2", sha2, merged); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("approve blocked part: %v", err)
	}
}

func TestWorkVerifyFailureBlocks(t *testing.T) {
	f := newWorkFixture(t, 1, func(s *WorkSpec) {
		s.MergeMode = "auto"
		s.Verify = []VerifyCmd{{Name: "broken", Argv: []string{"git", "no-such-subcommand"}}}
	})
	if v := f.plan(t).Verify[0]; v.TimeoutSec != defaultVerifyTimeout {
		t.Fatalf("verify timeout default: %+v", v)
	}
	f.passReview(t, "p1", commitT(t, f.part(t, "p1").Worktree, "a.txt", "a\n"))
	part := f.waitPart(t, "p1", "blocked")
	if part.Block.Kind != "verify" || part.Block.Result == nil || part.Block.Result.ExitCode == 0 || part.Block.Result.Name != "broken" {
		t.Fatalf("verify block: %+v %+v", part.Block, part.Block.Result)
	}
	if plan := f.plan(t); plan.Halted == nil || plan.Halted.Kind != "verify" || plan.Merging != nil {
		t.Fatalf("halt: %+v", plan)
	}
	if f.integrationTip(t) != f.base {
		t.Fatal("integration moved after a failed verify")
	}
}

func TestWorkAutoMergesConfirmWaitsForApproval(t *testing.T) {
	f := newWorkFixture(t, 2, func(s *WorkSpec) {
		s.MergeMode = "auto"
		s.Verify = []VerifyCmd{{Name: "version", Argv: []string{"git", "--version"}, TimeoutSec: 60}}
	})
	sha1 := commitT(t, f.part(t, "p1").Worktree, "a.txt", "a\n")
	sha2 := commitT(t, f.part(t, "p2").Worktree, "b.txt", "b\n")

	f.passReview(t, "p1", sha1)
	p1 := f.waitPart(t, "p1", "merged")
	if p1.ReadyMode != "auto" || p1.Approval != nil || f.integrationTip(t) != p1.MergedSHA || parentCount(t, f.repo, p1.MergedSHA) != 2 {
		t.Fatalf("auto merge: %+v", p1)
	}

	if _, err := f.hub.SetMergeMode(f.room.ID, "confirm"); err != nil {
		t.Fatal(err)
	}
	f.passReview(t, "p2", sha2)
	f.waitPart(t, "p2", "awaiting_approval")
	if _, err := f.hub.SetMergeMode(f.room.ID, "auto"); err != nil {
		t.Fatal(err)
	}
	if p2 := f.part(t, "p2"); p2.State != "awaiting_approval" || p2.ReadyMode != "confirm" {
		t.Fatalf("D6: toggle changed a queued part: %+v", p2)
	}
	if _, err := f.hub.ApprovePart(f.room.ID, "p2", sha2, f.base); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("stale integration SHA: %v", err)
	}
	if _, err := f.hub.ApprovePart(f.room.ID, "p2", sha1, p1.MergedSHA); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("stale submit SHA: %v", err)
	}
	if f.part(t, "p2").State != "awaiting_approval" {
		t.Fatal("rejected approval changed the part")
	}
	diff, err := f.hub.PartDiff(f.room.ID, "p2")
	if err != nil || diff.HeadSHA != sha2 || len(diff.Files) != 1 || diff.Files[0].Path != "b.txt" || diff.Files[0].Status != "A" || diff.Added != 1 {
		t.Fatalf("part diff: %v %+v", err, diff)
	}
	if _, err := f.hub.ApprovePart(f.room.ID, "p2", sha2, p1.MergedSHA); err != nil {
		t.Fatal(err)
	}
	p2 := f.waitPart(t, "p2", "merged")
	tip := f.integrationTip(t)
	if tip != p2.MergedSHA || p2.Approval == nil || p2.Approval.IntegrationSHA != p1.MergedSHA {
		t.Fatalf("confirm merge: %+v tip %s", p2, tip)
	}
	gitT(t, f.repo, "cat-file", "-e", tip+":a.txt")
	gitT(t, f.repo, "cat-file", "-e", tip+":b.txt")
	if plan := f.plan(t); len(plan.Queue) != 0 || plan.Halted != nil || plan.Merging != nil {
		t.Fatalf("queue after merges: %+v", plan)
	}
	if plan := waitWorktreeCleanup(t, f); !plan.IntegrationWorktreeRemoved || !plan.part("p1").WorktreeRemoved || !plan.part("p2").WorktreeRemoved {
		t.Fatalf("cleanup after last merge: %+v", plan)
	}
	assertWorktreeExists(t, filepath.Join(f.hub.workRoot, f.room.ID, "integration"), false)
}

func TestWorkRestartRecoversMergeIntent(t *testing.T) {
	cases := []struct {
		name  string
		tip   func(f *workFixture, merge, other string) string // where the integration branch points at restart
		state string
	}{
		{"merge_sha", func(f *workFixture, merge, other string) string { return merge }, "merged"},
		{"before_sha", func(f *workFixture, merge, other string) string { return f.base }, "blocked"},
		{"other", func(f *workFixture, merge, other string) string { return other }, "blocked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWorkFixture(t, 1, func(s *WorkSpec) { s.MergeMode = "auto" })
			wt := f.part(t, "p1").Worktree
			merge := commitT(t, wt, "a.txt", "a\n")
			other := commitT(t, wt, "b.txt", "b\n")
			integration := f.plan(t).Integration

			f.hub.mu.Lock()
			rs := f.hub.rooms[f.room.ID]
			part := rs.room.Work.part("p1")
			part.State, part.SubmitSHA, part.ReadyMode = "merging", merge, "auto"
			rs.room.Work.Queue = []string{"p1"}
			rs.room.Work.Merging = &MergeIntent{PartID: "p1", SubmitSHA: merge, BeforeSHA: f.base, MergeSHA: merge}
			err := f.store.SaveRoom(rs.room)
			f.hub.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if tip := tc.tip(f, merge, other); tip != f.base {
				gitT(t, f.repo, "update-ref", "refs/heads/"+integration, tip, f.base)
			}

			restarted, err := NewHub(f.store, f.reg, t.TempDir(), log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restarted.Shutdown(context.Background()) })
			snap, err := restarted.Snapshot(f.room.ID)
			if err != nil {
				t.Fatal(err)
			}
			plan := snap.Work
			got := plan.part("p1")
			if got.State != tc.state || plan.Merging != nil {
				t.Fatalf("recovered: %+v merging %+v", got, plan.Merging)
			}
			if tc.state == "merged" {
				if got.MergedSHA != merge || plan.IntegrationSHA != merge || len(plan.Queue) != 0 || plan.Halted != nil {
					t.Fatalf("merged recovery: %+v", plan)
				}
				return
			}
			if got.Block.Kind != "restart" || got.Block.From != "queued" || plan.Halted == nil || plan.Halted.Kind != "restart" || plan.IntegrationSHA != f.base {
				t.Fatalf("blocked recovery: %+v %+v", got.Block, plan)
			}
		})
	}
}
