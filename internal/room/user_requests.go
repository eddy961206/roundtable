package room

import (
	"fmt"
	"strings"
)

// UserRequest is a durable request to the person, independent of Ask's
// next-reply lifetime and Request's seat-to-seat scheduling. Its source is
// the existing user_request notice; a resolution is another stored message.
type UserRequest struct {
	ID          string `json:"id"` // notice message ID
	Seat        string `json:"seat"`
	Seq         int64  `json:"seq"` // notice seq; rt wait's receipt cursor
	SourceMsg   string `json:"source_msg"`
	Part        string `json:"part,omitempty"`
	Text        string `json:"text"`
	Status      string `json:"status"` // pending | answered | acknowledged
	At          int64  `json:"at"`
	Answer      string `json:"answer,omitempty"`
	ResolvedSeq int64  `json:"resolved_seq,omitempty"`
	ResolvedAt  int64  `json:"resolved_at,omitempty"`
}

// UserRequestReply is stored WITH the answer/acknowledgement, not in a later
// write. An empty Answer is an acknowledgement that does not call the seat.
type UserRequestReply struct {
	ID     string `json:"id"`
	Answer string `json:"answer,omitempty"`
}

func (rs *roomState) userRequest(id string) *UserRequest {
	for i := range rs.userRequests {
		if rs.userRequests[i].ID == id {
			return &rs.userRequests[i]
		}
	}
	return nil
}

// applyUserRequestMessage is used after a successful append and on startup.
// Old notices already carry notice_for/notice_msg/part; they become pending
// requests without rewriting the room or guessing whether prose answered them.
func (rs *roomState) applyUserRequestMessage(m *Message) {
	if m.NoticeMarker == "user_request" {
		rs.userRequests = append(rs.userRequests, UserRequest{
			ID: m.ID, Seat: m.NoticeFor, Seq: m.Seq, SourceMsg: m.NoticeMsg,
			Part: m.Part, At: m.TS, Status: "pending",
			Text: strings.TrimPrefix(m.Text, "@"+m.NoticeFor+" 사용자 확인 요청: "),
		})
	}
	if reply := m.UserRequestReply; reply != nil {
		r := rs.userRequest(reply.ID)
		r.Status, r.Answer, r.ResolvedSeq, r.ResolvedAt = "answered", reply.Answer, m.Seq, m.TS
		if reply.Answer == "" {
			r.Status = "acknowledged"
		}
	}
}

// ResolveUserRequest either sends an answer to the original seat or records
// an acknowledgement without starting a turn. Both are one message append:
// after restart the same message restores the resolution, even if a later
// room metadata save failed. Caller-visible errors before append leave it open.
func (h *Hub) ResolveUserRequest(roomID, id, text string, acknowledge bool) (*Message, UserRequest, error) {
	text = strings.TrimSpace(text)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, UserRequest{}, fmt.Errorf("서버가 종료 중입니다")
	}
	rs := h.rooms[roomID]
	if rs == nil {
		return nil, UserRequest{}, fmt.Errorf("방이 없습니다: %s", roomID)
	}
	r := rs.userRequest(id)
	if r == nil {
		return nil, UserRequest{}, fmt.Errorf("사용자 요청이 없습니다: %s", id)
	}
	if r.Status != "pending" {
		return nil, UserRequest{}, fmt.Errorf("이미 처리된 사용자 요청입니다: %s (메시지 seq %d)", r.Status, r.ResolvedSeq)
	}
	if acknowledge == (text != "") {
		return nil, UserRequest{}, fmt.Errorf("text와 acknowledge:true 중 하나만 지정하세요")
	}
	reply := &UserRequestReply{ID: id, Answer: text}
	if !acknowledge {
		m, err := h.acceptUser(rs, fmt.Sprintf("사용자 요청 「%s」에 대한 답: %s", r.Text, text), r.Seat, nil, nil, nil, reply, "")
		if err != nil {
			return nil, UserRequest{}, err
		}
		return m, *rs.userRequest(id), nil
	}
	m := rs.nextMessage("system", "system", fmt.Sprintf("@%s 사용자 요청을 확인했습니다: %s", r.Seat, r.Text))
	m.UserRequestReply = reply
	if err := h.store.AppendMessage(roomID, m); err != nil {
		rs.msgs = rs.msgs[:len(rs.msgs)-1]
		return nil, UserRequest{}, h.reportPersistenceError(rs, "사용자 요청 확인", err)
	}
	rs.applyUserRequestMessage(m)
	rs.broadcast("message", m)
	rs.broadcast("user_request", *rs.userRequest(id))
	h.saveRoom(rs)
	return m, *rs.userRequest(id), nil
}
