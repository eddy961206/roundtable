package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// exitGrace is how long a CLI process may stay after it reported the turn
// complete (Codex turn/completed, agy result) before roundtable ends it.
var exitGrace = 60 * time.Second

// Codex runs one app-server process per room turn and watches child rollouts.
type Codex struct {
	cmd            string
	sessionsDir    string
	windowsSandbox string // "" disables OS sandboxing; explicit value opts back in
}

func NewCodex(cmd string) *Codex {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".codex", "sessions")
	if ch := os.Getenv("CODEX_HOME"); ch != "" {
		dir = filepath.Join(ch, "sessions")
	}
	return &Codex{cmd: cmd, sessionsDir: dir}
}

// SetWindowsSandbox opts roundtable runs back into OS sandboxing. Empty keeps
// the 2026-10-20 user decision: no OS sandbox, including interactive resumes.
func (c *Codex) SetWindowsSandbox(mode string) { c.windowsSandbox = mode }

func (c *Codex) ID() string           { return "codex" }
func (c *Codex) Label() string        { return "Codex" }
func (c *Codex) Command() string      { return c.cmd }
func (c *Codex) Available() bool      { return available(c.cmd) }
func (c *Codex) CanSteer() bool       { return true }
func (c *Codex) CanFork() bool        { return true }
func (c *Codex) CanSteerImages() bool { return true }

// model/list also advertises ultra orchestration, but forwarding it through
// turn/start effort has not been verified. Expose the verified range.
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
	args := []string{"app-server"}
	// Plugins add their skills to the instructions, and Codex re-sends that whole
	// list (about 10K tokens) whenever its shortened form changes, which happened on
	// most resumed turns (measured 2026-10-20). Roundtable runs use no plugin tools.
	args = append(args, "-c", "suppress_unstable_features_warning=true", "-c", "features.plugins=false")
	if o.Model != "" {
		args = append(args, "-c", fmt.Sprintf("model=%q", o.Model))
	}
	if o.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", o.Effort))
	}
	if o.MCP != nil && o.Mode != ModeChat {
		// The fixed GET-only MCP also works when shell sandboxing is enabled.
		key := "mcp_servers." + o.MCP.Name
		quoted := make([]string, len(o.MCP.Args))
		for i, a := range o.MCP.Args {
			quoted[i] = tomlString(a)
		}
		// approval_policy="never" rejects MCP calls that need approval; the
		// server is read-only by construction, so its tools are pre-approved.
		args = append(args, "-c", key+".command="+tomlString(o.MCP.Command), "-c", key+".args=["+strings.Join(quoted, ",")+"]",
			"-c", key+`.default_tools_approval_mode="approve"`,
			"-c", key+`.enabled=true`, "-c", key+`.required=true`,
			"-c", key+`.startup_timeout_sec=10`, "-c", key+`.tool_timeout_sec=25`)
	}
	args = append(args, codexPermissionArgs(o.Mode, windowsSandbox)...)
	if o.Mode == ModeWrite {
		cache := tomlString(filepath.Join(o.ScratchDir, "codex", "go-build"))
		// Explicit shell env injection survives Codex's inherited-env filters.
		args = append(args, "-c", "shell_environment_policy.set.GOCACHE="+cache)
		if windowsSandbox != "" {
			args = append(args, "-c", "sandbox_workspace_write.writable_roots=["+cache+"]")
		}
	}
	return args
}

// Keep headless and interactive permission policies identical. Codex has no
// separate read-only tool allow-list, so default read is an instruction only.
func codexPermissionArgs(mode, windowsSandbox string) []string {
	policy := "danger-full-access"
	args := []string{"-c", `approval_policy="never"`}
	if windowsSandbox != "" {
		args = append(args, "-c", fmt.Sprintf("windows.sandbox=%q", windowsSandbox))
		policy = "read-only"
		if mode == ModeWrite {
			policy = "workspace-write"
		}
	}
	return append(args, "-c", fmt.Sprintf("sandbox_mode=%q", policy))
}

func codexPrompt(o RunOptions) string {
	if o.MCP == nil || o.MCP.Name != "gitea" || o.Mode == ModeChat || o.SessionID != "" || o.ForkSession != "" {
		return o.Prompt
	}
	return "Gitea 조회는 gitea_list·gitea_search·gitea_issue·gitea_pull·gitea_pull_diff 도구를 사용하세요. " +
		"도구가 없거나 조회에 실패하면 사유를 보고하고 같은 실패를 반복하지 마세요.\n\n" + o.Prompt
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

// threadTotals reads a saved thread's total_token_usage from its rollout file.
func (c *Codex) threadTotals(id string) (map[string]any, error) {
	paths, err := filepath.Glob(filepath.Join(c.sessionsDir, "*", "*", "*", "rollout-*-"+id+".jsonl"))
	if err != nil || len(paths) != 1 {
		return nil, fmt.Errorf("포크할 Codex 세션 %s의 기록 파일을 찾지 못함", id)
	}
	f, err := os.Open(paths[0])
	if err != nil {
		return nil, fmt.Errorf("포크할 Codex 세션 기록: %w", err)
	}
	defer f.Close()
	total := map[string]any{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		if line := sc.Text(); strings.Contains(line, `"total_token_usage"`) {
			if t := obj(obj(obj(parseLine(line), "payload"), "info"), "total_token_usage"); t != nil {
				total = t
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("포크할 Codex 세션 기록: %w", err)
	}
	return total, nil
}

// codexLimitsFromRollout reads account limits and context only. The turn's
// usage baseline is captured before turn/start and must never move here.
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
