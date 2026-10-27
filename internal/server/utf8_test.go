package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func rawCall(t *testing.T, ts *httptest.Server, method, path string, body []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

// 2026-10-27: curl in Windows Git Bash sent an inline -d argument in the console
// code page; encoding/json turned the bytes into U+FFFD and the seat role was
// stored as "���� �˼�". "교차 검수" in CP949 is the byte string below.
const cp949Role = "\xb1\xb3\xc2\xf7 \xb0\xcb\xbc\xf6"

func TestNonUTF8BodyRejectedNotStored(t *testing.T) {
	ts := newTestServer(t)
	code, rm := call(t, ts, "POST", "/api/rooms", map[string]any{"title": "utf8", "mode": "chat", "participants": []string{"claude"}})
	if code != 201 && code != 200 {
		t.Fatalf("create %d %v", code, rm)
	}
	id := rm["id"].(string)
	seats := func() []any {
		_, page := call(t, ts, "GET", "/api/rooms/"+id, nil)
		return page["room"].(map[string]any)["participants"].([]any)
	}
	before := len(seats())

	code, out := rawCall(t, ts, "POST", "/api/rooms/"+id+"/participants", []byte(`{"provider":"claude","role":"`+cp949Role+`"}`))
	if code != 400 || !strings.Contains(out["error"].(string), "UTF-8") {
		t.Fatalf("CP949 body: %d %v", code, out)
	}
	if n := len(seats()); n != before {
		t.Fatalf("a seat was stored from a rejected body: %d → %d", before, n)
	}
	code, out = rawCall(t, ts, "PATCH", "/api/rooms/"+id, []byte(`{"title":"`+cp949Role+`"}`))
	if code != 400 {
		t.Fatalf("CP949 title: %d %v", code, out)
	}
	if _, page := call(t, ts, "GET", "/api/rooms/"+id, nil); page["room"].(map[string]any)["title"] != "utf8" {
		t.Fatalf("title changed by a rejected body: %v", page["room"].(map[string]any)["title"])
	}

	code, out = rawCall(t, ts, "POST", "/api/rooms/"+id+"/participants", []byte(`{"provider":"claude","role":"교차 검수"}`))
	if code != 201 {
		t.Fatalf("UTF-8 body: %d %v", code, out)
	}
	all := seats()
	if len(all) != before+1 || all[len(all)-1].(map[string]any)["role"] != "교차 검수" {
		t.Fatalf("UTF-8 role not kept: %v", all[len(all)-1])
	}
}
