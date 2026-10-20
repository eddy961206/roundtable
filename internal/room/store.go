package room

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"roundtable/internal/agent"
)

// Store persists rooms as plain files: room.json, messages.jsonl (upserts,
// last line per id wins) and events.jsonl (append only).
type Store struct {
	root string
	mu   sync.Mutex
}

func NewStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "rooms"), 0o700); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) Root() string { return s.root }

func (s *Store) roomDir(id string) string { return filepath.Join(s.root, "rooms", id) }

// roomFile is room.json: the room plus its work plan, which room events omit (D13).
type roomFile struct {
	*Room
	Work *WorkPlan `json:"work,omitempty"`
}

func (s *Store) SaveRoom(r *Room) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.roomDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(roomFile{Room: r, Work: r.Work}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, "room.json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "room.json"))
}

func (s *Store) DeleteRoom(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return os.RemoveAll(s.roomDir(id))
}

func (s *Store) AppendMessage(roomID string, m *Message) error {
	return s.appendLine(filepath.Join(s.roomDir(roomID), "messages.jsonl"), m)
}

func (s *Store) AppendTrace(roomID string, t TraceLine) error {
	return s.appendLine(filepath.Join(s.roomDir(roomID), "events.jsonl"), t)
}

func (s *Store) appendLine(path string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(append(b, '\n'))
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	return closeErr
}

// SaveLimits keeps the last known vendor limits so they survive a restart.
func (s *Store) SaveLimits(v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.root, "limits.json"), b, 0o600)
}

// LoadLimits returns the saved limits map, or nil when there is none yet.
func (s *Store) LoadLimits(into any) error {
	b, err := os.ReadFile(filepath.Join(s.root, "limits.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(b, into)
}

// LoadAll reads full stored history for callers that explicitly need every trace.
func (s *Store) LoadAll() ([]*Room, map[string][]*Message, map[string][]TraceLine, error) {
	rooms, messages, traces, err := s.loadRooms(true, true)
	interruptStreaming(messages)
	return rooms, messages, traces, err
}

// LoadRooms leaves event payloads on disk. Only missing legacy session metadata
// is recovered while loading messages; ordinary room reads never scan traces.
func (s *Store) LoadRooms() ([]*Room, map[string][]*Message, error) {
	rooms, messages, _, err := s.loadRooms(false, true)
	interruptStreaming(messages)
	return rooms, messages, err
}

// Public history readers keep their existing in-memory recovery semantics.
// NewHub persists interruptions separately, before it launches any new turns.
func interruptStreaming(messages map[string][]*Message) {
	for _, roomMessages := range messages {
		for _, m := range roomMessages {
			if m.Status == "streaming" {
				m.Status = "cancelled"
			}
		}
	}
}

// captureRoomFiles holds the append lock only for one room's metadata and
// message-file boundary. Incomplete/deleted directories without room.json are
// not rooms. Other read errors remain visible to every caller.
func (s *Store) captureRoomFiles(id string) ([]byte, *os.File, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := os.OpenRoot(s.roomDir(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, err
	}
	defer dir.Close()
	data, err := dir.ReadFile("room.json")
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, 0, nil
		}
		return nil, nil, 0, err
	}
	// Root.Open shares delete access on Windows, so a long snapshot reader
	// does not prevent room deletion while it consumes the captured prefix.
	f, err := dir.Open("messages.jsonl")
	if err != nil {
		if os.IsNotExist(err) {
			return data, nil, 0, nil // a new room may have no messages yet
		}
		return nil, nil, 0, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, 0, err
	}
	return data, f, info.Size(), nil
}

func (s *Store) readRoomSnapshot(id string) (*Room, []*Message, error) {
	data, f, size, err := s.captureRoomFiles(id)
	if err != nil || data == nil {
		return nil, nil, err
	}
	if f != nil {
		defer f.Close()
	}
	// A room saved before blind_first_default existed has no key and keeps the default.
	r := Room{BlindFirstDefault: true}
	file := roomFile{Room: &r}
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, nil, fmt.Errorf("room %s: %w", id, err)
	}
	r.Work = file.Work
	messages := []*Message{}
	if f != nil {
		messages, err = readMessages(io.LimitReader(f, size), f.Name())
		if err != nil {
			return nil, nil, err
		}
	}
	// Messages can be committed after the last room metadata save (system
	// notes, or a crash during a turn). Never reuse their sequence numbers.
	for _, m := range messages {
		r.Seq = max(r.Seq, m.Seq)
		r.UpdatedAt = max(r.UpdatedAt, m.TS)
	}
	return &r, messages, nil
}

func (s *Store) loadRooms(withTraces, recoverSessions bool) ([]*Room, map[string][]*Message, map[string][]TraceLine, error) {
	entries, err := os.ReadDir(filepath.Join(s.root, "rooms"))
	if err != nil {
		return nil, nil, nil, err
	}
	var rooms []*Room
	msgs := map[string][]*Message{}
	traces := map[string][]TraceLine{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := s.roomDir(e.Name())
		r, roomMessages, err := s.readRoomSnapshot(e.Name())
		if err != nil {
			return nil, nil, nil, err
		}
		if r == nil {
			continue
		}
		// Older rooms used the provider ID as their single seat identity.
		// Keep that identity so messages and resumable sessions remain attached.
		for _, p := range r.Participants {
			if p.Provider == "" {
				p.Provider = p.ID
			}
		}
		rooms = append(rooms, r)
		msgs[r.ID] = roomMessages
		missingSessions := map[string]*Message{}
		for _, m := range msgs[r.ID] {
			if recoverSessions && m.Kind == "agent" && m.SessionID == "" {
				missingSessions[m.ID] = m
			}
		}
		if withTraces || len(missingSessions) > 0 {
			err = scanJSONLines(filepath.Join(dir, "events.jsonl"), func(line []byte) error {
				var record TraceLine
				if err := json.Unmarshal(line, &record); err != nil {
					return err
				}
				if withTraces {
					traces[r.ID] = append(traces[r.ID], record)
				}
				if message := missingSessions[record.MsgID]; message != nil && record.Event.Kind == agent.KindSession && record.Event.Text != "" {
					message.SessionID = record.Event.Text
				}
				return nil
			})
			if err != nil {
				return nil, nil, nil, err
			}
		}
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].UpdatedAt > rooms[j].UpdatedAt })
	return rooms, msgs, traces, nil
}

func readMessages(source io.Reader, path string) ([]*Message, error) {
	byID := map[string]*Message{}
	var order []string
	err := readJSONLines(source, path, func(line []byte) error {
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		if _, ok := byID[m.ID]; !ok {
			order = append(order, m.ID)
		}
		byID[m.ID] = &m
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]*Message, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// scanJSONLines reports corrupt/truncated records instead of silently losing
// everything after Scanner's old 32 MiB line limit.
func scanJSONLines(path string, consume func([]byte) error) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	return readJSONLines(f, path, consume)
}

func readJSONLines(source io.Reader, path string, consume func([]byte) error) error {
	reader := bufio.NewReaderSize(source, 64*1024)
	for lineNumber := 1; ; lineNumber++ {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if decodeErr := consume(line); decodeErr != nil {
				return fmt.Errorf("%s:%d: %w", path, lineNumber, decodeErr)
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%s:%d: %w", path, lineNumber, err)
		}
	}
}

// ReadTrace reads one message's events from a stable file-size snapshot. Opening
// and sizing share the append lock, but the scan does not block live writers.
func (s *Store) ReadTrace(roomID, messageID string) ([]agent.Event, error) {
	path := filepath.Join(s.roomDir(roomID), "events.jsonl")
	s.mu.Lock()
	f, err := os.Open(path)
	if err != nil {
		s.mu.Unlock()
		if os.IsNotExist(err) {
			return []agent.Event{}, nil
		}
		return nil, err
	}
	info, err := f.Stat()
	s.mu.Unlock()
	defer f.Close()
	if err != nil {
		return nil, err
	}
	result := []agent.Event{}
	err = readJSONLines(io.LimitReader(f, info.Size()), path, func(line []byte) error {
		var header struct {
			MsgID string `json:"msg_id"`
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return err
		}
		if header.MsgID != messageID {
			return nil
		}
		var record TraceLine
		if err := json.Unmarshal(line, &record); err != nil {
			return err
		}
		result = append(result, record.Event)
		return nil
	})
	return result, err
}
