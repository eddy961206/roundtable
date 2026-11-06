package rtmcp

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	httpserver "roundtable/internal/server"
)

func waitHub(t *testing.T, run func(context.Context, agent.RunOptions) (agent.Result, error)) (*room.Hub, *Server) {
	t.Helper()
	p := &scriptedProvider{Provider: agent.NewClaude("__missing_test_cli__"), id: "claude", run: run}
	store, err := room.NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hub, err := room.NewHub(store, agent.NewRegistry(p), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := hub.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	http := httptest.NewServer(httpserver.New(hub, nil, fstest.MapFS{}))
	t.Cleanup(http.Close)
	s, err := New(http.URL, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.client.CloseIdleConnections)
	return hub, s
}

func callJSON(t *testing.T, s *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, _ := json.Marshal(args)
	value, err := s.call(context.Background(), name, raw)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(value)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWaitReturnsTheFirstFinishedRoom(t *testing.T) {
	release := map[string]chan struct{}{"빠름": make(chan struct{}), "느림": make(chan struct{})}
	hub, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
		for key, ch := range release {
			if strings.Contains(opts.Prompt, key) {
				select {
				case <-ch:
					return agent.Result{Text: key + " 답", SessionID: "s", Usage: map[string]any{"input_tokens": 5.0, "cache_read_input_tokens": 95.0, "output_tokens": 3.0}}, nil
				case <-ctx.Done():
					return agent.Result{}, ctx.Err()
				}
			}
		}
		return agent.Result{Text: "?"}, nil
	})
	fast, err := hub.CreateRoom(room.RoomSpec{Title: "빠른 방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	slow, err := hub.CreateRoom(room.RoomSpec{Title: "느린 방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := hub.PostUser(fast.ID, "빠름", "")
	b, _ := hub.PostUser(slow.ID, "느림", "")
	rooms := fast.ID + ":" + strconv.FormatInt(a.Seq, 10) + "," + slow.ID + ":" + strconv.FormatInt(b.Seq, 10)

	done := make(chan map[string]any, 1)
	go func() { done <- callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 5}) }()
	select {
	case got := <-done:
		t.Fatalf("returned before any room finished: %v", got)
	case <-time.After(300 * time.Millisecond):
	}
	close(release["빠름"])
	got := <-done
	ready := got["ready"].([]any)
	first := ready[0].(map[string]any)
	if len(ready) != 1 || first["room"] != fast.ID || got["timed_out"] != false || got["waiting"] != slow.ID+":"+strconv.FormatInt(b.Seq, 10) {
		t.Fatalf("wait result %v", got)
	}
	msgs := first["messages"].([]any)
	if last := msgs[len(msgs)-1].(map[string]any); last["text"] != "빠름 답" || last["kind"] != "agent" {
		t.Fatalf("ready room messages %v", msgs)
	}

	timedOut := callJSON(t, s, "rt_wait", map[string]any{"rooms": got["waiting"], "timeout_seconds": 1})
	if timedOut["timed_out"] != true || len(timedOut["ready"].([]any)) != 0 || timedOut["waiting"] != got["waiting"] {
		t.Fatalf("timeout result %v", timedOut)
	}
	close(release["느림"])
	second := callJSON(t, s, "rt_wait", map[string]any{"rooms": got["waiting"], "timeout_seconds": 5})
	if r := second["ready"].([]any); len(r) != 1 || r[0].(map[string]any)["room"] != slow.ID || second["waiting"] != "" {
		t.Fatalf("second wait %v", second)
	}

	usage := callJSON(t, s, "rt_usage", map[string]any{"rooms": fast.ID + "," + slow.ID})
	totals := usage["totals"].(map[string]any)
	if totals["turns"] != 2.0 || totals["input_tokens"] != 200.0 || totals["cached_tokens"] != 190.0 || totals["uncached_tokens"] != 10.0 || len(usage["rooms"].([]any)) != 2 {
		t.Fatalf("usage %v", usage)
	}
}

// Without after_seq a room counts only replies newer than the wait's start,
// so an already answered room does not end the wait at once.
func TestWaitWithoutSeqIgnoresEarlierReplies(t *testing.T) {
	hub, s := waitHub(t, func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "답", SessionID: "s"}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "먼저", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for snap, _ := hub.Snapshot(r.ID); len(snap.Messages) < 2 || snap.Messages[1].Status != "done"; snap, _ = hub.Snapshot(r.ID) {
		if time.Now().After(deadline) {
			t.Fatal("first reply did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	got := callJSON(t, s, "rt_wait", map[string]any{"rooms": r.ID, "timeout_seconds": 1})
	if got["timed_out"] != true {
		t.Fatalf("an earlier reply ended the wait: %v", got)
	}
}

// 2026-11-04: a seat answered at seq 2 and the server then added a token
// warning at seq 3. rt wait --max_chars 50 spent the whole budget on the
// warning, left the answer out of messages and so never saw the room finish.
func TestWaitSeesReplyBehindSystemNoticeWithSmallMaxChars(t *testing.T) {
	hub, s := waitHub(t, func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "구현을 마쳤어", SessionID: "s", Usage: map[string]any{"input_tokens": 2000.0, "output_tokens": 1.0}}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	warn := int64(1000)
	if _, err := hub.UpdateRoom(r.ID, room.RoomPatch{InputWarnTokens: &warn}); err != nil {
		t.Fatal(err)
	}
	sent, err := hub.PostUser(r.ID, "구현해", "")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for snap, _ := hub.Snapshot(r.ID); len(snap.Messages) < 3; snap, _ = hub.Snapshot(r.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("no token warning after the reply: %+v", snap.Messages)
		}
		time.Sleep(20 * time.Millisecond)
	}
	snap, _ := hub.Snapshot(r.ID)
	if m := snap.Messages; m[1].Kind != "agent" || m[1].Status != "done" || m[2].Kind != "system" || len([]rune(m[2].Text)) <= 50 {
		t.Fatalf("want reply then a warning longer than max_chars: %+v", m)
	}
	for _, summary := range []bool{false, true} {
		got := callJSON(t, s, "rt_wait", map[string]any{"rooms": r.ID + ":" + strconv.FormatInt(sent.Seq, 10), "timeout_seconds": 1, "max_chars": 50, "summary": summary})
		ready := got["ready"].([]any)
		if got["timed_out"] != false || len(ready) != 1 || ready[0].(map[string]any)["next_seq"] != 3.0 || got["waiting"] != "" {
			t.Fatalf("summary=%v: finished room not ready: %v", summary, got)
		}
	}
}

// GET /api/rooms and the message read are two requests. When a turn starts
// between them the room list said idle but the snapshot says active: that
// read must not end the wait, and the turn's end must be re-read
// although no message was added (tools/tla/RtWait.tla ReadyOnlyWhenIdle).
func TestWaitIgnoresAReadThatSawATurnStart(t *testing.T) {
	var mu sync.Mutex
	queue := `[{"id":"t2","agent":"s1","status":"running"}]`
	reads := 0
	signal := make(chan struct{})
	s := newHTTP(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/stream":
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			select {
			case <-signal:
				io.WriteString(w, "data: {\"type\":\"rooms\"}\n\n")
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			}
			<-r.Context().Done()
		case "/api/rooms":
			io.WriteString(w, `[{"id":"r1","active":false,"messages":2}]`)
		case "/api/rooms/r1":
			mu.Lock()
			defer mu.Unlock()
			reads++
			body := snapshotBody(`[{"id":"m1","seq":1,"kind":"user","author":"user","status":"done","text":"질문"},{"id":"m2","seq":2,"kind":"agent","author":"s1","status":"done","text":"답"}]`, queue)
			io.WriteString(w, strings.Replace(body, `{"room":`, `{"active":`+strconv.FormatBool(queue != `[]`)+`,"room":`, 1))
		default:
			t.Error(r.URL)
		}
	}, false)
	got := call(t, s, "rt_wait", `{"rooms":"r1:1","timeout_seconds":1}`)
	if got["timed_out"] != true || len(got["ready"].([]any)) != 0 {
		t.Fatalf("a read that listed a running turn ended the wait: %v", got)
	}
	done := make(chan map[string]any, 1)
	go func() { done <- call(t, s, "rt_wait", `{"rooms":"r1:1","timeout_seconds":5}`) }()
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	queue = `[]`
	mu.Unlock()
	close(signal)
	got = <-done
	if got["timed_out"] != false || len(got["ready"].([]any)) != 1 {
		t.Fatalf("the turn's end with an unchanged message count was not re-read: %v", got)
	}
}

// A post to a running seat is steered into its turn: rt post returns the
// steer's seq, but the answer is that turn's reply, an older seq. rt wait on
// the returned seq used to wait past the answer until it timed out.
func TestWaitFollowsASteerIntoItsTurn(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	hub, s := waitHub(t, func(ctx context.Context, opts agent.RunOptions) (agent.Result, error) {
		close(started)
		var receipt agent.SteerReceipt
		select {
		case input := <-opts.Steer:
			receipt = agent.SteerReceipt{ID: input.ID, State: agent.InputAccepted}
		case <-ctx.Done():
			return agent.Result{}, ctx.Err()
		}
		<-release
		return agent.Result{Text: "반영한 답", SessionID: "s", Receipts: []agent.SteerReceipt{receipt}}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "구현해", ""); err != nil {
		t.Fatal(err)
	}
	<-started
	snap, _ := hub.Snapshot(r.ID)
	seat := snap.Room.Participants[0].ID
	posted := callJSON(t, s, "rt_post", map[string]any{"room": r.ID, "target": seat, "text": "이것도 반영해"})
	if posted["live"] == true || posted["delivery"].(map[string]any)["state"] != "pending" {
		t.Fatalf("the post was not steered: %v", posted)
	}
	rooms := r.ID + ":" + strconv.FormatInt(int64(posted["seq"].(float64)), 10)
	if got := callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 1}); got["timed_out"] != true {
		t.Fatalf("ready while the steered turn runs: %v", got)
	}
	close(release)
	got := callJSON(t, s, "rt_wait", map[string]any{"rooms": rooms, "timeout_seconds": 5})
	ready := got["ready"].([]any)
	if got["timed_out"] != false || len(ready) != 1 {
		t.Fatalf("the steered turn's reply did not finish the wait: %v", got)
	}
	msgs := ready[0].(map[string]any)["messages"].([]any)
	if first := msgs[0].(map[string]any); first["kind"] != "agent" || first["text"] != "반영한 답" {
		t.Fatalf("ready does not carry the steered reply: %v", msgs)
	}
}

// With after_seq 0 a spent max_chars budget ends rt_read's paging after the
// newest 200 messages, so rt wait never saw a reply behind them.
func TestWaitAfterZeroFindsReplyBehindAFullPage(t *testing.T) {
	hub, s := waitHub(t, func(context.Context, agent.RunOptions) (agent.Result, error) {
		return agent.Result{Text: "답", SessionID: "s"}, nil
	})
	r, err := hub.CreateRoom(room.RoomSpec{Title: "방", Mode: "chat", Participants: []string{"claude"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.PostUser(r.ID, "먼저", ""); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for snap, _ := hub.Snapshot(r.ID); len(snap.Messages) < 2 || snap.Messages[1].Status != "done"; snap, _ = hub.Snapshot(r.ID) {
		if time.Now().After(deadline) {
			t.Fatal("first reply did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	off := false
	snap, _ := hub.Snapshot(r.ID)
	if _, err := hub.UpdateRoom(r.ID, room.RoomPatch{Participants: []room.ParticipantPatch{{ID: snap.Room.Participants[0].ID, Enabled: &off}}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if _, err := hub.PostUser(r.ID, "메모 "+strconv.Itoa(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	got := callJSON(t, s, "rt_wait", map[string]any{"rooms": r.ID + ":0", "timeout_seconds": 1, "max_chars": 50})
	if ready := got["ready"].([]any); got["timed_out"] != false || len(ready) != 1 {
		t.Fatalf("reply behind a full page not seen: %v", got)
	}
	// rt_read keeps its contract: a spent budget ends paging without after_seq.
	read := callJSON(t, s, "rt_read", map[string]any{"room": r.ID, "max_chars": 50})
	if read["omitted_messages"] == nil {
		t.Fatalf("rt_read changed: %v", read)
	}
}
