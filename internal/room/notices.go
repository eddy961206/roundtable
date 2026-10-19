package room

import "fmt"

func (rs *roomState) pendingNotices(seat string) []*Message {
	var notices []*Message
	for _, m := range rs.msgs {
		if m.Kind == "system" && m.NoticeFor == seat && !m.NoticeDelivered {
			notices = append(notices, m)
		}
	}
	return notices
}

func (rs *roomState) addTurnNotices(pr *prepared) {
	for _, m := range rs.pendingNotices(pr.p.ID) {
		pr.opts.Prompt += fmt.Sprintf("\n[System 공지 %s]\n%s\n", m.ID, m.Text)
		pr.notices = append(pr.notices, m)
	}
}

// A failed/cancelled invocation leaves notices undelivered. Persist the
// receipt on the message, without advancing either history cursor past cap.
func (h *Hub) acknowledgeNotices(rs *roomState, pr *prepared) error {
	for _, m := range pr.notices {
		next := *m
		next.NoticeDelivered = true
		if err := h.store.AppendMessage(rs.room.ID, &next); err != nil {
			return err
		}
		m.NoticeDelivered = true
	}
	return nil
}
