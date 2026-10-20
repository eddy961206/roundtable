package room

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"roundtable/internal/agent"
)

type Request struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	To        string `json:"to"`
	Text      string `json:"text"`
	SourceMsg string `json:"source_msg"`
	Status    string `json:"status"`
	ResultMsg string `json:"result_msg,omitempty"`
	Reason    string `json:"reason,omitempty"`
	At        int64  `json:"at"`
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
	rs.broadcast("request", r)
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
		if m.ID == next.SourceMsg && slices.Contains(m.mentionIDs(rs.room.Participants), next.To) {
			keepTurn = true
		}
	}
	if !keepTurn {
		rs.queue = slices.DeleteFunc(rs.queue, func(t *Turn) bool { return t.Group == next.SourceMsg && t.Agent == next.To && t.Reason == "mention" })
		rs.refreshParticipantStates()
		rs.broadcast("queue", rs.queueView())
	}
	return next, nil
}

func (rs *roomState) pendingRequests(to string, cap int64) []Request {
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
	for _, r := range rs.pendingRequests(pr.p.ID, rs.historyCap(pr.t)) {
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

// markerError records which handler rejected the line and the reply it came
// from; the model at that time is the source reply's Model.
func (h *Hub) markerError(rs *roomState, pr *prepared, kind, marker, reason string) error {
	m := rs.nextMessage("system", "system", fmt.Sprintf("@%s 마커 거절 (%s): %s", pr.p.ID, marker, reason))
	m.NoticeFor, m.NoticeMarker, m.NoticeMsg = pr.p.ID, kind, pr.msg.ID
	if pr.part != nil {
		m.Part = pr.part.ID
	}
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
	reject := func(kind, marker, reason string) {
		if err := h.markerError(rs, pr, kind, marker, reason); err != nil {
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
					reject("attach", line, "chat 모드는 파일 첨부를 처리하지 않음")
					continue
				}
				m := attachMarker.FindStringSubmatch(line)
				if m == nil {
					reject("attach", line, "[[ATTACH:경로]] 한 줄 형식 필요")
					continue
				}
				if len(msg.Attachments) >= MaxAttachments {
					reject("attach", line, "메시지당 최대 4장")
					continue
				}
				f, err := attachmentSource(m[1], pr.opts.Workspace, pr.opts.ScratchDir)
				if err != nil {
					reject("attach", line, err.Error())
					continue
				}
				a, err := h.store.saveAttachment(rs.room.ID, filepath.Base(m[1]), pr.p.ID, f)
				f.Close()
				if err != nil {
					reject("attach", line, err.Error())
					continue
				}
				msg.Attachments = append(msg.Attachments, a)
			case strings.HasPrefix(line, "[[REQUEST:"):
				if pr.t.Reason == "discussion" || pr.t.Reason == "review" {
					reject("request", line, "discussion·review 턴에서는 새 요청을 만들 수 없음")
					continue
				}
				m := requestMarker.FindStringSubmatch(line)
				if m == nil || strings.TrimSpace(m[2]) == "" {
					reject("request", line, "[[REQUEST:@좌석ID]] 요청 내용 한 줄 형식 필요")
					continue
				}
				to, why := rs.requestTarget(m[1])
				if to == nil || to.ID == pr.p.ID {
					reject("request", line, why+" · 다른 활성 좌석의 ID나 고유한 이름이 필요함")
					continue
				}
				if part := rs.seatPart(to.ID); part != nil {
					reject("request", line, fmt.Sprintf("@%s은 파트 %s 작업 중이라 요청을 받을 수 없음 · 파트가 끝난 뒤 다시 요청하거나 사용자에게 알려", to.ID, part.ID))
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
					reject("request_result", line, "요청 결과 신호는 응답 끝의 연속된 신호 줄 블록에 써야 함")
					continue
				}
				r := rs.request(m[2])
				if r == nil || r.To != pr.p.ID || r.Status != "in_progress" || !slices.Contains(pr.requests, r.ID) {
					reject("request_result", line, "이 턴에서 받은 진행 중 요청 ID가 아님")
					continue
				}
				if m[1] == "REJECT" && strings.TrimSpace(m[3]) == "" {
					reject("request_result", line, "거절 이유가 필요함")
					continue
				}
				if m[1] == "DONE" && m[3] != "" {
					reject("request_result", line, "완료 신호 뒤에는 내용을 쓰지 마세요")
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
				reject("request", line, "요청 마커 형식 오류: [[REQUEST:@좌석ID]] 내용 또는 [[REQUEST_DONE:요청ID]] / [[REQUEST_REJECT:요청ID]] 이유")
			case strings.HasPrefix(line, "[[DECISION"):
				d, err := parseDecisionMarker(line)
				if err != nil {
					reject("decision", line, err.Error())
					continue
				}
				d.ID, d.By, d.SourceMsg, d.At = newID(), pr.p.ID, msg.ID, nowMs()
				if err := h.saveDecision(rs, d); err != nil {
					processingErr = err
					continue
				}
				msg.Decisions = append(msg.Decisions, d.ID)
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
		if !strings.HasPrefix(trimmed, "[[REQUEST") && !strings.HasPrefix(trimmed, "[[ATTACH") && !strings.HasPrefix(trimmed, "[[DECISION") {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func markerBrief(mode string) string {
	text := "- 참가자에게 요청: [[REQUEST:@좌석ID 또는 @이름]] 확인할 내용 한 줄 (discussion·review 턴에서는 금지)\n- 받은 요청 완료(응답 끝 신호 블록): [[REQUEST_DONE:요청ID]]\n- 받은 요청 거절(응답 끝 신호 블록): [[REQUEST_REJECT:요청ID]] 거절 이유\n- 응답 끝 신호(요청 결과, 토론 신호, 파트 완료 신호)가 여럿이면 빈 줄 없이 한 줄씩 모으세요. 순서는 자유입니다. 이번 턴에 해당하는 신호만 쓰세요.\n- 끝나지 않은 파트를 맡은 좌석에는 요청할 수 없습니다. 파트가 끝난 뒤 다시 요청하거나 사용자에게 알리세요.\n- 결정 기록(한 줄, 결정이 정해졌을 때만):" + decisionMarkerForm + "\n"
	if mode != agent.ModeChat {
		text += "- 기존 이미지 공유(한 줄): [[ATTACH:경로]] — PNG·JPEG·WEBP, 파일당 10MB, 메시지당 4장. 상대 경로는 작업 폴더 기준이고, 서버 scratch의 파일은 절대 경로로 쓰세요.\n"
		if mode == agent.ModeWrite {
			text += "- 직접 화면을 캡처할 수 있습니다. 작업 폴더에 저장 후 예: [[ATTACH:캡처.png]]\n"
		} else {
			text += "- read 모드에서는 이미 있는 이미지만 공유하세요. 직접 캡처하거나 파일을 만들지 마세요.\n"
		}
	}
	return text
}

// requestTarget resolves a request recipient by seat ID, or by a display label
// that names exactly one active seat. why explains a failed lookup.
func (rs *roomState) requestTarget(name string) (*Participant, string) {
	if p := rs.room.participant(name); p != nil {
		if !p.Enabled || p.Removed {
			return nil, "@" + name + "는 비활성 또는 삭제된 좌석"
		}
		return p, ""
	}
	if strings.IndexFunc(name, unicode.IsSpace) >= 0 {
		return nil, "공백이 있는 이름은 요청 대상으로 사용할 수 없음 · @좌석ID를 사용하세요"
	}
	var hits []*Participant
	for _, p := range rs.room.enabled() {
		if strings.EqualFold(p.Label, name) {
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], ""
	case 0:
		return nil, "@" + name + "와 일치하는 활성 좌석 없음"
	}
	ids := make([]string, len(hits))
	for i, p := range hits {
		ids[i] = "@" + p.ID
	}
	return nil, "이름 @" + name + "이 여러 좌석(" + strings.Join(ids, ", ") + ")과 겹침"
}
