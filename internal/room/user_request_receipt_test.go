package room

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// Copy the exact committed files while the old invocation is still running.
// The restored hub owns a different directory, so stopping the original test
// invocation cannot rewrite the simulated crash image.
func copyUserRequestStore(t *testing.T, source string) *Store {
	t.Helper()
	dest := t.TempDir()
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(dest)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestResolvedUserRequestSurvivesReceiptRecoveryOnce(t *testing.T) {
	for _, tc := range []struct {
		name, state, history, final string
		replay                      bool
	}{
		{"not written", "pending", "", "delivered", true},
		{"accepted", agent.InputAccepted, "", agent.InputAccepted, false},
		{"response lost but received", agent.InputSending, agent.InputAccepted, agent.InputAccepted, false},
		{"response lost and absent", agent.InputSending, agent.InputRejected, "delivered", true},
		{"history unavailable", agent.InputUncertain, "error", "failed", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex")
			ps[0].steer = true
			postQueueMessage(t, h, r.ID, "ask for input", "codex")
			finishCall(nextCall(t, ps[0]), "[[REQUEST:@User]] 설정 값")
			waitIdle(t, h, r.ID)
			snap, _ := h.Snapshot(r.ID)
			id := snap.UserRequests[0].ID
			postQueueMessage(t, h, r.ID, "work continues", "codex")
			running := nextCall(t, ps[0])
			answer, request, err := h.ResolveUserRequest(r.ID, id, "RECOVER_RESOLVED_ANSWER", false)
			if err != nil || answer.Live || request.Status != "answered" || answer.UserRequestReply == nil {
				t.Fatalf("%+v %+v %v", answer, request, err)
			}
			input := <-running.opts.Steer
			if input.ID != answer.ID {
				t.Fatal(input)
			}
			if tc.state != "pending" {
				running.emit(agent.Event{Kind: agent.KindDelivery, Receipt: &agent.SteerReceipt{ID: input.ID, State: tc.state, SessionID: "codex-session", TurnID: "provider-turn", ClientID: input.ID}})
			}
			h.mu.Lock()
			store := copyUserRequestStore(t, h.store.Root())
			h.mu.Unlock()
			if err := h.Stop(r.ID); err != nil {
				t.Fatal(err)
			}
			waitIdle(t, h, r.ID)
			p := &recoveryProvider{controlledProvider: &controlledProvider{fakeProvider: fakeProvider{id: "codex", steer: true}, callsCh: make(chan *controlledCall, 8)}, resolved: tc.history}
			reg := agent.NewRegistry(p)
			restored, err := NewHub(store, reg, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = restored.Shutdown(context.Background()) })
			if tc.replay {
				call := nextCall(t, p.controlledProvider)
				if strings.Count(call.opts.Prompt, "RECOVER_RESOLVED_ANSWER") != 1 {
					t.Fatal(call.opts.Prompt)
				}
				finishCall(call, "recovered input")
			}
			s := queueSnapshot(t, restored, r.ID, func(s *Snapshot) bool { return !s.Active })
			if len(s.UserRequests) != 1 || s.UserRequests[0].Status != "answered" || s.UserRequests[0].ResolvedSeq != answer.Seq {
				t.Fatalf("request reopened: %+v", s.UserRequests)
			}
			answers := 0
			for _, m := range s.Messages {
				if m.UserRequestReply != nil && m.UserRequestReply.ID == id {
					answers++
					if m.ID != answer.ID || m.Delivery.State != tc.final {
						t.Fatalf("answer changed during receipt recovery: %+v", m)
					}
				}
			}
			if answers != 1 || len(p.callsCh) != 0 {
				t.Fatalf("resolution replayed: answers=%d calls=%d", answers, len(p.callsCh))
			}
			if _, _, err := restored.ResolveUserRequest(r.ID, id, "duplicate", false); err == nil {
				t.Fatal("answered request could be resolved again")
			}
			// A second restart must not replay the first recovery's completed input.
			again, err := NewHub(store, reg, t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = again.Shutdown(context.Background()) })
			queueSnapshot(t, again, r.ID, func(s *Snapshot) bool { return !s.Active })
			postQueueMessage(t, again, r.ID, "new instruction after restart", "codex")
			next := nextCall(t, p.controlledProvider)
			if strings.Contains(next.opts.Prompt, "RECOVER_RESOLVED_ANSWER") {
				t.Fatal("received or unresolved answer was silently resent", next.opts.Prompt)
			}
			finishCall(next, "next done")
			queueSnapshot(t, again, r.ID, func(s *Snapshot) bool { return !s.Active })
			if len(p.callsCh) != 0 {
				t.Fatal("extra recovery invocation")
			}
		})
	}
}
