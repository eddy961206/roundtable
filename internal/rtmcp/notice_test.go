package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	httpserver "roundtable/internal/server"
)

// rt post --notice true through the real HTTP server: recorded, no turn, and
// read by the target's next ordinary turn. The ordinary post still starts one.
func TestPostNoticeAgainstHTTPServer(t *testing.T) {
	var runs atomic.Int32
	prompts := make(chan string, 4)
	a := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "claude"}
	a.run = func(_ context.Context, opts agent.RunOptions) (agent.Result, error) {
		runs.Add(1)
		prompts <- opts.Prompt
		return agent.Result{Text: "확인"}, nil
	}
	store, err := room.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub, err := room.NewHub(store, agent.NewRegistry(a), t.TempDir(), log.New(io.Discard, "", 0))
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
	r, err := hub.CreateRoom(room.RoomSpec{Title: "알림", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpserver.New(hub, nil, fstest.MapFS{}))
	defer srv.Close()
	s, err := New(srv.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.client.CloseIdleConnections()

	// The CLI form the usage watcher uses, with --notice true.
	var out, errOut bytes.Buffer
	code := s.RunCLI(context.Background(), []string{"post", "--room", r.ID, "--target", "claude", "--text", "-", "--wait_seconds", "0", "--notice", "true"}, strings.NewReader("사용량 41%"), &out, &errOut)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got["notice"] != true || got["seq"] == nil {
		t.Fatalf("result %s (%v)", out.String(), err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := runs.Load(); n != 0 {
		t.Fatalf("notice started %d turn(s)", n)
	}
	snap, _ := hub.Snapshot(r.ID)
	if last := snap.Messages[len(snap.Messages)-1]; !last.Notice || last.Text != "사용량 41%" || len(snap.Queue) != 0 {
		t.Fatalf("notice not recorded: %+v queue=%v", last, snap.Queue)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	raw, _ := json.Marshal(map[string]any{"room": r.ID, "target": "claude", "text": "다음 질문", "wait_seconds": 3})
	if _, err := s.call(ctx, "rt_post", raw); err != nil {
		t.Fatal(err)
	}
	if n := runs.Load(); n != 1 {
		t.Fatalf("ordinary post should start one turn, got %d", n)
	}
	if p := <-prompts; !strings.Contains(p, "사용량 41%") || !strings.Contains(p, "다음 질문") {
		t.Fatalf("next turn did not read the notice:\n%s", p)
	}
}

func TestPostHelpSaysWhenToUseNotice(t *testing.T) {
	s, err := New("http://127.0.0.1:1", false)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.RunCLI(context.Background(), []string{"post", "--help"}, strings.NewReader(""), &out, io.Discard)
	help := out.String()
	if !strings.Contains(help, "--notice boolean") || !strings.Contains(help, "상태·사용량 보고처럼 답이 필요 없는 정보") {
		t.Fatalf("post help:\n%s", help)
	}
}
