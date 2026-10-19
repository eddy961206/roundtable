package agent

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Codex runs `codex exec --json` and watches ~/.codex/sessions for sub-agent threads.
type Codex struct {
	cmd            string
	sessionsDir    string
	windowsSandbox string // "" keeps the user's config.toml choice
}

func NewCodex(cmd string) *Codex {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".codex", "sessions")
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		dir = filepath.Join(ch, "sessions")
	}
	return &Codex{cmd: cmd, sessionsDir: dir}
}

// SetWindowsSandbox pins [windows].sandbox for roundtable runs only. On
// 2026-10-19 the elevated setup refresh failed on every shell command while
// another process held a Codex runtime binary open; "unelevated" still kept
// workspace-write runs from writing outside the workspace.
func (c *Codex) SetWindowsSandbox(mode string) { c.windowsSandbox = mode }

func (c *Codex) ID() string      { return "codex" }
func (c *Codex) Label() string   { return "Codex" }
func (c *Codex) Command() string { return c.cmd }
func (c *Codex) Available() bool { return available(c.cmd) }
func (c *Codex) CanSteer() bool  { return false }

// model/list also advertises ultra orchestration, but forwarding it through
// exec's model_reasoning_effort has not been verified. Expose the verified range.
func (c *Codex) Efforts() []string                 { return []string{"low", "medium", "high", "xhigh", "max"} }
func (c *Codex) ModelEfforts() map[string][]string { return nil }
func (c *Codex) DefaultModel() (string, string)    { return "gpt-6.1-sol", "high" }
func (c *Codex) ContextWindow(model string) int    { return 475_000 }
func (c *Codex) Models() []ModelOption {
	// Verified with Codex 0.162.1 app-server model/list on 2026-10-18.
	return []ModelOption{
		{ID: "gpt-6.1-sol", Label: "GPT-6.1 Sol"},
		{ID: "gpt-6-astra", Label: "GPT-6 Astra"},
		{ID: "gpt-6-sol", Label: "GPT-6 Sol"},
		{ID: "gpt-6-luna", Label: "GPT-6 Luna"},
		{ID: "gpt-5.6-sol", Label: "GPT-5.6 Sol"},
		{ID: "gpt-5.6-terra", Label: "GPT-5.6 Terra"},
		{ID: "gpt-5.6-luna", Label: "GPT-5.6 Luna"},
		{ID: "gpt-daybreak-blue-latest", Label: "Daybreak Blue"},
	}
}

func codexArgs(o RunOptions, windowsSandbox string) []string {
	// `codex exec resume` accepts fewer flags than `codex exec` (no --color, no --cd),
	// and wants options before the positional session id.
	resume := o.SessionID != ""
	args := []string{"exec"}
	if resume {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--skip-git-repo-check", "-c", "suppress_unstable_features_warning=true")
	for _, im := range o.Images {
		args = append(args, "--image", im.Path)
	}
	if !resume {
		args = append(args, "--color", "never")
	}
	if o.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", o.Model))
	}
	if o.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", o.Effort))
	}
	if o.MCP != nil && o.Mode != ModeChat {
		// MCP servers run outside Codex's command sandbox, so a read-only
		// server can reach the network while shell commands stay offline.
		key := "mcp_servers." + o.MCP.Name
		quoted := make([]string, len(o.MCP.Args))
		for i, a := range o.MCP.Args {
			quoted[i] = tomlString(a)
		}
		// approval_policy="never" rejects MCP calls that need approval; the
		// server is read-only by construction, so its tools are pre-approved.
		args = append(args, "-c", key+".command="+tomlString(o.MCP.Command), "-c", key+".args=["+strings.Join(quoted, ",")+"]",
			"-c", key+`.default_tools_approval_mode="approve"`)
	}
	if windowsSandbox != "" {
		args = append(args, "-c", fmt.Sprintf("windows.sandbox=%q", windowsSandbox))
	}
	switch o.Mode {
	case ModeWrite:
		args = append(args, "-c", `sandbox_mode="workspace-write"`, "-c", `approval_policy="never"`)
		cache := tomlString(filepath.Join(o.ScratchDir, "codex", "go-build"))
		// Config overrides also work with exec resume (which has no --add-dir).
		// Explicit shell env injection survives Codex's inherited-env filters.
		args = append(args, "-c", "sandbox_workspace_write.writable_roots=["+cache+"]",
			"-c", "shell_environment_policy.set.GOCACHE="+cache)
	default:
		args = append(args, "-c", `sandbox_mode="read-only"`, "-c", `approval_policy="never"`)
	}
	if resume {
		return append(args, o.SessionID, "-")
	}
	return append(args, "--cd", workDir(o), "-")
}

func (c *Codex) Run(ctx context.Context, o RunOptions, emit func(Event)) (Result, error) {
	if err := validateImages(o.Images); err != nil {
		return Result{}, err
	}
	path, err := resolveCommand(c.cmd)
	if err != nil {
		return Result{}, fmt.Errorf("codex CLI를 찾지 못함: %w", err)
	}
	cmd := exec.Command(path, codexArgs(o, c.windowsSandbox)...)
	cmd.Dir = workDir(o)
	cmd.Env = os.Environ()
	if o.Mode == ModeWrite {
		if !filepath.IsAbs(o.ScratchDir) {
			return Result{}, fmt.Errorf("Codex 쓰기 모드에는 절대 경로 ScratchDir가 필요함")
		}
		cache := filepath.Join(o.ScratchDir, "codex", "go-build")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return Result{}, fmt.Errorf("Codex Go 빌드 캐시 생성: %w", err)
		}
		cmd.Env = append(cmd.Env, "GOCACHE="+cache)
	}
	p := newCodexParser()
	watcher := newRolloutWatcher(c.sessionsDir, time.Now().Add(-5*time.Second))
	stop := make(chan struct{})
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(1500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				if id := p.threadID(); id != "" {
					watcher.scan(id, emit)
				}
			}
		}
	}()
	stderr, runErr := runLines(ctx, cmd, o.Prompt, func(line string) { p.Line(line, emit) })
	close(stop)
	<-watchDone
	if id := p.threadID(); id != "" {
		watcher.scan(id, emit)
		watcher.finish(emit)
		if path := watcher.pathOf(id); path != "" {
			codexLimitsFromRollout(path, p, emit)
		}
	}
	fin := p.Finish(emit)
	res := Result{Text: fin.Text, SessionID: fin.SessionID, Usage: fin.Usage, Stderr: stderr}
	return res, finishError("codex", fin, runErr, filterCodexStderr(stderr))
}

func filterCodexStderr(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "rmcp::transport") || strings.Contains(l, "Reading additional input from stdin") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.Join(keep, "\n")
}

type codexParser struct {
	threadMu  sync.RWMutex
	thread    string
	parts     []string
	errText   string
	usage     map[string]any
	names     map[string]string
	context   int
	window    int
	completed bool
}

func newCodexParser() *codexParser { return &codexParser{names: map[string]string{}} }

func (p *codexParser) threadID() string {
	p.threadMu.RLock()
	defer p.threadMu.RUnlock()
	return p.thread
}

func (p *codexParser) Line(line string, emit func(Event)) {
	m := parseLine(line)
	if m == nil {
		return
	}
	typ := str(m, "type")
	switch typ {
	case "thread.started":
		p.threadMu.Lock()
		p.thread = str(m, "thread_id")
		p.threadMu.Unlock()
		x := ev(KindSession)
		x.Text = p.thread
		emit(x)
	case "turn.completed":
		p.completed = true
		p.usage = obj(m, "usage")
	case "turn.failed":
		p.errText = firstNonEmpty(str(obj(m, "error"), "message"), "turn failed")
	case "error":
		p.errText = firstNonEmpty(str(m, "message"), str(obj(m, "error"), "message"), "error")
	case "item.started", "item.completed":
		p.item(typ == "item.completed", obj(m, "item"), emit)
	}
}

func (p *codexParser) item(done bool, it map[string]any, emit func(Event)) {
	id := str(it, "id")
	switch str(it, "type") {
	case "agent_message":
		if !done {
			return
		}
		t := str(it, "text")
		if strings.TrimSpace(t) == "" {
			return
		}
		p.parts = append(p.parts, t)
		x := ev(KindText)
		x.Text = t
		emit(x)
	case "reasoning":
		if done {
			if t := str(it, "text"); t != "" {
				x := ev(KindThinking)
				x.Text = clip(t, 4000)
				emit(x)
			}
		}
	case "command_execution":
		if !done {
			x := ev(KindToolCall)
			x.ID, x.Name, x.Input = id, "shell", str(it, "command")
			p.names[id] = "shell"
			emit(x)
			return
		}
		x := ev(KindToolResult)
		x.ID, x.Name, x.Output = id, "shell", clip(str(it, "aggregated_output"), maxCapture)
		x.Meta = map[string]any{"exit_code": it["exit_code"], "status": str(it, "status")}
		if _, known := p.names[id]; !known {
			x.Input = str(it, "command")
		}
		emit(x)
	case "file_change":
		if done {
			x := ev(KindToolResult)
			x.ID, x.Name, x.Output = id, "file_change", compactJSON(it["changes"])
			x.Meta = map[string]any{"status": str(it, "status")}
			emit(x)
		}
	case "mcp_tool_call":
		name := str(it, "server") + "/" + str(it, "tool")
		if !done {
			x := ev(KindToolCall)
			x.ID, x.Name, x.Input = id, name, compactJSON(it["arguments"])
			emit(x)
			return
		}
		x := ev(KindToolResult)
		x.ID, x.Name, x.Output = id, name, contentText(obj(it, "result")["content"])
		if e := it["error"]; e != nil {
			x.Meta = map[string]any{"error": compactJSON(e)}
		}
		emit(x)
	case "web_search":
		if done {
			x := ev(KindToolResult)
			x.ID, x.Name, x.Input = id, "web_search", str(it, "query")
			emit(x)
		}
	case "collab_tool_call":
		x := ev(KindStatus)
		tool := str(it, "tool")
		recv := arr(it, "receiver_thread_ids")
		state := "시작"
		if done {
			state = "완료"
		}
		x.Text = fmt.Sprintf("서브에이전트 협업 %s %s", tool, state)
		x.Meta = map[string]any{"tool": tool, "receivers": recv, "prompt": str(it, "prompt")}
		emit(x)
	case "error":
		msg := str(it, "message")
		if strings.Contains(msg, "Under-development features") || strings.Contains(msg, "skill descriptions were shortened") {
			return
		}
		if strings.Contains(msg, "Code Mode is unavailable") {
			x := ev(KindStatus)
			x.Text = "Codex code-mode host 없음 (셸 도구로 진행)"
			emit(x)
			return
		}
		x := ev(KindError)
		x.Text = msg
		emit(x)
	case "todo_list":
		x := ev(KindStatus)
		x.Text = "할 일 목록 갱신"
		x.Meta = map[string]any{"items": it["items"]}
		emit(x)
	}
}

func (p *codexParser) Finish(emit func(Event)) Final {
	if p.usage != nil {
		if p.context > 0 {
			p.usage["context_tokens"] = p.context
		}
		if p.window > 0 {
			p.usage["context_window"] = p.window
		}
		x := ev(KindUsage)
		x.Meta = p.usage
		emit(x)
	}
	return Final{Text: strings.TrimSpace(strings.Join(p.parts, "\n\n")), SessionID: p.thread, Err: p.errText, Usage: p.usage, Complete: p.completed}
}

// codexLimitsFromRollout reads the newest token_count event of the turn's own
// rollout file: it carries the account rate limits and the context window.
func codexLimitsFromRollout(path string, p *codexParser, emit func(Event)) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var last map[string]any
	var observedAt int64
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		line := sc.Text()
		if !strings.Contains(line, `"token_count"`) {
			continue
		}
		m := parseLine(line)
		if m == nil || str(m, "type") != "event_msg" {
			continue
		}
		if pl := obj(m, "payload"); str(pl, "type") == "token_count" {
			last = pl
			observedAt = 0
			if ts, err := time.Parse(time.RFC3339Nano, str(m, "timestamp")); err == nil {
				observedAt = ts.UnixMilli()
			}
		}
	}
	if last == nil {
		return
	}
	if info := obj(last, "info"); info != nil {
		if w, ok := info["model_context_window"].(float64); ok {
			p.window = int(w)
		}
		if lu := obj(info, "last_token_usage"); lu != nil {
			in, _ := lu["input_tokens"].(float64)
			p.context = int(in)
		}
	}
	rl := obj(last, "rate_limits")
	if rl == nil {
		return
	}
	l := Limits{Provider: "codex", At: observedAt, Source: "codex_rollout", Plan: str(rl, "plan_type")}
	for _, key := range []string{"primary", "secondary"} {
		w := obj(rl, key)
		if w == nil {
			continue
		}
		used, hasUsed := w["used_percent"].(float64)
		if !hasUsed || used < 0 || used > 100 {
			continue
		}
		mins, _ := w["window_minutes"].(float64)
		reset, _ := w["resets_at"].(float64)
		l.Windows = append(l.Windows, LimitWindow{Label: windowLabel(int(mins)), UsedPercent: used, ResetsAt: int64(reset)})
	}
	if cr := obj(rl, "credits"); cr != nil {
		if unlimited, _ := cr["unlimited"].(bool); unlimited {
			l.Credits = "무제한"
		} else if b := str(cr, "balance"); b != "" {
			l.Credits = b
			if strings.Contains(b, ".") {
				l.Credits = strings.TrimRight(strings.TrimRight(b, "0"), ".")
			}
		}
	}
	if t := str(rl, "rate_limit_reached_type"); t != "" {
		l.Note = "한도 도달: " + t
	}
	x := ev(KindLimits)
	x.Meta = map[string]any{"limits": l}
	emit(x)
}

func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return "한도"
	case minutes%(60*24) == 0:
		return fmt.Sprintf("%d일", minutes/(60*24))
	case minutes%60 == 0:
		return fmt.Sprintf("%d시간", minutes/60)
	}
	return fmt.Sprintf("%d분", minutes)
}

// tomlString quotes s as a TOML literal string when possible ('...' keeps
// Windows backslashes as-is), otherwise as a basic string with escapes.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	return strconv.Quote(s)
}
