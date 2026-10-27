package agent

import (
	"encoding/json"
	"testing"
)

func TestOpenCodePolicyShellOnlyInWrite(t *testing.T) {
	for mode, want := range map[string]bool{ModeChat: false, ModeRead: false, ModeWrite: true} {
		raw, err := openCodePolicy(RunOptions{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Agents map[string]struct {
				Permissions []openCodePermission `json:"permissions"`
			} `json:"agents"`
		}
		if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Fatal(err)
		}
		got := false
		for _, r := range cfg.Agents[openCodeAgent(mode)].Permissions {
			if r.Action == "shell" && r.Effect == "allow" {
				got = true
			}
		}
		if got != want {
			t.Errorf("%s: shell allowed = %v, want %v", mode, got, want)
		}
	}
}
