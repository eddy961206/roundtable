package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	"roundtable/internal/server"
)

// localCLI stands in for this PC's CLI: it records what it ran.
type localCLI struct {
	mu   sync.Mutex
	opts []agent.RunOptions
}

func (*localCLI) ID() string                        { return "codex" }
func (*localCLI) Label() string                     { return "Codex" }
func (*localCLI) Command() string                   { return "codex" }
func (*localCLI) Models() []agent.ModelOption       { return nil }
func (*localCLI) ModelEfforts() map[string][]string { return nil }
func (*localCLI) Efforts() []string                 { return nil }
func (*localCLI) DefaultModel() (string, string)    { return "m", "" }
func (*localCLI) Available() bool                   { return true }
func (*localCLI) CanSteer() bool                    { return false }
func (*localCLI) CanFork() bool                     { return false }
func (*localCLI) ContextWindow(string) int          { return 0 }
func (c *localCLI) Run(_ context.Context, o agent.RunOptions, _ func(agent.Event)) (agent.Result, error) {
	c.mu.Lock()
	c.opts = append(c.opts, o)
	c.mu.Unlock()
	return agent.Result{Text: "이 PC에서 답함", SessionID: "local-session", Usage: map[string]any{"input_tokens": 1000.0, "cached_input_tokens": 800.0, "output_tokens": 5.0}}, nil
}

// One turn end to end: the host room calls the remote seat, this worker runs
// it with its own CLI at no more than its own mode, and the answer lands in
// the room with the CLI session and usage.
func TestWorkerRunsOneTurn(t *testing.T) {
	store, _ := room.NewStore(t.TempDir())
	hub, err := room.NewHub(store, agent.NewRegistry(room.NewRemoteProvider()), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	r, err := hub.CreateRoom(room.RoomSpec{Title: "작업실", Kind: room.KindWork, Mode: agent.ModeWrite, Workspace: t.TempDir(), ParticipantSpecs: []room.ParticipantSpec{{Provider: room.RemoteProvider, Label: "eddy"}}})
	if err != nil {
		t.Fatal(err)
	}
	seat := r.Participants[0].ID
	token, err := hub.IssueRemoteToken(r.ID, seat)
	if err != nil {
		t.Fatal(err)
	}
	allow, _ := server.ParseAllow(server.DefaultCommunityAllow)
	ts := httptest.NewServer(server.NewCommunity(hub, allow, fstest.MapFS{"people.html": {Data: []byte("<html>people</html>")}}))
	t.Cleanup(ts.Close)
	cli := &localCLI{}
	cfg := Config{URL: ts.URL, Token: token, Provider: cli, Workspace: t.TempDir(), Scratch: t.TempDir(), MaxMode: agent.ModeRead, Once: true, Client: &http.Client{}, Log: log.New(io.Discard, "", 0)}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	// The first poll marks the worker online; then the seat is called.
	deadline := time.Now().Add(3 * time.Second)
	for {
		snap, _ := hub.Snapshot(r.ID)
		if snap.Room.Participants[0].RemoteSeenAt > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker never polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := hub.PostUser(r.ID, "원격 좌석, 확인해 줘", seat); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the worker did not finish its turn")
	}
	cli.mu.Lock()
	ran := cli.opts[0]
	cli.mu.Unlock()
	if ran.Mode != agent.ModeRead || ran.Workspace != cfg.Workspace || !strings.Contains(ran.Prompt, "원격 좌석, 확인해 줘") || !strings.Contains(ran.Prompt, "read 모드로 실행") {
		t.Fatalf("a write room must run at the worker's read cap in its own folder: %+v", ran)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		snap, _ := hub.Snapshot(r.ID)
		p := snap.Room.Participants[0]
		var reply *room.Message
		for _, m := range snap.Messages {
			if m.Kind == "agent" {
				reply = m
			}
		}
		if reply != nil && reply.Status == "done" {
			if reply.Text != "이 PC에서 답함" || p.SessionID != "local-session" || p.InputAll != 1000 {
				t.Fatalf("reply %+v seat %+v", reply, p)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no reply in the room: %+v", snap.Messages)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// PR #4 review (P2): when the result upload fails on the network, the worker
// keeps the finished answer and retries the upload. Polling again would hand
// it the same claimed turn and run it a second time (in write mode, twice).
func TestWorkerRetriesResultWithoutRunningAgain(t *testing.T) {
	var mu sync.Mutex
	uploads := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/community/v1/me":
			_, _ = w.Write([]byte(`{"room":"r","seat":"s"}`))
		case r.URL.Path == "/community/v1/job":
			_, _ = w.Write([]byte(`{"turn":"t1","room":"r","seat":"s","prompt":"한 번만 실행","mode":"chat","timeout_seconds":60}`))
		case r.URL.Path == "/community/v1/job/t1/result":
			uploads++
			if uploads < 3 {
				w.WriteHeader(http.StatusServiceUnavailable) // the host is restarting
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	t.Cleanup(srv.Close)
	cli := &localCLI{}
	cfg := Config{URL: srv.URL, Token: "rtc_test", Provider: cli, Scratch: t.TempDir(), MaxMode: agent.ModeChat, Once: true, Client: &http.Client{}, Log: log.New(io.Discard, "", 0), retryBase: time.Millisecond}
	if err := Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	cli.mu.Lock()
	runs := len(cli.opts)
	cli.mu.Unlock()
	if runs != 1 || uploads != 3 {
		t.Fatalf("runs %d uploads %d: the turn must run once and its result be uploaded until accepted", runs, uploads)
	}
}

// slowCLI writes a little and then runs until it is cancelled.
type slowCLI struct {
	localCLI
	stopped chan error
}

func (c *slowCLI) Run(ctx context.Context, o agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	emit(agent.Event{Kind: agent.KindTextDelta, Text: "쓰는 중"})
	<-ctx.Done()
	c.stopped <- ctx.Err()
	return agent.Result{Text: "쓰는 중"}, ctx.Err()
}

// A running turn reports its answer so far; when the host answers that the
// turn is gone (cancelled there), the worker stops its CLI at once instead of
// spending the person's tokens until the turn limit.
func TestWorkerReportsProgressAndStopsCancelledTurn(t *testing.T) {
	var mu sync.Mutex
	var reports []string
	workers := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		workers[r.Header.Get("X-RT-Worker")] = true
		switch r.URL.Path {
		case "/community/v1/me":
			_, _ = w.Write([]byte(`{"room":"r","seat":"s"}`))
		case "/community/v1/job":
			_, _ = w.Write([]byte(`{"turn":"t1","room":"r","seat":"s","prompt":"길게","mode":"chat","timeout_seconds":600}`))
		case "/community/v1/job/t1/progress":
			var body struct{ Text string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			reports = append(reports, body.Text)
			if len(reports) >= 2 {
				w.WriteHeader(http.StatusConflict) // the host stopped the turn
				_, _ = w.Write([]byte(`{"error":"취소됨"}`))
				return
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/community/v1/job/t1/result":
			w.WriteHeader(http.StatusConflict)
		}
	}))
	t.Cleanup(srv.Close)
	cli := &slowCLI{stopped: make(chan error, 1)}
	cfg := Config{URL: srv.URL, Token: "rtc_test", Provider: cli, Scratch: t.TempDir(), MaxMode: agent.ModeChat, Once: true, Client: &http.Client{}, Log: log.New(io.Discard, "", 0), reportGap: 20 * time.Millisecond}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), cfg) }()
	select {
	case err := <-cli.stopped:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the CLI stopped with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the CLI kept running after the host stopped the turn")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// The unchanged answer is not sent again; the empty report keeps the claim.
	if len(reports) < 2 || reports[0] != "쓰는 중" || reports[1] != "" {
		t.Fatalf("progress reports %q", reports)
	}
	if len(workers) != 1 || workers[""] {
		t.Fatalf("worker ids %v: every request carries this process's one id", workers)
	}
}

func TestWorkerConfigChecks(t *testing.T) {
	cli := &localCLI{}
	for name, c := range map[string]Config{
		"no token":         {URL: "http://h:1", Provider: cli, MaxMode: "chat"},
		"bad url":          {URL: "h:1/x", Token: "rtc_x", Provider: cli, MaxMode: "chat"},
		"write, no folder": {URL: "http://h:1", Token: "rtc_x", Provider: cli, MaxMode: "write"},
		"unknown mode":     {URL: "http://h:1", Token: "rtc_x", Provider: cli, MaxMode: "admin"},
	} {
		if err := c.validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if got := normalizeUsage("codex", map[string]any{"input_tokens": 10.0, "cached_input_tokens": 4.0}); got["input_tokens"] != 6.0 {
		t.Fatalf("codex usage: %v", got)
	}
}
