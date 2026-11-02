// Package worker runs a remote seat's turns on this PC (docs/사내에이전트_커뮤니티_설계.md).
// It polls the community server with the seat's connection token, runs each
// turn with this PC's own CLI and login, and posts back only the answer, the
// CLI session id and token usage. Nothing else on this PC is sent.
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

// Config is one worker. MaxMode caps the room's mode: a room in write mode
// still runs chat here when the person started the worker with chat.
type Config struct {
	URL       string // community server, e.g. http://host:8790
	Token     string
	Provider  agent.Provider
	Model     string
	Effort    string
	Workspace string // required for read and write
	Scratch   string // the CLI's directory in chat mode
	MaxMode   string
	Once      bool // stop after one turn (tests, a first check)
	Client    *http.Client
	Log       *log.Logger
	retryBase time.Duration // first wait before re-uploading a result; 0 is a second
}

var modeRank = map[string]int{agent.ModeChat: 0, agent.ModeRead: 1, agent.ModeWrite: 2}

// ErrUnauthorized ends the worker: the token was revoked or the seat removed.
var ErrUnauthorized = errors.New("연결 토큰이 거절됨(좌석 삭제·토큰 재발급 확인)")

func (c Config) validate() error {
	u, err := url.Parse(c.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("서버 주소는 http(s)://호스트:포트 형식이어야 함: %q", c.URL)
	}
	if !strings.HasPrefix(c.Token, "rtc_") {
		return errors.New("연결 토큰이 없거나 형식이 다름: 호스트 화면의 원격 좌석에서 발급한 rtc_… 토큰이 필요함")
	}
	if _, ok := modeRank[c.MaxMode]; !ok {
		return fmt.Errorf("mode는 chat·read·write 중 하나: %q", c.MaxMode)
	}
	if c.MaxMode != agent.ModeChat && c.Workspace == "" {
		return errors.New("read·write 모드는 -workspace 폴더가 필요함")
	}
	if c.Provider == nil || !c.Provider.Available() {
		return errors.New("이 PC에서 지정한 CLI를 찾지 못함")
	}
	return nil
}

// Run serves the seat until ctx ends, the token is refused or Once finishes.
// Network failures back off and retry; they never end the worker.
func Run(ctx context.Context, c Config) error {
	if err := c.validate(); err != nil {
		return err
	}
	c.URL = strings.TrimRight(c.URL, "/")
	var me room.RemoteSeatView
	if err := c.call(ctx, http.MethodGet, "/community/v1/me", nil, &me); err != nil {
		return err
	}
	c.Log.Printf("연결됨: 공간 %q(%s) 좌석 @%s(%s) · 이 PC의 %s, 최대 %s 모드", me.RoomTitle, me.Room, me.Seat, me.Label, c.Provider.ID(), c.MaxMode)
	backoff := time.Second
	for ctx.Err() == nil {
		var job room.RemoteJob
		err := c.call(ctx, http.MethodGet, fmt.Sprintf("/community/v1/job?wait=%d", int(room.MaxRemoteWait.Seconds())-5), nil, &job)
		switch {
		case errors.Is(err, ErrUnauthorized):
			return err
		case errors.Is(err, errNoJob):
			backoff = time.Second
			continue
		case err != nil:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			c.Log.Printf("서버 연결 실패, %s 뒤 다시 시도: %v", backoff, err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		if err := c.deliver(ctx, job.Turn, c.execute(ctx, job)); err != nil {
			return err
		}
		if c.Once {
			return nil
		}
	}
	return ctx.Err()
}

// execute runs one turn with this PC's CLI.
func (c Config) execute(ctx context.Context, job room.RemoteJob) room.RemoteResult {
	mode := job.Mode
	if modeRank[mode] > modeRank[c.MaxMode] {
		mode = c.MaxMode
	}
	workspace := c.Workspace
	if mode == agent.ModeChat {
		workspace = ""
	}
	where := "작업 폴더 없음(대화만)"
	if workspace != "" {
		where = "이 PC의 작업 폴더 " + workspace
	}
	prompt := fmt.Sprintf("(이 턴은 참가자 PC의 worker가 %s 모드로 실행합니다. %s.)\n%s", mode, where, job.Prompt)
	timeout := time.Duration(job.TimeoutSeconds) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c.Log.Printf("턴 %s 시작: %q @%s (%s 모드)", job.Turn, job.RoomTitle, job.Seat, mode)
	started := time.Now()
	res, err := c.Provider.Run(runCtx, agent.RunOptions{Prompt: prompt, SessionID: job.SessionID, Model: c.Model, Effort: c.Effort, Mode: mode, Workspace: workspace, ScratchDir: c.Scratch, Timeout: timeout}, func(agent.Event) {})
	out := room.RemoteResult{Text: res.Text, SessionID: res.SessionID, Usage: normalizeUsage(c.Provider.ID(), res.Usage)}
	if err != nil {
		out.Error = err.Error()
	}
	c.Log.Printf("턴 %s 끝: %s, 답 %d자, 오류=%v", job.Turn, time.Since(started).Round(time.Second), len([]rune(res.Text)), err)
	return out
}

// deliver uploads a finished answer until the host takes it or says the turn
// is gone (a 4xx: cancelled or timed out there). A network failure or 5xx
// keeps the answer and retries: polling again would hand back the same claimed
// turn and run it a second time, in write mode repeating its changes (PR #4 review).
func (c Config) deliver(ctx context.Context, turn string, res room.RemoteResult) error {
	wait := c.retryBase
	if wait == 0 {
		wait = time.Second
	}
	for {
		err := c.call(ctx, http.MethodPost, "/community/v1/job/"+url.PathEscape(turn)+"/result", res, nil)
		var status httpError
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrUnauthorized):
			return err
		case errors.As(err, &status) && status.code < 500:
			c.Log.Printf("턴 %s 결과를 서버가 받지 않음(서버에서 취소·시간 초과됐을 수 있음): %v", turn, err)
			return nil
		case ctx.Err() != nil:
			return ctx.Err()
		}
		c.Log.Printf("턴 %s 결과를 올리지 못해 %s 뒤 다시 올림: %v", turn, wait, err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		wait = min(wait*2, time.Minute)
	}
}

// httpError is a refused request; code tells a gone turn (4xx) from a host
// that may come back (5xx).
type httpError struct {
	code int
	msg  string
}

func (e httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.code, e.msg) }

// normalizeUsage reports usage the way non-Codex providers do: Codex counts
// cache reads inside input_tokens, the room counts them beside it.
func normalizeUsage(provider string, usage map[string]any) map[string]any {
	if usage == nil || provider != "codex" {
		return usage
	}
	out := map[string]any{}
	for k, v := range usage {
		out[k] = v
	}
	input, _ := usage["input_tokens"].(float64)
	cached, _ := usage["cached_input_tokens"].(float64)
	out["input_tokens"] = input - cached
	return out
}

var errNoJob = errors.New("no job")

// call sends one request with the token and decodes a JSON answer. 204 is errNoJob.
func (c Config) call(ctx context.Context, method, path string, body, into any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusNoContent:
		return errNoJob
	case resp.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case resp.StatusCode >= 300:
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return httpError{resp.StatusCode, e.Error}
		}
		return httpError{resp.StatusCode, strings.TrimSpace(string(data))}
	}
	if into == nil {
		return nil
	}
	return json.Unmarshal(data, into)
}
