package agent

import "strings"

type openCodeParser struct {
	session   string
	parts     []string
	errText   string
	completed bool
	usage     map[string]any
}

func newOpenCodeParser() *openCodeParser { return &openCodeParser{usage: map[string]any{}} }

func (p *openCodeParser) Line(line string, emit func(Event)) {
	m := parseLine(line)
	if m == nil {
		return
	}
	if id := str(m, "sessionID"); id != "" && p.session == "" {
		p.session = id
		x := ev(KindSession)
		x.Text = id
		emit(x)
	}
	part := obj(m, "part")
	switch str(m, "type") {
	case "text":
		if text := str(part, "text"); text != "" {
			p.parts = append(p.parts, text)
			x := ev(KindText)
			x.Text = text
			emit(x)
		}
	case "reasoning":
		x := ev(KindThinking)
		x.Text = clip(str(part, "text"), 4000)
		emit(x)
	case "step_start":
		p.completed = false
	case "step_finish":
		p.completed = openCodeFinished(str(part, "reason"))
		p.addUsage(part)
	case "tool_use":
		state := obj(part, "state")
		id := str(part, "id")
		name := str(part, "tool")
		x := ev(KindToolCall)
		x.ID, x.Name, x.Input = id, name, compactJSON(state["input"])
		emit(x)
		x = ev(KindToolResult)
		x.ID, x.Name = id, name
		x.Output = clip(firstNonEmpty(str(state, "output"), str(state, "error")), maxCapture)
		if str(state, "status") == "error" {
			x.Meta = map[string]any{"error": true}
		}
		emit(x)
	case "error":
		p.errText = firstNonEmpty(str(obj(m, "error"), "message"), "OpenCode 실행 오류")
		p.completed = false
	}
}

func (p *openCodeParser) Finish(emit func(Event)) Final {
	if len(p.usage) > 0 {
		x := ev(KindUsage)
		x.Meta = p.usage
		emit(x)
	}
	return Final{Text: strings.TrimSpace(strings.Join(p.parts, "\n\n")), SessionID: p.session, Err: p.errText, Usage: p.usage, Complete: p.completed}
}

func openCodeFinished(reason string) bool {
	return reason == "stop" || reason == "length" || reason == "end_turn"
}

func (p *openCodeParser) addUsage(part map[string]any) {
	tokens := obj(part, "tokens")
	add := func(key string, v any) {
		n, ok := v.(float64)
		if !ok {
			return
		}
		old, _ := p.usage[key].(float64)
		p.usage[key] = old + n
	}
	add("input_tokens", tokens["input"])
	add("output_tokens", tokens["output"])
	add("total_cost_usd", part["cost"])
	if cache := obj(tokens, "cache"); cache != nil {
		add("cache_read_input_tokens", cache["read"])
		add("cache_creation_input_tokens", cache["write"])
	}
	// tokens.input excludes the cache, so the context this step carried is the
	// sum; input alone shrinks whenever more of it is cached and looked like a
	// compaction (measured 2026-10-20: 1027, 197, 725, 279).
	if n, ok := tokens["input"].(float64); ok {
		cache := obj(tokens, "cache")
		read, _ := cache["read"].(float64)
		write, _ := cache["write"].(float64)
		p.usage["context_tokens"] = n + read + write
	}
}
