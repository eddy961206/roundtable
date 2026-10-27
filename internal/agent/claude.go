package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Claude runs `claude -p --input-format stream-json --output-format stream-json`.
// Keeping stdin open lets the room inject a user message while the turn runs.
type Claude struct{ cmd string }

// Applied per invocation; global/user/project files are not changed.
const claudeSandboxSettings = `{"sandbox":{"enabled":false}}`

func NewClaude(cmd string) *Claude { return &Claude{cmd: cmd} }

func (c *Claude) ID() string      { return "claude" }
func (c *Claude) Label() string   { return "Claude Code" }
func (c *Claude) Command() string { return c.cmd }
func (c *Claude) Available() bool { return available(c.cmd) }
func (c *Claude) CanSteer() bool  { return true }
func (c *Claude) CanFork() bool   { return true }

// Efforts are the values `claude --effort` accepts (claude --help, 2.1.299).
func (c *Claude) Efforts() []string                 { return []string{"low", "medium", "high", "xhigh", "max"} }
func (c *Claude) ModelEfforts() map[string][]string { return nil }
func (c *Claude) DefaultModel() (string, string)    { return "opus", "high" }
func (c *Claude) ContextWindow(model string) int {
	if strings.Contains(model, "[1m]") {
		return 1_000_000
	}
	return 200_000
}
func (c *Claude) Models() []ModelOption {
	return []ModelOption{
		{ID: "opus", Label: "Opus (최신)"},
		{ID: "sonnet", Label: "Sonnet (최신)"},
		{ID: "haiku", Label: "Haiku (최신)"},
		{ID: "fable", Label: "Fable (최신)"},
		{ID: "claude-fable-5-1", Label: "Fable 5.1"},
		{ID: "claude-opus-5-5", Label: "Opus 5.5"},
		{ID: "claude-sonnet-5-5", Label: "Sonnet 5.5"},
		{ID: "claude-haiku-5-5", Label: "Haiku 5.5"},
	}
}

// claudeEnv is the CLI environment for one run. A nested Claude Code refuses
// to start when it thinks it is inside another session. The room's compaction
// window, when set, replaces any inherited one; the user's settings are untouched.
func claudeEnv(o RunOptions) []string {
	if o.CompactWindow <= 0 {
		return envWithout("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_AUTO_COMPACT_WINDOW")
	}
	env := envWithout("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT", "CLAUDE_CODE_AUTO_COMPACT_WINDOW")
	return append(env, "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.Itoa(o.CompactWindow))
}

func claudeArgs(o RunOptions) []string {
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--strict-mcp-config", "--settings", claudeSandboxSettings}
	for _, dir := range imageDirs(o.Images) {
		args = append(args, "--add-dir", dir)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if o.SessionID != "" {
		args = append(args, "--resume", o.SessionID)
	}
	if o.ForkSession != "" {
		// Claude stores sessions per working directory, so the fork runs where
		// the original session ran.
		args = append(args, "--resume", o.ForkSession, "--fork-session")
	}
	// Print mode cannot prompt for permission, so every tool that needs approval
	// must be listed here; one --allowedTools carries the whole list.
	var allowed []string
	if o.MCP != nil && o.Mode != ModeChat {
		// --strict-mcp-config (above) keeps the user's own MCP servers out; this
		// adds only the given server and pre-approves its tools, since print mode
		// cannot prompt for permission.
		cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{
			o.MCP.Name: map[string]any{"type": "stdio", "command": o.MCP.Command, "args": o.MCP.Args},
		}})
		args = append(args, "--mcp-config", string(cfg))
		allowed = append(allowed, "mcp__"+o.MCP.Name)
	}
	switch o.Mode {
	case ModeRead:
		args = append(args, "--tools", "Read,Grep,Glob,Agent,Task,WebSearch,WebFetch", "--add-dir", o.Workspace)
	case ModeWrite:
		// Bash runs unsandboxed on the user's machine; the user accepted that so
		// Claude seats can build and test like the Codex and agy seats (2026-10-19).
		args = append(args, "--permission-mode", "acceptEdits", "--add-dir", o.Workspace)
		allowed = append(allowed, "Bash", "PowerShell") // Claude Code on Windows runs shell commands through PowerShell
	default:
		args = append(args, "--tools", "")
	}
	if len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	return args
}

func claudeUserLine(text string) []byte {
	b, _ := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": text}}},
	})
	return append(b, '\n')
}

func (c *Claude) Run(ctx context.Context, o RunOptions, emit func(Event)) (Result, error) {
	input, err := claudeImageLine(o)
	if err != nil {
		return Result{}, err
	}
	path, err := resolveCommand(c.cmd)
	if err != nil {
		return Result{}, fmt.Errorf("claude CLI를 찾지 못함: %w", err)
	}
	cmd := exec.Command(path, claudeArgs(o)...)
	cmd.Dir = workDir(o)
	cmd.Env = claudeEnv(o)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Result{}, err
	}
	p := newClaudeParser()
	var once sync.Once
	closeStdin := func() { once.Do(func() { _ = stdin.Close() }) }
	done := make(chan struct{})
	steerDone := make(chan struct{})
	var steered atomic.Int32
	var finished atomic.Bool
	var inputErr error // read after steerDone closes
	go func() {
		defer close(steerDone)
		if _, err := stdin.Write(input); err != nil {
			inputErr = fmt.Errorf("Claude 입력 전달 실패: %w", err)
			closeStdin()
			return
		}
		for {
			select {
			case <-done:
				return
			case msg, ok := <-o.Steer:
				if !ok {
					return
				}
				if finished.Load() {
					return // the turn already ended; the room redelivers it next turn
				}
				if _, err := stdin.Write(claudeUserLine(msg)); err != nil {
					return
				}
				steered.Add(1)
				x := ev(KindStatus)
				x.Text = "조정 메시지 주입"
				emit(x)
			}
		}
	}()
	onLine := func(line string) {
		p.Line(line, emit)
		if p.finished {
			finished.Store(true)
			closeStdin()
		}
	}
	stderr, runErr := runLines(ctx, cmd, "", onLine)
	close(done)
	closeStdin()
	<-steerDone
	fin := p.Finish(emit)
	res := Result{Text: fin.Text, SessionID: fin.SessionID, Usage: fin.Usage, Stderr: stderr, Steered: int(steered.Load())}
	if inputErr != nil {
		return res, inputErr
	}
	return res, finishError("claude", fin, runErr, stderr)
}

// finishError turns the parser's view plus the exit status into the turn error.
// Output without a normal end-of-turn event is a crash, not an answer.
func finishError(name string, fin Final, runErr error, stderr string) error {
	if fin.Err != "" {
		return fmt.Errorf("%s", fin.Err)
	}
	if runErr != nil && !fin.Complete {
		return fmt.Errorf("%s 실행 실패: %v: %s", name, runErr, lastLines(stderr, 3))
	}
	if !fin.Complete {
		return fmt.Errorf("%s 가 응답을 끝내지 못함 (완료 이벤트 없음): %s", name, lastLines(stderr, 3))
	}
	return nil
}

func workDir(o RunOptions) string {
	if o.Mode == ModeChat || o.Workspace == "" {
		return o.ScratchDir
	}
	return o.Workspace
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// claudeParser follows the stream-json protocol of `claude -p`.
type claudeParser struct {
	session    string
	parts      []string // top-level assistant text blocks
	result     string
	errText    string
	usage      map[string]any
	context    int    // tokens in the last API call = current context size
	model      string // model of the top-level assistant messages
	finished   bool
	toolNames  map[string]string // tool_use id -> name
	subagents  map[string]bool   // tool_use ids that spawned subagents
	taskToTool map[string]string // task_id -> tool_use id
	subModels  map[string]string // subagent tool_use id -> model last reported
}

func newClaudeParser() *claudeParser {
	return &claudeParser{toolNames: map[string]string{}, subagents: map[string]bool{}, taskToTool: map[string]string{}, subModels: map[string]string{}}
}

func (p *claudeParser) Line(line string, emit func(Event)) {
	m := parseLine(line)
	if m == nil {
		return
	}
	parent := str(m, "parent_tool_use_id")
	switch str(m, "type") {
	case "system":
		p.system(m, emit)
	case "rate_limit_event":
		p.limits(obj(m, "rate_limit_info"), emit)
	case "stream_event":
		e := obj(m, "event")
		if str(e, "type") == "content_block_delta" && parent == "" {
			d := obj(e, "delta")
			if str(d, "type") == "text_delta" && str(d, "text") != "" {
				x := ev(KindTextDelta)
				x.Text = str(d, "text")
				emit(x)
			}
		}
	case "assistant":
		p.assistant(m, parent, emit)
	case "user":
		p.user(m, parent, emit)
	case "result":
		p.finished = true
		if s := str(m, "session_id"); s != "" {
			p.session = s
		}
		p.result = str(m, "result")
		if isErr, _ := m["is_error"].(bool); isErr {
			p.errText = firstNonEmpty(str(m, "result"), contentText(m["errors"]), str(m, "subtype"))
		}
		p.usage = map[string]any{}
		// total_cost_usd in the result is the session's running total (it grows on
		// every resumed turn); the room turns it into this turn's cost.
		if v, ok := m["total_cost_usd"]; ok {
			p.usage["session_cost_usd"] = v
		}
		for _, k := range []string{"duration_ms", "duration_api_ms", "num_turns"} {
			if v, ok := m[k]; ok {
				p.usage[k] = v
			}
		}
		if u := obj(m, "usage"); u != nil {
			for _, k := range []string{"input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens"} {
				if v, ok := u[k]; ok {
					p.usage[k] = v
				}
			}
		}
		if p.context > 0 {
			p.usage["context_tokens"] = p.context
		}
		// modelUsage carries each model's context window (claude-opus-5-5: 1000000 on
		// CLI 2.1.299). ContextWindow's fixed 200000 made the context alarm read 226%
		// for a seat at 45%.
		if mu := obj(m, "modelUsage"); mu != nil {
			if w, ok := obj(mu, p.model)["contextWindow"].(float64); ok && w > 0 {
				p.usage["context_window"] = int(w)
			}
		}
	}
}

func (p *claudeParser) limits(info map[string]any, emit func(Event)) {
	if info == nil {
		return
	}
	l := Limits{Provider: "claude", At: now(), Source: "claude_turn_event"}
	labels := map[string]string{"five_hour": "5시간", "seven_day": "7일", "seven_day_sonnet": "7일·Sonnet", "seven_day_opus": "7일·Opus"}
	order := []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet"}
	wins := obj(info, "unifiedWindows")
	for _, k := range order {
		w := obj(wins, k)
		if w == nil {
			continue
		}
		util, hasUtil := w["utilization"].(float64)
		if !hasUtil || util < 0 || util > 1 {
			continue
		}
		reset, _ := w["resetsAt"].(float64)
		l.Windows = append(l.Windows, LimitWindow{Label: labels[k], UsedPercent: util * 100, ResetsAt: int64(reset)})
	}
	if len(l.Windows) == 0 {
		return
	}
	if s := str(info, "status"); s != "" && s != "allowed" {
		l.Note = s
	}
	x := ev(KindLimits)
	x.Meta = map[string]any{"limits": l}
	emit(x)
}

func (p *claudeParser) system(m map[string]any, emit func(Event)) {
	sub := str(m, "subtype")
	if s := str(m, "session_id"); s != "" && p.session == "" {
		p.session = s
	}
	switch sub {
	case "init":
		x := ev(KindSession)
		x.Text = p.session
		x.Meta = map[string]any{"model": str(m, "model")}
		emit(x)
	case "task_started":
		tool := str(m, "tool_use_id")
		p.taskToTool[str(m, "task_id")] = tool
		if !p.subagents[tool] {
			p.subagents[tool] = true
			x := ev(KindSubagentStart)
			x.Parent = tool
			x.Name = firstNonEmpty(str(m, "subagent_type"), "subagent")
			x.Input = str(m, "prompt")
			x.Meta = map[string]any{"description": str(m, "description"), "async": true}
			emit(x)
		}
	case "task_progress":
		x := ev(KindSubagentMsg)
		x.Parent = str(m, "tool_use_id")
		x.Role = "status"
		x.Text = str(m, "description")
		if u := obj(m, "usage"); u != nil {
			x.Meta = u
		}
		emit(x)
	case "task_notification":
		x := ev(KindSubagentEnd)
		x.Parent = str(m, "tool_use_id")
		x.Output = str(m, "summary")
		x.Meta = map[string]any{"status": str(m, "status")}
		emit(x)
	case "permission_denied":
		x := ev(KindStatus)
		x.Text = "권한 거부: " + str(m, "tool_name")
		if a := str(m, "agent_id"); a != "" {
			if tool, ok := p.taskToTool[a]; ok {
				x.Kind = KindSubagentMsg
				x.Parent = tool
				x.Role = "status"
			}
		}
		emit(x)
	case "hook_started", "hook_response", "commands_changed", "background_tasks_changed", "task_updated", "status":
		// noise for the chat view
	default:
		x := ev(KindStatus)
		x.Text = sub
		emit(x)
	}
}

func (p *claudeParser) assistant(m map[string]any, parent string, emit func(Event)) {
	msg := obj(m, "message")
	if parent == "" {
		if model := str(msg, "model"); model != "" && model != "<synthetic>" {
			p.model = model
		}
		if u := obj(msg, "usage"); u != nil {
			in, _ := u["input_tokens"].(float64)
			cr, _ := u["cache_read_input_tokens"].(float64)
			cc, _ := u["cache_creation_input_tokens"].(float64)
			if total := int(in + cr + cc); total > 0 {
				p.context = total
			}
		}
	}
	// The CLI does not report a subagent's reasoning effort, only its model.
	// "<synthetic>" marks messages the CLI made up itself (no model ran).
	if model := str(msg, "model"); parent != "" && model != "" && model != "<synthetic>" && p.subModels[parent] != model {
		p.subModels[parent] = model
		x := ev(KindSubagentMeta)
		x.Parent = parent
		x.Meta = map[string]any{"model": model}
		emit(x)
	}
	for _, b := range arr(msg, "content") {
		blk, _ := b.(map[string]any)
		switch str(blk, "type") {
		case "text":
			t := str(blk, "text")
			if t == "" {
				continue
			}
			if parent == "" {
				p.parts = append(p.parts, t)
			} else {
				x := ev(KindSubagentMsg)
				x.Parent, x.Role, x.Text = parent, "assistant", t
				emit(x)
			}
		case "thinking":
			// Some models return the thinking block without its text (only a
			// signature). An empty event says nothing, so it is not recorded.
			if t := strings.TrimSpace(str(blk, "thinking")); parent == "" && t != "" {
				x := ev(KindThinking)
				x.Text = clip(t, 4000)
				emit(x)
			}
		case "tool_use":
			id, name := str(blk, "id"), str(blk, "name")
			p.toolNames[id] = name
			input := obj(blk, "input")
			if parent != "" {
				x := ev(KindSubagentMsg)
				x.Parent, x.Role, x.ID, x.Name, x.Input = parent, "tool_call", id, name, compactJSON(input)
				emit(x)
				continue
			}
			if name == "Agent" || name == "Task" {
				if !p.subagents[id] {
					p.subagents[id] = true
					x := ev(KindSubagentStart)
					x.Parent = id
					x.Name = firstNonEmpty(str(input, "subagent_type"), "subagent")
					x.Input = str(input, "prompt")
					// model is the alias the caller asked for (e.g. "haiku"), if any.
					x.Meta = map[string]any{"description": str(input, "description"), "model": str(input, "model")}
					emit(x)
				}
				continue
			}
			x := ev(KindToolCall)
			x.ID, x.Name, x.Input = id, name, compactJSON(input)
			emit(x)
		}
	}
}

func (p *claudeParser) user(m map[string]any, parent string, emit func(Event)) {
	msg := obj(m, "message")
	for _, b := range arr(msg, "content") {
		blk, _ := b.(map[string]any)
		if str(blk, "type") != "tool_result" {
			continue
		}
		id := str(blk, "tool_use_id")
		out := contentText(blk["content"])
		isErr, _ := blk["is_error"].(bool)
		if parent != "" {
			x := ev(KindSubagentMsg)
			x.Parent, x.Role, x.ID, x.Output = parent, "tool_result", id, out
			x.Name = p.toolNames[id]
			if isErr {
				x.Meta = map[string]any{"error": true}
			}
			emit(x)
			continue
		}
		if p.subagents[id] {
			if strings.Contains(out, "Async agent launched") {
				continue // the real report arrives as task_notification
			}
			x := ev(KindSubagentEnd)
			x.Parent, x.Output = id, out
			emit(x)
			continue
		}
		x := ev(KindToolResult)
		x.ID, x.Name, x.Output = id, p.toolNames[id], out
		if isErr {
			x.Meta = map[string]any{"error": true}
		}
		emit(x)
	}
}

func (p *claudeParser) Finish(emit func(Event)) Final {
	text := strings.TrimSpace(strings.Join(p.parts, "\n\n"))
	if text == "" {
		text = strings.TrimSpace(p.result)
	}
	if p.usage != nil {
		x := ev(KindUsage)
		x.Meta = p.usage
		emit(x)
	}
	return Final{Text: text, SessionID: p.session, Err: p.errText, Usage: p.usage, Complete: p.finished}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
