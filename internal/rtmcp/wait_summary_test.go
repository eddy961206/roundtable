package rtmcp

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

const longReview = "리뷰 결과야.\n\n" +
	"| 파일:줄 | 문제 |\n|---|---|\n| a.go:10 | 경합 |\n" +
	"긴 설명 문단이 여러 줄 이어진다. " + "근거 " + "…" + "\n" +
	"- **판정(capture): 수정 필요**\n" +
	"추가 설명.\n\n" +
	"[[REQUEST:@writer]] 위 표대로 고쳐 줘\n" +
	"[[DECISION: x | 근거: y | 반대: 없음 | 미확인: 없음]]"

// rt_wait summary keeps the status, verdict lines and the closing signal block,
// so an orchestrator need not read whole reviews to decide the next step.
func TestWaitSummaryKeepsVerdictAndSignals(t *testing.T) {
	text := strings.Replace(longReview, "…", strings.Repeat("긴 본문 ", 400), 1)
	hub, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: text, SessionID: "s"}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "검수", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	sent := callJSON(t, s, "rt_post", map[string]any{"room": r.ID, "text": "검수해 줘"})
	seq := int64(sent["seq"].(float64))
	rooms := r.ID + ":" + strconv.FormatInt(seq, 10)

	// The seat answer, not the room's rejection notice for the unknown @writer.
	answer := func(res map[string]any) map[string]any {
		for _, m := range res["ready"].([]any)[0].(map[string]any)["messages"].([]any) {
			if mm := m.(map[string]any); mm["author"] == "claude" {
				return mm
			}
		}
		t.Fatalf("no seat answer: %v", res)
		return nil
	}
	full := callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 30, "max_chars": 100000})
	if got := answer(full)["text"].(string); got != text {
		t.Fatalf("without summary the answer is whole: %d chars", len(got))
	}

	sum := callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 30, "summary": true})
	last := answer(sum)
	got := last["text"].(string)
	want := "- **판정(capture): 수정 필요**\n[[REQUEST:@writer]] 위 표대로 고쳐 줘\n[[DECISION: x | 근거: y | 반대: 없음 | 미확인: 없음]]"
	if got != want || last["summarized"] != true || last["chars"].(float64) != float64(len([]rune(text))) || last["status"] != "done" {
		t.Fatalf("summary:\n%q\n%v", got, last)
	}
}

func TestSummarizeTextEdges(t *testing.T) {
	for in, want := range map[string]string{
		"설명만 있고 판정 없음":                  "설명만 있고 판정 없음", // no verdict: the closing prose lines stand in
		"Verdict: approve\n본문":          "Verdict: approve",
		"본문\n결론: 승인\n\nWORK:DONE":       "결론: 승인\nWORK:DONE",
		"```\n판정: 코드 안\n```\n판정(x): 승인": "판정(x): 승인",
		"[[REQUEST:@a]] 중간 마커\n본문 끝":    "본문 끝\n[[REQUEST:@a]] 중간 마커", // the server acts on marker lines anywhere
		"본문\n[[REQUEST_DONE:r1]]\n\n":   "본문\n[[REQUEST_DONE:r1]]",
	} {
		if got := summarizeText(in); got != want {
			t.Errorf("%q\n got %q\nwant %q", in, got, want)
		}
	}
}
