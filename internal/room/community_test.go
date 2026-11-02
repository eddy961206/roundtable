package room

import (
	"context"
	"errors"
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
// proposed card the person approves, and only then is it a current decision.
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
	if got := h.rooms[r.ID].activeDecisionTexts(r.Seq); len(got) != 1 {
		t.Fatalf("an approved result is a current decision: %v", got)
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
		job, err := h.RemoteNext(context.Background(), token, 3*time.Second)
		if err != nil {
			t.Error(err)
		}
		polled <- job
	}()
	waitFor(t, func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.remote[remoteKey(r.ID, seat)] != nil && h.remote[remoteKey(r.ID, seat)].polling == 1 })
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
	if err := h.RemoteFinish(token, "other-turn", RemoteResult{Text: "x"}); err == nil {
		t.Fatal("a result for another turn was accepted")
	}
	if err := h.RemoteFinish(token, job.Turn, RemoteResult{Text: "원격에서 답함", SessionID: "s-remote", Usage: map[string]any{"input_tokens": 50.0, "cache_read_input_tokens": 950.0}}); err != nil {
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
	job, err := h.RemoteNext(context.Background(), token, 3*time.Second)
	if err != nil || job == nil || !strings.Contains(job.Prompt, "자리 비운 사이 질문") {
		t.Fatalf("catch-up job %+v %v", job, err)
	}
	if err := h.RemoteFinish(token, job.Turn, RemoteResult{Text: "늦게 답함"}); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	again, err := h.RemoteNext(context.Background(), token, 0)
	if err != nil || again != nil {
		t.Fatalf("the catch-up runs once: %+v %v", again, err)
	}
	reissued, err := h.IssueRemoteToken(r.ID, seat)
	if err != nil || reissued == token {
		t.Fatal(err)
	}
	if _, err := h.RemoteNext(context.Background(), token, 0); !errors.Is(err, ErrRemoteAuth) {
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
