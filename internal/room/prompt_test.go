package room

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestStandingBriefRolesAndPriority(t *testing.T) {
	r := &Room{
		Title:     "설계방",
		Mode:      agent.ModeWrite,
		Workspace: "ws",
		Participants: []*Participant{
			{ID: "claude-a", Provider: "claude", Model: "claude-opus", Label: "Fable", Enabled: true, Role: "큰 그림과 UX를 먼저 깊게 본다."},
			{ID: "codex-b", Provider: "codex", Model: "gpt-sol", Label: "Sol", Enabled: true, Role: "논리와 경계 조건을 먼저 깊게 본다."},
			{ID: "agy-c", Provider: "agy", Model: "gemini-argon", Label: "Argon", Enabled: true, Role: ""},
			{ID: "off-d", Provider: "codex", Model: "gpt-sol", Label: "Off", Enabled: false, Role: "비활성 좌석 역할"},
		},
	}
	me := r.Participants[0]
	brief := standingBrief(r, me)
	for _, want := range []string{
		"당신의 역할:\n큰 그림과 UX를 먼저 깊게 본다.",
		"강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.",
		"다른 참가자들의 역할:\n- @codex-b(Sol): 논리와 경계 조건을 먼저 깊게 본다.",
		"[[ATTACH:캡처.png]]",
		"[[REQUEST:@좌석ID]]",
		"시각 검토",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("standingBrief missing %q:\n%s", want, brief)
		}
	}
	for _, gone := range []string{"@agy-c(Argon):", "비활성 좌석 역할"} {
		if strings.Contains(brief, gone) {
			t.Errorf("standingBrief must omit empty or disabled role %q:\n%s", gone, brief)
		}
	}

	// Empty role on self and others omits both role sections.
	keyWithRoles := briefKey(brief)
	r.Participants[0].Role = ""
	r.Participants[1].Role = "   "
	emptyBrief := standingBrief(r, me)
	if strings.Contains(emptyBrief, "당신의 역할") || strings.Contains(emptyBrief, "다른 참가자들의 역할") {
		t.Fatalf("empty roles must be omitted:\n%s", emptyBrief)
	}
	if briefKey(emptyBrief) == keyWithRoles {
		t.Fatal("role change must change briefKey")
	}
}

func TestBriefedReminderIncludesRoleAndSignalBlockRule(t *testing.T) {
	r := &Room{
		Title: "설계방",
		Mode:  agent.ModeRead,
		Participants: []*Participant{
			{ID: "agy-a", Provider: "agy", Label: "Argon", Enabled: true, Role: "문구·소통과 시각 검토를 먼저 깊게 본다.\n다른 문제도 말한다."},
		},
	}
	me := r.Participants[0]
	hist := []*Message{{Kind: "user", Author: "user", Text: "확인해줘", Status: "done"}}

	withRole := BuildPrompt(r, me, hist, turnContext{Reason: "mention", Briefed: true})
	for _, want := range []string{
		"당신의 역할: 문구·소통과 시각 검토를 먼저 깊게 본다. 다른 문제도 말한다.",
		"강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.",
		"응답 끝 신호 블록",
		"[[REQUEST_DONE:요청ID]]",
		"[[REQUEST_REJECT:요청ID]]",
		"[[DISCUSSION:...]]",
		"WORK:DONE",
	} {
		if !strings.Contains(withRole, want) {
			t.Errorf("briefed prompt with role missing %q:\n%s", want, withRole)
		}
	}

	me.Role = ""
	withoutRole := BuildPrompt(r, me, hist, turnContext{Reason: "mention", Briefed: true})
	if strings.Contains(withoutRole, "당신의 역할:") {
		t.Errorf("briefed prompt without role must omit role line:\n%s", withoutRole)
	}
	if !strings.Contains(withoutRole, "응답 끝 신호 블록") {
		t.Errorf("briefed prompt must always include signal block rule:\n%s", withoutRole)
	}
}

func TestBuildPartPromptRoleAndVisualReview(t *testing.T) {
	plan := &WorkPlan{Goal: "화면 개선", BaseSHA: "abc1234"}
	part := &Part{ID: "p1", Title: "UI 카드", Brief: "역할 카드 추가", Scope: []string{"web/js/"}, Worktree: "/tmp/wt", Branch: "rt/room/p1"}

	noRolePrompt := BuildPartPrompt(plan, part, "")
	if strings.Contains(noRolePrompt, "당신의 역할:") {
		t.Errorf("part prompt without role must omit role line:\n%s", noRolePrompt)
	}
	for _, want := range []string{
		"[[ATTACH:캡처.png]]",
		"[[REQUEST:@좌석ID]]",
		"시각 검토",
	} {
		if !strings.Contains(noRolePrompt, want) {
			t.Errorf("part prompt missing %q:\n%s", want, noRolePrompt)
		}
	}

	customPrompt := BuildPartPrompt(plan, part, "큰 그림·UX를 먼저 깊게 본다.")
	for _, want := range []string{
		"당신의 역할: 큰 그림·UX를 먼저 깊게 본다.",
		"강점은 먼저 깊게 볼 우선순위일 뿐 다른 관점의 지적을 막지 않습니다.",
	} {
		if !strings.Contains(customPrompt, want) {
			t.Errorf("custom role missing %q from part prompt:\n%s", want, customPrompt)
		}
	}

	h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
	rs := h.rooms[r.ID]
	author := r.Participants[0]
	reviewer := r.Participants[1]
	author.Role = "문구·소통과 시각 검토를 먼저 깊게 본다."
	reviewer.Role = "논리와 경계 조건을 먼저 깊게 본다."
	partInRoom := &Part{ID: "p1", Title: "UI 카드", Brief: "역할 카드 추가", Scope: []string{"web/js/"}, Seat: author.ID, Reviewer: reviewer.ID, Worktree: r.Workspace, Branch: "rt/room/p1", State: "working"}
	rs.room.Work = &WorkPlan{Goal: "화면 개선", BaseSHA: "abc1234", ReviewRounds: 3, Parts: []*Part{partInRoom}}
	turn := &Turn{ID: "t1", Agent: author.ID, Reason: "part", Part: "p1", Status: "queued"}
	pr := h.buildPartTurn(rs, turn, author, &fakeProvider{id: "codex"}, partInRoom)
	if pr == nil || !strings.Contains(pr.opts.Prompt, "당신의 역할: 문구·소통과 시각 검토를 먼저 깊게 본다.") {
		t.Fatalf("buildPartTurn did not pass Participant.Role to BuildPartPrompt: %+v", pr)
	}
	diff := &PartDiff{PartID: "p1", BaseSHA: "abc1234", HeadSHA: "def5678", Added: 1, Files: []DiffFile{{Path: "web/js/a.js", Status: "M", Added: 1, Patch: "+ok"}}}
	h.startReview(rs, partInRoom, "working", "def5678", diff)
	if partInRoom.Review == nil || partInRoom.Review.Pending == nil || !strings.Contains(partInRoom.Review.Pending.Prompt, "당신의 역할: 논리와 경계 조건을 먼저 깊게 본다.") {
		t.Fatalf("startReview did not inject reviewer role into review prompt: %+v", partInRoom.Review)
	}
	reviewer.SessionID = "chat-session"
	partInRoom.SessionID = "part-session"
	reviewTurn := h.buildTurn(rs, rs.queue[0])
	if reviewTurn == nil || reviewTurn.opts.SessionID != "" || reviewTurn.opts.Mode != agent.ModeRead || !strings.Contains(reviewTurn.opts.Prompt, "당신의 역할: "+reviewer.Role) {
		t.Fatalf("fresh reviewer turn lost its role or session isolation: %+v", reviewTurn)
	}
	next, _, err := partInRoom.Review.Apply(findingHeader + findingOne)
	if err != nil || next == nil || next.Who != "author" {
		t.Fatalf("author step: %+v, %v", next, err)
	}
	h.runReviewStep(rs, partInRoom, *next)
	authorTurn := h.buildTurn(rs, rs.queue[len(rs.queue)-1])
	if authorTurn == nil || authorTurn.opts.SessionID != "part-session" || !strings.Contains(authorTurn.opts.Prompt, "당신의 역할: "+author.Role) {
		t.Fatalf("author review turn lost its role or part session: %+v", authorTurn)
	}
}

func TestAttachmentNoticeIncludesVisualReviewGuidance(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	a, err := h.UploadAttachment(r.ID, "screen.png", bytes.NewReader(pngImage(t)))
	if err != nil {
		t.Fatal(err)
	}
	rs := h.rooms[r.ID]
	p := r.Participants[0]
	hist := []*Message{{ID: "m2", Kind: "user", Author: "user", Text: "with image", Attachments: []Attachment{a}}}

	var noImgOpts agent.RunOptions
	h.addTurnImages(rs, p, []*Message{{ID: "m1", Kind: "user", Author: "user", Text: "no image"}}, &noImgOpts, "mention")
	if strings.Contains(noImgOpts.Prompt, "시각 검토") {
		t.Fatalf("unexpected image guidance when no attachments exist: %s", noImgOpts.Prompt)
	}

	writeMention := agent.RunOptions{Mode: agent.ModeWrite}
	h.addTurnImages(rs, p, hist, &writeMention, "mention")
	for _, want := range []string{"[첨부 " + a.ID, "[[ATTACH:캡처.png]]", "[[REQUEST:@좌석ID]]", "시각 검토"} {
		if !strings.Contains(writeMention.Prompt, want) {
			t.Errorf("write mention prompt missing %q:\n%s", want, writeMention.Prompt)
		}
	}

	readDiscussion := agent.RunOptions{Mode: agent.ModeRead}
	h.addTurnImages(rs, p, hist, &readDiscussion, "discussion")
	if !strings.Contains(readDiscussion.Prompt, "[[ATTACH:캡처.png]]") || strings.Contains(readDiscussion.Prompt, "[[REQUEST") {
		t.Errorf("read discussion prompt must allow ATTACH and forbid REQUEST:\n%s", readDiscussion.Prompt)
	}

	chatMention := agent.RunOptions{Mode: agent.ModeChat}
	h.addTurnImages(rs, p, hist, &chatMention, "mention")
	if !strings.Contains(chatMention.Prompt, "[[REQUEST:@좌석ID]]") || strings.Contains(chatMention.Prompt, "[[ATTACH") {
		t.Errorf("chat mention prompt must allow REQUEST and forbid ATTACH:\n%s", chatMention.Prompt)
	}

	chatDiscussion := agent.RunOptions{Mode: agent.ModeChat}
	h.addTurnImages(rs, p, hist, &chatDiscussion, "discussion")
	if strings.Contains(chatDiscussion.Prompt, "[[ATTACH") || strings.Contains(chatDiscussion.Prompt, "[[REQUEST") {
		t.Errorf("chat discussion prompt must not suggest ATTACH or REQUEST:\n%s", chatDiscussion.Prompt)
	}
}

func TestRolePresetsFileValid(t *testing.T) {
	raw, err := os.ReadFile("role_presets.json")
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Description string `json:"description"`
		Presets     []struct {
			ID        string   `json:"id"`
			Name      string   `json:"name"`
			Directive string   `json:"directive"`
			Tags      []string `json:"tags"`
		} `json:"presets"`
		ModelSuggestions []struct {
			Match  string `json:"match"`
			Preset string `json:"preset"`
		} `json:"model_suggestions"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("role_presets.json invalid JSON: %v", err)
	}
	if !strings.Contains(data.Description, "수정") {
		t.Errorf("description must explain that the file is user-editable: %q", data.Description)
	}
	if len(data.Presets) < 3 || len(data.Presets) > 5 {
		t.Fatalf("expected 3~5 presets, got %d", len(data.Presets))
	}
	byID := map[string]bool{}
	for _, preset := range data.Presets {
		if preset.ID == "" || preset.Name == "" || preset.Directive == "" || len(preset.Tags) == 0 {
			t.Errorf("incomplete preset: %+v", preset)
		}
		if byID[preset.ID] {
			t.Errorf("duplicate preset id: %s", preset.ID)
		}
		byID[preset.ID] = true
	}
	wantMatches := map[string]bool{"claude-*": false, "gpt-*": false, "gemini-*": false}
	for _, s := range data.ModelSuggestions {
		if !byID[s.Preset] {
			t.Errorf("suggestion %q references unknown preset %q", s.Match, s.Preset)
		}
		if _, ok := wantMatches[s.Match]; ok {
			wantMatches[s.Match] = true
		}
	}
	for m, seen := range wantMatches {
		if !seen {
			t.Errorf("missing model_suggestion for %q", m)
		}
	}
}
