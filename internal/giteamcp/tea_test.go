package giteamcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The test executable acts as the launcher; no real login or network is used.
func TestMain(m *testing.M) {
	mode := os.Getenv("ROUNDTABLE_TEA_TEST_MODE")
	// MCP hosts need not forward arbitrary parent environment variables.
	// This synthetic login also permits an actual Codex/MCP timeout probe.
	if mode == "" && len(os.Args) > 3 && os.Args[1] == "api" && os.Args[3] == "roundtable-timeout-fixture" {
		mode = "wait"
	}
	switch mode {
	case "ok":
		fmt.Print(`{"number":11}`)
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, "secret-must-not-escape")
		os.Exit(17)
	case "wait":
		// A descendant inherits the output pipes just as tea-vault.py does.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "ROUNDTABLE_TEA_TEST_MODE=child")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(18)
		}
		_ = child.Wait()
		os.Exit(0)
	case "child":
		// Bounded even if tree termination is broken, so a failing test cannot
		// leave a permanent process or test-binary lock behind.
		if path := os.Getenv("ROUNDTABLE_TEA_TEST_HEARTBEAT"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				os.Exit(19)
			}
			defer f.Close()
			for range 150 {
				_, _ = f.WriteString(".")
				time.Sleep(20 * time.Millisecond)
			}
		} else {
			time.Sleep(30 * time.Second)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestTeaProcessDeadlineAndErrors(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fetch := TeaFetcher(exe, "test-login")
	t.Setenv("ROUNDTABLE_TEA_TEST_MODE", "ok")
	body, err := fetch(context.Background(), "/repos/Acme/app/pulls/11")
	if err != nil || string(body) != `{"number":11}` {
		t.Fatalf("success: %s %v", body, err)
	}
	t.Setenv("ROUNDTABLE_TEA_TEST_MODE", "fail")
	_, err = fetch(context.Background(), "/repos/Acme/app/pulls/11")
	if err == nil || !strings.Contains(err.Error(), "exit 17") || strings.Contains(err.Error(), "secret-must-not-escape") {
		t.Fatalf("unsafe or missing failure: %v", err)
	}
	t.Setenv("ROUNDTABLE_TEA_TEST_MODE", "wait")
	heartbeat := filepath.Join(t.TempDir(), "heartbeat")
	t.Setenv("ROUNDTABLE_TEA_TEST_HEARTBEAT", heartbeat)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := fetch(ctx, "/repos/Acme/app/pulls/11")
		done <- err
	}()
	for {
		if b, _ := os.ReadFile(heartbeat); len(b) > 0 {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("launcher exited before descendant started: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	start := time.Now()
	cancel()
	err = <-done
	if !errors.Is(err, context.Canceled) || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("launcher/descendant deadline not bounded: %v after %s", err, time.Since(start))
	}
	before, _ := os.ReadFile(heartbeat)
	time.Sleep(100 * time.Millisecond)
	after, _ := os.ReadFile(heartbeat)
	if len(before) != len(after) {
		t.Fatal("descendant kept running after launcher cancellation")
	}
}

func TestToolBudgetCoversAllFetchesAndContinuesServing(t *testing.T) {
	var deadlines []time.Time
	s, err := New([]string{"Acme/app"}, func(ctx context.Context, path string) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("missing tool deadline")
		}
		deadlines = append(deadlines, deadline)
		if strings.HasSuffix(path, "/pulls/11") {
			return []byte(`{"number":11}`), nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	s.timeout = 30 * time.Millisecond
	var out bytes.Buffer
	err = s.Serve(strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"gitea_pull","arguments":{"repo":"Acme/app","number":11}}}
{"jsonrpc":"2.0","id":2,"method":"ping"}
`), &out)
	if err != nil || !strings.Contains(out.String(), `"isError":true`) || !strings.Contains(out.String(), "30ms") || !strings.Contains(out.String(), `"id":2,"jsonrpc":"2.0","result":{}`) {
		t.Fatalf("deadline must be reported and next request served: %s %v", out.String(), err)
	}
	if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("budget reset between PR and files: %v", deadlines)
	}
}
