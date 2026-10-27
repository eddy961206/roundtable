package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Agy runs Google's Antigravity CLI in print mode.
type Agy struct {
	cmd        string
	statusPath string // written by `roundtable agy-statusline` (see agystatus.go)
	mu         sync.Mutex
	models     []ModelOption
	efforts    map[string][]string
	loaded     time.Time
}

func NewAgy(cmd, statusPath string) *Agy { return &Agy{cmd: cmd, statusPath: statusPath} }

func (a *Agy) ID() string      { return "agy" }
func (a *Agy) Label() string   { return "Antigravity" }
func (a *Agy) Command() string { return a.cmd }
func (a *Agy) Available() bool { return available(a.cmd) }
func (a *Agy) CanSteer() bool  { return false }
func (a *Agy) CanFork() bool   { return false }

// Efforts is the union `agy --effort` accepts; ModelEfforts narrows it per model.
func (a *Agy) Efforts() []string              { return []string{"low", "medium", "high", "xhigh", "max"} }
func (a *Agy) DefaultModel() (string, string) { return "gemini-3.8-flash", "high" }

func (a *Agy) ModelEfforts() map[string][]string {
	a.Models()
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.efforts
}
func (a *Agy) ContextWindow(model string) int {
	switch {
	case strings.HasPrefix(model, "claude"):
		return 200_000
	case strings.HasPrefix(model, "gpt-oss"):
		return 131_072
	}
	return 1_048_576
}

// Models asks `agy models` (cached for 10 minutes). Its slugs carry the effort
// ("gemini-4-argon-high"); agy also accepts the base name with --effort, which is
// what roundtable uses so effort is chosen independently of the model.
func (a *Agy) Models() []ModelOption {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.loaded.IsZero() && time.Since(a.loaded) < 10*time.Minute {
		return a.models
	}
	a.models, a.efforts = groupAgyModels(listOutput(a.cmd, "models"))
	a.loaded = time.Now()
	return a.models
}

var agySlugRe = regexp.MustCompile(`^(.+)-(low|medium|high|xhigh|max)$`)
var agyLabelEffortRe = regexp.MustCompile(`\s*\((Low|Medium|High|XHigh|Max)\)\s*$`)

// groupAgyModels turns "gemini-4-argon-high\tGemini 4 Argon (High)" lines into
// base models with their effort lists, keeping agy's order.
func groupAgyModels(lines []string) ([]ModelOption, map[string][]string) {
	rank := map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}
	var models []ModelOption
	efforts := map[string][]string{}
	for _, l := range lines {
		parts := strings.SplitN(l, "\t", 2)
		if len(parts) != 2 || strings.Contains(strings.TrimSpace(parts[0]), " ") {
			continue
		}
		slug, label := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		base, effort := slug, ""
		if m := agySlugRe.FindStringSubmatch(slug); m != nil {
			base, effort = m[1], m[2]
		}
		if _, seen := efforts[base]; !seen {
			models = append(models, ModelOption{ID: base, Label: agyLabelEffortRe.ReplaceAllString(label, "")})
			efforts[base] = nil
		}
		if effort != "" {
			efforts[base] = append(efforts[base], effort)
		}
	}
	for base, list := range efforts {
		sort.Slice(list, func(i, j int) bool { return rank[list[i]] < rank[list[j]] })
		efforts[base] = list
	}
	return models, efforts
}

// agyArgs maps the room mode onto agy flags.
// --disable-slash-commands is only used in write mode: agy ignores --mode plan
// while slash expansion is disabled ("--mode plan has no effect while slash
// command expansion is disabled"), and on 2026-10-17 a read-mode run with both
// flags created a file. Without it, plan mode refused and wrote a plan instead.
func agyArgs(o RunOptions) []string {
	args := []string{"--output-format", "stream-json", "--input-format", "stream-json"}
	for _, dir := range imageDirs(o.Images) {
		args = append(args, "--add-dir", dir)
	}
	if o.Mode == ModeWrite {
		args = append(args, "--disable-slash-commands")
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
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
	// The prompt goes through stdin (agyInput): agy 1.4 reads stream-json turns
	// there, so long turns no longer hit the Windows command-line limit.
	return append(args, "-p=")
}

// agyInput is one stream-json user turn (verified with agy 1.4.2 on 2026-10-19,
// including a 212KB prompt answered without truncation).
func agyInput(prompt string) string {
	b, err := json.Marshal(map[string]any{"event": "user", "message": map[string]string{"content": prompt}})
	if err != nil {
		panic(err) // a string map always marshals
	}
	return string(b) + "\n"
}

func (a *Agy) Run(ctx context.Context, o RunOptions, emit func(Event)) (Result, error) {
	if err := validateImages(o.Images); err != nil {
		return Result{}, err
	}
	path, err := resolveCommand(a.cmd)
	if err != nil {
		return Result{}, fmt.Errorf("agy CLI를 찾지 못함: %w", err)
	}
	started := time.Now()
	cmd := exec.Command(path, agyArgs(o)...)
	cmd.Dir = workDir(o)
	cmd.Env = os.Environ()
	p := newAgyParser()
	stderr, runErr := runLines(ctx, cmd, agyInput(o.Prompt), func(line string) { p.Line(line, emit) })
	fin := p.Finish(emit)
	res := Result{Text: fin.Text, SessionID: fin.SessionID, Usage: fin.Usage, Stderr: stderr}
	if l, ok := a.CurrentLimits(); ok && l.At >= started.UnixMilli() {
		x := ev(KindLimits)
		x.Meta = map[string]any{"limits": l}
		emit(x)
	}
	if err := finishError("agy", fin, runErr, stderr); err != nil {
		return res, err
	}
	if res.Text == "" {
		return res, fmt.Errorf("agy가 빈 응답을 반환함 (사용량 한도나 로그인 상태를 확인)")
	}
	return res, nil
}

type agyParser struct {
	conv      string
	text      strings.Builder
	response  string
	errText   string
	steps     map[string]map[string]any // step index -> usage of that model call
	lastStep  string
	duration  any
	completed bool
	toolSeen  map[string]bool
	subs      map[string]*agySub // conversation_id -> state
}

type agySub struct {
	logPath string
	offset  int64
	ended   bool
}

func newAgyParser() *agyParser {
	return &agyParser{toolSeen: map[string]bool{}, subs: map[string]*agySub{}, steps: map[string]map[string]any{}}
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
		p.completed = true
		p.response = str(r, "response")
		if str(r, "status") != "SUCCESS" {
			p.errText = firstNonEmpty(str(r, "error"), str(r, "status"))
		}
		// result.usage sums every call of the whole conversation (resumed turns
		// included), so this turn's usage is summed from its own steps in Finish.
		p.duration = r["duration_seconds"]
	}
}

func (p *agyParser) step(s map[string]any, emit func(Event)) {
	idx := fmt.Sprintf("s%v", s["step_index"])
	state := str(s, "state")
	switch str(s, "step_type") {
	case "agent_response":
		if u := obj(s, "usage"); u != nil {
			p.steps[idx], p.lastStep = u, idx
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
			r := takeTranscriptReader(f)
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
			putTranscriptReader(r)
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
	usage := p.turnUsage()
	if usage != nil {
		x := ev(KindUsage)
		x.Meta = usage
		emit(x)
	}
	return Final{Text: text, SessionID: p.conv, Err: p.errText, Usage: usage, Complete: p.completed}
}

// turnUsage sums this invocation's model calls. A step's input_tokens excludes
// cache_read_tokens, so the context the last call carried is their sum.
func (p *agyParser) turnUsage() map[string]any {
	if len(p.steps) == 0 {
		return nil
	}
	sum := map[string]float64{}
	for _, u := range p.steps {
		for _, k := range []string{"input_tokens", "output_tokens", "thinking_tokens", "cache_read_tokens", "total_tokens"} {
			n, _ := u[k].(float64)
			sum[k] += n
		}
	}
	usage := map[string]any{}
	for k, n := range sum {
		usage[k] = n
	}
	last := p.steps[p.lastStep]
	in, _ := last["input_tokens"].(float64)
	cached, _ := last["cache_read_tokens"].(float64)
	usage["context_tokens"] = in + cached
	if p.completed {
		usage["duration_seconds"] = p.duration
	}
	return usage
}
