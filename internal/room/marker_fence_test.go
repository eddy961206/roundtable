package room

import (
	"strings"
	"testing"
)

// 2026-10-27: a seat quoted a forwarded answer inside a ``` block and the
// server ran the quoted [[REQUEST]] line as the seat's own marker. Markers in
// a code block are quoted text, as mentions in a code block already are.
func TestMarkersInCodeBlockDoNotRun(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	postQueueMessage(t, h, r.ID, "quote it", "codex")
	call := nextCall(t, ps[0])
	answer := strings.Join([]string{
		"받은 원문:",
		"```",
		"[[REQUEST:@claude]] 인용한 요청",
		"[[DECISION: 인용한 결정 | 근거: a.md:1 | 반대: 없음 | 미확인: 없음]]",
		"[[ATTACH:quoted.png]]",
		"```",
		"[[REQUEST:@claude]] 진짜 요청",
	}, "\n")
	finishCall(call, answer)
	snap := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return len(s.Requests) > 0 })
	if len(snap.Requests) != 1 || snap.Requests[0].Text != "진짜 요청" {
		t.Fatalf("only the marker outside the code block runs: %+v", snap.Requests)
	}
	if len(snap.Decisions) != 0 {
		t.Fatalf("a quoted decision must not become a card: %+v", snap.Decisions)
	}
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, "마커 거절") {
			t.Fatalf("a quoted marker is not rejected either: %s", m.Text)
		}
	}
	finishCall(nextCall(t, ps[1]), "done\n[[REQUEST_DONE:"+snap.Requests[0].ID+"]]")
	finishCall(nextCall(t, ps[0]), "ok") // the result goes back to the requester
	waitIdle(t, h, r.ID)

	if n := markerLines("```\n[[REQUEST:@claude]] x\n```\n[[ATTACH:a.png]]"); n != 1 {
		t.Fatalf("pending markers of a failed turn count only lines outside code: %d", n)
	}
}

func TestRequestToSelfSaysWhy(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	postQueueMessage(t, h, r.ID, "start", "codex")
	finishCall(nextCall(t, ps[0]), "answer\n[[REQUEST:@codex]] 나에게")
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	var notice string
	for _, m := range snap.Messages {
		if m.Kind == "system" && strings.Contains(m.Text, "마커 거절") {
			notice = m.Text
		}
	}
	if !strings.Contains(notice, "자기 자신") {
		t.Fatalf("a request to the seat itself must say so: %q", notice)
	}
}
