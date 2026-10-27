package room

import (
	"strconv"
	"strings"
	"testing"
)

// 2026-10-27: the orchestrator read each review in one room and re-typed it into
// another, paying for the long text twice. Forward copies the original text
// server-side, names its source in the record and in the receiving prompt, and
// does not let quoted @mentions or markers start turns in the receiving room.
func TestForwardCopiesOriginalWithSource(t *testing.T) {
	h, src, ps := controlledHub(t, "codex", "claude")
	dst, err := h.CreateRoom(RoomSpec{Title: "작성 방", Mode: "chat", Participants: []string{"codex", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	review := "판정: 수정 필요\n| a.go:1 | 경합 |\n@claude 도 봐 줘\n[[REQUEST:@claude]] 재확인"
	if _, err := h.PostUser(src.ID, "검수해 줘", "codex"); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), review)
	nextCall(t, ps[1]).result <- controlledResult{text: "ok"} // the source room's own request/mention
	waitIdle(t, h, src.ID)
	snap, _ := h.Snapshot(src.ID)
	var seq int64
	for _, m := range snap.Messages {
		if m.Kind == "agent" && m.Author == "codex" {
			seq = m.Seq
		}
	}

	m, err := h.Forward(dst.ID, src.ID, seq, "codex", "이 표대로 고쳐 줘")
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != review || m.Forward == nil || m.Forward.Room != src.ID || m.Forward.Seq != seq || m.Forward.Author != "codex" || m.Target != "codex" {
		t.Fatalf("%+v %+v", m, m.Forward)
	}
	call := nextCall(t, ps[0])
	if !strings.Contains(call.opts.Prompt, review) {
		t.Fatalf("the original text must arrive whole: %s", call.opts.Prompt)
	}
	for _, s := range []string{"전달", src.ID + "#" + strconv.FormatInt(seq, 10), "@codex", "이 표대로 고쳐 줘"} {
		if !strings.Contains(call.opts.Prompt, s) {
			t.Fatalf("source %q missing: %s", s, call.opts.Prompt)
		}
	}
	finishCall(call, "고침")
	waitIdle(t, h, dst.ID)
	noMoreCalls(t, ps[1]) // quoted @claude and [[REQUEST:@claude]] start nothing here
	got, _ := h.Snapshot(dst.ID)
	if len(got.Requests) != 0 {
		t.Fatalf("a forwarded marker is quoted text: %+v", got.Requests)
	}
}

func TestForwardRefusals(t *testing.T) {
	h, src, ps := controlledHub(t, "codex")
	dst, _ := h.CreateRoom(RoomSpec{Title: "다른 방", Mode: "chat", Participants: []string{"codex"}})
	if _, err := h.PostUser(src.ID, "hi", "codex"); err != nil {
		t.Fatal(err)
	}
	running := nextCall(t, ps[0])
	snap, _ := h.Snapshot(src.ID)
	streaming := snap.Messages[len(snap.Messages)-1].Seq
	for name, call := range map[string]func() error{
		"missing seq":     func() error { _, err := h.Forward(dst.ID, src.ID, 999, "", ""); return err },
		"streaming reply": func() error { _, err := h.Forward(dst.ID, src.ID, streaming, "", ""); return err },
		"missing room":    func() error { _, err := h.Forward(dst.ID, "nope", 1, "", ""); return err },
		"same room":       func() error { _, err := h.Forward(src.ID, src.ID, 1, "", ""); return err },
		"inactive target": func() error { _, err := h.Forward(dst.ID, src.ID, 1, "ghost", ""); return err },
		"multi-line note": func() error { _, err := h.Forward(dst.ID, src.ID, 1, "", "a\nb"); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	finishCall(running, "ok")
	waitIdle(t, h, src.ID)
}
