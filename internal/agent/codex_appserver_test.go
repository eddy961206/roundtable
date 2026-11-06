package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeCodexEnv = "ROUNDTABLE_FAKE_CODEX_RPC"

type fakeCodexRPC struct {
	Scenario string
	Capture  string
	History  string
}

// The test executable implements the actual bidirectional stdio boundary.
// No vendor login, inference or network is involved in these fault cases.
func runFakeCodexRPC(raw string) {
	var spec fakeCodexRPC
	if json.Unmarshal([]byte(raw), &spec) != nil {
		os.Exit(2)
	}
	if len(os.Args) < 2 || os.Args[1] != "app-server" {
		fmt.Fprintln(os.Stderr, "expected app-server, exec is not supported")
		os.Exit(2)
	}
	if spec.Scenario == "missing" {
		fmt.Fprintln(os.Stderr, "error: unrecognized subcommand 'app-server'")
		os.Exit(2)
	}
	enc := json.NewEncoder(os.Stdout)
	write := func(v any) { _ = enc.Encode(v) }
	notify := func(method string, params any) { write(map[string]any{"method": method, "params": params}) }
	thread, turn := "thread-test", "turn-test"
	complete := func(text string) {
		notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "shell", "type": "commandExecution", "command": "probe", "aggregatedOutput": text, "exitCode": 0, "status": "completed"}})
		notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "answer", "type": "agentMessage", "text": text, "phase": "final_answer"}})
		notify("thread/tokenUsage/updated", map[string]any{"threadId": thread, "turnId": turn, "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 120, "cachedInputTokens": 30, "outputTokens": 10, "totalTokens": 130}, "last": map[string]any{"inputTokens": 70}, "modelContextWindow": 475000}})
		notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "completed", "items": []any{}}})
	}
	requests := []string{"item/commandExecution/requestApproval", "item/fileChange/requestApproval", "item/permissions/requestApproval", "item/tool/requestUserInput", "mcpServer/elicitation/request", "currentTime/read"}
	nextRequest := 0
	scan := bufio.NewScanner(os.Stdin)
	scan.Buffer(make([]byte, 64*1024), maxLineBytes)
	for scan.Scan() {
		var p codexPacket
		if json.Unmarshal(scan.Bytes(), &p) != nil {
			os.Exit(3)
		}
		if spec.Capture != "" {
			f, _ := os.OpenFile(spec.Capture, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if f != nil {
				fmt.Fprintln(f, scan.Text())
				f.Close()
			}
		}
		reply := func(result any) { write(map[string]any{"id": p.ID, "result": result}) }
		failure := func(code int, message string) {
			write(map[string]any{"id": p.ID, "error": map[string]any{"code": code, "message": message}})
		}
		var params map[string]any
		_ = json.Unmarshal(p.Params, &params)
		switch p.Method {
		case "initialize":
			reply(map[string]string{"userAgent": "codex/0.166.0-test"})
		case "initialized":
		case "thread/start", "thread/resume", "thread/fork":
			if p.Method == "thread/resume" {
				thread = str(params, "threadId")
			}
			if p.Method == "thread/fork" {
				thread = "fork-test"
			}
			reply(map[string]any{"thread": map[string]any{"id": thread}})
			notify("thread/started", map[string]any{"thread": map[string]any{"id": thread}})
		case "turn/start":
			reply(map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
			notify("turn/started", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn}})
			switch spec.Scenario {
			case "steer", "reject", "unsupported", "lost":
				notify("item/started", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "shell", "type": "commandExecution", "command": "probe", "status": "inProgress"}})
			case "unknown":
				write(map[string]any{"id": "server-1", "method": "unknown/operation", "params": map[string]any{}})
			case "requests":
				write(map[string]any{"id": "server-1", "method": requests[0], "params": map[string]any{}})
			case "warning":
				fmt.Fprintln(os.Stderr, "WARN rmcp::transport: pipe closed during shutdown\nWARN app_server: a new harmless warning")
				notify("deprecationNotice", map[string]any{"summary": "old field is deprecated"})
				complete("finished")
			default:
				complete("finished")
			}
		case "turn/steer":
			if str(params, "expectedTurnId") != turn {
				failure(-32600, "wrong expectedTurnId")
				continue
			}
			switch spec.Scenario {
			case "reject":
				failure(-32600, "no active turn to steer")
				complete("original")
			case "unsupported":
				failure(-32601, "method not found: turn/steer")
				complete("original")
			default:
				client := str(params, "clientUserMessageId")
				if spec.History != "" {
					_ = os.WriteFile(spec.History, []byte(client), 0600)
				}
				if spec.Scenario == "lost" {
					os.Exit(3)
				}
				reply(map[string]any{"turnId": turn})
				items := arr(params, "input")
				text := ""
				if len(items) > 0 {
					text, _ = items[0].(map[string]any)["text"].(string)
				}
				notify("item/completed", map[string]any{"threadId": thread, "turnId": turn, "item": map[string]any{"id": "user-steer", "type": "userMessage", "clientId": client, "content": items}})
				complete(text)
			}
		case "thread/items/list":
			client, _ := os.ReadFile(spec.History)
			data := []any{}
			if len(client) > 0 {
				data = append(data, map[string]any{"turnId": turn, "item": map[string]any{"type": "userMessage", "clientId": string(client)}})
			}
			reply(map[string]any{"data": data, "nextCursor": nil})
		case "turn/interrupt":
			reply(map[string]any{})
			notify("turn/completed", map[string]any{"threadId": thread, "turn": map[string]any{"id": turn, "status": "interrupted"}})
		case "":
			if string(p.ID) != `"server-1"` {
				os.Exit(4)
			}
			if spec.Scenario == "unknown" {
				if p.Error == nil || p.Error.Code != -32601 {
					os.Exit(5)
				}
				complete("unknown answered")
			} else {
				if len(p.Result) == 0 {
					os.Exit(6)
				}
				nextRequest++
				if nextRequest < len(requests) {
					write(map[string]any{"id": "server-1", "method": requests[nextRequest], "params": map[string]any{}})
				} else {
					complete("requests answered")
				}
			}
		default:
			failure(-32601, "method not found")
		}
	}
	if spec.Scenario == "linger" {
		time.Sleep(20 * time.Second)
	}
	os.Exit(0)
}

func fakeCodex(t *testing.T, scenario string) (*Codex, fakeCodexRPC) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	spec := fakeCodexRPC{Scenario: scenario, Capture: filepath.Join(dir, "rpc.jsonl"), History: filepath.Join(dir, "history")}
	b, _ := json.Marshal(spec)
	t.Setenv(fakeCodexEnv, string(b))
	t.Setenv("CODEX_HOME", dir)
	return NewCodex(exe), spec
}

func TestCodexAppServerSteerReceipt(t *testing.T) {
	c, spec := fakeCodex(t, "steer")
	inputs := make(chan SteerInput, 1)
	var states []string
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	res, err := c.Run(ctx, RunOptions{Mode: ModeChat, ScratchDir: t.TempDir(), Steer: inputs, Model: "test-model", Effort: "low"}, func(e Event) {
		if e.Kind == KindToolCall {
			inputs <- SteerInput{ID: "message-1", Text: "new instruction"}
		}
		if e.Receipt != nil {
			states = append(states, e.Receipt.State)
		}
	})
	if err != nil || res.Text != "new instruction" || res.SessionID != "thread-test" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(res.Receipts) != 1 || res.Receipts[0].ID != "message-1" || res.Receipts[0].State != InputAccepted || strings.Join(states, ",") != "sending,accepted" {
		t.Fatalf("receipts=%+v states=%v", res.Receipts, states)
	}
	if res.Usage["input_tokens"] != 120.0 || res.Usage["cached_input_tokens"] != 30.0 {
		t.Fatal(res.Usage)
	}
	data, _ := os.ReadFile(spec.Capture)
	if strings.Count(string(data), `"method":"turn/start"`) != 1 || !strings.Contains(string(data), `"expectedTurnId":"turn-test"`) {
		t.Fatalf("%s", data)
	}
}

func TestCodexSteerFailureReceipts(t *testing.T) {
	for _, scenario := range []string{"reject", "unsupported", "lost"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := fakeCodex(t, scenario)
			inputs := make(chan SteerInput, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			res, err := c.Run(ctx, RunOptions{Mode: ModeChat, ScratchDir: t.TempDir(), Steer: inputs}, func(e Event) {
				if e.Kind == KindToolCall {
					inputs <- SteerInput{ID: "input-1", Text: "correction"}
				}
			})
			if len(res.Receipts) != 1 {
				t.Fatalf("%+v %v", res, err)
			}
			if scenario == "lost" {
				if err == nil || res.Receipts[0].State != InputUncertain {
					t.Fatalf("%+v %v", res, err)
				}
				receipt, readErr := c.ResolveSteer(ctx, res.Receipts[0])
				if readErr != nil || receipt.State != InputAccepted {
					t.Fatalf("history receipt=%+v %v", receipt, readErr)
				}
			} else if err != nil || res.Receipts[0].State != InputRejected {
				t.Fatalf("%+v %v", res, err)
			}
		})
	}
}

func TestCodexServerRequestsAlwaysReceiveAnAnswer(t *testing.T) {
	for _, scenario := range []string{"unknown", "requests", "warning"} {
		t.Run(scenario, func(t *testing.T) {
			c, _ := fakeCodex(t, scenario)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			var events []Event
			res, err := c.Run(ctx, RunOptions{Mode: ModeChat, ScratchDir: t.TempDir()}, func(e Event) { events = append(events, e) })
			if err != nil || res.Text == "" {
				t.Fatalf("%+v %v", res, err)
			}
			if len(kinds(events, KindStatus)) == 0 || len(kinds(events, KindError)) != 0 {
				t.Fatalf("request/warning must be visible without becoming a turn error: %+v", events)
			}
		})
	}
}

func TestCodexWithoutAppServerFailsExplicitly(t *testing.T) {
	c, _ := fakeCodex(t, "missing")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.Run(ctx, RunOptions{Mode: ModeChat, ScratchDir: t.TempDir()}, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "app-server") {
		t.Fatalf("error=%v", err)
	}
}
