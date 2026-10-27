package server

import (
	"strings"
	"testing"
	"time"
)

// The handoff route runs the summary turn and then reports the switch; a seat
// with no session is refused with the reason.
func TestHandoffRoute(t *testing.T) {
	ts := newTestServer(t)
	_, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "인계", "mode": "chat", "participants": []string{"claude"}})
	id := rm["id"].(string)
	seat := rm["participants"].([]any)[0].(map[string]any)["id"].(string)
	path := "/api/rooms/" + id + "/participants/" + seat + "/handoff"
	if code, body := call(t, ts, "POST", path, map[string]any{}); code != 400 || !strings.Contains(body["error"].(string), "세션") {
		t.Fatalf("no session yet: %d %v", code, body)
	}
	call(t, ts, "POST", "/api/rooms/"+id+"/messages", map[string]any{"text": "hi"})
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, got := call(t, ts, "GET", "/api/rooms/"+id, nil)
		p := got["room"].(map[string]any)["participants"].([]any)[0].(map[string]any)
		if p["session_id"] != "" && p["status"] == "idle" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first turn did not finish")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code, m := call(t, ts, "POST", path, map[string]any{"note": "결정도"}); code != 201 || m["seq"] == nil {
		t.Fatalf("handoff %d %v", code, m)
	}
	for {
		_, got := call(t, ts, "GET", "/api/rooms/"+id, nil)
		p := got["room"].(map[string]any)["participants"].([]any)[0].(map[string]any)
		if p["history_from"] != nil && p["session_id"] == "" {
			return
		}
		if time.Now().After(deadline.Add(5 * time.Second)) {
			t.Fatalf("seat did not switch: %v", p)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
