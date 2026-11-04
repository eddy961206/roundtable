package room

import (
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// 2026-11-04: an agy seat's `rg -n "upgrade-history"` read a stdin pipe that
// agy never closes for 1h54m. The trace had the tool call and nothing after
// it, but the seat showed only "running". The seat must name the call still
// waiting for its result, and the running turn must say since when its CLI
// has been quiet.
func TestRunningSeatShowsOpenToolCallAndQuietTime(t *testing.T) {
	h, r, ps := controlledHub(t, "agy")
	postQueueMessage(t, h, r.ID, "작업", "agy")
	call := nextCall(t, ps[0])
	seat := func(s *Snapshot) *Participant { return s.Room.Participants[0] }
	runningTurn := func(s *Snapshot) *Turn {
		for _, turn := range s.Queue {
			if turn.Status == "running" {
				return turn
			}
		}
		return nil
	}
	started := queueSnapshot(t, h, r.ID, func(s *Snapshot) bool { return runningTurn(s) != nil })
	if runningTurn(started).Quiet == 0 || seat(started).Tool != nil {
		t.Fatalf("a started turn is quiet from its start and has no tool: %+v %+v", runningTurn(started), seat(started).Tool)
	}

	hung := `{"CommandLine":"rg -n \"upgrade-history\""}`
	call.emit(agent.Event{Kind: agent.KindToolCall, ID: "s41", Name: "run_command", Input: hung})
	s, err := h.Snapshot(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	tool := seat(s).Tool
	if tool == nil || tool.ID != "s41" || tool.Name != "run_command" || tool.Input != hung || tool.Since == 0 || seat(s).StatusText != "도구: run_command" {
		t.Fatalf("open tool call: %+v (%q)", tool, seat(s).StatusText)
	}
	if runningTurn(s).Quiet != tool.Since {
		t.Fatalf("quiet since %d, last event %d", runningTurn(s).Quiet, tool.Since)
	}

	// A later call that finishes leaves the older, still open call in view.
	call.emit(agent.Event{Kind: agent.KindToolCall, ID: "s42", Name: "read_file", Input: strings.Repeat("가", 400)})
	call.emit(agent.Event{Kind: agent.KindToolResult, ID: "s42", Name: "read_file"})
	if s, _ = h.Snapshot(r.ID); seat(s).Tool == nil || seat(s).Tool.ID != "s41" {
		t.Fatalf("oldest open call must stay: %+v", seat(s).Tool)
	}
	call.emit(agent.Event{Kind: agent.KindToolResult, ID: "s41", Name: "run_command"})
	if s, _ = h.Snapshot(r.ID); seat(s).Tool != nil {
		t.Fatalf("answered call must leave the seat: %+v", seat(s).Tool)
	}
	call.emit(agent.Event{Kind: agent.KindToolCall, ID: "s43", Name: "run_command", Input: hung})
	finishCall(call, "끝")
	waitIdle(t, h, r.ID)
	if s, _ = h.Snapshot(r.ID); seat(s).Tool != nil {
		t.Fatalf("a finished turn leaves no tool on the seat: %+v", seat(s).Tool)
	}
}

func TestOpenToolInputIsClipped(t *testing.T) {
	var tools openTools
	tool, changed := tools.observe(agent.Event{Kind: agent.KindToolCall, ID: "w", Name: "write_file", Input: strings.Repeat("가", 400)}, 1)
	if !changed || len([]rune(tool.Input)) != toolInputRunes+1 || !strings.HasSuffix(tool.Input, "…") {
		t.Fatalf("clipped input: %d runes, changed=%v", len([]rune(tool.Input)), changed)
	}
	if _, changed := tools.observe(agent.Event{Kind: agent.KindTextDelta, Text: "x"}, 2); changed {
		t.Fatal("text must not change the open call")
	}
}
