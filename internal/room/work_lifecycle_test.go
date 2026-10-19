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

	"roundtable/internal/agent"
)

func controlWorkSeat(t *testing.T, f *workFixture, id string) *controlledProvider {
	t.Helper()
	p := &controlledProvider{fakeProvider: fakeProvider{id: id, steer: true}, callsCh: make(chan *controlledCall, 16)}
	providers := []agent.Provider{p}
	for _, other := range []string{"claude", "codex"} {
		if other != id {
			providers = append(providers, f.reg.Get(other))
		}
	}
	f.reg = agent.NewRegistry(providers...)
	f.hub.mu.Lock()
	f.hub.reg = f.reg
	f.hub.mu.Unlock()
	return p
}

func restartWorkHub(t *testing.T, f *workFixture) *Hub {
	t.Helper()
	h, err := NewHub(f.store, f.reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
	return h
}

func TestWorkReviewRetryAfterRestart(t *testing.T) {
	for _, graceful := range []bool{false, true} {
		t.Run(map[bool]string{false: "crash", true: "shutdown"}[graceful], func(t *testing.T) {
			f := newWorkFixture(t, 1, nil)
			reviewer := controlWorkSeat(t, f, "codex")
			commitT(t, f.part(t, "p1").Worktree, "done.txt", "done\n")
			if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			original := nextCall(t, reviewer)
			if graceful {
				if err := f.hub.Shutdown(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			h := restartWorkHub(t, f)
			if _, err := h.RetryPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			retried := nextCall(t, reviewer)
			if retried.opts.Prompt == "" || retried.opts.Prompt != original.opts.Prompt || retried.opts.SessionID != "" {
				t.Fatalf("review retry lost its pending step: %+v", retried.opts)
			}
			finishCall(retried, findingHeader)
			queueSnapshot(t, h, f.room.ID, func(s *Snapshot) bool { return s.Work.part("p1").State == "awaiting_approval" })
		})
	}
}

func TestWorkSubmitRejectsRunningPart(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	author := controlWorkSeat(t, f, "claude")
	postQueueMessage(t, f.hub, f.room.ID, "still working", "claude")
	call := nextCall(t, author)
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); !errors.Is(err, ErrWorkConflict) {
		t.Fatalf("submission while the author can still write: %v", err)
	}
	finishCall(call, "done")
}

func TestWorkDoneReservesBeforeFollowup(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	author := controlWorkSeat(t, f, "claude")
	reviewer := controlWorkSeat(t, f, "codex")
	commitT(t, f.part(t, "p1").Worktree, "done.txt", "done\n")
	postQueueMessage(t, f.hub, f.room.ID, "finish", "claude")
	call := nextCall(t, author)
	postQueueMessage(t, f.hub, f.room.ID, "followup must wait", "claude")
	finishCall(call, "finished\nWORK:DONE") // the steer was not consumed; it must be queued
	review := nextCall(t, reviewer)
	select {
	case next := <-author.callsCh:
		t.Fatalf("author restarted while submission/review was in progress: %s", next.opts.Prompt)
	default:
	}
	if part := f.part(t, "p1"); part.State != "reviewing" {
		t.Fatalf("submission lost its reservation: %+v", part)
	}
	finishCall(review, findingHeader)
	part := f.waitPart(t, "p1", "awaiting_approval")
	if _, err := f.hub.ApprovePart(f.room.ID, "p1", part.SubmitSHA, f.base); err != nil {
		t.Fatal(err)
	}
	chat := nextCall(t, author)
	if chat.opts.Workspace != f.repo || !strings.Contains(chat.opts.Prompt, "followup must wait") {
		t.Fatal("queued followup did not return to chat after the part merged")
	}
	finishCall(chat, "chat resumed")
}

func TestWorkPartHistoryDoesNotUseChatCursor(t *testing.T) {
	f := newWorkFixture(t, 1, nil, func(h *Hub, r *Room) {
		postQueueMessage(t, h, r.ID, "BEFORE_PLAN_SECRET", "claude")
		waitIdle(t, h, r.ID)
	})
	author := controlWorkSeat(t, f, "claude")
	// A failed turn may emit its session before failing. Resetting the chat
	// session must not then replay pre-plan messages into that part session.
	postQueueMessage(t, f.hub, f.room.ID, "FAILED_PART_REQUEST", "claude")
	failed := nextCall(t, author)
	failed.emit(agent.Event{Kind: agent.KindSession, Text: "partial-part-session"})
	failed.result <- controlledResult{err: errors.New("provider failed after session creation")}
	queueSnapshot(t, f.hub, f.room.ID, func(s *Snapshot) bool { return s.Room.participant("claude").Status == "idle" })
	if err := f.hub.ResetSessions(f.room.ID); err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, f.hub, f.room.ID, "AFTER_PLAN_REQUEST", "claude")
	call := nextCall(t, author)
	if strings.Contains(call.opts.Prompt, "BEFORE_PLAN_SECRET") || !strings.Contains(call.opts.Prompt, "AFTER_PLAN_REQUEST") || !strings.Contains(call.opts.Prompt, "FAILED_PART_REQUEST") {
		t.Fatalf("wrong part delivery boundary: %s", call.opts.Prompt)
	}
	live := postQueueMessage(t, f.hub, f.room.ID, "LIVE_PART_REQUEST", "claude")
	if !live.Live {
		t.Fatal("part steer was not accepted")
	}
	if text := <-call.opts.Steer; text != "LIVE_PART_REQUEST" {
		t.Fatalf("unexpected steer: %s", text)
	}
	call.result <- controlledResult{text: "part answer", steered: 1}
	queueSnapshot(t, f.hub, f.room.ID, func(s *Snapshot) bool { return len(s.Queue) == 0 && s.Room.participant("claude").Status == "idle" })
	if _, err := f.hub.AbandonPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, f.hub, f.room.ID, "CHAT_RETURN", "claude")
	chat := nextCall(t, author)
	if !strings.Contains(chat.opts.Prompt, "AFTER_PLAN_REQUEST") || !strings.Contains(chat.opts.Prompt, "LIVE_PART_REQUEST") {
		t.Fatalf("part delivery consumed chat history: %s", chat.opts.Prompt)
	}
	finishCall(chat, "chat answer")
}

func TestWorkFirstPartTurnKeepsPriorityMessage(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	author := controlWorkSeat(t, f, "claude")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	rs.room.Work.part("p1").SessionID = ""
	f.hub.mu.Unlock()
	postQueueMessage(t, f.hub, f.room.ID, "PRIORITY_REQUEST", "claude")
	bootstrap := nextCall(t, author)
	if strings.Contains(bootstrap.opts.Prompt, "PRIORITY_REQUEST") {
		t.Fatal("first part turn must contain only plan context")
	}
	finishCall(bootstrap, "initialized")
	followup := nextCall(t, author)
	if !strings.Contains(followup.opts.Prompt, "PRIORITY_REQUEST") {
		t.Fatal("priority request was dropped by the first part turn")
	}
	finishCall(followup, "handled")
}

func TestWorkReviewDoesNotSteerChat(t *testing.T) {
	for _, who := range []string{"reviewer", "author"} {
		t.Run(who, func(t *testing.T) {
			f := newWorkFixture(t, 1, nil)
			id := "codex"
			if who == "author" {
				id = "claude"
			}
			seat := controlWorkSeat(t, f, id)
			f.hub.mu.Lock()
			rs := f.hub.rooms[f.room.ID]
			part := rs.room.Work.part("p1")
			part.State = "reviewing"
			h := f.hub
			h.runReviewStep(rs, part, ReviewStep{Who: who, Kind: "findings", Prompt: "review only"})
			h.pump(rs)
			f.hub.mu.Unlock()
			call := nextCall(t, seat)
			m := postQueueMessage(t, h, f.room.ID, "CHAT_NOT_REVIEW", id)
			if m.Live || call.opts.Steer != nil {
				t.Fatal("ordinary chat can enter the review protocol")
			}
			if _, err := h.AbandonPart(f.room.ID, "p1"); err != nil {
				t.Fatal(err)
			}
			chat := nextCall(t, seat)
			if !strings.Contains(chat.opts.Prompt, "CHAT_NOT_REVIEW") {
				t.Fatal("queued chat was not delivered after the review")
			}
			finishCall(chat, "chat answer")
		})
	}
}

func TestWorkFailedReviewStartKeepsSubmitSHA(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	sha := commitT(t, f.part(t, "p1").Worktree, "done.txt", "done\n")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	part := rs.room.Work.part("p1")
	// A mismatched diff causes Start to reject without accepting the SHA.
	f.hub.startReview(rs, part, "working", sha, &PartDiff{})
	got := part.SubmitSHA
	f.hub.mu.Unlock()
	if got != "" {
		t.Fatalf("failed review start committed the submit SHA: %s", got)
	}
	if _, err := f.hub.RetryPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	reviewer := controlWorkSeat(t, f, "codex")
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	nextCall(t, reviewer)
}

func TestWorkFailedReviewResubmitKeepsSubmitSHA(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	wt := f.part(t, "p1").Worktree
	first := commitT(t, wt, "done.txt", "first\n")
	diff, err := partDiff(f.repo, "p1", f.base, first)
	if err != nil {
		t.Fatal(err)
	}
	review := NewReviewState(3)
	if _, err := review.Start(ReviewInput{Ref: ReviewRef{PartID: "p1", BaseSHA: f.base, HeadSHA: first}, Diff: diff}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := review.Apply(findingHeader + findingOne); err != nil {
		t.Fatal(err)
	}
	if _, outcome, err := review.Apply(authorHeader + "| r1-1 | ACCEPT | fix it |\n"); err != nil || outcome != "fix" {
		t.Fatalf("author reply: %s %v", outcome, err)
	}
	second := commitT(t, wt, "done.txt", "second\n")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	part := rs.room.Work.part("p1")
	part.State, part.SubmitSHA, part.Review = "fixing", first, review
	f.hub.startReview(rs, part, "fixing", second, &PartDiff{})
	got := part.SubmitSHA
	f.hub.mu.Unlock()
	if got != first {
		t.Fatalf("failed Resubmit replaced accepted SHA %s with %s", first, got)
	}
	if _, err := f.hub.RetryPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	reviewer := controlWorkSeat(t, f, "codex")
	if _, err := f.hub.SubmitPart(f.room.ID, "p1"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, reviewer)
	if !strings.Contains(call.opts.Prompt, second) || f.part(t, "p1").Review.Phase != "recheck" {
		t.Fatal("rejected SHA could not be resubmitted for recheck")
	}
}

func TestWorkStartupPersistsApprovalWait(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	sha := commitT(t, f.part(t, "p1").Worktree, "done.txt", "done\n")
	f.hub.mu.Lock()
	part := f.hub.rooms[f.room.ID].room.Work.part("p1")
	plan := f.hub.rooms[f.room.ID].room.Work
	part.State, part.SubmitSHA, part.ReadyMode = "queued", sha, "confirm"
	plan.Queue = []string{"p1"}
	err := f.store.SaveRoom(f.hub.rooms[f.room.ID].room)
	f.hub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h := restartWorkHub(t, f)
	rooms, _, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	snap, err := h.Snapshot(f.room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rooms[0].Work.part("p1").State != "awaiting_approval" || rooms[0].Seq != snap.Room.Seq {
		t.Fatalf("startup transition was not persisted: %s disk seq %d memory seq %d", rooms[0].Work.part("p1").State, rooms[0].Seq, snap.Room.Seq)
	}
}

// This test executable doubles as an argv-only verify command. File barriers
// keep the merge between commit creation and intent persistence without sleeps
// that assume how fast Git or the machine is.
func TestWorkVerifyWaitProcess(t *testing.T) {
	args := os.Args
	if len(args) < 4 || args[len(args)-3] != "--work-verify" {
		return
	}
	ready, release := args[len(args)-2], args[len(args)-1]
	if err := os.WriteFile(ready, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(20 * time.Second)
	for {
		if _, err := os.Stat(release); err == nil {
			return
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("verify barrier was not released")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestWorkStartupPersistsMergeBeforeVerify(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ready, release := filepath.Join(dir, "ready"), filepath.Join(dir, "release")
	f := newWorkFixture(t, 1, func(s *WorkSpec) {
		s.MergeMode = "auto"
		s.Verify = []VerifyCmd{{Name: "barrier", Argv: []string{exe, "-test.run=^TestWorkVerifyWaitProcess$", "--", "--work-verify", ready, release}, TimeoutSec: 30}}
	})
	sha := commitT(t, f.part(t, "p1").Worktree, "done.txt", "done\n")
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	part, plan := rs.room.Work.part("p1"), rs.room.Work
	part.State, part.SubmitSHA, part.ReadyMode = "queued", sha, "auto"
	plan.Queue = []string{"p1"}
	err = f.store.SaveRoom(rs.room)
	f.hub.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h := restartWorkHub(t, f)
	t.Cleanup(func() {
		if err := os.WriteFile(release, []byte("release"), 0o600); err != nil {
			t.Error(err)
		}
	})
	deadline := time.After(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-deadline:
			t.Fatal("startup merge did not reach verify")
		case <-time.After(5 * time.Millisecond):
		}
	}
	rooms, _, err := f.store.LoadRooms()
	if err != nil {
		t.Fatal(err)
	}
	if disk := rooms[0].Work; disk.part("p1").State != "merging" || disk.Merging != nil {
		t.Fatalf("startup did not persist pre-intent merging state: %+v", disk)
	}
	// Recover exactly the persisted pre-intent state, as a new process would.
	recovered := &roomState{room: rooms[0]}
	if err := h.recoverWork(recovered); err != nil {
		t.Fatal(err)
	}
	if p := recovered.room.Work.part("p1"); p.State != "blocked" || p.Block.Kind != "restart" {
		t.Fatalf("restart would automatically repeat the merge: %+v", p)
	}
}

func TestWorkNativePartSessionPausesSeat(t *testing.T) {
	f := newWorkFixture(t, 1, nil)
	author := controlWorkSeat(t, f, "claude")
	snap, _ := f.hub.Snapshot(f.room.ID)
	var messageID string
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Part == "p1" {
			messageID = m.ID
		}
	}
	if messageID == "" {
		t.Fatal("no recorded part turn")
	}
	_, opts, paused, err := f.hub.DetachNativeSession(f.room.ID, "claude", messageID)
	if err != nil {
		t.Fatal(err)
	}
	if !paused || opts.SessionID != f.part(t, "p1").SessionID {
		t.Fatal("native part session did not pause its author")
	}
	enabled := true
	if _, err := f.hub.UpdateRoom(f.room.ID, RoomPatch{Participants: []ParticipantPatch{{ID: "claude", Enabled: &enabled}}}); err == nil {
		t.Fatal("author could be enabled while its part session is open")
	}
	f.hub.mu.Lock()
	rs := f.hub.rooms[f.room.ID]
	rs.room.participant("claude").Enabled = true // independently exercise pump's session reservation
	rs.enqueue(&Turn{ID: newID(), Agent: "claude", Reason: "part", Part: "p1", Status: "queued"})
	f.hub.pump(rs)
	running := len(rs.running)
	f.hub.mu.Unlock()
	if running != 0 || len(author.callsCh) != 0 {
		t.Fatal("pump reused a session reserved for its native CLI")
	}
	f.hub.ReleaseNativeSession("claude", opts.SessionID)
}
