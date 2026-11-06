package rtmcp

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// seatsWindow is how many recent messages per room rt_seats scans for each
// seat's last reply. An older reply is left out (last_seq 0).
const seatsWindow = 40

// quietWarnSeconds is how long a running seat may go without a CLI event
// before rt_seats flags it. On 2026-11-04 an agy shell command waited on stdin
// for 1h54m and the seat looked like any other running seat.
const quietWarnSeconds = 600

type seatState struct {
	ID      string     `json:"id"`
	Label   string     `json:"label"`
	Model   string     `json:"model,omitempty"`
	Enabled bool       `json:"enabled"`
	Removed bool       `json:"removed"`
	Status  string     `json:"status"` // idle | queued | running
	Text    string     `json:"status_text"`
	Tool    *toolState `json:"tool"`
	Context int        `json:"context_tokens"`
	Window  int        `json:"context_window"`
	Session string     `json:"session_id"`
	Turns   int        `json:"turns"`
	Held    string     `json:"budget_held"`
	Paced   int64      `json:"paced_until"`
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
	// A running seat: what it is doing, the tool call still waiting for its
	// result, and seconds since its CLI last produced an event. Warn is set
	// once that quiet passes quietWarnSeconds.
	Activity string   `json:"activity,omitempty"`
	Tool     *toolRow `json:"tool,omitempty"`
	Quiet    int64    `json:"quiet_seconds,omitempty"`
	Warn     string   `json:"warn,omitempty"`
	// PacedUntil is when a queued seat's speak interval ends (community spaces).
	PacedUntil int64  `json:"paced_until,omitempty"`
	LastSeq    int64  `json:"last_seq,omitempty"`
	Last       string `json:"last,omitempty"`
	// Asks are this seat's questions to the person still waiting or open.
	Asks         []askView     `json:"asks,omitempty"`
	UserRequests []userRequest `json:"user_requests,omitempty"`
}

type toolState struct {
	Name  string `json:"name"`
	Input string `json:"input"`
	Since int64  `json:"since"`
}

type toolRow struct {
	Name    string `json:"name"`
	Input   string `json:"input,omitempty"`
	Seconds int64  `json:"seconds"` // since the call started
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
			Messages     []apiMessage  `json:"messages"`
			Queue        []apiTurn     `json:"queue"`
			Asks         []askView     `json:"asks"`
			UserRequests []userRequest `json:"user_requests"`
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
		pendingRequests := []userRequest{}
		for _, request := range p.UserRequests {
			if request.Status == "pending" {
				pendingRequests = append(pendingRequests, request)
			}
		}
		for _, sp := range p.Room.Participants {
			if sp.Removed || !sp.Enabled {
				continue
			}
			row := seatRow{ID: sp.ID, Label: sp.Label, Model: sp.Model, Status: sp.Status, Context: sp.Context, Window: sp.Window, Session: sp.Session != "", Turns: sp.Turns, Held: sp.Held, PacedUntil: sp.Paced}
			if sp.Status == "running" {
				row.observeRunning(sp, p.Queue, time.Now().UnixMilli())
			}
			if m, ok := last[sp.ID]; ok {
				row.LastSeq = m.Seq
				row.Last = m.Status + ": " + clipLine(strings.ReplaceAll(summarizeText(m.Text), "\n", " / "), 120)
			}
			for _, a := range p.Asks {
				if a.Seat == sp.ID && a.unanswered() {
					row.Asks = append(row.Asks, a)
				}
			}
			for _, request := range pendingRequests {
				if request.Seat == sp.ID {
					row.UserRequests = append(row.UserRequests, request)
				}
			}
			rows = append(rows, row)
		}
		// Room-level requests retain a disabled/removed author's pending item.
		out = append(out, map[string]any{"room": id, "title": p.Room.Title, "active": len(p.Queue) > 0, "seats": rows, "user_requests": pendingRequests})
	}
	return map[string]any{"rooms": out}, nil
}

// observeRunning fills what a running seat is doing from its participant
// state and its running turn in the queue (now in ms).
func (row *seatRow) observeRunning(sp seatState, queue []apiTurn, now int64) {
	row.Activity = sp.Text
	if sp.Tool != nil {
		row.Tool = &toolRow{Name: sp.Tool.Name, Input: sp.Tool.Input, Seconds: (now - sp.Tool.Since) / 1000}
	}
	for _, t := range queue {
		if t.Agent == sp.ID && t.Status == "running" {
			row.Quiet = (now - t.Quiet) / 1000
		}
	}
	switch {
	case row.Quiet < quietWarnSeconds:
	case row.Tool == nil:
		row.Warn = fmt.Sprintf("%d분째 CLI 출력 없음", row.Quiet/60)
	default:
		row.Warn = fmt.Sprintf("%d분째 CLI 출력 없음: 도구 %s 실행 중. 명령이 입력(stdin)을 기다리거나 멈췄을 수 있음", row.Quiet/60, row.Tool.Name)
	}
}

func clipLine(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
