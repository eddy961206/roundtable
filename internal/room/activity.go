package room

import "roundtable/internal/agent"

// toolInputRunes bounds the tool input kept on a seat: enough for a command
// line, not a whole file write.
const toolInputRunes = 300

// ToolActivity is a tool call of the running turn that has no result yet.
type ToolActivity struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Input string `json:"input,omitempty"`
	Since int64  `json:"since"` // ms the call was reported
}

// openTools tracks one turn's tool calls until their results arrive. Calls can
// overlap (parallel tools, subagents), so the oldest open one is shown: a hung
// command stays visible while later calls come and go.
type openTools []ToolActivity

// observe applies one CLI event and returns the oldest open call (nil when
// none) and whether that call changed.
func (o *openTools) observe(e agent.Event, at int64) (*ToolActivity, bool) {
	before := o.oldest()
	switch e.Kind {
	case agent.KindToolCall:
		input := []rune(e.Input)
		if len(input) > toolInputRunes {
			input = append(input[:toolInputRunes], '…')
		}
		*o = append(*o, ToolActivity{ID: e.ID, Name: e.Name, Input: string(input), Since: at})
	case agent.KindToolResult:
		for i, t := range *o {
			if t.ID == e.ID {
				*o = append((*o)[:i], (*o)[i+1:]...)
				break
			}
		}
	}
	after := o.oldest()
	return after, (before == nil) != (after == nil) || (before != nil && *before != *after)
}

func (o openTools) oldest() *ToolActivity {
	if len(o) == 0 {
		return nil
	}
	t := o[0]
	return &t
}
