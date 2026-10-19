package room

// Work division: plan, part worktrees, submit checks, review steps and the
// merge queue. Contract: docs/작업분배_적대적리뷰_계약.md §3·§5·§6 (P2).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"roundtable/internal/agent"
)

// ErrWorkConflict marks a request that does not fit the current plan state (HTTP 409).
var ErrWorkConflict = errors.New("작업 상태 충돌")

const (
	defaultReviewRounds  = 3
	defaultVerifyTimeout = 600
	gitTimeout           = 2 * time.Minute
	outputTailBytes      = 8 * 1024
	diffPatchLimit       = 1 << 20
	workDoneSignal       = "WORK:DONE"
	busyPlan             = "plan" // workBusy key while CreateWork runs git outside the hub lock
)

// ---- plan helpers -----------------------------------------------------------

func (plan *WorkPlan) part(id string) *Part {
	for _, p := range plan.Parts {
		if p.ID == id {
			return p
		}
	}
	return nil
}

// refreshHalted keeps D5: the queue stays stopped while any part is blocked.
func (plan *WorkPlan) refreshHalted() {
	plan.Halted = nil
	for _, p := range plan.Parts {
		if p.State == "blocked" {
			halted := *p.Block
			plan.Halted = &halted
			return
		}
	}
}

// activePart is the seat's working or fixing part; its turns run there (D9).
func (rs *roomState) activePart(seat string) *Part {
	p := rs.seatPart(seat)
	if p != nil && (p.State == "working" || p.State == "fixing") {
		return p
	}
	return nil
}

// seatPart owns the seat's work context until the part is merged or abandoned.
func (rs *roomState) seatPart(seat string) *Part {
	if rs.room.Work == nil {
		return nil
	}
	for _, p := range rs.room.Work.Parts {
		if p.Seat == seat && p.State != "merged" && p.State != "abandoned" {
			return p
		}
	}
	return nil
}

// cloneWork detaches a plan from hub state so callers can serialize it unlocked.
func cloneWork(plan *WorkPlan) *WorkPlan {
	if plan == nil {
		return nil
	}
	b, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	var out WorkPlan
	if err := json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return &out
}

func approvalValid(plan *WorkPlan, part *Part) bool {
	a := part.Approval
	return a != nil && a.SubmitSHA == part.SubmitSHA && a.IntegrationSHA == plan.IntegrationSHA
}

func shortSHA(sha string) string {
	if len(sha) > 10 {
		return sha[:10]
	}
	return sha
}

// workNote records a plan state change as a persisted system message (D13).
func (h *Hub) workNote(rs *roomState, text string) {
	m := rs.addSystem(text)
	_ = h.store.AppendMessage(rs.room.ID, m)
}

func (h *Hub) saveWork(rs *roomState) error {
	rs.broadcast("work", rs.room.Work)
	return h.store.SaveRoom(rs.room)
}

func (h *Hub) blockPart(rs *roomState, part *Part, kind, from, message string, result *CommandResult) {
	part.State, part.Approval = "blocked", nil
	part.Block = &Block{Kind: kind, From: from, Message: message, Result: result, At: nowMs()}
	rs.room.Work.refreshHalted()
	h.workNote(rs, fmt.Sprintf("파트 %s 정지(%s): %s", part.ID, kind, message))
}

// workPart resolves a part for an API call. Caller holds h.mu.
func (h *Hub) workPart(roomID, partID string) (*roomState, *Part, error) {
	if h.closing {
		return nil, nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[roomID]
	if !ok {
		return nil, nil, fmt.Errorf("방 없음: %s", roomID)
	}
	if rs.room.Work == nil {
		return nil, nil, fmt.Errorf("작업 계획 없음: %s", roomID)
	}
	part := rs.room.Work.part(partID)
	if part == nil {
		return nil, nil, fmt.Errorf("파트 없음: %s", partID)
	}
	return rs, part, nil
}

// ---- plan creation (input boundary) ------------------------------------------

func planFromSpec(r *Room, spec WorkSpec) (*WorkPlan, error) {
	goal := strings.TrimSpace(spec.Goal)
	if goal == "" {
		return nil, fmt.Errorf("작업 목표가 비어 있음")
	}
	mode := spec.MergeMode
	if mode == "" {
		mode = "confirm"
	}
	if mode != "auto" && mode != "confirm" {
		return nil, fmt.Errorf("알 수 없는 병합 모드: %s", mode)
	}
	rounds := spec.ReviewRounds
	if rounds == 0 {
		rounds = defaultReviewRounds
	}
	if rounds < 1 {
		return nil, fmt.Errorf("리뷰 라운드는 1 이상이어야 함")
	}
	verify := make([]VerifyCmd, 0, len(spec.Verify))
	for _, v := range spec.Verify {
		if strings.TrimSpace(v.Name) == "" || len(v.Argv) == 0 || strings.TrimSpace(v.Argv[0]) == "" {
			return nil, fmt.Errorf("검증 명령에는 이름과 argv가 필요함")
		}
		if v.TimeoutSec == 0 {
			v.TimeoutSec = defaultVerifyTimeout
		}
		if v.TimeoutSec < 0 {
			return nil, fmt.Errorf("검증 명령 %s의 제한 시간이 음수임", v.Name)
		}
		v.Argv = slices.Clone(v.Argv)
		verify = append(verify, v)
	}
	if len(spec.Parts) == 0 {
		return nil, fmt.Errorf("파트가 1개 이상 필요함")
	}
	plan := &WorkPlan{Goal: goal, Integration: "rt/" + r.ID + "/integration", MergeMode: mode, ReviewRounds: rounds, Verify: verify, Queue: []string{}}
	seats := map[string]bool{}
	for i, ps := range spec.Parts {
		id := fmt.Sprintf("p%d", i+1)
		title, brief := strings.TrimSpace(ps.Title), strings.TrimSpace(ps.Brief)
		if title == "" || brief == "" {
			return nil, fmt.Errorf("파트 %d: 제목과 완료 조건이 필요함", i+1)
		}
		if len(ps.Scope) == 0 {
			return nil, fmt.Errorf("파트 %d: 범위가 1개 이상 필요함", i+1)
		}
		for _, scope := range ps.Scope {
			clean := path.Clean(filepath.ToSlash(scope))
			if strings.TrimSpace(scope) == "" || filepath.VolumeName(scope) != "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				return nil, fmt.Errorf("파트 %d: 범위는 저장소 상대 경로여야 함: %q", i+1, scope)
			}
		}
		seat, reviewer := r.participant(ps.Seat), r.participant(ps.Reviewer)
		if seat == nil || seat.Removed || !seat.Enabled {
			return nil, fmt.Errorf("파트 %d: 활성 담당 좌석이 아님: %s", i+1, ps.Seat)
		}
		if reviewer == nil || reviewer.Removed || !reviewer.Enabled {
			return nil, fmt.Errorf("파트 %d: 활성 리뷰어 좌석이 아님: %s", i+1, ps.Reviewer)
		}
		if reviewer.Provider == seat.Provider {
			return nil, fmt.Errorf("파트 %d: 리뷰어는 담당과 다른 공급사여야 함(%s)", i+1, seat.Provider)
		}
		if seats[seat.ID] {
			return nil, fmt.Errorf("파트 %d: 좌석 %s가 이미 다른 파트를 맡음", i+1, seat.ID)
		}
		seats[seat.ID] = true
		plan.Parts = append(plan.Parts, &Part{ID: id, Title: title, Brief: brief, Scope: slices.Clone(ps.Scope), Seat: seat.ID, Reviewer: reviewer.ID, Branch: "rt/" + r.ID + "/" + id, State: "working"})
	}
	return plan, nil
}

// CreateWork validates the spec, creates the integration and part worktrees from
// the workspace HEAD and starts every part session.
func (h *Hub) CreateWork(roomID string, spec WorkSpec) (*WorkPlan, error) {
	rs, plan, repo, err := h.reservePlan(roomID, spec)
	if err != nil {
		return nil, err
	}
	made, err := createWorktrees(repo, filepath.Join(h.workRoot, roomID), plan)
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(rs.workBusy, busyPlan)
	if err == nil && (rs.deleted || h.closing) {
		err = errors.Join(fmt.Errorf("작업 계획을 만드는 동안 방이 삭제되었거나 서버가 종료됨"), removeWorktrees(repo, made))
	}
	if err != nil {
		return nil, err
	}
	rs.room.Work = plan
	for _, part := range plan.Parts {
		part.LastSeen = rs.room.Seq
	}
	if err := h.store.SaveRoom(rs.room); err != nil {
		rs.room.Work = nil
		return nil, errors.Join(err, removeWorktrees(repo, made))
	}
	h.workNote(rs, fmt.Sprintf("작업 계획 시작 · 파트 %d개 · 기준 %s · 통합 branch %s · 병합 모드 %s", len(plan.Parts), shortSHA(plan.BaseSHA), plan.Integration, plan.MergeMode))
	rs.broadcast("work", plan)
	for _, part := range plan.Parts {
		rs.enqueue(&Turn{ID: newID(), Agent: part.Seat, Reason: "part", Part: part.ID, Cap: rs.room.Seq, Created: nowMs(), Status: "queued"})
	}
	h.pump(rs)
	return cloneWork(plan), nil
}

// reservePlan validates under the hub lock and marks the room busy while git runs unlocked.
func (h *Hub) reservePlan(roomID string, spec WorkSpec) (*roomState, *WorkPlan, string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, nil, "", fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[roomID]
	if !ok {
		return nil, nil, "", fmt.Errorf("방 없음: %s", roomID)
	}
	if rs.room.Mode != agent.ModeWrite {
		return nil, nil, "", fmt.Errorf("작업 계획은 write 방에서만 만들 수 있음")
	}
	if rs.room.Work != nil || rs.workBusy[busyPlan] {
		return nil, nil, "", fmt.Errorf("%w: 이 방에는 이미 작업 계획이 있음", ErrWorkConflict)
	}
	plan, err := planFromSpec(rs.room, spec)
	if err != nil {
		return nil, nil, "", err
	}
	rs.workBusy[busyPlan] = true
	return rs, plan, rs.room.Workspace, nil
}

// worktreeSet lists what createWorktrees made so a failed plan is undone without force.
type worktreeSet struct {
	base      string
	worktrees []string
	branches  []string
}

func createWorktrees(repo, dir string, plan *WorkPlan) (*worktreeSet, error) {
	base, err := gitLine(repo, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return nil, fmt.Errorf("작업 폴더의 기준 커밋을 읽지 못함: %w", err)
	}
	top, err := gitLine(repo, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if rel, err := filepath.Rel(filepath.Clean(filepath.FromSlash(top)), dir); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("worktree 폴더가 사용자 저장소 안에 있음: %s", dir)
	}
	branches := []string{plan.Integration}
	paths := []string{filepath.Join(dir, "integration")}
	for _, p := range plan.Parts {
		branches = append(branches, p.Branch)
		paths = append(paths, filepath.Join(dir, p.ID))
	}
	for _, b := range branches {
		exists, err := branchExists(repo, b)
		if err != nil {
			return nil, err
		}
		if exists {
			return nil, fmt.Errorf("이미 있는 branch: %s", b)
		}
	}
	for _, p := range paths {
		if _, err := os.Lstat(p); err == nil {
			return nil, fmt.Errorf("이미 있는 경로: %s", p)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	made := &worktreeSet{base: base}
	add := func(branch, wt string, worktreeArgs ...string) error {
		if _, err := runGit(repo, "branch", branch, base); err != nil {
			return err
		}
		made.branches = append(made.branches, branch)
		if _, err := runGit(repo, append([]string{"worktree", "add"}, worktreeArgs...)...); err != nil {
			return err
		}
		made.worktrees = append(made.worktrees, wt)
		return nil
	}
	// The integration worktree stays detached so update-ref can move its branch (D2).
	if err := add(plan.Integration, paths[0], "--detach", paths[0], base); err != nil {
		return nil, errors.Join(err, removeWorktrees(repo, made))
	}
	for i, p := range plan.Parts {
		if err := add(p.Branch, paths[i+1], paths[i+1], p.Branch); err != nil {
			return nil, errors.Join(err, removeWorktrees(repo, made))
		}
		p.Worktree = paths[i+1]
	}
	plan.BaseSHA, plan.IntegrationSHA = base, base
	return made, nil
}

// removeWorktrees undoes only resources this plan created: clean worktrees and
// branches that still point at the base commit.
func removeWorktrees(repo string, made *worktreeSet) error {
	var errs []error
	for _, wt := range slices.Backward(made.worktrees) {
		if _, err := runGit(repo, "worktree", "remove", wt); err != nil {
			errs = append(errs, err)
		}
	}
	for _, b := range made.branches {
		if _, err := runGit(repo, "update-ref", "-d", "refs/heads/"+b, made.base); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// ---- plan controls ------------------------------------------------------------

// SetMergeMode flips the room toggle. Parts already queued keep their ReadyMode (D6).
func (h *Hub) SetMergeMode(roomID, mode string) (*WorkPlan, error) {
	if mode != "auto" && mode != "confirm" {
		return nil, fmt.Errorf("알 수 없는 병합 모드: %s", mode)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[roomID]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", roomID)
	}
	plan := rs.room.Work
	if plan == nil {
		return nil, fmt.Errorf("작업 계획 없음: %s", roomID)
	}
	plan.MergeMode = mode
	h.workNote(rs, "병합 모드 변경: "+mode+" (이미 대기열에 들어간 파트는 그때 모드를 유지)")
	if err := h.saveWork(rs); err != nil {
		return nil, err
	}
	return cloneWork(plan), nil
}

// SubmitPart runs the §3 submit checks and starts (or resumes) the review.
func (h *Hub) SubmitPart(roomID, partID string) (*WorkPlan, error) {
	h.mu.Lock()
	rs, part, err := h.workPart(roomID, partID)
	if err == nil {
		err = h.reserveSubmit(rs, part)
	}
	h.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return h.submitReserved(rs, part)
}

// reserveSubmit closes the write-turn gate before releasing h.mu for git.
func (h *Hub) reserveSubmit(rs *roomState, part *Part) error {
	switch {
	case part.State != "working" && part.State != "fixing":
		return fmt.Errorf("%w: 파트 %s는 %s 상태라 제출할 수 없음", ErrWorkConflict, part.ID, part.State)
	case rs.workBusy[part.ID]:
		return fmt.Errorf("%w: 파트 %s 제출 검사가 이미 진행 중", ErrWorkConflict, part.ID)
	case rs.runningFor(part.Seat) != nil:
		return fmt.Errorf("%w: 파트 %s 담당 좌석이 실행 중", ErrWorkConflict, part.ID)
	}
	rs.workBusy[part.ID] = true
	return nil
}

func (h *Hub) submitReserved(rs *roomState, part *Part) (*WorkPlan, error) {
	h.mu.Lock()
	roomID, partID := rs.room.ID, part.ID
	from, prev, repo, base := part.State, part.SubmitSHA, rs.room.Workspace, rs.room.Work.BaseSHA
	wt, branch := part.Worktree, part.Branch
	h.mu.Unlock()

	sha, err := checkSubmit(wt, branch, base, prev)
	var diff *PartDiff
	if err == nil {
		diff, err = partDiff(repo, partID, base, sha)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(rs.workBusy, partID)
	if rs.deleted {
		return nil, fmt.Errorf("방 없음: %s", roomID)
	}
	if part.State != from {
		return nil, fmt.Errorf("%w: 제출 검사 중 파트 %s가 %s 상태로 바뀜", ErrWorkConflict, partID, part.State)
	}
	if err != nil {
		h.blockPart(rs, part, "submit", from, "제출 검사 실패: "+err.Error(), nil)
	} else {
		h.startReview(rs, part, from, sha, diff)
	}
	saveErr := h.saveWork(rs)
	h.pump(rs)
	if saveErr != nil {
		return nil, saveErr
	}
	return cloneWork(rs.room.Work), nil
}

func checkSubmit(wt, branch, base, prev string) (string, error) {
	head, err := gitLine(wt, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return "", fmt.Errorf("파트 worktree의 branch를 읽지 못함: %w", err)
	}
	if head != branch {
		return "", fmt.Errorf("파트 worktree가 %s가 아니라 %s에 있음", branch, head)
	}
	status, err := runGit(wt, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(status) != "" {
		return "", fmt.Errorf("커밋하지 않은 변경 또는 추적하지 않는 파일이 있음:\n%s", clipRunes(strings.TrimRight(status, "\n"), 2000))
	}
	sha, err := gitLine(wt, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", err
	}
	if _, err := runGit(wt, "merge-base", "--is-ancestor", base, sha); err != nil {
		return "", fmt.Errorf("기준 커밋 %s에서 이어진 이력이 아님: %w", shortSHA(base), err)
	}
	count, err := gitLine(wt, "rev-list", "--count", base+".."+sha)
	if err != nil {
		return "", err
	}
	n, err := strconv.Atoi(count)
	if err != nil {
		return "", fmt.Errorf("커밋 수를 읽지 못함: %w", err)
	}
	if n == 0 {
		return "", fmt.Errorf("기준 커밋 %s 이후 커밋이 없음", shortSHA(base))
	}
	if sha == prev {
		return "", fmt.Errorf("이전 제출 %s와 같은 커밋이라 수정 내용이 없음", shortSHA(sha))
	}
	return sha, nil
}

// ApprovePart is the confirm-mode merge button. The approval binds both SHAs (D6).
func (h *Hub) ApprovePart(roomID, partID, submitSHA, integrationSHA string) (*WorkPlan, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, part, err := h.workPart(roomID, partID)
	if err != nil {
		return nil, err
	}
	plan := rs.room.Work
	switch {
	case part.State != "awaiting_approval":
		return nil, fmt.Errorf("%w: 파트 %s는 %s 상태라 승인할 수 없음", ErrWorkConflict, partID, part.State)
	case plan.Halted != nil:
		return nil, fmt.Errorf("%w: 병합 대기열이 정지되어 있음", ErrWorkConflict)
	case submitSHA != part.SubmitSHA || integrationSHA != plan.IntegrationSHA:
		return nil, fmt.Errorf("%w: 승인 기준이 현재 값과 다름(제출 %s, 통합 %s)", ErrWorkConflict, shortSHA(part.SubmitSHA), shortSHA(plan.IntegrationSHA))
	}
	part.Approval = &Approval{SubmitSHA: submitSHA, IntegrationSHA: integrationSHA, At: nowMs()}
	part.State = "queued"
	h.workNote(rs, fmt.Sprintf("파트 %s 병합 승인 · 제출 %s · 통합 %s", partID, shortSHA(submitSHA), shortSHA(integrationSHA)))
	h.advanceQueue(rs)
	if err := h.saveWork(rs); err != nil {
		return nil, err
	}
	return cloneWork(plan), nil
}

// RetryPart clears a block and returns the part to Block.From.
func (h *Hub) RetryPart(roomID, partID string) (*WorkPlan, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, part, err := h.workPart(roomID, partID)
	if err != nil {
		return nil, err
	}
	if part.State != "blocked" {
		return nil, fmt.Errorf("%w: 파트 %s는 정지 상태가 아님", ErrWorkConflict, partID)
	}
	from := part.Block.From
	var pending *ReviewStep
	if from == "reviewing" {
		pending = part.Review.Pending
		if pending == nil || pending.Prompt == "" {
			return nil, fmt.Errorf("%w: 파트 %s의 리뷰 단계를 다시 실행할 프롬프트가 없음", ErrWorkConflict, partID)
		}
	}
	part.State, part.Block = from, nil
	rs.room.Work.refreshHalted()
	h.workNote(rs, fmt.Sprintf("파트 %s 재시도 → %s", partID, from))
	if pending != nil {
		h.runReviewStep(rs, part, *pending)
	}
	h.advanceQueue(rs)
	saveErr := h.saveWork(rs)
	h.pump(rs)
	if saveErr != nil {
		return nil, saveErr
	}
	return cloneWork(rs.room.Work), nil
}

// AbandonPart drops a part from the plan. Its branch and worktree stay for the user.
func (h *Hub) AbandonPart(roomID, partID string) (*WorkPlan, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, part, err := h.workPart(roomID, partID)
	if err != nil {
		return nil, err
	}
	if part.State == "merging" || part.State == "merged" || part.State == "abandoned" || rs.workBusy[partID] {
		return nil, fmt.Errorf("%w: 파트 %s는 지금 포기할 수 없음(%s)", ErrWorkConflict, partID, part.State)
	}
	plan := rs.room.Work
	part.State, part.Block, part.Approval = "abandoned", nil, nil
	plan.Queue = slices.DeleteFunc(plan.Queue, func(id string) bool { return id == partID })
	plan.refreshHalted()
	rs.queue = slices.DeleteFunc(rs.queue, func(t *Turn) bool { return t.Part == partID })
	for _, run := range rs.running {
		if run.turn.Part == partID {
			run.cancel()
		}
	}
	h.workNote(rs, fmt.Sprintf("파트 %s 포기 · branch %s는 남겨 둠", partID, part.Branch))
	h.advanceQueue(rs)
	saveErr := h.saveWork(rs)
	h.pump(rs)
	if saveErr != nil {
		return nil, saveErr
	}
	return cloneWork(plan), nil
}

// PartDiff returns base..submit of a submitted part (D12).
func (h *Hub) PartDiff(roomID, partID string) (*PartDiff, error) {
	h.mu.Lock()
	rs, part, err := h.workPart(roomID, partID)
	if err == nil && part.SubmitSHA == "" {
		err = fmt.Errorf("%w: 파트 %s는 아직 제출되지 않음", ErrWorkConflict, partID)
	}
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	repo, base, head := rs.room.Workspace, rs.room.Work.BaseSHA, part.SubmitSHA
	h.mu.Unlock()
	return partDiff(repo, partID, base, head)
}

// ---- review steps (rules live in ReviewState) ---------------------------------

func (h *Hub) startReview(rs *roomState, part *Part, from, sha string, diff *PartDiff) {
	plan := rs.room.Work
	in := ReviewInput{Ref: ReviewRef{PartID: part.ID, Round: 1, BaseSHA: plan.BaseSHA, HeadSHA: sha}, Goal: plan.Goal, Brief: part.Brief, Scope: part.Scope, Diff: diff}
	var step ReviewStep
	var err error
	review := part.Review
	if from == "working" {
		review = NewReviewState(plan.ReviewRounds)
		step, err = review.Start(in)
	} else {
		in.Ref.Round = len(review.Rounds) + 1
		step, err = review.Resubmit(in)
	}
	if err != nil {
		h.blockPart(rs, part, "review", from, "리뷰를 시작하지 못함: "+err.Error(), nil)
		return
	}
	part.SubmitSHA, part.State, part.Review = sha, "reviewing", review
	h.workNote(rs, fmt.Sprintf("파트 %s 제출 · %s · 리뷰 %d라운드", part.ID, shortSHA(sha), in.Ref.Round))
	h.runReviewStep(rs, part, step)
}

func (h *Hub) runReviewStep(rs *roomState, part *Part, step ReviewStep) {
	agentID := part.Seat
	switch step.Who {
	case "reviewer":
		agentID = part.Reviewer
	case "author":
	default:
		h.blockPart(rs, part, "review", "reviewing", "알 수 없는 리뷰 단계 대상: "+step.Who, nil)
		return
	}
	rs.enqueue(&Turn{ID: newID(), Agent: agentID, Reason: "review", Part: part.ID, Cap: rs.room.Seq, Created: nowMs(), Status: "queued", prompt: step.Prompt, step: step.Kind})
}

// finishReview applies a finished review turn's outcome. Apply already ran in runTurn.
func (h *Hub) finishReview(rs *roomState, part *Part, msg *Message, next *ReviewStep, outcome string) {
	if part.State != "reviewing" {
		return // abandoned while the turn ran
	}
	defer rs.broadcast("work", rs.room.Work)
	if msg.Status != "done" {
		h.blockPart(rs, part, "review", "reviewing", "리뷰 턴 실패: "+msg.Error, nil)
		return
	}
	h.workNote(rs, fmt.Sprintf("파트 %s 리뷰 %s 반영 · 미해결 %d건", part.ID, msg.ReviewStep, part.Review.Summary.Unresolved))
	switch outcome {
	case "":
		if next == nil {
			h.blockPart(rs, part, "review", "reviewing", "리뷰가 다음 단계를 주지 않음", nil)
			return
		}
		h.runReviewStep(rs, part, *next)
	case "fix":
		part.State = "fixing"
		rs.enqueue(&Turn{ID: newID(), Agent: part.Seat, Reason: "part", Part: part.ID, Cap: rs.room.Seq, Created: nowMs(), Status: "queued", prompt: BuildFixPrompt(part)})
	case "passed":
		h.queuePart(rs, part)
	case "disputed":
		h.blockPart(rs, part, "review", "fixing", "반박 항목을 리뷰어가 유지해 이견이 남음", nil)
	case "limit":
		h.blockPart(rs, part, "review", "fixing", fmt.Sprintf("리뷰 라운드 상한(%d)을 넘김", part.Review.MaxRounds), nil)
	default:
		h.blockPart(rs, part, "review", "reviewing", "알 수 없는 리뷰 결과: "+outcome, nil)
	}
}

// reviewTurnLost blocks a part whose pending review turn was dropped before running.
func (h *Hub) reviewTurnLost(rs *roomState, t *Turn, why string) {
	part := rs.room.Work.part(t.Part)
	if part.State != "reviewing" {
		return
	}
	h.blockPart(rs, part, "review", "reviewing", why, nil)
	_ = h.saveWork(rs)
}

// ---- merge queue (§5) ---------------------------------------------------------

func (h *Hub) queuePart(rs *roomState, part *Part) {
	plan := rs.room.Work
	if s := part.Review.Summary; !s.Passed || s.Unresolved != 0 {
		// D4: any unresolved item keeps the part out of the queue.
		h.blockPart(rs, part, "review", "fixing", fmt.Sprintf("리뷰 통과 신호와 요약이 맞지 않음(미해결 %d건)", s.Unresolved), nil)
		return
	}
	part.State, part.ReadyMode, part.Approval = "queued", plan.MergeMode, nil
	plan.Queue = append(plan.Queue, part.ID)
	h.workNote(rs, fmt.Sprintf("파트 %s 리뷰 통과 · 병합 대기열 %d번째 · %s", part.ID, len(plan.Queue), part.ReadyMode))
	h.advanceQueue(rs)
}

// advanceQueue starts the head part's merge when D4–D6 allow it. Caller holds h.mu.
func (h *Hub) advanceQueue(rs *roomState) {
	plan := rs.room.Work
	if h.closing || plan == nil || plan.Halted != nil || plan.Merging != nil || len(plan.Queue) == 0 {
		return
	}
	part := plan.part(plan.Queue[0])
	if part.State != "queued" {
		return
	}
	if part.ReadyMode == "confirm" && !approvalValid(plan, part) {
		part.State, part.Approval = "awaiting_approval", nil
		h.workNote(rs, fmt.Sprintf("파트 %s 병합 승인 대기 · 제출 %s · 통합 %s", part.ID, shortSHA(part.SubmitSHA), shortSHA(plan.IntegrationSHA)))
		return
	}
	part.State = "merging"
	h.turnWG.Add(1)
	go h.mergePart(rs, part, part.SubmitSHA, plan.IntegrationSHA, plan.Integration, filepath.Join(h.workRoot, rs.room.ID, "integration"), plan.Verify)
}

func (h *Hub) mergePart(rs *roomState, part *Part, submitSHA, beforeSHA, branch, dir string, verify []VerifyCmd) {
	defer h.turnWG.Done()
	message := fmt.Sprintf("merge: [work] 파트 %s 병합", part.ID)
	mergeSHA, block := mergeAndVerify(dir, message, submitSHA, beforeSHA, verify)
	h.mu.Lock()
	if rs.deleted {
		h.mu.Unlock()
		return
	}
	plan := rs.room.Work
	if block == nil {
		plan.Merging = &MergeIntent{PartID: part.ID, SubmitSHA: submitSHA, BeforeSHA: beforeSHA, MergeSHA: mergeSHA}
		if err := h.store.SaveRoom(rs.room); err != nil {
			plan.Merging = nil
			block = &Block{Kind: "worktree", Message: "병합 기록을 저장하지 못함: " + err.Error()}
		}
	}
	if block != nil {
		h.blockPart(rs, part, block.Kind, "queued", block.Message, block.Result)
		_ = h.saveWork(rs)
		h.mu.Unlock()
		return
	}
	rs.broadcast("work", plan)
	h.mu.Unlock()

	_, casErr := runGit(dir, "update-ref", "refs/heads/"+branch, mergeSHA, beforeSHA)
	h.mu.Lock()
	defer h.mu.Unlock()
	if rs.deleted {
		return
	}
	plan.Merging = nil
	if casErr != nil {
		h.blockPart(rs, part, "conflict", "queued", "통합 branch가 병합 전 값이 아니라 갱신하지 못함: "+casErr.Error(), nil)
	} else {
		h.completeMerge(rs, part, mergeSHA)
		h.advanceQueue(rs)
	}
	h.pump(rs)
	_ = h.saveWork(rs)
}

func (h *Hub) completeMerge(rs *roomState, part *Part, mergeSHA string) {
	plan := rs.room.Work
	part.State, part.MergedSHA = "merged", mergeSHA
	plan.IntegrationSHA = mergeSHA
	plan.Queue = slices.DeleteFunc(plan.Queue, func(id string) bool { return id == part.ID })
	h.workNote(rs, fmt.Sprintf("파트 %s 병합 완료 · %s → %s", part.ID, plan.Integration, shortSHA(mergeSHA)))
}

// mergeAndVerify makes the --no-ff merge commit on a detached integration
// worktree and runs verify there. It never moves the integration branch.
func mergeAndVerify(dir, message, submitSHA, beforeSHA string, verify []VerifyCmd) (string, *Block) {
	if _, err := runGit(dir, "checkout", "--quiet", "--detach", beforeSHA); err != nil {
		return "", &Block{Kind: "worktree", Message: "통합 worktree를 준비하지 못함: " + err.Error()}
	}
	argv := []string{"git", "merge", "--no-ff", "--no-edit", "-m", message, submitSHA}
	if _, err := runGit(dir, argv[1:]...); err != nil {
		out, diffErr := runGit(dir, "diff", "--name-only", "-z", "--diff-filter=U")
		var abortErr error
		if _, headErr := runGit(dir, "rev-parse", "-q", "--verify", "MERGE_HEAD"); headErr == nil {
			_, abortErr = runGit(dir, "merge", "--abort")
		}
		conflicts := strings.FieldsFunc(out, func(r rune) bool { return r == 0 })
		kind := "conflict"
		if len(conflicts) == 0 {
			kind = "worktree"
		}
		result := &CommandResult{Name: "merge", Argv: argv, ExitCode: exitCode(err), Output: tailText(err.Error()), Conflicts: conflicts}
		return "", &Block{Kind: kind, Message: errors.Join(fmt.Errorf("병합 실패"), diffErr, abortErr).Error(), Result: result}
	}
	mergeSHA, err := gitLine(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", &Block{Kind: "worktree", Message: err.Error()}
	}
	for _, v := range verify {
		if result, ok := runVerify(dir, v); !ok {
			return "", &Block{Kind: "verify", Message: fmt.Sprintf("검증 %s 실패", v.Name), Result: result}
		}
	}
	return mergeSHA, nil
}

func runVerify(dir string, v VerifyCmd) (*CommandResult, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(v.TimeoutSec)*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, v.Argv[0], v.Argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 5 * time.Second // grandchildren holding the pipe must not outlive the timeout
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	output := out.String()
	if err != nil {
		output += "\n" + err.Error()
	}
	result := &CommandResult{Name: v.Name, Argv: v.Argv, ExitCode: exitCode(err), TimedOut: errors.Is(ctx.Err(), context.DeadlineExceeded), Output: tailText(output)}
	return result, err == nil
}

func exitCode(err error) int {
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	return -1
}

func tailText(s string) string {
	if len(s) <= outputTailBytes {
		return s
	}
	cut := len(s) - outputTailBytes
	for cut < len(s) && !utf8.RuneStart(s[cut]) {
		cut++
	}
	return s[cut:]
}

// ---- restart recovery (§5) ----------------------------------------------------

// recoverWork settles states whose in-memory work died with the previous process.
func (h *Hub) recoverWork(rs *roomState) error {
	plan := rs.room.Work
	for _, part := range plan.Parts {
		switch {
		case part.State == "reviewing":
			h.blockPart(rs, part, "restart", "reviewing", "서버 재시작으로 진행 중이던 리뷰 턴이 사라짐", nil)
		case part.State == "merging" && (plan.Merging == nil || plan.Merging.PartID != part.ID):
			h.blockPart(rs, part, "restart", "queued", "병합 커밋을 기록하기 전에 서버가 재시작됨", nil)
		}
	}
	if m := plan.Merging; m != nil {
		part := plan.part(m.PartID)
		if part == nil {
			return fmt.Errorf("방 %s: 병합 기록의 파트 없음: %s", rs.room.ID, m.PartID)
		}
		tip, err := gitLine(rs.room.Workspace, "rev-parse", "--verify", "refs/heads/"+plan.Integration)
		switch {
		case err != nil:
			h.blockPart(rs, part, "restart", "queued", "재시작 후 통합 branch를 읽지 못함: "+err.Error(), nil)
		case tip == m.MergeSHA:
			h.completeMerge(rs, part, m.MergeSHA)
		case tip == m.BeforeSHA:
			h.blockPart(rs, part, "restart", "queued", fmt.Sprintf("병합 커밋 %s를 만들었지만 통합 branch를 옮기기 전에 서버가 재시작됨", shortSHA(m.MergeSHA)), nil)
		default:
			h.blockPart(rs, part, "restart", "queued", fmt.Sprintf("통합 branch가 병합 전(%s)도 병합 후(%s)도 아닌 %s를 가리킴", shortSHA(m.BeforeSHA), shortSHA(m.MergeSHA), shortSHA(tip)), nil)
		}
		plan.Merging = nil
	}
	return h.store.SaveRoom(rs.room)
}

// ---- WORK:DONE ----------------------------------------------------------------

// submitAsync runs the submit checks for a WORK:DONE reply outside the turn's lock.
func (h *Hub) submitAsync(rs *roomState, partID string) {
	part := rs.room.Work.part(partID)
	if err := h.reserveSubmit(rs, part); err != nil {
		h.workNote(rs, fmt.Sprintf("파트 %s의 %s 제출을 처리하지 못함: %v", partID, workDoneSignal, err))
		return
	}
	h.turnWG.Add(1)
	go func() {
		defer h.turnWG.Done()
		if _, err := h.submitReserved(rs, part); err != nil {
			h.mu.Lock()
			defer h.mu.Unlock()
			if !rs.deleted {
				h.workNote(rs, fmt.Sprintf("파트 %s의 %s 제출을 처리하지 못함: %v", partID, workDoneSignal, err))
			}
		}
	}()
}

// ---- diff (D12) ---------------------------------------------------------------

func partDiff(repo, partID, base, head string) (*PartDiff, error) {
	names, err := runGit(repo, "diff", "--name-status", "-z", "-M", base, head)
	if err != nil {
		return nil, err
	}
	patch, err := runGit(repo, "diff", "-M", "--no-color", "--no-ext-diff", base, head)
	if err != nil {
		return nil, err
	}
	files, err := parseNameStatus(names)
	if err != nil {
		return nil, err
	}
	sections := splitPatch(patch)
	if len(sections) != len(files) {
		return nil, fmt.Errorf("diff 파일 수(%d)와 patch 수(%d)가 다름", len(files), len(sections))
	}
	out := &PartDiff{PartID: partID, BaseSHA: base, HeadSHA: head, Files: files}
	size := 0
	for i, section := range sections {
		f := &out.Files[i]
		f.Added, f.Deleted = countPatchLines(section)
		out.Added += f.Added
		out.Deleted += f.Deleted
		size += len(section)
		if size > diffPatchLimit {
			out.Truncated = true
			continue
		}
		f.Patch = section
	}
	return out, nil
}

func parseNameStatus(raw string) ([]DiffFile, error) {
	tokens := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	files := []DiffFile{}
	for i := 0; i < len(tokens) && tokens[i] != ""; {
		status := tokens[i][:1]
		switch status {
		case "R":
			if i+2 >= len(tokens) {
				return nil, fmt.Errorf("이름 변경 diff 항목이 잘림")
			}
			files = append(files, DiffFile{Path: tokens[i+2], Status: status})
			i += 3
		case "A", "M", "D":
			if i+1 >= len(tokens) {
				return nil, fmt.Errorf("diff 항목이 잘림")
			}
			files = append(files, DiffFile{Path: tokens[i+1], Status: status})
			i += 2
		default:
			return nil, fmt.Errorf("지원하지 않는 diff 상태: %s", tokens[i])
		}
	}
	return files, nil
}

// splitPatch cuts a full patch at each "diff --git" header; content lines are
// always prefixed, so a header can only start a file section.
func splitPatch(patch string) []string {
	var sections []string
	start := -1
	for i := 0; i < len(patch); {
		if strings.HasPrefix(patch[i:], "diff --git ") {
			if start >= 0 {
				sections = append(sections, patch[start:i])
			}
			start = i
		}
		next := strings.IndexByte(patch[i:], '\n')
		if next < 0 {
			break
		}
		i += next + 1
	}
	if start >= 0 {
		sections = append(sections, patch[start:])
	}
	return sections
}

func countPatchLines(section string) (added, deleted int) {
	inHunk := false
	for line := range strings.Lines(section) {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case !inHunk:
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			deleted++
		}
	}
	return added, deleted
}

// ---- git ----------------------------------------------------------------------

func runGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func gitLine(dir string, args ...string) (string, error) {
	out, err := runGit(dir, args...)
	return strings.TrimSpace(out), err
}

func branchExists(repo, branch string) (bool, error) {
	_, err := runGit(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
