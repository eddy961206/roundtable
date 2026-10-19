// Package rtmcp exposes the existing Roundtable HTTP API as a stdio MCP server.
package rtmcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const protocolVersion = "2024-11-05"

type Server struct {
	base     string
	readOnly bool
	client   *http.Client
}

// New only accepts a loopback HTTP origin, without credentials, paths or query.
// Redirects and proxies are disabled so requests cannot leave that origin.
func New(base string, readOnly bool) (*Server, error) {
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("url 해석 실패: %w", err)
	}
	ip := net.ParseIP(u.Hostname())
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		(u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback())) ||
		u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("url은 인증 정보·경로·쿼리가 없는 loopback HTTP 주소여야 함")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("loopback 외 연결 거절: %s", address)
		}
		return nil
	}}
	transport.DialContext = dialer.DialContext
	return &Server{base: strings.TrimSuffix(base, "/"), readOnly: readOnly, client: &http.Client{
		Timeout: 15 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
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

// Serve follows gitea-mcp's newline-delimited JSON-RPC transport. Stdout is
// reserved for protocol messages. Notifications never invoke HTTP tools.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	enc := json.NewEncoder(out)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var req rpcRequest
		var result any
		var rpcErr *rpcError
		var id any
		if !json.Valid(line) {
			rpcErr = &rpcError{-32700, "parse error"}
		} else if err := json.Unmarshal(line, &req); err != nil || line[0] != '{' || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
			rpcErr = &rpcError{-32600, "invalid request"}
		} else {
			if len(req.ID) == 0 {
				continue
			}
			id = req.ID
			result, rpcErr = s.handle(req)
		}
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 {
		return true
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(id))
	dec.UseNumber()
	if dec.Decode(&value) != nil {
		return false
	}
	switch v := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := v.Int64()
		return err == nil
	}
	return false
}

func (s *Server) handle(req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "roundtable", "version": "1"},
			"instructions":    "Roundtable HTTP API. rt_post의 timed_out은 대기만 종료하며 실행 중인 턴을 취소하지 않는다.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": s.tools()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if len(req.Params) == 0 || req.Params[0] != '{' || json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
			return nil, &rpcError{-32602, "invalid params"}
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage(`{}`)
		}
		value, err := s.call(context.Background(), p.Name, p.Arguments)
		if err != nil {
			return map[string]any{"content": []map[string]any{{"type": "text", "text": err.Error()}}, "isError": true}, nil
		}
		b, err := json.Marshal(value)
		if err != nil {
			return nil, &rpcError{-32603, err.Error()}
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": string(b)}}}, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}
