package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

func (s *Server) api(ctx context.Context, method, path string, body, dest any) error {
	var input io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.base+path, input)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	const maxBody = 32 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("%s %s 응답: %w", method, path, err)
	}
	if len(b) > maxBody {
		return fmt.Errorf("%s %s: 응답이 32MiB를 초과함", method, path)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	if err := json.Unmarshal(b, dest); err != nil {
		return fmt.Errorf("%s %s 응답 해석 실패: %w", method, path, err)
	}
	return nil
}

type seat struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Role     string `json:"role"`
	Enabled  bool   `json:"enabled"`
	Removed  bool   `json:"removed"`
}

type apiMessage struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	Author string `json:"author"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
	Text   string `json:"text"`
	Role   string `json:"role"`
	Error  string `json:"error"`
	Live   bool   `json:"live"`
	TurnID string `json:"turn_id"`
	Target string `json:"target"`
}

type page struct {
	Room *struct {
		ID           string `json:"id"`
		Seq          int64  `json:"seq"`
		Title        string `json:"title"`
		Mode         string `json:"mode"`
		Participants []seat `json:"participants"`
	} `json:"room"`
	Messages []apiMessage      `json:"messages"`
	Queue    []apiTurn         `json:"queue"`
	Requests []json.RawMessage `json:"requests"`
	History  struct {
		HasMore   bool  `json:"has_more"`
		BeforeSeq int64 `json:"before_seq"`
	} `json:"history"`
}

func (s *Server) snapshot(ctx context.Context, room string, before int64, limit int) (*page, error) {
	path := "/api/rooms/" + room + "?limit=" + strconv.Itoa(limit)
	if before > 0 {
		path += "&before=" + strconv.FormatInt(before, 10)
	}
	var p page
	if err := s.api(ctx, "GET", path, nil, &p); err != nil {
		return nil, err
	}
	if p.Room == nil || p.Room.ID != room {
		return nil, fmt.Errorf("%s: 방 응답의 id가 요청과 다름", path)
	}
	return &p, nil
}

func (s *Server) listRooms(ctx context.Context) (any, error) {
	var rooms []struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
	}
	if err := s.api(ctx, "GET", "/api/rooms", nil, &rooms); err != nil {
		return nil, err
	}
	rows := make([]any, 0, len(rooms))
	for _, room := range rooms {
		// Room summaries omit seats. The bounded snapshot supplies them.
		if !regexpID.MatchString(room.ID) {
			return nil, fmt.Errorf("방 목록의 id 형식 오류")
		}
		p, err := s.snapshot(ctx, room.ID, 0, 1)
		if err != nil {
			return nil, err
		}
		if p.Room.Participants == nil {
			p.Room.Participants = []seat{}
		}
		rows = append(rows, map[string]any{"id": room.ID, "title": p.Room.Title, "mode": p.Room.Mode, "participants": p.Room.Participants, "active": room.Active})
	}
	return map[string]any{"rooms": rows}, nil
}

type readMessage struct {
	Seq       int64  `json:"seq"`
	Author    string `json:"author"`
	Name      string `json:"name"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	Text      string `json:"text"`
	Error     string `json:"error,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

type readResult struct {
	Room         string        `json:"room"`
	Messages     []readMessage `json:"messages"`
	NextSeq      int64         `json:"next_seq"`
	Truncated    bool          `json:"truncated"`
	OmittedChars int64         `json:"omitted_chars"`
}

func (s *Server) read(ctx context.Context, room string, after, maxChars int64) (*readResult, error) {
	latest, err := s.snapshot(ctx, room, 0, 200)
	if err != nil {
		return nil, err
	}
	return s.readPages(ctx, room, after, maxChars, latest)
}

func (s *Server) readPages(ctx context.Context, room string, after, maxChars int64, latest *page) (*readResult, error) {
	return s.readSelected(ctx, room, after, after, maxChars, latest, nil)
}

func (s *Server) readSelected(ctx context.Context, room string, after, lower, maxChars int64, latest *page, includeTurns map[string]bool) (*readResult, error) {
	names := map[string]string{"user": "user", "system": "system"}
	for _, p := range latest.Room.Participants {
		names[p.ID] = p.Label
	}
	result := &readResult{Room: room, NextSeq: after, Messages: []readMessage{}}
	current := latest
	seen := map[int64]bool{}
	var before int64
	for {
		oldest := int64(0)
		for _, m := range current.Messages {
			if m.Seq < 1 {
				return nil, fmt.Errorf("메시지 seq가 올바르지 않음")
			}
			if oldest == 0 || m.Seq < oldest {
				oldest = m.Seq
			}
			if (m.Seq <= after && !includeTurns[m.TurnID]) || seen[m.Seq] {
				continue
			}
			seen[m.Seq] = true
			result.NextSeq = max(result.NextSeq, m.Seq)
			result.Messages = append(result.Messages, readMessage{Seq: m.Seq, Author: m.Author, Name: names[m.Author], Role: m.Role, Status: m.Status, Text: m.Text, Error: m.Error})
		}
		if !current.History.HasMore {
			break
		}
		if oldest == 0 {
			return nil, fmt.Errorf("대화 페이지가 비었는데 has_more가 켜져 있음")
		}
		if oldest <= lower {
			break
		}
		next := current.History.BeforeSeq
		if oldest == 0 || next != oldest || (before > 0 && next >= before) {
			return nil, fmt.Errorf("대화 페이지의 before_seq가 진행하지 않음")
		}
		before = next
		var err error
		current, err = s.snapshot(ctx, room, before, 200)
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(result.Messages, func(i, j int) bool { return result.Messages[i].Seq < result.Messages[j].Seq })
	remaining := maxChars
	for i := len(result.Messages) - 1; i >= 0; i-- {
		m := &result.Messages[i]
		chars := int64(utf8.RuneCountInString(m.Text))
		if chars > remaining {
			result.Truncated, m.Truncated = true, true
			result.OmittedChars += chars - remaining
			if remaining == 0 {
				m.Text = ""
			} else {
				m.Text = string([]rune(m.Text)[chars-remaining:])
			}
			remaining = 0
		} else {
			remaining -= chars
		}
	}
	return result, nil
}

type postResult struct {
	Seq  int64 `json:"seq"`
	Live bool  `json:"live,omitempty"`
	*readResult
	TimedOut *bool `json:"timed_out,omitempty"`
}

func (s *Server) post(ctx context.Context, a arguments) (any, error) {
	var watch *observer
	if a.WaitSeconds != nil {
		var err error
		watch, err = s.observe(ctx, a.Room)
		if err != nil {
			return nil, err
		}
		defer watch.close()
		baseline, err := s.snapshot(ctx, a.Room, 0, 1)
		if err != nil {
			return nil, err
		}
		watch.seed(baseline)
		if err := s.seedRunningReply(ctx, a.Room, a.Target, baseline, watch); err != nil {
			return nil, err
		}
	}
	var sent apiMessage
	if err := s.api(ctx, "POST", "/api/rooms/"+a.Room+"/messages", map[string]string{"text": a.Text, "target": a.Target}, &sent); err != nil {
		return nil, err
	}
	if sent.ID == "" || sent.Seq < 1 {
		return nil, fmt.Errorf("전송 응답의 id 또는 seq가 없음; 재전송 전에 방을 조회해")
	}
	result := &postResult{Seq: sent.Seq, Live: sent.Live}
	if a.WaitSeconds == nil {
		return result, nil
	}
	deadline := time.Now().Add(time.Duration(*a.WaitSeconds) * time.Second)
	initialCtx := ctx
	initialCancel := func() {}
	if *a.WaitSeconds > 0 {
		initialCtx, initialCancel = context.WithDeadline(ctx, deadline)
	}
	latest, err := s.snapshot(initialCtx, a.Room, 0, 1)
	initialCancel()
	if err != nil {
		return nil, fmt.Errorf("메시지 seq=%d 전송됨; 대기 조회 실패(재전송하지 말 것): %w", sent.Seq, err)
	}
	timedOut := false
	var related map[string]bool
	lower := sent.Seq
	for {
		// The HTTP snapshot supplies current activity; SSE supplies durable-for-
		// this-call links even when ancestors finished between two snapshots.
		syncCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if *a.WaitSeconds > 0 {
			cancel()
			syncCtx, cancel = context.WithDeadline(ctx, deadline)
		}
		err := watch.catchUp(syncCtx, latest, sent)
		cancel()
		if err != nil {
			if related != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				timedOut = true
				break
			}
			return nil, fmt.Errorf("메시지 seq=%d 전송됨; SSE 동기화 실패(재전송하지 말 것): %w", sent.Seq, err)
		}
		var pending bool
		related, lower, pending, err = watch.related(latest, sent)
		if err != nil {
			return nil, fmt.Errorf("메시지 seq=%d 전송됨; 턴 추적 실패: %w", sent.Seq, err)
		}
		if !pending {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			timedOut = true
			break
		}
		timer := time.NewTimer(min(remaining, 100*time.Millisecond))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("메시지 seq=%d 전송됨; 대기 중단: %w", sent.Seq, ctx.Err())
		case <-timer.C:
		}
		if time.Until(deadline) <= 0 {
			timedOut = true
			break
		}
		pollCtx, cancel := context.WithDeadline(ctx, deadline)
		observed, err := s.snapshot(pollCtx, a.Room, 0, 1)
		cancel()
		if err != nil {
			if ctx.Err() == nil && time.Until(deadline) <= 0 {
				timedOut = true
				break
			}
			return nil, fmt.Errorf("메시지 seq=%d 전송됨; 대기 조회 실패(재전송하지 말 것): %w", sent.Seq, err)
		}
		latest = observed
	}
	// Fetch bodies once after waiting. Include the existing steering reply even
	// when its sequence precedes the submitted user message.
	latest, err = s.snapshot(ctx, a.Room, 0, 200)
	if err != nil {
		return nil, fmt.Errorf("메시지 seq=%d 전송됨; 답변 조회 실패: %w", sent.Seq, err)
	}
	read, err := s.readSelected(ctx, a.Room, sent.Seq, lower, 20000, latest, related)
	if err != nil {
		return nil, fmt.Errorf("메시지 seq=%d 전송됨; 답변 조회 실패(재전송하지 말 것): %w", sent.Seq, err)
	}
	result.readResult, result.TimedOut = read, &timedOut
	return result, nil
}
