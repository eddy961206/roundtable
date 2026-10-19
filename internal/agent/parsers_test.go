package agent

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func feed(t *testing.T, p lineParser, fixture string) ([]Event, Final) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	emit := func(e Event) { events = append(events, e) }
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		p.Line(sc.Text(), emit)
	}
	fin := p.Finish(emit)
	return events, fin
}

func kinds(events []Event, kind string) []Event {
	var out []Event
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestClaudeSubagentStream(t *testing.T) {
	events, fin := feed(t, newClaudeParser(), "claude_subagent.jsonl")
	if fin.SessionID == "" {
		t.Fatal("session id missing")
	}
	if fin.Err != "" {
		t.Fatalf("unexpected error: %s", fin.Err)
	}
	if !strings.Contains(fin.Text, "파일 5개") {
		t.Fatalf("final text should be the top-level answer, got %q", fin.Text)
	}
	starts := kinds(events, KindSubagentStart)
	if len(starts) != 1 || starts[0].Name != "Explore" || starts[0].Input == "" {
		t.Fatalf("subagent_start wrong: %+v", starts)
	}
	parent := starts[0].Parent
	var sawTool, sawText, sawDenied bool
	for _, e := range kinds(events, KindSubagentMsg) {
		if e.Parent != parent {
			t.Fatalf("subagent_msg with foreign parent %q", e.Parent)
		}
		switch {
		case e.Role == "tool_call" && e.Name == "Glob":
			sawTool = true
		case e.Role == "assistant" && strings.Contains(e.Text, "파일 5개"):
			sawText = true
		case e.Role == "status" && strings.Contains(e.Text, "권한 거부"):
			sawDenied = true
		}
	}
	if !sawTool || !sawText || !sawDenied {
		t.Fatalf("subagent inner events incomplete: tool=%v text=%v denied=%v", sawTool, sawText, sawDenied)
	}
	ends := kinds(events, KindSubagentEnd)
	if len(ends) != 1 || !strings.Contains(ends[0].Output, "파일 5개") {
		t.Fatalf("subagent_end wrong: %+v", ends)
	}
	if len(kinds(events, KindToolCall)) != 0 {
		t.Fatalf("Agent tool must not appear as a plain tool_call")
	}
	// Every subagent message repeats the model; only the first one is news.
	// The CLI does not report the subagent's effort, so none is invented.
	metas := kinds(events, KindSubagentMeta)
	if len(metas) != 1 || metas[0].Parent != parent || metas[0].Meta["model"] != "claude-sonnet-5-5" || metas[0].Meta["effort"] != nil {
		t.Fatalf("subagent_meta wrong: %+v", metas)
	}
	if fin.Usage["session_cost_usd"] == nil {
		t.Fatal("usage missing cost")
	}
	if c, ok := fin.Usage["context_tokens"].(int); !ok || c <= 0 {
		t.Fatalf("context_tokens missing: %v", fin.Usage["context_tokens"])
	}
}

func TestClaudePartialDeltasMatchFinal(t *testing.T) {
	events, fin := feed(t, newClaudeParser(), "claude_partial.jsonl")
	var b strings.Builder
	for _, e := range kinds(events, KindTextDelta) {
		b.WriteString(e.Text)
	}
	if strings.TrimSpace(b.String()) != fin.Text {
		t.Fatalf("deltas %q != final %q", b.String(), fin.Text)
	}
	lim := kinds(events, KindLimits)
	if len(lim) == 0 {
		t.Fatal("rate_limit_event should produce a limits event")
	}
	l := lim[0].Meta["limits"].(Limits)
	if len(l.Windows) < 2 || l.Windows[0].Label != "5시간" {
		t.Fatalf("limits windows wrong: %+v", l)
	}
}

func TestCodexToolStream(t *testing.T) {
	first := newCodexParser()
	first.before = map[string]any{} // rollout read: a new thread has no earlier totals
	events, fin := feed(t, first, "codex_tool.jsonl")
	if fin.SessionID != "01a143d0-5491-7f11-8241-62db0dcc8673" {
		t.Fatalf("thread id %q", fin.SessionID)
	}
	if !strings.Contains(fin.Text, "hello probe") || !strings.Contains(fin.Text, "요약할게") {
		t.Fatalf("final text should join agent messages: %q", fin.Text)
	}
	calls, results := kinds(events, KindToolCall), kinds(events, KindToolResult)
	if len(calls) != 1 || calls[0].Name != "shell" || !strings.Contains(calls[0].Input, "Get-Content") {
		t.Fatalf("tool_call wrong: %+v", calls)
	}
	if len(results) != 1 || results[0].ID != calls[0].ID || !strings.Contains(results[0].Output, "hello probe") {
		t.Fatalf("tool_result wrong: %+v", results)
	}
	if len(kinds(events, KindError)) != 0 {
		t.Fatal("under-development warning must be suppressed")
	}
	if fin.Usage["input_tokens"] == nil {
		t.Fatal("usage missing")
	}
}

func TestCodexCollabStream(t *testing.T) {
	events, _ := feed(t, newCodexParser(), "codex_collab.jsonl")
	var saw bool
	for _, e := range kinds(events, KindStatus) {
		if strings.Contains(e.Text, "협업 wait") {
			saw = true
		}
	}
	if !saw {
		t.Fatal("collab_tool_call should surface as status")
	}
}

func TestCodexChildRollout(t *testing.T) {
	path := filepath.Join("testdata", "codex_child_rollout.jsonl")
	meta := readSessionMeta(path)
	if meta == nil || meta.parent != "01a143d1-3003-7e93-87b6-88454c387ce7" || !strings.Contains(meta.name, "Archimedes") {
		t.Fatalf("session meta wrong: %+v", meta)
	}
	f, _ := os.Open(path)
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	for sc.Scan() {
		events = append(events, rolloutEvents(meta.id, sc.Text())...)
	}
	var sawCall, sawOut, sawAnswer, sawDeveloper bool
	for _, e := range events {
		if e.Parent != meta.id {
			t.Fatalf("wrong parent %q", e.Parent)
		}
		switch {
		case e.Role == "tool_call" && e.Name == "exec":
			sawCall = true
		case e.Role == "tool_result" && strings.Contains(e.Output, "hello probe"):
			sawOut = true
		case e.Role == "assistant" && strings.Contains(e.Text, "hello probe"):
			sawAnswer = true
		case e.Role == "developer":
			sawDeveloper = true
		}
	}
	if !sawCall || !sawOut || !sawAnswer || sawDeveloper {
		t.Fatalf("rollout events: call=%v out=%v answer=%v developer=%v", sawCall, sawOut, sawAnswer, sawDeveloper)
	}
	if len(kinds(events, KindSubagentEnd)) != 1 {
		t.Fatal("task_complete should end the subagent")
	}
	metas := kinds(events, KindSubagentMeta)
	if len(metas) != 1 || metas[0].Meta["model"] != "gpt-6.1-sol" || metas[0].Meta["effort"] != "high" {
		t.Fatalf("subagent_meta from turn_context wrong: %+v", metas)
	}
}

func TestCodexRolloutMetaOnlyOnChange(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("testdata", "codex_child_rollout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var turnContext string
	for _, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, `"type":"turn_context"`) {
			turnContext = l
		}
	}
	// A second turn with the same settings, then a turn at another effort.
	lowered := strings.Replace(turnContext, `"effort":"high"`, `"effort":"low"`, 1)
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	body := strings.TrimRight(string(src), "\n") + "\n" + turnContext + "\n" + lowered + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var events []Event
	(&rolloutWatcher{}).tail(&rolloutFile{path: path, id: "child"}, func(e Event) { events = append(events, e) })
	metas := kinds(events, KindSubagentMeta)
	if len(metas) != 2 || metas[0].Meta["effort"] != "high" || metas[1].Meta["effort"] != "low" || metas[1].Parent != "child" {
		t.Fatalf("subagent_meta should follow changes only: %+v", metas)
	}
}

// subagent_meta always names the model and carries effort only when the CLI
// reported it, so the UI can read meta.model without guarding against gaps.
func TestCodexSubagentMetaContract(t *testing.T) {
	if got := rolloutEvents("child", `{"type":"turn_context","payload":{"effort":"high"}}`); len(got) != 0 {
		t.Fatalf("turn_context without model must not emit: %+v", got)
	}
	got := rolloutEvents("child", `{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":""}}`)
	if len(got) != 1 || got[0].Meta["model"] != "gpt-6-astra" {
		t.Fatalf("subagent_meta wrong: %+v", got)
	}
	if _, ok := got[0].Meta["effort"]; ok {
		t.Fatalf("unreported effort must be absent: %+v", got[0].Meta)
	}
}

func TestCodexLimitsFromRollout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rollout.jsonl")
	line := `{"timestamp":"x","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1},"last_token_usage":{"input_tokens":25802,"cached_input_tokens":13440,"output_tokens":9},"model_context_window":475000},"rate_limits":{"primary":{"used_percent":18.0,"window_minutes":10080,"resets_at":1792649591},"secondary":{"used_percent":3.5,"window_minutes":300,"resets_at":1792150000},"credits":{"has_credits":true,"unlimited":false,"balance":"62500.0000000000"},"plan_type":"pro"}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newCodexParser()
	var events []Event
	codexLimitsFromRollout(path, p, func(e Event) { events = append(events, e) })
	if p.window != 475000 || p.context != 25802 {
		t.Fatalf("context parse wrong: window=%d ctx=%d", p.window, p.context)
	}
	lim := kinds(events, KindLimits)
	if len(lim) != 1 {
		t.Fatal("limits event missing")
	}
	l := lim[0].Meta["limits"].(Limits)
	if len(l.Windows) != 2 || l.Windows[0].Label != "7일" || l.Windows[1].Label != "5시간" || l.Plan != "pro" || l.Credits != "62500" {
		t.Fatalf("limits wrong: %+v", l)
	}
}

// turn.completed carries the thread's totals; a resumed turn reports only the
// difference from the totals recorded before its turn_context.
func TestCodexResumedTurnUsageIsPerTurn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	tc := func(total int) string {
		return fmt.Sprintf(`{"timestamp":"x","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d},"last_token_usage":{"input_tokens":%d}}}}`+"\n", total, total/2, total/100, total/3)
	}
	rollout := `{"timestamp":"x","type":"turn_context","payload":{}}` + "\n" + tc(30000) +
		`{"timestamp":"x","type":"turn_context","payload":{}}` + "\n" + tc(50000) + tc(64000)
	if err := os.WriteFile(path, []byte(rollout), 0o600); err != nil {
		t.Fatal(err)
	}
	p := newCodexParser()
	p.Line(`{"type":"turn.completed","usage":{"input_tokens":64000,"cached_input_tokens":32000,"output_tokens":640}}`, func(Event) {})
	codexLimitsFromRollout(path, p, func(Event) {})
	fin := p.Finish(func(Event) {})
	if fin.Usage["input_tokens"] != 34000.0 || fin.Usage["cached_input_tokens"] != 17000.0 || fin.Usage["output_tokens"] != 340.0 || fin.Usage["context_tokens"] != 21333 {
		t.Fatalf("usage %v", fin.Usage)
	}

	unread := newCodexParser()
	unread.Line(`{"type":"turn.completed","usage":{"input_tokens":64000}}`, func(Event) {})
	if fin := unread.Finish(func(Event) {}); fin.Usage != nil {
		t.Fatalf("thread totals without the rollout must not be reported as turn usage: %v", fin.Usage)
	}
}

func TestAgySubagentStream(t *testing.T) {
	events, fin := feed(t, newAgyParser(), "agy_subagent.jsonl")
	if fin.SessionID == "" || fin.Err != "" {
		t.Fatalf("session=%q err=%q", fin.SessionID, fin.Err)
	}
	if !strings.Contains(fin.Text, "hello probe") {
		t.Fatalf("final text: %q", fin.Text)
	}
	starts := kinds(events, KindSubagentStart)
	if len(starts) != 1 || starts[0].Parent != "edcedd7d-3d16-47df-b8bc-fb8fab6756a8" || !strings.Contains(starts[0].Name, "File Reader") {
		t.Fatalf("subagent_start wrong: %+v", starts)
	}
	if lp, _ := starts[0].Meta["log"].(string); !strings.HasSuffix(lp, "transcript.jsonl") {
		t.Fatalf("log path not resolved: %v", starts[0].Meta["log"])
	}
	// Neither subagent_info nor the child transcript names the child's model.
	if metas := kinds(events, KindSubagentMeta); len(metas) != 0 {
		t.Fatalf("agy subagent model must not be guessed: %+v", metas)
	}
	calls := kinds(events, KindToolCall)
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	if !strings.Contains(strings.Join(names, ","), "view_file") {
		t.Fatalf("tool calls: %v", names)
	}
	if fin.Usage["total_tokens"] == nil {
		t.Fatal("usage missing")
	}
}

func TestAgyToolStreamDeltas(t *testing.T) {
	events, fin := feed(t, newAgyParser(), "agy_tool.jsonl")
	var b strings.Builder
	for _, e := range kinds(events, KindTextDelta) {
		b.WriteString(e.Text)
	}
	if strings.TrimSpace(b.String()) != fin.Text {
		t.Fatalf("deltas %q != final %q", b.String(), fin.Text)
	}
	results := kinds(events, KindToolResult)
	if len(results) != 1 || results[0].Name != "view_file" {
		t.Fatalf("tool_result wrong: %+v", results)
	}
}

func TestAgyTranscript(t *testing.T) {
	f, err := os.Open(filepath.Join("testdata", "agy_child_transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		events = append(events, agyTranscriptEvents("conv", sc.Text())...)
	}
	var roles []string
	for _, e := range events {
		roles = append(roles, e.Role)
	}
	want := "system,tool_call,tool_result,tool_call,tool_result,assistant"
	if got := strings.Join(roles, ","); got != want {
		t.Fatalf("roles %s != %s", got, want)
	}
	if !strings.HasPrefix(events[0].Text, "Please read the file") {
		t.Fatalf("system message not trimmed: %q", events[0].Text)
	}
}

func TestArgsPerMode(t *testing.T) {
	ws := `D:\ws`
	c := strings.Join(claudeArgs(RunOptions{Mode: ModeRead, Workspace: ws, Model: "opus", Effort: "high", SessionID: "sid"}), " ")
	for _, want := range []string{"--input-format stream-json", "--resume sid", "--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch", "--add-dir " + ws, "--model opus", "--effort high"} {
		if !strings.Contains(c, want) {
			t.Errorf("claude read args missing %q: %s", want, c)
		}
	}
	if strings.Contains(c, "dangerously") {
		t.Error("claude args must never bypass permissions")
	}
	x := codexArgs(RunOptions{Mode: ModeWrite, Workspace: ws, Model: "gpt-6.1-sol", Effort: "high"}, "")
	xs := strings.Join(x, " ")
	for _, want := range []string{"exec --json", `model="gpt-6.1-sol"`, `model_reasoning_effort="high"`, `sandbox_mode="workspace-write"`, "--cd " + ws + " -"} {
		if !strings.Contains(xs, want) {
			t.Errorf("codex write args missing %q: %s", want, xs)
		}
	}
	if strings.Contains(xs, "bypass") {
		t.Error("codex args must never bypass the sandbox")
	}
	xr := strings.Join(codexArgs(RunOptions{Mode: ModeRead, Workspace: ws, SessionID: "tid"}, ""), " ")
	if !strings.HasPrefix(xr, "exec resume --json") || !strings.HasSuffix(xr, " tid -") || !strings.Contains(xr, `sandbox_mode="read-only"`) || strings.Contains(xr, "--cd") || strings.Contains(xr, "--color") {
		t.Errorf("codex resume args wrong: %s", xr)
	}
	a := agyArgs(RunOptions{Mode: ModeChat, Model: "gemini-3.8-flash-high", Prompt: "hi", SessionID: "cid"})
	as := strings.Join(a, " ")
	for _, want := range []string{"--mode plan", "--model gemini-3.8-flash-high", "--conversation cid", "--sandbox", "--input-format stream-json"} {
		if !strings.Contains(as, want) {
			t.Errorf("agy args missing %q: %s", want, as)
		}
	}
	if a[len(a)-1] != "-p=" {
		t.Error("agy prompt must come through stdin, not the command line")
	}
}

func TestFileURLToPath(t *testing.T) {
	got := fileURLToPath("file:///C:/Users/x/.gemini/t/transcript.jsonl")
	if !strings.HasSuffix(got, "transcript.jsonl") || strings.HasPrefix(got, "/C:") {
		t.Fatalf("got %q", got)
	}
}

func TestResolveCommandPrefersNpmVendoredExe(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows PATH semantics")
	}
	dir := t.TempDir()
	shim := filepath.Join(dir, "codex.cmd")
	os.WriteFile(shim, []byte("@echo off"), 0o600)
	vendored := filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-x64", "vendor", "x86_64-pc-windows-msvc", "bin")
	os.MkdirAll(vendored, 0o700)
	os.WriteFile(filepath.Join(vendored, "codex.exe"), []byte("x"), 0o600)
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "codex.exe"), []byte("y"), 0o600)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+other)
	got, err := resolveCommand("codex")
	if err != nil || got != filepath.Join(vendored, "codex.exe") {
		t.Fatalf("got %q err %v", got, err)
	}
	t.Setenv("PATH", other)
	if got, _ := resolveCommand("codex"); got != filepath.Join(other, "codex.exe") {
		t.Fatalf("plain exe lookup broken: %q", got)
	}
	if _, err := resolveCommand("definitely-missing-cli"); err == nil {
		t.Fatal("missing command must error")
	}
}

// agy's result.usage sums the whole conversation; a resumed turn must report
// only its own calls, and the context is the last call's input plus cache.
func TestAgyResumedTurnUsageIsPerTurn(t *testing.T) {
	_, fin := feed(t, newAgyParser(), "agy_resumed.jsonl")
	want := map[string]float64{"input_tokens": 2900, "output_tokens": 50, "thinking_tokens": 5, "cache_read_tokens": 42400, "total_tokens": 45355, "context_tokens": 22900, "duration_seconds": 3.5}
	for k, v := range want {
		if fin.Usage[k] != v {
			t.Errorf("%s = %v, want %v", k, fin.Usage[k], v)
		}
	}
}

// OpenCode's tokens.input excludes the cache; the context is input plus cache.
func TestOpenCodeContextIncludesCache(t *testing.T) {
	p := newOpenCodeParser()
	for _, line := range []string{
		`{"type":"step_finish","sessionID":"ses_1","part":{"reason":"tool-calls","cost":0.001,"tokens":{"input":1027,"output":5,"cache":{"read":0,"write":0}}}}`,
		`{"type":"step_finish","sessionID":"ses_1","part":{"reason":"stop","cost":0.002,"tokens":{"input":197,"output":2,"cache":{"read":1024,"write":10}}}}`,
	} {
		p.Line(line, func(Event) {})
	}
	fin := p.Finish(func(Event) {})
	if fin.Usage["context_tokens"] != 1231.0 || fin.Usage["input_tokens"] != 1224.0 || fin.Usage["cache_read_input_tokens"] != 1024.0 {
		t.Fatalf("usage %v", fin.Usage)
	}
}
