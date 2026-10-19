package room

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Decision is one decision card; see docs/에이전트MCP_블라인드토론_결정카드_계약.md (C).
// Every state change appends one line to decisions.jsonl and the last line per
// id wins on replay. Dropping a card changes its status; nothing is deleted.
type Decision struct {
	ID         string `json:"id"`
	Text       string `json:"text"`
	Evidence   string `json:"evidence"`
	Dissent    string `json:"dissent"`
	Unverified string `json:"unverified"`
	By         string `json:"by"`         // participant id or "user"
	SourceMsg  string `json:"source_msg"` // message that recorded the card; empty for cards added on screen
	Status     string `json:"status"`     // active | superseded | dropped
	At         int64  `json:"at"`         // time of this state line; strictly increases per card
}

const (
	maxBriefDecisions = 10  // active decision texts carried by the standing brief
	maxDecisionText   = 300 // runes; the text goes into every standing brief
	maxDecisionField  = 2000
)

// DecisionInput is a card added or edited by a person. Nil fields stay unchanged on PATCH.
type DecisionInput struct {
	Text       *string `json:"text,omitempty"`
	Evidence   *string `json:"evidence,omitempty"`
	Dissent    *string `json:"dissent,omitempty"`
	Unverified *string `json:"unverified,omitempty"`
	Status     *string `json:"status,omitempty"`
}

var decisionMarker = regexp.MustCompile(`^\[\[DECISION:\s*(.*?)\s*\|\s*근거:\s*(.*?)\s*\|\s*반대:\s*(.*?)\s*\|\s*미확인:\s*(.*?)\s*\]\]$`)

const decisionMarkerForm = "[[DECISION: 결정 한 줄 | 근거: URL·파일·메시지 | 반대: 남은 반대 의견 또는 없음 | 미확인: 확인 못 한 것 또는 없음]]"

// parseDecisionMarker reads one marker line. Every field is required; agents
// write 없음 when there is no dissent or nothing unverified.
func parseDecisionMarker(line string) (Decision, error) {
	m := decisionMarker.FindStringSubmatch(line)
	if m == nil {
		return Decision{}, fmt.Errorf("%s 한 줄 형식 필요", decisionMarkerForm)
	}
	d := Decision{Text: m[1], Evidence: m[2], Dissent: m[3], Unverified: m[4], Status: "active"}
	if d.Text == "" || d.Evidence == "" || d.Dissent == "" || d.Unverified == "" {
		return Decision{}, fmt.Errorf("결정·근거·반대·미확인을 모두 채워야 함 (없으면 '없음')")
	}
	return d, validDecision(d)
}

func validDecision(d Decision) error {
	if d.Text == "" {
		return fmt.Errorf("결정 내용이 비어 있음")
	}
	if utf8.RuneCountInString(d.Text) > maxDecisionText {
		return fmt.Errorf("결정은 %d자 이하 한 줄이어야 함", maxDecisionText)
	}
	for _, field := range []string{d.Text, d.Evidence, d.Dissent, d.Unverified} {
		if strings.ContainsAny(field, "\r\n") {
			return fmt.Errorf("결정 카드의 각 항목은 한 줄이어야 함")
		}
		if utf8.RuneCountInString(field) > maxDecisionField {
			return fmt.Errorf("결정 카드 항목은 %d자 이하여야 함", maxDecisionField)
		}
	}
	switch d.Status {
	case "active", "superseded", "dropped":
		return nil
	}
	return fmt.Errorf("알 수 없는 결정 상태: %s (active·superseded·dropped)", d.Status)
}

func (s *Store) appendDecision(roomID string, d Decision) error {
	return s.appendLine(filepath.Join(s.roomDir(roomID), "decisions.jsonl"), d)
}

func (s *Store) loadDecisions(roomID string) ([]Decision, error) {
	out := []Decision{}
	indices := map[string]int{}
	err := scanJSONLines(filepath.Join(s.roomDir(roomID), "decisions.jsonl"), func(b []byte) error {
		var d Decision
		if err := json.Unmarshal(b, &d); err != nil {
			return err
		}
		if i, ok := indices[d.ID]; ok {
			out[i] = d
		} else {
			indices[d.ID] = len(out)
			out = append(out, d)
		}
		return nil
	})
	return out, err
}

func (rs *roomState) decision(id string) *Decision {
	for i := range rs.decisions {
		if rs.decisions[i].ID == id {
			return &rs.decisions[i]
		}
	}
	return nil
}

// saveDecision persists one state line before changing the live list. Caller holds h.mu.
func (h *Hub) saveDecision(rs *roomState, d Decision) error {
	if err := h.store.appendDecision(rs.room.ID, d); err != nil {
		return err
	}
	if old := rs.decision(d.ID); old != nil {
		*old = d
	} else {
		rs.decisions = append(rs.decisions, d)
	}
	rs.broadcast("decision", d)
	return nil
}

// activeDecisionTexts is the "현재 결정" list of the standing brief: text only,
// at most maxBriefDecisions, most recently changed first (later records win a
// tie). A card recorded by a message above cap is left out with that message,
// so a blind round-1 turn never sees a peer's round-1 decision.
func (rs *roomState) activeDecisionTexts(cap int64) []string {
	sources := map[string]int64{}
	for _, d := range rs.decisions {
		if d.Status == "active" && d.SourceMsg != "" {
			sources[d.SourceMsg] = 0
		}
	}
	for _, m := range rs.msgs {
		if _, ok := sources[m.ID]; ok {
			sources[m.ID] = m.Seq
		}
	}
	var picked []int
	for i, d := range rs.decisions {
		if d.Status == "active" && (d.SourceMsg == "" || sources[d.SourceMsg] <= cap) {
			picked = append(picked, i)
		}
	}
	sort.SliceStable(picked, func(a, b int) bool {
		da, db := rs.decisions[picked[a]], rs.decisions[picked[b]]
		if da.At != db.At {
			return da.At > db.At
		}
		return picked[a] > picked[b]
	})
	out := []string{}
	for _, i := range picked[:min(len(picked), maxBriefDecisions)] {
		out = append(out, rs.decisions[i].Text)
	}
	return out
}

func (h *Hub) Decisions(roomID string) ([]Decision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.rooms[roomID]
	if rs == nil {
		return nil, fmt.Errorf("방 없음: %s", roomID)
	}
	return append([]Decision{}, rs.decisions...), nil
}

func trimmed(s *string) string {
	if s == nil {
		return ""
	}
	return strings.TrimSpace(*s)
}

// AddDecision records a card written by a person on screen.
func (h *Hub) AddDecision(roomID string, in DecisionInput) (Decision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.rooms[roomID]
	if rs == nil {
		return Decision{}, fmt.Errorf("방 없음: %s", roomID)
	}
	if in.Status != nil && *in.Status != "active" {
		return Decision{}, fmt.Errorf("새 결정은 active 상태로만 추가할 수 있음")
	}
	d := Decision{ID: newID(), Text: trimmed(in.Text), Evidence: trimmed(in.Evidence), Dissent: trimmed(in.Dissent), Unverified: trimmed(in.Unverified), By: "user", Status: "active", At: nowMs()}
	if err := validDecision(d); err != nil {
		return Decision{}, err
	}
	if err := h.saveDecision(rs, d); err != nil {
		return Decision{}, err
	}
	return d, nil
}

// UpdateDecision edits a card or changes its status. Status changes keep the card.
func (h *Hub) UpdateDecision(roomID, id string, in DecisionInput) (Decision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs := h.rooms[roomID]
	if rs == nil {
		return Decision{}, fmt.Errorf("방 없음: %s", roomID)
	}
	old := rs.decision(id)
	if old == nil {
		return Decision{}, fmt.Errorf("결정 없음: %s", id)
	}
	if in.Text == nil && in.Evidence == nil && in.Dissent == nil && in.Unverified == nil && in.Status == nil {
		return Decision{}, fmt.Errorf("바꿀 항목이 없음")
	}
	next := *old
	if in.Text != nil {
		next.Text = trimmed(in.Text)
	}
	if in.Evidence != nil {
		next.Evidence = trimmed(in.Evidence)
	}
	if in.Dissent != nil {
		next.Dissent = trimmed(in.Dissent)
	}
	if in.Unverified != nil {
		next.Unverified = trimmed(in.Unverified)
	}
	if in.Status != nil {
		next.Status = *in.Status
	}
	if err := validDecision(next); err != nil {
		return Decision{}, err
	}
	if next == *old {
		return next, nil
	}
	// At strictly increases per card, even for two changes in one millisecond,
	// so clients can drop any state older than the one they show.
	next.At = max(nowMs(), old.At+1)
	if err := h.saveDecision(rs, next); err != nil {
		return Decision{}, err
	}
	return next, nil
}
