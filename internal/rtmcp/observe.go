package rtmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
)

type apiTurn struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Group  string `json:"group"`
	Status string `json:"status"`
	Cap    int64  `json:"cap"`
}

// Link metadata, not response text or traces, is retained from the existing SSE
// feed. Polling alone loses completed turns and cannot identify live steering.
type messageLink struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	TurnID string `json:"turn_id"`
	Target string `json:"target"`
	Live   bool   `json:"live"`
}

type observer struct {
	mu        sync.Mutex
	seq       int64
	turns     map[string]apiTurn
	messages  map[string]messageLink
	live      map[string]string
	queue     []apiTurn
	queueSeen bool
	err       error
	changed   chan struct{}
	body      io.ReadCloser
	done      chan struct{}
}

func (s *Server) observe(ctx context.Context, room string) (*observer, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/api/rooms/"+room+"/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := *s.client
	client.Timeout = 0 // the request context, not the ordinary 15s timeout, owns this stream
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SSE 연결 실패: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("SSE 응답 오류: HTTP %d: %s", resp.StatusCode, b)
	}
	o := &observer{turns: map[string]apiTurn{}, messages: map[string]messageLink{}, live: map[string]string{}, changed: make(chan struct{}, 1), body: resp.Body, done: make(chan struct{})}
	go o.read()
	return o, nil
}

func (o *observer) close() { _ = o.body.Close(); <-o.done }

func (o *observer) signal() {
	select {
	case o.changed <- struct{}{}:
	default:
	}
}

func (o *observer) read() {
	defer close(o.done)
	scanner := bufio.NewScanner(o.body)
	scanner.Buffer(make([]byte, 64*1024), 32<<20)
	var data []string
	var event string
	var readErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 && event != "hello" {
				if readErr = o.apply([]byte(strings.Join(data, "\n"))); readErr != nil {
					break
				}
			}
			data, event = nil, ""
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		}
	}
	if readErr == nil {
		readErr = scanner.Err()
	}
	if readErr == nil {
		readErr = io.ErrUnexpectedEOF
	}
	o.mu.Lock()
	o.err = fmt.Errorf("SSE 연결 중단; 턴 완료를 확인할 수 없음: %w", readErr)
	o.mu.Unlock()
	o.signal()
}

func (o *observer) apply(b []byte) error {
	var event struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &event); err != nil {
		return fmt.Errorf("SSE JSON 오류: %w", err)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch event.Type {
	case "message":
		var m messageLink
		if err := json.Unmarshal(event.Data, &m); err != nil {
			return err
		}
		if m.ID == "" || m.Seq < 1 {
			return fmt.Errorf("SSE 메시지 id 또는 seq가 없음")
		}
		if m.Live {
			for _, turn := range o.queue {
				if turn.Agent == m.Target && turn.Status == "running" {
					o.live[m.ID] = turn.ID
				}
			}
		}
		o.messages[m.ID] = m
		o.seq = max(o.seq, m.Seq)
	case "queue":
		var turns []apiTurn
		if err := json.Unmarshal(event.Data, &turns); err != nil {
			return err
		}
		for _, turn := range turns {
			if turn.ID == "" {
				return fmt.Errorf("SSE 큐에 턴 id가 없음")
			}
			o.turns[turn.ID] = turn
		}
		o.queue, o.queueSeen = turns, true
	}
	o.signal()
	return nil
}

func (o *observer) seed(p *page) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, turn := range p.Queue {
		o.turns[turn.ID] = turn
	}
	for _, m := range p.Messages {
		if _, exists := o.messages[m.ID]; !exists {
			o.messages[m.ID] = messageLink{ID: m.ID, Seq: m.Seq, TurnID: m.TurnID}
		}
	}
	// A full queue event already covers seats that predate the subscription.
	// Never overwrite a newer event with an HTTP snapshot acquired earlier.
	if !o.queueSeen {
		o.queue = p.Queue
	}
	o.seq = max(o.seq, p.Room.Seq)
}

// A live reply may have opened long before subscribing and below the most
// recent history page. Find that one pre-existing turn once, before posting.
// Turns starting after subscription are linked by their ordered SSE events.
func (s *Server) seedRunningReply(ctx context.Context, room, target string, baseline *page, watch *observer) error {
	var id string
	for _, turn := range baseline.Queue {
		if turn.Agent == target && turn.Status == "running" {
			id = turn.ID
		}
	}
	if id == "" {
		return nil
	}
	current := baseline
	var before int64
	for {
		for _, m := range current.Messages {
			if m.TurnID == id {
				watch.seed(current)
				return nil
			}
		}
		next := current.History.BeforeSeq
		if !current.History.HasMore || next < 1 || (before > 0 && next >= before) {
			return fmt.Errorf("기존 실행 턴 %s의 응답을 HTTP 기록에서 찾지 못함", id)
		}
		before = next
		var err error
		current, err = s.snapshot(ctx, room, before, 200)
		if err != nil {
			return err
		}
	}
}

// A post-submission snapshot is atomic on the HTTP server. Catching the stream
// up to its message sequence recovers every turn link created before that read,
// including complete A -> B -> C chains between two polling requests.
func (o *observer) catchUp(ctx context.Context, p *page, sent apiMessage) error {
	for {
		o.mu.Lock()
		err := o.err
		_, seen := o.messages[sent.ID]
		ready := seen && o.seq >= p.Room.Seq
		o.mu.Unlock()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-o.changed:
		}
	}
}

func (o *observer) related(p *page, sent apiMessage) (map[string]bool, int64, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err != nil {
		return nil, 0, false, o.err
	}
	for _, turn := range p.Queue {
		o.turns[turn.ID] = turn
	}
	groups := map[string]bool{sent.ID: true}
	turns := map[string]bool{}
	lower := sent.Seq
	if sent.Live {
		id := o.live[sent.ID]
		turn, ok := o.turns[id]
		if !ok || id == "" {
			return nil, 0, false, fmt.Errorf("SSE에서 steering 대상 턴을 식별하지 못함")
		}
		turns[id] = true
		lower = min(lower, turn.Cap)
	}
	for changed := true; changed; {
		changed = false
		for id, turn := range o.turns {
			if !turns[id] && groups[turn.Group] {
				turns[id], changed = true, true
			}
		}
		for id, m := range o.messages {
			if !groups[id] && turns[m.TurnID] {
				groups[id], changed = true, true
			}
		}
	}
	for _, turn := range p.Queue {
		if turns[turn.ID] {
			return turns, lower, true, nil
		}
	}
	return turns, lower, false, nil
}
