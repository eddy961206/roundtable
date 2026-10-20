package room

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func terminalWorkReview(t *testing.T, phase string) (*workFixture, *controlledProvider, *controlledProvider) {
	t.Helper()
	f := newWorkFixture(t, 1, func(s *WorkSpec) { s.ReviewRounds = 1 })
	author, reviewer := controlWorkSeat(t, f, "claude"), controlWorkSeat(t, f, "codex")
	commitT(t, f.part(t, "p1").Worktree, "done.txt", "first\n")
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, reviewer), findingHeader+findingOne)
	action := "ACCEPT"
	if phase == "disputed" {
		action = "REJECT"
	}
	finishCall(nextCall(t, author), authorHeader+"| r1-1 | "+action+" | evidence |\n")
	if phase == "disputed" {
		finishCall(nextCall(t, reviewer), rebuttalHeader+"| r1-1 | MAINTAIN | still broken |\n")
	}
	p := f.waitPart(t, "p1", "blocked")
	if p.Review.Phase != phase || p.Block.Kind != "review" {
		t.Fatalf("unexpected terminal review: %+v", p)
	}
	return f, author, reviewer
}

func TestWorkTerminalReviewRetry(t *testing.T) {
	for _, phase := range []string{"limit", "disputed"} {
		t.Run(phase, func(t *testing.T) {
			f, author, reviewer := terminalWorkReview(t, phase)
			before := f.part(t, "p1")
			plan, err := f.hub.RetryPart(f.room.ID, "p1")
			if err != nil {
				t.Fatal(err)
			}
			p := plan.part("p1")
			if p.State != "fixing" || p.Review.Phase != "fix" || p.Review.MaxRounds != 2 || plan.ReviewRounds != 1 || plan.Halted != nil || !reflect.DeepEqual(p.Review.Rounds, before.Review.Rounds) {
				t.Fatalf("retry changed wrong state: %+v", p)
			}
			fix := nextCall(t, author)
			if !strings.Contains(fix.opts.Prompt, "r1-1") {
				t.Fatal("fix prompt omitted unresolved finding")
			}
			sha := commitT(t, p.Worktree, "done.txt", "fixed\n")
			finishCall(fix, "fixed\nWORK:DONE")
			recheck := nextCall(t, reviewer)
			p = f.part(t, "p1")
			if p.Review.Phase != "recheck" || p.Review.Rounds[1].Ref.Round != 2 || p.SubmitSHA != sha || !strings.Contains(recheck.opts.Prompt, "r1-1") || !strings.Contains(recheck.opts.Prompt, before.Review.Rounds[0].Items[0].Status) {
				t.Fatalf("resubmission lost original finding: %+v", p.Review)
			}
			finishCall(recheck, recheckHeader+"| r1-1 | FIXED | fixed in new SHA |\n"+findingHeader)
			p = f.waitPart(t, "p1", "awaiting_approval")
			if !p.Review.Summary.Passed || p.Review.Rounds[0].Items[0].Status != "fixed" {
				t.Fatalf("recheck did not resolve finding: %+v", p.Review)
			}
			assertWorkNote(t, f, "사람이 리뷰 라운드 1회 추가 허용 · 상한 2", 1)
		})
	}
}

func TestWorkResolvePreservesFindingsAndAudit(t *testing.T) {
	for _, phase := range []string{"limit", "disputed"} {
		t.Run(phase, func(t *testing.T) {
			f, _, _ := terminalWorkReview(t, phase)
			before := f.part(t, "p1")
			at := nowMs()
			plan, err := f.hub.ResolvePart(f.room.ID, "p1")
			if err != nil {
				t.Fatal(err)
			}
			p := plan.part("p1")
			r := p.Review.Resolution
			if p.State != "awaiting_approval" || !reflect.DeepEqual(plan.Queue, []string{"p1"}) || plan.Halted != nil || p.Block != nil || p.ReadyMode != "confirm" || r == nil || r.By != "user" || r.At < at || r.At > nowMs() || !reflect.DeepEqual(r.ItemIDs, []string{"r1-1"}) {
				t.Fatalf("resolve did not queue with audit: %+v / %+v", p, r)
			}
			if !reflect.DeepEqual(p.Review.Rounds, before.Review.Rounds) || p.Review.Summary != before.Review.Summary || p.Review.Phase != phase {
				t.Fatal("human decision rewrote the review result")
			}
			if _, err := f.hub.ResolvePart(f.room.ID, "p1"); !errors.Is(err, ErrWorkConflict) {
				t.Fatalf("duplicate resolution must conflict: %v", err)
			}
			assertWorkNote(t, f, "사람이 미해결 리뷰 항목을 넘기기로 판정", 1)
			if err := f.hub.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			h := restartWorkHub(t, f)
			snap, _ := h.Snapshot(f.room.ID)
			if !reflect.DeepEqual(snap.Work.part("p1").Review, p.Review) {
				t.Fatal("audit or findings changed across restart")
			}
			if _, err := h.ApprovePart(f.room.ID, "p1", p.SubmitSHA, plan.IntegrationSHA); err != nil {
				t.Fatal(err)
			}
			f.hub = h
			f.waitPart(t, "p1", "merged")
			if gitT(t, f.repo, "show", f.integrationTip(t)+":done.txt") != "first" {
				t.Fatal("resolved submission was not actually merged")
			}
		})
	}
}

func TestWorkResolveRejectsOtherBlocks(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	if _, err := f.hub.ResolvePart(f.room.ID, "p1"); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("working part accepted: %v", err)
	}
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hub.ResolvePart(f.room.ID, "p1"); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("submit block accepted: %v", err)
	}
}

func assertWorkNote(t *testing.T, f *workFixture, text string, want int) {
	t.Helper()
	snap, err := f.hub.Snapshot(f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, text) {
			count++
		}
	}
	if count != want {
		t.Fatalf("system note %q: got %d want %d", text, count, want)
	}
}

func pendingWorkReview(t *testing.T, kind string) (*workFixture, *controlledProvider, *controlledCall, string) {
	t.Helper()
	f := newWorkFixture(t, 1, nil)
	author, reviewer := controlWorkSeat(t, f, "claude"), controlWorkSeat(t, f, "codex")
	commitT(t, f.part(t, "p1").Worktree, "done.txt", "first\n")
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, reviewer)
	if kind == "findings" {
		return f, reviewer, call, findingHeader
	}
	finishCall(call, findingHeader+findingOne)
	call = nextCall(t, author)
	if kind == "author_reply" {
		return f, author, call, authorHeader + "| r1-1 | ACCEPT | fix it |\n"
	}
	if kind == "rebuttal" {
		finishCall(call, authorHeader+"| r1-1 | REJECT | not a defect |\n")
		return f, reviewer, nextCall(t, reviewer), rebuttalHeader + "| r1-1 | WITHDRAW | agreed |\n"
	}
	finishCall(call, authorHeader+"| r1-1 | ACCEPT | fix it |\n")
	fix := nextCall(t, author)
	commitT(t, f.part(t, "p1").Worktree, "done.txt", "fixed\n")
	finishCall(fix, "fixed\nWORK:DONE")
	return f, reviewer, nextCall(t, reviewer), recheckHeader + "| r1-1 | FIXED | verified |\n" + findingHeader
}

func TestWorkReviewFormatCorrection(t *testing.T) {
	for _, kind := range []string{"findings", "author_reply", "rebuttal", "recheck"} {
		for _, succeeds := range []bool{true, false} {
			name := kind + "/success"
			if !succeeds {
				name = kind + "/failure"
			}
			t.Run(name, func(t *testing.T) {
				f, provider, first, valid := pendingWorkReview(t, kind)
				before := f.part(t, "p1").Review
				_, _, parseErr := before.Apply("not a table")
				finishCall(first, "not a table")
				retry := nextCall(t, provider)
				p := f.part(t, "p1")
				if p.State != "reviewing" || p.Review.Phase != kind || p.Review.Pending.FormatRetries != 1 || !reflect.DeepEqual(p.Review.Rounds, before.Rounds) || !strings.HasPrefix(retry.opts.Prompt, first.opts.Prompt) || !strings.Contains(retry.opts.Prompt, parseErr.Error()) || retry.opts.Mode != first.opts.Mode {
					t.Fatalf("wrong correction step: %+v", p.Review)
				}
				if kind == "author_reply" {
					if retry.opts.SessionID != "claude-session" || retry.opts.Mode != agent.ModeWrite {
						t.Fatalf("author session lost: %+v", retry.opts)
					}
				} else if retry.opts.SessionID != "" || retry.opts.Mode != agent.ModeRead {
					t.Fatal("reviewer correction must use a fresh read-only session")
				}
				rooms, _, err := f.store.LoadRooms()
				if err != nil || rooms[0].Work.part("p1").Review.Pending.FormatRetries != 1 {
					t.Fatalf("counter not persisted before dispatch: %v", err)
				}
				if succeeds {
					finishCall(retry, valid)
					want := "awaiting_approval"
					if kind == "author_reply" {
						want = "fixing"
					}
					f.waitPart(t, "p1", want)
				} else {
					finishCall(retry, "still not a table")
					p = f.waitPart(t, "p1", "blocked")
					if p.Block.Kind != "review" || p.Review.Pending.FormatRetries != 1 || !reflect.DeepEqual(p.Review.Rounds, before.Rounds) {
						t.Fatalf("second failure did not preserve and block: %+v", p)
					}
					select {
					case <-provider.callsCh:
						t.Fatal("more than one automatic correction")
					default:
					}
				}
				assertWorkNote(t, f, "리뷰 표 형식 오류로 1회 다시 요청: "+parseErr.Error(), 1)
				stats, err := f.hub.ModelStats()
				if err != nil {
					t.Fatal(err)
				}
				formatErrors, wantFormatErrors := 0, 2
				if succeeds {
					wantFormatErrors = 1
				}
				for _, row := range stats.Rows {
					formatErrors += row.ReviewFormatErrors
				}
				if formatErrors != wantFormatErrors {
					t.Fatalf("review format errors = %d, want %d: %+v", formatErrors, wantFormatErrors, stats.Rows)
				}
			})
		}
	}
}

func TestWorkReviewFormatCorrectionSurvivesRestart(t *testing.T) {
	for _, kind := range []string{"findings", "author_reply"} {
		t.Run(kind, func(t *testing.T) {
			f, provider, first, _ := pendingWorkReview(t, kind)
			finishCall(first, "bad table")
			retry := nextCall(t, provider)
			if err := f.hub.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			h := restartWorkHub(t, f)
			f.hub = h
			if _, err := h.RetryPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			resumed := nextCall(t, provider)
			if resumed.opts.Prompt != retry.opts.Prompt || resumed.opts.SessionID != retry.opts.SessionID {
				t.Fatal("restart lost correction prompt or session")
			}
			finishCall(resumed, "bad table after restart")
			p := f.waitPart(t, "p1", "blocked")
			if p.Block.Kind != "review" || p.Review.Pending.FormatRetries != 1 {
				t.Fatalf("restart reset the correction allowance: %+v", p)
			}
			assertWorkNote(t, f, "리뷰 표 형식 오류로 1회 다시 요청:", 1)
			select {
			case <-provider.callsCh:
				t.Fatal("restart granted another automatic correction")
			default:
			}
		})
	}
}
