package rtmcp

import (
	"bytes"
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

// askingProvider is a Codex-like seat: no steering, and it can ask the person
// mid-turn through emit.
type askingProvider struct {
	agent.Provider
	run func(context.Context, agent.RunOptions, func(agent.Event)) (agent.Result, error)
}

func (p *askingProvider) ID() string      { return "codex" }
func (p *askingProvider) Available() bool { return true }
func (p *askingProvider) CanSteer() bool  { return false }
func (p *askingProvider) CanFork() bool   { return false }
func (p *askingProvider) Run(ctx context.Context, opts agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	return p.run(ctx, opts, emit)
}

func TestAskSurfacesInWaitSeatsReadAndAnswer(t *testing.T) {
	ask, release := make(chan struct{}), make(chan struct{})
	prompts := make(chan string, 4)
	turn := 0
	p := &askingProvider{Provider: agent.NewClaude("__missing_test_cli__")}
	p.run = func(ctx context.Context, opts agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
		turn++
		prompts <- opts.Prompt
		if turn > 1 {
			return agent.Result{Text: "답 받음"}, nil
		}
		<-ask
		emit(agent.Event{TS: time.Now().UnixMilli(), Kind: agent.KindAsk, ID: "call_5a79", Meta: map[string]any{"questions": []agent.AskQuestion{{Title: "누락 파일만 복원해도 될까?", Options: []string{"누락 파일만 복원", "메인은 그대로 두기"}}}}})
		select {
		case <-release:
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
		return agent.Result{Text: "누락 파일만 복원해도 될까?\n- 누락 파일만 복원\n- 메인은 그대로 두기\n\n판정: 질문을 남기고 진행함"}, nil
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
	r, err := hub.CreateRoom(room.RoomSpec{Title: "질문", Mode: "chat", Participants: []string{"codex"}})
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
	call := func(name string, args map[string]any) (any, error) {
		raw, _ := json.Marshal(args)
		return s.call(ctx, name, raw)
	}
	sent, err := hub.PostUser(r.ID, "시작", "codex")
	if err != nil {
		t.Fatal(err)
	}
	<-prompts
	waited := make(chan *waitResult, 1)
	go func() {
		res, err := s.wait(ctx, r.ID+":1", 5*time.Second, 20000)
		if err != nil {
			t.Error(err)
		}
		waited <- res
	}()
	time.Sleep(300 * time.Millisecond) // let the wait record the room's question count first
	close(ask)

	// A new question ends the wait before the turn does.
	res := <-waited
	if res == nil || !res.Asked || res.TimedOut || len(res.Ready) != 0 || res.Waiting != r.ID+":1" || len(res.Asking) != 1 || len(res.Asking[0].Asks) != 1 {
		t.Fatalf("wait: %+v", res)
	}
	got := res.Asking[0].Asks[0]
	if got.ID != "call_5a79" || got.Seat != "codex" || got.Status != "waiting" || got.Seq <= sent.Seq || got.Questions[0].Options[1] != "메인은 그대로 두기" {
		t.Fatalf("asking: %+v", got)
	}
	seats, err := call("rt_seats", map[string]any{"rooms": r.ID})
	if err != nil {
		t.Fatal(err)
	}
	rows := seats.(map[string]any)["rooms"].([]any)[0].(map[string]any)["seats"].([]seatRow)
	if len(rows) != 1 || len(rows[0].Asks) != 1 || rows[0].Asks[0].ID != "call_5a79" {
		t.Fatalf("seats: %+v", rows)
	}

	// The CLI answer picks the first option.
	var out, errOut bytes.Buffer
	if code := s.RunCLI(ctx, []string{"answer", "--room", r.ID, "--id", "call_5a79", "--choice", "1"}, nil, &out, &errOut); code != 0 {
		t.Fatalf("answer exit %d: %s", code, errOut.String())
	}
	var answered struct {
		Seq int64   `json:"seq"`
		Ask askView `json:"ask"`
	}
	if err := json.Unmarshal(out.Bytes(), &answered); err != nil {
		t.Fatal(err)
	}
	if answered.Ask.Status != "answered" || answered.Ask.Answer != "누락 파일만 복원" || answered.Ask.AnswerSeq != answered.Seq {
		t.Fatalf("answer: %s", out.String())
	}
	if _, err := call("rt_answer", map[string]any{"room": r.ID, "id": "call_5a79", "text": "다시"}); err == nil || !strings.Contains(err.Error(), "이미 답한") {
		t.Fatal(err)
	}
	close(release)
	select {
	case prompt := <-prompts:
		if !strings.Contains(prompt, "사용자 답: 누락 파일만 복원") {
			t.Fatal(prompt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("answer turn not started")
	}

	// Once the room is done, the summary still carries the question and its state.
	done, err := s.wait(ctx, r.ID+":1", 5*time.Second, summaryBudget)
	if err != nil {
		t.Fatal(err)
	}
	summarize(done.Ready)
	if len(done.Ready) != 1 || len(done.Ready[0].Asks) != 1 || done.Ready[0].Asks[0].Status != "answered" || done.Asked || len(done.Asking) != 0 {
		t.Fatalf("done: %+v", done)
	}
	read, err := s.read(ctx, r.ID, answered.Seq, 20000)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Asks) != 0 {
		t.Fatalf("an answered question older than after_seq is left out: %+v", read.Asks)
	}
}
