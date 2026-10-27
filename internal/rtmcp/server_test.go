package rtmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func newHTTP(t *testing.T, handler http.HandlerFunc, readOnly bool) *Server {
	t.Helper()
	h := httptest.NewServer(handler)
	t.Cleanup(h.Close)
	s, err := New(h.URL, readOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.client.CloseIdleConnections)
	return s
}

func call(t *testing.T, s *Server, name, args string) map[string]any {
	t.Helper()
	v, err := s.call(context.Background(), name, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func snapshotBody(messages string, queue string) string {
	return `{"room":{"id":"r1","title":"논의","mode":"chat","participants":[{"id":"s1","label":"검토자","provider":"codex","model":"chosen-model","role":"정확성"}]},"messages":` + messages + `,"queue":` + queue + `,"requests":[{"id":"q1","status":"pending"}],"history":{"has_more":false}}`
}

func TestToolsHTTP(t *testing.T) {
	var paths []string
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /api/rooms":
			io.WriteString(w, `[{"id":"r1","active":true}]`)
		case "GET /api/rooms/r1":
			io.WriteString(w, snapshotBody(`[{"id":"m2","seq":2,"author":"s1","role":"당시 역할","status":"done","text":"답"}]`, `[]`))
		case "POST /api/rooms":
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, ok := body["participants"]; ok {
				t.Error("must use participant_specs")
			}
			var participants []map[string]any
			if err := json.Unmarshal(body["participant_specs"], &participants); err != nil {
				t.Error(err)
			}
			want := []map[string]any{{"provider": "codex", "model": "chosen-model", "effort": "", "label": "검토자", "role": "정확성", "fork_from": map[string]any{"room": "base", "seat": "codex-1"}}}
			if !reflect.DeepEqual(participants, want) {
				t.Errorf("participants: %v", participants)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"created","participants":[{"id":"codex-2","label":"검토자","provider":"codex","model":"chosen-model","role":"정확성","enabled":true}]}`)
		case "POST /api/rooms/r1/messages":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["text"] != "질문" || body["target"] != "s1" {
				t.Errorf("post body: %v", body)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"m1","seq":1}`)
		case "POST /api/rooms/r1/discussion":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(body, map[string]any{"rounds": float64(2), "blind_first": false}) {
				t.Errorf("discussion body: %v", body)
			}
			io.WriteString(w, `{"ok":true}`)
		case "POST /api/rooms/r1/requests/q1/cancel":
			io.WriteString(w, `{"id":"q1","status":"cancelled"}`)
		case "GET /api/rooms/r1/decisions":
			io.WriteString(w, `[{"id":"d1","text":"채택","evidence":"메시지 2","dissent":"없음","unverified":"실행","by":"s1","source_msg":"m2","status":"active","at":1}]`)
		case "PATCH /api/rooms/r1/decisions/d1":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if !reflect.DeepEqual(body, map[string]any{"status": "active", "dissent": ""}) {
				t.Errorf("decision patch body: %v", body)
			}
			io.WriteString(w, `{"id":"d1","text":"채택","status":"active","dissent":""}`)
		default:
			t.Errorf("unexpected path: %s", r.URL)
			http.NotFound(w, r)
		}
	}, false)
	rooms := call(t, s, "rt_list_rooms", `{}`)["rooms"].([]any)
	row := rooms[0].(map[string]any)
	if row["id"] != "r1" || row["title"] != "논의" || row["mode"] != "chat" || row["active"] != true {
		t.Fatalf("rooms: %v", row)
	}
	seat := row["participants"].([]any)[0].(map[string]any)
	if _, role := seat["role"]; seat["id"] != "s1" || seat["label"] != "검토자" || seat["provider"] != "codex" || seat["model"] != "chosen-model" || role {
		t.Fatalf("seat: %v", seat)
	}
	created := call(t, s, "rt_create_room", `{"title":"논의","workspace":"","mode":"chat","participants":[{"provider":"codex","model":"chosen-model","effort":"","label":"검토자","role":"정확성","fork_from":{"room":"base","seat":"codex-1"}}]}`)
	wantCreated := map[string]any{"room": "created", "participants": []any{map[string]any{"id": "codex-2", "label": "검토자", "provider": "codex", "model": "chosen-model", "enabled": true}}}
	if !reflect.DeepEqual(created, wantCreated) {
		t.Fatal(created)
	}
	if got := call(t, s, "rt_post", `{"room":"r1","text":"질문","target":"s1"}`); !reflect.DeepEqual(got, map[string]any{"seq": float64(1)}) {
		t.Fatal(got)
	}
	read := call(t, s, "rt_read", `{"room":"r1","after_seq":1}`)
	m := read["messages"].([]any)[0].(map[string]any)
	if m["seq"] != float64(2) || m["name"] != "검토자" || m["role"] != "당시 역할" || m["status"] != "done" || m["text"] != "답" || read["truncated"] != false {
		t.Fatal(read)
	}
	if got := call(t, s, "rt_discuss", `{"room":"r1","rounds":2,"blind_first":false}`); got["ok"] != true {
		t.Fatal(got)
	}
	if got := call(t, s, "rt_requests", `{"room":"r1"}`); got["requests"].([]any)[0].(map[string]any)["status"] != "pending" {
		t.Fatal(got)
	}
	if got := call(t, s, "rt_cancel_request", `{"room":"r1","request":"q1"}`); got["status"] != "cancelled" {
		t.Fatal(got)
	}
	d := call(t, s, "rt_decisions", `{"room":"r1"}`)["decisions"].([]any)[0].(map[string]any)
	if d["unverified"] != "실행" || d["status"] != "active" {
		t.Fatal(d)
	}
	if got := call(t, s, "rt_update_decision", `{"room":"r1","id":"d1","status":"active","dissent":""}`); got["status"] != "active" {
		t.Fatal(got)
	}
	if len(paths) != 10 {
		t.Fatalf("requests: %v", paths)
	}
}

func TestInputValidationDoesNotCallHTTP(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid input reached HTTP") }, false)
	cases := []struct{ name, args string }{
		{"rt_list_rooms", `{"extra":1}`}, {"rt_list_rooms", `null`}, {"rt_list_rooms", `[]`},
		{"rt_create_room", `{}`}, {"rt_create_room", `{"title":" ","workspace":"","mode":"chat","participants":[{"provider":"codex"}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"read","participants":[{"provider":"codex"}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"other","participants":[{"provider":"codex"}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"chat","participants":[]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"chat","participants":[{"provider":"codex","count":2}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"chat","participants":[{"provider":"codex","effort":null}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"chat","participants":[{"provider":"codex","fork_from":{"room":"base"}}]}`},
		{"rt_create_room", `{"title":"x","workspace":"","mode":"chat","participants":[{"provider":"codex","fork_from":{"room":"base","seat":"../x"}}]}`},
		{"rt_post", `{"room":"r1","text":" "}`}, {"rt_post", `{"room":"r1","text":"x","wait_seconds":601}`},
		{"rt_post", `{"room":"r1","text":"x","wait_seconds":-1}`}, {"rt_post", `{"room":"r1","text":"x","wait_seconds":0.5}`},
		{"rt_post", `{"room":"r1","text":"x","wait_seconds":null}`}, {"rt_post", `{"room":"r1","text":"x","target":"../stop"}`},
		{"rt_read", `{"room":"r1","after_seq":-1}`}, {"rt_read", `{"room":"r1","max_chars":0}`},
		{"rt_read", `{"room":"r1","after_seq":9223372036854775808}`}, {"rt_read", `{"room":"r1","max_chars":"4"}`},
		{"rt_discuss", `{"room":"r1","rounds":0}`}, {"rt_discuss", `{"room":"r1","rounds":1,"blind_first":1}`},
		{"rt_requests", `{"room":"../x"}`}, {"rt_cancel_request", `{"room":"r1"}`}, {"rt_cancel_request", `{"room":"r1","request":"q?x"}`},
		{"rt_decisions", `{"room":null}`}, {"rt_decisions", `{"room":1}`}, {"unknown", `{}`},
		{"rt_list_rooms", `{"limit":0}`}, {"rt_list_rooms", `{"limit":101}`},
		{"rt_update_decision", `{"room":"r1","id":"d1"}`}, {"rt_update_decision", `{"room":"r1","status":"active"}`},
		{"rt_update_decision", `{"room":"r1","id":"d1","status":"proposed"}`}, {"rt_update_decision", `{"room":"r1","id":"d1","text":" "}`},
		{"rt_update_decision", `{"room":"r1","id":"../d1","status":"dropped"}`}, {"rt_update_decision", `{"room":"r1","id":"d1","by":"user"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			if _, err := s.call(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}

// A range error names the accepted bounds so a script can fix its argument
// instead of mistaking the failure for an empty result.
func TestIntegerRangeErrorNamesBounds(t *testing.T) {
	s := newHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid input reached HTTP") }, false)
	_, err := s.call(context.Background(), "rt_list_rooms", json.RawMessage(`{"limit":200}`))
	if err == nil || !strings.Contains(err.Error(), "(1~100)") {
		t.Fatalf("want bounds in range error, got %v", err)
	}
}

func TestReadOnly(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("write in read-only: %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/rooms":
			io.WriteString(w, `[]`)
		case "/api/rooms/r1":
			io.WriteString(w, snapshotBody(`[]`, `[]`))
		case "/api/rooms/r1/decisions":
			io.WriteString(w, `[]`)
		case "/api/version":
			io.WriteString(w, `{"state":"behind","behind_by":2,"update_available":true,"commits":[]}`)
		default:
			t.Error(r.URL)
		}
	}, true)
	var names []string
	for _, tool := range s.tools() {
		names = append(names, tool.Name)
		if !tool.Annotations["readOnlyHint"] {
			t.Fatal(tool)
		}
	}
	want := []string{"rt_list_rooms", "rt_read", "rt_wait", "rt_usage", "rt_version", "rt_requests", "rt_decisions"}
	if !reflect.DeepEqual(names, want) {
		t.Fatal(names)
	}
	for _, name := range []string{"rt_list_rooms", "rt_read", "rt_requests", "rt_decisions"} { // rt_wait and rt_usage: wait_rooms_test.go
		args := `{"room":"r1"}`
		if name == "rt_list_rooms" {
			args = `{}`
		}
		call(t, s, name, args)
	}
	for _, name := range []string{"rt_create_room", "rt_post", "rt_discuss", "rt_cancel_request", "rt_update_decision", "rt_apply_markers", "rt_cancel_turn", "rt_forward"} {
		if _, err := s.call(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("write exposed: %s", name)
		}
	}
	if v := call(t, s, "rt_version", `{}`); v["state"] != "behind" || v["update_available"] != true || v["behind_by"] != float64(2) {
		t.Fatal(v)
	}
	if _, err := s.call(context.Background(), "rt_version", json.RawMessage(`{"room":"r1"}`)); err == nil {
		t.Fatal("rt_version accepted an unknown argument")
	}
}

func TestReadPaginationAndUnicodeBudget(t *testing.T) {
	var pages []string
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("before"))
		if r.URL.Query().Get("before") == "" {
			io.WriteString(w, strings.Replace(snapshotBody(`[{"seq":3,"author":"s1","status":"done","text":"가나다"},{"seq":4,"author":"s1","status":"streaming","text":"🌕별"}]`, `[]`), `"has_more":false`, `"has_more":true,"before_seq":3`, 1))
		} else if r.URL.Query().Get("before") == "3" {
			io.WriteString(w, snapshotBody(`[{"seq":1,"author":"user","status":"done","text":"옛말"},{"seq":2,"author":"s1","status":"error","text":"오래된답","error":"failed"}]`, `[]`))
		} else {
			t.Error(r.URL)
		}
	}, false)
	// The budget fills from the newest message. seq 3 crosses it and keeps its
	// end; seq 2 is left out whole and only counted.
	got := call(t, s, "rt_read", `{"room":"r1","after_seq":1,"max_chars":4}`)
	if got["truncated"] != true || got["next_seq"] != float64(4) || got["omitted_chars"] != float64(1) || got["omitted_messages"] != float64(1) || got["first_seq"] != float64(3) {
		t.Fatal(got)
	}
	m := got["messages"].([]any)
	if len(m) != 2 || m[0].(map[string]any)["text"] != "나다" || m[0].(map[string]any)["truncated"] != true || m[1].(map[string]any)["text"] != "🌕별" {
		t.Fatal(m)
	}
	if !reflect.DeepEqual(pages, []string{"", "3"}) {
		t.Fatal(pages)
	}
	pages = nil
	got = call(t, s, "rt_read", `{"room":"r1","after_seq":4}`)
	if _, ok := got["first_seq"]; len(got["messages"].([]any)) != 0 || len(pages) != 1 || got["next_seq"] != float64(4) || ok {
		t.Fatal(got, pages)
	}
}

// Without after_seq the read starts from the newest message and stops paging
// once the budget is spent; the history total counts what it did not read.
func TestReadWithoutAfterSeqStopsAtBudget(t *testing.T) {
	var pages []string
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		before := r.URL.Query().Get("before")
		pages = append(pages, before)
		switch before {
		case "":
			io.WriteString(w, strings.Replace(snapshotBody(`[{"id":"m5","seq":5,"author":"user","status":"done","text":"질문"},{"id":"m6","seq":6,"author":"s1","status":"done","text":"답변","reply_to":"m5"}]`, `[]`), `"has_more":false`, `"has_more":true,"before_seq":5,"total":6`, 1))
		case "5":
			io.WriteString(w, strings.Replace(snapshotBody(`[{"id":"m3","seq":3,"author":"user","status":"done","text":"옛 질문"},{"id":"m4","seq":4,"author":"s1","status":"done","text":"옛 답"}]`, `[]`), `"has_more":false`, `"has_more":true,"before_seq":3,"total":6`, 1))
		default:
			t.Error(r.URL)
		}
	}, false)
	got := call(t, s, "rt_read", `{"room":"r1","max_chars":3}`)
	m := got["messages"].([]any)
	if len(m) != 2 || m[0].(map[string]any)["text"] != "문" || m[1].(map[string]any)["reply_to"] != float64(5) {
		t.Fatal(m)
	}
	if got["omitted_messages"] != float64(4) || got["first_seq"] != float64(5) || got["next_seq"] != float64(6) || got["omitted_chars"] != float64(1) {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(pages, []string{""}) {
		t.Fatalf("paged past the spent budget: %v", pages)
	}
	// A budget that fits the first page reads on, and stops at the next one.
	pages = nil
	got = call(t, s, "rt_read", `{"room":"r1","max_chars":6}`)
	if len(got["messages"].([]any)) != 3 || got["omitted_messages"] != float64(3) || got["first_seq"] != float64(4) || !reflect.DeepEqual(pages, []string{"", "5"}) {
		t.Fatal(got, pages)
	}
}

func TestReadMessageFieldsOnlyWhenSet(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, snapshotBody(`[
			{"id":"m1","seq":1,"kind":"user","author":"user","status":"done","text":"봐 줘","target":"s1","attachments":[{"id":"a1","name":"화면.png","mime":"image/png","bytes":10}]},
			{"id":"m2","seq":2,"kind":"agent","author":"s1","status":"done","text":"봤어","reply_to":"m1","requests":["q1"],"decisions":["d1"],"turn_id":"t1"}]`, `[]`))
	}, false)
	got := call(t, s, "rt_read", `{"room":"r1"}`)
	m := got["messages"].([]any)
	question, answer := m[0].(map[string]any), m[1].(map[string]any)
	if question["kind"] != "user" || question["target"] != "s1" || !reflect.DeepEqual(question["attachments"], []any{map[string]any{"id": "a1", "name": "화면.png"}}) {
		t.Fatal(question)
	}
	for _, key := range []string{"reply_to", "requests", "decisions", "truncated", "error"} {
		if _, ok := question[key]; ok {
			t.Fatalf("empty %s present: %v", key, question)
		}
	}
	if answer["reply_to"] != float64(1) || !reflect.DeepEqual(answer["requests"], []any{"q1"}) || !reflect.DeepEqual(answer["decisions"], []any{"d1"}) {
		t.Fatal(answer)
	}
	for _, key := range []string{"target", "attachments", "turn_id", "id"} {
		if _, ok := answer[key]; ok {
			t.Fatalf("%s present: %v", key, answer)
		}
	}
	if _, ok := got["omitted_messages"]; ok {
		t.Fatal(got)
	}
}

// reply_to is resolved through older pages; a trigger missing from the whole
// history is a contract failure, not a silently dropped field.
func TestReadReplyToResolvesThroughOlderPages(t *testing.T) {
	missing := false
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("before") == "" {
			trigger := "m1"
			if missing {
				trigger = "gone"
			}
			io.WriteString(w, strings.Replace(snapshotBody(`[{"id":"m2","seq":2,"author":"s1","status":"done","text":"답","reply_to":"`+trigger+`"}]`, `[]`), `"has_more":false`, `"has_more":true,"before_seq":2,"total":2`, 1))
			return
		}
		io.WriteString(w, snapshotBody(`[{"id":"m1","seq":1,"author":"user","status":"done","text":"질문"}]`, `[]`))
	}, false)
	got := call(t, s, "rt_read", `{"room":"r1","after_seq":1}`)
	if m := got["messages"].([]any); len(m) != 1 || m[0].(map[string]any)["reply_to"] != float64(1) {
		t.Fatal(got)
	}
	missing = true
	if _, err := s.call(context.Background(), "rt_read", json.RawMessage(`{"room":"r1","after_seq":1}`)); err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatal(err)
	}
}

func TestReadDefaultBudget(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		message, _ := json.Marshal(map[string]any{"seq": 1, "author": "s1", "text": strings.Repeat("한", 20001), "status": "done"})
		io.WriteString(w, snapshotBody("["+string(message)+"]", `[]`))
	}, false)
	got := call(t, s, "rt_read", `{"room":"r1"}`)
	text := got["messages"].([]any)[0].(map[string]any)["text"].(string)
	if utf8.RuneCountInString(text) != 20000 || got["omitted_chars"] != float64(1) {
		t.Fatal(len(text), got["omitted_chars"])
	}
}

func TestEmptyRoomList(t *testing.T) {
	// The existing API encodes a nil slice as null when there are no rooms.
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `null`) }, false)
	if got := call(t, s, "rt_list_rooms", `{}`); len(got["rooms"].([]any)) != 0 {
		t.Fatal(got)
	}
}

// Removed seats cannot be addressed, so the list leaves them out; a stopped
// seat stays with enabled:false.
func TestRoomListLeavesOutRemovedSeats(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/rooms" {
			io.WriteString(w, `[{"id":"r1","active":false}]`)
			return
		}
		io.WriteString(w, `{"room":{"id":"r1","participants":[{"id":"active","enabled":true},{"id":"stopped","enabled":false},{"id":"archived","enabled":false,"removed":true}]}}`)
	}, false)
	seats := call(t, s, "rt_list_rooms", `{}`)["rooms"].([]any)[0].(map[string]any)["participants"].([]any)
	if len(seats) != 2 || seats[0].(map[string]any)["enabled"] != true || seats[1].(map[string]any)["id"] != "stopped" || seats[1].(map[string]any)["enabled"] != false {
		t.Fatal(seats)
	}
}

func TestRoomListDefaultLimit(t *testing.T) {
	snapshots := 0
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/rooms" {
			var rooms []string
			for i := range defaultListRooms + 5 {
				rooms = append(rooms, fmt.Sprintf(`{"id":"r%d"}`, i))
			}
			io.WriteString(w, "["+strings.Join(rooms, ",")+"]")
			return
		}
		snapshots++
		io.WriteString(w, `{"room":{"id":"`+strings.TrimPrefix(r.URL.Path, "/api/rooms/")+`","participants":[]}}`)
	}, false)
	if rooms := call(t, s, "rt_list_rooms", `{}`)["rooms"].([]any); len(rooms) != defaultListRooms || snapshots != defaultListRooms {
		t.Fatal(len(rooms), snapshots)
	}
}

func TestDiscussionDefaultIsOmitted(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, ok := body["blind_first"]; ok {
			t.Error("must preserve room default")
		}
		io.WriteString(w, `{"ok":true}`)
	}, false)
	call(t, s, "rt_discuss", `{"room":"r1","rounds":1}`)
}

func TestHTTPFailuresAndMalformedPages(t *testing.T) {
	for _, body := range []string{`null`, `{}`, `<html>bad</html>`, `{"room":{"id":"wrong"}}`, strings.Replace(snapshotBody(`[]`, `[]`), `"has_more":false`, `"has_more":true,"before_seq":1`, 1)} {
		t.Run(body, func(t *testing.T) {
			s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) }, false)
			if _, err := s.call(context.Background(), "rt_read", json.RawMessage(`{"room":"r1"}`)); err == nil {
				t.Fatal("bad HTTP accepted")
			}
		})
	}
	for _, code := range []int{301, 404, 500} {
		s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(code)
			io.WriteString(w, `{"error":"explicit failure"}`)
		}, false)
		_, err := s.call(context.Background(), "rt_decisions", json.RawMessage(`{"room":"r1"}`))
		if err == nil || !strings.Contains(err.Error(), fmt.Sprint(code)) {
			t.Fatal(err)
		}
	}
}

func TestURLSecurity(t *testing.T) {
	for _, base := range []string{"", "http://example.com", "http://0.0.0.0:8787", "http://192.0.2.1", "ftp://127.0.0.1", "http://user:secret@localhost", "http://localhost/api", "http://localhost?", "http://localhost?q=x", "http://localhost#x"} {
		if _, err := New(base, false); err == nil {
			t.Errorf("accepted %q", base)
		}
	}
	for _, base := range []string{"http://127.0.0.1:8787", "http://localhost:8787/", "http://[::1]:8787", "https://127.0.0.1"} {
		if _, err := New(base, false); err != nil {
			t.Error(base, err)
		}
	}
	hits := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer target.Close()
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}, false)
	if _, err := s.call(context.Background(), "rt_list_rooms", json.RawMessage(`{}`)); err == nil || hits != 0 {
		t.Fatal(err, hits)
	}
}

func TestStdioProtocol(t *testing.T) {
	hits := 0
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) { hits++; io.WriteString(w, `[]`) }, false)
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":"list","method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"rt_list_rooms","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"rt_post","arguments":{"room":"r1"}}}`,
		`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"rt_post","arguments":{"room":"r1","text":"must not execute"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":6,"method":"unknown"}`,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":[]}`,
		`{"jsonrpc":"2.0","id":{},"method":"ping"}`,
		`{"jsonrpc":"1.0","id":8,"method":"ping"}`,
		`{bad`, `null`, `[]`,
	}, "\n")
	// An MCP client keeps stdin open until responses arrive. EOF is shutdown,
	// so feeding a finite reader would intentionally cancel outstanding tools.
	in, send := io.Pipe()
	out, receive := io.Pipe()
	defer send.Close()
	defer out.Close()
	defer receive.Close()
	done := make(chan error, 1)
	go func() { done <- s.Serve(in, receive); _ = receive.Close() }()
	written := make(chan error, 1)
	go func() { _, err := fmt.Fprintln(send, input); written <- err }()
	decoded := make(chan map[string]any, 16)
	decodeErr := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(out)
		for {
			var response map[string]any
			if err := decoder.Decode(&response); err != nil {
				decodeErr <- err
				return
			}
			decoded <- response
		}
	}()
	var responses []map[string]any
	for len(responses) < 12 {
		select {
		case response := <-decoded:
			responses = append(responses, response)
		case err := <-decodeErr:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("stdio response did not arrive")
		}
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	_ = send.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not stop at EOF")
	}
	if len(responses) != 12 || hits != 1 {
		t.Fatal(len(responses), hits, responses)
	}
	byID := map[string]map[string]any{}
	codes := map[int]int{}
	for _, response := range responses {
		byID[fmt.Sprint(response["id"])] = response
		if e, ok := response["error"].(map[string]any); ok {
			codes[int(e["code"].(float64))]++
		}
	}
	initialized := byID["1"]["result"].(map[string]any)
	if initialized["protocolVersion"] != protocolVersion {
		t.Fatal(byID["1"])
	}
	// The two SKILL.md rules reach agents that only see the MCP instructions.
	if text, _ := initialized["instructions"].(string); !strings.Contains(text, "다시 보내지 말고") || !strings.Contains(text, "resume_after_seq를 after_seq로") || !strings.Contains(text, "사람에게 확인받는다") {
		t.Fatal(initialized["instructions"])
	}
	if len(byID["list"]["result"].(map[string]any)["tools"].([]any)) != 16 {
		t.Fatal(byID["list"])
	}
	content := byID["3"]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != `{"rooms":[]}` {
		t.Fatal(content)
	}
	if byID["4"]["result"].(map[string]any)["isError"] != true {
		t.Fatal(byID["4"])
	}
	if !reflect.DeepEqual(codes, map[int]int{-32600: 4, -32700: 1, -32601: 1, -32602: 1}) {
		t.Fatal(codes)
	}
}

func TestApplyMarkersPostsSeq(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/rooms/r1/markers" || string(body) != `{"seq":27}` {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, body)
		}
		io.WriteString(w, `{"message":{"seq":27,"requests":["q1"]}}`)
	}, false)
	got := call(t, s, "rt_apply_markers", `{"room":"r1","seq":27}`)
	if got["message"].(map[string]any)["requests"].([]any)[0] != "q1" {
		t.Fatal(got)
	}
	if _, err := s.call(context.Background(), "rt_apply_markers", json.RawMessage(`{"room":"r1"}`)); err == nil {
		t.Fatal("seq is required")
	}
}

// 2026-10-27: stopping a duplicate turn through participants/{id}/stop also
// turned the seat off, so a later [[REQUEST]] to it was refused. rt_cancel_turn
// cancels only the seat's running turn.
func TestCancelTurnKeepsSeat(t *testing.T) {
	var calls []string
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/rooms/r1":
			io.WriteString(w, snapshotBody(`[]`, `[{"id":"t0","agent":"s1","status":"queued"},{"id":"t1","agent":"s1","status":"running"},{"id":"t2","agent":"s2","status":"running"}]`))
		case "DELETE /api/rooms/r1/turns/t1":
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}, false)
	got := call(t, s, "rt_cancel_turn", `{"room":"r1","seat":"s1"}`)
	if got["cancelled"] != "t1" || got["seat_enabled"] != true {
		t.Fatal(got)
	}
	for _, c := range calls {
		if strings.Contains(c, "/stop") || strings.Contains(c, "participants") {
			t.Fatalf("seat stop called: %v", calls)
		}
	}
	calls = nil
	s2 := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, snapshotBody(`[]`, `[{"id":"t0","agent":"s1","status":"queued"}]`))
	}, false)
	if _, err := s2.call(context.Background(), "rt_cancel_turn", json.RawMessage(`{"room":"r1","seat":"s1"}`)); err == nil || !strings.Contains(err.Error(), "실행 중") {
		t.Fatalf("no running turn must be an error: %v", err)
	}
}

func TestForwardPostsSourceAndTarget(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != "POST" || r.URL.Path != "/api/rooms/to1/forward" || string(body) != `{"from_room":"from1","note":"이대로 고쳐","seq":27,"target":"w1"}` {
			t.Errorf("%s %s %s", r.Method, r.URL.Path, body)
		}
		io.WriteString(w, `{"id":"m","seq":41}`)
	}, false)
	got := call(t, s, "rt_forward", `{"room":"to1","from_room":"from1","seq":27,"target":"w1","note":"이대로 고쳐"}`)
	if got["seq"] != float64(41) {
		t.Fatal(got)
	}
	if _, err := s.call(context.Background(), "rt_forward", json.RawMessage(`{"room":"to1","seq":27}`)); err == nil {
		t.Fatal("from_room is required")
	}
}

func TestReadCarriesErrorKind(t *testing.T) {
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, snapshotBody(`[{"id":"m1","seq":1,"author":"s1","kind":"agent","status":"error","text":"half","error":"턴 시간 상한 45분을 넘어 중단됨","error_kind":"turn_timeout"}]`, `[]`))
	}, true)
	m := call(t, s, "rt_read", `{"room":"r1"}`)["messages"].([]any)[0].(map[string]any)
	if m["error_kind"] != "turn_timeout" || m["status"] != "error" {
		t.Fatal(m)
	}
}
