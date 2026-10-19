package room

import "fmt"

func (rs *roomState) pendingNotices(seat, partID string) []*Message {
	var notices []*Message
	for _, m := range rs.msgs {
		if m.Kind == "system" && m.NoticeFor == seat && m.Part == partID && !m.NoticeDelivered {
			notices = append(notices, m)
		}
	}
	return notices
}

// Notice scope is the seat's reusable room session or its specific part.
// Disposable reviewer sessions and plan-only bootstrap turns never receive it.
func (pr *prepared) noticeScope() (partID string, eligible bool) {
	if pr.reviewer || pr.bootstrap {
		return "", false
	}
	if pr.part != nil {
		return pr.part.ID, true
	}
	return "", true
}

func (rs *roomState) addTurnNotices(pr *prepared) {
	partID, eligible := pr.noticeScope()
	if !eligible {
		return
	}
	for _, m := range rs.pendingNotices(pr.p.ID, partID) {
		pr.opts.Prompt += fmt.Sprintf("\n[System 공지 %s]\n%s\n", m.ID, m.Text)
		pr.notices = append(pr.notices, m)
	}
}

// A failed/cancelled invocation leaves notices undelivered. Persist the
// receipt on the message, without advancing either history cursor past cap.
func (h *Hub) acknowledgeNotices(rs *roomState, pr *prepared) error {
	partID, eligible := pr.noticeScope()
	if !eligible {
		return nil
	}
	for _, m := range pr.notices {
		if m.NoticeFor != pr.p.ID || m.Part != partID {
			continue
		}
		next := *m
		next.NoticeDelivered = true
		if err := h.store.AppendMessage(rs.room.ID, &next); err != nil {
			return err
		}
		m.NoticeDelivered = true
	}
	return nil
}
