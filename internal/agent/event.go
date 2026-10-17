// Package agent runs the vendor CLIs (Claude Code, Codex, Antigravity) in
// non-interactive mode and turns their streaming output into one event model.
package agent

import "time"

// Event kinds. Every provider parser emits only these.
const (
	KindSession       = "session"        // Text = provider session/thread id
	KindTextDelta     = "text_delta"     // Text = streamed fragment of the reply
	KindText          = "text"           // Text = a completed reply block (providers without deltas)
	KindThinking      = "thinking"       // Text = reasoning summary, if the CLI exposes one
	KindToolCall      = "tool_call"      // Name = tool, Input = arguments, ID = call id
	KindToolResult    = "tool_result"    // ID = call id, Output = result
	KindSubagentStart = "subagent_start" // Parent = subagent id, Name = type/role, Input = task prompt
	KindSubagentMsg   = "subagent_msg"   // Parent = subagent id, Role = assistant|user|tool_call|tool_result|system
	KindSubagentEnd   = "subagent_end"   // Parent = subagent id, Output = final report
	KindStatus        = "status"         // Text = free-form status line
	KindError         = "error"          // Text = error message
	KindUsage         = "usage"          // Meta = tokens/cost/context_tokens/context_window
	KindLimits        = "limits"         // Meta = Limits snapshot of the vendor account
)

// Limits is the vendor account's remaining allowance as the CLI reports it.
type Limits struct {
	Provider string        `json:"provider"`
	At       int64         `json:"at"`
	Source   string        `json:"source,omitempty"`
	Windows  []LimitWindow `json:"windows"`
	Plan     string        `json:"plan,omitempty"`
	Credits  string        `json:"credits,omitempty"`
	Note     string        `json:"note,omitempty"`
}

// LimitWindow is one rolling window such as "5시간" or "7일".
type LimitWindow struct {
	Label       string  `json:"label"`
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"` // unix seconds, 0 if unknown
}

// Event is one normalized trace item of an agent turn.
type Event struct {
	TS     int64          `json:"ts"`
	Kind   string         `json:"kind"`
	ID     string         `json:"id,omitempty"`
	Parent string         `json:"parent,omitempty"`
	Name   string         `json:"name,omitempty"`
	Role   string         `json:"role,omitempty"`
	Input  string         `json:"input,omitempty"`
	Output string         `json:"output,omitempty"`
	Text   string         `json:"text,omitempty"`
	Meta   map[string]any `json:"meta,omitempty"`
}

func now() int64 { return time.Now().UnixMilli() }

func ev(kind string) Event { return Event{TS: now(), Kind: kind} }

// Final is what a parser knows once the process has exited.
type Final struct {
	Text      string
	SessionID string
	Err       string
	Usage     map[string]any
	Complete  bool // the CLI reported a normal end of turn
}

// lineParser turns one stdout line into events and keeps the final answer.
type lineParser interface {
	Line(line string, emit func(Event))
	Finish(emit func(Event)) Final
}
