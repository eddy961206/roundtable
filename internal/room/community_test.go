package room

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"roundtable/internal/agent"
)

// A community space starts with the conservative guards of the 2026-11-02
// proposal ("five minutes per post"); an ordinary room keeps none.
func TestSpaceDefaults(t *testing.T) {
	h, plain := newTestHub(t, &fakeProvider{id: "codex"})
	if plain.Kind != "" || plain.SpeakIntervalSeconds != 0 || plain.DailySpeakLimit != 0 || plain.SeatInputBudget != 0 {
		t.Fatalf("an ordinary room got space guards: %+v", plain)
	}
	lounge, err := h.CreateRoom(RoomSpec{Title: "휴게실", Kind: KindLounge, Participants: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if lounge.Mode != agent.ModeChat || !lounge.Persona || lounge.MaxChain != 2 || lounge.SpeakIntervalSeconds != 300 || lounge.DailySpeakLimit != 12 || lounge.SeatInputBudget != 1_000_000 || lounge.RoomInputBudget != 5_000_000 {
		t.Fatalf("lounge defaults: %+v", lounge)
	}
	work, err := h.CreateRoom(RoomSpec{Title: "작업실", Kind: KindWork, Workspace: t.TempDir(), Participants: []string{"codex"}, SpeakIntervalSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	if work.Mode != agent.ModeRead || work.SpeakIntervalSeconds != 60 || work.DailySpeakLimit != 24 || work.MaxChain != defaultMaxChain || work.SeatInputBudget != 5_000_000 {
		t.Fatalf("workroom defaults (an explicit interval wins): %+v", work)
	}
	if _, err := h.CreateRoom(RoomSpec{Title: "x", Kind: "party", Participants: []string{"codex"}}); err == nil {
		t.Fatal("an unknown space kind was accepted")
	}
	if _, err := h.UpdateRoom(work.ID, RoomPatch{DailySpeakLimit: ref(-1)}); err == nil {
		t.Fatal("a negative daily limit was accepted")
	}
	rooms := h.Rooms()
	kinds := map[string]string{}
	for _, s := range rooms {
		kinds[s.ID] = s.Kind
	}
	if kinds[lounge.ID] != KindLounge || kinds[work.ID] != KindWork {
		t.Fatalf("room list must carry the space kind: %+v", rooms)
	}
}

// A turn another seat started waits for the speak interval since that seat's
// last reply; a person's direct message does not.
func TestSpeakIntervalPacesChainedTurns(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{SpeakIntervalSeconds: ref(1)}); err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, h, r.ID, "warm up", "codex")
	finishCall(nextCall(t, ps[1]), "ok")
	waitIdle(t, h, r.ID)
	replied := time.Now()
	postQueueMessage(t, h, r.ID, "ask codex", "claude")
	finishCall(nextCall(t, ps[0]), "@codex 확인해 줘")
	noMoreCalls(t, ps[1]) // 300 ms: still inside the 1 s interval
	snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return len(s.Queue) == 1 })
	if p := snap.Room.participant("codex"); p.PacedUntil == 0 {
		t.Fatalf("the waiting seat must show until when: %+v", p)
	}
	call := nextCall(t, ps[1])
	if waited := time.Since(replied); waited < 900*time.Millisecond {
		t.Fatalf("the chained turn started %s after the seat's reply", waited)
	}
	finishCall(call, "확인함")
	waitIdle(t, h, r.ID)
	postQueueMessage(t, h, r.ID, "direct", "codex")
	start := time.Now()
	finishCall(nextCall(t, ps[1]), "바로 답함")
	if time.Since(start) > 700*time.Millisecond {
		t.Fatal("a person's direct message waited for the interval")
	}
	snap, _ = h.Snapshot(r.ID)
	if p := snap.Room.participant("codex"); p.PacedUntil != 0 {
		t.Fatalf("a started turn must clear the wait: %+v", p)
	}
}

func TestDailySpeakLimitHoldsSeat(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{DailySpeakLimit: ref(1)}); err != nil {
		t.Fatal(err)
	}
	runOnce(t, h, r, ps[0], "first", nil)
	postQueueMessage(t, h, r.ID, "second", "codex")
	noMoreCalls(t, ps[0])
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if countSystem(snap, "24시간 발화가 상한 1건") != 1 {
		t.Fatalf("the hold must be explained: %+v", snap.Messages)
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{DailySpeakLimit: ref(5)}); err != nil {
		t.Fatal(err)
	}
	call := runOnce(t, h, r, ps[0], "third", nil)
	if !strings.Contains(call.opts.Prompt, "second") || !strings.Contains(call.opts.Prompt, "third") {
		t.Fatalf("held messages must reach the seat once the limit rises: %s", call.opts.Prompt)
	}
}

func TestAgentsPausedStartsNoTurn(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AgentsPaused: ref(true)}); err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, h, r.ID, "anyone?", "")
	noMoreCalls(t, ps[0])
	noMoreCalls(t, ps[1])
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if countSystem(snap, "발화가 꺼져 있음") != 2 {
		t.Fatalf("each held seat is told once: %+v", snap.Messages)
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AgentsPaused: ref(false)}); err != nil {
		t.Fatal(err)
	}
	call := runOnce(t, h, r, ps[1], "back", nil)
	if !strings.Contains(call.opts.Prompt, "anyone?") {
		t.Fatalf("the message sent while paused must be delivered later: %s", call.opts.Prompt)
	}
}

// A person's task is a request from "user"; the seat's done signal leaves a
// proposed card the person approves. The approved card stays on the card list,
// out of the seats' standing brief.
func TestAssignedTaskResultWaitsForApproval(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	m, task, err := h.AssignTask(r.ID, "codex", "README 오타 고치기")
	if err != nil {
		t.Fatal(err)
	}
	if m.Target != "codex" || len(m.Requests) != 1 || task.From != "user" || task.SourceMsg != m.ID {
		t.Fatalf("task message %+v request %+v", m, task)
	}
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "[요청 "+task.ID+" from @user: README 오타 고치기]") {
		t.Fatalf("the seat must get the task with its id: %s", call.opts.Prompt)
	}
	finishCall(call, "고쳤어\n[[REQUEST_DONE:"+task.ID+"]]")
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if snap.Requests[0].Status != "done" || len(snap.Decisions) != 1 {
		t.Fatalf("requests %+v decisions %+v", snap.Requests, snap.Decisions)
	}
	card := snap.Decisions[0]
	if card.Status != "proposed" || card.Task != task.ID || card.By != "codex" || !strings.Contains(card.Text, "README 오타 고치기") {
		t.Fatalf("result card: %+v", card)
	}
	if got := h.rooms[r.ID].activeDecisionTexts(r.Seq); len(got) != 0 {
		t.Fatalf("an unapproved result entered the brief: %v", got)
	}
	if _, err := h.UpdateDecision(r.ID, card.ID, DecisionInput{Status: ref("active")}); err != nil {
		t.Fatal(err)
	}
	snap, _ = h.Snapshot(r.ID)
	if snap.Decisions[0].Status != "active" {
		t.Fatalf("the approval must be recorded on the card: %+v", snap.Decisions[0])
	}
	// One seat's finished work is not a rule for every seat (review 2026-11-03).
	if got := h.rooms[r.ID].activeDecisionTexts(r.Seq); len(got) != 0 {
		t.Fatalf("an approved result entered every seat's brief: %v", got)
	}
	if _, _, err := h.AssignTask(r.ID, "codex", "두 줄\n과제"); err == nil {
		t.Fatal("a multi-line task was accepted")
	}
	if _, _, err := h.AssignTask(r.ID, "nobody", "x"); err == nil {
		t.Fatal("a task for a missing seat was accepted")
	}
}

func remoteHub(t *testing.T) (*Hub, *Room, string) {
	t.Helper()
	h, _ := newTestHub(t, &fakeProvider{id: "codex"})
	h.reg.Add(NewRemoteProvider())
	r, err := h.CreateRoom(RoomSpec{Title: "작업실", Kind: KindWork, Workspace: t.TempDir(), ParticipantSpecs: []ParticipantSpec{{Provider: RemoteProvider, Label: "eddy"}}})
	if err != nil {
		t.Fatal(err)
	}
	token, err := h.IssueRemoteToken(r.ID, r.Participants[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Stop(r.ID) })
	return h, r, token
}

func TestRemoteSeatRunsOnWorker(t *testing.T) {
	h, r, token := remoteHub(t)
	seat := r.Participants[0].ID
	me, err := h.RemoteMe(token)
	if err != nil || me.Seat != seat || me.Room != r.ID {
		t.Fatalf("me %+v %v", me, err)
	}
	if _, err := h.RemoteMe("rtc_wrong"); !errors.Is(err, ErrRemoteAuth) {
		t.Fatalf("a wrong token: %v", err)
	}
	polled := make(chan *RemoteJob, 1)
	go func() {
		job, err := h.RemoteNext(context.Background(), token, "w1", 3*time.Second)
		if err != nil {
			t.Error(err)
		}
		polled <- job
	}()
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.remote[remoteKey(r.ID, seat)] != nil && h.remote[remoteKey(r.ID, seat)].polling == 1
	})
	if _, err := h.PostUser(r.ID, "원격 좌석에게", seat); err != nil {
		t.Fatal(err)
	}
	job := <-polled
	if job == nil || !strings.Contains(job.Prompt, "원격 좌석에게") || job.Mode != agent.ModeRead || job.SessionID != "" {
		t.Fatalf("job %+v", job)
	}
	if !strings.Contains(job.Prompt, "참가자 본인의 PC에서 실행") || strings.Contains(job.Prompt, r.Workspace) {
		t.Fatalf("a remote seat's brief must not name this server's folder: %s", job.Prompt)
	}
	if err := h.RemoteFinish(token, "w1", "other-turn", RemoteResult{Text: "x"}); err == nil {
		t.Fatal("a result for another turn was accepted")
	}
	if err := h.RemoteFinish(token, "w1", job.Turn, RemoteResult{Text: "원격에서 답함", SessionID: "s-remote", Usage: map[string]any{"input_tokens": 50.0, "cache_read_input_tokens": 950.0}}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if got := agentTexts(snap, seat); len(got) != 1 || got[0] != "done:원격에서 답함" {
		t.Fatalf("replies %v", got)
	}
	p := snap.Room.participant(seat)
	if p.SessionID != "s-remote" || p.InputAll != 1000 || p.RemoteSeenAt == 0 {
		t.Fatalf("session, usage and last poll: %+v", p)
	}
}

// Turns dropped while no worker polls are answered once, by the first poll
// after the worker comes back.
func TestRemoteSeatCatchesUpAfterAway(t *testing.T) {
	h, r, token := remoteHub(t)
	seat := r.Participants[0].ID
	if _, err := h.PostUser(r.ID, "자리 비운 사이 질문", seat); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if countSystem(snap, "worker가 연결돼 있지 않음") != 1 || !snap.Room.participant(seat).RemoteMissed {
		t.Fatalf("an away worker must be explained: %+v", snap.Messages)
	}
	job, err := h.RemoteNext(context.Background(), token, "w1", 3*time.Second)
	if err != nil || job == nil || !strings.Contains(job.Prompt, "자리 비운 사이 질문") {
		t.Fatalf("catch-up job %+v %v", job, err)
	}
	if err := h.RemoteFinish(token, "w1", job.Turn, RemoteResult{Text: "늦게 답함"}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	again, err := h.RemoteNext(context.Background(), token, "w1", 0)
	if err != nil || again != nil {
		t.Fatalf("the catch-up runs once: %+v %v", again, err)
	}
	reissued, err := h.IssueRemoteToken(r.ID, seat)
	if err != nil || reissued == token {
		t.Fatal(err)
	}
	if _, err := h.RemoteNext(context.Background(), token, "w1", 0); !errors.Is(err, ErrRemoteAuth) {
		t.Fatalf("a replaced token must stop working: %v", err)
	}
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.RemoteMe(reissued); err != nil {
		t.Fatalf("tokens survive a restart: %v", err)
	}
	if _, err := h.IssueRemoteToken(r.ID, "nobody"); err == nil {
		t.Fatal("a token for a missing seat")
	}
}

// PR #4 review (P1): a poll that started with a token re-issued while it
// waited must not receive the next job. Rotation revokes waiting polls too.
func TestReissuedTokenRevokesWaitingPoll(t *testing.T) {
	h, r, old := remoteHub(t)
	seat := r.Participants[0].ID
	polled := make(chan error, 1)
	go func() {
		job, err := h.RemoteNext(context.Background(), old, "w1", 3*time.Second)
		if job != nil {
			err = errors.New("the revoked poll got job " + job.Turn)
		}
		polled <- err
	}()
	waitFor(t, func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		sl := h.remote[remoteKey(r.ID, seat)]
		return sl != nil && sl.polling == 1
	})
	fresh, err := h.IssueRemoteToken(r.ID, seat)
	if err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.rooms[r.ID].room.participant(seat).RemoteSeenAt = nowMs() // the seat still counts as online
	h.mu.Unlock()
	if _, err := h.PostUser(r.ID, "새 토큰 이후의 일", seat); err != nil {
		t.Fatal(err)
	}
	if err := <-polled; !errors.Is(err, ErrRemoteAuth) {
		t.Fatalf("the waiting poll of a replaced token: %v", err)
	}
	job, err := h.RemoteNext(context.Background(), fresh, "w1", 3*time.Second)
	if err != nil || job == nil || !strings.Contains(job.Prompt, "새 토큰 이후의 일") {
		t.Fatalf("the new token must still get the job: %+v %v", job, err)
	}
	if err := h.RemoteFinish(fresh, "w1", job.Turn, RemoteResult{Text: "새 worker가 답함"}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
}

// PR #4 review (P2): a task for a seat that is running a steerable turn waits
// for its own turn. Injected as live steering, the seat would never get the
// request id and the task could not be finished.
func TestTaskIsNotLiveSteering(t *testing.T) {
	h, r, ps := controlledHub(t, "claude")
	ps[0].steer = true
	postQueueMessage(t, h, r.ID, "먼저 하던 일", "claude")
	first := nextCall(t, ps[0])
	m, task, err := h.AssignTask(r.ID, "claude", "실행 중에 맡긴 과제")
	if err != nil {
		t.Fatal(err)
	}
	if m.Live {
		t.Fatal("a task was injected into the running turn")
	}
	finishCall(first, "먼저 하던 일 끝")
	second := nextCall(t, ps[0])
	if !strings.Contains(second.opts.Prompt, "[요청 "+task.ID+" from @user") {
		t.Fatalf("the next turn must carry the task's request id: %s", second.opts.Prompt)
	}
	finishCall(second, "했어\n[[REQUEST_DONE:"+task.ID+"]]")
	waitIdle(t, h, r.ID)
	if got := h.rooms[r.ID].request(task.ID).Status; got != "done" {
		t.Fatalf("task status %s", got)
	}
}

// A worker's progress reports stream into the reply like a local CLI's
// deltas; once the turn is stopped here, the next report tells the worker so.
func TestRemoteProgressStreamsAndStopReachesWorker(t *testing.T) {
	h, r, token := remoteHub(t)
	seat := r.Participants[0].ID
	h.mu.Lock()
	h.rooms[r.ID].room.participant(seat).RemoteSeenAt = nowMs()
	h.mu.Unlock()
	if _, err := h.PostUser(r.ID, "길게 생각해 줘", seat); err != nil {
		t.Fatal(err)
	}
	job, err := h.RemoteNext(context.Background(), token, "w1", 3*time.Second)
	if err != nil || job == nil {
		t.Fatalf("%+v %v", job, err)
	}
	streaming := func() string {
		snap, _ := h.Snapshot(r.ID)
		for _, m := range snap.Messages {
			if m.Kind == "agent" && m.Status == "streaming" {
				return m.Text
			}
		}
		return "(none)"
	}
	if err := h.RemoteProgress(token, "w1", job.Turn, "첫 문단을"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return streaming() == "첫 문단을" })
	if err := h.RemoteProgress(token, "w1", job.Turn, "첫 문단을 쓰는 중"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return streaming() == "첫 문단을 쓰는 중" })
	if err := h.Stop(r.ID); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	if err := h.RemoteProgress(token, "w1", job.Turn, "더 쓴 내용"); !errors.Is(err, ErrRemoteTurnGone) {
		t.Fatalf("a stopped turn's progress must tell the worker to stop: %v", err)
	}
}

// Review 2026-11-03 (Sol61, Argon4): a second worker started with the same
// token must not run a turn the first one is running, and a stopped poll must
// not claim it. A worker that stops reporting loses the turn to the next poll.
func TestRemoteClaimLeaseKeepsOneWorker(t *testing.T) {
	h, r, token := remoteHub(t)
	seat := r.Participants[0].ID
	h.mu.Lock()
	h.rooms[r.ID].room.participant(seat).RemoteSeenAt = nowMs()
	h.mu.Unlock()
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if job, err := h.RemoteNext(gone, token, "w0", 0); job != nil || err == nil {
		t.Fatalf("a stopped poll got %+v %v", job, err)
	}
	if _, err := h.PostUser(r.ID, "한 번만 실행해 줘", seat); err != nil {
		t.Fatal(err)
	}
	job, err := h.RemoteNext(context.Background(), token, "w1", 3*time.Second)
	if err != nil || job == nil {
		t.Fatalf("%+v %v", job, err)
	}
	if again, err := h.RemoteNext(context.Background(), token, "w2", 0); err != nil || again != nil {
		t.Fatalf("a second worker took the running turn: %+v %v", again, err)
	}
	if err := h.RemoteProgress(token, "w2", job.Turn, "끼어듦"); !errors.Is(err, ErrRemoteTurnGone) {
		t.Fatalf("a second worker's report was taken: %v", err)
	}
	if err := h.RemoteProgress(token, "w1", job.Turn, ""); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.remote[remoteKey(r.ID, seat)].beat = time.Now().Add(-remoteLease) // w1 went quiet
	h.mu.Unlock()
	taken, err := h.RemoteNext(context.Background(), token, "w2", 0)
	if err != nil || taken == nil || taken.Turn != job.Turn {
		t.Fatalf("a restarted worker must take the turn over: %+v %v", taken, err)
	}
	if err := h.RemoteFinish(token, "w1", job.Turn, RemoteResult{Text: "옛 worker"}); !errors.Is(err, ErrRemoteTurnGone) {
		t.Fatalf("the old worker's answer was taken: %v", err)
	}
	if err := h.RemoteFinish(token, "w2", job.Turn, RemoteResult{Text: "새 worker가 답함"}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if last := snap.Messages[len(snap.Messages)-1]; last.Text != "새 worker가 답함" {
		t.Fatalf("reply %q", last.Text)
	}
	if job, err := h.RemoteNext(context.Background(), token, "w2", 0); err != nil || job != nil {
		t.Fatalf("%+v %v", job, err)
	}
	h.mu.Lock()
	slots := len(h.remote)
	h.remoteTokens["stale"] = remoteToken{Room: "deleted-room", Seat: "x"}
	h.mu.Unlock()
	if slots != 0 {
		t.Fatalf("an idle seat keeps %d slots in memory", slots)
	}
	if _, err := h.IssueRemoteToken(r.ID, seat); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	_, stale := h.remoteTokens["stale"]
	h.mu.Unlock()
	if stale {
		t.Fatal("a deleted room's token stayed after the next issue")
	}
}

// Auto chat is off until a person turns it on; then a space quiet for the set
// minutes, with someone watching, gets one line from the seat that spoke least
// recently, which calls nobody. Each unanswered line doubles the quiet time,
// and after three it waits for a person. Nothing happens outside the hours or
// while agents are paused.
func TestAutoChatSpeaksInQuietSpaceOnly(t *testing.T) {
	h, r, ps := controlledHub(t, "claude", "codex")
	later := func(d time.Duration) time.Time { return time.Now().Add(d) }
	h.AutoChatTick(later(time.Hour))
	noMoreCalls(t, ps[0]) // off by default
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AutoChatMinutes: ref(10)}); err != nil {
		t.Fatal(err)
	}
	h.AutoChatTick(later(11 * time.Minute))
	noMoreCalls(t, ps[0]) // nobody has the space open
	_, unsub, err := h.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	h.AutoChatTick(later(5 * time.Minute))
	noMoreCalls(t, ps[0]) // not quiet long enough
	h.AutoChatTick(later(11 * time.Minute))
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "자율 잡담") {
		t.Fatalf("the auto turn must say why it speaks: %s", call.opts.Prompt)
	}
	finishCall(call, "@codex 커피 한잔 어때요")
	reply := nextCall(t, ps[1]) // the called seat answers once
	if !strings.Contains(reply.opts.Prompt, "(자율 대화) @claude가") {
		t.Fatalf("the called seat must know who talked to it: %s", reply.opts.Prompt)
	}
	finishCall(reply, "@claude 좋아, 라떼로")
	noMoreCalls(t, ps[0]) // the answer calls nobody
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if n := len(snap.Messages); snap.Messages[n-2].Reason != "auto" || snap.Messages[n-1].Reason != "auto-reply" {
		t.Fatalf("the line and its answer must be marked as auto chat: %+v", snap.Messages[n-2:])
	}
	h.AutoChatTick(later(11 * time.Minute))
	noMoreCalls(t, ps[0]) // the seats' own answer is no answer: 20 minutes now
	noMoreCalls(t, ps[1])
	h.AutoChatTick(later(21 * time.Minute))
	finishCall(nextCall(t, ps[0]), "좋아요") // the seat that spoke least recently
	waitIdle(t, h, r.ID)

	hour := later(41 * time.Minute).Hour()
	away := fmt.Sprintf("%d-24", hour+1)
	if hour == 23 {
		away = "0-23"
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AutoChatHours: ref(away)}); err != nil {
		t.Fatal(err)
	}
	h.AutoChatTick(later(41 * time.Minute))
	noMoreCalls(t, ps[0])
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AutoChatHours: ref(""), AgentsPaused: ref(true)}); err != nil {
		t.Fatal(err)
	}
	h.AutoChatTick(later(41 * time.Minute))
	noMoreCalls(t, ps[0])
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AgentsPaused: ref(false)}); err != nil {
		t.Fatal(err)
	}
	h.AutoChatTick(later(31 * time.Minute))
	noMoreCalls(t, ps[0]) // two unanswered lines: 40 minutes
	h.AutoChatTick(later(41 * time.Minute))
	finishCall(nextCall(t, ps[1]), "커피 마시는 중")
	waitIdle(t, h, r.ID)
	h.AutoChatTick(later(24 * time.Hour))
	noMoreCalls(t, ps[0]) // three lines nobody answered: wait for a person
	noMoreCalls(t, ps[1])

	h.mu.Lock()
	rs := h.rooms[r.ID]
	free := h.autoChatSeat(rs, later(3*time.Hour).UnixMilli())
	rs.room.DailySpeakLimit = 2 // auto lines and answers may use one of each seat's two
	spent := h.autoChatSeat(rs, later(3*time.Hour).UnixMilli())
	rs.room.DailySpeakLimit = 0
	h.mu.Unlock()
	if free == nil || spent != nil {
		t.Fatalf("auto chat must leave half of the daily limit to people: %v %v", free, spent)
	}
	for _, bad := range []RoomPatch{{AutoChatMinutes: ref(5)}, {AutoChatHours: ref("18-9")}, {AutoChatHours: ref("아침")}} {
		if _, err := h.UpdateRoom(r.ID, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

// Review 2026-11-03 (Opus55, GLM): a line of chat reads the latest talk, not
// the whole backlog of a seat that has no session yet.
func TestAutoChatReadsOnlyRecentTalk(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.UpdateRoom(r.ID, RoomPatch{AutoChatMinutes: ref(10)}); err != nil {
		t.Fatal(err)
	}
	_, unsub, err := h.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	h.mu.Lock()
	rs := h.rooms[r.ID]
	for i := 0; i < 20; i++ {
		rs.nextMessage("system", "system", fmt.Sprintf("옛 이야기 %02d", i))
	}
	h.mu.Unlock()
	h.AutoChatTick(time.Now().Add(11 * time.Minute))
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "옛 이야기 19") || strings.Contains(call.opts.Prompt, "옛 이야기 07") {
		t.Fatalf("auto chat history: %s", call.opts.Prompt)
	}
	finishCall(call, "네")
	waitIdle(t, h, r.ID)
}

// Review 2026-11-03 (Opus55): an approved task result is one seat's work, not
// a room rule. It must not join every seat's standing brief.
func TestApprovedTaskStaysOutOfBriefs(t *testing.T) {
	h, r, _ := controlledHub(t, "codex")
	h.mu.Lock()
	rs := h.rooms[r.ID]
	rs.decisions = append(rs.decisions,
		Decision{ID: "d1", Text: "보존 기간은 30일", Status: "active", At: 1},
		Decision{ID: "d2", Text: "과제 결과: 디스크 경고 설계", Status: "active", Task: "q1", At: 2})
	got := rs.activeDecisionTexts(rs.room.Seq)
	h.mu.Unlock()
	if len(got) != 1 || got[0] != "보존 기간은 30일" {
		t.Fatalf("brief decisions %q", got)
	}
}

// A person on another PC is named to the seats, so a space with several
// people does not read as one "User".
func TestPersonMessageNamesItsAuthor(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostAs(r.ID, "보존 기간 검토해 줘", "codex", "부장님"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "User (부장님)] 보존 기간 검토해 줘") {
		t.Fatalf("the seat must see who asked: %s", call.opts.Prompt)
	}
	finishCall(call, "네")
	waitIdle(t, h, r.ID)
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
