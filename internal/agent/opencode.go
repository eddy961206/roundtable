package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OpenCode runs a private v2 CLI server, while the CLI retains its own account
// configuration and native session database. No API key is read or copied here.
type OpenCode struct {
	cmd     string
	catalog openCodeCatalog
}

func NewOpenCode(cmd string) *OpenCode { return &OpenCode{cmd: cmd} }
func (o *OpenCode) ID() string         { return "opencode" }
func (o *OpenCode) Label() string      { return "OpenCode" }
func (o *OpenCode) Command() string    { return o.cmd }
func (o *OpenCode) Available() bool    { return available(o.cmd) }
func (o *OpenCode) CanSteer() bool     { return false }
func (o *OpenCode) CanFork() bool      { return true }

// A worker model is always an explicit user choice, never a public-catalog default.
func (o *OpenCode) DefaultModel() (string, string) { return "", "" }

func openCodeAgent(mode string) string { return "roundtable-" + mode }

type openCodePermission struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// V2 appends custom-agent permissions after global rules. Unknown tools remain
// denied and subagents stay unavailable. File tools are limited to the selected
// workspace. Write mode also allows the v2 "shell" tool, which is not an OS
// sandbox: the user accepted unsandboxed shell for write seats so every
// provider can build and test (Claude 2026-10-19, OpenCode 2026-10-27).
func openCodePolicy(o RunOptions) (string, error) {
	if o.Mode != ModeChat && o.Mode != ModeRead && o.Mode != ModeWrite {
		return "", fmt.Errorf("OpenCode 권한 모드가 올바르지 않아: %s", o.Mode)
	}
	rules := []openCodePermission{{"*", "*", "deny"}}
	if o.Mode != ModeChat {
		for _, action := range []string{"read", "glob", "grep"} {
			rules = append(rules, openCodePermission{action, "*", "allow"})
		}
		if o.Mode == ModeWrite {
			rules = append(rules, openCodePermission{"edit", "*", "allow"}, openCodePermission{"shell", "*", "allow"})
		}
		for _, action := range []string{"read", "edit"} {
			for _, pattern := range []string{"../*", "/*", "?:/*", "*.env", "*.env.*"} {
				rules = append(rules, openCodePermission{action, pattern, "deny"})
			}
		}
	}
	configuration := map[string]any{
		"warming": false,
		"agents": map[string]any{openCodeAgent(o.Mode): map[string]any{
			"description": "Roundtable workspace worker",
			"mode":        "primary",
			"permissions": rules,
		}},
	}
	b, err := json.Marshal(configuration)
	return string(b), err
}

func openCodeModel(model, effort string) (string, error) {
	provider, id, ok := strings.Cut(model, "/")
	if !ok || provider == "" || id == "" || strings.ContainsAny(model, "\x00\r\n") {
		return "", fmt.Errorf("OpenCode 모델을 공급사/모델 형식으로 선택해")
	}
	if strings.Contains(model, "#") {
		if effort != "" {
			return "", fmt.Errorf("모델 ID의 #추론수준과 별도 추론 수준을 함께 지정할 수 없어")
		}
		return model, nil
	}
	if effort != "" {
		model += "#" + effort
	}
	return model, nil
}

func (o *OpenCode) Run(ctx context.Context, opts RunOptions, emit func(Event)) (Result, error) {
	path, err := resolveCommand(o.cmd)
	if err != nil {
		return Result{}, fmt.Errorf("OpenCode CLI 실행 파일을 찾지 못했어")
	}
	model, err := openCodeModel(opts.Model, opts.Effort)
	if err != nil {
		return Result{}, err
	}
	configuration, err := openCodePolicy(opts)
	if err != nil {
		return Result{}, err
	}
	dir, err := filepath.Abs(workDir(opts))
	if err != nil {
		return Result{}, err
	}
	args := []string{"run", "--standalone", "--format", "json", "--thinking", "--agent", openCodeAgent(opts.Mode), "--model", model}
	switch {
	case opts.SessionID != "":
		args = append(args, "--session", opts.SessionID)
	case opts.ForkSession != "":
		args = append(args, "--session", opts.ForkSession, "--fork")
	default:
		args = append(args, "--title", "Roundtable · "+model)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = append(envWithout("OPENCODE_CONFIG_CONTENT", "PWD"), "OPENCODE_CONFIG_CONTENT="+configuration, "PWD="+dir)
	p := newOpenCodeParser()
	started := time.Now().UnixMilli()
	stderr, runErr := runLines(ctx, cmd, opts.Prompt, func(line string) { p.Line(line, emit) })
	if runErr == nil && ctx.Err() == nil && p.errText == "" {
		// The CLI's final projection restores text, but can omit step_finish.
		// Stored messages are the completion/usage authority for every invocation.
		verifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err := p.readTurn(verifyCtx, path, opts, started)
		cancel()
		if err != nil {
			p.completed = false
			p.errText = err.Error()
		}
	}
	fin := p.Finish(emit)
	res := Result{Text: fin.Text, SessionID: fin.SessionID, Usage: fin.Usage, Stderr: stderr}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if runErr != nil {
		// The CLI reports provider failures (limits, auth) as an error event
		// before exiting non-zero; that event, not the exit status, is the cause.
		if fin.Err != "" {
			return res, fmt.Errorf("%s", fin.Err)
		}
		if tail := lastLines(stderr, 3); tail != "" {
			return res, fmt.Errorf("OpenCode 실행이 끝나지 못했어: %w: %s", runErr, tail)
		}
		return res, fmt.Errorf("OpenCode 실행이 끝나지 못했어: %w", runErr)
	}
	return res, finishError("OpenCode", fin, nil, "")
}

func openCodeEnv() []string {
	// Metadata discovery does not inherit a caller's temporary agent overlay.
	return envWithout("OPENCODE_CONFIG_CONTENT")
}
