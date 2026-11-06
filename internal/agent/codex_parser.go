package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// The app-server protocol is parsed directly into rt events. There is no exec
// JSON conversion or second execution path.
type codexParser struct {
	threadMu  sync.RWMutex
	thread    string
	turn      string
	parts     []string
	errText   string
	usage     map[string]any
	before    map[string]any // captured once before turn/start, never moved by steering
	names     map[string]string
	seen      map[string]bool
	context   int
	window    int
	completed bool
}

func newCodexParser() *codexParser {
	return &codexParser{names: map[string]string{}, seen: map[string]bool{}}
}

func (p *codexParser) threadID() string {
	p.threadMu.RLock()
	defer p.threadMu.RUnlock()
	return p.thread
}

func (p *codexParser) setThread(id string, emit func(Event)) {
	if id == "" || p.threadID() != "" {
		return
	}
	p.threadMu.Lock()
	p.thread = id
	p.threadMu.Unlock()
	x := ev(KindSession)
	x.Text = id
	emit(x)
}

func (p *codexParser) Line(line string, emit func(Event)) {
	var packet codexPacket
	if json.Unmarshal([]byte(line), &packet) == nil {
		p.packet(packet, emit)
	}
}

func (p *codexParser) packet(packet codexPacket, emit func(Event)) {
	var m map[string]any
	if json.Unmarshal(packet.Params, &m) != nil {
		return
	}
	if id := str(m, "threadId"); id != "" && p.threadID() != "" && id != p.threadID() {
		return
	}
	switch packet.Method {
	case "thread/started":
		p.setThread(str(obj(m, "thread"), "id"), emit)
	case "turn/started":
		if p.turn == "" {
			p.turn = str(obj(m, "turn"), "id")
		}
	case "turn/completed":
		turn := obj(m, "turn")
		if p.turn == "" || str(turn, "id") != p.turn {
			return
		}
		p.completed = true
		if status := str(turn, "status"); status != "completed" {
			p.errText = firstNonEmpty(str(obj(turn, "error"), "message"), "Codex 턴 "+status)
		}
	case "thread/tokenUsage/updated":
		if p.turn == "" || str(m, "turnId") != p.turn {
			return // a resume may first report the preceding turn
		}
		u := obj(m, "tokenUsage")
		p.usage = codexTokenMap(obj(u, "total"))
		if n, ok := obj(u, "last")["inputTokens"].(float64); ok {
			p.context = int(n)
		}
		if n, ok := u["modelContextWindow"].(float64); ok {
			p.window = int(n)
		}
	case "account/rateLimits/updated":
		if limits, err := parseCodexRateLimits(packet.Params, now()); err == nil {
			x := ev(KindLimits)
			x.Meta = map[string]any{"limits": limits}
			emit(x)
		}
	case "item/started", "item/completed":
		if p.turn != "" && str(m, "turnId") == p.turn {
			p.item(packet.Method == "item/completed", obj(m, "item"), emit)
		}
	case "turn/plan/updated":
		x := ev(KindStatus)
		x.Text, x.Meta = "할 일 목록 갱신", map[string]any{"items": m["plan"]}
		emit(x)
	case "error":
		text := firstNonEmpty(str(obj(m, "error"), "message"), "Codex 오류")
		kind := KindStatus
		if retry, _ := m["willRetry"].(bool); !retry {
			p.errText, kind = text, KindError
		}
		x := ev(kind)
		x.Text = text
		emit(x)
	case "roundtable/serverRequest":
		x := ev(KindStatus)
		x.Text = str(m, "text")
		emit(x)
	case "deprecationNotice", "warning":
		x := ev(KindStatus)
		x.Text = firstNonEmpty(str(m, "summary"), str(m, "message"), str(m, "details"))
		if x.Text != "" {
			emit(x)
		}
	}
}

func codexTokenMap(total map[string]any) map[string]any {
	if total == nil {
		return nil
	}
	out := map[string]any{}
	for camel, snake := range map[string]string{
		"inputTokens": "input_tokens", "cachedInputTokens": "cached_input_tokens",
		"cacheWriteInputTokens": "cache_write_input_tokens", "outputTokens": "output_tokens",
		"reasoningOutputTokens": "reasoning_output_tokens", "totalTokens": "total_tokens",
	} {
		if n, ok := total[camel].(float64); ok {
			out[snake] = n
		}
	}
	return out
}

func (p *codexParser) item(done bool, it map[string]any, emit func(Event)) {
	id, typ := str(it, "id"), str(it, "type")
	if done {
		if p.seen[id] {
			return
		}
		p.seen[id] = true
	}
	switch typ {
	case "agentMessage":
		if !done {
			return
		}
		if str(it, "delivery") == "async" {
			var questions []AskQuestion
			b, _ := json.Marshal(it["questions"])
			if json.Unmarshal(b, &questions) == nil && len(questions) > 0 {
				x := ev(KindAsk)
				x.ID, x.Meta = id, map[string]any{"questions": questions}
				emit(x)
				return
			}
			x := ev(KindStatus)
			x.Text = "Codex 사용자 질문(" + id + ")의 questions 필드를 해석하지 못함"
			emit(x)
			return
		}
		if text := str(it, "text"); strings.TrimSpace(text) != "" {
			p.parts = append(p.parts, text)
			x := ev(KindText)
			x.Text = text
			emit(x)
		}
	case "reasoning":
		if done {
			var summaries []string
			for _, value := range arr(it, "summary") {
				if text, ok := value.(string); ok {
					summaries = append(summaries, text)
				}
			}
			if text := strings.Join(summaries, "\n"); text != "" {
				x := ev(KindThinking)
				x.Text = clip(text, 4000)
				emit(x)
			}
		}
	case "commandExecution":
		x := ev(KindToolCall)
		x.ID, x.Name, x.Input = id, "shell", str(it, "command")
		if done {
			x.Kind, x.Output = KindToolResult, clip(str(it, "aggregatedOutput"), maxCapture)
			x.Meta = map[string]any{"exit_code": it["exitCode"], "status": str(it, "status")}
		}
		emit(x)
	case "fileChange":
		x := ev(KindToolCall)
		x.ID, x.Name, x.Input = id, "file_change", compactJSON(it["changes"])
		if done {
			x.Kind, x.Output = KindToolResult, x.Input
			x.Meta = map[string]any{"status": str(it, "status")}
		}
		emit(x)
	case "mcpToolCall":
		x := ev(KindToolCall)
		x.ID, x.Name, x.Input = id, str(it, "server")+"/"+str(it, "tool"), compactJSON(it["arguments"])
		if done {
			x.Kind, x.Output = KindToolResult, contentText(obj(it, "result")["content"])
			if e := it["error"]; e != nil {
				x.Meta = map[string]any{"error": compactJSON(e)}
			}
		}
		emit(x)
	case "webSearch":
		if done {
			x := ev(KindToolResult)
			x.ID, x.Name, x.Input = id, "web_search", str(obj(it, "action"), "query")
			emit(x)
		}
	case "collabAgentToolCall":
		state := "시작"
		if done {
			state = "완료"
		}
		x := ev(KindStatus)
		x.Text = fmt.Sprintf("서브에이전트 협업 %s %s", str(it, "tool"), state)
		x.Meta = map[string]any{"tool": it["tool"], "receivers": it["receiverThreadIds"], "prompt": it["prompt"]}
		emit(x)
	case "plan":
		if done {
			x := ev(KindStatus)
			x.Text = firstNonEmpty(str(it, "text"), "할 일 목록 갱신")
			emit(x)
		}
	}
}

func (p *codexParser) Finish(emit func(Event)) Final {
	var usage map[string]any
	if p.before != nil && p.usage != nil {
		usage = map[string]any{}
		for key, value := range p.usage {
			n, ok := value.(float64)
			prev, _ := p.before[key].(float64)
			if ok {
				if n < prev {
					usage = nil
					x := ev(KindStatus)
					x.Text = "Codex 누적 사용량이 시작 기준보다 작아 이번 턴 사용량을 확정하지 못함"
					emit(x)
					break
				}
				usage[key] = n - prev
			}
		}
		if usage != nil {
			if p.context > 0 {
				usage["context_tokens"] = p.context
			}
			if p.window > 0 {
				usage["context_window"] = p.window
			}
			x := ev(KindUsage)
			x.Meta = usage
			emit(x)
		}
	}
	return Final{Text: strings.TrimSpace(strings.Join(p.parts, "\n\n")), SessionID: p.threadID(), Err: p.errText, Usage: usage, Complete: p.completed}
}
