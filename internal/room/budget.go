package room

import (
	"fmt"
	"strconv"
)

// Usage guards of a room (docs/다중에이전트_도구조사_20261027.md, items 2 and 7):
// a notice when a seat's cumulative input passes each multiple of the warning
// level, a notice when a seat's context passes a share of its model window, and
// optional caps that stop new turns of a seat (or of every seat) at a budget.
const (
	DefaultInputWarnTokens    = 10_000_000
	DefaultContextWarnPercent = 70
)

// inputWarn is the warning step in tokens; 0 when off.
func (r *Room) inputWarn() float64 {
	switch {
	case r.InputWarnTokens < 0:
		return 0
	case r.InputWarnTokens == 0:
		return DefaultInputWarnTokens
	}
	return float64(r.InputWarnTokens)
}

// contextWarn is the warning share of the model window in percent; 0 when off.
func (r *Room) contextWarn() int {
	switch {
	case r.ContextWarnPercent < 0:
		return 0
	case r.ContextWarnPercent == 0:
		return DefaultContextWarnPercent
	}
	return r.ContextWarnPercent
}

func validateGuards(r *Room) error {
	if r.InputWarnTokens < -1 {
		return fmt.Errorf("input_warn_tokens는 -1(끔), 0(기본 %d) 또는 양수여야 함", DefaultInputWarnTokens)
	}
	if r.SeatInputBudget < 0 || r.RoomInputBudget < 0 {
		return fmt.Errorf("입력 예산 상한은 0(없음) 또는 양수여야 함")
	}
	if r.ContextWarnPercent < -1 || r.ContextWarnPercent > 99 {
		return fmt.Errorf("context_warn_percent는 -1(끔), 0(기본 %d) 또는 1~99여야 함", DefaultContextWarnPercent)
	}
	return nil
}

// normalizedInput is every input token a reply's model calls processed,
// counted the way RoomUsage counts it (cache reads included for all providers).
func normalizedInput(provider string, usage map[string]any) float64 {
	var t UsageTotals
	t.add(provider, &Message{Usage: usage})
	return t.Input
}

// backfillInput counts InputAll from history for rooms saved before it existed.
func backfillInput(rs *roomState) {
	for _, p := range rs.room.Participants {
		if p.InputAll != 0 {
			continue
		}
		for _, m := range rs.msgs {
			if m.Kind == "agent" && m.Author == p.ID && m.Status != "streaming" {
				p.InputAll += normalizedInput(p.Provider, m.Usage)
			}
		}
	}
}

func (rs *roomState) roomInput() float64 {
	total := 0.0
	for _, p := range rs.room.Participants {
		total += p.InputAll
	}
	return total
}

// usageNotices runs once a reply's usage is final. Caller holds h.mu.
func (h *Hub) usageNotices(rs *roomState, p *Participant) {
	if warn := rs.room.inputWarn(); warn > 0 {
		if level := int(p.InputAll / warn); level > p.InputWarned {
			p.InputWarned = level
			h.addSystem(rs, fmt.Sprintf("@%s 이 방 누적 입력이 %s 토큰을 넘음(%s, 캐시 포함) · 한 세션을 오래 이어 쓰면 턴마다 입력이 커짐: 작업이 바뀌는 지점이면 세션 초기화나 기반 포크로 새 세션을 고려",
				p.ID, comma(float64(level)*warn), comma(p.InputAll)))
		}
	}
	if pct := rs.room.contextWarn(); pct > 0 && p.ContextWindow > 0 {
		ratio := p.ContextTokens * 100 / p.ContextWindow
		switch {
		case ratio >= pct && !p.ContextWarned:
			p.ContextWarned = true
			h.addSystem(rs, fmt.Sprintf("@%s 문맥이 모델 한도의 %d%%(%s/%s 토큰)에 도달함 · 작업이 바뀌는 지점이면 결정·남은 일을 정리하게 한 뒤 세션 초기화나 새 세션(포크)으로 바꾸는 것을 고려",
				p.ID, ratio, comma(float64(p.ContextTokens)), comma(float64(p.ContextWindow))))
		case ratio < pct:
			p.ContextWarned = false // compaction or a new session re-arms the warning
		}
	}
}

// budgetHold tells why the seat may not start a turn now, "" when it may.
func (rs *roomState) budgetHold(p *Participant) string {
	if b := rs.room.SeatInputBudget; b > 0 && p.InputAll >= float64(b) {
		return fmt.Sprintf("좌석 누적 입력 %s 토큰이 좌석 예산 상한 %s에 도달", comma(p.InputAll), comma(float64(b)))
	}
	if b := rs.room.RoomInputBudget; b > 0 {
		if total := rs.roomInput(); total >= float64(b) {
			return fmt.Sprintf("방 누적 입력 %s 토큰이 방 예산 상한 %s에 도달", comma(total), comma(float64(b)))
		}
	}
	return ""
}

// holdTurn drops a turn the budget does not allow. Its messages stay unseen, so
// the seat's next turn after the cap rises carries them. One notice per reason.
func (h *Hub) holdTurn(rs *roomState, p *Participant, why string) {
	if p.BudgetHeld == why {
		return
	}
	p.BudgetHeld = why
	h.addSystem(rs, fmt.Sprintf("@%s 턴 보류: %s · 이 좌석에 온 메시지는 전달하지 않고 남겨 둠. 방 설정(또는 rt_update_room)에서 상한을 올리거나 0으로 지우면 다음 턴에 함께 전달됨", p.ID, why))
}

func comma(v float64) string {
	s := strconv.FormatInt(int64(v), 10)
	neg := s[0] == '-'
	if neg {
		s = s[1:]
	}
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}
