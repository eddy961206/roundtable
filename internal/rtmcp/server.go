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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const protocolVersion = "2024-11-05"

// instructions is what an MCP-only agent learns before reading any tool
// description; the full guide lives in docs/AGENT_GUIDE.md.
const instructions = "Roundtable: 서로 다른 공급사의 AI CLI 좌석이 한 방에서 일하는 로컬 서버의 HTTP API.\n" +
	"- 작업 하나에 방 하나를 쓴다. 작성 좌석과 다른 공급사의 검수 좌석을 같은 방에 두고 [[REQUEST:@좌석ID]]로 직접 주고받게 한다. 오케스트레이터는 검수 원문을 옮기지 말고 판정 줄과 head만 읽는다. 끝난 방에는 더 보내지 않는다.\n" +
	"- 좌석은 이름이 아니라 id로 부른다. target 없이 보낸 메시지는 켜진 좌석 전원을 부른다.\n" +
	"- 여러 방은 wait_seconds 없이 rt_post하고 받은 seq로 rt_wait(rooms=방:seq,...)를 부른다. 폴링하지 않는다. 판정만 필요하면 rt_wait summary:true. 다른 방의 답은 rt_forward로 원문 그대로 넘긴다(읽고 다시 쓰지 않는다).\n" +
	"- 좌석 세션이 길어지면(문맥 경보, 작업이 바뀌는 지점) rt_handoff로 인계 요약을 남기고 새 세션으로 바꾼다. 모델 호출마다 문맥 전체를 다시 읽으므로 긴 세션은 턴마다 비싸진다.\n" +
	"- 409 작업 폴더 사용 중은 같은 폴더의 다른 방이 끝난 뒤 다시 보낸다. 좌석의 held_until(한도 오류)·budget_held(입력 예산) 보류 안내가 있으면 같은 좌석으로 곧장 재시도하지 않는다.\n" +
	"- rt_post의 timed_out은 대기만 종료하며 실행 중인 턴을 취소하지 않는다. timed_out이면 같은 메시지를 다시 보내지 말고 결과의 resume_after_seq를 after_seq로 주어 rt_read로 이어 읽는다(live:true면 기존 답의 seq가 보낸 seq보다 작아 보낸 seq로는 그 답이 빠진다).\n" +
	"- 사람이 진행 중인 방에서 토론 시작·요청 취소·결정 확정처럼 흐름을 바꾸는 호출은 먼저 사람에게 확인받는다.\n" +
	"- 좌석 추가·max_chain·좌석 켜기는 HTTP 전용이다. 기능 색인·전달 규칙·공급사별 능력·실패 대처는 Roundtable 저장소의 docs/AGENT_GUIDE.md(https://github.com/eddy961206/roundtable/blob/main/docs/AGENT_GUIDE.md)를 읽는다."

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
	transport.ResponseHeaderTimeout = 15 * time.Second
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

// Serve uses one reader and one writer. Tool calls run independently so a long
// wait cannot block ping, cancellation or another tool. EOF cancels pending calls.
func (s *Server) Serve(in io.Reader, out io.Writer) error {
	type input struct {
		req     rpcRequest
		err     *rpcError
		scanErr error
	}
	inputs := make(chan input)
	quit := make(chan struct{})
	defer close(quit)
	go func() {
		defer close(inputs)
		scanner := bufio.NewScanner(in)
		scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var req rpcRequest
			var rpcErr *rpcError
			if !json.Valid(line) {
				rpcErr = &rpcError{-32700, "parse error"}
			} else if err := json.Unmarshal(line, &req); err != nil || line[0] != '{' || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
				rpcErr = &rpcError{-32600, "invalid request"}
			}
			select {
			case inputs <- input{req: req, err: rpcErr}:
			case <-quit:
				return
			}
		}
		if err := scanner.Err(); err != nil {
			select {
			case inputs <- input{scanErr: err}:
			case <-quit:
			}
		}
	}()
	type runningCall struct{ cancel context.CancelFunc }
	type reply struct {
		req    rpcRequest
		call   *runningCall
		result any
		err    *rpcError
	}
	active := map[string]*runningCall{}
	replies := make(chan reply)
	var workers sync.WaitGroup
	defer func() {
		for _, call := range active {
			call.cancel()
		}
		if closer, ok := in.(io.Closer); ok {
			_ = closer.Close()
		}
		workers.Wait()
	}()
	enc := json.NewEncoder(out)
	write := func(id any, result any, rpcErr *rpcError) error {
		resp := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			resp["error"] = rpcErr
		} else {
			resp["result"] = result
		}
		return enc.Encode(resp)
	}
	for {
		select {
		case item, ok := <-inputs:
			if !ok {
				return nil
			}
			if item.scanErr != nil {
				return item.scanErr
			}
			req := item.req
			if item.err != nil {
				if err := write(nil, nil, item.err); err != nil {
					return err
				}
				continue
			}
			if len(req.ID) == 0 {
				if req.Method == "notifications/cancelled" {
					var p struct {
						RequestID json.RawMessage `json:"requestId"`
					}
					if json.Unmarshal(req.Params, &p) == nil && len(p.RequestID) > 0 && validID(p.RequestID) {
						key := rpcIDKey(p.RequestID)
						if call := active[key]; call != nil {
							delete(active, key)
							call.cancel()
						}
					}
				}
				continue
			}
			key := rpcIDKey(req.ID)
			if active[key] != nil {
				if err := write(req.ID, nil, &rpcError{-32600, "request id already running"}); err != nil {
					return err
				}
				continue
			}
			if req.Method != "tools/call" {
				result, rpcErr := s.handle(context.Background(), req)
				if err := write(req.ID, result, rpcErr); err != nil {
					return err
				}
				continue
			}
			ctx, cancel := context.WithCancel(context.Background())
			call := &runningCall{cancel: cancel}
			active[key] = call
			workers.Add(1)
			go func() {
				defer workers.Done()
				result, rpcErr := s.handle(ctx, req)
				select {
				case replies <- reply{req, call, result, rpcErr}:
				case <-ctx.Done():
				}
			}()
		case reply := <-replies:
			key := rpcIDKey(reply.req.ID)
			if active[key] != reply.call {
				continue
			} // cancelled or id reused
			delete(active, key)
			reply.call.cancel()
			if err := write(reply.req.ID, reply.result, reply.err); err != nil {
				return err
			}
		}
	}
}

func rpcIDKey(id json.RawMessage) string {
	var s string
	if json.Unmarshal(id, &s) == nil {
		return "s:" + s
	}
	var n int64
	_ = json.Unmarshal(id, &n)
	return "n:" + strconv.FormatInt(n, 10)
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

func (s *Server) handle(ctx context.Context, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "roundtable", "version": "1"},
			"instructions":    instructions,
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
		value, err := s.call(ctx, p.Name, p.Arguments)
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
