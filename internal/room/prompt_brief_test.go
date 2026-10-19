package room

import (
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestBriefedPromptOmitsStandingBrief(t *testing.T) {
	r := &Room{Title: "설계", Mode: "read", Workspace: "ws", Note: "결정은 사람이 한다",
		Participants: []*Participant{{ID: "claude-a", Provider: "claude", Label: "Opus", Enabled: true}, {ID: "codex-b", Provider: "codex", Label: "Astra", Enabled: true}}}
	me := r.Participants[0]
	hist := []*Message{{Kind: "user", Author: "user", Text: "다음 질문", Status: "done"}}
	full := BuildPrompt(r, me, hist, turnContext{Reason: "mention"})
	short := BuildPrompt(r, me, hist, turnContext{Reason: "mention", Briefed: true})
	if !strings.Contains(full, "결정은 사람이 한다") || !strings.Contains(full, "규칙:") {
		t.Fatal("unbriefed resume must carry the standing brief")
	}
	for _, gone := range []string{"결정은 사람이 한다", "규칙:", "@codex-b(Astra)"} {
		if strings.Contains(short, gone) {
			t.Errorf("briefed prompt repeats %q", gone)
		}
	}
	for _, want := range []string{"받은 그대로", `"@claude-a"(Opus)`, "[User] 다음 질문"} {
		if !strings.Contains(short, want) {
			t.Errorf("briefed prompt missing %q", want)
		}
	}
	d := BuildPrompt(r, me, nil, turnContext{Reason: "discussion", Round: 1, TotalRounds: 2, Briefed: true})
	if !strings.Contains(d, "추가로 호출할 수 없습니다") || !strings.Contains(d, "토론 1/2") {
		t.Error("turn rules must survive a briefed prompt")
	}
}

func TestBriefKeyFollowsRoomSettings(t *testing.T) {
	r := &Room{Title: "설계", Mode: "read", Workspace: "ws", Note: "A",
		Participants: []*Participant{{ID: "claude-a", Provider: "claude", Label: "Opus", Enabled: true}, {ID: "codex-b", Provider: "codex", Label: "Astra", Enabled: true}}}
	me := r.Participants[0]
	before := briefKey(standingBrief(r, me, nil))
	if before != briefKey(standingBrief(r, me, nil)) {
		t.Fatal("same settings must give the same key")
	}
	r.Note = "B"
	afterNote := briefKey(standingBrief(r, me, nil))
	r.Participants[1].Enabled = false
	afterRoster := briefKey(standingBrief(r, me, nil))
	if afterNote == before || afterRoster == afterNote {
		t.Fatal("note and roster changes must change the key")
	}
}

func TestCompactionDuringTurnResendsBriefNextTurn(t *testing.T) {
	h, r, ps := controlledHub(t, "claude")
	usage := func(call *controlledCall, context float64) {
		call.emit(agent.Event{Kind: agent.KindUsage, Meta: map[string]any{"context_tokens": context}})
	}
	postQueueMessage(t, h, r.ID, "one", "claude")
	first := nextCall(t, ps[0])
	usage(first, 5000)
	finishCall(first, "a")
	waitIdle(t, h, r.ID)

	postQueueMessage(t, h, r.ID, "two", "claude")
	second := nextCall(t, ps[0])
	if !strings.Contains(second.opts.Prompt, "받은 그대로") {
		t.Fatalf("a resumed turn sends the one-line reminder:\n%s", second.opts.Prompt)
	}
	usage(second, 1200) // the CLI compacted the session during this turn
	finishCall(second, "b")
	waitIdle(t, h, r.ID)

	postQueueMessage(t, h, r.ID, "three", "claude")
	third := nextCall(t, ps[0])
	if strings.Contains(third.opts.Prompt, "받은 그대로") || !strings.Contains(third.opts.Prompt, "규칙:") {
		t.Fatalf("after compaction the standing brief must be sent again:\n%s", third.opts.Prompt)
	}
	finishCall(third, "c")
	waitIdle(t, h, r.ID)
}
