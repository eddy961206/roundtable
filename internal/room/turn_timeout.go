package room

import (
	"fmt"
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
