package room

import (
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// 2026-10-28: in the heaviest Codex rooms 18~45% of input was empty
// write_stdin polls (yield 1 s) while a test ran, each a full model call at
// 200~300k context. A/B on one test task: 30 calls → 11, input -69%, same
// results (docs/토큰효율_측정_20261028.md). The rule goes only to seats whose
// tool has the wait parameter, and only where they run commands.
func TestStandingBriefWaitRulePerProvider(t *testing.T) {
	for _, tc := range []struct {
		provider, mode string
		want, gone     []string
	}{
		{"codex", agent.ModeWrite, []string{"yield_time_ms", "300000"}, []string{"WaitMsBeforeAsync"}},
		{"codex", agent.ModeRead, []string{"yield_time_ms"}, nil},
		{"codex", agent.ModeChat, nil, []string{"yield_time_ms"}},
		{"claude", agent.ModeWrite, nil, []string{"yield_time_ms", "WaitMsBeforeAsync"}},
	} {
		r := &Room{Title: "방", Mode: tc.mode, Workspace: "ws", Participants: []*Participant{{ID: "s", Provider: tc.provider, Label: "S", Enabled: true}}}
		brief := standingBrief(r, r.Participants[0], nil, "")
		for _, w := range tc.want {
			if !strings.Contains(brief, w) {
				t.Errorf("%s/%s brief misses %q", tc.provider, tc.mode, w)
			}
		}
		for _, g := range tc.gone {
			if strings.Contains(brief, g) {
				t.Errorf("%s/%s brief must not carry %q", tc.provider, tc.mode, g)
			}
		}
	}
}
