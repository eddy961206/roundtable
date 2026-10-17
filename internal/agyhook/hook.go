// Package agyhook installs roundtable as a pass-through in front of the user's
// Antigravity statusLine command, so the quota JSON agy already produces can be
// shown in roundtable. It edits one key of ~/.gemini/antigravity-cli/settings.json,
// keeps every other key in place and order, and can restore the original.
package agyhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"roundtable/internal/agent"
)

// Marker identifies a command line written by Install.
const Marker = "agy-statusline"

// Config is stored next to the status file and remembers what to chain to.
type Config struct {
	Out       string `json:"out"`        // status file roundtable reads
	Chain     string `json:"chain"`      // the user's previous statusLine command ("" = none)
	HadStatus bool   `json:"had_status"` // settings had a statusLine object before install
	PrevRaw   string `json:"prev_raw"`   // the original statusLine JSON, restored on uninstall
}

// SettingsPath is agy's user settings file.
func SettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), nil
}

// Run is the statusLine command itself: save stdin, then hand the same bytes
// to the chained command and pass its output through unchanged.
func Run(configPath string, stdin io.Reader, stdout, stderr io.Writer) int {
	payload, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "roundtable agy-statusline:", err)
		return 1
	}
	var cfg Config
	b, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Fprintln(stderr, "roundtable agy-statusline: 설정 없음:", err)
		return 1
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		fmt.Fprintln(stderr, "roundtable agy-statusline: 설정 손상:", err)
		return 1
	}
	if err := agent.WriteAgyStatus(cfg.Out, payload, time.Now()); err != nil {
		fmt.Fprintln(stderr, "roundtable agy-statusline:", err)
	}
	if cfg.Chain == "" {
		return 0
	}
	cmd := shellCommand(cfg.Chain)
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, "roundtable agy-statusline:", err)
		return 1
	}
	return 0
}

func shellCommand(line string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd.exe")
		setRawCmdLine(cmd, "/d /s /c \""+line+"\"")
		return cmd
	}
	return exec.Command("/bin/sh", "-c", line)
}

// Install points agy's statusLine at `<exe> agy-statusline --config <cfgPath>`.
func Install(settingsPath, exe, cfgPath, out string) (string, error) {
	doc, err := readSettings(settingsPath)
	if err != nil {
		return "", err
	}
	cfg := Config{Out: out}
	if raw, ok := doc.get("statusLine"); ok {
		cfg.HadStatus = true
		cfg.PrevRaw = string(raw)
		var sl struct {
			Command string `json:"command"`
			Enabled *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(raw, &sl); err != nil {
			return "", fmt.Errorf("statusLine 해석 실패: %w", err)
		}
		if strings.Contains(sl.Command, Marker) {
			return "", fmt.Errorf("이미 설치되어 있음: %s", sl.Command)
		}
		if sl.Enabled == nil || *sl.Enabled {
			cfg.Chain = sl.Command
		}
	}
	cb, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(cfgPath, cb, 0o600); err != nil {
		return "", err
	}
	command := fmt.Sprintf("%s %s --config %s", quote(exe), Marker, quote(cfgPath))
	sl, err := json.Marshal(map[string]any{"type": "command", "command": command, "enabled": true})
	if err != nil {
		return "", err
	}
	doc.set("statusLine", sl)
	backup := settingsPath + ".roundtable-backup"
	orig, err := os.ReadFile(settingsPath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(backup, orig, 0o600); err != nil {
		return "", err
	}
	return command, writeSettings(settingsPath, doc)
}

// Uninstall restores the statusLine that was there before Install.
func Uninstall(settingsPath, cfgPath string) error {
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("설치 기록 없음: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return err
	}
	doc, err := readSettings(settingsPath)
	if err != nil {
		return err
	}
	raw, ok := doc.get("statusLine")
	if !ok || !strings.Contains(string(raw), Marker) {
		return fmt.Errorf("현재 statusLine 이 roundtable 훅이 아님 — 손대지 않음")
	}
	if cfg.HadStatus {
		doc.set("statusLine", json.RawMessage(cfg.PrevRaw))
	} else {
		doc.del("statusLine")
	}
	if err := writeSettings(settingsPath, doc); err != nil {
		return err
	}
	return os.Remove(cfgPath)
}

func quote(s string) string {
	if strings.ContainsAny(s, " \t\"") {
		return "\"" + s + "\""
	}
	return s
}

func readSettings(path string) (*orderedObject, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &orderedObject{}, nil
		}
		return nil, err
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	return parseOrdered(b)
}

func writeSettings(path string, doc *orderedObject) error {
	b, err := doc.marshalIndent()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
