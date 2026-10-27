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

const handoffRequest = "이 세션을 정리하고 새 세션으로 넘기려 해. 다음 세션이 이 대화 없이 이어서 일할 수 있게 인계 요약을 써 줘. " +
	"들어갈 것: 확인한 동작과 근거 위치(파일:줄·커밋), 결정한 것, 남은 위험·미해결, 진행 중인 작업 상태(브랜치·커밋·바꾼 파일), 다음에 할 일. " +
	"새로 조사하지 말고 지금까지 확인한 것으로 쓰고, 확인하지 못한 것은 그렇다고 적어. 원문을 되풀이하지 말고 2,000자 이내로."

// Handoff asks seat for a handoff summary; when that turn is done the seat's
// next turn starts a new session. The request is a user message addressed to
// the seat; the returned copy carries its seq for waiting.
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
	switch {
	case p == nil || p.Removed || !p.Enabled:
		return nil, fmt.Errorf("활성 좌석 없음: %s", seat)
	case rs.runningFor(seat) != nil:
		return nil, fmt.Errorf("@%s 턴이 실행 중 · 그 턴이 끝난 뒤 인계해(rt wait)", seat)
	case p.SessionID == "":
		return nil, fmt.Errorf("@%s는 이어 쓰는 세션이 없어 인계할 것이 없음 · 다음 턴은 이미 새 세션으로 시작함", seat)
	case rs.activePart(seat) != nil:
		return nil, fmt.Errorf("@%s는 작업 계획의 파트를 맡고 있어 파트 세션을 따로 씀 · 파트가 끝난 뒤 인계해", seat)
	}
	if other := h.workspaceRival(rs); other != nil {
		return nil, rivalError(other)
	}
	text := handoffRequest
	if note != "" {
		text += "\n추가로 담을 것: " + note
	}
	m := rs.nextMessage("user", "user", text)
	m.Target = seat
	none := []string{}
	m.Mentions = &none
	if err := h.store.AppendMessage(roomID, m); err != nil {
		rs.msgs = rs.msgs[:len(rs.msgs)-1]
		return nil, h.reportPersistenceError(rs, "인계 요청", err)
	}
	rs.broadcast("message", m)
	h.saveRoom(rs)
	rs.enqueueFront(&Turn{ID: newID(), Agent: seat, Reason: "handoff", Group: m.ID, Cap: m.Seq, Fresh: fresh, Created: nowMs(), Status: "queued"})
	h.pump(rs)
	mc := *m
	return &mc, nil
}

// finishHandoff switches the seat to a new session once its summary is done;
// a failed or empty summary keeps the old session. Caller holds h.mu.
func (h *Hub) finishHandoff(rs *roomState, p *Participant, msg *Message, fresh bool) {
	if msg.Status != "done" || strings.TrimSpace(msg.Text) == "" {
		h.addSystem(rs, fmt.Sprintf("@%s 인계 요약 #%d이 끝나지 않아 세션을 그대로 둠 · 다시 인계하거나 그대로 이어 써", p.ID, msg.Seq))
		return
	}
	p.SessionID, p.ContextTokens, p.Brief, p.Unseen = "", 0, "", nil
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
