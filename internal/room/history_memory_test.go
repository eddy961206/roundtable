package room

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

// History memory workloads: one room with n synthetic messages written
// straight to messages.jsonl, then loaded by a fresh Hub as the server does at
// startup. Message sizes follow the real data seen on 2026-11-03 (about 2.9 KB
// per stored line): one person's line of 300 bytes for every two seat replies
// of 2,000 bytes with a usage map. No CLI or model runs.
var historySizes = []int{10_000, 100_000}

func writeHistoryRoom(tb testing.TB, n int) (*Store, string, int64) {
	tb.Helper()
	dir := tb.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		tb.Fatal(err)
	}
	h, err := NewHub(s, agent.NewRegistry(&fakeProvider{id: "codex"}), dir, nil)
	if err != nil {
		tb.Fatal(err)
	}
	r, err := h.CreateRoom(RoomSpec{Title: "history", Mode: agent.ModeChat, ParticipantSpecs: []ParticipantSpec{{Provider: "codex", Model: "m"}, {Provider: "codex", Model: "m"}}})
	if err != nil {
		tb.Fatal(err)
	}
	seats := []string{r.Participants[0].ID, r.Participants[1].ID}
	path := filepath.Join(s.roomDir(r.ID), "messages.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	userText := strings.Repeat("사람 메시지 ", 30)[:300]
	agentText := strings.Repeat("seat reply text ", 125)
	for i := 1; i <= n; i++ {
		m := &Message{ID: fmt.Sprintf("m%07d", i), Seq: int64(i), TS: int64(i) * 1000, Status: "done"}
		if i%3 == 1 {
			m.Kind, m.Author, m.Text = "user", "user", userText
		} else {
			m.Kind, m.Author, m.Text, m.Provider, m.Model, m.SessionID, m.Mode = "agent", seats[i%2], agentText, "codex", "m", "session-"+seats[i%2], "chat"
			m.Usage = map[string]any{"input_tokens": 1200.0, "output_tokens": 400.0, "cached_input_tokens": 9000.0, "context_tokens": 40000.0, "context_window": 200000.0, "total_cost_usd": 0.01}
			m.Timing = &TurnTiming{QueuedMs: 3}
			m.Trace = 12
		}
		if err := enc.Encode(m); err != nil {
			tb.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err := f.Close(); err != nil {
		tb.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		tb.Fatal(err)
	}
	return s, r.ID, info.Size()
}

func liveHeap() uint64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapAlloc
}

// BenchmarkHistoryLoad reports the startup load time and the heap the loaded
// history keeps alive after GC (live-MB, live-B/msg).
func BenchmarkHistoryLoad(b *testing.B) {
	for _, n := range historySizes {
		b.Run(fmt.Sprintf("messages=%d", n), func(b *testing.B) {
			s, id, size := writeHistoryRoom(b, n)
			var live uint64
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				before := liveHeap()
				b.StartTimer()
				h, err := NewHub(s, agent.NewRegistry(&fakeProvider{id: "codex"}), s.Root(), nil)
				if err != nil {
					b.Fatal(err)
				}
				b.StopTimer()
				live = liveHeap() - before
				if got := len(h.rooms[id].msgs); got != n {
					b.Fatalf("loaded %d messages, want %d", got, n)
				}
				runtime.KeepAlive(h)
				b.StartTimer()
			}
			b.ReportMetric(float64(size)/(1<<20), "disk-MB")
			b.ReportMetric(float64(live)/(1<<20), "live-MB")
			b.ReportMetric(float64(live)/float64(n), "live-B/msg")
		})
	}
}

// BenchmarkHistoryScan times the reads that walk the whole history on every
// open or turn: the first snapshot page, a seat's delta, the ask list and the
// room usage page.
func BenchmarkHistoryScan(b *testing.B) {
	for _, n := range historySizes {
		s, id, _ := writeHistoryRoom(b, n)
		h, err := NewHub(s, agent.NewRegistry(&fakeProvider{id: "codex"}), s.Root(), nil)
		if err != nil {
			b.Fatal(err)
		}
		rs := h.rooms[id]
		p := rs.room.Participants[0]
		p.LastSeen = int64(n - 10)
		cases := []struct {
			name string
			run  func() error
		}{
			{"snapshot", func() error { _, err := h.SnapshotJSON(id); return err }},
			{"delta", func() error {
				h.mu.Lock()
				out, _, _ := rs.deltaFor(p, false, rs.room.Seq)
				h.mu.Unlock()
				if len(out) == 0 {
					return fmt.Errorf("empty delta")
				}
				return nil
			}},
			{"asks", func() error { h.mu.Lock(); rs.askViews(); h.mu.Unlock(); return nil }},
			{"usage", func() error { _, err := h.RoomUsage(id); return err }},
		}
		for _, c := range cases {
			b.Run(fmt.Sprintf("messages=%d/%s", n, c.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					if err := c.run(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
