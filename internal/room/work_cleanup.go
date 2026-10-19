package room

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type worktreeCleanup struct {
	partID string // empty for the integration worktree
	name   string
	path   string
	branch string
}

func removedWorktreeError(part *Part) error {
	return fmt.Errorf("%w: 파트 %s worktree는 이미 정리됨: %s", ErrWorkConflict, part.ID, part.Worktree)
}

// worktreeCleanupTargets snapshots only terminal work while no merge can be
// using the integration directory. Caller holds h.mu (or owns startup recovery).
func (h *Hub) worktreeCleanupTargets(rs *roomState) []worktreeCleanup {
	plan := rs.room.Work
	if plan == nil || plan.Merging != nil {
		return nil
	}
	finished := len(plan.Parts) > 0
	var targets []worktreeCleanup
	for _, part := range plan.Parts {
		if part.State == "merging" {
			return nil // mergeAndVerify runs before MergeIntent is written
		}
		if part.State != "merged" && part.State != "abandoned" {
			finished = false
		}
		if part.State == "merged" && !part.WorktreeRemoved {
			targets = append(targets, worktreeCleanup{part.ID, part.ID, part.Worktree, part.Branch})
		}
	}
	if finished && !plan.IntegrationWorktreeRemoved {
		targets = append(targets, worktreeCleanup{name: "integration", path: filepath.Join(h.workRoot, rs.room.ID, "integration"), branch: plan.Integration})
	}
	return targets
}

// startWorktreeCleanup reserves the merge gate under h.mu, then runs git outside
// it. The worker is drained by Shutdown. Failed removals are retried on restart
// or the next terminal transition, never in a tight retry loop.
func (h *Hub) startWorktreeCleanup(rs *roomState) {
	if h.closing || rs.deleted || rs.workBusy[busyCleanup] {
		return
	}
	targets := h.worktreeCleanupTargets(rs)
	if len(targets) == 0 {
		return
	}
	rs.workBusy[busyCleanup] = true
	repo, roomID := rs.room.Workspace, rs.room.ID
	h.turnWG.Add(1)
	go func() {
		defer h.turnWG.Done()
		attempted := map[string]bool{}
		for {
			for _, target := range targets {
				attempted[target.name] = true
				err := h.removePlanWorktree(repo, roomID, target)
				h.mu.Lock()
				if !rs.deleted {
					h.recordWorktreeCleanup(rs, target, err)
					if saveErr := h.saveWork(rs); saveErr != nil {
						h.workNote(rs, "worktree 정리 결과 저장 실패: "+saveErr.Error())
					}
				}
				h.mu.Unlock()
			}
			h.mu.Lock()
			// A different part may have been abandoned while git ran. Include its
			// newly finished plan without retrying failed removals in this worker.
			targets = nil
			if !rs.deleted {
				for _, target := range h.worktreeCleanupTargets(rs) {
					if !attempted[target.name] {
						targets = append(targets, target)
					}
				}
			}
			if len(targets) != 0 {
				h.mu.Unlock()
				continue
			}
			delete(rs.workBusy, busyCleanup)
			if !rs.deleted {
				h.advanceQueue(rs)
				_ = h.saveWork(rs)
			}
			h.mu.Unlock()
			return
		}
	}()
}

func (h *Hub) recordWorktreeCleanup(rs *roomState, target worktreeCleanup, err error) {
	label := "통합"
	if target.partID != "" {
		label = "파트 " + target.partID
	}
	if err != nil {
		h.workNote(rs, fmt.Sprintf("%s worktree 정리 실패 · 상태와 branch는 유지: %v", label, err))
		return
	}
	if target.partID == "" {
		rs.room.Work.IntegrationWorktreeRemoved = true
	} else {
		rs.room.Work.part(target.partID).WorktreeRemoved = true
	}
	h.workNote(rs, fmt.Sprintf("%s worktree 정리 완료 · branch %s는 남겨 둠", label, target.branch))
}

func (h *Hub) removePlanWorktree(repo, roomID string, target worktreeCleanup) error {
	// Reject traversal and paths that do not match this plan's generated layout.
	for _, component := range []string{roomID, target.name} {
		if component == "" || component == "." || component == ".." || strings.ContainsAny(component, `/\:`) {
			return fmt.Errorf("계획의 worktree 경로가 아님: %s", target.path)
		}
	}
	expected := filepath.Join(h.workRoot, roomID, target.name)
	if !filepath.IsAbs(target.path) || !sameWorktreePath(expected, target.path) || target.branch != "rt/"+roomID+"/"+target.name {
		return fmt.Errorf("계획의 worktree 경로 또는 branch와 일치하지 않음: %s", target.path)
	}
	dataRoot, err := filepath.Abs(h.store.Root())
	if err != nil {
		return err
	}
	if !sameWorktreePath(h.workRoot, filepath.Join(dataRoot, "worktrees")) {
		return fmt.Errorf("worktree 경로가 데이터 폴더 아래가 아님: %s", target.path)
	}
	realData, err := filepath.EvalSymlinks(dataRoot)
	if err != nil {
		return err
	}
	// Check each existing ancestor too: a room directory junction must not
	// redirect deletion to another plan or outside the data directory.
	path, realExpected := dataRoot, realData
	for _, component := range []string{"worktrees", roomID, target.name} {
		path, realExpected = filepath.Join(path, component), filepath.Join(realExpected, component)
		actual, err := filepath.EvalSymlinks(path)
		if os.IsNotExist(err) {
			break
		}
		if err != nil {
			return err
		}
		if !sameWorktreePath(actual, realExpected) {
			return fmt.Errorf("worktree 경로가 다른 위치로 연결됨: %s", path)
		}
	}
	if _, err := os.Lstat(target.path); os.IsNotExist(err) {
		// A crash can occur after git removes the worktree but before the flag
		// is saved. Accept absence only if git's registration is also gone.
		list, listErr := runGit(repo, "worktree", "list", "--porcelain", "-z")
		if listErr != nil {
			return listErr
		}
		registered := false
		for _, field := range strings.Split(list, "\x00") {
			if strings.HasPrefix(field, "worktree ") && sameWorktreePath(strings.TrimPrefix(field, "worktree "), target.path) {
				registered = true
				break
			}
		}
		if !registered {
			return nil
		}
	} else if err != nil {
		return err
	}
	_, err = runGit(repo, "worktree", "remove", target.path)
	return err
}

func sameWorktreePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}
