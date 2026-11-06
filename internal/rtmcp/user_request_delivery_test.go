package rtmcp

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

// A resolution closes the human request at append, while its answer follows
// the ordinary per-message delivery contract. The other pending request has
// already been receipted by this waiter and must not wake it again.
func TestResolveUserRequestWaitTracksDeliveryAndRequestCursor(t *testing.T) {
	for _, outcome := range []string{agent.InputAccepted, agent.InputRejected, agent.InputUncertain} {
		t.Run(outcome, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			inputSeen := make(chan agent.SteerInput, 1)
			var calls atomic.Int32
			h, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
				switch calls.Add(1) {
				case 1:
					return agent.Result{Text: "[[REQUEST:@User]] 설정 값\n[[REQUEST:@User]] 나중에 확인할 일", SessionID: "session"}, nil
				case 2:
					close(started)
					var input agent.SteerInput
					select {
					case input = <-opts.Steer:
					case <-ctx.Done():
						return agent.Result{}, ctx.Err()
					}
					inputSeen <- input
					select {
					case <-release:
					case <-ctx.Done():
						return agent.Result{}, ctx.Err()
					}
					return agent.Result{Text: "current turn ended", SessionID: "session", Receipts: []agent.SteerReceipt{{ID: input.ID, State: outcome, Detail: "injected outcome"}}}, nil
				case 3:
					if outcome != agent.InputRejected || strings.Count(opts.Prompt, "RESOLVED_VALUE") != 1 {
						return agent.Result{}, fmt.Errorf("unexpected answer replay: %s", opts.Prompt)
					}
					return agent.Result{Text: "next turn received the answer", SessionID: "session"}, nil
				default:
					return agent.Result{}, fmt.Errorf("duplicate execution")
				}
			})
			r, err := h.CreateRoom(room.RoomSpec{Title: "request + receipt", Mode: agent.ModeChat, Participants: []string{"claude"}})
			if err != nil {
				t.Fatal(err)
			}
			first, err := h.PostUser(r.ID, "request values", "")
			if err != nil {
				t.Fatal(err)
			}
			requested, err := s.wait(context.Background(), fmt.Sprintf("%s:%d", r.ID, first.Seq), 3*time.Second, 1000)
			if err != nil || len(requested.UserRequests) != 1 || len(requested.UserRequests[0].Requests) != 2 {
				t.Fatalf("%+v %v", requested, err)
			}
			requests := requested.UserRequests[0].Requests
			request, cursor := requests[0], requests[1].Seq
			if _, err := h.PostUser(r.ID, "continue while the requests remain pending", request.Seat); err != nil {
				t.Fatal(err)
			}
			<-started
			resolved := callJSON(t, s, "rt_resolve_user_request", map[string]any{"room": r.ID, "id": request.ID, "text": "RESOLVED_VALUE"})
			delivery, ok := resolved["delivery"].(map[string]any)
			if !ok || delivery["state"] != "pending" || resolved["live"] != false || resolved["user_request"].(map[string]any)["status"] != "answered" {
				t.Fatalf("resolution must expose pending input separately from answered request: %v", resolved)
			}
			input := <-inputSeen
			if !strings.Contains(input.Text, "RESOLVED_VALUE") {
				t.Fatal(input)
			}
			waitCursor := fmt.Sprintf("%s:%d:%d", r.ID, int64(resolved["seq"].(float64)), cursor)
			before, err := s.wait(context.Background(), waitCursor, 80*time.Millisecond, 1)
			if err != nil || !before.TimedOut || before.UserRequested || len(before.Ready) != 0 {
				t.Fatalf("wait mistook saved answer or old request receipt for completion: %+v %v", before, err)
			}
			close(release)
			done, err := s.wait(context.Background(), waitCursor, 3*time.Second, 1)
			if err != nil || done.TimedOut || done.UserRequested || len(done.Ready) != 1 {
				t.Fatalf("delivery did not finish wait: %+v %v", done, err)
			}
			snap, err := h.Snapshot(r.ID)
			if err != nil || snap.UserRequests[0].Status != "answered" || snap.UserRequests[1].Status != "pending" {
				t.Fatalf("request state changed with input receipt: %+v %v", snap, err)
			}
			wantCalls, wantState := int32(2), "accepted"
			if outcome == agent.InputRejected {
				wantCalls, wantState = 3, "delivered"
			}
			if outcome == agent.InputUncertain {
				wantState = "failed"
			}
			for _, m := range snap.Messages {
				if m.ID == input.ID && (m.Delivery.State != wantState || m.Live != (outcome == agent.InputAccepted)) {
					t.Fatalf("answer delivery: %+v", m)
				}
			}
			if calls.Load() != wantCalls {
				t.Fatalf("calls=%d want=%d", calls.Load(), wantCalls)
			}
			if outcome == agent.InputUncertain {
				visible := false
				for _, m := range done.Ready[0].Messages {
					visible = visible || (m.Delivery != nil && m.Delivery.State == "failed")
				}
				if !visible {
					t.Fatalf("wait hid the failed request answer: %+v", done.Ready[0])
				}
			}
		})
	}
}
