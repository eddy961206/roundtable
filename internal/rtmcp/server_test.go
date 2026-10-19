package rtmcp

import (
	"bytes"
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
			want := []map[string]any{{"provider": "codex", "model": "chosen-model", "effort": "", "label": "검토자", "role": "정확성"}}
			if !reflect.DeepEqual(participants, want) {
				t.Errorf("participants: %v", participants)
			}
			w.WriteHeader(201)
			io.WriteString(w, `{"id":"created"}`)
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
	if seat["id"] != "s1" || seat["label"] != "검토자" || seat["provider"] != "codex" || seat["model"] != "chosen-model" || seat["role"] != "정확성" {
		t.Fatalf("seat: %v", seat)
	}
	if got := call(t, s, "rt_create_room", `{"title":"논의","workspace":"","mode":"chat","participants":[{"provider":"codex","model":"chosen-model","effort":"","label":"검토자","role":"정확성"}]}`); got["room"] != "created" {
		t.Fatal(got)
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
	if len(paths) != 9 {
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
		{"rt_post", `{"room":"r1","text":" "}`}, {"rt_post", `{"room":"r1","text":"x","wait_seconds":601}`},
		{"rt_post", `{"room":"r1","text":"x","wait_seconds":-1}`}, {"rt_post", `{"room":"r1","text":"x","wait_seconds":0.5}`},
		{"rt_post", `{"room":"r1","text":"x","wait_seconds":null}`}, {"rt_post", `{"room":"r1","text":"x","target":"../stop"}`},
		{"rt_read", `{"room":"r1","after_seq":-1}`}, {"rt_read", `{"room":"r1","max_chars":0}`},
		{"rt_read", `{"room":"r1","after_seq":9223372036854775808}`}, {"rt_read", `{"room":"r1","max_chars":"4"}`},
		{"rt_discuss", `{"room":"r1","rounds":0}`}, {"rt_discuss", `{"room":"r1","rounds":1,"blind_first":1}`},
		{"rt_requests", `{"room":"../x"}`}, {"rt_cancel_request", `{"room":"r1"}`}, {"rt_cancel_request", `{"room":"r1","request":"q?x"}`},
		{"rt_decisions", `{"room":null}`}, {"rt_decisions", `{"room":1}`}, {"unknown", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			if _, err := s.call(context.Background(), tc.name, json.RawMessage(tc.args)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
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
	want := []string{"rt_list_rooms", "rt_read", "rt_requests", "rt_decisions"}
	if !reflect.DeepEqual(names, want) {
		t.Fatal(names)
	}
	for _, name := range want {
		args := `{"room":"r1"}`
		if name == "rt_list_rooms" {
			args = `{}`
		}
		call(t, s, name, args)
	}
	for _, name := range []string{"rt_create_room", "rt_post", "rt_discuss", "rt_cancel_request"} {
		if _, err := s.call(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Fatalf("write exposed: %s", name)
		}
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
	got := call(t, s, "rt_read", `{"room":"r1","after_seq":1,"max_chars":4}`)
	if got["truncated"] != true || got["next_seq"] != float64(4) || got["omitted_chars"] != float64(5) {
		t.Fatal(got)
	}
	m := got["messages"].([]any)
	if len(m) != 3 || m[0].(map[string]any)["text"] != "" || m[1].(map[string]any)["text"] != "나다" || m[2].(map[string]any)["text"] != "🌕별" {
		t.Fatal(m)
	}
	if m[0].(map[string]any)["error"] != "failed" || m[0].(map[string]any)["truncated"] != true {
		t.Fatal(m[0])
	}
	if !reflect.DeepEqual(pages, []string{"", "3"}) {
		t.Fatal(pages)
	}
	pages = nil
	got = call(t, s, "rt_read", `{"room":"r1","after_seq":4}`)
	if len(got["messages"].([]any)) != 0 || len(pages) != 1 || got["next_seq"] != float64(4) {
		t.Fatal(got, pages)
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

func TestPostWait(t *testing.T) {
	for _, tc := range []struct {
		name        string
		wait, polls int
		timeout     bool
	}{
		{"complete", 2, 2, false}, {"zero-complete", 0, 0, false}, {"zero-timeout", 0, 1000, true}, {"timeout", 1, 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, posts := 0, 0
			s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					posts++
					io.WriteString(w, `{"id":"trigger","seq":10}`)
					return
				}
				count++
				queue := `[{"group":"unrelated"}]`
				status := "done"
				if count <= tc.polls {
					queue = `[{"group":"trigger"},{"group":"unrelated"}]`
					status = "streaming"
				}
				io.WriteString(w, snapshotBody(fmt.Sprintf(`[{"seq":10,"author":"user","status":"done","text":"질문"},{"seq":11,"author":"s1","status":%q,"text":"답"}]`, status), queue))
			}, false)
			start := time.Now()
			got := call(t, s, "rt_post", fmt.Sprintf(`{"room":"r1","text":"질문","wait_seconds":%d}`, tc.wait))
			elapsed := time.Since(start)
			if got["seq"] != float64(10) || got["timed_out"] != tc.timeout || got["next_seq"] != float64(11) || posts != 1 {
				t.Fatal(got, posts)
			}
			if tc.name == "timeout" && (elapsed < time.Second || elapsed > 2*time.Second) {
				t.Fatal(elapsed)
			}
			if len(got["messages"].([]any)) != 1 {
				t.Fatal(got)
			}
		})
	}
}

func TestPostWaitErrorsKeepSentSequence(t *testing.T) {
	posts := 0
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
			io.WriteString(w, `{"id":"m1","seq":7}`)
			return
		}
		http.Error(w, "unavailable", 503)
	}, false)
	_, err := s.call(context.Background(), "rt_post", json.RawMessage(`{"room":"r1","text":"x","wait_seconds":1}`))
	if err == nil || !strings.Contains(err.Error(), "seq=7") || posts != 1 {
		t.Fatal(err, posts)
	}
}

func TestPostWaitDeadlineCancelsSlowPoll(t *testing.T) {
	polls := 0
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			io.WriteString(w, `{"id":"trigger","seq":10}`)
			return
		}
		polls++
		if polls > 1 {
			<-r.Context().Done()
			return
		}
		io.WriteString(w, snapshotBody(`[{"seq":11,"author":"s1","status":"streaming","text":"관측된 답"}]`, `[{"group":"trigger"}]`))
	}, false)
	start := time.Now()
	got := call(t, s, "rt_post", `{"room":"r1","text":"x","wait_seconds":1}`)
	if got["timed_out"] != true || time.Since(start) > 2*time.Second || polls != 2 {
		t.Fatal(got, time.Since(start), polls)
	}
}

func TestEmptyRoomList(t *testing.T) {
	// The existing API encodes a nil slice as null when there are no rooms.
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `null`) }, false)
	if got := call(t, s, "rt_list_rooms", `{}`); len(got["rooms"].([]any)) != 0 {
		t.Fatal(got)
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
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var responses []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte("\n")) {
		var v map[string]any
		if err := json.Unmarshal(line, &v); err != nil {
			t.Fatal(err)
		}
		responses = append(responses, v)
	}
	if len(responses) != 12 || hits != 1 {
		t.Fatal(len(responses), hits, out.String())
	}
	if responses[0]["result"].(map[string]any)["protocolVersion"] != protocolVersion {
		t.Fatal(responses[0])
	}
	if len(responses[1]["result"].(map[string]any)["tools"].([]any)) != 8 || responses[1]["id"] != "list" {
		t.Fatal(responses[1])
	}
	content := responses[2]["result"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != `{"rooms":[]}` {
		t.Fatal(content)
	}
	if responses[3]["result"].(map[string]any)["isError"] != true {
		t.Fatal(responses[3])
	}
	wantCodes := map[int]int{5: -32601, 6: -32602, 7: -32600, 8: -32600, 9: -32700, 10: -32600, 11: -32600}
	for i, code := range wantCodes {
		if responses[i]["error"].(map[string]any)["code"] != float64(code) {
			t.Fatal(i, responses[i])
		}
	}
}
