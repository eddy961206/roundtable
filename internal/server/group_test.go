package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPatchRoomGroupTooLong(t *testing.T) {
	ts := newTestServer(t)
	_, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "g", "mode": "chat", "participants": []string{"claude"}})
	id := rm["id"].(string)
	if code, out := call(t, ts, "PATCH", "/api/rooms/"+id, map[string]any{"group": "기존"}); code != 200 {
		t.Fatalf("그룹 지정 PATCH %d %v", code, out)
	}
	if code, out := call(t, ts, "PATCH", "/api/rooms/"+id, map[string]any{"title": "x", "group": strings.Repeat("a", 121)}); code != 400 {
		t.Fatalf("121자 그룹 PATCH %d %v", code, out)
	}
	_, snap := call(t, ts, "GET", "/api/rooms/"+id, nil)
	if got := snap["room"].(map[string]any); got["title"] != "g" || got["group"] != "기존" {
		t.Fatalf("거절 뒤 방 %v", got)
	}
}

func TestCreateRoomGroup(t *testing.T) {
	ts := newTestServer(t)
	spec := map[string]any{"title": "g", "mode": "chat", "participants": []string{"claude"}, "group": strings.Repeat("a", 121)}
	if code, out := call(t, ts, "POST", "/api/rooms", spec); code != 400 {
		t.Fatalf("121자 그룹 POST %d %v", code, out)
	}
	res, err := http.Get(ts.URL + "/api/rooms")
	if err != nil {
		t.Fatal(err)
	}
	var rooms []any
	_ = json.NewDecoder(res.Body).Decode(&rooms)
	res.Body.Close()
	if len(rooms) != 0 {
		t.Fatalf("거절 뒤 방 목록 %v", rooms)
	}
	spec["group"] = "g1"
	if code, rm := call(t, ts, "POST", "/api/rooms", spec); code != 201 || rm["group"] != "g1" {
		t.Fatalf("그룹 지정 POST %d %v", code, rm)
	}
}
