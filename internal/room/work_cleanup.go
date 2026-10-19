package room

import (
	"errors"
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
	sha    string // submitted commit for parts; saved integration tip otherwise
}

var errCleanupNativeOpen = errors.New("CLI 창을 닫으면 정리: 이 worktree에서 네이티브 CLI 세션이 열려 있음")

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
			targets = append(targets, worktreeCleanup{part.ID, part.ID, part.Worktree, part.Branch, part.SubmitSHA})
		}
	}
	if finished && !plan.IntegrationWorktreeRemoved {
		targets = append(targets, worktreeCleanup{name: "integration", path: plan.IntegrationWorktree, branch: plan.Integration, sha: plan.IntegrationSHA})
	}
	return targets
}

// Caller holds h.mu. Directory matching also covers historical part sessions.
func (h *Hub) worktreeNativeOpen(path string) bool {
	for _, dir := range h.nativeSessions {
		if sameWorktreePath(dir, path) {
			return true
		}
	}
	return false
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
				h.mu.Lock()
				nativeOpen := h.worktreeNativeOpen(target.path)
				h.mu.Unlock()
				var err error
				if nativeOpen {
					err = errCleanupNativeOpen
				} else {
					attempted[target.name] = true
					err = h.removePlanWorktree(repo, roomID, target)
				}
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
					if !attempted[target.name] && !h.worktreeNativeOpen(target.path) {
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
	lastError := &rs.room.Work.IntegrationCleanupError
	if target.partID != "" {
		label = "파트 " + target.partID
		lastError = &rs.room.Work.part(target.partID).WorktreeCleanupError
	}
	if err != nil {
		if *lastError == err.Error() {
			return
		}
		*lastError = err.Error()
		if errors.Is(err, errCleanupNativeOpen) {
			h.workNote(rs, fmt.Sprintf("%s worktree 정리 보류 · %v", label, err))
		} else {
			h.workNote(rs, fmt.Sprintf("%s worktree 정리 실패 · 상태와 branch는 유지: %v\n조치: 미커밋 파일을 커밋하거나 따로 보관하고 경로·HEAD·branch를 확인해. 폴더와 Git 등록이 어긋났으면 git worktree prune으로 남은 등록을 정리하고, 보존할 파일 확인 후 남은 폴더도 지워. 저장 경로의 폴더와 등록이 모두 없으면 다음 정리 시도에서 완료로 처리해.", label, err))
		}
		return
	}
	*lastError = ""
	if target.partID == "" {
		rs.room.Work.IntegrationWorktreeRemoved = true
	} else {
		rs.room.Work.part(target.partID).WorktreeRemoved = true
	}
	h.workNote(rs, fmt.Sprintf("%s worktree 정리 완료 · branch %s는 남겨 둠", label, target.branch))
}

func (h *Hub) removePlanWorktree(repo, roomID string, target worktreeCleanup) error {
	if target.path == "" {
		return fmt.Errorf("저장된 worktree 경로가 없음: %s", target.name)
	}
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
	list, err := runGit(repo, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return err
	}
	registered := false
	var branchPath string
	for _, record := range strings.Split(list, "\x00\x00") {
		var path, branch string
		for _, field := range strings.Split(record, "\x00") {
			if strings.HasPrefix(field, "worktree ") {
				path = strings.TrimPrefix(field, "worktree ")
			} else if strings.HasPrefix(field, "branch ") {
				branch = strings.TrimPrefix(field, "branch ")
			}
		}
		if sameWorktreePath(path, target.path) {
			registered = true
		}
		if branch == "refs/heads/"+target.branch {
			branchPath = path
		}
	}
	if !registered && branchPath != "" {
		return fmt.Errorf("branch의 등록 경로가 저장 경로와 다름: %s (저장 %s)", branchPath, target.path)
	}
	_, pathErr := os.Lstat(target.path)
	if !registered && os.IsNotExist(pathErr) {
		// A previous remove may have completed before its result was saved.
		// Only the validated, persisted path can be reconciled this way.
		return nil
	}
	if !registered {
		return fmt.Errorf("Git worktree 등록 목록에 저장 경로가 없음: %s", target.path)
	}
	if pathErr != nil {
		return fmt.Errorf("등록된 worktree 경로를 읽지 못함: %w", pathErr)
	}
	if target.partID != "" {
		branch, err := gitLine(target.path, "symbolic-ref", "--quiet", "HEAD")
		if err != nil || branch != "refs/heads/"+target.branch {
			return fmt.Errorf("HEAD가 파트 branch를 가리키지 않음: %s (기대 %s)", target.path, target.branch)
		}
	}
	if target.sha == "" {
		return fmt.Errorf("보존할 커밋 기록이 없음: %s", target.name)
	}
	if _, err := runGit(repo, "merge-base", "--is-ancestor", target.sha, "refs/heads/"+target.branch); err != nil {
		return fmt.Errorf("branch가 기록된 커밋 %s를 포함하는지 확인하지 못함: %w", target.sha, err)
	}
	// Integration intentionally uses detached HEAD; preserve any additional
	// commits that are not reachable from its retained branch.
	head, err := gitLine(target.path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	if _, err := runGit(repo, "merge-base", "--is-ancestor", head, "refs/heads/"+target.branch); err != nil {
		return fmt.Errorf("worktree HEAD가 남겨 둘 branch에 보존되어 있지 않음: %w", err)
	}
	_, err = runGit(repo, "worktree", "remove", target.path)
	return err
}

func sameWorktreePath(a, b string) bool {
	rel, err := filepath.Rel(a, b)
	return err == nil && rel == "."
}
