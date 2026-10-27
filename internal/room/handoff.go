package room

import (
	"fmt"
	"strings"
)

// A long-running seat re-reads its whole context on every model call, so the
// cost of a session grows with its length. Handoff has the seat write a summary
// in its current session, then starts its next turn in a new session that holds
// only that summary and later messages (the room keeps the full record). A seat
// started from a base session forks the base again unless fresh is set.
// 2026-10-28 A/B: -43% input on the following review+fix turns with the same
// defects found and fixed (docs/토큰효율_측정_20261028.md).
//
// State rules, kept few on purpose (Argon review of 11effa1):
//   - A handoff starts only on a quiet seat: no running or queued turn, no
//     unfinished part, no discussion, no hold, and not already handed off.
//     Anything else is refused with the reason instead of waiting in the queue.
//   - The request is the turn's own instruction, not a room message, so a
//     failed handoff leaves nothing behind for the next turn.
//   - The handoff turn takes no steer, requests or markers.
//   - HistoryFrom stays set until a later turn is done; until then every turn
//     starts a new session from the summary (and the messages still unseen at
//     the switch). A failed attempt commits no session or cursor, and its own
//     text is not fed back. fresh on a handed-off seat only drops the base.

const handoffRequest = "이번 턴은 인계 요약 턴이야. 이 세션을 정리하고 새 세션으로 넘기려 해. 다음 세션이 이 대화 없이 이어서 일할 수 있게 인계 요약을 써 줘. " +
	"들어갈 것: 확인한 동작과 근거 위치(파일:줄·커밋), 결정한 것, 남은 위험·미해결, 진행 중인 작업 상태(브랜치·커밋·바꾼 파일), 다음에 할 일. " +
	"새로 조사하지 말고 지금까지 확인한 것으로 쓰고, 확인하지 못한 것은 그렇다고 적어. 이 턴의 [[...]] 마커는 처리하지 않으니 쓰지 마. 원문을 되풀이하지 말고 2,000자 이내로."

// Handoff asks seat for a handoff summary; when that turn is done the seat's
// next turn starts a new session. It returns the system notice that announces
// the request; its seq is the after_seq to wait on.
func (h *Hub) Handoff(roomID, seat, note string, fresh bool) (*Message, error) {
	note = strings.TrimSpace(note)
	if strings.ContainsAny(note, "\r\n") || len([]rune(note)) > 300 {
		return nil, fmt.Errorf("인계 메모는 300자 이내 한 줄")
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
	p := rs.room.participant(seat)
	if p == nil || p.Removed || !p.Enabled {
		return nil, fmt.Errorf("활성 좌석 없음: %s", seat)
	}
	if fresh && p.HistoryFrom > 0 && p.Fork != nil && rs.runningFor(seat) == nil {
		// Already handed off, but the base can no longer be (or should not be)
		// forked: drop it; the next turn starts from the same summary.
		p.Fork = nil
		h.saveRoom(rs)
		rs.broadcast("participant", p)
		m, err := h.addSystem(rs, fmt.Sprintf("@%s 기반 포크를 버림 · 다음 턴은 인계 요약 #%d부터 기반 없이 새 세션으로 시작", seat, p.HistoryFrom))
		if err != nil {
			return nil, err
		}
		mc := *m
		return &mc, nil
	}
	if why := rs.handoffBlocked(h, p, fresh); why != "" {
		return nil, fmt.Errorf("@%s 인계 거절: %s", seat, why)
	}
	if other := h.workspaceRival(rs); other != nil {
		return nil, rivalError(other)
	}
	text := handoffRequest
	if note != "" {
		text += "\n추가로 담을 것: " + note
	}
	m, err := h.addSystem(rs, fmt.Sprintf("@%s에게 인계 요약을 요청함 · 요약이 끝나면 다음 턴부터 새 세션을 씀", seat))
	if err != nil {
		return nil, err
	}
	rs.enqueueFront(&Turn{ID: newID(), Agent: seat, Reason: "handoff", Group: m.ID, Cap: m.Seq, Fresh: fresh, Created: nowMs(), Status: "queued", prompt: text})
	h.pump(rs)
	mc := *m
	return &mc, nil
}

// handoffBlocked is why seat cannot hand off right now, "" when it can.
// Caller holds h.mu.
func (rs *roomState) handoffBlocked(h *Hub, p *Participant, fresh bool) string {
	if rs.disc != nil {
		return "토론 중 · 토론이 끝난 뒤 인계해"
	}
	for _, t := range rs.queue {
		if t.Agent == p.ID {
			if t.Status == "running" {
				return "턴이 실행 중 · 그 턴이 끝난 뒤 인계해(rt wait)"
			}
			return "대기 중인 턴이 있음 · 그 턴들이 끝난 뒤 인계해"
		}
	}
	switch {
	case rs.runningFor(p.ID) != nil:
		return "턴이 실행 중 · 그 턴이 끝난 뒤 인계해(rt wait)"
	case p.HistoryFrom > 0:
		return fmt.Sprintf("이미 인계 요약 #%d을 남겨 다음 턴이 새 세션으로 시작함", p.HistoryFrom)
	case p.SessionID == "":
		return "이어 쓰는 세션이 없어 인계할 것이 없음 · 다음 턴은 이미 새 세션으로 시작함"
	case rs.seatPart(p.ID) != nil:
		return "작업 계획의 파트를 맡고 있어 파트 세션을 따로 씀 · 파트가 병합·포기된 뒤 인계해"
	case seatHeld(p):
		return "한도 보류 중 · 보류가 풀린 뒤 인계해"
	}
	if why := rs.budgetHold(p); why != "" {
		return why + " · 상한을 올린 뒤 인계해"
	}
	if p.Fork != nil && !fresh {
		if _, ok := h.rooms[p.Fork.Room]; !ok {
			return fmt.Sprintf("기반 방 %s이 없어 기반을 다시 포크할 수 없음 · fresh:true로 기반 없이 인계해", p.Fork.Room)
		}
	}
	return ""
}

// finishHandoff switches the seat to a new session once its summary is done;
// a failed or empty summary keeps the old session. Caller holds h.mu.
func (h *Hub) finishHandoff(rs *roomState, p *Participant, msg *Message, fresh bool) {
	if msg.Status != "done" || strings.TrimSpace(msg.Text) == "" {
		h.addSystem(rs, fmt.Sprintf("@%s 인계 요약 #%d이 끝나지 않아 세션을 그대로 둠 · 다시 인계하거나 그대로 이어 써", p.ID, msg.Seq))
		return
	}
	// Unseen holes (replies still streaming at the switch) stay: the new
	// session receives them with the summary.
	p.SessionID, p.ContextTokens, p.Brief = "", 0, ""
	p.HistoryFrom, p.ContextWarned = msg.Seq, false
	start := "새 세션"
	switch {
	case p.Fork != nil && fresh:
		p.Fork = nil
	case p.Fork != nil:
		start = "기반 세션 " + p.Fork.Room + "/" + p.Fork.Seat + "의 새 포크"
	}
	h.addSystem(rs, fmt.Sprintf("@%s 인계 요약 #%d을 남기고 세션을 바꿈 · 다음 턴은 %s에서 이 요약과 그 뒤 메시지만 받음(원문은 방 기록에 남음)", p.ID, msg.Seq, start))
	rs.broadcast("participant", p)
}
