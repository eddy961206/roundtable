package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These benchmarks exercise the same transcript polling used while a CLI is
// streaming, without starting a CLI or making model calls. Files are warmed by
// setup and every poll opens/closes them just as production does.
func BenchmarkAgyTranscriptIdle(b *testing.B) {
	for _, count := range []int{1, 4, 16, 32} {
		b.Run(fmt.Sprintf("subagents_%d", count), func(b *testing.B) {
			p := newAgyParser()
			dir := b.TempDir()
			body := []byte(`{"type":"PLANNER_RESPONSE","content":"already observed"}` + "\n")
			for i := 0; i < count; i++ {
				id := fmt.Sprintf("child-%d", i)
				path := filepath.Join(dir, id+".jsonl")
				if err := os.WriteFile(path, body, 0o600); err != nil {
					b.Fatal(err)
				}
				p.subs[id] = &agySub{logPath: path, offset: int64(len(body))}
			}
			emit := func(Event) { b.Fatal("idle poll emitted a duplicate event") }
			p.readTranscripts(emit, false) // warm filesystem and any reader cache
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.readTranscripts(emit, false)
			}
		})
	}
}

// Run explicitly with -benchtime=1000x (or longer) to compare steady repeated
// polling resources. Heap is measured after GC with parser state still alive;
// this is not RSS, and it does not stand in for a multi-hour real CLI soak.
func BenchmarkTranscriptResourceSoak(b *testing.B) {
	dir := b.TempDir()
	p := newAgyParser()
	files := make([]*rolloutFile, 32)
	for i := range files {
		id := fmt.Sprintf("child-%d", i)
		path := filepath.Join(dir, id+".jsonl")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			b.Fatal(err)
		}
		p.subs[id] = &agySub{logPath: path}
		files[i] = &rolloutFile{path: path, id: id}
	}
	w := &rolloutWatcher{}
	emit := func(Event) { b.Fatal("idle poll emitted an event") }
	poll := func() {
		p.readTranscripts(emit, false)
		for _, rf := range files {
			w.tail(rf, emit)
		}
	}
	fdCount := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return -1 // Linux descriptor observation is optional on other hosts.
		}
		return len(entries)
	}
	poll()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	goroutines, descriptors := runtime.NumGoroutine(), fdCount()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		poll()
	}
	b.StopTimer()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(p)
	runtime.KeepAlive(files)
	b.ReportMetric(float64(after.HeapAlloc), "live-heap-B")
	b.ReportMetric(float64(after.NumGC-before.NumGC), "GCs")
	b.ReportMetric(float64(runtime.NumGoroutine()-goroutines), "goroutine-delta")
	if end := fdCount(); descriptors >= 0 && end >= 0 {
		b.ReportMetric(float64(end-descriptors), "fd-delta")
	}
}

func BenchmarkCodexRolloutIdle(b *testing.B) {
	for _, count := range []int{1, 4, 16, 32} {
		b.Run(fmt.Sprintf("subagents_%d", count), func(b *testing.B) {
			dir := b.TempDir()
			body := []byte(`{"type":"event_msg","payload":{"type":"already_observed"}}` + "\n")
			files := make([]*rolloutFile, count)
			for i := range files {
				id := fmt.Sprintf("child-%d", i)
				path := filepath.Join(dir, id+".jsonl")
				if err := os.WriteFile(path, body, 0o600); err != nil {
					b.Fatal(err)
				}
				files[i] = &rolloutFile{path: path, id: id, offset: int64(len(body))}
			}
			w := &rolloutWatcher{}
			emit := func(Event) { b.Fatal("idle poll emitted a duplicate event") }
			for _, rf := range files {
				w.tail(rf, emit)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for _, rf := range files {
					w.tail(rf, emit)
				}
			}
		})
	}
}

// Replaying one complete line isolates read/parsing cost from fixture writes.
// The largest line exceeds the reader buffer and guards against improving idle
// polls at the expense of large tool outputs.
func BenchmarkTranscriptActive(b *testing.B) {
	for _, size := range []int{4 << 10, 64 << 10, 512 << 10} {
		for _, provider := range []string{"agy", "codex"} {
			b.Run(fmt.Sprintf("%s/bytes_%d", provider, size), func(b *testing.B) {
				text := strings.Repeat("x", size)
				body := `{"type":"PLANNER_RESPONSE","content":"` + text + `"}` + "\n"
				if provider == "codex" {
					body = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}}` + "\n"
				}
				path := filepath.Join(b.TempDir(), "transcript.jsonl")
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					b.Fatal(err)
				}
				p := newAgyParser()
				sub := &agySub{logPath: path}
				p.subs["child"] = sub
				w := &rolloutWatcher{}
				rf := &rolloutFile{path: path, id: "child"}
				seen := 0
				emit := func(Event) { seen++ }
				poll := func() {
					if provider == "agy" {
						sub.offset = 0
						p.readTranscripts(emit, false)
					} else {
						rf.offset = 0
						w.tail(rf, emit)
					}
				}
				poll()
				seen = 0
				b.SetBytes(int64(len(body)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					poll()
				}
				b.StopTimer()
				if seen != b.N {
					b.Fatalf("got %d events for %d polls", seen, b.N)
				}
			})
		}
	}
}
