package room

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"roundtable/internal/agent"
)

type Request struct {
	WaitReason string `json:"wait_reason,omitempty"` // derived from the recipient's current part
	ID         string `json:"id"`
	From       string `json:"from"`
	To         string `json:"to"`
	Text       string `json:"text"`
	SourceMsg  string `json:"source_msg"`
	Status     string `json:"status"`
	ResultMsg  string `json:"result_msg,omitempty"`
	Reason     string `json:"reason,omitempty"`
	At         int64  `json:"at"`
}

func (s *Store) appendRequest(roomID string, r Request) error {
	return s.appendLine(filepath.Join(s.roomDir(roomID), "requests.jsonl"), r)
}

func (s *Store) loadRequests(roomID string) ([]Request, error) {
	out := []Request{}
	indices := map[string]int{}
	err := scanJSONLines(filepath.Join(s.roomDir(roomID), "requests.jsonl"), func(b []byte) error {
		var r Request
		if err := json.Unmarshal(b, &r); err != nil {
			return err
		}
		if i, ok := indices[r.ID]; ok {
			out[i] = r
		} else {
			indices[r.ID] = len(out)
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

func (rs *roomState) request(id string) *Request {
	for i := range rs.requests {
		if rs.requests[i].ID == id {
			return &rs.requests[i]
		}
	}
	return nil
}

func (h *Hub) saveRequest(rs *roomState, r Request) error {
	if err := h.store.appendRequest(rs.room.ID, r); err != nil {
		return err
	}
	if old := rs.request(r.ID); old != nil {
		*old = r
	} else {
		rs.requests = append(rs.requests, r)
	}
	rs.broadcast("request", rs.requestView(r))
	return nil
}

func (h *Hub) CancelRequest(roomID, id string) (Request, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.rooms[roomID]
	if rs == nil {
		return Request{}, fmt.Errorf("방 없음")
	}
	r := rs.request(id)
	if r == nil {
		return Request{}, fmt.Errorf("요청 없음: %s", id)
	}
	if r.Status != "pending" && r.Status != "in_progress" {
		return Request{}, fmt.Errorf("이미 종료된 요청: %s", r.Status)
	}
	next := *r
	next.Status = "cancelled"
	if err := h.saveRequest(rs, next); err != nil {
		return Request{}, err
	}
	// A queued request-only invocation has no work left after its last request
	// is cancelled. Preserve an explicit ordinary mention in the same message.
	keepTurn := false
	for _, other := range rs.requests {
		if other.SourceMsg == next.SourceMsg && other.To == next.To && other.Status == "pending" {
			keepTurn = true
		}
	}
	for _, m := range rs.msgs {
		if m.ID == next.SourceMsg && slices.Contains(ParseParticipantMentions(ordinaryMentionText(m.Text), rs.room.Participants), next.To) {
			keepTurn = true
		}
	}
	if !keepTurn {
		rs.queue = slices.DeleteFunc(rs.queue, func(t *Turn) bool { return t.Group == next.SourceMsg && t.Agent == next.To && t.Reason == "mention" })
		rs.refreshParticipantStates()
		rs.broadcast("queue", rs.queueView())
	}
	return rs.requestView(next), nil
}

func (rs *roomState) requestView(r Request) Request {
	r.WaitReason = ""
	if r.Status == "pending" {
		if part := rs.seatPart(r.To); part != nil {
			r.WaitReason = fmt.Sprintf("파트 %s 종료 대기 (%s)", part.ID, part.State)
		}
	}
	return r
}

func (rs *roomState) requestViews() []Request {
	out := make([]Request, 0, len(rs.requests))
	for _, r := range rs.requests {
		out = append(out, rs.requestView(r))
	}
	return out
}

// Request-only queue entries wait for the room session to become available.
// An ordinary explicit mention in the same message keeps its existing routing.
func (rs *roomState) deferredRequestTurn(t *Turn) bool {
	if t.Reason != "mention" || rs.seatPart(t.Agent) == nil {
		return false
	}
	for _, m := range rs.msgs {
		if m.ID != t.Group {
			continue
		}
		if slices.Contains(ParseParticipantMentions(ordinaryMentionText(m.Text), rs.room.Participants), t.Agent) {
			return false
		}
		for _, id := range m.Requests {
			if r := rs.request(id); r != nil && r.To == t.Agent {
				return true
			}
		}
	}
	return false
}

func (rs *roomState) isRequestSource(m *Message, to string) bool {
	for _, id := range m.Requests {
		if r := rs.request(id); r != nil && r.To == to {
			return true
		}
	}
	return false
}

func (rs *roomState) pendingRequests(to string, cap int64) []Request {
	if rs.seatPart(to) != nil {
		return nil
	}
	var out []Request
	for _, r := range rs.requests {
		if r.To != to || r.Status != "pending" {
			continue
		}
		for _, m := range rs.msgs {
			if m.ID == r.SourceMsg && m.Seq <= cap {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

func (h *Hub) beginRequests(rs *roomState, pr *prepared) {
	if pr.part != nil || pr.bootstrap || pr.t.Reason == "review" {
		return
	}
	cap := pr.t.Cap
	if pr.t.Reason == "discussion" {
		cap = rs.room.Seq
	}
	for _, r := range rs.pendingRequests(pr.p.ID, cap) {
		r.Status = "in_progress"
		if err := h.saveRequest(rs, r); err != nil {
			pr.inputErr = err
			return
		}
		pr.requests = append(pr.requests, r.ID)
		pr.opts.Prompt += fmt.Sprintf("\n[요청 %s from @%s: %s] 응답 끝 신호 블록에 한 줄씩: [[REQUEST_DONE:%s]] 또는 [[REQUEST_REJECT:%s]] 이유", r.ID, r.From, r.Text, r.ID, r.ID)
	}
}

var attachMarker = regexp.MustCompile(`^\[\[ATTACH:(.+)\]\]$`)
var requestMarker = regexp.MustCompile(`^\[\[REQUEST:@([^\]\s]+)\]\]\s+(.+)$`)
var resultMarker = regexp.MustCompile(`^\[\[REQUEST_(DONE|REJECT):([^\]\s]+)\]\](?:\s+(.*))?$`)

func (h *Hub) markerError(rs *roomState, pr *prepared, marker, reason string) error {
	m := rs.nextMessage("system", "system", fmt.Sprintf("@%s 마커 거절 (%s): %s", pr.p.ID, marker, reason))
	m.NoticeFor, m.Part = pr.p.ID, pr.t.Part
	if err := h.store.AppendMessage(rs.room.ID, m); err != nil {
		return err
	}
	rs.broadcast("message", m)
	return nil
}

// Markers never run for an errored or interrupted response. Pending requests
// claimed by this turn return to pending unless a valid terminal signal wins.
func (h *Hub) finishMarkers(rs *roomState, pr *prepared, raw string) error {
	msg := pr.msg
	var processingErr error
	reject := func(marker, reason string) {
		if err := h.markerError(rs, pr, marker, reason); err != nil {
			processingErr = err
		}
	}
	if msg.Status == "done" {
		lines, signalStart := trailingSignals(raw)
		for i, rawLine := range lines {
			line := strings.TrimSpace(rawLine)
			switch {
			case strings.HasPrefix(line, "[[ATTACH"):
				if pr.opts.Mode == agent.ModeChat {
					reject(line, "chat 모드는 파일 첨부를 처리하지 않음")
					continue
				}
				m := attachMarker.FindStringSubmatch(line)
				if m == nil {
					reject(line, "[[ATTACH:경로]] 한 줄 형식 필요")
					continue
				}
				if len(msg.Attachments) >= MaxAttachments {
					reject(line, "메시지당 최대 4장")
					continue
				}
				f, err := attachmentSource(m[1], pr.opts.Workspace, pr.opts.ScratchDir)
				if err != nil {
					reject(line, err.Error())
					continue
				}
				a, err := h.store.saveAttachment(rs.room.ID, filepath.Base(m[1]), pr.p.ID, f)
				f.Close()
				if err != nil {
					reject(line, err.Error())
					continue
				}
				msg.Attachments = append(msg.Attachments, a)
			case strings.HasPrefix(line, "[[REQUEST:"):
				if pr.t.Reason == "discussion" || pr.t.Reason == "review" {
					reject(line, "discussion·review 턴에서는 새 요청을 만들 수 없음")
					continue
				}
				m := requestMarker.FindStringSubmatch(line)
				if m == nil || strings.TrimSpace(m[2]) == "" {
					reject(line, "[[REQUEST:@좌석ID]] 요청 내용 한 줄 형식 필요")
					continue
				}
				to := rs.room.participant(m[1])
				if to == nil || !to.Enabled || to.Removed || to.ID == pr.p.ID {
					reject(line, "다른 활성 좌석 ID가 필요함")
					continue
				}
				r := Request{ID: newID(), From: pr.p.ID, To: to.ID, Text: strings.TrimSpace(m[2]), SourceMsg: msg.ID, Status: "pending", At: nowMs()}
				if err := h.saveRequest(rs, r); err != nil {
					processingErr = err
					continue
				}
				msg.Requests = append(msg.Requests, r.ID)
			case strings.HasPrefix(line, "[[REQUEST_DONE") || strings.HasPrefix(line, "[[REQUEST_REJECT"):
				m := resultMarker.FindStringSubmatch(line)
				if m == nil || i < signalStart {
					reject(line, "요청 결과 신호는 응답 끝의 연속된 신호 줄 블록에 써야 함")
					continue
				}
				r := rs.request(m[2])
				if r == nil || r.To != pr.p.ID || r.Status != "in_progress" || !slices.Contains(pr.requests, r.ID) {
					reject(line, "이 턴에서 받은 진행 중 요청 ID가 아님")
					continue
				}
				if m[1] == "REJECT" && strings.TrimSpace(m[3]) == "" {
					reject(line, "거절 이유가 필요함")
					continue
				}
				if m[1] == "DONE" && m[3] != "" {
					reject(line, "완료 신호 뒤에는 내용을 쓰지 마세요")
					continue
				}
				next := *r
				next.Status, next.ResultMsg = "done", msg.ID
				if m[1] == "REJECT" {
					next.Status, next.Reason = "rejected", strings.TrimSpace(m[3])
				}
				if err := h.saveRequest(rs, next); err != nil {
					processingErr = err
				}
			case strings.HasPrefix(line, "[[REQUEST"):
				reject(line, "요청 마커 형식 오류: [[REQUEST:@좌석ID]] 내용 또는 [[REQUEST_DONE:요청ID]] / [[REQUEST_REJECT:요청ID]] 이유")
			}
		}
	}
	for _, id := range pr.requests {
		if r := rs.request(id); r != nil && r.Status == "in_progress" {
			next := *r
			next.Status = "pending"
			if err := h.saveRequest(rs, next); err != nil {
				processingErr = err
			}
		}
	}
	return processingErr
}

func ordinaryMentionText(text string) string {
	var lines []string
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "[[REQUEST") && !strings.HasPrefix(trimmed, "[[ATTACH") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func markerBrief(mode string) string {
	text := "- 참가자에게 요청: [[REQUEST:@좌석ID]] 확인할 내용 한 줄 (discussion·review 턴에서는 금지)\n- 받은 요청 완료(응답 끝 신호 블록): [[REQUEST_DONE:요청ID]]\n- 받은 요청 거절(응답 끝 신호 블록): [[REQUEST_REJECT:요청ID]] 거절 이유\n- 여러 요청 결과·WORK:DONE·[[DISCUSSION:AGREE]]는 응답 끝에 빈 줄 없이 한 줄씩 모으세요. 블록 안의 순서는 자유입니다.\n- 파트를 맡은 좌석의 받은 요청은 파트 종료까지 pending으로 대기하며 파트 세션에 전달하지 않습니다.\n"
	if mode != agent.ModeChat {
		text += "- 기존 이미지 공유(한 줄): [[ATTACH:이미지.png]] — 작업 폴더 또는 서버 scratch 안의 PNG·JPEG·WEBP, 파일당 10MB, 메시지당 4장.\n"
		if mode == agent.ModeWrite {
			text += "- 직접 화면을 캡처할 수 있습니다. 저장 후 예: [[ATTACH:캡처.png]]\n"
		} else {
			text += "- read 모드에서는 이미 있는 이미지만 공유하세요. 직접 캡처하거나 파일을 만들지 마세요.\n"
		}
	}
	return text
}
