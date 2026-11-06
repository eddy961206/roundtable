package room

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestUserRequestSurvivesNextTurnAndResolvesOnce(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	postQueueMessage(t, h, r.ID, "요청해", "codex")
	first := nextCall(t, ps[0])
	postQueueMessage(t, h, r.ID, "이미 대기 중인 다음 작업", "codex")
	finishCall(first, "[[REQUEST:@User]] 어느 브랜치?\n[[REQUEST:@사람]] 외부 결과 확인")
	second := nextCall(t, ps[0])
	snap, _ := h.Snapshot(r.ID)
	if len(snap.UserRequests) != 2 || snap.UserRequests[0].Status != "pending" || len(snap.Requests) != 0 {
		t.Fatalf("new turn closed or rerouted user requests: %+v", snap)
	}
	request := snap.UserRequests[0]
	answer, resolved, err := h.ResolveUserRequest(r.ID, request.ID, "fix/user-request", false)
	if err != nil || answer.Live || answer.Target != "codex" || resolved.Status != "answered" || resolved.ResolvedSeq != answer.Seq {
		t.Fatalf("answer: %+v %+v %v", answer, resolved, err)
	}
	if _, _, err := h.ResolveUserRequest(r.ID, request.ID, "중복 답", false); err == nil {
		t.Fatal("duplicate resolution accepted")
	}
	if _, ack, err := h.ResolveUserRequest(r.ID, snap.UserRequests[1].ID, "", true); err != nil || ack.Status != "acknowledged" {
		t.Fatal(ack, err)
	}
	finishCall(second, "다음 작업 끝")
	third := nextCall(t, ps[0])
	if !strings.Contains(third.opts.Prompt, "사용자 요청 「어느 브랜치?」에 대한 답: fix/user-request") {
		t.Fatal(third.opts.Prompt)
	}
	finishCall(third, "답 받음")
	waitIdle(t, h, r.ID)
	if len(ps[1].callsCh) != 0 || len(ps[0].callsCh) != 0 {
		t.Fatal("answer/acknowledgement called another seat or started an extra turn")
	}
	reloaded, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ = reloaded.Snapshot(r.ID)
	if snap.UserRequests[0].Status != "answered" || snap.UserRequests[0].Answer != "fix/user-request" || snap.UserRequests[1].Status != "acknowledged" || reloaded.Rooms()[0].UserRequests != 0 {
		t.Fatal(snap.UserRequests)
	}
}

func TestUserRequestAnswerDelivery(t *testing.T) {
	for _, state := range []string{"idle", "queued", "running-steer"} {
		t.Run(state, func(t *testing.T) {
			h, r, ps := controlledHub(t, "codex")
			postQueueMessage(t, h, r.ID, "요청", "codex")
			finishCall(nextCall(t, ps[0]), "[[REQUEST:@User]] 설정 값")
			waitIdle(t, h, r.ID)
			snap, _ := h.Snapshot(r.ID)
			id := snap.UserRequests[0].ID
			var running *controlledCall
			switch state {
			case "queued":
				postQueueMessage(t, h, r.ID, "진행 작업", "codex")
				running = nextCall(t, ps[0])
				postQueueMessage(t, h, r.ID, "먼저 온 지시", "codex")
			case "running-steer":
				ps[0].steer = true
				postQueueMessage(t, h, r.ID, "실행 중", "codex")
				running = nextCall(t, ps[0])
			}
			msg, resolved, err := h.ResolveUserRequest(r.ID, id, "123", false)
			if err != nil || resolved.Status != "answered" {
				t.Fatal(msg, resolved, err)
			}
			if state == "running-steer" {
				if !msg.Live {
					t.Fatal("steer-capable running seat was not steered")
				}
				select {
				case text := <-running.opts.Steer:
					if !strings.Contains(text, "대한 답: 123") {
						t.Fatal(text)
					}
				default:
					t.Fatal("missing live answer")
				}
				running.result <- controlledResult{text: "반영", steered: 1}
			} else {
				if state == "queued" {
					finishCall(running, "진행 작업 끝")
					first := nextCall(t, ps[0])
					if !strings.Contains(first.opts.Prompt, "먼저 온 지시") || strings.Contains(first.opts.Prompt, "대한 답: 123") {
						t.Fatalf("targeted input FIFO changed: %s", first.opts.Prompt)
					}
					finishCall(first, "먼저 처리")
				}
				answerTurn := nextCall(t, ps[0])
				if !strings.Contains(answerTurn.opts.Prompt, "대한 답: 123") {
					t.Fatal(answerTurn.opts.Prompt)
				}
				finishCall(answerTurn, "반영")
			}
			waitIdle(t, h, r.ID)
		})
	}
}

func TestUserRequestLegacyNoticeAndUnavailableSeat(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	rs := h.rooms[r.ID]
	// Exact pre-feature schema: no added fields or separate request file.
	m := rs.nextMessage("system", "system", "@codex 사용자 확인 요청: 이전 요청")
	m.NoticeMarker, m.NoticeFor, m.NoticeMsg, m.Part = "user_request", "codex", "old-reply", "part-old"
	if err := h.store.AppendMessage(r.ID, m); err != nil {
		t.Fatal(err)
	}
	if _, err := h.addSystem(rs, "사용자가 이미 처리했다고 말함"); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap, _ := reloaded.Snapshot(r.ID)
	if len(snap.UserRequests) != 1 {
		t.Fatal(snap.UserRequests)
	}
	request := snap.UserRequests[0]
	if request.ID != m.ID || request.Seq != m.Seq || request.Part != "part-old" || request.Status != "pending" || request.Text != "이전 요청" {
		t.Fatal(request)
	}
	reloaded.rooms[r.ID].room.Participants[0].Enabled = false
	if _, _, err := reloaded.ResolveUserRequest(r.ID, m.ID, "답", false); err == nil {
		t.Fatal("answer to disabled seat accepted")
	}
	if reloaded.rooms[r.ID].userRequest(m.ID).Status != "pending" {
		t.Fatal("failed answer resolved the request")
	}
	if _, resolved, err := reloaded.ResolveUserRequest(r.ID, m.ID, "", true); err != nil || resolved.Status != "acknowledged" {
		t.Fatal(resolved, err)
	}
	reloadedAgain, err := NewHub(h.store, h.reg, h.scratch, nil)
	if err != nil || reloadedAgain.rooms[r.ID].userRequest(m.ID).Status != "acknowledged" {
		t.Fatal("legacy request reopened after restart", err)
	}
}

func TestUserRequestResolutionAppendIsAcceptanceBoundary(t *testing.T) {
	for _, fail := range []string{"messages.jsonl", "room.json.tmp"} {
		for _, acknowledge := range []bool{false, true} {
			t.Run(fail+"/ack="+map[bool]string{false: "false", true: "true"}[acknowledge], func(t *testing.T) {
				h, r, ps := controlledHub(t, "codex")
				postQueueMessage(t, h, r.ID, "요청", "codex")
				finishCall(nextCall(t, ps[0]), "[[REQUEST:@User]] 처리해 줘")
				waitIdle(t, h, r.ID)
				snap, _ := h.Snapshot(r.ID)
				id := snap.UserRequests[0].ID
				path := filepath.Join(h.store.roomDir(r.ID), fail)
				if fail == "messages.jsonl" {
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
				text := "처리했어"
				if acknowledge {
					text = ""
				}
				msg, result, err := h.ResolveUserRequest(r.ID, id, text, acknowledge)
				if fail == "messages.jsonl" {
					if err == nil || msg != nil || h.rooms[r.ID].userRequest(id).Status != "pending" || len(ps[0].callsCh) != 0 {
						t.Fatalf("failed append accepted: %+v %+v %v", msg, result, err)
					}
				} else {
					if err != nil || msg == nil || result.Status == "pending" {
						t.Fatalf("accepted resolution lost: %+v %+v %v", msg, result, err)
					}
					if !acknowledge {
						finishCall(nextCall(t, ps[0]), "수신")
						waitIdle(t, h, r.ID)
					}
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if fail == "messages.jsonl" {
					if err := os.Rename(path+".saved", path); err != nil {
						t.Fatal(err)
					}
				}
				reloaded, err := NewHub(h.store, h.reg, h.scratch, nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := reloaded.rooms[r.ID].userRequest(id); (got.Status == "pending") != (fail == "messages.jsonl") {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestUserRequestMarkerBoundaries(t *testing.T) {
	for _, reason := range []string{"discussion", "review", "part", "mention"} {
		t.Run(reason, func(t *testing.T) {
			h, r := newTestHub(t, &fakeProvider{id: "codex"})
			rs := h.rooms[r.ID]
			msg := rs.nextMessage("agent", "codex", "[[REQUEST:@User]] 확인")
			pr := &prepared{p: r.Participants[0], t: &Turn{Reason: reason}, msg: msg, opts: agent.RunOptions{Mode: agent.ModeChat}}
			if reason == "part" || reason == "review" {
				pr.part = &Part{ID: "p1"}
			}
			if err := h.finishMarkers(rs, pr, msg.Text, true); err != nil {
				t.Fatal(err)
			}
			want := reason == "part" || reason == "mention"
			if (len(rs.userRequests) == 1) != want {
				t.Fatal(rs.userRequests)
			}
			if reason == "part" && rs.userRequests[0].Part != "p1" {
				t.Fatal("part context lost")
			}
		})
	}
	// A uniquely named active seat still wins over the human alias.
	h, r := newTestHub(t, &fakeProvider{id: "codex"}, &fakeProvider{id: "claude"})
	r.Participants[1].Label = "User"
	rs := h.rooms[r.ID]
	msg := rs.nextMessage("agent", "codex", "[[REQUEST:@User]] 좌석 작업")
	pr := &prepared{p: r.Participants[0], t: &Turn{}, msg: msg, opts: agent.RunOptions{Mode: agent.ModeChat}}
	if err := h.finishMarkers(rs, pr, msg.Text, true); err != nil {
		t.Fatal(err)
	}
	if len(rs.userRequests) != 0 || len(rs.requests) != 1 || rs.requests[0].To != "claude" {
		t.Fatal(rs.userRequests, rs.requests)
	}
}
