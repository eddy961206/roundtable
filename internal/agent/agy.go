package agent

import (
	"bufio"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const agyPromptLimit = 24 * 1024 // Windows command line budget

// Agy runs Google's Antigravity CLI in print mode.
type Agy struct {
	cmd    string
	mu     sync.Mutex
	models []ModelOption
	loaded time.Time
}

func NewAgy(cmd string) *Agy { return &Agy{cmd: cmd} }

func (a *Agy) ID() string      { return "agy" }
func (a *Agy) Label() string   { return "Antigravity" }
func (a *Agy) Command() string { return a.cmd }
func (a *Agy) Available() bool { return available(a.cmd) }
func (a *Agy) CanSteer() bool  { return false }
func (a *Agy) ContextWindow(model string) int {
	switch {
	case strings.HasPrefix(model, "claude"):
		return 200_000
	case strings.HasPrefix(model, "gpt-oss"):
		return 131_072
	}
	return 1_048_576
}

// Models asks `agy models` (cached for 10 minutes); the slug already encodes the effort.
func (a *Agy) Models() []ModelOption {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.models) > 0 && time.Since(a.loaded) < 10*time.Minute {
		return a.models
	}
	var out []ModelOption
	for _, l := range listOutput(a.cmd, "models") {
		parts := strings.SplitN(l, "\t", 2)
		if len(parts) != 2 || strings.Contains(parts[0], " ") {
			continue
		}
		out = append(out, ModelOption{ID: strings.TrimSpace(parts[0]), Label: strings.TrimSpace(parts[1])})
	}
	if len(out) == 0 {
		out = []ModelOption{
			{ID: "gemini-3.8-flash-high", Label: "Gemini 3.8 Flash (High)"},
			{ID: "gemini-4-argon-high", Label: "Gemini 4 Argon (High)"},
			{ID: "claude-opus-5-5-high", Label: "Claude Opus 5.5 (High)"},
		}
	}
	a.models, a.loaded = out, time.Now()
	return out
}

func agyArgs(o RunOptions) []string {
	args := []string{"--sandbox", "--disable-slash-commands", "--output-format", "stream-json"}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.SessionID != "" {
		args = append(args, "--conversation", o.SessionID)
	}
	if o.Timeout > 0 {
		args = append(args, "--print-timeout", fmt.Sprintf("%ds", int(o.Timeout.Seconds())))
	}
	switch o.Mode {
	case ModeRead:
		args = append(args, "--mode", "plan", "--add-dir", o.Workspace)
	case ModeWrite:
		args = append(args, "--mode", "accept-edits", "--add-dir", o.Workspace)
	default:
		args = append(args, "--mode", "plan")
	}
	args = append(args, "-p="+compactPrompt(o.Prompt, agyPromptLimit))
	return args
}

func compactPrompt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	marker := "\n\n[명령줄 길이 한도로 이전 대화 일부 생략]\n\n"
	head := 6000
	tail := limit - len(marker) - head
	h := s[:head]
	for len(h) > 0 && !isRuneStart(s[len(h)]) {
		h = h[:len(h)-1]
	}
	t := s[len(s)-tail:]
	for len(t) > 0 && !isRuneStart(t[0]) {
		t = t[1:]
	}
	return h + marker + t
}

func (a *Agy) Run(ctx context.Context, o RunOptions, emit func(Event)) (Result, error) {
	path, err := resolveCommand(a.cmd)
	if err != nil {
		return Result{}, fmt.Errorf("agy CLI를 찾지 못함: %w", err)
	}
	cmd := exec.Command(path, agyArgs(o)...)
	cmd.Dir = workDir(o)
	cmd.Env = os.Environ()
	p := newAgyParser()
	stderr, runErr := runLines(ctx, cmd, "", func(line string) { p.Line(line, emit) })
	fin := p.Finish(emit)
	res := Result{Text: fin.Text, SessionID: fin.SessionID, Usage: fin.Usage, Stderr: stderr}
	if fin.Err != "" {
		return res, fmt.Errorf("%s", fin.Err)
	}
	if runErr != nil && res.Text == "" {
		return res, fmt.Errorf("agy 실행 실패: %v: %s", runErr, lastLines(stderr, 3))
	}
	if res.Text == "" && runErr == nil {
		return res, fmt.Errorf("agy가 빈 응답을 반환함 (사용량 한도나 로그인 상태를 확인)")
	}
	return res, nil
}

type agyParser struct {
	conv     string
	text     strings.Builder
	response string
	errText  string
	usage    map[string]any
	context  int
	toolSeen map[string]bool
	subs     map[string]*agySub // conversation_id -> state
}

type agySub struct {
	logPath string
	offset  int64
	ended   bool
}

func newAgyParser() *agyParser {
	return &agyParser{toolSeen: map[string]bool{}, subs: map[string]*agySub{}}
}

func (p *agyParser) Line(line string, emit func(Event)) {
	m := parseLine(line)
	if m == nil {
		return
	}
	switch str(m, "event") {
	case "init":
		p.conv = str(m, "conversation_id")
		x := ev(KindSession)
		x.Text = p.conv
		x.Meta = map[string]any{"model": str(obj(m, "init"), "model")}
		emit(x)
	case "step_update":
		p.step(obj(m, "step_update"), emit)
	case "result":
		r := obj(m, "result")
		p.response = str(r, "response")
		if str(r, "status") != "SUCCESS" {
			p.errText = firstNonEmpty(str(r, "error"), str(r, "status"))
		}
		p.usage = obj(r, "usage")
		if p.usage != nil {
			p.usage["duration_seconds"] = r["duration_seconds"]
		}
	}
}

func (p *agyParser) step(s map[string]any, emit func(Event)) {
	idx := fmt.Sprintf("s%v", s["step_index"])
	state := str(s, "state")
	switch str(s, "step_type") {
	case "agent_response":
		if u := obj(s, "usage"); u != nil {
			if in, ok := u["input_tokens"].(float64); ok && int(in) > 0 {
				p.context = int(in)
			}
		}
		if d := str(s, "text_delta"); d != "" {
			p.text.WriteString(d)
			x := ev(KindTextDelta)
			x.Text = d
			emit(x)
		}
	case "tool":
		info := obj(s, "tool_info")
		name := firstNonEmpty(str(s, "tool_name"), str(info, "name"))
		if state == "ACTIVE" && !p.toolSeen[idx] {
			p.toolSeen[idx] = true
			x := ev(KindToolCall)
			x.ID, x.Name, x.Input = idx, name, compactJSON(info["parameters"])
			emit(x)
		}
		if state == "DONE" {
			x := ev(KindToolResult)
			x.ID, x.Name, x.Output = idx, name, contentText(info["output"])
			if !p.toolSeen[idx] {
				x.Input = compactJSON(info["parameters"])
				p.toolSeen[idx] = true
			}
			if e := str(obj(info, "error"), "message"); e != "" {
				x.Meta = map[string]any{"error": e}
			}
			emit(x)
		}
	case "subagent":
		if state != "DONE" {
			x := ev(KindStatus)
			x.Text = "서브에이전트 호출 중"
			emit(x)
			return
		}
		for _, raw := range arr(obj(s, "subagent_info"), "subagents") {
			sub, _ := raw.(map[string]any)
			id := str(sub, "conversation_id")
			if id == "" || p.subs[id] != nil {
				continue
			}
			p.subs[id] = &agySub{logPath: fileURLToPath(str(sub, "log_uri"))}
			x := ev(KindSubagentStart)
			x.Parent = id
			x.Name = strings.TrimSpace(str(sub, "role") + " (" + str(sub, "type_name") + ")")
			x.Input = str(sub, "initial_prompt")
			x.Meta = map[string]any{"log": p.subs[id].logPath}
			emit(x)
		}
	default:
		if state == "DONE" && str(s, "step_type") != "user_input" {
			x := ev(KindStatus)
			x.Text = str(s, "step_type")
			emit(x)
		}
	}
	p.readTranscripts(emit, false)
}

// readTranscripts streams new lines of every sub-agent transcript.jsonl.
func (p *agyParser) readTranscripts(emit func(Event), final bool) {
	for id, sub := range p.subs {
		if sub.ended || sub.logPath == "" {
			continue
		}
		f, err := os.Open(sub.logPath)
		if err != nil {
			continue
		}
		if _, err := f.Seek(sub.offset, 0); err == nil {
			r := bufio.NewReaderSize(f, 256*1024)
			for {
				line, err := r.ReadString('\n')
				if err != nil {
					if final && strings.TrimSpace(line) != "" {
						sub.offset += int64(len(line))
						for _, x := range agyTranscriptEvents(id, line) {
							emit(x)
						}
					}
					break
				}
				sub.offset += int64(len(line))
				for _, x := range agyTranscriptEvents(id, line) {
					emit(x)
				}
			}
		}
		f.Close()
		if final {
			sub.ended = true
			x := ev(KindSubagentEnd)
			x.Parent = id
			emit(x)
		}
	}
}

// agyTranscriptEvents maps one line of an Antigravity transcript.jsonl.
func agyTranscriptEvents(convID, line string) []Event {
	m := parseLine(line)
	if m == nil {
		return nil
	}
	var out []Event
	mk := func(role string) Event {
		x := ev(KindSubagentMsg)
		x.Parent, x.Role = convID, role
		return x
	}
	switch str(m, "type") {
	case "SYSTEM_MESSAGE":
		x := mk("system")
		x.Text = clip(trimSystemMessage(str(m, "content")), 4000)
		out = append(out, x)
	case "PLANNER_RESPONSE":
		if c := str(m, "content"); strings.TrimSpace(c) != "" {
			x := mk("assistant")
			x.Text = c
			out = append(out, x)
		}
		for _, raw := range arr(m, "tool_calls") {
			tc, _ := raw.(map[string]any)
			x := mk("tool_call")
			x.Name = str(tc, "name")
			x.Input = compactJSON(tc["args"])
			out = append(out, x)
		}
	case "GENERIC":
		x := mk("tool_result")
		x.Output = clip(str(m, "content"), maxCapture)
		out = append(out, x)
	}
	return out
}

func trimSystemMessage(s string) string {
	if i := strings.Index(s, "content="); i >= 0 {
		s = s[i+len("content="):]
		s = strings.TrimSuffix(strings.TrimSpace(s), "</SYSTEM_MESSAGE>")
	}
	return strings.TrimSpace(s)
}

func fileURLToPath(u string) string {
	if u == "" {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "file" {
		return u
	}
	path := parsed.Path
	if runtime.GOOS == "windows" {
		path = strings.TrimPrefix(path, "/")
		path = filepath.FromSlash(path)
	}
	return path
}

func (p *agyParser) Finish(emit func(Event)) Final {
	p.readTranscripts(emit, true)
	text := strings.TrimSpace(p.response)
	if text == "" {
		text = strings.TrimSpace(p.text.String())
	}
	if p.usage != nil {
		if p.context > 0 {
			p.usage["context_tokens"] = p.context
		}
		x := ev(KindUsage)
		x.Meta = p.usage
		emit(x)
	}
	return Final{Text: text, SessionID: p.conv, Err: p.errText, Usage: p.usage}
}
