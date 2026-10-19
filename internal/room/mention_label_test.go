package room

import (
	"reflect"
	"strings"
	"testing"
)

func TestMentionsMatchDisplayLabels(t *testing.T) {
	ps := []*Participant{
		{ID: "codex-a1", Provider: "codex", Label: "Astra", Enabled: true},
		{ID: "agy-b2", Provider: "agy", Label: "Flash", Enabled: true},
		{ID: "claude-c3", Provider: "claude", Label: "Opus", Enabled: true},
		{ID: "claude-d4", Provider: "claude", Label: "Opus", Enabled: true},
	}
	cases := map[string][]string{
		"@Astra Flash에게 물어봐":   {"codex-a1"},
		"@flash야 확인해":          {"agy-b2"},
		"@Opus 둘 다":            {"claude-c3", "claude-d4"},
		"@Astral 은 다른 이름":      nil,
		"`@Astra` 코드 안은 호출 아님": nil,
	}
	for text, want := range cases {
		if got := ParseParticipantMentions(text, ps); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v want %v", text, got, want)
		}
	}
}

func TestRequestTargetByIDOrUniqueLabel(t *testing.T) {
	rs := &roomState{room: &Room{Participants: []*Participant{
		{ID: "codex-a1", Provider: "codex", Label: "Astra", Enabled: true},
		{ID: "claude-c3", Provider: "claude", Label: "Opus", Enabled: true},
		{ID: "claude-d4", Provider: "claude", Label: "Opus", Enabled: true},
		{ID: "agy-off", Provider: "agy", Label: "Flash", Enabled: false},
	}}}
	if p, _ := rs.requestTarget("codex-a1"); p == nil || p.ID != "codex-a1" {
		t.Fatal("seat ID must resolve")
	}
	if p, _ := rs.requestTarget("astra"); p == nil || p.ID != "codex-a1" {
		t.Fatal("unique label must resolve case-insensitively")
	}
	if p, why := rs.requestTarget("Opus"); p != nil || !strings.Contains(why, "@claude-c3") || !strings.Contains(why, "@claude-d4") {
		t.Fatalf("ambiguous label must be rejected with the seat IDs: %v %q", p, why)
	}
	if p, why := rs.requestTarget("Flash"); p != nil || !strings.Contains(why, "없음") {
		t.Fatalf("disabled seat must not resolve: %v %q", p, why)
	}
}
