package rtmcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"testing"
)

// A worktree removed after its PR merged leaves the room pointing at a missing
// folder; rt_update_room repoints it without recreating the room or its seats.
func TestUpdateRoomPatchesOnlyGivenFields(t *testing.T) {
	var got map[string]any
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PATCH" || r.URL.Path != "/api/rooms/r1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"r1"}`)
	}, false)
	if _, err := s.call(context.Background(), "rt_update_room", json.RawMessage(`{"room":"r1","workspace":"D:/w/new"}`)); err != nil {
		t.Fatal(err)
	}
	if want := map[string]any{"workspace": "D:/w/new"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PATCH body %v, want %v", got, want)
	}
	for _, args := range []string{`{"room":"r1"}`, `{"room":"r1","mode":"admin"}`, `{"room":"../x","title":"t"}`} {
		if _, err := s.call(context.Background(), "rt_update_room", json.RawMessage(args)); err == nil {
			t.Errorf("accepted %s", args)
		}
	}
}
