package room

import (
	"fmt"
	"strings"

	"roundtable/internal/agent"
)

// Ask is one question a seat asked the person during a reply (agent.KindAsk,
// Codex request_user_input_async). The CLI does not wait for the answer: the
// seat keeps working, and an answer reaches it as a user message addressed to
// it, in the next turn when it is still running.
type Ask struct {
	ID        string              `json:"id"` // the CLI's call id
	TS        int64               `json:"ts"`
	Questions []agent.AskQuestion `json:"questions"`
	Answer    string              `json:"answer,omitempty"`
	AnswerSeq int64               `json:"answer_seq,omitempty"` // user message that carried Answer
	ClosedSeq int64               `json:"closed_seq,omitempty"` // the seat's next reply, begun without an answer
}

// AskView is an ask with the reply that holds it and its status:
// waiting (that reply is still running), open (it ended, no answer yet),
// answered, or closed (the seat began its next reply first, so the question
// passed; a message it got meanwhile may have answered it).
type AskView struct {
	Ask
	Seat   string `json:"seat"`
	Seq    int64  `json:"seq"`
	Status string `json:"status"`
}

func (a Ask) status(m *Message) string {
	switch {
	case a.AnswerSeq > 0:
		return "answered"
	case a.ClosedSeq > 0:
		return "closed"
	case m.Status == "streaming":
		return "waiting"
	}
	return "open"
}

func countOpenAsks(msgs []*Message) int {
	n := 0
	for _, m := range msgs {
		for _, a := range m.Asks {
			if a.AnswerSeq == 0 && a.ClosedSeq == 0 {
				n++
			}
		}
	}
	return n
}

func (rs *roomState) askViews() []AskView {
	out := []AskView{}
	for _, m := range rs.msgs {
		for _, a := range m.Asks {
			out = append(out, AskView{Ask: a, Seat: m.Author, Seq: m.Seq, Status: a.status(m)})
		}
	}
	return out
}

// recordAsk keeps a question on the running reply. Caller holds h.mu.
func (h *Hub) recordAsk(rs *roomState, msg *Message, e agent.Event) error {
	msg.Asks = append(msg.Asks, Ask{ID: e.ID, TS: e.TS, Questions: e.Meta["questions"].([]agent.AskQuestion)})
	rs.openAsks++
	if err := h.store.AppendMessage(rs.room.ID, msg); err != nil {
		return h.reportPersistenceError(rs, "사용자 질문", err)
	}
	rs.broadcast("message", msg)
	return nil
}

// closeAsks closes the seat's unanswered questions when it begins the reply
// next. Caller holds h.mu.
func (h *Hub) closeAsks(rs *roomState, seat string, reply *Message) {
	if rs.openAsks == 0 {
		return
	}
	for _, m := range rs.msgs {
		if m.Author != seat || m == reply || len(m.Asks) == 0 {
			continue
		}
		changed := false
		for i := range m.Asks {
			if m.Asks[i].status(m) == "open" {
				m.Asks[i].ClosedSeq = reply.Seq
				rs.openAsks--
				changed = true
			}
		}
		if !changed {
			continue
		}
		if err := h.store.AppendMessage(rs.room.ID, m); err != nil {
			h.reportPersistenceError(rs, "질문 종료 기록", err)
		}
		rs.broadcast("message", m)
	}
}

// AnswerAsk sends the answer to the seat that asked, as a user message
// addressed to it, and marks the question answered. choice (1-based) picks a
// suggested option of a single question; otherwise text is the answer.
func (h *Hub) AnswerAsk(roomID, askID string, choice int, text string) (*Message, AskView, error) {
	text = strings.TrimSpace(text)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, AskView{}, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[roomID]
	if !ok {
		return nil, AskView{}, fmt.Errorf("방 없음: %s", roomID)
	}
	var asked *Message
	var a *Ask
	for _, m := range rs.msgs {
		for i := range m.Asks {
			if m.Asks[i].ID == askID {
				asked, a = m, &m.Asks[i]
			}
		}
	}
	if a == nil {
		return nil, AskView{}, fmt.Errorf("질문 없음: %s", askID)
	}
	switch a.status(asked) {
	case "answered":
		return nil, AskView{}, fmt.Errorf("이미 답한 질문: 답 메시지 seq %d", a.AnswerSeq)
	case "closed":
		return nil, AskView{}, fmt.Errorf("좌석이 답 없이 다음 답(seq %d)을 시작해 질문이 지나감; 새 지시는 rt post --target %s로 보내", a.ClosedSeq, asked.Author)
	}
	if (choice > 0) == (text != "") {
		return nil, AskView{}, fmt.Errorf("choice와 text 중 하나만 지정해야 함")
	}
	if choice > 0 {
		if len(a.Questions) != 1 || choice > len(a.Questions[0].Options) {
			return nil, AskView{}, fmt.Errorf("choice는 질문이 하나이고 선택지가 있을 때 1~선택지 수만 쓸 수 있음; 그 밖에는 text로 답해")
		}
		text = a.Questions[0].Options[choice-1]
	}
	titles := make([]string, len(a.Questions))
	for i, q := range a.Questions {
		titles[i] = clipRunes(q.Title, 240)
	}
	m, err := h.acceptUser(rs, fmt.Sprintf("질문 「%s」에 대한 사용자 답: %s", strings.Join(titles, " / "), text), asked.Author, nil, nil)
	if err != nil {
		return nil, AskView{}, err
	}
	a.Answer, a.AnswerSeq = text, m.Seq
	rs.openAsks--
	view := AskView{Ask: *a, Seat: asked.Author, Seq: asked.Seq, Status: a.status(asked)}
	if err := h.store.AppendMessage(rs.room.ID, asked); err != nil {
		return m, view, h.reportPersistenceError(rs, fmt.Sprintf("답 메시지 seq %d는 전송됨; 질문 상태", m.Seq), err)
	}
	rs.broadcast("message", asked)
	return m, view, nil
}
