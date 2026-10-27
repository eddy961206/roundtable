package room

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// A room's workspace can disappear after the room is created (for example a
// git worktree removed after its PR merged). The turn must fail with a reason
// that names the folder instead of the OS process-start error, and the
// provider must not be started.
func TestMissingWorkspaceFailsBeforeProviderRun(t *testing.T) {
	prov := &fakeProvider{id: "codex"}
	h, _ := newTestHub(t, prov)
	dir := filepath.Join(t.TempDir(), "ws")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := h.CreateRoom(RoomSpec{Title: "작업", Mode: agent.ModeWrite, Workspace: dir, Participants: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := h.PostUser(r.ID, "go", ""); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, h, r.ID)
	snap, err := h.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := snap.Messages[len(snap.Messages)-1]
	if last.Status != "error" || !strings.Contains(last.Error, "작업 폴더가 없") || !strings.Contains(last.Error, dir) {
		t.Fatalf("want missing-workspace error naming %s, got status=%q error=%q", dir, last.Status, last.Error)
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	if len(prov.prompts) != 0 {
		t.Fatalf("provider ran %d times with a missing workspace", len(prov.prompts))
	}
}
