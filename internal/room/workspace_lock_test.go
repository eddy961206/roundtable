package room

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func roomIn(t *testing.T, h *Hub, title, mode, dir string) *Room {
	t.Helper()
	r, err := h.CreateRoom(RoomSpec{Title: title, Mode: mode, Workspace: dir, Participants: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// 2026-10-27: a reviewer ran in the worktree an author was still changing, so the
// reviewed commit and the files on disk differed. Two rooms on one folder may not
// run together when either writes; posting is refused with the busy room named,
// and turns already queued wait until that room is done.
func TestWorkspaceBusyRefusesPostAndWaitsQueuedTurns(t *testing.T) {
	h, _, ps := controlledHub(t, "codex")
	dir := t.TempDir()
	writer := roomIn(t, h, "작성 방", "write", dir)
	reviewer := roomIn(t, h, "검수 방", "read", dir)
	if _, err := h.PostUser(writer.ID, "write it", ""); err != nil {
		t.Fatal(err)
	}
	running := nextCall(t, ps[0])

	_, err := h.PostUser(reviewer.ID, "review it", "")
	if !errors.Is(err, ErrWorkspaceBusy) || !strings.Contains(err.Error(), writer.ID) || !strings.Contains(err.Error(), "작성 방") {
		t.Fatalf("post must be refused naming the busy room: %v", err)
	}
	snap, _ := h.Snapshot(reviewer.ID)
	if len(snap.Messages) != 0 {
		t.Fatalf("a refused post must not be stored: %+v", snap.Messages)
	}

	// A turn that reaches the queue another way (a chained mention) waits.
	h.mu.Lock()
	rs := h.rooms[reviewer.ID]
	m := rs.nextMessage("user", "user", "queued review")
	rs.enqueue(&Turn{ID: newID(), Agent: "codex", Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
	h.pump(rs)
	h.mu.Unlock()
	noMoreCalls(t, ps[0])

	finishCall(running, "written")
	review := nextCall(t, ps[0]) // the writer finished: the waiting review starts
	if !strings.Contains(review.opts.Prompt, "queued review") {
		t.Fatalf("%s", review.opts.Prompt)
	}
	finishCall(review, "reviewed")
	waitIdle(t, h, reviewer.ID)
	if _, err := h.PostUser(reviewer.ID, "again", ""); err != nil {
		t.Fatalf("an idle folder accepts posts: %v", err)
	}
	finishCall(nextCall(t, ps[0]), "ok")
	waitIdle(t, h, reviewer.ID)
}

func TestWorkspaceBusyOnlyWhenOneWritesInTheSameFolder(t *testing.T) {
	h, _, ps := controlledHub(t, "codex")
	dir := t.TempDir()
	sub := filepath.Join(dir, "wt")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name               string
		aMode, bMode, bDir string
		busy               bool
	}{
		{"two readers share a folder", "read", "read", dir, false},
		{"writer blocks a reader", "write", "read", dir, true},
		{"reader blocks a writer", "read", "write", dir, true},
		{"different folders", "write", "write", sub, false},
		{"a parent folder is not compared", "write", "write", filepath.Dir(dir), false},
		{"chat rooms have no folder", "write", "chat", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := roomIn(t, h, "A", tc.aMode, dir)
			b := roomIn(t, h, "B", tc.bMode, tc.bDir)
			if _, err := h.PostUser(a.ID, "work", ""); err != nil {
				t.Fatal(err)
			}
			running := nextCall(t, ps[0])
			_, err := h.PostUser(b.ID, "work too", "")
			if busy := errors.Is(err, ErrWorkspaceBusy); busy != tc.busy {
				t.Fatalf("busy=%v err=%v", busy, err)
			}
			finishCall(running, "done")
			if !tc.busy {
				finishCall(nextCall(t, ps[0]), "done")
			}
			waitIdle(t, h, a.ID)
			waitIdle(t, h, b.ID)
		})
	}
}
