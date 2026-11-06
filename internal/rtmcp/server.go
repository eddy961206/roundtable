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
// description; the full guide lives in docs/AGENT_GUIDE.md. Serial position:
// the rule that saves the most tokens comes first and the one that protects
// people comes last; the middle does not repeat tool descriptions.
const instructions = "Roundtable: 서로 다른 공급사의 AI CLI 좌석이 한 방에서 일하는 로컬 서버의 HTTP API.\n" +
	"- 작업 하나에 방 하나를 쓴다. 작성 좌석과 다른 공급사의 검수 좌석을 같은 방에 두고 [[REQUEST:@좌석ID]]로 직접 주고받게 한다. 오케스트레이터는 원문을 옮기지 않고 판정 줄만 읽는다(rt_wait summary:true, 다른 방 답은 rt_forward). 끝난 방에는 보내지 않고 rt_archive_rooms로 보관한다.\n" +
	"- 좌석은 이름이 아니라 id로 부른다. target 없이 보낸 메시지는 켜진 좌석 전원을 부른다.\n" +
	"- 여러 방은 wait_seconds 없이 rt_post하고 받은 seq로 rt_wait(rooms=방:seq,...)를 부른다. 폴링하지 않는다.\n" +
	"- rt_wait의 user_requested:true는 좌석의 사용자 요청입니다. user_requests의 내용·상태를 읽고 rt_resolve_user_request의 text 또는 acknowledge:true로 처리하세요. 바로 답할 수 없으면 결과의 waiting을 다음 rooms에 그대로 쓰세요. 세 번째 seq는 이미 받은 요청 알림의 수신 위치여서 반복 반환을 막습니다. 커서 없는 호출은 기존 미처리 요청도 알립니다. asked:true인 턴 중 질문은 기존 rt_answer로 답합니다.\n" +
	"- timed_out은 대기만 끝낸 것이다. 같은 메시지를 다시 보내지 말고 결과의 resume_after_seq를 after_seq로 주어 rt_read로 이어 읽는다.\n" +
	"- 좌석이 보류되면(held_until·budget_held·paced_until, 409 작업 폴더 사용 중) 같은 좌석으로 곧장 재시도하지 않고 사유의 다음 행동을 따른다. 세션이 길어지면 rt_handoff.\n" +
	"- 좌석 안내문에는 '모두 반말' 규칙이 있다. 외부에 게시할 초안을 맡길 때는 지시에 문체를 따로 적는다.\n" +
	"- 좌석 추가·좌석 켜기는 HTTP 전용이다. 세부는 Roundtable 저장소의 docs/AGENT_GUIDE.md(https://github.com/eddy961206/roundtable/blob/main/docs/AGENT_GUIDE.md) 맨 앞 '읽는 법'에서 필요한 절만 읽는다.\n" +
	"- 사람이 진행 중인 방이나 공간에서 흐름을 바꾸는 호출(토론 시작·요청 취소·결정 확정·과제 배정·권한을 묻는 좌석 질문에 답하기)은 먼저 사람에게 확인받는다."

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
