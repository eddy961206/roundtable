package rtmcp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	httpserver "roundtable/internal/server"
)

type scriptedProvider struct {
	agent.Provider
	id  string
	run func(context.Context, agent.RunOptions) (agent.Result, error)
}

func (p *scriptedProvider) ID() string      { return p.id }
func (p *scriptedProvider) Available() bool { return true }
func (p *scriptedProvider) CanSteer() bool  { return true }
func (p *scriptedProvider) CanFork() bool   { return false }
func (p *scriptedProvider) Run(ctx context.Context, opts agent.RunOptions, _ func(agent.Event)) (agent.Result, error) {
	return p.run(ctx, opts)
}

func TestWaitAgainstExistingHTTPServer(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(map[bool]string{false: "chain", true: "steering-chain"}[live], func(t *testing.T) {
			startedA, injected, startedB := make(chan struct{}), make(chan struct{}), make(chan struct{})
			releaseA, releaseB := make(chan struct{}), make(chan struct{})
			a := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "claude"}
			b := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "codex"}
			a.run = func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
				close(startedA)
				steered := 0
				if live {
					select {
					case <-opts.Steer:
						close(injected)
						steered = 1
					case <-ctx.Done():
						return agent.Result{}, ctx.Err()
					}
					select {
					case <-releaseA:
					case <-ctx.Done():
						return agent.Result{}, ctx.Err()
					}
				}
				return agent.Result{Text: "첫 답 @codex", Steered: steered}, nil
			}
			b.run = func(ctx context.Context, _ agent.RunOptions) (agent.Result, error) {
				close(startedB)
				select {
				case <-releaseB:
					return agent.Result{Text: "연쇄 답"}, nil
				case <-ctx.Done():
					return agent.Result{}, ctx.Err()
				}
			}
			store, err := room.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			hub, err := room.NewHub(store, agent.NewRegistry(a, b), t.TempDir(), log.New(io.Discard, "", 0))
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
			r, err := hub.CreateRoom(room.RoomSpec{Title: "검증", Mode: "chat", Participants: []string{"claude", "codex"}})
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
			if live {
				if _, err := hub.PostUser(r.ID, "시작", "claude"); err != nil {
					t.Fatal(err)
				}
				select {
				case <-startedA:
				case <-time.After(2 * time.Second):
					t.Fatal("initial turn not started")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			type outcome struct {
				value any
				err   error
			}
			done := make(chan outcome, 1)
			go func() {
				args, _ := json.Marshal(map[string]any{"room": r.ID, "text": "질문", "target": "claude", "wait_seconds": 3})
				value, err := s.call(ctx, "rt_post", args)
				done <- outcome{value, err}
			}()
			if live {
				select {
				case <-injected:
				case <-time.After(2 * time.Second):
					t.Fatal("steering was not injected")
				}
				select {
				case result := <-done:
					t.Fatalf("returned before steering completed: %v", result)
				default:
				}
				close(releaseA)
			}
			select {
			case <-startedB:
			case <-time.After(2 * time.Second):
				t.Fatal("chain turn not started")
			}
			select {
			case result := <-done:
				t.Fatalf("returned before chain completed: %v", result)
			default:
			}
			close(releaseB)
			select {
			case result := <-done:
				if result.err != nil {
					t.Fatal(result.err)
				}
				post := result.value.(*postResult)
				if *post.TimedOut || len(post.Messages) != 2 {
					t.Fatalf("%+v", post)
				}
				if live && (!post.Live || post.Messages[0].Seq >= post.Seq) {
					t.Fatalf("old steering reply omitted: %+v", post)
				}
				for _, m := range post.Messages {
					if m.Status != "done" {
						t.Fatal(m)
					}
				}
			case <-time.After(3 * time.Second):
				t.Fatal("MCP did not observe completion")
			}
		})
	}
}

// A seat's [[DECISION]] marker arrives proposed; rt_update_decision confirms it
// through the existing PATCH, and rt_read reads the real history page.
func TestDecisionConfirmationAgainstHTTPServer(t *testing.T) {
	a := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "claude"}
	a.run = func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "정했어.\n[[DECISION: 캐시는 30초 | 근거: 메시지 | 반대: 없음 | 미확인: 없음]]"}, nil
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
	r, err := hub.CreateRoom(room.RoomSpec{Title: "결정", Mode: "chat", Participants: []string{"claude"}})
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
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	tool := func(name string, args map[string]any, dest any) {
		t.Helper()
		raw, _ := json.Marshal(args)
		value, err := s.call(ctx, name, raw)
		if err != nil {
			t.Fatal(name, err)
		}
		b, _ := json.Marshal(value)
		if err := json.Unmarshal(b, dest); err != nil {
			t.Fatal(err)
		}
	}
	var post struct {
		TimedOut bool `json:"timed_out"`
	}
	tool("rt_post", map[string]any{"room": r.ID, "text": "정해 줘", "wait_seconds": 3}, &post)
	if post.TimedOut {
		t.Fatal("turn did not finish")
	}
	var list struct {
		Decisions []room.Decision `json:"decisions"`
	}
	tool("rt_decisions", map[string]any{"room": r.ID}, &list)
	if len(list.Decisions) != 1 || list.Decisions[0].Status != "proposed" {
		t.Fatalf("%+v", list)
	}
	var confirmed room.Decision
	tool("rt_update_decision", map[string]any{"room": r.ID, "id": list.Decisions[0].ID, "status": "active"}, &confirmed)
	if confirmed.Status != "active" || confirmed.Text != "캐시는 30초" {
		t.Fatalf("%+v", confirmed)
	}
	var read readResult
	tool("rt_read", map[string]any{"room": r.ID, "max_chars": 4}, &read)
	if len(read.Messages) != 1 || read.Messages[0].Kind != "agent" || len(read.Messages[0].Decisions) != 1 || read.OmittedMessages != 1 || read.FirstSeq != read.Messages[0].Seq {
		t.Fatalf("%+v", read)
	}
}
