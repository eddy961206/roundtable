package room

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"roundtable/internal/agent"
)

func TestTraceTailKeepsWindowAndParentContext(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRoom(&Room{ID: "r"}); err != nil {
		t.Fatal(err)
	}
	events := []agent.Event{
		{Kind: agent.KindSubagentStart, Parent: "absent"},
		{Kind: agent.KindSubagentStart, Parent: "a", Input: "task a"},
		{Kind: agent.KindSubagentMeta, Parent: "a", Text: "old"},
		{Kind: agent.KindSubagentStart, Parent: "b", Input: "task b"},
		{Kind: agent.KindSubagentMeta, Parent: "b", Text: "old"},
		{Kind: agent.KindSubagentMeta, Parent: "a", Text: "latest a"},
		{Kind: agent.KindText, Text: "omitted"},
		{Kind: agent.KindSubagentMsg, Parent: "a"},
		{Kind: agent.KindSubagentMeta, Parent: "b", Text: "latest b"},
		{Kind: agent.KindSubagentStart, Parent: "c"},
		{Kind: agent.KindSubagentEnd, Parent: "b"},
		{Kind: agent.KindSubagentMsg, Parent: "missing-context"},
	}
	for i, event := range events {
		event.TS = int64(i)
		events[i] = event
		if err := s.AppendTrace("r", TraceLine{MsgID: "m", Event: event}); err != nil {
			t.Fatal(err)
		}
		if err := s.AppendTrace("r", TraceLine{MsgID: "other", Event: event}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.ReadTraceTail("r", "m", 5)
	want := append([]agent.Event{events[1], events[3], events[5]}, events[7:]...)
	if err != nil || page.Total != len(events) || !page.Truncated || !reflect.DeepEqual(page.Events, want) {
		t.Fatalf("tail: %+v, err %v; want %v", page, err, want)
	}
	full, err := s.ReadTrace("r", "m")
	if err != nil || !reflect.DeepEqual(full, events) {
		t.Fatalf("full: %v, %v", full, err)
	}
	for _, limit := range []int{len(events), 5000} {
		page, err := s.ReadTraceTail("r", "m", limit)
		if err != nil || page.Total != len(events) || page.Truncated || !reflect.DeepEqual(page.Events, events) {
			t.Fatalf("unclipped %d: %+v, %v", limit, page, err)
		}
	}
	for _, roomID := range []string{"r", "missing"} {
		page, err := s.ReadTraceTail(roomID, "missing", 1)
		if err != nil || page.Total != 0 || page.Truncated || page.Events == nil || len(page.Events) != 0 {
			t.Fatalf("empty: %+v, %v", page, err)
		}
	}
}

func TestReverseJSONLinesBoundariesAndSnapshot(t *testing.T) {
	lines := []string{`{"first":1}`, `{"large":"` + strings.Repeat("한", 90_000) + `"}`, `{"last":3}`}
	for _, newline := range []string{"\n", "\r\n"} {
		for _, trailing := range []bool{false, true} {
			data := strings.Join(lines, newline)
			if trailing {
				data += newline
			}
			var got []string
			err := readJSONLinesReverse(strings.NewReader(data+"not in snapshot"), int64(len(data)), func(line []byte) error {
				if !json.Valid(line) {
					t.Fatalf("invalid line, length %d", len(line))
				}
				got = append(got, string(bytes.TrimSpace(line)))
				return nil
			})
			want := slices.Clone(lines)
			slices.Reverse(want)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("reverse mismatch, err %v", err)
			}
		}
	}
	for _, size := range []int{65535, 65536, 65537, 131072} {
		data := `"` + strings.Repeat("x", size-3) + "\"\n"
		count := 0
		err := readJSONLinesReverse(strings.NewReader(data), int64(len(data)), func(line []byte) error {
			count++
			if !json.Valid(line) || len(line) != size-1 {
				t.Fatalf("boundary %d", size)
			}
			return nil
		})
		if err != nil || count != 1 {
			t.Fatalf("boundary %d: count %d, %v", size, count, err)
		}
	}
}

func TestTraceTailReportsCorruptRecordsOutsideWindow(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRoom(&Room{ID: "r"}); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		"\n", "{broken\n", "{broken\n{\"msg_id\":\"m\",\"event\":{\"kind\":\"text\"}}\n",
	} {
		if err := os.WriteFile(filepath.Join(s.roomDir("r"), "events.jsonl"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReadTraceTail("r", "m", 1); err == nil {
			t.Fatalf("accepted corrupt record %q", data)
		}
	}
}
