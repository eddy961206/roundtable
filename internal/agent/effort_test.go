package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Real `agy models` output captured on 2026-10-17 (agy 1.3.5).
var agyModelsSample = strings.Split(`Fetching available models...
gemini-4-argon-medium	Gemini 4 Argon (Medium)
gemini-4-argon-high	Gemini 4 Argon (High)
gemini-4-argon-low	Gemini 4 Argon (Low)
gemini-3.8-flash-high	Gemini 3.8 Flash (High)
gemini-3.8-flash-medium	Gemini 3.8 Flash (Medium)
gemini-3.8-flash-low	Gemini 3.8 Flash (Low)
claude-opus-5-5-low	Claude Opus 5.5 (Low)
claude-opus-5-5-high	Claude Opus 5.5 (High)
gpt-oss-120b-medium	GPT-OSS 120B (Medium)`, "\n")

func TestGroupAgyModels(t *testing.T) {
	models, efforts := groupAgyModels(agyModelsSample)
	var ids, labels []string
	for _, m := range models {
		ids = append(ids, m.ID)
		labels = append(labels, m.Label)
	}
	if want := []string{"gemini-4-argon", "gemini-3.8-flash", "claude-opus-5-5", "gpt-oss-120b"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %v", ids)
	}
	if labels[0] != "Gemini 4 Argon" {
		t.Fatalf("effort suffix must be stripped from the label: %q", labels[0])
	}
	if got := efforts["gemini-4-argon"]; !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
		t.Fatalf("argon efforts %v", got)
	}
	if got := efforts["claude-opus-5-5"]; !reflect.DeepEqual(got, []string{"low", "high"}) {
		t.Fatalf("opus efforts %v", got)
	}
	if got := efforts["gpt-oss-120b"]; !reflect.DeepEqual(got, []string{"medium"}) {
		t.Fatalf("gpt-oss efforts %v", got)
	}
}

func TestAgyArgsReadOnlyKeepsPlanMode(t *testing.T) {
	for _, mode := range []string{ModeChat, ModeRead} {
		a := strings.Join(agyArgs(RunOptions{Mode: mode, Workspace: `D:\ws`, Model: "gemini-4-argon", Effort: "high", Prompt: "x"}), " ")
		if strings.Contains(a, "--disable-slash-commands") {
			t.Errorf("%s: --disable-slash-commands would silently disable --mode plan: %s", mode, a)
		}
		if !strings.Contains(a, "--mode plan") || !strings.Contains(a, "--model gemini-4-argon --effort high") {
			t.Errorf("%s args: %s", mode, a)
		}
	}
	w := strings.Join(agyArgs(RunOptions{Mode: ModeWrite, Workspace: `D:\ws`, Prompt: "x"}), " ")
	if !strings.Contains(w, "--disable-slash-commands") || !strings.Contains(w, "--mode accept-edits") || strings.Contains(w, "--effort") {
		t.Errorf("write args: %s", w)
	}
}

func TestEffortsPerProvider(t *testing.T) {
	want := []string{"low", "medium", "high", "xhigh", "max"}
	if !reflect.DeepEqual(NewClaude("claude").Efforts(), want) || !reflect.DeepEqual(NewCodex("codex").Efforts(), want) {
		t.Fatal("claude/codex efforts")
	}
	if m, e := NewAgy("agy", "").DefaultModel(); m != "gemini-3.8-flash" || e != "high" {
		t.Fatalf("agy default %s/%s", m, e)
	}
}

func TestAgyStatusRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agy-status.json")
	// shape documented by agy's own statusline sample (statusline.ps1 -Test)
	payload := []byte(`{"email":"someone@example.com","cwd":"C:/secret","plan_tier":"ultra","model":{"id":"gemini-3.8-flash"},"context_window":{"total_tokens":184000,"context_window_size":1048576},"quota":{"gemini-5h":{"remaining_fraction":0.72,"reset_in_seconds":9660},"gemini-weekly":{"remaining_fraction":0.51,"reset_in_seconds":374400}}}`)
	now := time.UnixMilli(1792231000000)
	if err := WriteAgyStatus(path, payload, now); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); strings.Contains(string(raw), "example.com") || strings.Contains(string(raw), "secret") {
		t.Fatalf("e-mail and paths must not be stored: %s", raw)
	}
	a := NewAgy("agy", path)
	l, ok := a.CurrentLimits()
	if !ok || l.Provider != "agy" || l.At != now.UnixMilli() || len(l.Windows) != 2 || l.Plan != "ultra" {
		t.Fatalf("limits %+v ok=%v", l, ok)
	}
	if w := l.Windows[0]; w.Label != "Gemini 5시간" || w.UsedPercent < 27.9 || w.UsedPercent > 28.1 || w.ResetsAt != now.Unix()+9660 {
		t.Fatalf("5h window %+v", w)
	}
	abs := []byte(`{"quota":{"gemini-weekly":{"remaining_fraction":0.4,"reset_time":"2026-10-20T00:00:00Z"}}}`)
	WriteAgyStatus(path, abs, now)
	if l, ok := a.CurrentLimits(); !ok || l.Windows[0].Label != "Gemini 주간" || l.Windows[0].ResetsAt != 1792454400 {
		t.Fatalf("reset_time must be used: %+v", l)
	}
	if err := WriteAgyStatus(path, []byte("not json"), now); err == nil {
		t.Fatal("garbage must be rejected")
	}
	if _, ok := NewAgy("agy", filepath.Join(t.TempDir(), "missing.json")).CurrentLimits(); ok {
		t.Fatal("missing file must report not ok")
	}
	os.WriteFile(path, []byte(`{"observed_at":1,"data":{"model":{}}}`), 0o600)
	if _, ok := a.CurrentLimits(); ok {
		t.Fatal("status without quota must report not ok")
	}
}
