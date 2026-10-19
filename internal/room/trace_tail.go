package room

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"roundtable/internal/agent"
)

// TraceTail contains the last N events plus earlier start/latest meta records
// for parents in that window. Events remain in file order. Truncated describes
// the N-event window, even if supplementary records fill all of the gaps.
type TraceTail struct {
	Events    []agent.Event `json:"events"`
	Total     int           `json:"total"`
	Truncated bool          `json:"truncated"`
}

func (h *Hub) ReadTraceTail(id, msgID string, limit int) (TraceTail, error) {
	if err := h.checkTraceMessage(id, msgID); err != nil {
		return TraceTail{}, err
	}
	return h.store.ReadTraceTail(id, msgID, limit)
}

// ReadTraceTail takes the same stable size snapshot as ReadTrace. An exact
// per-message total still requires scanning the file, but only selected events
// are decoded in full and retained: O(limit + largest line) live memory.
// The HTTP boundary validates limit (1..5000).
func (s *Store) ReadTraceTail(roomID, messageID string, limit int) (TraceTail, error) {
	result := TraceTail{Events: []agent.Event{}}
	path := filepath.Join(s.roomDir(roomID), "events.jsonl")
	s.mu.Lock()
	f, err := os.Open(path)
	if err != nil {
		s.mu.Unlock()
		if os.IsNotExist(err) {
			return result, nil
		}
		return result, err
	}
	info, err := f.Stat()
	s.mu.Unlock()
	defer f.Close()
	if err != nil {
		return result, err
	}
	type parentState struct{ start, meta bool }
	parents := map[string]parentState{}
	err = readJSONLinesReverse(f, info.Size(), func(line []byte) error {
		var header struct {
			MsgID string `json:"msg_id"`
			Event struct {
				Kind   string `json:"kind"`
				Parent string `json:"parent"`
			} `json:"event"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return err
		}
		if header.MsgID != messageID {
			return nil
		}
		result.Total++
		inWindow := result.Total <= limit
		state, wanted := parents[header.Event.Parent]
		keep := inWindow
		if header.Event.Parent != "" && (inWindow || wanted) {
			switch header.Event.Kind {
			case agent.KindSubagentStart:
				keep = keep || !state.start
				state.start = true
			case agent.KindSubagentMeta:
				keep = keep || !state.meta
				state.meta = true
			}
			parents[header.Event.Parent] = state
		}
		if keep {
			var record TraceLine
			if err := json.Unmarshal(line, &record); err != nil {
				return err
			}
			result.Events = append(result.Events, record.Event)
		}
		return nil
	})
	if err != nil {
		return TraceTail{}, fmt.Errorf("%s: %w", path, err)
	}
	slices.Reverse(result.Events)
	result.Truncated = result.Total > limit
	return result, nil
}

// Read blocks from EOF without loading the file or imposing a line-size limit.
// A line is accumulated in reverse, then restored before decoding. A final
// newline is a delimiter; blank or malformed records still report an error.
func readJSONLinesReverse(source io.ReaderAt, size int64, consume func([]byte) error) error {
	block := make([]byte, 64*1024)
	var line []byte
	end := size
	for end > 0 {
		start := max(int64(0), end-int64(len(block)))
		chunk := block[:end-start]
		if _, err := source.ReadAt(chunk, start); err != nil {
			return err
		}
		for i := len(chunk) - 1; i >= 0; i-- {
			if chunk[i] != '\n' {
				line = append(line, chunk[i])
				continue
			}
			if start+int64(i) == size-1 {
				continue
			}
			slices.Reverse(line)
			if err := consume(line); err != nil {
				return fmt.Errorf("record after byte %d: %w", start+int64(i), err)
			}
			line = line[:0]
		}
		end = start
	}
	if size > 0 {
		slices.Reverse(line)
		return consume(line)
	}
	return nil
}
