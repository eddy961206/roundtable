package rtmcp

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// waitRooms is "<room>[:<after_seq>],...". Without after_seq a room counts only
// replies newer than its latest message when the wait starts.
var waitRooms = regexp.MustCompile(`^[A-Za-z0-9_-]+(:[0-9]+)?(,[A-Za-z0-9_-]+(:[0-9]+)?)*$`)

var roomList = regexp.MustCompile(`^[A-Za-z0-9_-]+(,[A-Za-z0-9_-]+)*$`)

type waitTarget struct {
	room  string
	after int64
	given bool
}

func parseWaitRooms(s string) ([]waitTarget, error) {
	var out []waitTarget
	seen := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		room, seq, given := strings.Cut(item, ":")
		if seen[room] {
			return nil, fmt.Errorf("rooms에 같은 방이 두 번 있음: %s", room)
		}
		seen[room] = true
		t := waitTarget{room: room, given: given}
		if given {
			n, err := strconv.ParseInt(seq, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("rooms의 after_seq 오류: %s", item)
			}
			t.after = n
		}
		out = append(out, t)
	}
	return out, nil
}

// waitResult lists every room that finished; waiting is the rooms argument
// for the next rt_wait (each ready room's next_seq is in its read result).
// asking holds the unanswered seat questions of the rooms still waiting; asked
// is true when the wait ended early because a seat asked a new one.
type waitResult struct {
	Ready    []*readResult `json:"ready"`
	Waiting  string        `json:"waiting"`
	TimedOut bool          `json:"timed_out"`
	Asked    bool          `json:"asked,omitempty"`
	Asking   []roomAsks    `json:"asking,omitempty"`
}

type roomAsks struct {
	Room string    `json:"room"`
	Asks []askView `json:"asks"`
}

// wait returns once at least one room is idle (no running or queued turn)
// with an agent reply above its after_seq. The room-list SSE feed wakes it;
// a broken feed is an error, never a finished wait.
func (s *Server) wait(ctx context.Context, rooms string, timeout time.Duration, maxChars int64) (*waitResult, error) {
	targets, err := parseWaitRooms(rooms)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	feed, err := s.subscribeRooms(ctx)
	if err != nil {
		return nil, err
	}
	for i, t := range targets {
		p, err := s.snapshot(ctx, t.room, 0, 1)
		if err != nil {
			return nil, err
		}
		if !t.given {
			targets[i].after = p.Room.Seq
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	checked := map[string]int{} // message count at the last read of an idle room
	var fewest map[string]int   // lowest unanswered question count seen per room
	for {
		ready, waiting, asks, err := s.readyRooms(ctx, targets, maxChars, checked)
		if err != nil {
			return nil, err
		}
		asked := false
		if fewest == nil {
			fewest = asks
		}
		for room, n := range asks {
			asked = asked || n > fewest[room]
			fewest[room] = min(fewest[room], n)
		}
		timedOut := false
		if len(ready) == 0 && !asked {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-deadline.C:
				timedOut = true
			case err := <-feed:
				if err != nil {
					return nil, err
				}
				continue
			}
		}
		res := &waitResult{Ready: ready, Waiting: waiting, TimedOut: timedOut, Asked: asked}
		if res.Asking, err = s.asking(ctx, waiting, asks); err != nil {
			return nil, err
		}
		return res, nil
	}
}

// asking lists the unanswered questions of the rooms still waiting.
func (s *Server) asking(ctx context.Context, waiting string, asks map[string]int) ([]roomAsks, error) {
	var out []roomAsks
	for _, item := range strings.Split(waiting, ",") {
		room, _, _ := strings.Cut(item, ":")
		if asks[room] == 0 {
			continue
		}
		p, err := s.snapshot(ctx, room, 0, 1)
		if err != nil {
			return nil, err
		}
		ra := roomAsks{Room: room}
		for _, a := range p.Asks {
			if a.unanswered() {
				ra.Asks = append(ra.Asks, a)
			}
		}
		out = append(out, ra)
	}
	return out, nil
}

// readyRooms also returns each target room's count of unanswered questions.
func (s *Server) readyRooms(ctx context.Context, targets []waitTarget, maxChars int64, checked map[string]int) ([]*readResult, string, map[string]int, error) {
	var rooms []struct {
		ID       string `json:"id"`
		Active   bool   `json:"active"`
		Messages int    `json:"messages"`
		Asks     int    `json:"asks"`
	}
	if err := s.api(ctx, "GET", "/api/rooms", nil, &rooms); err != nil {
		return nil, "", nil, err
	}
	summaries := map[string]int{}
	active := map[string]bool{}
	all := map[string]int{}
	for _, r := range rooms {
		summaries[r.ID], active[r.ID], all[r.ID] = r.Messages, r.Active, r.Asks
	}
	asks := map[string]int{}
	ready := []*readResult{}
	var waiting []string
	for _, t := range targets {
		count, ok := summaries[t.room]
		if !ok {
			return nil, "", nil, fmt.Errorf("방 없음: %s", t.room)
		}
		asks[t.room] = all[t.room]
		done := false
		if !active[t.room] && checked[t.room] != count {
			checked[t.room] = count
			read, err := s.read(ctx, t.room, t.after, maxChars)
			if err != nil {
				return nil, "", nil, err
			}
			for _, m := range read.Messages {
				done = done || (m.Kind == "agent" && m.Status != "streaming")
			}
			if done {
				ready = append(ready, read)
			}
		}
		if !done {
			waiting = append(waiting, t.room+":"+strconv.FormatInt(t.after, 10))
		}
	}
	return ready, strings.Join(waiting, ","), asks, nil
}

// subscribeRooms opens GET /api/stream. The channel receives nil for every
// change signal and one error when the stream ends.
func (s *Server) subscribeRooms(ctx context.Context) (<-chan error, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", s.base+"/api/stream", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := *s.client
	client.Timeout = 0 // the request context owns this stream
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("방 목록 SSE 연결 실패: %w", err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("방 목록 SSE 응답 오류: HTTP %d: %s", resp.StatusCode, b)
	}
	feed := make(chan error, 1)
	go func() {
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if line := scanner.Text(); strings.HasPrefix(line, "data:") && strings.Contains(line, `"rooms"`) {
				select {
				case feed <- nil:
				default: // one pending signal already makes the waiter re-read the list
				}
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		if ctx.Err() != nil {
			return
		}
		// Replace a pending change signal: the waiter must see the failure.
		select {
		case <-feed:
		default:
		}
		feed <- fmt.Errorf("방 목록 SSE 연결 중단; 완료를 확인할 수 없음: %w", err)
	}()
	return feed, nil
}

// usageTotals mirrors the server's normalized UsageTotals.
type usageTotals struct {
	Turns      int     `json:"turns"`
	Errors     int     `json:"errors"`
	Input      float64 `json:"input_tokens"`
	Cached     float64 `json:"cached_tokens"`
	Uncached   float64 `json:"uncached_tokens"`
	Output     float64 `json:"output_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	MaxContext int     `json:"max_context_tokens"`
	DurationMs int64   `json:"duration_ms"`
}

type roomUsage struct {
	Room   string           `json:"room"`
	Title  string           `json:"title"`
	Totals usageTotals      `json:"totals"`
	Seats  []map[string]any `json:"seats"`
}

func (s *Server) usage(ctx context.Context, rooms string) (any, error) {
	out := struct {
		Rooms  []roomUsage `json:"rooms"`
		Totals usageTotals `json:"totals"`
	}{Rooms: []roomUsage{}}
	for _, id := range strings.Split(rooms, ",") {
		var u roomUsage
		if err := s.api(ctx, "GET", "/api/rooms/"+id+"/usage", nil, &u); err != nil {
			return nil, err
		}
		t := &out.Totals
		t.Turns += u.Totals.Turns
		t.Errors += u.Totals.Errors
		t.Input += u.Totals.Input
		t.Cached += u.Totals.Cached
		t.Uncached += u.Totals.Uncached
		t.Output += u.Totals.Output
		t.CostUSD += u.Totals.CostUSD
		t.MaxContext = max(t.MaxContext, u.Totals.MaxContext)
		t.DurationMs += u.Totals.DurationMs
		out.Rooms = append(out.Rooms, u)
	}
	return out, nil
}
