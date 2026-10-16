package room

import (
	"strings"
	"testing"
)

func TestOversizedNewestMessageIsClippedNotDropped(t *testing.T) {
	r := &Room{Title: "t", Mode: "chat", Participants: []*Participant{{ID: "claude", Label: "C", Enabled: true}}}
	big := strings.Repeat("가", maxHistoryChars)
	hist := []*Message{{Kind: "user", Author: "user", Text: "짧은 옛 메시지", Status: "done"}, {Kind: "user", Author: "user", Text: big, Status: "done"}}
	p := BuildPrompt(r, r.Participants[0], hist, turnContext{Reason: "broadcast", FirstTurn: true})
	if !strings.Contains(p, "[User] 가가가") || !strings.Contains(p, "뒷부분 생략") {
		t.Fatal("newest message must be included (clipped)")
	}
	if len(p) > maxHistoryChars+4096 {
		t.Fatalf("prompt too large: %d", len(p))
	}
}
