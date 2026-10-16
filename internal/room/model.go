// Package room keeps the group-chat state and drives agent turns.
package room

import "roundtable/internal/agent"

// Participant is one agent seat in a room.
type Participant struct {
	ID            string         `json:"id"` // provider id: claude | codex | agy
	Label         string         `json:"label"`
	Enabled       bool           `json:"enabled"`
	Model         string         `json:"model"`
	Effort        string         `json:"effort"`
	SessionID     string         `json:"session_id"`
	LastSeen      int64          `json:"last_seen"` // highest message seq delivered to this agent
	Status        string         `json:"status"`    // idle | queued | running
	StatusText    string         `json:"status_text"`
	Turns         int            `json:"turns"`
	Usage         map[string]any `json:"usage,omitempty"` // last turn
	Totals        Totals         `json:"totals"`          // this room, all turns
	ContextTokens int            `json:"context_tokens"`  // size of the agent's last API call
	ContextWindow int            `json:"context_window"`
}

// Totals accumulates token usage of one participant.
type Totals struct {
	Input  float64 `json:"input"`
	Output float64 `json:"output"`
	Cached float64 `json:"cached"`
	Cost   float64 `json:"cost_usd"`
	Turns  int     `json:"turns"`
}

// Room is one chat room bound to a workspace folder.
type Room struct {
	ID               string         `json:"id"`
	Title            string         `json:"title"`
	CreatedAt        int64          `json:"created_at"`
	UpdatedAt        int64          `json:"updated_at"`
	Workspace        string         `json:"workspace"`
	Mode             string         `json:"mode"` // chat | read | write
	Note             string         `json:"note"` // standing context shown to every agent
	Participants     []*Participant `json:"participants"`
	MaxChain         int            `json:"max_chain"`         // how deep agent→agent mentions may go per user message
	DiscussionRounds int            `json:"discussion_rounds"` // default rounds for 토론 mode
	Seq              int64          `json:"seq"`
	Persona          bool           `json:"persona"` // allow light banter between agents
}

func (r *Room) participant(id string) *Participant {
	for _, p := range r.Participants {
		if p.ID == id {
			return p
		}
	}
	return nil
}

func (r *Room) enabled() []*Participant {
	var out []*Participant
	for _, p := range r.Participants {
		if p.Enabled {
			out = append(out, p)
		}
	}
	return out
}

// Message is one chat line. Agent messages stream in and end as done|error|cancelled.
type Message struct {
	ID       string         `json:"id"`
	Seq      int64          `json:"seq"`
	TS       int64          `json:"ts"`
	Kind     string         `json:"kind"`   // user | agent | system
	Author   string         `json:"author"` // "user" or participant id
	Text     string         `json:"text"`
	Status   string         `json:"status"`           // done | streaming | error | cancelled
	Target   string         `json:"target,omitempty"` // user message addressed to one agent only
	Live     bool           `json:"live,omitempty"`   // injected into a running turn (steering)
	TurnID   string         `json:"turn_id,omitempty"`
	Reason   string         `json:"reason,omitempty"` // mention | broadcast | discussion
	Round    int            `json:"round,omitempty"`
	Signal   string         `json:"signal,omitempty"` // discussion signal
	Error    string         `json:"error,omitempty"`
	Model    string         `json:"model,omitempty"`
	Duration int64          `json:"duration_ms,omitempty"`
	Usage    map[string]any `json:"usage,omitempty"`
	Trace    int            `json:"trace"` // number of trace events stored
}

// Turn is a queued request for one agent to speak.
type Turn struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Reason  string `json:"reason"`
	Group   string `json:"group"` // turns sharing a group run in parallel
	Depth   int    `json:"depth"`
	Round   int    `json:"round,omitempty"`
	Status  string `json:"status"` // queued | running
	Created int64  `json:"created"`
}

// TraceLine binds an agent event to the message it belongs to.
type TraceLine struct {
	MsgID string      `json:"msg_id"`
	Event agent.Event `json:"event"`
}
