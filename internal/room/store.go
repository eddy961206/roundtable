package room

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
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

func (s *Store) SaveRoom(r *Room) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.roomDir(r.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
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
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
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

// LoadAll reads every room with its messages and traces.
func (s *Store) LoadAll() ([]*Room, map[string][]*Message, map[string][]TraceLine, error) {
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
		b, err := os.ReadFile(filepath.Join(dir, "room.json"))
		if err != nil {
			continue
		}
		var r Room
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, nil, nil, fmt.Errorf("room %s: %w", e.Name(), err)
		}
		// Older rooms used the provider ID as their single seat identity.
		// Keep that identity so messages and resumable sessions remain attached.
		for _, p := range r.Participants {
			if p.Provider == "" {
				p.Provider = p.ID
			}
		}
		rooms = append(rooms, &r)
		msgs[r.ID] = loadMessages(filepath.Join(dir, "messages.jsonl"))
		traces[r.ID] = loadTraces(filepath.Join(dir, "events.jsonl"))
	}
	sort.Slice(rooms, func(i, j int) bool { return rooms[i].UpdatedAt > rooms[j].UpdatedAt })
	return rooms, msgs, traces, nil
}

func loadMessages(path string) []*Message {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	byID := map[string]*Message{}
	var order []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if _, ok := byID[m.ID]; !ok {
			order = append(order, m.ID)
		}
		if m.Status == "streaming" {
			m.Status = "cancelled"
		}
		byID[m.ID] = &m
	}
	out := make([]*Message, 0, len(order))
	for _, id := range order {
		out = append(out, byID[id])
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

func loadTraces(path string) []TraceLine {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []TraceLine
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 32*1024*1024)
	for sc.Scan() {
		var t TraceLine
		if err := json.Unmarshal(sc.Bytes(), &t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}
