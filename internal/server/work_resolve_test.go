package server

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

func TestResolvePartHTTPConflict(t *testing.T) {
	for _, state := range []string{"working", "fixing", "queued", "awaiting_approval", "merged", "abandoned", "blocked", "review_start_failed"} {
		t.Run(state, func(t *testing.T) {
			store, err := room.NewStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			part := &room.Part{ID: "p1", State: state}
			if state == "blocked" {
				part.Block = &room.Block{Kind: "submit", From: "working"}
			}
			if state == "review_start_failed" {
				part.State, part.SubmitSHA = "blocked", "old-submission-sha"
				part.Block = &room.Block{Kind: "review", From: "fixing"}
				part.Review = &room.ReviewState{Phase: "fix"}
			}
			r := &room.Room{ID: "resolve-test", Work: &room.WorkPlan{MergeMode: "confirm", Parts: []*room.Part{part}}}
			if err := store.SaveRoom(r); err != nil {
				t.Fatal(err)
			}
			reg := agent.NewRegistry(echoProvider{})
			h, err := room.NewHub(store, reg, t.TempDir(), log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = h.Shutdown(context.Background()) })
			ts := httptest.NewServer(New(h, reg, fstest.MapFS{"index.html": {Data: []byte("ok")}}))
			t.Cleanup(ts.Close)
			if code, body := call(t, ts, "POST", "/api/rooms/resolve-test/work/parts/p1/resolve", nil); code != 409 {
				t.Fatalf("resolve in %s: status %d body %v", state, code, body)
			}
		})
	}
}
