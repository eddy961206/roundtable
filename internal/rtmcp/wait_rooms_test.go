package rtmcp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	httpserver "roundtable/internal/server"
)

func waitHub(t *testing.T, run func(context.Context, agent.RunOptions) (agent.Result, error)) (*room.Hub, *Server) {
	t.Helper()
	p := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "claude", run: run}
	store, err := room.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub, err := room.NewHub(store, agent.NewRegistry(p), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	http := httptest.NewServer(httpserver.New(hub, nil, fstest.MapFS{}))
	t.Cleanup(http.Close)
	s, err := New(http.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.client.CloseIdleConnections)
	return hub, s
}

func callJSON(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	value, err := s.call(context.Background(), name, raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(value)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWaitReturnsTheFirstFinishedRoom(t *testing.T) {
	release := map[string]chan struct{}{"빠름": make(chan struct{}), "느림": make(chan struct{})}
	hub, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
		for key, ch := range release {
			if strings.Contains(opts.Prompt, key) {
				select {
				case <-ch:
					return agent.Result{Text: key + " 답", SessionID: "s", Usage: map[string]any{"input_tokens": 5.0, "cache_read_input_tokens": 95.0, "output_tokens": 3.0}}, nil
				case <-ctx.Done():
					return agent.Result{}, ctx.Err()
				}
			}
		}
		return agent.Result{Text: "?"}, nil
	})
	fast, err := hub.CreateRoom(room.RoomSpec{Title: "빠른 방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := hub.CreateRoom(room.RoomSpec{Title: "느린 방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := hub.PostUser(fast.ID, "빠름", "")
	b, _ := hub.PostUser(slow.ID, "느림", "")
	rooms := fast.ID + ":" + strconv.FormatInt(a.Seq, 10) + "," + slow.ID + ":" + strconv.FormatInt(b.Seq, 10)

	done := make(chan map[string]any, 1)
	go func() { done <- callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 5}) }()
	select {
	case got := <-done:
		t.Fatalf("returned before any room finished: %v", got)
	case <-time.After(300 * time.Millisecond):
	}
	close(release["빠름"])
	got := <-done
	ready := got["ready"].([]any)
	first := ready[0].(map[string]any)
	if len(ready) != 1 || first["room"] != fast.ID || got["timed_out"] != false || got["waiting"] != slow.ID+":"+strconv.FormatInt(b.Seq, 10) {
		t.Fatalf("wait result %v", got)
	}
	msgs := first["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["text"] != "빠름 답" || last["kind"] != "agent" {
		t.Fatalf("ready room messages %v", msgs)
	}

	timedOut := callJSON(t, s, "rt_wait", map[string]any{"rooms": got["waiting"], "timeout_seconds": 1})
	if timedOut["timed_out"] != true || len(timedOut["ready"].([]any)) != 0 || timedOut["waiting"] != got["waiting"] {
		t.Fatalf("timeout result %v", timedOut)
	}
	close(release["느림"])
	second := callJSON(t, s, "rt_wait", map[string]any{"rooms": got["waiting"], "timeout_seconds": 5})
	if r := second["ready"].([]any); len(r) != 1 || r[0].(map[string]any)["room"] != slow.ID || second["waiting"] != "" {
		t.Fatalf("second wait %v", second)
	}

	usage := callJSON(t, s, "rt_usage", map[string]any{"rooms": fast.ID + "," + slow.ID})
	totals := usage["totals"].(map[string]any)
	if totals["turns"] != 2.0 || totals["input_tokens"] != 200.0 || totals["cached_tokens"] != 190.0 || totals["uncached_tokens"] != 10.0 || len(usage["rooms"].([]any)) != 2 {
		t.Fatalf("usage %v", usage)
	}
}

// Without after_seq a room counts only replies newer than the wait's start,
// so an already answered room does not end the wait at once.
func TestWaitWithoutSeqIgnoresEarlierReplies(t *testing.T) {
	hub, s := waitHub(t, func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "답", SessionID: "s"}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "먼저", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for snap, _ := hub.Snapshot(r.ID); len(snap.Messages) < 2 || snap.Messages[1].Status != "done"; snap, _ = hub.Snapshot(r.ID) {
		if time.Now().After(deadline) {
			t.Fatal("first reply did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := callJSON(t, s, "rt_wait", map[string]any{"rooms": r.ID, "timeout_seconds": 1})
	if got["timed_out"] != true {
		t.Fatalf("an earlier reply ended the wait: %v", got)
	}
}
