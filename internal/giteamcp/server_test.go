package giteamcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeGitea struct{ paths []string }

func (f *fakeGitea) fetch(_ context.Context, path string) ([]byte, error) {
	f.paths = append(f.paths, path)
	switch {
	case strings.HasPrefix(path, "/repos/Acme/app/issues?"):
		return []byte(`[{"number":12,"title":"atomic save","state":"closed","user":{"login":"a"},"updated_at":"2026-10-19T01:02:03Z","comments":2,"pull_request":{"merged":true}}]`), nil
	case path == "/repos/Acme/app/issues/3":
		return []byte(`{"number":3,"title":"질문","state":"open","user":{"login":"b"},"created_at":"2026-10-10T00:00:00Z","body":"본문"}`), nil
	case strings.HasPrefix(path, "/repos/Acme/app/issues/3/comments"):
		return []byte(`[{"user":{"login":"c"},"body":"결정: Q-1 = 예","created_at":"2026-10-11T00:00:00Z"}]`), nil
	case path == "/repos/Acme/app/pulls/12.diff":
		return []byte("diff --git a/x b/x\n"), nil
	}
	return nil, nil
}

func rpc(t *testing.T, s *Server, reqs ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(strings.NewReader(strings.Join(reqs, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatal(err)
		}
		res = append(res, m)
	}
	return res
}

func text(m map[string]any) (string, bool) {
	r := m["result"].(map[string]any)
	c := r["content"].([]any)[0].(map[string]any)
	isErr, _ := r["isError"].(bool)
	return c["text"].(string), isErr
}

func TestProtocolAndTools(t *testing.T) {
	f := &fakeGitea{}
	s, err := New([]string{"Acme/app", "Acme/spec"}, f.fetch)
	if err != nil {
		t.Fatal(err)
	}
	res := rpc(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"gitea_list","arguments":{"repo":"Acme/app","kind":"pulls"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"gitea_issue","arguments":{"repo":"Acme/app","number":3}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"gitea_pull_diff","arguments":{"repo":"Acme/app","number":12}}}`,
	)
	if len(res) != 5 {
		t.Fatalf("notification must not get a reply; got %d replies", len(res))
	}
	if res[0]["result"].(map[string]any)["protocolVersion"] != protocolVersion {
		t.Fatal("initialize")
	}
	if n := len(res[1]["result"].(map[string]any)["tools"].([]any)); n != 5 {
		t.Fatalf("tools %d", n)
	}
	if txt, isErr := text(res[2]); isErr || !strings.Contains(txt, "#12 [PR(병합)·closed] atomic save") {
		t.Fatalf("list: %q", txt)
	}
	if txt, _ := text(res[3]); !strings.Contains(txt, "결정: Q-1 = 예") || !strings.Contains(txt, "본문") {
		t.Fatalf("issue: %q", txt)
	}
	if txt, _ := text(res[4]); !strings.HasPrefix(txt, "diff --git") {
		t.Fatalf("diff: %q", txt)
	}
	if !strings.Contains(f.paths[0], "type=pulls&state=all") {
		t.Fatalf("list path %s", f.paths[0])
	}
}

func TestRejectsAnythingOutsideTheAllowList(t *testing.T) {
	f := &fakeGitea{}
	s, _ := New([]string{"Acme/app"}, f.fetch)
	cases := []string{
		`{"name":"gitea_issue","arguments":{"repo":"Acme/private","number":1}}`,
		`{"name":"gitea_issue","arguments":{"repo":"Acme/app/../private","number":1}}`,
		`{"name":"gitea_issue","arguments":{"repo":"Acme/app","number":0}}`,
		`{"name":"gitea_issue","arguments":{"repo":"Acme/app","number":"1; rm"}}`,
		`{"name":"gitea_list","arguments":{"repo":"Acme/app","kind":"comments"}}`,
		`{"name":"gitea_search","arguments":{"repo":"Acme/app","query":""}}`,
		`{"name":"gitea_post_comment","arguments":{"repo":"Acme/app","number":1}}`,
	}
	for _, c := range cases {
		res := rpc(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":`+c+`}`)
		if _, isErr := text(res[0]); !isErr {
			t.Errorf("must be rejected: %s", c)
		}
	}
	if len(f.paths) != 0 {
		t.Fatalf("rejected calls must not reach Gitea: %v", f.paths)
	}
	res := rpc(t, s, `{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"gitea_search","arguments":{"repo":"Acme/app","query":"a&state=open&x=1"}}}`)
	text(res[0])
	if !strings.Contains(f.paths[0], "q=a%26state%3Dopen%26x%3D1") {
		t.Fatalf("query must be escaped: %s", f.paths[0])
	}
	if _, err := New([]string{"not a repo"}, f.fetch); err == nil {
		t.Fatal("bad allow-list entry must fail")
	}
}
