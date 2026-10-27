package room

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func agentMsg(t *testing.T, snap *Snapshot, author, status string) *Message {
	t.Helper()
	for i := len(snap.Messages) - 1; i >= 0; i-- {
		if m := snap.Messages[i]; m.Kind == "agent" && m.Author == author && m.Status == status {
			return m
		}
	}
	t.Fatalf("no %s message from %s", status, author)
	return nil
}

func systemSays(snap *Snapshot, parts ...string) bool {
	for _, m := range snap.Messages {
		if m.Kind != "system" {
			continue
		}
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(m.Text, p)
		}
		if ok {
			return true
		}
	}
	return false
}

// 2026-10-27, room 8a864d16ac29 #27: the answer ended with [[REQUEST:@Terra-reviewer]],
// then the CLI failed with "stream idle timeout". Markers of a failed turn stay
// unapplied (the answer may be cut); the room now says so and a person or the
// orchestrator applies them when the answer is complete.
func TestErroredTurnMarkersWaitForApply(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	if _, err := h.PostUser(r.ID, "start", "codex"); err != nil {
		t.Fatal(err)
	}
	call := nextCall(t, ps[0])
	call.result <- controlledResult{text: "work done\n[[REQUEST:@claude]] please re-check", err: errors.New("stream idle timeout (5m0s)")}
	waitIdle(t, h, r.ID)
	noMoreCalls(t, ps[1])
	snap, _ := h.Snapshot(r.ID)
	failed := agentMsg(t, snap, "codex", "error")
	if len(snap.Requests) != 0 || failed.PendingMarkers != 1 {
		t.Fatalf("a failed turn must not apply markers but must count them: requests %+v, pending %d", snap.Requests, failed.PendingMarkers)
	}
	if !systemSays(snap, "#"+strconv.FormatInt(failed.Seq, 10), "마커 1개", "처리하지 않음") {
		t.Fatalf("no notice about unapplied markers: %+v", snap.Messages)
	}

	applied, err := h.ApplyMarkers(r.ID, failed.Seq)
	if err != nil {
		t.Fatal(err)
	}
	if applied.PendingMarkers != 0 || len(applied.Requests) != 1 {
		t.Fatalf("%+v", applied)
	}
	to := nextCall(t, ps[1])
	if !strings.Contains(to.opts.Prompt, "please re-check") {
		t.Fatalf("request not delivered: %s", to.opts.Prompt)
	}
	finishCall(to, "checked")
	waitIdle(t, h, r.ID)
	if _, err := h.ApplyMarkers(r.ID, failed.Seq); err == nil {
		t.Fatal("markers applied twice")
	}
	snap, _ = h.Snapshot(r.ID)
	if done := agentMsg(t, snap, "claude", "done"); done != nil {
		if _, err := h.ApplyMarkers(r.ID, done.Seq); err == nil {
			t.Fatal("a done turn has no pending markers to apply")
		}
	}
	if !systemSays(snap, "#"+strconv.FormatInt(failed.Seq, 10), "마커를 실행함") {
		t.Fatal("no record of the manual apply")
	}
}

// The requested seat finished the work, wrote [[REQUEST_DONE]], then its CLI failed.
// The request stays pending (unchanged rule) until the markers are applied.
func TestErroredTurnResultMarkerClosesRequestOnApply(t *testing.T) {
	h, r, ps := controlledHub(t, "codex", "claude")
	if _, err := h.PostUser(r.ID, "start", "codex"); err != nil {
		t.Fatal(err)
	}
	finishCall(nextCall(t, ps[0]), "[[REQUEST:@claude]] inspect it")
	to := nextCall(t, ps[1])
	snap, _ := h.Snapshot(r.ID)
	req := snap.Requests[0]
	to.result <- controlledResult{text: "inspected\n[[REQUEST_DONE:" + req.ID + "]]", err: errors.New("stream idle timeout (5m0s)")}
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if snap.Requests[0].Status != "pending" {
		t.Fatalf("an errored turn must not close the request by itself: %+v", snap.Requests[0])
	}
	failed := agentMsg(t, snap, "claude", "error")
	if _, err := h.ApplyMarkers(r.ID, failed.Seq); err != nil {
		t.Fatal(err)
	}
	result := nextCall(t, ps[0])
	if !strings.Contains(result.opts.Prompt, "inspected") {
		t.Fatalf("requester did not get the result: %s", result.opts.Prompt)
	}
	finishCall(result, "thanks")
	waitIdle(t, h, r.ID)
	snap, _ = h.Snapshot(r.ID)
	if got := snap.Requests[0]; got.Status != "done" || got.ResultMsg != failed.ID {
		t.Fatalf("%+v", got)
	}
}

func TestErroredTurnWithoutMarkersHasNothingToApply(t *testing.T) {
	h, r, ps := controlledHub(t, "codex")
	if _, err := h.PostUser(r.ID, "start", "codex"); err != nil {
		t.Fatal(err)
	}
	nextCall(t, ps[0]).result <- controlledResult{text: "half an answer about [[REQUEST:@x]] markers", err: errors.New("boom")}
	waitIdle(t, h, r.ID)
	snap, _ := h.Snapshot(r.ID)
	failed := agentMsg(t, snap, "codex", "error")
	if failed.PendingMarkers != 0 || systemSays(snap, "처리하지 않음") {
		t.Fatalf("prose mentioning a marker is not a marker line: %+v", failed)
	}
	if _, err := h.ApplyMarkers(r.ID, failed.Seq); err == nil {
		t.Fatal("nothing to apply")
	}
}
