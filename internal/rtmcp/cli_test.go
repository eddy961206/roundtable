package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

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
