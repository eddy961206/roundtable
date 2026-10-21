package room

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// All workloads are deterministic and use local synthetic events, never a CLI
// or paid model. Copy this file to the baseline worktree to compare revisions.
func BenchmarkJSONLScan(b *testing.B) {
	for _, payloadSize := range []int{64, 4096} {
		b.Run(fmt.Sprintf("payload=%d", payloadSize), func(b *testing.B) {
			line, err := json.Marshal(TraceLine{MsgID: "message", Event: agent.Event{Kind: agent.KindToolResult, Output: strings.Repeat("x", payloadSize)}})
			if err != nil {
				b.Fatal(err)
			}
			data := bytes.Repeat(append(line, '\n'), 4096)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				count := 0
				err := readJSONLines(bytes.NewReader(data), "synthetic.jsonl", func(line []byte) error {
					if !json.Valid(line) {
						return fmt.Errorf("invalid JSON")
					}
					count++
					return nil
				})
				if err != nil || count != 4096 {
					b.Fatalf("scan: count=%d, err=%v", count, err)
				}
			}
		})
	}
}

func benchmarkTraceStore(b *testing.B) (*Store, int64) {
	b.Helper()
	s, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if err := s.SaveRoom(&Room{ID: "room"}); err != nil {
		b.Fatal(err)
	}
	var data bytes.Buffer
	enc := json.NewEncoder(&data)
	const events = 8192
	for i := 0; i < events; i++ {
		id := "other"
		if i%4 == 0 {
			id = "message"
		}
		e := agent.Event{Kind: agent.KindToolResult, Parent: "child", TS: int64(i), Output: strings.Repeat("content ", 128)}
		if i == 0 {
			e.Kind, e.Input = agent.KindSubagentStart, "nested synthetic task"
		}
		if err := enc.Encode(TraceLine{MsgID: id, Event: e}); err != nil {
			b.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(s.roomDir("room"), "events.jsonl"), data.Bytes(), 0600); err != nil {
		b.Fatal(err)
	}
	return s, int64(data.Len())
}

func BenchmarkTraceTail(b *testing.B) {
	s, size := benchmarkTraceStore(b)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tail, err := s.ReadTraceTail("room", "message", 200)
		if err != nil || tail.Total != 8192/4 || len(tail.Events) != 201 || !tail.Truncated {
			b.Fatalf("tail: total=%d events=%d, err=%v", tail.Total, len(tail.Events), err)
		}
	}
}

func BenchmarkReadTrace(b *testing.B) {
	s, size := benchmarkTraceStore(b)
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		events, err := s.ReadTrace("room", "message")
		if err != nil || len(events) != 8192/4 || events[0].Kind != agent.KindSubagentStart {
			b.Fatalf("full trace: events=%d, err=%v", len(events), err)
		}
	}
}

func BenchmarkBroadcastFanout(b *testing.B) {
	for _, clients := range []int{1, 4, 16, 32} {
		b.Run(fmt.Sprintf("subscribers=%d", clients), func(b *testing.B) {
			rs := &roomState{room: &Room{ID: "room"}, subs: map[chan []byte]struct{}{}}
			channels := make([]chan []byte, clients)
			for i := range channels {
				channels[i] = make(chan []byte, 1)
				rs.subs[channels[i]] = struct{}{}
			}
			payload := map[string]any{"id": "message", "text": "synthetic streamed token"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rs.broadcast("delta", payload)
				for _, ch := range channels {
					<-ch
				}
			}
		})
	}
}

type performanceProvider struct {
	fakeProvider
	events int
}

func (p *performanceProvider) Run(ctx context.Context, _ agent.RunOptions, emit func(agent.Event)) (agent.Result, error) {
	for i := 0; i < p.events; i++ {
		if err := ctx.Err(); err != nil {
			return agent.Result{}, err
		}
		emit(agent.Event{Kind: agent.KindTextDelta, Text: "streamed token ", TS: int64(i)})
	}
	return agent.Result{Text: strings.Repeat("streamed token ", p.events)}, nil
}

// Each iteration drives a complete simultaneous set of turns through the real
// runTurn persistence/fanout/completion path. Setup and teardown are excluded.
// Four buffered subscribers model four tabs without network scheduling noise;
// the separate fanout benchmark varies subscribers from one to thirty-two.
func BenchmarkConcurrentStreaming(b *testing.B) {
	for _, seats := range []int{1, 4, 16, 32} {
		b.Run(fmt.Sprintf("participants=%d", seats), func(b *testing.B) {
			root := b.TempDir()
			p := &performanceProvider{fakeProvider: fakeProvider{id: "codex"}, events: 128}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				dir := filepath.Join(root, fmt.Sprintf("run-%d", i))
				s, err := NewStore(dir)
				if err != nil {
					b.Fatal(err)
				}
				h, err := NewHub(s, agent.NewRegistry(p), dir, nil)
				if err != nil {
					b.Fatal(err)
				}
				specs := make([]ParticipantSpec, seats)
				for j := range specs {
					specs[j] = ParticipantSpec{Provider: "codex", Model: "m", Role: strings.Repeat("Review carefully. ", 32)}
				}
				r, err := h.CreateRoom(RoomSpec{Title: "performance", Mode: agent.ModeChat, ParticipantSpecs: specs})
				if err != nil {
					b.Fatal(err)
				}
				rs := h.rooms[r.ID]
				for j := 0; j < 4; j++ {
					rs.subs[make(chan []byte, seats*270+100)] = struct{}{}
				}
				preparedTurns := make([]*prepared, seats)
				for j, seat := range r.Participants {
					t := &Turn{ID: fmt.Sprintf("turn-%d", j), Agent: seat.ID, Reason: "broadcast"}
					pr := &prepared{t: t, p: seat, prov: p, opts: agent.RunOptions{Mode: agent.ModeChat, ScratchDir: dir}}
					h.prepareTurn(rs, pr)
					preparedTurns[j] = pr
				}
				h.turnWG.Add(seats)
				b.StartTimer()
				for _, pr := range preparedTurns {
					go h.runTurn(rs, pr)
				}
				h.turnWG.Wait()
				b.StopTimer()
				if len(rs.subs) != 4 {
					b.Fatal("benchmark subscriber overflow")
				}
				for _, pr := range preparedTurns {
					if pr.msg.Status != "done" || pr.msg.Trace != p.events || pr.msg.Text != strings.TrimSpace(strings.Repeat("streamed token ", p.events)) {
						b.Fatalf("turn failed: %+v", pr.msg)
					}
				}
				if err := os.RemoveAll(dir); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
		})
	}
}
