// Package room keeps the group-chat state and drives agent turns.
package room

import "roundtable/internal/agent"

// Participant is one agent seat in a room.
type Participant struct {
	ID            string         `json:"id"` // stable seat identity
	Provider      string         `json:"provider"`
	Removed       bool           `json:"removed,omitempty"` // retain authors and usage after removal
	Label         string         `json:"label"`
	Enabled       bool           `json:"enabled"`
	Model         string         `json:"model"`
	Effort        string         `json:"effort"`
	Role          string         `json:"role,omitempty"` // seat role directive; see docs/역할카드_개선_계약.md
	SessionID     string         `json:"session_id"`
	Fork          *ForkSource    `json:"fork,omitempty"`   // base session copied on the first turn; see docs/기반문맥_포크_계약.md
	Brief         string         `json:"brief,omitempty"`  // briefKey of the standing brief this session holds
	LastSeen      int64          `json:"last_seen"`        // highest message seq considered for delivery
	Unseen        []int64        `json:"unseen,omitempty"` // unfinished replies below LastSeen
	Status        string         `json:"status"`           // idle | queued | running
	StatusText    string         `json:"status_text"`
	Turns         int            `json:"turns"`
	Usage         map[string]any `json:"usage,omitempty"` // last turn
	Totals        Totals         `json:"totals"`          // this room, all turns
	ContextTokens int            `json:"context_tokens"`  // size of the agent's last API call
	ContextWindow int            `json:"context_window"`
	// Usage guards (budget.go): normalized cumulative input in this room, the
	// warning level already announced, whether the context warning is out, and
	// the budget reason last announced for a held turn.
	InputAll      float64 `json:"input_all"`
	InputWarned   int     `json:"input_warned,omitempty"`
	ContextWarned bool    `json:"context_warned,omitempty"`
	BudgetHeld    string  `json:"budget_held,omitempty"`
	// A provider limit error holds the seat's new turns until HoldUntil (ms);
	// HoldReason is the error that caused it (limit_hold.go).
	HoldUntil  int64  `json:"hold_until,omitempty"`
	HoldReason string `json:"hold_reason,omitempty"`
	// HistoryFrom is the seq of the handoff summary a new session starts from;
	// its first turn gets that summary and later messages only (handoff.go).
	HistoryFrom int64 `json:"history_from,omitempty"`
}

// ForkSource is the base seat session a seat starts from. The copy is made on
// the seat's first turn (SessionID empty) and kept as provenance afterwards.
type ForkSource struct {
	Room    string `json:"room"`
	Seat    string `json:"seat"`
	Session string `json:"session"`
	Dir     string `json:"dir"` // the base room's CLI directory (sessionDir); "" is chat
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
	// BlindFirstDefault is the 토론 시작 default: round 1 answers independently from one cap.
	BlindFirstDefault bool `json:"blind_first_default"`
	// Usage guards (budget.go). InputWarnTokens and ContextWarnPercent: 0 is the
	// default, -1 off. The budgets: 0 is no cap.
	InputWarnTokens    int64 `json:"input_warn_tokens,omitempty"`
	SeatInputBudget    int64 `json:"seat_input_budget,omitempty"`
	RoomInputBudget    int64 `json:"room_input_budget,omitempty"`
	ContextWarnPercent int   `json:"context_warn_percent,omitempty"`
	TurnTimeoutMinutes int   `json:"turn_timeout_minutes,omitempty"` // 0: DefaultTurnTimeoutMinutes
	// CompactWindowTokens makes Claude seats compact their session at this
	// context size (budget.go); 0 keeps the CLI default.
	CompactWindowTokens int `json:"compact_window_tokens,omitempty"`
	// QuietDone: a [[REQUEST_DONE]] result does not wake its requester
	// (chainMentions). A rejection still does. For review rooms where a pass
	// needs no reply and a fix is asked with its own [[REQUEST]].
	QuietDone bool `json:"quiet_done,omitempty"`
	// Archived rooms are left out of room lists unless asked for; they keep
	// working and an active one is still listed.
	Archived bool      `json:"archived,omitempty"`
	Work     *WorkPlan `json:"-"` // stored in room.json "work"; clients get Snapshot.work and "work" events
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
		if p.Enabled && !p.Removed {
			out = append(out, p)
		}
	}
	return out
}

// Message is one chat line. Agent messages stream in and end as done|error|cancelled.
type Message struct {
	NoticeFor        string         `json:"notice_for,omitempty"` // durable out-of-cap feedback to this seat; visible to everyone
	NoticeDelivered  bool           `json:"notice_delivered,omitempty"`
	NoticeMarker     string         `json:"notice_marker,omitempty"` // marker feedback: attach | request | request_result | decision | discussion
	NoticeMsg        string         `json:"notice_msg,omitempty"`    // reply whose marker was rejected
	UserOnly         bool           `json:"user_only,omitempty"`     // for people and the orchestrator; never in a seat's history (usage warnings)
	Attachments      []Attachment   `json:"attachments,omitempty"`
	Requests         []string       `json:"requests,omitempty"`
	Decisions        []string       `json:"decisions,omitempty"`       // decision cards recorded by this reply's markers
	Forward          *ForwardSource `json:"forward,omitempty"`         // a copy of another room's message (Hub.Forward)
	PendingMarkers   int            `json:"pending_markers,omitempty"` // marker lines a failed turn left unapplied (Hub.ApplyMarkers)
	MarkersApplied   bool           `json:"markers_applied,omitempty"` // a person applied this failed turn's markers: the answer counts as complete
	Mentions         *[]string      `json:"mentions,omitempty"`        // nil: legacy; empty list: resolved without ordinary mentions
	ID               string         `json:"id"`
	Seq              int64          `json:"seq"`
	TS               int64          `json:"ts"`
	Kind             string         `json:"kind"`   // user | agent | system
	Author           string         `json:"author"` // "user" or participant id
	Text             string         `json:"text"`
	Status           string         `json:"status"`           // done | streaming | error | cancelled
	Target           string         `json:"target,omitempty"` // user message addressed to one agent only
	Live             bool           `json:"live,omitempty"`   // injected into a running turn (steering)
	TurnID           string         `json:"turn_id,omitempty"`
	ReplyTo          string         `json:"reply_to,omitempty"` // triggering message ID, when the turn has one
	Reason           string         `json:"reason,omitempty"`   // mention | broadcast | discussion | part | review
	Round            int            `json:"round,omitempty"`
	Signal           string         `json:"signal,omitempty"` // discussion signal or WORK:DONE
	Part             string         `json:"part,omitempty"`
	ReviewStep       string         `json:"review_step,omitempty"` // findings | author_reply | rebuttal | recheck
	Error            string         `json:"error,omitempty"`
	ErrorKind        string         `json:"error_kind,omitempty"` // review_format: the review table was not applied
	Model            string         `json:"model,omitempty"`
	Provider         string         `json:"provider,omitempty"`
	SessionID        string         `json:"session_id,omitempty"`
	SessionDirectory string         `json:"session_directory,omitempty"`
	Mode             string         `json:"mode,omitempty"`
	Effort           string         `json:"effort,omitempty"`
	Duration         int64          `json:"duration_ms,omitempty"`
	Role             string         `json:"role,omitempty"`   // the seat role when this reply was produced
	Timing           *TurnTiming    `json:"timing,omitempty"` // stage durations of this agent turn
	Usage            map[string]any `json:"usage,omitempty"`
	Trace            int            `json:"trace"` // number of trace events stored
}

// Turn is a queued request for one agent to speak.
type Turn struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	Reason   string `json:"reason"`
	Group    string `json:"group"` // broadcast members share the trigger message cap
	Cap      int64  `json:"cap"`   // inclusive message sequence boundary; discussion reads current history
	Depth    int    `json:"depth"`
	Round    int    `json:"round,omitempty"`
	Status   string `json:"status"` // queued | running
	Created  int64  `json:"created"`
	Part     string `json:"part,omitempty"`  // work part this turn runs for (D9, review)
	Blind    bool   `json:"blind,omitempty"` // blind first discussion round: runs concurrently and reads only up to Cap
	Fresh    bool   `json:"fresh,omitempty"` // handoff: the new session does not fork the seat's base again
	parallel bool   // broadcast prompt instructions survive independent start times
	priority bool   // targeted user messages precede ordinary waiting turns
	prompt   string // hub-built part or review instruction
	step     string // review step kind
}

// TraceLine binds an agent event to the message it belongs to.
type TraceLine struct {
	MsgID string      `json:"msg_id"`
	Event agent.Event `json:"event"`
}

// TurnTiming splits an agent turn into stages so slow CLIs and slow service
// paths can be told apart. All values are milliseconds; zero means the stage
// did not happen (for example no output before an error).
type TurnTiming struct {
	QueuedMs      int64 `json:"queued_ms"`       // turn created -> started
	FirstOutputMs int64 `json:"first_output_ms"` // started -> first CLI event
	TotalMs       int64 `json:"total_ms"`        // started -> finished
}
