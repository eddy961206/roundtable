package room

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseMentions(t *testing.T) {
	ids := []string{"claude", "codex", "agy"}
	cases := []struct {
		in   string
		want []string
	}{
		{"@codex 이거 봐줘", []string{"codex"}},
		{"@codex야 @agy도 같이", []string{"codex", "agy"}},
		{"@all 의견?", []string{"claude", "codex", "agy"}},
		{"@모두 의견?", []string{"claude", "codex", "agy"}},
		{"codex가 말했듯이", nil},
		{"me@codex.com 으로 보내", nil},
		{"`@codex` 는 코드야", nil},
		{"```\n@agy\n```\n@claude 만 호출", []string{"claude"}},
		{"@codexx 는 다른 사람", nil},
		{"@Claude 대문자", []string{"claude"}},
		{"@codex @codex 중복", []string{"codex"}},
	}
	for _, c := range cases {
		got := ParseMentions(c.in, ids)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
}

func TestParseSignal(t *testing.T) {
	clean, sig := ParseSignal("동의해.\n\n[[DISCUSSION:AGREE]]")
	if clean != "동의해." || sig != "AGREE" {
		t.Fatalf("got %q %q", clean, sig)
	}
	clean, sig = ParseSignal("마커 없음")
	if clean != "마커 없음" || sig != "" {
		t.Fatalf("got %q %q", clean, sig)
	}
	_, sig = ParseSignal("[[DISCUSSION:CONCLUDE]]  ")
	if sig != "CONCLUDE" {
		t.Fatalf("got %q", sig)
	}
}

func TestBuildPrompt(t *testing.T) {
	r := &Room{Title: "설계 검토", Mode: "read", Workspace: `D:\ws`, Note: "결정은 사람이 한다",
		Participants: []*Participant{{ID: "claude", Label: "Claude Code", Enabled: true}, {ID: "codex", Label: "Codex", Enabled: true}, {ID: "agy", Label: "Antigravity", Enabled: false}}}
	me := r.Participants[0]
	hist := []*Message{{Kind: "user", Author: "user", Text: "안녕", Status: "done"}, {Kind: "agent", Author: "codex", Text: "반가워", Status: "done"}, {Kind: "agent", Author: "agy", Status: "error", Error: "exit 1"}}
	p := BuildPrompt(r, me, hist, turnContext{Reason: "broadcast", Parallel: true, FirstTurn: true})
	for _, want := range []string{`"@claude"`, "@codex(Codex)", "결정은 사람이 한다", "[User] 안녕", "[@codex] 반가워", "(응답 실패: exit 1)", "동시에 받았습니다", "지금까지의 대화", `D:\ws`, "@all"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(p, "@agy(") {
		t.Error("disabled participant must not be in the roster")
	}
	d := BuildPrompt(r, me, nil, turnContext{Reason: "discussion", Round: 2, TotalRounds: 3})
	for _, want := range []string{"토론 2/3", "[[DISCUSSION:CONTINUE]]", "(없음)", "새로 올라온 메시지", "추가로 호출할 수 없습니다"} {
		if !strings.Contains(d, want) {
			t.Errorf("discussion prompt missing %q", want)
		}
	}
}
