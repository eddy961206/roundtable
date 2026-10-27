package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// With this variable set the test binary acts as `opencode run`: it prints the
// given stdout lines and stderr, then exits with the given code (see TestMain).
const fakeOpenCodeEnv = "ROUNDTABLE_FAKE_OPENCODE"

type fakeOpenCode struct {
	Stdout []string
	Stderr string
	Exit   int
}

func TestMain(m *testing.M) {
	if spec := os.Getenv(fakeOpenCodeEnv); spec != "" {
		var f fakeOpenCode
		if err := json.Unmarshal([]byte(spec), &f); err != nil {
			fmt.Fprintln(os.Stderr, "bad fake spec:", err)
			os.Exit(2)
		}
		for _, line := range f.Stdout {
			fmt.Println(line)
		}
		fmt.Fprint(os.Stderr, f.Stderr)
		os.Exit(f.Exit)
	}
	os.Exit(m.Run())
}

func runFakeOpenCode(t *testing.T, f fakeOpenCode) error {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(f)
	t.Setenv(fakeOpenCodeEnv, string(spec))
	_, err = NewOpenCode(exe).Run(context.Background(), RunOptions{Mode: ModeChat, ScratchDir: t.TempDir(), Model: "opencode/test-model"}, func(Event) {})
	return err
}

// Measured 2026-10-27: a free-tier limit ends `opencode run --format json` with
// exit 1 after this event; the turn error showed only "exit status 1".
func TestOpenCodeRunErrorEventExplainsExit(t *testing.T) {
	err := runFakeOpenCode(t, fakeOpenCode{Exit: 1, Stderr: "noise\n", Stdout: []string{
		`{"type":"step_start","sessionID":"ses_1","part":{}}`,
		`{"type":"error","timestamp":1793084400000,"sessionID":"ses_1","error":{"type":"provider.free-limit","message":"Free usage limit reached. Upgrade or wait.","status":429}}`,
	}})
	want := "OpenCode 오류(provider.free-limit, 429): Free usage limit reached. Upgrade or wait."
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
}

func TestOpenCodeRunExitWithoutErrorEventShowsStderr(t *testing.T) {
	err := runFakeOpenCode(t, fakeOpenCode{Exit: 1, Stderr: "one\ntwo\nthree\nfatal: provider not configured\n"})
	if err == nil || !strings.Contains(err.Error(), "OpenCode 실행이 끝나지 못했어: exit 1: ") ||
		!strings.Contains(err.Error(), "two | three | fatal: provider not configured") || strings.Contains(err.Error(), "one") {
		t.Fatalf("error = %v, want exit status and the last 3 stderr lines", err)
	}
}

func TestOpenCodeErrorEventText(t *testing.T) {
	for line, want := range map[string]string{
		`{"type":"error","error":{"type":"provider.auth","message":"bad key"}}`: "OpenCode 오류(provider.auth): bad key",
		`{"type":"error","error":{"message":"boom","status":500}}`:              "OpenCode 오류(500): boom",
		`{"type":"error","error":{"message":"plain"}}`:                          "OpenCode 오류: plain",
		`{"type":"error","error":{"type":"provider.free-limit","status":429}}`:  "OpenCode 오류(provider.free-limit, 429): OpenCode 실행 오류",
	} {
		p := newOpenCodeParser()
		p.Line(line, func(Event) {})
		if got := p.Finish(func(Event) {}).Err; got != want {
			t.Errorf("%s\n got %q\nwant %q", line, got, want)
		}
	}
}
