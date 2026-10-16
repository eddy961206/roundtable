package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

type echoProvider struct{}

func (echoProvider) ID() string                  { return "claude" }
func (echoProvider) Label() string               { return "Claude" }
func (echoProvider) Command() string             { return "claude" }
func (echoProvider) Models() []agent.ModelOption { return nil }
func (echoProvider) Available() bool             { return true }
func (echoProvider) CanSteer() bool              { return true }
func (echoProvider) ContextWindow(string) int    { return 10 }
func (echoProvider) Run(ctx context.Context, o agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	emit(agent.Event{Kind: agent.KindToolCall, ID: "t1", Name: "Read", Input: `{"f":"x"}`})
	emit(agent.Event{Kind: agent.KindToolResult, ID: "t1", Output: "ok"})
	emit(agent.Event{Kind: agent.KindTextDelta, Text: "echo"})
	return agent.Result{Text: "echo", SessionID: "s"}, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, _ := room.NewStore(t.TempDir())
	hub, err := room.NewHub(store, agent.NewRegistry(echoProvider{}), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	web := fstest.MapFS{"index.html": {Data: []byte("<html>ok</html>")}}
	ts := httptest.NewServer(New(hub, agent.NewRegistry(echoProvider{}), web))
	t.Cleanup(ts.Close)
	return ts
}

func call(t *testing.T, ts *httptest.Server, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, ts.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestAPIFlow(t *testing.T) {
	ts := newTestServer(t)
	if code, _ := call(t, ts, "GET", "/", nil); code != 200 {
		t.Fatalf("index %d", code)
	}
	code, prov := call(t, ts, "GET", "/api/providers", nil)
	if code != 200 || prov["providers"] == nil {
		t.Fatalf("providers %d %v", code, prov)
	}
	code, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "api", "mode": "chat", "participants": []string{"claude"}})
	if code != 201 {
		t.Fatalf("create %d %v", code, rm)
	}
	id := rm["id"].(string)

	// SSE: open before posting so the message events are observed live
	req, _ := http.NewRequest("GET", ts.URL+"/api/rooms/"+id+"/stream", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type %s", ct)
	}

	if code, m := call(t, ts, "POST", "/api/rooms/"+id+"/messages", map[string]any{"text": "hi"}); code != 201 || m["kind"] != "user" {
		t.Fatalf("post %d %v", code, m)
	}
	seen := map[string]bool{}
	sc := bufio.NewScanner(res.Body)
	deadline := time.Now().Add(4 * time.Second)
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev struct {
			Type string         `json:"type"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev) != nil {
			continue
		}
		seen[ev.Type] = true
		if ev.Type == "message" && ev.Data["kind"] == "agent" && ev.Data["status"] == "done" {
			break
		}
	}
	for _, want := range []string{"message", "trace", "delta", "participant"} {
		if !seen[want] {
			t.Errorf("sse missing %s: %v", want, seen)
		}
	}

	code, snap := call(t, ts, "GET", "/api/rooms/"+id, nil)
	msgs := snap["messages"].([]any)
	if code != 200 || len(msgs) != 2 {
		t.Fatalf("snapshot %d %v", code, snap)
	}
	agentMsg := msgs[1].(map[string]any)
	if agentMsg["text"] != "echo" {
		t.Fatalf("agent text %v", agentMsg)
	}
	req2, _ := http.NewRequest("GET", ts.URL+"/api/rooms/"+id+"/trace/"+agentMsg["id"].(string), nil)
	r2, _ := http.DefaultClient.Do(req2)
	var events []map[string]any
	_ = json.NewDecoder(r2.Body).Decode(&events)
	r2.Body.Close()
	if len(events) < 3 || events[0]["kind"] != "tool_call" {
		t.Fatalf("trace %v", events)
	}
	r3, _ := http.Get(ts.URL + "/api/rooms/" + id + "/export.md")
	md, _ := io.ReadAll(r3.Body)
	r3.Body.Close()
	if !strings.Contains(string(md), "echo") {
		t.Fatal("export missing text")
	}
	if code, _ := call(t, ts, "PATCH", "/api/rooms/"+id, map[string]any{"title": "renamed"}); code != 200 {
		t.Fatal("patch failed")
	}
	if code, _ := call(t, ts, "DELETE", "/api/rooms/"+id, nil); code != 200 {
		t.Fatal("delete failed")
	}
	if code, _ := call(t, ts, "GET", "/api/rooms/"+id, nil); code != 404 {
		t.Fatal("room should be gone")
	}
}
