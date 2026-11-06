package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"roundtable/internal/room"
	"strings"
	"testing"
)

func TestCLIWaitChunks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		results   []*waitResult
		fail      bool
		wantCalls int
	}{
		{"timeout", []*waitResult{{TimedOut: true, Waiting: "r1:7"}, {TimedOut: true, Waiting: "r1:7"}, {TimedOut: true, Waiting: "r1:7"}}, false, 3},
		{"ready", []*waitResult{{TimedOut: true, Waiting: "r1:7"}, {Ready: []*readResult{{}}}}, false, 2},
		{"asked", []*waitResult{{TimedOut: true, Waiting: "r1:7"}, {Asked: true, Waiting: "r1:7"}}, false, 2},
		{"user request", []*waitResult{{TimedOut: true, Waiting: "r1:7:5"}, {UserRequested: true, Waiting: "r1:7:9"}}, false, 2},
		{"sse error", []*waitResult{{TimedOut: true, Waiting: "r1:7"}}, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			input := map[string]any{"rooms": "r1", "timeout_seconds": int64(5), "max_chars": int64(42), "summary": true}
			value, err := cliWaitChunks(context.Background(), input, 2, func(_ context.Context, name string, raw json.RawMessage) (any, error) {
				var got map[string]any
				if e := json.Unmarshal(raw, &got); e != nil {
					t.Fatal(e)
				}
				wantRoom := "r1"
				if calls > 0 {
					wantRoom = tc.results[calls-1].Waiting
				}
				wantSeconds := float64(2)
				if calls == 2 {
					wantSeconds = 1
				}
				want := map[string]any{"rooms": wantRoom, "timeout_seconds": wantSeconds, "max_chars": float64(42), "summary": true}
				if name != "rt_wait" || !reflect.DeepEqual(got, want) {
					t.Fatalf("call %d: %s %v", calls, name, got)
				}
				calls++
				if tc.fail && calls == tc.wantCalls {
					return nil, errors.New("SSE disconnected")
				}
				return tc.results[calls-1], nil
			})
			if calls != tc.wantCalls || (err != nil) != tc.fail {
				t.Fatalf("calls=%d value=%v err=%v", calls, value, err)
			}
			if !tc.fail && value != tc.results[len(tc.results)-1] {
				t.Fatalf("last result changed: %v", value)
			}
		})
	}
}

func TestCLIWaitChunksWithRealWait(t *testing.T) {
	hub, s := waitHub(t, nil)
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	value, err := s.cliWait(context.Background(), map[string]any{"rooms": r.ID, "timeout_seconds": int64(2)}, 1)
	if err != nil {
		t.Fatal(err)
	}
	result := value.(*waitResult)
	if !result.TimedOut || len(result.Ready) != 0 || result.Waiting != r.ID+":0" {
		t.Fatalf("result: %+v", result)
	}
}

func TestCLIWaitTimeoutLimits(t *testing.T) {
	s := newHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call HTTP") }, false)
	for _, n := range []string{"0", "86401"} {
		code, out, errOut := runCLI(s, "", "wait", "--rooms", "r1", "--timeout_seconds", n)
		if code != 1 || out != "" || !strings.Contains(errOut, "1~86400") {
			t.Errorf("%s: code=%d out=%q err=%q", n, code, out, errOut)
		}
	}
	_, err := s.call(context.Background(), "rt_wait", json.RawMessage(`{"rooms":"r1","timeout_seconds":3601}`))
	if err == nil || !strings.Contains(err.Error(), "1~3600") {
		t.Fatalf("MCP limit: %v", err)
	}
	_, out, _ := runCLI(s, "", "wait", "--help")
	if !strings.Contains(out, "86400초") || !strings.Contains(out, "1~3600초") {
		t.Fatalf("wait help: %s", out)
	}
}

func runCLI(s *Server, stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := s.RunCLI(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestCLITypedArgumentsReachTheSameTool(t *testing.T) {
	var got map[string]any
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		io.WriteString(w, `{"ok":true}`)
	}, false)
	code, out, errOut := runCLI(s, "", "discuss", "--room", "r1", "--rounds=2", "--blind_first", "false")
	if code != 0 || strings.TrimSpace(out) != `{"ok":true}` || errOut != "" {
		t.Fatalf("code=%d out=%q err=%q", code, out, errOut)
	}
	if got["rounds"] != 2.0 || got["blind_first"] != false {
		t.Fatalf("body %v", got)
	}
}

func TestCLIReadsLongTextFromStdin(t *testing.T) {
	var got map[string]any
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			io.WriteString(w, `{"id":"m1","seq":3}`)
			return
		}
		io.WriteString(w, snapshotBody(`[]`, `[]`))
	}, false)
	long := strings.Repeat("긴 본문 \"따옴표\" ", 4000)
	code, _, errOut := runCLI(s, long, "post", "--room", "r1", "--text", "-")
	if code != 0 || got["text"] != long {
		t.Fatalf("code=%d err=%q text len=%d", code, errOut, len(got["text"].(string)))
	}
}

func TestCLIInputErrorsFailBeforeHTTP(t *testing.T) {
	s := newHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call HTTP") }, false)
	for _, args := range [][]string{
		{"post", "--room", "r1"}, // missing required text
		{"post", "--room", "r1", "--text", "a", "--wait_seconds", "x"},
		{"post", "--room", "r1", "--text", "a", "--colour", "red"},
		{"post", "--room", "r1", "--room", "r2", "--text", "a"},
		{"post", "room", "r1"},
		{"post", "--room"},
		{"create_room", "--title", "t", "--workspace", "", "--mode", "chat", "--participants", "[oops"},
		{"post", "--room", "r1", "--text", "-", "--target", "-"},
		{"nope"},
	} {
		code, out, errOut := runCLI(s, "x", args...)
		if code == 0 || out != "" || errOut == "" {
			t.Errorf("%v: code=%d out=%q err=%q", args, code, out, errOut)
		}
	}
}

func TestCLIHelpListsToolsAndArguments(t *testing.T) {
	s := newHTTP(t, func(http.ResponseWriter, *http.Request) { t.Error("must not call HTTP") }, false)
	_, out, _ := runCLI(s, "", "help")
	for _, want := range []string{"list_rooms", "post", "decisions", "roundtable rt <명령> --help"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
	_, out, _ = runCLI(s, "", "create_room", "--help")
	for _, want := range []string{"--mode chat|read|write (필수)", "--participants array(JSON) (필수)", `"provider":"agy"`} {
		if !strings.Contains(out, want) {
			t.Errorf("create_room help missing %q:\n%s", want, out)
		}
	}
	for _, cmd := range []string{"create_room", "update_room"} {
		if _, out, _ = runCLI(s, "", cmd, "--help"); !strings.Contains(out, "--group string") {
			t.Errorf("%s help missing --group:\n%s", cmd, out)
		}
	}
}

func TestListRoomsLimitKeepsNewestAndSkipsOtherSnapshots(t *testing.T) {
	var snapshots []string
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/rooms" {
			io.WriteString(w, `[{"id":"new"},{"id":"mid"},{"id":"old"}]`)
			return
		}
		snapshots = append(snapshots, r.URL.Path)
		io.WriteString(w, `{"room":{"id":"`+strings.TrimPrefix(r.URL.Path, "/api/rooms/")+`","participants":[]}}`)
	}, false)
	code, out, errOut := runCLI(s, "", "list_rooms", "--limit", "2")
	if code != 0 || !strings.Contains(out, `"id":"new"`) || !strings.Contains(out, `"id":"mid"`) || strings.Contains(out, `"id":"old"`) || len(snapshots) != 2 {
		t.Fatalf("code=%d out=%s err=%s snapshots=%v", code, out, errOut, snapshots)
	}
}
