package room

import (
	"strings"
	"testing"
)

// Marker and mention parsing kept breaking at the same edges: quoted markers
// run as real ones, a signal block cut by a marker line, a mention inside
// code (ac5988b, f14f132, 0e4871b, c3587ae, 0e87e43, ca68122). These fuzzers
// state the boundary rules for any text instead of one example per bug.
//
// Run longer locally: go test ./internal/room -run '^$' -fuzz FuzzQuotedMarkersStayText -fuzztime 30s

var fuzzSeeds = []string{
	"[[REQUEST:@codex]] 봐줘",
	"[[DECISION: a | 근거: b | 반대: 없음 | 미확인: 없음]]",
	"@codex 확인해 줘",
	"[[ATTACH:a.png]]\n[[REQUEST_DONE:abc]]",
	"WORK:DONE\n[[DISCUSSION:CONTINUE]]",
	"`@codex` 와 ``@all``",
	"  [[REQUEST:@all]] 들여쓴 줄\n\n",
}

// A reply that quotes any text in a ``` block runs none of its markers and
// calls nobody.
func FuzzQuotedMarkersStayText(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	seats := []*Participant{{ID: "codex", Provider: "codex", Label: "Codex", Enabled: true}, {ID: "claude", Provider: "claude", Label: "검수", Enabled: true}}
	f.Fuzz(func(t *testing.T, body string) {
		if strings.Contains(body, "```") {
			t.Skip("a fence inside the body would end the quote")
		}
		quoted := "인용합니다.\n```\n" + body + "\n```\n이상입니다."
		if n := markerLines(quoted); n != 0 {
			t.Fatalf("%d marker lines counted inside a code block: %q", n, body)
		}
		if got := ParseParticipantMentions(quoted, seats); len(got) != 0 {
			t.Fatalf("mentions %v parsed inside a code block: %q", got, body)
		}
		for i, live := range markerCandidates(quoted) {
			if live {
				t.Fatalf("line %d of a quoted body is a marker candidate: %q", i, body)
			}
		}
	})
}

// A discussion signal on the reply's last line is always read, and taking it
// out never changes the text above the signal block.
func FuzzTrailingSignalIsRead(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		clean, signal := ParseSignal(body + "\n[[DISCUSSION:AGREE]]")
		if signal != "AGREE" {
			t.Fatalf("the last-line signal was not read (got %q): %q", signal, body)
		}
		if strings.Contains(clean, "[[DISCUSSION:AGREE]]") && !strings.Contains(body, "[[DISCUSSION:AGREE]]") {
			t.Fatalf("the signal stayed in the text: %q", clean)
		}
		lines, start := trailingSignals(body + "\n[[DISCUSSION:AGREE]]")
		if start >= len(lines) {
			t.Fatalf("the signal line is outside the block: %q", body)
		}
	})
}
