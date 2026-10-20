package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// NativeSession is an argv plan for opening an existing CLI conversation.
// It contains no prompt and never selects a provider's most recent session.
type NativeSession struct {
	Provider    string            `json:"provider"`
	SessionID   string            `json:"session_id"`
	Executable  string            `json:"executable"`
	Args        []string          `json:"args"`
	Directory   string            `json:"directory"`
	Environment map[string]string `json:"environment,omitempty"`
}

// NativeSessionCommand preserves the room's model and permission flags when
// handing a saved session to its native interactive CLI. It does not launch it.
func NativeSessionCommand(prov Provider, o RunOptions) (NativeSession, error) {
	if prov == nil {
		return NativeSession{}, fmt.Errorf("공급자를 찾지 못함")
	}
	if o.SessionID == "" || strings.TrimSpace(o.SessionID) != o.SessionID || strings.HasPrefix(o.SessionID, "-") || strings.ContainsAny(o.SessionID, "\x00\r\n") {
		return NativeSession{}, fmt.Errorf("다시 열 수 있는 세션 ID가 없음")
	}
	if o.Mode != ModeChat && o.Mode != ModeRead && o.Mode != ModeWrite {
		return NativeSession{}, fmt.Errorf("알 수 없는 권한 모드: %s", o.Mode)
	}
	dir := workDir(o)
	if dir == "" {
		return NativeSession{}, fmt.Errorf("세션 작업 폴더가 없음")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return NativeSession{}, err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return NativeSession{}, fmt.Errorf("세션 작업 폴더를 찾지 못함: %s", dir)
	}
	plan := NativeSession{Provider: prov.ID(), SessionID: o.SessionID, Directory: dir}
	switch prov.ID() {
	case "claude":
		plan.Args = []string{"--resume", o.SessionID, "--strict-mcp-config", "--settings", claudeSandboxSettings}
		if o.Model != "" {
			plan.Args = append(plan.Args, "--model", o.Model)
		}
		if o.Effort != "" {
			plan.Args = append(plan.Args, "--effort", o.Effort)
		}
		switch o.Mode {
		case ModeRead:
			plan.Args = append(plan.Args, "--tools", "Read,Grep,Glob,Agent,Task,WebSearch,WebFetch", "--add-dir", dir)
		case ModeWrite:
			plan.Args = append(plan.Args, "--permission-mode", "acceptEdits", "--add-dir", dir, "--allowedTools", "Bash,PowerShell")
		default:
			plan.Args = append(plan.Args, "--tools", "")
		}
	case "codex":
		c, ok := prov.(*Codex)
		if !ok {
			return NativeSession{}, fmt.Errorf("Codex 샌드박스 설정을 읽을 수 없음")
		}
		plan.Args = []string{"resume", "--include-non-interactive", "--cd", dir}
		plan.Args = append(plan.Args, codexPermissionArgs(o.Mode, c.windowsSandbox)...)
		if o.Model != "" {
			plan.Args = append(plan.Args, "-c", fmt.Sprintf("model=%q", o.Model))
		}
		if o.Effort != "" {
			plan.Args = append(plan.Args, "-c", fmt.Sprintf("model_reasoning_effort=%q", o.Effort))
		}
		plan.Args = append(plan.Args, o.SessionID)
	case "agy":
		plan.Args = []string{"--conversation", o.SessionID}
		if o.Model != "" {
			plan.Args = append(plan.Args, "--model", o.Model)
		}
		if o.Effort != "" {
			plan.Args = append(plan.Args, "--effort", o.Effort)
		}
		if o.Mode == ModeWrite {
			plan.Args = append(plan.Args, "--mode", "accept-edits")
		} else {
			plan.Args = append(plan.Args, "--mode", "plan")
		}
		if o.Mode != ModeChat {
			plan.Args = append(plan.Args, "--add-dir", dir)
		}
	case "opencode":
		o.Workspace, o.ScratchDir = dir, dir
		configuration, err := openCodePolicy(o)
		if err != nil {
			return NativeSession{}, err
		}
		plan.Environment = map[string]string{"OPENCODE_CONFIG_CONTENT": configuration, "PWD": dir}
		plan.Args = []string{"mini", "--standalone", "--session", o.SessionID, "--agent", openCodeAgent(o.Mode), "--replay"}
	default:
		return NativeSession{}, fmt.Errorf("원본 세션 열기를 지원하지 않는 공급자: %s", prov.ID())
	}
	plan.Executable, err = resolveCommand(prov.Command())
	if err != nil {
		return NativeSession{}, err
	}
	return plan, nil
}
