package room

import (
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func forkSpec(provider, room, seat string) ParticipantSpec {
	return ParticipantSpec{Provider: provider, ForkFrom: &ForkRef{Room: room, Seat: seat}}
}

func TestForkSeatCopiesBaseSessionOnFirstTurn(t *testing.T) {
	h, base, ps := controlledHub(t, "claude")
	postQueueMessage(t, h, base.ID, "공통 문서를 읽어", "claude")
	read := nextCall(t, ps[0])
	usage := map[string]any{"session_cost_usd": 0.40}
	read.emit(agent.Event{Kind: agent.KindUsage, Meta: usage})
	read.result <- controlledResult{text: "준비됨", usage: usage}
	waitIdle(t, h, base.ID)

	task, err := h.CreateRoom(RoomSpec{Title: "작업", Mode: agent.ModeChat, ParticipantSpecs: []ParticipantSpec{forkSpec("claude", base.ID, "claude")}})
	if err != nil {
		t.Fatal(err)
	}
	seat := task.Participants[0]
	if seat.Fork == nil || *seat.Fork != (ForkSource{Room: base.ID, Seat: "claude", Session: "claude-session"}) || seat.Model != "m" {
		t.Fatalf("fork seat: %+v fork %+v", seat, seat.Fork)
	}
	postQueueMessage(t, h, task.ID, "작업 1", seat.ID)
	first := nextCall(t, ps[0])
	if first.opts.ForkSession != "claude-session" || first.opts.SessionID != "" {
		t.Fatalf("first turn must fork, got session %q fork %q", first.opts.SessionID, first.opts.ForkSession)
	}
	if !strings.Contains(first.opts.Prompt, "기반 세션을 복사") || !strings.Contains(first.opts.Prompt, "규칙:") || !strings.Contains(first.opts.Prompt, "작업 1") {
		t.Fatalf("forked first turn needs the full brief and the fork note:\n%s", first.opts.Prompt)
	}
	// The fork reports the base session's running cost plus its own.
	usage = map[string]any{"session_cost_usd": 0.45}
	first.emit(agent.Event{Kind: agent.KindUsage, Meta: usage})
	first.result <- controlledResult{text: "작업 1 끝", usage: usage}
	waitIdle(t, h, task.ID)

	postQueueMessage(t, h, task.ID, "작업 1 보완", seat.ID)
	second := nextCall(t, ps[0])
	if second.opts.ForkSession != "" || second.opts.SessionID != "claude-session" || strings.Contains(second.opts.Prompt, "기반 세션을 복사") {
		t.Fatalf("later turns resume the fork, got session %q fork %q", second.opts.SessionID, second.opts.ForkSession)
	}
	finishCall(second, "보완 끝")
	waitIdle(t, h, task.ID)

	snap, _ := h.Snapshot(task.ID)
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Text == "작업 1 끝" {
			if cost := m.Usage["total_cost_usd"].(float64); cost < 0.0499 || cost > 0.0501 {
				t.Fatalf("fork turn cost %v, want 0.05", cost)
			}
		}
	}
	if err := h.ResetSessions(task.ID); err != nil {
		t.Fatal(err)
	}
	snap, _ = h.Snapshot(task.ID)
	if p := snap.Room.participant(seat.ID); p.Fork != nil || p.SessionID != "" {
		t.Fatalf("reset must start a fresh session, not the fork: %+v", p)
	}
}

func TestForkSeatValidation(t *testing.T) {
	h, base, ps := controlledHub(t, "claude", "codex")
	create := func(mode, workspace string, spec ParticipantSpec) error {
		_, err := h.CreateRoom(RoomSpec{Title: "작업", Mode: mode, Workspace: workspace, ParticipantSpecs: []ParticipantSpec{spec}})
		return err
	}
	if err := create(agent.ModeChat, "", forkSpec("claude", base.ID, "claude")); err == nil || !strings.Contains(err.Error(), "아직 세션이 없음") {
		t.Fatalf("a base seat without a session: %v", err)
	}
	postQueueMessage(t, h, base.ID, "읽어", "claude")
	finishCall(nextCall(t, ps[0]), "준비됨")
	waitIdle(t, h, base.ID)

	other := "x"
	cases := []struct {
		name, mode, workspace string
		spec                  ParticipantSpec
		want                  string
	}{
		{"missing room", agent.ModeChat, "", forkSpec("claude", "nope", "claude"), "기반 방 없음"},
		{"missing seat", agent.ModeChat, "", forkSpec("claude", base.ID, "nope"), "좌석 nope가 없음"},
		{"provider", agent.ModeChat, "", forkSpec("codex", base.ID, "claude"), "공급자는 claude"},
		{"model", agent.ModeChat, "", ParticipantSpec{Provider: "claude", Model: other, ForkFrom: &ForkRef{Room: base.ID, Seat: "claude"}}, "모델은 기반 좌석과 같은"},
	}
	for _, c := range cases {
		if err := create(c.mode, c.workspace, c.spec); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	ps[0].noFork = true
	if err := create(agent.ModeChat, "", forkSpec("claude", base.ID, "claude")); err == nil || !strings.Contains(err.Error(), "포크를 지원하지 않음") {
		t.Fatalf("provider without fork: %v", err)
	}
}

func TestForkFailsWhileBaseSeatRuns(t *testing.T) {
	h, base, ps := controlledHub(t, "claude")
	postQueueMessage(t, h, base.ID, "읽어", "claude")
	finishCall(nextCall(t, ps[0]), "준비됨")
	waitIdle(t, h, base.ID)
	task, err := h.CreateRoom(RoomSpec{Title: "작업", Mode: agent.ModeChat, ParticipantSpecs: []ParticipantSpec{forkSpec("claude", base.ID, "claude")}})
	if err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, h, base.ID, "더 읽어", "claude")
	running := nextCall(t, ps[0])
	postQueueMessage(t, h, task.ID, "작업", task.Participants[0].ID)
	snap := queueSnapshot(t, h, task.ID, func(s *Snapshot) bool {
		for _, m := range s.Messages {
			if m.Kind == "agent" && m.Status == "error" {
				return true
			}
		}
		return false
	})
	for _, m := range snap.Messages {
		if m.Kind == "agent" && !strings.Contains(m.Error, "실행 중이라 세션을 포크할 수 없음") {
			t.Fatalf("fork during a base turn: %+v", m)
		}
	}
	finishCall(running, "끝")
	waitIdle(t, h, base.ID)
}

// Claude, Codex and OpenCode fork across directories (checked 2026-10-27); a
// fork in another folder is told where the copied context was read.
func TestForkIntoAnotherDirectoryNamesBothFolders(t *testing.T) {
	h, base, ps := controlledHub(t, "claude")
	postQueueMessage(t, h, base.ID, "읽어", "claude")
	finishCall(nextCall(t, ps[0]), "준비됨")
	waitIdle(t, h, base.ID)
	dir := t.TempDir()
	task, err := h.CreateRoom(RoomSpec{Title: "작업", Mode: agent.ModeWrite, Workspace: dir, ParticipantSpecs: []ParticipantSpec{forkSpec("claude", base.ID, "claude")}})
	if err != nil {
		t.Fatal(err)
	}
	if task.Participants[0].Fork.Dir != "" {
		t.Fatalf("a chat base records no folder: %+v", task.Participants[0].Fork)
	}
	postQueueMessage(t, h, task.ID, "작업", task.Participants[0].ID)
	call := nextCall(t, ps[0])
	if call.opts.ForkSession != "claude-session" || !strings.Contains(call.opts.Prompt, "채팅 모드(작업 폴더 없음)였고, 지금 작업 폴더는 "+task.Workspace) {
		t.Fatalf("moved fork prompt:\n%s", call.opts.Prompt)
	}
	finishCall(call, "끝")
	waitIdle(t, h, task.ID)
}
