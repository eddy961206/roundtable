package room

import (
	"io"
	"log"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func askEvent(id string, q ...agent.AskQuestion) agent.Event {
	return agent.Event{TS: nowMs(), Kind: agent.KindAsk, ID: id, Meta: map[string]any{"questions": q}}
}

func roomAsks(t *testing.T, h *Hub, id string) (map[string]AskView, int) {
	t.Helper()
	snap, err := h.Snapshot(id)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]AskView{}
	for _, a := range snap.Asks {
		out[a.ID] = a
	}
	for _, s := range h.Rooms() {
		if s.ID == id {
			return out, s.Asks
		}
	}
	t.Fatal("room summary missing")
	return nil, 0
}

func TestAskAnswerAndClose(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "start", ""); err != nil {
		t.Fatal(err)
	}
	first := nextCall(t, ps[0])
	first.emit(askEvent("call_1", agent.AskQuestion{Title: "누락 파일만 복원할까?", Options: []string{"누락 파일만 복원", "메인은 그대로 두기"}}))
	asks, open := roomAsks(t, h, r.ID)
	if a := asks["call_1"]; a.Status != "waiting" || a.Seat != "codex" || a.Seq == 0 || open != 1 {
		t.Fatal(asks, open)
	}
	if _, _, err := h.AnswerAsk(r.ID, "call_1", 3, ""); err == nil {
		t.Fatal("choice out of range accepted")
	}
	if _, _, err := h.AnswerAsk(r.ID, "call_1", 1, "직접"); err == nil {
		t.Fatal("choice and text together accepted")
	}
	m, view, err := h.AnswerAsk(r.ID, "call_1", 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if m.Target != "codex" || !strings.Contains(m.Text, "사용자 답: 누락 파일만 복원") || view.Status != "answered" || view.AnswerSeq != m.Seq {
		t.Fatal(m, view)
	}
	if _, _, err := h.AnswerAsk(r.ID, "call_1", 2, ""); err == nil || !strings.Contains(err.Error(), "이미 답한") {
		t.Fatal(err)
	}
	first.result <- controlledResult{text: "질문을 남기고 계속 진행함"}
	// Codex cannot steer, so the answer arrives as the seat's next turn.
	second := nextCall(t, ps[0])
	if !strings.Contains(second.opts.Prompt, "사용자 답: 누락 파일만 복원") {
		t.Fatal(second.opts.Prompt)
	}
	second.emit(askEvent("call_2", agent.AskQuestion{Title: "다음 단계는?"}))
	second.result <- controlledResult{text: "두 번째 질문"}
	waitIdle(t, h, r.ID)
	asks, open = roomAsks(t, h, r.ID)
	if asks["call_1"].Status != "answered" || asks["call_2"].Status != "open" || open != 1 {
		t.Fatal(asks, open)
	}
	if _, _, err := h.AnswerAsk(r.ID, "call_2", 1, ""); err == nil {
		t.Fatal("choice accepted for a question without options")
	}
	// Another message reaches the seat first: its next reply closes the question.
	if _, err := h.PostUser(r.ID, "다른 지시", "codex"); err != nil {
		t.Fatal(err)
	}
	third := nextCall(t, ps[0])
	third.result <- controlledResult{text: "다른 일"}
	waitIdle(t, h, r.ID)
	asks, open = roomAsks(t, h, r.ID)
	if a := asks["call_2"]; a.Status != "closed" || a.ClosedSeq <= a.Seq || open != 0 {
		t.Fatal(asks, open)
	}
	if _, _, err := h.AnswerAsk(r.ID, "call_2", 0, "늦은 답"); err == nil || !strings.Contains(err.Error(), "지나감") {
		t.Fatal(err)
	}
	reloaded, err := NewHub(h.store, h.reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	asks, open = roomAsks(t, reloaded, r.ID)
	if asks["call_1"].Answer != "누락 파일만 복원" || asks["call_2"].Status != "closed" || open != 0 {
		t.Fatal(asks, open)
	}
}

func TestAskOpenAfterRestartIsCounted(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "start", ""); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	call.emit(askEvent("call_1", agent.AskQuestion{Title: "어느 쪽?", Options: []string{"A", "B"}}))
	call.result <- controlledResult{text: "done"}
	waitIdle(t, h, r.ID)
	reloaded, err := NewHub(h.store, h.reg, t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	asks, open := roomAsks(t, reloaded, r.ID)
	if asks["call_1"].Status != "open" || open != 1 {
		t.Fatal(asks, open)
	}
}
