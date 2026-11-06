package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"
)

var codexRPCTimeout = 30 * time.Second

type codexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *codexRPCError) Error() string { return fmt.Sprintf("Codex RPC %d: %s", e.Code, e.Message) }

type codexPacket struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *codexRPCError  `json:"error"`
	err    error
}

// One process and one stdout reader per Run. Notifications and server requests
// are serviced even while a client RPC is awaiting its response.
type codexRPC struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	stderr    io.ReadCloser
	packets   chan codexPacket
	stopRead  chan struct{}
	done      chan struct{}
	seq       int
	errText   strings.Builder // read only after done
	waitErr   error
	closeOnce sync.Once
}

func startCodexRPC(cmd *exec.Cmd) (*codexRPC, error) {
	r := &codexRPC{cmd: cmd, packets: make(chan codexPacket, 64), stopRead: make(chan struct{}), done: make(chan struct{})}
	var err error
	if r.stdin, err = cmd.StdinPipe(); err != nil {
		return nil, err
	}
	if r.stdout, err = cmd.StdoutPipe(); err != nil {
		r.stdin.Close()
		return nil, err
	}
	if r.stderr, err = cmd.StderrPipe(); err != nil {
		r.stdin.Close()
		r.stdout.Close()
		return nil, err
	}
	hideWindow(cmd)
	if err := cmd.Start(); err != nil {
		r.stdin.Close()
		r.stdout.Close()
		r.stderr.Close()
		return nil, fmt.Errorf("Codex app-server 실행 실패: %w", err)
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		defer close(r.packets)
		scan := bufio.NewScanner(r.stdout)
		scan.Buffer(make([]byte, 64*1024), maxLineBytes)
		for scan.Scan() {
			var p codexPacket
			if err := json.Unmarshal(scan.Bytes(), &p); err != nil {
				p.err = fmt.Errorf("Codex app-server JSON 응답 오류: %w", err)
			}
			select {
			case r.packets <- p:
			case <-r.stopRead:
			}
		}
		if err := scan.Err(); err != nil {
			select {
			case r.packets <- codexPacket{err: err}:
			case <-r.stopRead:
			}
		}
	}()
	go func() {
		defer readers.Done()
		scan := bufio.NewScanner(r.stderr)
		scan.Buffer(make([]byte, 64*1024), maxLineBytes)
		for scan.Scan() {
			remaining := maxStderrBytes - r.errText.Len()
			if remaining > 0 {
				line := scan.Text() + "\n"
				if len(line) > remaining {
					line = line[:remaining]
				}
				r.errText.WriteString(line)
			}
		}
	}()
	go func() {
		readers.Wait()
		r.waitErr = cmd.Wait()
		close(r.done)
	}()
	return r, nil
}

func (r *codexRPC) write(v any) error { return json.NewEncoder(r.stdin).Encode(v) }

func (r *codexRPC) send(method string, params any) (int, error) {
	r.seq++
	return r.seq, r.write(map[string]any{"id": r.seq, "method": method, "params": params})
}

func (r *codexRPC) call(ctx context.Context, method string, params any, observe func(codexPacket)) (json.RawMessage, error) {
	id, err := r.send(method, params)
	if err != nil {
		return nil, err
	}
	timer := time.NewTimer(codexRPCTimeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return nil, fmt.Errorf("Codex %s 응답 시간 초과", method)
		case p, ok := <-r.packets:
			if !ok {
				return nil, fmt.Errorf("Codex app-server가 %s 응답 전에 종료됨", method)
			}
			if p.err != nil {
				return nil, p.err
			}
			if p.Method != "" && len(p.ID) > 0 {
				if err := r.serverRequest(p, observe); err != nil {
					return nil, err
				}
				continue
			}
			if string(p.ID) == fmt.Sprint(id) {
				if p.Error != nil {
					return nil, p.Error
				}
				return p.Result, nil
			}
			if observe != nil {
				observe(p)
			}
		}
	}
}

// The headless permission policy is never. An RPC response is mandatory even
// for an unknown request: ignoring one leaves the provider waiting forever.
func (r *codexRPC) serverRequest(p codexPacket, observe func(codexPacket)) error {
	var result any
	var rpcErr *codexRPCError
	text := "Codex 서버 요청 " + p.Method + ": "
	switch p.Method {
	case "item/commandExecution/requestApproval", "item/fileChange/requestApproval":
		result = map[string]any{"decision": "decline"}
		text += "승인 정책 never에 따라 거절"
	case "item/permissions/requestApproval":
		result = map[string]any{"permissions": map[string]any{}, "scope": "turn"}
		text += "추가 권한을 부여하지 않음"
	case "mcpServer/elicitation/request":
		result = map[string]any{"action": "decline"}
		text += "대화형 MCP 입력을 지원하지 않아 거절"
	case "item/tool/requestUserInput":
		result = map[string]any{"answers": map[string]any{}}
		text += "동기 질문은 빈 응답으로 종료; 사용자 질문은 request_user_input_async 사용"
	case "currentTime/read":
		result = map[string]any{"currentTimeAt": time.Now().Unix()}
		text += "현재 시각 반환"
	default:
		rpcErr = &codexRPCError{Code: -32601, Message: "roundtable does not support server request " + p.Method}
		text += "지원하지 않는 요청으로 오류 응답"
	}
	if observe != nil {
		b, _ := json.Marshal(map[string]string{"text": text})
		observe(codexPacket{Method: "roundtable/serverRequest", Params: b})
	}
	if rpcErr != nil {
		return r.write(map[string]any{"id": p.ID, "error": rpcErr})
	}
	return r.write(map[string]any{"id": p.ID, "result": result})
}

func (r *codexRPC) initialize(ctx context.Context, observe func(codexPacket)) error {
	_, err := r.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "roundtable", "version": "1"},
		"capabilities": map[string]any{"experimentalApi": true},
	}, observe)
	if err != nil {
		return fmt.Errorf("Codex app-server 초기화 실패 (지원 CLI 필요): %w", err)
	}
	return r.write(map[string]any{"method": "initialized", "params": map[string]any{}})
}

func (r *codexRPC) close(ctx context.Context) (string, error) {
	r.closeOnce.Do(func() {
		close(r.stopRead)
		_ = r.stdin.Close()
		timer := time.NewTimer(exitGrace)
		defer timer.Stop()
		lingered := false
		select {
		case <-r.done:
		case <-ctx.Done():
			killTree(r.cmd)
		case <-timer.C:
			lingered = true
			killTree(r.cmd)
		}
		_ = r.stdout.Close()
		_ = r.stderr.Close()
		<-r.done
		if lingered {
			r.errText.WriteString(fmt.Sprintf("\n[roundtable] codex가 완료 뒤 %s 동안 끝나지 않아 프로세스를 정리함", exitGrace))
		}
	})
	return r.errText.String(), r.waitErr
}
