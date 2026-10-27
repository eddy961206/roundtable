package room

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"roundtable/internal/agent"
)

// ErrWorkspaceBusy refuses work for a folder another room is running in when
// either room writes (docs/다중에이전트_도구조사_20261027.md item 3). Only the
// exact same folder inside a git work tree counts. A folder outside git, such
// as a worktrees root that rooms share while each works in its own worktree
// below it, is never locked; that is left to the orchestrator's claims.
var ErrWorkspaceBusy = errors.New("작업 폴더 사용 중")

func workspaceKey(r *Room) string {
	if r.Mode == agent.ModeChat || strings.TrimSpace(r.Workspace) == "" {
		return ""
	}
	k := filepath.Clean(r.Workspace)
	if !inGitWorkTree(k) {
		return ""
	}
	if runtime.GOOS == "windows" {
		k = strings.ToLower(k)
	}
	return k
}

// inGitWorkTree tells whether dir or a folder above it has a .git entry (a
// directory, or the file a linked worktree has).
func inGitWorkTree(dir string) bool {
	for {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// workspaceRival is another room with a running turn in rs's folder that rs may
// not run beside. Caller holds h.mu.
func (h *Hub) workspaceRival(rs *roomState) *roomState {
	key := workspaceKey(rs.room)
	if key == "" {
		return nil
	}
	for _, other := range h.rooms {
		if other == rs || workspaceKey(other.room) != key || !(rs.room.Mode == agent.ModeWrite || other.room.Mode == agent.ModeWrite) {
			continue
		}
		for _, run := range other.running {
			if runsInRoomFolder(other, run.turn) {
				return other
			}
		}
	}
	return nil
}

// runsInRoomFolder: part and review turns, and any turn of a seat with an
// active part, run in that part's own worktree.
func runsInRoomFolder(rs *roomState, t *Turn) bool {
	return t.Reason != "part" && t.Reason != "review" && rs.activePart(t.Agent) == nil
}

func rivalError(other *roomState) error {
	return fmt.Errorf("%w: 방 %s(%s, %s 모드)이 같은 작업 폴더 %s에서 실행 중 · 그 방의 턴이 끝난 뒤 다시 보내거나 다른 worktree를 쓰는 방을 써",
		ErrWorkspaceBusy, other.room.ID, other.room.Title, other.room.Mode, other.room.Workspace)
}

// pumpPeers starts turns that waited for rs's folder. Caller holds h.mu.
func (h *Hub) pumpPeers(rs *roomState) {
	key := workspaceKey(rs.room)
	if key == "" {
		return
	}
	for _, other := range h.rooms {
		if other != rs && len(other.queue) > 0 && workspaceKey(other.room) == key {
			h.pump(other)
		}
	}
}
