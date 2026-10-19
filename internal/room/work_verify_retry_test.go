package room

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestWorkVerifyRetryFixesAndRechecks(t *testing.T) {
	for _, rounds := range []int{1, 3} {
		t.Run(strconv.Itoa(rounds), func(t *testing.T) {
			f := newWorkFixture(t, 1, func(s *WorkSpec) {
				s.ReviewRounds = rounds
				s.Verify = []VerifyCmd{{Name: "required-file", Argv: []string{"git", "cat-file", "-e", "HEAD:verified.txt"}, TimeoutSec: 30}}
			})
			author, reviewer := controlWorkSeat(t, f, "claude"), controlWorkSeat(t, f, "codex")
			first := commitT(t, f.part(t, "p1").Worktree, "done.txt", "first\n")
			if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			finishCall(nextCall(t, reviewer), findingHeader)
			f.waitPart(t, "p1", "awaiting_approval")
			if _, err := f.hub.ApprovePart(f.room.ID, "p1", first, f.base); err != nil {
				t.Fatal(err)
			}
			blocked := f.waitPart(t, "p1", "blocked")
			if blocked.Block.Kind != "verify" || blocked.Block.Result.Output == "" || f.integrationTip(t) != f.base {
				t.Fatalf("verify did not stop before updating integration: %+v", blocked)
			}
			plan, err := f.hub.RetryPart(f.room.ID, "p1")
			if err != nil {
				t.Fatal(err)
			}
			p := plan.part("p1")
			if p.State != "fixing" || p.Review.Phase != "fix" || p.Review.MaxRounds != max(rounds, 2) || p.Review.Summary.Passed || p.SubmitSHA != first || p.Approval != nil || p.ReadyMode != "" || p.Block != nil || plan.Halted != nil || len(plan.Queue) != 0 || !reflect.DeepEqual(p.Review.Rounds, blocked.Review.Rounds) {
				t.Fatalf("verify retry retained stale merge eligibility: %+v / %+v", p, plan)
			}
			fix := nextCall(t, author)
			for _, want := range []string{"required-file", "HEAD:verified.txt", blocked.Block.Result.Output, "WORK:DONE"} {
				if !strings.Contains(fix.opts.Prompt, want) {
					t.Fatalf("fix instructions missing %q: %s", want, fix.opts.Prompt)
				}
			}
			if fix.opts.SessionID != blocked.SessionID || fix.opts.Workspace != blocked.Worktree {
				t.Fatal("fix did not run in the original part session and worktree")
			}
			stored, _, err := f.store.LoadRooms()
			if err != nil || stored[0].Work.part("p1").Review.Phase != "fix" || len(stored[0].Work.Queue) != 0 {
				t.Fatalf("retry state not persisted: %v", err)
			}
			finishCall(fix, "no new commit yet")
			waitIdle(t, f.hub, f.room.ID)
			plan, err = f.hub.SubmitPart(f.room.ID, "p1")
			if err != nil || plan.part("p1").Block.Kind != "submit" || !strings.Contains(plan.part("p1").Block.Message, "같은 커밋") {
				t.Fatalf("unchanged SHA was accepted: %+v, %v", plan, err)
			}
			if _, err := f.hub.RetryPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			second := commitT(t, p.Worktree, "verified.txt", "fixed\n")
			if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			recheck := nextCall(t, reviewer)
			p = f.part(t, "p1")
			if p.Review.Phase != "recheck" || len(p.Review.Rounds) != 2 || p.Review.Input.Ref.Round != 2 || p.SubmitSHA != second || recheck.opts.SessionID != "" || !strings.Contains(recheck.opts.Prompt, second) {
				t.Fatalf("new submission did not start a fresh second review: %+v", p)
			}
			finishCall(recheck, recheckHeader+findingHeader)
			p = f.waitPart(t, "p1", "awaiting_approval")
			if queue := f.plan(t).Queue; !reflect.DeepEqual(queue, []string{"p1"}) {
				t.Fatalf("recheck duplicated queue entry: %v", queue)
			}
			if _, err := f.hub.ApprovePart(f.room.ID, "p1", first, f.base); !errors.Is(err, ErrWorkConflict) {
				t.Fatalf("old approval accepted: %v", err)
			}
			if _, err := f.hub.ApprovePart(f.room.ID, "p1", second, f.base); err != nil {
				t.Fatal(err)
			}
			p = f.waitPart(t, "p1", "merged")
			if f.integrationTip(t) != p.MergedSHA || gitT(t, f.repo, "show", p.MergedSHA+":verified.txt") != "fixed" {
				t.Fatal("corrected SHA did not pass verify and reach integration")
			}
			wantExtra := 0
			if rounds == 1 {
				wantExtra = 1
			}
			assertWorkNote(t, f, "사람이 리뷰 라운드 1회 추가 허용", wantExtra)
		})
	}
}

func TestReviewVerifyRetryRevokesHumanResolution(t *testing.T) {
	rs := startedReview(t, 1)
	applyReview(t, rs, findingHeader+findingOne, "author_reply", "")
	applyReview(t, rs, authorHeader+"| r1-1 | REJECT | evidence |\n", "rebuttal", "")
	applyReview(t, rs, rebuttalHeader+"| r1-1 | MAINTAIN | still broken |\n", "", "disputed")
	rs.Resolve(nowMs())
	before := append([]ReviewRound(nil), rs.Rounds...)
	extra, err := rs.ReopenAfterVerify()
	if err != nil || !extra || rs.MaxRounds != 2 || rs.Phase != "fix" || rs.Pending != nil || rs.Resolution != nil || rs.Summary.Complete || !reflect.DeepEqual(rs.Rounds, before) {
		t.Fatalf("verify retry lost findings or retained waiver: %+v, %v", rs, err)
	}
	if _, err := rs.Resubmit(protocolInput("head-two")); err != nil {
		t.Fatal(err)
	}
	applyReview(t, rs, recheckHeader+"| r1-1 | UNFIXED | still broken |\n"+findingHeader, "", "disputed")
	if rs.Resolution != nil || rs.Summary.Passed || rs.Summary.Unresolved != 1 {
		t.Fatal("old human resolution bypassed the new review")
	}
}
