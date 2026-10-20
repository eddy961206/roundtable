package room

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecisionMarkerRecordsCard(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	events, unsub, err := h.Subscribe(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer unsub()
	postQueueMessage(t, h, r.ID, "decide", "codex")
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, "[[DECISION: 결정 한 줄 | 근거:") || !strings.Contains(call.opts.Prompt, "방 사용자가 확정해야") {
		t.Fatal("standing brief must explain the decision marker and that it only proposes")
	}
	finishCall(call, "합의했어.\n[[DECISION: 캐시는 30초 | 근거: docs/a.md:12 | 반대: 없음 | 미확인: 부하 시험]]\n끝.")
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if len(snap.Decisions) != 1 {
		t.Fatalf("%+v", snap.Decisions)
	}
	d := snap.Decisions[0]
	if d.Text != "캐시는 30초" || d.Evidence != "docs/a.md:12" || d.Dissent != "없음" || d.Unverified != "부하 시험" || d.By != "codex" || d.Status != "proposed" || d.SourceMsg == "" || d.At == 0 {
		t.Fatalf("%+v", d)
	}
	var source *Message
	for _, m := range snap.Messages {
		if m.ID == d.SourceMsg {
			source = m
		}
	}
	if source == nil || source.Author != "codex" || len(source.Decisions) != 1 || source.Decisions[0] != d.ID {
		t.Fatalf("source message %+v", source)
	}
	found := false
	for len(events) > 0 {
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(<-events, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "decision" {
			var payload Decision
			_ = json.Unmarshal(event.Data, &payload)
			found = found || payload == d
		}
	}
	if !found {
		t.Fatal("no decision SSE event")
	}
}

func TestDecisionMarkerFormatErrorsAreRejected(t *testing.T) {
	bad := []string{
		"[[DECISION: 근거 없는 결정]]",
		"[[DECISION: 결정 | 근거: x | 반대: 없음]]",
		"[[DECISION:  | 근거: x | 반대: 없음 | 미확인: 없음]]",
		"[[DECISION: 결정 | 근거: x | 반대:  | 미확인: 없음]]",
		"[[DECISION: " + strings.Repeat("가", maxDecisionText+1) + " | 근거: x | 반대: 없음 | 미확인: 없음]]",
		"[[DECISIONS: 결정 | 근거: x | 반대: 없음 | 미확인: 없음]]",
	}
	h, r, ps := controlledHub(t, "codex")
	postQueueMessage(t, h, r.ID, "decide", "")
	finishCall(nextCall(t, ps[0]), "answer\n"+strings.Join(bad, "\n"))
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if len(snap.Decisions) != 0 {
		t.Fatalf("malformed markers recorded: %+v", snap.Decisions)
	}
	rejections := 0
	for _, m := range snap.Messages {
		if m.Kind == "system" && m.NoticeFor == "codex" && strings.Contains(m.Text, "마커 거절 ([[DECISION") {
			rejections++
		}
	}
	if rejections != len(bad) {
		t.Fatalf("want %d rejections, got %d", len(bad), rejections)
	}
	// The reasons reach the author's next prompt like other marker rejections.
	postQueueMessage(t, h, r.ID, "again", "")
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "한 줄 형식 필요") || !strings.Contains(next.opts.Prompt, "모두 채워야 함") || !strings.Contains(next.opts.Prompt, fmt.Sprintf("%d자 이하", maxDecisionText)) {
		t.Fatal(next.opts.Prompt)
	}
	finishCall(next, "ok")
	waitIdle(t, h, r.ID)

	// An errored turn never records a decision.
	postQueueMessage(t, h, r.ID, "fail", "")
	failed := nextCall(t, ps[0])
	failed.result <- controlledResult{text: "[[DECISION: x | 근거: y | 반대: 없음 | 미확인: 없음]]", err: os.ErrPermission}
	waitIdle(t, h, r.ID)
	if snap, _ := h.Snapshot(r.ID); len(snap.Decisions) != 0 {
		t.Fatal(snap.Decisions)
	}
}

func TestDecisionEditsReplayAfterRestart(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "claude"})
	text, evidence := "사람 결정", "회의록"
	d, err := h.AddDecision(r.ID, DecisionInput{Text: &text, Evidence: &evidence})
	if err != nil || d.By != "user" || d.Status != "active" || d.SourceMsg != "" {
		t.Fatal(d, err)
	}
	edited := "고친 결정"
	if _, err := h.UpdateDecision(r.ID, d.ID, DecisionInput{Text: &edited}); err != nil {
		t.Fatal(err)
	}
	dropped := "dropped"
	final, err := h.UpdateDecision(r.ID, d.ID, DecisionInput{Status: &dropped})
	if err != nil || final.Text != edited || final.Evidence != evidence || final.Status != "dropped" {
		t.Fatal(final, err)
	}
	for _, in := range []DecisionInput{{}, {Status: ptr("deleted")}, {Text: ptr(" ")}, {Text: ptr("a\nb")}} {
		if _, err := h.UpdateDecision(r.ID, d.ID, in); err == nil {
			t.Fatalf("accepted %+v", in)
		}
	}
	if _, err := h.UpdateDecision(r.ID, "missing", DecisionInput{Text: &text}); err == nil {
		t.Fatal("unknown decision accepted")
	}
	if _, err := h.AddDecision(r.ID, DecisionInput{Text: &text, Status: &dropped}); err == nil {
		t.Fatal("new decisions start active")
	}
	data, err := os.ReadFile(filepath.Join(h.store.roomDir(r.ID), "decisions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 3 {
		t.Fatalf("one line per state change: %s", data)
	}
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.Decisions(r.ID)
	if err != nil || len(replay) != 1 || replay[0] != final {
		t.Fatalf("replay %+v != %+v (%v)", replay, final, err)
	}
}

func ptr(s string) *string { return &s }

// A seat's card is only proposed, whatever its dissent says: it stays out of
// the standing brief (and so does not re-send the brief) until someone sets it
// active, and it never returns to proposed.
func TestProposedDecisionJoinsBriefOnlyWhenConfirmed(t *testing.T) {
	a := &fakeProvider{id: "claude", reply: func(n int, _ string) string {
		if n == 1 {
			return "[[DECISION: 제안 카드 | 근거: 메시지 | 반대: 없음 | 미확인: 없음]]"
		}
		return "ok"
	}}
	h, r := newTestHub(t, a)
	h.PostUser(r.ID, "first", "")
	waitIdle(t, h, r.ID)
	cards, _ := h.Decisions(r.ID)
	if len(cards) != 1 || cards[0].Status != "proposed" {
		t.Fatalf("%+v", cards)
	}
	h.PostUser(r.ID, "second", "")
	waitIdle(t, h, r.ID)
	if strings.Contains(a.prompts[1], "현재 결정") || strings.Contains(a.prompts[1], "제안 카드") {
		t.Fatalf("a proposed card reached the brief:\n%s", a.prompts[1])
	}
	if _, err := h.UpdateDecision(r.ID, cards[0].ID, DecisionInput{Text: ptr("고친 제안")}); err != nil {
		t.Fatal(err)
	}
	if got, _ := h.Decisions(r.ID); got[0].Status != "proposed" {
		t.Fatalf("an edit must not confirm: %+v", got[0])
	}
	confirmed, err := h.UpdateDecision(r.ID, cards[0].ID, DecisionInput{Status: ptr("active")})
	if err != nil || confirmed.Status != "active" {
		t.Fatal(confirmed, err)
	}
	h.PostUser(r.ID, "third", "")
	waitIdle(t, h, r.ID)
	if !strings.Contains(a.prompts[2], "현재 결정(최신순):\n- 고친 제안\n") {
		t.Fatalf("a confirmed card must re-send the brief:\n%s", a.prompts[2])
	}
	if _, err := h.UpdateDecision(r.ID, cards[0].ID, DecisionInput{Status: ptr("proposed")}); err == nil {
		t.Fatal("an active card went back to proposed")
	}
	if _, err := h.AddDecision(r.ID, DecisionInput{Text: ptr("사람 제안"), Status: ptr("proposed")}); err == nil {
		t.Fatal("a person's card starts active")
	}
}

// The client drops any card state whose at is older than the one it shows
// (web/js/decisions.js isStaleDecision); that is sound only if every change of
// a card gets a strictly larger at, including several changes in one
// millisecond and a wall clock that moved back.
func TestDecisionAtStrictlyIncreasesPerChange(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "claude"})
	d, err := h.AddDecision(r.ID, DecisionInput{Text: ptr("start")})
	if err != nil {
		t.Fatal(err)
	}
	last := d.At
	for i, in := range []DecisionInput{{Text: ptr("edit")}, {Status: ptr("dropped")}, {Status: ptr("active")}, {Dissent: ptr("반대")}} {
		next, err := h.UpdateDecision(r.ID, d.ID, in)
		if err != nil {
			t.Fatal(err)
		}
		if next.At <= last {
			t.Fatalf("change %d: at %d did not pass %d", i, next.At, last)
		}
		last = next.At
	}
	h.mu.Lock()
	future := *h.rooms[r.ID].decision(d.ID)
	future.At = nowMs() + time.Hour.Milliseconds()
	err = h.saveDecision(h.rooms[r.ID], future)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	next, err := h.UpdateDecision(r.ID, d.ID, DecisionInput{Status: ptr("superseded")})
	if err != nil || next.At != future.At+1 {
		t.Fatalf("clock moved back: at %d, want %d (%v)", next.At, future.At+1, err)
	}
}

func TestStandingBriefCarriesTenNewestActiveDecisionTexts(t *testing.T) {
	a := &fakeProvider{id: "claude"}
	h, r := newTestHub(t, a)
	for i := range 12 {
		text := fmt.Sprintf("decision-%02d", i)
		if _, err := h.AddDecision(r.ID, DecisionInput{Text: &text, Evidence: ptr("evidence-text"), Dissent: ptr("dissent-text"), Unverified: ptr("unverified-text")}); err != nil {
			t.Fatal(err)
		}
	}
	snap, _ := h.Snapshot(r.ID)
	if _, err := h.UpdateDecision(r.ID, snap.Decisions[11].ID, DecisionInput{Status: ptr("superseded")}); err != nil {
		t.Fatal(err)
	}
	h.PostUser(r.ID, "first", "")
	waitIdle(t, h, r.ID)
	prompt := a.prompts[0]
	_, list, ok := strings.Cut(prompt, "현재 결정(최신순):\n")
	if !ok {
		t.Fatal(prompt)
	}
	list, _, _ = strings.Cut(list, "\n\n")
	want := []string{}
	for i := 10; i >= 1; i-- {
		want = append(want, fmt.Sprintf("- decision-%02d", i))
	}
	if list != strings.Join(want, "\n") {
		t.Fatalf("brief decisions:\n%s", list)
	}
	for _, leaked := range []string{"evidence-text", "dissent-text", "unverified-text", "decision-11", "decision-00"} {
		if strings.Contains(prompt, leaked) {
			t.Fatalf("brief must hold active texts only, found %s", leaked)
		}
	}
	// Same decisions: the resumed session gets the short reminder.
	h.PostUser(r.ID, "second", "")
	waitIdle(t, h, r.ID)
	if strings.Contains(a.prompts[1], "현재 결정") {
		t.Fatal("unchanged brief was re-sent")
	}
	// A changed decision changes the brief key, so the next turn re-sends it.
	if _, err := h.UpdateDecision(r.ID, snap.Decisions[10].ID, DecisionInput{Status: ptr("dropped")}); err != nil {
		t.Fatal(err)
	}
	h.PostUser(r.ID, "third", "")
	waitIdle(t, h, r.ID)
	third := a.prompts[2]
	if !strings.Contains(third, "현재 결정(최신순):\n- decision-09\n") || strings.Contains(third, "decision-10") {
		t.Fatal(third)
	}
	// With 11 active cards (decision-10 restored), editing the oldest card
	// makes it the most recent change: it goes first and the oldest unchanged
	// card (decision-01) leaves the ten.
	time.Sleep(2 * time.Millisecond)
	if _, err := h.UpdateDecision(r.ID, snap.Decisions[10].ID, DecisionInput{Status: ptr("active")}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := h.UpdateDecision(r.ID, snap.Decisions[0].ID, DecisionInput{Text: ptr("decision-00-edited")}); err != nil {
		t.Fatal(err)
	}
	h.PostUser(r.ID, "fourth", "")
	waitIdle(t, h, r.ID)
	fourth := a.prompts[3]
	if !strings.Contains(fourth, "현재 결정(최신순):\n- decision-00-edited\n- decision-10\n- decision-09\n") || strings.Contains(fourth, "- decision-01\n") {
		t.Fatal(fourth)
	}
}

func TestDecisionMarkerDoesNotTriggerMentions(t *testing.T) {
	a := &fakeProvider{id: "claude", reply: func(int, string) string {
		return "[[DECISION: @codex 담당 | 근거: 메시지 | 반대: 없음 | 미확인: 없음]]"
	}}
	b := &fakeProvider{id: "codex"}
	h, r := newTestHub(t, a, b)
	h.PostUser(r.ID, "@claude 정해", "")
	waitIdle(t, h, r.ID)
	time.Sleep(50 * time.Millisecond)
	if b.calls() != 0 {
		t.Fatal("a mention inside a decision marker called a participant")
	}
	if snap, _ := h.Snapshot(r.ID); len(snap.Decisions) != 1 {
		t.Fatal(snap.Decisions)
	}
}
