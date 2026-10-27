package server

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

// blockingProvider keeps a turn running until release closes.
type blockingProvider struct {
	echoProvider
	release chan struct{}
}

func (p blockingProvider) Run(ctx context.Context, o agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	select {
	case <-p.release:
		return agent.Result{Text: "끝", SessionID: "s"}, nil
	case <-ctx.Done():
		return agent.Result{}, ctx.Err()
	}
}

func TestShutdownWhenIdle(t *testing.T) {
	release := make(chan struct{})
	store, _ := room.NewStore(t.TempDir())
	hub, err := room.NewHub(store, agent.NewRegistry(blockingProvider{release: release}), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hub.Shutdown(context.Background()) })
	srv := New(hub, nil, fstest.MapFS{})
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "긴 작업", ""); err != nil {
		t.Fatal(err)
	}
	if code, out := call(t, ts, "POST", "/api/shutdown", map[string]any{"timeout_seconds": 1}); code != 400 {
		t.Fatalf("shutdown without when_idle: %d %v", code, out)
	}
	if code, out := call(t, ts, "POST", "/api/shutdown", map[string]any{"when_idle": true, "timeout_seconds": 1}); code != 409 {
		t.Fatalf("busy server must stay up: %d %v", code, out)
	}
	select {
	case <-srv.Stopped():
		t.Fatal("stopped while busy")
	default:
	}
	time.AfterFunc(200*time.Millisecond, func() { close(release) })
	code, out := call(t, ts, "POST", "/api/shutdown", map[string]any{"when_idle": true, "timeout_seconds": 5})
	dir, _ := os.Getwd()
	if code != 200 || out["dir"] != dir || len(out["args"].([]any)) != len(os.Args) || out["pid"] != float64(os.Getpid()) {
		t.Fatalf("idle shutdown: %d %v", code, out)
	}
	select {
	case <-srv.Stopped():
	case <-time.After(time.Second):
		t.Fatal("Stopped was not closed")
	}
	snap, _ := hub.Snapshot(r.ID)
	if m := snap.Messages[len(snap.Messages)-1]; m.Kind != "agent" || m.Status != "done" {
		t.Fatalf("the running turn must finish, not be cancelled: %+v", m)
	}
}
