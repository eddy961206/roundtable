package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

// Regression: the marker's system notice used to be invisible to rt wait
// while an already queued follow-up kept the room active (2026-11-06).
func TestWaitUserRequestWhileNextTurnRuns(t *testing.T) {
	firstStarted, secondStarted := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	hub, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
		if opts.SessionID == "" {
			close(firstStarted)
			select {
			case <-release:
				return agent.Result{Text: "[[REQUEST:@User]] 커밋을 push해 줘", SessionID: "session"}, nil
			case <-ctx.Done():
				return agent.Result{}, ctx.Err()
			}
		}
		close(secondStarted)
		<-ctx.Done()
		return agent.Result{}, ctx.Err()
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "사용자 요청", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	sent, err := hub.PostUser(r.ID, "첫 작업", "")
	if err != nil {
		t.Fatal(err)
	}
	<-firstStarted
	// A broadcast is queued even if the provider can steer targeted input.
	if _, err := hub.PostUser(r.ID, "다음 작업", ""); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case <-secondStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("queued follow-up did not start")
	}
	got, err := s.wait(context.Background(), r.ID+":"+strconv.FormatInt(sent.Seq, 10), 200*time.Millisecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(got)
	var result map[string]any
	json.Unmarshal(b, &result)
	if result["user_requested"] != true || got.TimedOut || len(got.Ready) != 0 {
		t.Fatalf("pending user request did not interrupt active-room wait: %s", b)
	}
	requests := result["user_requests"].([]any)[0].(map[string]any)["requests"].([]any)
	request := requests[0].(map[string]any)
	if request["text"] != "커밋을 push해 줘" || request["status"] != "pending" {
		t.Fatalf("request content/status missing: %s", b)
	}
	// Returning the cursor is receipt, not completion. It suppresses another
	// wake but must leave the request visible for a later human answer.
	next, err := s.wait(context.Background(), got.Waiting, 100*time.Millisecond, 1)
	if err != nil || !next.TimedOut {
		t.Fatalf("same pending request woke the next wait: %+v, %v", next, err)
	}
	if len(next.UserRequests) != 1 || next.UserRequests[0].Requests[0].Status != "pending" {
		t.Fatal("receipted request disappeared", next.UserRequests)
	}
	seats := callJSON(t, s, "rt_seats", map[string]any{"rooms": r.ID})
	row := seats["rooms"].([]any)[0].(map[string]any)
	if len(row["user_requests"].([]any)) != 1 || len(row["seats"].([]any)[0].(map[string]any)["user_requests"].([]any)) != 1 {
		t.Fatal(seats)
	}
	read, err := s.read(context.Background(), r.ID, 1000, 1)
	if err != nil || len(read.UserRequests) != 1 {
		t.Fatal("old request lost outside read window", read, err)
	}
	// CLI and MCP share the resolution surface. Acknowledgement is durable
	// and doesn't need a receipt cursor to suppress subsequent notifications.
	var stdout, stderr bytes.Buffer
	code := s.RunCLI(context.Background(), []string{"resolve_user_request", "--room", r.ID, "--id", request["id"].(string), "--acknowledge", "true"}, nil, &stdout, &stderr)
	if code != 0 || !strings.Contains(stdout.String(), `"status":"acknowledged"`) {
		t.Fatal(code, stdout.String(), stderr.String())
	}
	afterAck, err := s.wait(context.Background(), r.ID+":1", 100*time.Millisecond, 1)
	if err != nil || !afterAck.TimedOut || afterAck.UserRequested || len(afterAck.UserRequests) != 0 {
		t.Fatal("acknowledged request woke wait", afterAck, err)
	}
}

// List and snapshot are different HTTP reads. These races must not depend
// on a count increase (the old request can be resolved as a new one arrives).
func TestWaitUserRequestArrivalAndResolutionRaces(t *testing.T) {
	for _, kind := range []string{"arrives", "replaces", "resolved-before-read"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			var once sync.Once
			listed, changed := make(chan struct{}), make(chan struct{})
			requests := []userRequest{}
			if kind != "arrives" {
				requests = append(requests, userRequest{ID: "old", Seat: "s1", Seq: 5, Text: "첫 요청", Status: "pending"})
			}
			s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/stream" {
					w.Header().Set("Content-Type", "text/event-stream")
					w.(http.Flusher).Flush()
					select {
					case <-changed:
						io.WriteString(w, "data: {\"type\":\"rooms\"}\n\n")
						w.(http.Flusher).Flush()
					case <-r.Context().Done():
						return
					}
					<-r.Context().Done()
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/rooms":
					fmt.Fprintf(w, `[{"id":"r1","active":true,"messages":10,"user_requests":%d}]`, len(requests))
					if kind == "resolved-before-read" {
						requests[0].Status = "acknowledged"
					}
					once.Do(func() { close(listed) })
				case "/api/rooms/r1":
					json.NewEncoder(w).Encode(map[string]any{"room": map[string]any{"id": "r1", "seq": 10}, "active": true, "user_requests": requests})
				default:
					t.Error(r.URL)
				}
			}, false)
			cursor := "r1:1"
			if kind == "replaces" {
				cursor += ":5"
			}
			type outcome struct {
				result *waitResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := s.wait(context.Background(), cursor, 300*time.Millisecond, 1)
				done <- outcome{result, err}
			}()
			<-listed
			if kind != "resolved-before-read" {
				mu.Lock()
				requests = []userRequest{{ID: "new", Seat: "s1", Seq: 7, Text: "새 요청", Status: "pending"}}
				mu.Unlock()
				close(changed)
			}
			got := <-done
			if got.err != nil {
				t.Fatal(got.err)
			}
			if kind == "resolved-before-read" {
				if !got.result.TimedOut || got.result.UserRequested {
					t.Fatal("stale count notified a resolved request", got.result)
				}
			} else if !got.result.UserRequested || got.result.TimedOut || got.result.Waiting != "r1:1:7" || got.result.UserRequests[0].Requests[0].ID != "new" {
				t.Fatal(got.result)
			}
		})
	}
}

func TestWaitReadyRoomStillReportsOldUserRequest(t *testing.T) {
	hub, s := waitHub(t, func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "[[REQUEST:@User]] 실제 답변을 기다림", SessionID: "s"}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "완료된 방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "질문", ""); err != nil {
		t.Fatal(err)
	}
	// The request and the ready reply can arrive in the same wait response.
	got, err := s.wait(context.Background(), r.ID+":1", 3*time.Second, 1)
	if err != nil || len(got.Ready) != 1 || !got.UserRequested || got.Ready[0].UserRequests[0].Text != "실제 답변을 기다림" {
		t.Fatal(got, err)
	}
	// Omitting after_seq ignores existing replies, but MUST NOT ignore an
	// already pending user request. A different waiter has its own receipt.
	old, err := s.wait(context.Background(), r.ID, 200*time.Millisecond, 1)
	if err != nil || old.TimedOut || !old.UserRequested || len(old.Ready) != 0 {
		t.Fatal(old, err)
	}
	resolved := callJSON(t, s, "rt_resolve_user_request", map[string]any{"room": r.ID, "id": old.UserRequests[0].Requests[0].ID, "text": "답변 전달"})
	if resolved["user_request"].(map[string]any)["status"] != "answered" {
		t.Fatal(resolved)
	}
}
