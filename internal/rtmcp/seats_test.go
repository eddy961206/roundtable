package rtmcp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	httpserver "roundtable/internal/server"
)

// 2026-11-04: an agy seat's shell command read a never-closed stdin for 1h54m
// and rt_seats showed `status: running`, `last: "streaming: "` with nothing that
// told it apart from a seat at work.
func TestSeatsWarnsAboutLongQuietToolCall(t *testing.T) {
	now := time.Date(2026, 11, 4, 18, 52, 0, 0, time.Local).UnixMilli()
	hungAt := now - (114 * time.Minute).Milliseconds()
	sp := seatState{ID: "agy-713b", Status: "running", Text: "도구: run_command", Tool: &toolState{Name: "run_command", Input: `{"CommandLine":"rg -n \"upgrade-history\""}`, Since: hungAt}}
	queue := []apiTurn{{Agent: "other", Status: "running", Quiet: now}, {Agent: "agy-713b", Status: "running", Quiet: hungAt}}
	var row seatRow
	row.observeRunning(sp, queue, now)
	if row.Activity != "도구: run_command" || row.Tool == nil || row.Tool.Seconds != 114*60 || !strings.Contains(row.Tool.Input, "upgrade-history") || row.Quiet != 114*60 {
		t.Fatalf("running seat: %+v %+v", row, row.Tool)
	}
	if !strings.Contains(row.Warn, "114분째") || !strings.Contains(row.Warn, "run_command") || !strings.Contains(row.Warn, "stdin") {
		t.Fatalf("warn: %q", row.Warn)
	}

	// A background wait has no open call but is still quiet.
	row = seatRow{}
	row.observeRunning(seatState{ID: "agy-713b", Status: "running"}, queue, now)
	if row.Tool != nil || !strings.HasPrefix(row.Warn, "114분째 CLI 출력 없음") || strings.Contains(row.Warn, "stdin") {
		t.Fatalf("quiet without tool: %+v", row)
	}

	// Short quiet is ordinary work.
	row = seatRow{}
	row.observeRunning(sp, []apiTurn{{Agent: "agy-713b", Status: "running", Quiet: now - 30_000}}, now)
	if row.Warn != "" || row.Quiet != 30 {
		t.Fatalf("short quiet must not warn: %+v", row)
	}
}

func TestSeatsShowsRunningToolFromServer(t *testing.T) {
	called, release := make(chan struct{}), make(chan struct{})
	p := &askingProvider{Provider: agent.NewClaude("__missing_test_cli__")}
	p.run = func(ctx context.Context, opts agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
		emit(agent.Event{TS: time.Now().UnixMilli(), Kind: agent.KindToolCall, ID: "s41", Name: "run_command", Input: `{"CommandLine":"rg -n \"upgrade-history\""}`})
		close(called)
		select {
		case <-release:
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
		return agent.Result{Text: "끝"}, nil
	}
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
	r, err := hub.CreateRoom(room.RoomSpec{Title: "도구", Mode: "read", Workspace: t.TempDir(), Participants: []string{"codex"}})
	if err != nil {
		t.Fatal(err)
	}
	http := httptest.NewServer(httpserver.New(hub, nil, fstest.MapFS{}))
	defer http.Close()
	s, err := New(http.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := hub.PostUser(r.ID, "검색", "codex"); err != nil {
		t.Fatal(err)
	}
	<-called
	raw, _ := json.Marshal(map[string]any{"rooms": r.ID})
	seats, err := s.call(ctx, "rt_seats", raw)
	if err != nil {
		t.Fatal(err)
	}
	rows := seats.(map[string]any)["rooms"].([]any)[0].(map[string]any)["seats"].([]seatRow)
	if len(rows) != 1 || rows[0].Status != "running" || rows[0].Activity != "도구: run_command" || rows[0].Tool == nil || rows[0].Tool.Name != "run_command" || !strings.Contains(rows[0].Tool.Input, "upgrade-history") || rows[0].Quiet > 5 || rows[0].Warn != "" {
		t.Fatalf("seats: %+v", rows)
	}
	close(release)
}
