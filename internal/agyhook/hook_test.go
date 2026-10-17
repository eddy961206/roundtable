package agyhook

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sampleSettings = `{
  "model": "gemini-3.8-flash",
  "statusLine": {
    "type": "command",
    "command": "powershell -NoProfile -File C:/x/statusline.ps1",
    "enabled": true
  },
  "toolPermission": "always-proceed",
  "zeta": [1, 2]
}
`

func keysInOrder(t *testing.T, path string) []string {
	t.Helper()
	b, _ := os.ReadFile(path)
	o, err := parseOrdered(b)
	if err != nil {
		t.Fatal(err)
	}
	return o.keys
}

func TestInstallUninstallPreservesSettings(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte(sampleSettings), 0o600)
	cfg := filepath.Join(dir, "rt", "agy-statusline.json")
	out := filepath.Join(dir, "rt", "agy-status.json")

	cmd, err := Install(settings, `C:\Program Files\rt\roundtable.exe`, cfg, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmd, `"C:\Program Files\rt\roundtable.exe" agy-statusline --config `) {
		t.Fatalf("command not quoted: %s", cmd)
	}
	if got := strings.Join(keysInOrder(t, settings), ","); got != "model,statusLine,toolPermission,zeta" {
		t.Fatalf("key order changed: %s", got)
	}
	b, _ := os.ReadFile(settings)
	if !strings.Contains(string(b), `"toolPermission": "always-proceed"`) || !strings.Contains(string(b), Marker) {
		t.Fatalf("settings after install:\n%s", b)
	}
	var c Config
	cb, _ := os.ReadFile(cfg)
	json.Unmarshal(cb, &c)
	if c.Chain != "powershell -NoProfile -File C:/x/statusline.ps1" || c.Out != out || !c.HadStatus {
		t.Fatalf("config %+v", c)
	}
	if bk, _ := os.ReadFile(settings + ".roundtable-backup"); string(bk) != sampleSettings {
		t.Fatal("backup must be the original file")
	}
	if _, err := Install(settings, "x", cfg, out); err == nil {
		t.Fatal("second install must refuse")
	}

	if err := Uninstall(settings, cfg); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(settings)
	var a, o map[string]any
	json.Unmarshal(after, &a)
	json.Unmarshal([]byte(sampleSettings), &o)
	ab, _ := json.Marshal(a)
	ob, _ := json.Marshal(o)
	if !bytes.Equal(ab, ob) {
		t.Fatalf("uninstall must restore the original content:\n%s", after)
	}
	if got := strings.Join(keysInOrder(t, settings), ","); got != "model,statusLine,toolPermission,zeta" {
		t.Fatalf("key order after uninstall: %s", got)
	}
}

func TestInstallWithoutStatusLine(t *testing.T) {
	dir := t.TempDir()
	settings := filepath.Join(dir, "settings.json")
	os.WriteFile(settings, []byte("\xef\xbb\xbf{\"a\": 1}"), 0o600)
	cfg := filepath.Join(dir, "c.json")
	if _, err := Install(settings, "rt", cfg, filepath.Join(dir, "s.json")); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(settings, cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(keysInOrder(t, settings), ","); got != "a" {
		t.Fatalf("statusLine must be removed again, keys=%s", got)
	}
}

func TestRunTeesAndChains(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "agy-status.json")
	chain := "echo chained"
	if runtime.GOOS != "windows" {
		chain = "cat >/dev/null; echo chained"
	}
	cfg := filepath.Join(dir, "cfg.json")
	cb, _ := json.Marshal(Config{Out: out, Chain: chain})
	os.WriteFile(cfg, cb, 0o600)
	var stdout, stderr bytes.Buffer
	code := Run(cfg, strings.NewReader(`{"quota":{"gemini-5h":{"remaining_fraction":0.5}}}`), &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), "chained") {
		t.Fatalf("code=%d out=%q err=%q", code, stdout.String(), stderr.String())
	}
	b, err := os.ReadFile(out)
	if err != nil || !strings.Contains(string(b), `"remaining_fraction":0.5`) {
		t.Fatalf("status not stored: %s %v", b, err)
	}
}
