package rtmcp

import (
	"context"
	"fmt"
	"strings"
)

// seatsWindow is how many recent messages per room rt_seats scans for each
// seat's last reply. An older reply is left out (last_seq 0).
const seatsWindow = 40

type seatState struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Model   string `json:"model,omitempty"`
	Enabled bool   `json:"enabled"`
	Removed bool   `json:"removed"`
	Status  string `json:"status"` // idle | queued | running
	Context int    `json:"context_tokens"`
	Window  int    `json:"context_window"`
	Session string `json:"session_id"`
	Turns   int    `json:"turns"`
	Held    string `json:"budget_held"`
}

type seatRow struct {
	ID      string `json:"id"`
	Label   string `json:"label"`
	Model   string `json:"model,omitempty"`
	Status  string `json:"status"`
	Context int    `json:"ctx"`
	Window  int    `json:"window,omitempty"`
	Session bool   `json:"session"`
	Turns   int    `json:"turns"`
	Held    string `json:"held,omitempty"`
	LastSeq int64  `json:"last_seq,omitempty"`
	Last    string `json:"last,omitempty"`
}

// seats lists every active seat of the given rooms (or of the newest limit
// unarchived rooms) with what deciding a handoff or the next post needs.
func (s *Server) seats(ctx context.Context, rooms string, limit int) (any, error) {
	var ids []string
	if rooms != "" {
		ids = strings.Split(rooms, ",")
	} else {
		var listed []struct {
			ID       string `json:"id"`
			Active   bool   `json:"active"`
			Archived bool   `json:"archived"`
		}
		if err := s.api(ctx, "GET", "/api/rooms", nil, &listed); err != nil {
			return nil, err
		}
		for _, r := range listed {
			if len(ids) < limit && (!r.Archived || r.Active) {
				ids = append(ids, r.ID)
			}
		}
	}
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		var p struct {
			Room *struct {
				ID           string      `json:"id"`
				Title        string      `json:"title"`
				Participants []seatState `json:"participants"`
			} `json:"room"`
			Messages []apiMessage `json:"messages"`
			Queue    []apiTurn    `json:"queue"`
		}
		path := fmt.Sprintf("/api/rooms/%s?limit=%d", id, seatsWindow)
		if err := s.api(ctx, "GET", path, nil, &p); err != nil {
			return nil, err
		}
		if p.Room == nil || p.Room.ID != id {
			return nil, fmt.Errorf("%s: 방 응답의 id가 요청과 다름", path)
		}
		last := map[string]apiMessage{}
		for _, m := range p.Messages {
			if m.Kind == "agent" {
				last[m.Author] = m
			}
		}
		rows := []seatRow{}
		for _, sp := range p.Room.Participants {
			if sp.Removed || !sp.Enabled {
				continue
			}
			row := seatRow{ID: sp.ID, Label: sp.Label, Model: sp.Model, Status: sp.Status, Context: sp.Context, Window: sp.Window, Session: sp.Session != "", Turns: sp.Turns, Held: sp.Held}
			if m, ok := last[sp.ID]; ok {
				row.LastSeq = m.Seq
				row.Last = m.Status + ": " + clipLine(strings.ReplaceAll(summarizeText(m.Text), "\n", " / "), 120)
			}
			rows = append(rows, row)
		}
		out = append(out, map[string]any{"room": id, "title": p.Room.Title, "active": len(p.Queue) > 0, "seats": rows})
	}
	return map[string]any{"rooms": out}, nil
}

func clipLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
