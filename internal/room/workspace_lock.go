package room

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"

	"roundtable/internal/agent"
)

// ErrWorkspaceBusy refuses work for a folder another room is running in when
// either room writes (docs/다중에이전트_도구조사_20261027.md item 3). Only the
// exact same folder counts: a room on a parent folder (e.g. a worktrees root)
// cannot be told apart by path and is left to the orchestrator's claims.
var ErrWorkspaceBusy = errors.New("작업 폴더 사용 중")

func workspaceKey(r *Room) string {
	if r.Mode == agent.ModeChat || strings.TrimSpace(r.Workspace) == "" {
		return ""
	}
	k := filepath.Clean(r.Workspace)
	if runtime.GOOS == "windows" {
		k = strings.ToLower(k)
	}
	return k
}

// workspaceRival is another room with a running turn in rs's folder that rs may
// not run beside. Caller holds h.mu.
func (h *Hub) workspaceRival(rs *roomState) *roomState {
	key := workspaceKey(rs.room)
	if key == "" {
		return nil
	}
	for _, other := range h.rooms {
		if other == rs || len(other.running) == 0 || workspaceKey(other.room) != key {
			continue
		}
		if rs.room.Mode == agent.ModeWrite || other.room.Mode == agent.ModeWrite {
			return other
		}
	}
	return nil
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
