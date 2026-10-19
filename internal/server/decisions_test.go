package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestDecisionAPI(t *testing.T) {
	ts := newTestServer(t)
	_, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "d", "mode": "chat", "participants": []string{"claude"}})
	id := rm["id"].(string)
	if rm["blind_first_default"] != true {
		t.Fatalf("new rooms default to a blind first round: %v", rm)
	}
	list := func() []map[string]any {
		t.Helper()
		res, err := http.Get(ts.URL + "/api/rooms/" + id + "/decisions")
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var out []map[string]any
		if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&out) != nil || out == nil {
			t.Fatalf("list %d %v", res.StatusCode, out)
		}
		return out
	}
	if got := list(); len(got) != 0 {
		t.Fatal(got)
	}
	code, d := call(t, ts, "POST", "/api/rooms/"+id+"/decisions", map[string]any{"text": "API 결정", "evidence": "회의", "dissent": "없음", "unverified": "없음"})
	if code != 201 || d["by"] != "user" || d["status"] != "active" || d["source_msg"] != "" {
		t.Fatalf("add %d %v", code, d)
	}
	did := d["id"].(string)
	if code, out := call(t, ts, "POST", "/api/rooms/"+id+"/decisions", map[string]any{"text": ""}); code != 400 {
		t.Fatalf("empty text %d %v", code, out)
	}
	code, d = call(t, ts, "PATCH", "/api/rooms/"+id+"/decisions/"+did, map[string]any{"status": "dropped"})
	if code != 200 || d["status"] != "dropped" || d["text"] != "API 결정" {
		t.Fatalf("patch %d %v", code, d)
	}
	if code, out := call(t, ts, "PATCH", "/api/rooms/"+id+"/decisions/"+did, map[string]any{"status": "gone"}); code != 400 {
		t.Fatalf("bad status %d %v", code, out)
	}
	if code, _ := call(t, ts, "PATCH", "/api/rooms/"+id+"/decisions/missing", map[string]any{"text": "x"}); code != 400 {
		t.Fatal("unknown decision accepted")
	}
	if got := list(); len(got) != 1 || got[0]["status"] != "dropped" {
		t.Fatal(got)
	}
	if code, _ := call(t, ts, "GET", "/api/rooms/missing/decisions", nil); code != 404 {
		t.Fatal("missing room listed")
	}
	_, snap := call(t, ts, "GET", "/api/rooms/"+id, nil)
	if cards, ok := snap["decisions"].([]any); !ok || len(cards) != 1 {
		t.Fatalf("snapshot decisions %v", snap["decisions"])
	}
	// The discussion body accepts blind_first and rejects a malformed one.
	if code, _ := call(t, ts, "PATCH", "/api/rooms/"+id, map[string]any{"blind_first_default": false}); code != 200 {
		t.Fatal("patch blind_first_default")
	}
	// (This one-seat room cannot discuss; the decode error must come first.)
	if code, out := call(t, ts, "POST", "/api/rooms/"+id+"/discussion", map[string]any{"blind_first": "yes"}); code != 400 || !strings.Contains(out["error"].(string), "json") {
		t.Fatalf("malformed blind_first %d %v", code, out)
	}
}
