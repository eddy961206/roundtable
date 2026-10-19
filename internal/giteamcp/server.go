// Package giteamcp is a read-only MCP (Model Context Protocol) stdio server
// that lets agents look up Gitea issues and pull requests.
//
// Safety model: every request is a GET built by this package from validated
// parameters, against an allow-listed set of repositories, executed through the
// user's own `tea` login. No tool takes a raw URL, method or body, so an agent
// cannot write, comment, merge or reach another repository through it.
package giteamcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
)

const (
	protocolVersion = "2024-11-05"
	maxOutput       = 120 * 1024
)

// Fetcher performs one authenticated GET and returns the raw body.
type Fetcher func(path string) ([]byte, error)

// TeaFetcher runs `tea api --login <login> <path>` (GET is tea's default; no
// method or data flag is ever passed).
func TeaFetcher(tea, login string) Fetcher {
	return func(path string) ([]byte, error) {
		cmd := exec.Command(tea, "api", "--login", login, path)
		hideWindow(cmd)
		out, err := cmd.Output()
		if err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				return nil, fmt.Errorf("tea api %s: %s", path, strings.TrimSpace(string(ee.Stderr)))
			}
			return nil, err
		}
		return out, nil
	}
}

// Server holds the allow-list and the fetcher.
type Server struct {
	repos map[string]bool
	order []string
	fetch Fetcher
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// New validates the allow-list ("Owner/Name" entries).
func New(repos []string, fetch Fetcher) (*Server, error) {
	s := &Server{repos: map[string]bool{}, fetch: fetch}
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		if !repoRe.MatchString(r) {
			return nil, fmt.Errorf("저장소 이름 형식 오류: %q (Owner/Name)", r)
		}
		if !s.repos[r] {
			s.repos[r] = true
			s.order = append(s.order, r)
		}
	}
	if len(s.order) == 0 {
		return nil, fmt.Errorf("허용 저장소가 비어 있음")
	}
	return s, nil
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve speaks newline-delimited JSON-RPC 2.0 until stdin closes.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": nil, "error": rpcError{-32700, "parse error"}}); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // notification (e.g. notifications/initialized)
		}
		result, rerr := s.handle(req)
		resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
		if rerr != nil {
			resp["error"] = rerr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func (s *Server) handle(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "roundtable-gitea", "version": "1"},
			"instructions":    "Gitea 읽기 전용 조회. 허용 저장소: " + strings.Join(s.order, ", "),
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		var p struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{-32602, "invalid params"}
		}
		text, err := s.call(p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func (s *Server) tools() []map[string]any {
	repo := map[string]any{"type": "string", "enum": s.order, "description": "Owner/Name"}
	num := map[string]any{"type": "integer", "minimum": 1}
	obj := func(props map[string]any, required ...string) map[string]any {
		return map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}
	}
	return []map[string]any{
		{"name": "gitea_list", "description": "이슈 또는 PR 목록(번호·제목·상태·작성자·갱신 시각·댓글 수). 최신순 50개씩.",
			"inputSchema": obj(map[string]any{"repo": repo, "kind": map[string]any{"type": "string", "enum": []string{"issues", "pulls"}},
				"state": map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}}, "page": num}, "repo", "kind")},
		{"name": "gitea_search", "description": "이슈·PR 제목과 본문에서 키워드 검색.",
			"inputSchema": obj(map[string]any{"repo": repo, "query": map[string]any{"type": "string", "minLength": 1, "maxLength": 200},
				"state": map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}}}, "repo", "query")},
		{"name": "gitea_issue", "description": "이슈 또는 PR 한 건의 본문과 전체 댓글(번호는 이슈·PR 공통).",
			"inputSchema": obj(map[string]any{"repo": repo, "number": num}, "repo", "number")},
		{"name": "gitea_pull", "description": "PR 한 건의 상태·브랜치·병합 여부·변경 파일 목록·리뷰.",
			"inputSchema": obj(map[string]any{"repo": repo, "number": num}, "repo", "number")},
		{"name": "gitea_pull_diff", "description": "PR 전체 diff(최대 120KB, 넘으면 잘림).",
			"inputSchema": obj(map[string]any{"repo": repo, "number": num}, "repo", "number")},
	}
}

func (s *Server) call(name string, args map[string]json.RawMessage) (string, error) {
	str := func(k string) (string, error) {
		raw, ok := args[k]
		if !ok {
			return "", nil
		}
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("%s 는 문자열이어야 함", k)
		}
		return v, nil
	}
	intArg := func(k string, def int) (int, error) {
		raw, ok := args[k]
		if !ok {
			return def, nil
		}
		var v int
		if err := json.Unmarshal(raw, &v); err != nil || v < 1 {
			return 0, fmt.Errorf("%s 는 1 이상의 정수여야 함", k)
		}
		return v, nil
	}
	repo, err := str("repo")
	if err != nil {
		return "", err
	}
	if !s.repos[repo] {
		return "", fmt.Errorf("허용되지 않은 저장소: %q (허용: %s)", repo, strings.Join(s.order, ", "))
	}
	base := "/repos/" + repo
	state, err := str("state")
	if err != nil {
		return "", err
	}
	if state == "" {
		state = "all"
	}
	if state != "open" && state != "closed" && state != "all" {
		return "", fmt.Errorf("state 는 open|closed|all")
	}
	switch name {
	case "gitea_list":
		kind, err := str("kind")
		if err != nil {
			return "", err
		}
		if kind != "issues" && kind != "pulls" {
			return "", fmt.Errorf("kind 는 issues|pulls")
		}
		page, err := intArg("page", 1)
		if err != nil {
			return "", err
		}
		return s.listing(fmt.Sprintf("%s/issues?type=%s&state=%s&limit=50&page=%d", base, kind, state, page))
	case "gitea_search":
		q, err := str("query")
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(q) == "" || len(q) > 200 {
			return "", fmt.Errorf("query 는 1~200자")
		}
		return s.listing(fmt.Sprintf("%s/issues?state=%s&limit=50&q=%s", base, state, url.QueryEscape(q)))
	case "gitea_issue":
		n, err := intArg("number", 0)
		if err != nil || n == 0 {
			return "", fmt.Errorf("number 는 1 이상의 정수여야 함")
		}
		return s.issue(base, n)
	case "gitea_pull":
		n, err := intArg("number", 0)
		if err != nil || n == 0 {
			return "", fmt.Errorf("number 는 1 이상의 정수여야 함")
		}
		return s.pull(base, n)
	case "gitea_pull_diff":
		n, err := intArg("number", 0)
		if err != nil || n == 0 {
			return "", fmt.Errorf("number 는 1 이상의 정수여야 함")
		}
		b, err := s.fetch(fmt.Sprintf("%s/pulls/%d.diff", base, n))
		if err != nil {
			return "", err
		}
		return clip(string(b)), nil
	}
	return "", fmt.Errorf("알 수 없는 도구: %s", name)
}

func (s *Server) getJSON(path string, v any) error {
	b, err := s.fetch(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s 응답 해석 실패: %w", path, err)
	}
	return nil
}

type user struct {
	Login string `json:"login"`
}

type issueRow struct {
	Number   int    `json:"number"`
	Title    string `json:"title"`
	State    string `json:"state"`
	User     user   `json:"user"`
	Updated  string `json:"updated_at"`
	Created  string `json:"created_at"`
	Comments int    `json:"comments"`
	Body     string `json:"body"`
	PR       *struct {
		Merged bool `json:"merged"`
	} `json:"pull_request"`
}

func (s *Server) listing(path string) (string, error) {
	var rows []issueRow
	if err := s.getJSON(path, &rows); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d건\n", len(rows))
	for _, r := range rows {
		kind := "이슈"
		if r.PR != nil {
			kind = "PR"
			if r.PR.Merged {
				kind = "PR(병합)"
			}
		}
		fmt.Fprintf(&b, "#%d [%s·%s] %s — %s, 갱신 %s, 댓글 %d\n", r.Number, kind, r.State, r.Title, r.User.Login, short(r.Updated), r.Comments)
	}
	return clip(b.String()), nil
}

type comment struct {
	User    user   `json:"user"`
	Body    string `json:"body"`
	Created string `json:"created_at"`
}

func (s *Server) issue(base string, n int) (string, error) {
	var it issueRow
	if err := s.getJSON(fmt.Sprintf("%s/issues/%d", base, n), &it); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n상태 %s · 작성 %s · %s\n\n%s\n", it.Number, it.Title, it.State, it.User.Login, short(it.Created), it.Body)
	for page := 1; ; page++ {
		var cs []comment
		if err := s.getJSON(fmt.Sprintf("%s/issues/%d/comments?limit=50&page=%d", base, n, page), &cs); err != nil {
			return "", err
		}
		for _, c := range cs {
			fmt.Fprintf(&b, "\n--- 댓글 %s · %s\n%s\n", c.User.Login, short(c.Created), c.Body)
		}
		if len(cs) < 50 {
			break
		}
	}
	return clip(b.String()), nil
}

func (s *Server) pull(base string, n int) (string, error) {
	var pr struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		State  string `json:"state"`
		Merged bool   `json:"merged"`
		User   user   `json:"user"`
		Body   string `json:"body"`
		Head   struct {
			Ref string `json:"ref"`
			Sha string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		MergeSha string `json:"merge_commit_sha"`
	}
	if err := s.getJSON(fmt.Sprintf("%s/pulls/%d", base, n), &pr); err != nil {
		return "", err
	}
	var files []struct {
		Name string `json:"filename"`
		Add  int    `json:"additions"`
		Del  int    `json:"deletions"`
	}
	if err := s.getJSON(fmt.Sprintf("%s/pulls/%d/files?limit=100", base, n), &files); err != nil {
		return "", err
	}
	var reviews []struct {
		User  user   `json:"user"`
		State string `json:"state"`
		Body  string `json:"body"`
		At    string `json:"submitted_at"`
	}
	if err := s.getJSON(fmt.Sprintf("%s/pulls/%d/reviews", base, n), &reviews); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "PR #%d %s\n상태 %s · 병합 %v · %s → %s · head %s · merge %s · 작성 %s\n\n%s\n\n변경 파일 %d개\n",
		pr.Number, pr.Title, pr.State, pr.Merged, pr.Head.Ref, pr.Base.Ref, short7(pr.Head.Sha), short7(pr.MergeSha), pr.User.Login, pr.Body, len(files))
	for _, f := range files {
		fmt.Fprintf(&b, "  %s (+%d/-%d)\n", f.Name, f.Add, f.Del)
	}
	for _, r := range reviews {
		fmt.Fprintf(&b, "\n--- 리뷰 %s · %s · %s\n%s\n", r.User.Login, r.State, short(r.At), r.Body)
	}
	return clip(b.String()), nil
}

func short(ts string) string {
	if len(ts) >= 16 {
		return ts[:16]
	}
	return ts
}

func short7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func clip(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	cut := maxOutput
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "\n…[잘림: 출력 상한 120KB]"
}
