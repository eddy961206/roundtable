package room

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sort"
)

const DefaultHistoryLimit = 100
const MaxHistoryLimit = 200

type HistoryPage struct {
	HasMore   bool  `json:"has_more"`
	BeforeSeq int64 `json:"before_seq"`
	Total     int   `json:"total"`
}

// SnapshotPageJSON copies a bounded page under the lock and serializes the
// detached values afterward so other rooms can keep receiving agent events.
func (h *Hub) SnapshotPageJSON(id string, before int64, limit int) ([]byte, error) {
	if limit == 0 {
		limit = DefaultHistoryLimit
	}
	if before < 0 || limit < 1 || limit > MaxHistoryLimit {
		return nil, fmt.Errorf("대화 조회 범위가 올바르지 않음 (limit: 1~%d)", MaxHistoryLimit)
	}
	h.mu.Lock()
	rs := h.rooms[id]
	if rs == nil {
		h.mu.Unlock()
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	snapshot := h.snapshotLocked(rs, before, limit)
	h.mu.Unlock()
	return json.Marshal(snapshot)
}

func (h *Hub) snapshotLocked(rs *roomState, before int64, limit int) *Snapshot {
	roomCopy := *rs.room
	roomCopy.Participants = make([]*Participant, len(rs.room.Participants))
	for i, p := range rs.room.Participants {
		copy := *p
		copy.Unseen = slices.Clone(p.Unseen)
		copy.Usage = maps.Clone(p.Usage)
		roomCopy.Participants[i] = &copy
	}
	end := len(rs.msgs)
	if before > 0 {
		end = sort.Search(len(rs.msgs), func(i int) bool { return rs.msgs[i].Seq >= before })
	}
	start := max(0, end-limit)
	messages := make([]*Message, end-start)
	for i, m := range rs.msgs[start:end] {
		copy := *m
		if m.Timing != nil {
			timing := *m.Timing
			copy.Timing = &timing
		}
		copy.Usage = maps.Clone(m.Usage)
		copy.Attachments = slices.Clone(m.Attachments)
		copy.Requests = slices.Clone(m.Requests)
		copy.Decisions = slices.Clone(m.Decisions)
		messages[i] = &copy
	}
	queue := rs.queueView()
	for i, turn := range queue {
		copy := *turn
		queue[i] = &copy
	}
	discussion := rs.discView()
	if discussion != nil {
		discussion.Signals = maps.Clone(discussion.Signals)
	}
	history := HistoryPage{HasMore: start > 0, Total: len(rs.msgs)}
	if history.HasMore {
		history.BeforeSeq = messages[0].Seq
	}
	return &Snapshot{Room: &roomCopy, Messages: messages, Queue: queue, Discussion: discussion, Limits: h.limitsCopy(), History: history, Work: cloneWork(rs.room.Work), Requests: append([]Request{}, rs.requests...), Decisions: append([]Decision{}, rs.decisions...)}
}
