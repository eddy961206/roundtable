package room

import (
	"fmt"
	"strings"
	"time"
)

// A turn may run at most the room's turn_timeout_minutes (default 45). Running
// past it ends the turn as an error of kind "turn_timeout", not as a user stop.
const (
	DefaultTurnTimeoutMinutes = 45
	MinTurnTimeoutMinutes     = 5
	MaxTurnTimeoutMinutes     = 720
)

// turnTimeoutUnit is a minute; tests shorten it.
var turnTimeoutUnit = time.Minute

func (r *Room) turnTimeout() time.Duration {
	m := r.TurnTimeoutMinutes
	if m == 0 {
		m = DefaultTurnTimeoutMinutes
	}
	return time.Duration(m) * turnTimeoutUnit
}

func validateTurnTimeout(minutes int) error {
	if minutes != 0 && (minutes < MinTurnTimeoutMinutes || minutes > MaxTurnTimeoutMinutes) {
		return fmt.Errorf("turn_timeout_minutes는 %d~%d분이어야 함(0 또는 생략은 기본 %d분): %d", MinTurnTimeoutMinutes, MaxTurnTimeoutMinutes, DefaultTurnTimeoutMinutes, minutes)
	}
	return nil
}

func timeoutError(r *Room) string {
	return fmt.Sprintf("턴 시간 상한 %d분을 넘어 중단됨 · 방 설정 turn_timeout_minutes(%d~%d분)로 늘릴 수 있음",
		int(r.turnTimeout()/turnTimeoutUnit), MinTurnTimeoutMinutes, MaxTurnTimeoutMinutes)
}

// deadlineNote tells a write-mode turn when it will be cut off. agy cannot be
// steered, and a user line written to its stdin mid-turn waits until the whole
// turn (background commands included) has ended (checked 2026-10-28, agy 1.4.9).
// So a seat busy in a long command hears nothing more; the notice it gets is the
// one it read before starting that command. 2026-10-28 three Argon writers started
// a 27-minute mutation run 16 minutes in and were cut off before committing.
func deadlineNote(started time.Time, limit time.Duration) string {
	margin := min(10*turnTimeoutUnit, limit/4)
	end := started.Add(limit)
	return fmt.Sprintf("\n[턴 시간 상한] 이 턴은 %s(이 PC 시각)에 강제로 끝납니다(시작 %s, 상한 %d분). 그때 커밋하지 않은 변경은 작업 트리에 남고 다음 턴이 다시 확인해야 합니다. "+
		"오래 걸리는 명령(전체 시험·변형 시험 등)은 시작하기 전에 현재 시각을 확인하세요. 그 명령이 %s(상한 %d분 전)를 넘겨 끝날 것 같으면 시작하지 말고, 변경이 있으면 로컬 커밋한 뒤 \"진행 중: 다음 할 일\"로 답을 끝내세요.\n",
		end.Format("15:04"), started.Format("15:04"), int(limit/turnTimeoutUnit), end.Add(-margin).Format("15:04"), int(margin/turnTimeoutUnit))
}

// uncommittedNote reports what a timed-out write turn left uncommitted in dir, so
// the orchestrator sees it in the error without opening the worktree.
func uncommittedNote(dir string) string {
	out, err := runGit(dir, "status", "--porcelain")
	if err != nil {
		return " · 커밋 안 된 변경 확인 실패: " + err.Error()
	}
	if out == "" {
		return " · 커밋 안 된 변경 없음"
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	files := make([]string, 0, 5)
	for _, l := range lines[:min(5, len(lines))] {
		files = append(files, strings.TrimSpace(l[min(3, len(l)):]))
	}
	more := ""
	if len(lines) > 5 {
		more = " 외"
	}
	return fmt.Sprintf(" · 커밋 안 된 변경 %d개(%s%s)", len(lines), strings.Join(files, ", "), more)
}
