package rtmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fixture serves the existing API shape and event ordering. State changes
// happen under one lock, as on the real HTTP server; SSE and HTTP are independent.
type waitAPI struct {
	mu                                   sync.Mutex
	events                               chan []byte
	posted                               chan struct{}
	polled                               chan struct{}
	streamClosed                         chan struct{}
	messages                             []apiMessage
	queue                                []apiTurn
	seq                                  int64
	posts, polls, bodyReads              int
	finishAt                             int
	live, chain, rapid, slow, disconnect bool
	queuedChain, slowFirst               bool
	holdDone                             chan struct{}
}

func newWaitAPI(t *testing.T) (*waitAPI, *Server) {
	f := &waitAPI{events: make(chan []byte, 256), posted: make(chan struct{}), polled: make(chan struct{}), streamClosed: make(chan struct{}), finishAt: 2}
	return f, newHTTP(t, f.serve, false)
}

func (f *waitAPI) event(kind string, data any) {
	b, _ := json.Marshal(map[string]any{"type": kind, "data": data})
	f.events <- b
}

func (f *waitAPI) add(id, turn, text, status string) apiMessage {
	f.seq++
	m := apiMessage{ID: id, Seq: f.seq, Author: "s1", Text: text, Status: status, TurnID: turn}
	f.messages = append(f.messages, m)
	return m
}

func (f *waitAPI) start(id, group string) {
	q := apiTurn{ID: id, Agent: "s1", Group: group, Cap: f.seq, Status: "queued"}
	f.queue = append(f.queue, q)
	f.event("queue", f.queue)
	f.queue[len(f.queue)-1].Status = "running"
	m := f.add("reply-"+id, id, "응답-"+id, "streaming")
	f.event("message", m)
	f.event("queue", f.queue)
}

func (f *waitAPI) finish(id string) {
	for i := range f.messages {
		if f.messages[i].TurnID == id {
			f.messages[i].Status = "done"
			f.event("message", f.messages[i])
		}
	}
	f.queue = slices.DeleteFunc(f.queue, func(q apiTurn) bool { return q.ID == id })
	f.event("queue", f.queue)
}

func (f *waitAPI) initLive() {
	// The running response is older than the latest snapshot page.
	f.add("old-input", "", "이전 질문", "done")
	f.queue = []apiTurn{{ID: "a", Group: "old-input", Agent: "s1", Status: "running", Cap: 1}}
	f.add("reply-a", "a", "기존 응답", "streaming")
	for range 4 {
		f.add(fmt.Sprintf("other-%d", f.seq), "", "다른 메시지", "done")
	}
}

func (f *waitAPI) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/stream") {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: hello\ndata: 1\n\n")
		w.(http.Flusher).Flush()
		defer close(f.streamClosed)
		for {
			select {
			case <-r.Context().Done():
				return
			case b, ok := <-f.events:
				if !ok {
					return
				}
				if f.holdDone != nil && strings.Contains(string(b), `"turn_id":"a"`) && strings.Contains(string(b), `"status":"done"`) {
					select {
					case <-f.holdDone:
					case <-r.Context().Done():
						return
					}
				}
				fmt.Fprintf(w, "data: %s\n\n", b)
				w.(http.Flusher).Flush()
			}
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/rooms" {
		io.WriteString(w, `[]`)
		return
	}
	if r.Method == "POST" {
		f.posts++
		m := f.add("trigger", "", "질문", "done")
		m.Target, m.Live = "s1", f.live
		f.event("message", m)
		if !f.live {
			f.start("a", m.ID)
		}
		if f.rapid || f.finishAt == 0 {
			f.finish("a")
			if f.chain {
				f.start("b", "reply-a")
			}
		}
		close(f.posted)
		if f.disconnect {
			f.finishAt = 100000
			close(f.events)
		}
		_ = json.NewEncoder(w).Encode(m)
		return
	}
	limit := r.URL.Query().Get("limit")
	if f.posts > 0 && limit == "1" {
		f.polls++
		if f.polls == 1 {
			close(f.polled)
		}
		if f.slowFirst || (f.slow && f.polls > 1) {
			f.mu.Unlock()
			<-r.Context().Done()
			f.mu.Lock()
			return
		}
		if f.polls == f.finishAt {
			if f.rapid {
				f.finish("b")
			} else {
				f.finish("a")
				if f.chain {
					if f.queuedChain {
						f.queue = append(f.queue, apiTurn{ID: "b", Agent: "s1", Group: "reply-a", Status: "queued"})
						f.event("queue", f.queue)
					} else {
						f.start("b", "reply-a")
					}
				}
			}
		}
		if f.chain && !f.rapid && f.polls == f.finishAt+1 {
			if f.holdDone != nil {
				close(f.holdDone)
			}
			if f.queuedChain {
				m := f.add("reply-b", "b", "대기 후 응답", "done")
				f.event("message", m)
			}
			f.finish("b")
			f.start("c", "reply-b")
		}
		if f.chain && !f.rapid && f.polls == f.finishAt+2 {
			f.finish("c")
		}
	}
	if f.posts > 0 && limit == "200" {
		f.bodyReads++
	}
	messages := f.messages
	if limit == "1" && len(messages) > 1 {
		messages = messages[len(messages)-1:]
	}
	// Respect before pagination for the pre-existing steering reply search.
	if r.URL.Query().Get("before") != "" {
		var before int64
		fmt.Sscan(r.URL.Query().Get("before"), &before)
		messages = nil
		for _, m := range f.messages {
			if m.Seq < before {
				messages = append(messages, m)
			}
		}
	}
	hasMore := len(messages) > 0 && messages[0].Seq > 1
	var before int64
	if hasMore {
		before = messages[0].Seq
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"room":  map[string]any{"id": "r1", "seq": f.seq, "participants": []seat{{ID: "s1", Label: "검토자", Enabled: true}}},
		"queue": f.queue, "messages": messages,
		"history": map[string]any{"has_more": hasMore, "before_seq": before},
	})
}

func TestPostWait(t *testing.T) {
	for _, tc := range []struct {
		name            string
		seconds, finish int
		timeout         bool
	}{
		{"complete", 2, 2, false}, {"zero-complete", 0, 0, false}, {"zero-timeout", 0, 1000, true}, {"timeout", 1, 1000, true}, {"max-accepted", 600, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s := newWaitAPI(t)
			f.finishAt = tc.finish
			start := time.Now()
			got := call(t, s, "rt_post", fmt.Sprintf(`{"room":"r1","text":"x","wait_seconds":%d}`, tc.seconds))
			if got["seq"] != float64(1) || got["timed_out"] != tc.timeout || len(got["messages"].([]any)) != 1 {
				t.Fatal(got)
			}
			if tc.name == "timeout" && (time.Since(start) < time.Second || time.Since(start) > 2*time.Second) {
				t.Fatal(time.Since(start))
			}
			if f.posts != 1 || f.bodyReads != 1 {
				t.Fatal(f.posts, f.bodyReads)
			}
		})
	}
}

func TestSteeringIncludesOlderReplyAndChain(t *testing.T) {
	f, s := newWaitAPI(t)
	f.live, f.chain = true, true
	f.initLive()
	got := call(t, s, "rt_post", `{"room":"r1","text":"x","target":"s1","wait_seconds":2}`)
	if got["live"] != true || got["timed_out"] != false || f.polls < 4 {
		t.Fatal(got, f.polls)
	}
	messages := got["messages"].([]any)
	if len(messages) != 3 || messages[0].(map[string]any)["seq"] != float64(2) {
		t.Fatal(messages)
	}
	for _, m := range messages {
		if m.(map[string]any)["status"] != "done" {
			t.Fatal(messages)
		}
	}
}

func TestRapidChainBetweenSubmissionAndFirstRead(t *testing.T) {
	f, s := newWaitAPI(t)
	f.chain, f.rapid = true, true
	got := call(t, s, "rt_post", `{"room":"r1","text":"x","wait_seconds":2}`)
	if got["timed_out"] != false || f.polls < 2 || len(got["messages"].([]any)) != 2 {
		t.Fatal(got, f.polls)
	}
	for _, m := range got["messages"].([]any) {
		if m.(map[string]any)["status"] != "done" {
			t.Fatal(got)
		}
	}
}

func TestLiveQueuedChainWithOlderFinalEventDelayed(t *testing.T) {
	f, s := newWaitAPI(t)
	f.live, f.chain, f.queuedChain = true, true, true
	f.holdDone = make(chan struct{})
	f.initLive()
	got := call(t, s, "rt_post", `{"room":"r1","text":"x","target":"s1","wait_seconds":2}`)
	if got["timed_out"] != false || f.polls < 4 || len(got["messages"].([]any)) != 3 {
		t.Fatal(got, f.polls)
	}
}

func TestSlowInitialReadRespectsWaitDeadline(t *testing.T) {
	f, s := newWaitAPI(t)
	f.slowFirst = true
	start := time.Now()
	_, err := s.call(context.Background(), "rt_post", json.RawMessage(`{"room":"r1","text":"x","wait_seconds":1}`))
	if err == nil || !strings.Contains(err.Error(), "seq=1") || time.Since(start) > 2*time.Second || f.posts != 1 {
		t.Fatal(err, time.Since(start), f.posts)
	}
}

func TestPostWaitDeadlineCancelsSlowPoll(t *testing.T) {
	f, s := newWaitAPI(t)
	f.slow, f.finishAt = true, 1000
	start := time.Now()
	got := call(t, s, "rt_post", `{"room":"r1","text":"x","wait_seconds":1}`)
	if got["timed_out"] != true || time.Since(start) > 2*time.Second {
		t.Fatal(got, time.Since(start))
	}
}

func TestPostWaitStreamGapIsExplicitError(t *testing.T) {
	f, s := newWaitAPI(t)
	f.disconnect = true
	_, err := s.call(context.Background(), "rt_post", json.RawMessage(`{"room":"r1","text":"x","wait_seconds":1}`))
	if err == nil || !strings.Contains(err.Error(), "seq=1") || !strings.Contains(err.Error(), "SSE") || f.posts != 1 {
		t.Fatal(err, f.posts)
	}
}

func TestPostWaitMissingStreamDoesNotPost(t *testing.T) {
	posts := 0
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			posts++
		}
		http.Error(w, "unavailable", 404)
	}, false)
	_, err := s.call(context.Background(), "rt_post", json.RawMessage(`{"room":"r1","text":"x","wait_seconds":1}`))
	if err == nil || posts != 0 {
		t.Fatal(err, posts)
	}
}
