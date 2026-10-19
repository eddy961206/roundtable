package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Tests for the defects found by the cross-vendor review (Codex, 2026-10-16).

func TestIncompleteStreamIsNotComplete(t *testing.T) {
	p := newClaudeParser()
	p.Line(`{"type":"system","subtype":"init","session_id":"s1"}`, func(Event) {})
	p.Line(`{"type":"assistant","message":{"content":[{"type":"text","text":"반쯤 쓰다가"}]}}`, func(Event) {})
	fin := p.Finish(func(Event) {})
	if fin.Complete || fin.Text != "반쯤 쓰다가" {
		t.Fatalf("partial stream must not be complete: %+v", fin)
	}
	if err := finishError("claude", fin, nil, ""); err == nil {
		t.Fatal("missing result event must be an error even with text")
	}
	c := newCodexParser()
	c.Line(`{"type":"thread.started","thread_id":"t"}`, func(Event) {})
	c.Line(`{"type":"item.completed","item":{"type":"agent_message","text":"중간"}}`, func(Event) {})
	if fin := c.Finish(func(Event) {}); fin.Complete {
		t.Fatal("codex without turn.completed must not be complete")
	}
	c.Line(`{"type":"turn.completed","usage":{}}`, func(Event) {})
	if fin := c.Finish(func(Event) {}); !fin.Complete || finishError("codex", fin, nil, "") != nil {
		t.Fatal("codex with turn.completed must be complete")
	}
	g := newAgyParser()
	g.Line(`{"event":"init","conversation_id":"c"}`, func(Event) {})
	if fin := g.Finish(func(Event) {}); fin.Complete {
		t.Fatal("agy without result must not be complete")
	}
}

func TestResolveCommandWithExtension(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows")
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "codex.exe"), []byte("x"), 0o600)
	t.Setenv("PATH", dir)
	if got, err := resolveCommand("codex.exe"); err != nil || got != filepath.Join(dir, "codex.exe") {
		t.Fatalf("got %q err %v", got, err)
	}
}
