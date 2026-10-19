package room

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestRequestLifecycleAndReplay(t *testing.T) {
	for _, outcome := range []string{"done", "rejected", "pending", "cancelled", "error"} {
		t.Run(outcome, func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex", "claude")
			events, unsub, err := h.Subscribe(r.ID)
			if err != nil {
				t.Fatal(err)
			}
			defer unsub()
			if _, err := h.PostUser(r.ID, "start", "codex"); err != nil {
				t.Fatal(err)
			}
			from := nextCall(t, ps[0])
			from.result <- controlledResult{text: "[[REQUEST:@claude]] inspect the image"}
			to := nextCall(t, ps[1])
			snap, _ := h.Snapshot(r.ID)
			if len(snap.Requests) != 1 || snap.Requests[0].Status != "in_progress" {
				t.Fatalf("%+v", snap.Requests)
			}
			req := snap.Requests[0]
			if !strings.Contains(to.opts.Prompt, "[요청 "+req.ID+" from @codex: inspect the image]") || !strings.Contains(to.opts.Prompt, "[[REQUEST_DONE:"+req.ID+"]]") {
				t.Fatal(to.opts.Prompt)
			}
			var source *Message
			for _, m := range snap.Messages {
				if m.ID == req.SourceMsg {
					source = m
				}
			}
			if source == nil || len(source.Requests) != 1 || source.Requests[0] != req.ID {
				t.Fatal(source)
			}
			text := "answer without signal"
			switch outcome {
			case "done":
				text = "answer\n[[REQUEST_DONE:" + req.ID + "]]"
			case "rejected":
				text = "answer\n[[REQUEST_REJECT:" + req.ID + "]] out of scope"
			case "cancelled":
				if _, err := h.CancelRequest(r.ID, req.ID); err != nil {
					t.Fatal(err)
				}
				text = "late answer\n[[REQUEST_DONE:" + req.ID + "]]"
			}
			if outcome == "error" {
				to.result <- controlledResult{text: "[[REQUEST_DONE:" + req.ID + "]]", err: os.ErrPermission}
			} else {
				to.result <- controlledResult{text: text}
			}
			waitIdle(t, h, r.ID)
			snap, _ = h.Snapshot(r.ID)
			want := outcome
			if want == "error" {
				want = "pending"
			}
			got := snap.Requests[0]
			if got.Status != want {
				t.Fatalf("want %s, got %+v", want, got)
			}
			if (want == "done" || want == "rejected") && got.ResultMsg == "" {
				t.Fatal(got)
			}
			if want == "rejected" && got.Reason != "out of scope" {
				t.Fatal(got)
			}
			if want == "pending" || want == "cancelled" {
				if got.ResultMsg != "" {
					t.Fatal(got)
				}
			}
			data, err := os.ReadFile(filepath.Join(h.store.roomDir(r.ID), "requests.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(string(data)), "\n")
			if len(lines) < 3 {
				t.Fatalf("missing state journal: %s", data)
			}
			var first Request
			if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
				t.Fatal(err)
			}
			if first.Status != "pending" {
				t.Fatal(first)
			}
			restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
			if err != nil {
				t.Fatal(err)
			}
			replay, _ := restarted.Snapshot(r.ID)
			if len(replay.Requests) != 1 || replay.Requests[0] != got {
				t.Fatalf("replay %+v != %+v", replay.Requests, got)
			}
			found := false
			for len(events) > 0 {
				var event struct {
					Type string          `json:"type"`
					Data json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(<-events, &event); err != nil {
					t.Fatal(err)
				}
				if event.Type == "request" {
					var payload Request
					if err := json.Unmarshal(event.Data, &payload); err != nil {
						t.Fatal(err)
					}
					found = payload.ID == req.ID || found
				}
			}
			if !found {
				t.Fatal("no request SSE event")
			}
		})
	}
}

func TestRequestUnknownSeatAndIDFeedback(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "start", ""); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	call.result <- controlledResult{text: "answer\n[[REQUEST:@missing-seat]] do it\n[[REQUEST_DONE:unknown]]"}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if len(snap.Requests) != 0 {
		t.Fatal(snap.Requests)
	}
	if _, err := h.PostUser(r.ID, "next", ""); err != nil {
		t.Fatal(err)
	}
	next := nextCall(t, ps[0])
	if !strings.Contains(next.opts.Prompt, "다른 활성 좌석 ID") || !strings.Contains(next.opts.Prompt, "진행 중 요청 ID가 아님") {
		t.Fatal(next.opts.Prompt)
	}
	next.result <- controlledResult{text: "ok"}
	waitIdle(t, h, r.ID)
}

func TestRequestLimitAndRestartInProgress(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	r.MaxChain = 0
	if _, err := h.PostUser(r.ID, "start", "codex"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	call.result <- controlledResult{text: "[[REQUEST:@claude]] pending work"}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	if len(snap.Requests) != 1 || snap.Requests[0].Status != "pending" || len(ps[1].callsCh) != 0 {
		t.Fatal(snap.Requests)
	}
	h.mu.Lock()
	rs := h.rooms[r.ID]
	active := rs.requests[0]
	active.Status = "in_progress"
	err := h.saveRequest(rs, active)
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ = restarted.Snapshot(r.ID)
	if snap.Requests[0].Status != "pending" {
		t.Fatal(snap.Requests)
	}
	if _, err := restarted.CancelRequest(r.ID, active.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.CancelRequest(r.ID, active.ID); err == nil {
		t.Fatal("terminal state accepted another transition")
	}
}

func TestMarkersOnlyApplyToDoneAndModeBrief(t *testing.T) {
	for _, status := range []string{"error", "cancelled"} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		msg := rs.nextMessage("agent", "codex", "body")
		msg.Status = status
		pr := &prepared{p: r.Participants[0], t: &Turn{}, msg: msg, opts: agent.RunOptions{Mode: agent.ModeRead, Workspace: t.TempDir(), ScratchDir: h.scratch}}
		if err := h.finishMarkers(rs, pr, "[[ATTACH:missing.png]]\n[[REQUEST:@claude]] do it"); err != nil {
			t.Fatal(err)
		}
		if len(rs.requests) != 0 || len(msg.Attachments) != 0 || len(rs.msgs) != 1 {
			t.Fatal("unsuccessful turn processed markers")
		}
	}
	for _, mode := range []string{agent.ModeChat, agent.ModeRead, agent.ModeWrite} {
		r := &Room{Mode: mode}
		p := &Participant{ID: "codex"}
		brief := standingBrief(r, p)
		for _, marker := range []string{"[[REQUEST:@좌석ID]]", "[[REQUEST_DONE:요청ID]]", "[[REQUEST_REJECT:요청ID]]"} {
			if !strings.Contains(brief, marker) {
				t.Fatal(brief)
			}
		}
		if strings.Contains(brief, "[[ATTACH:") != (mode != agent.ModeChat) {
			t.Fatal(brief)
		}
		if strings.Contains(brief, "직접 화면을 캡처할 수") != (mode == agent.ModeWrite) {
			t.Fatal(brief)
		}
	}
}

func TestCancelQueuedRequestPreservesOtherWork(t *testing.T) {
	for _, explicitMention := range []bool{false, true} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		text := "[[REQUEST:@claude]] inspect"
		if explicitMention {
			text = "@claude also explain\n" + text
		}
		m := rs.nextMessage("agent", "codex", text)
		req := Request{ID: newID(), From: "codex", To: "claude", SourceMsg: m.ID, Status: "pending", Text: "inspect"}
		if err := h.saveRequest(rs, req); err != nil {
			t.Fatal(err)
		}
		rs.enqueue(&Turn{ID: newID(), Agent: "claude", Reason: "mention", Group: m.ID, Cap: m.Seq})
		if _, err := h.CancelRequest(r.ID, req.ID); err != nil {
			t.Fatal(err)
		}
		if (len(rs.queue) == 1) != explicitMention {
			t.Fatalf("explicit=%v queue=%v", explicitMention, rs.queue)
		}
	}
}

func TestRequestResultMustBeLastAndBelongToReceiver(t *testing.T) {
	for _, text := range []string{"[[REQUEST_DONE:r1]]\nmore text", "[[REQUEST_REJECT:r1]]", "[[REQUEST_DONE:r2]]", "[[REQUEST_DONE:r1]] extra"} {
		h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
		rs := h.rooms[r.ID]
		r1 := Request{ID: "r1", From: "codex", To: "claude", Status: "in_progress"}
		r2 := Request{ID: "r2", From: "claude", To: "codex", Status: "in_progress"}
		for _, req := range []Request{r1, r2} {
			if err := h.saveRequest(rs, req); err != nil {
				t.Fatal(err)
			}
		}
		msg := rs.nextMessage("agent", "claude", text)
		pr := &prepared{t: &Turn{}, p: r.Participants[1], msg: msg, requests: []string{"r1"}}
		if err := h.finishMarkers(rs, pr, text); err != nil {
			t.Fatal(err)
		}
		if rs.request("r1").Status != "pending" || rs.request("r2").Status != "in_progress" {
			t.Fatal(rs.requests)
		}
		if rs.msgs[len(rs.msgs)-1].Kind != "system" {
			t.Fatal("no rejection feedback")
		}
	}
}
