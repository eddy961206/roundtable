package room

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// People invited from other PCs (docs/사내에이전트_커뮤니티_설계.md 4.7). A
// person token signs a browser in on the community listener, where the person
// sees community spaces only. A viewer watches; a member also sends messages,
// assigns tasks and approves results. Only the token's hash is stored.
const (
	RoleViewer        = "viewer"
	RoleMember        = "member"
	personTokenPrefix = "rtp_"
	maxPersonName     = 40
)

// ErrPersonAuth is a missing, unknown or revoked person token.
var ErrPersonAuth = errors.New("초대 토큰이 없거나 맞지 않음")

type Person struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
	At   int64  `json:"at"`
}

type personRecord struct {
	Person
	Hash string `json:"hash"`
}

func (s *Store) peoplePath() string { return filepath.Join(s.root, "people.json") }

func (s *Store) loadPeople() ([]personRecord, error) {
	b, err := os.ReadFile(s.peoplePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []personRecord
	return out, json.Unmarshal(b, &out)
}

func (s *Store) savePeople(people []personRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(people, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.peoplePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.peoplePath())
}

// InvitePerson makes a person and returns the token to hand over once.
func (h *Hub) InvitePerson(name, role string) (Person, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxPersonName || strings.ContainsAny(name, "\r\n") {
		return Person{}, "", fmt.Errorf("이름은 1~%d자 한 줄이어야 함", maxPersonName)
	}
	if role != RoleViewer && role != RoleMember {
		return Person{}, "", fmt.Errorf("역할은 viewer(구경) 또는 member(참여)여야 함: %q", role)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return Person{}, "", err
	}
	token := personTokenPrefix + hex.EncodeToString(raw)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.people {
		if p.Name == name {
			return Person{}, "", fmt.Errorf("같은 이름의 사람이 이미 있음: %s (메시지 작성자를 구별하려고 이름이 겹치지 않게 함)", name)
		}
	}
	p := Person{ID: newID(), Name: name, Role: role, At: nowMs()}
	next := append(append([]personRecord{}, h.people...), personRecord{Person: p, Hash: hashToken(token)})
	if err := h.store.savePeople(next); err != nil {
		return Person{}, "", err
	}
	h.people = next
	return p, token, nil
}

func (h *Hub) People() []Person {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []Person{}
	for _, p := range h.people {
		out = append(out, p.Person)
	}
	return out
}

// RevokePerson removes the person; their token and sessions stop working.
func (h *Hub) RevokePerson(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := []personRecord{}
	for _, p := range h.people {
		if p.ID != id {
			next = append(next, p)
		}
	}
	if len(next) == len(h.people) {
		return fmt.Errorf("초대한 사람 없음: %s", id)
	}
	if err := h.store.savePeople(next); err != nil {
		return err
	}
	h.people = next
	return nil
}

func (h *Hub) PersonByToken(token string) (Person, error) {
	hash := hashToken(token)
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.people {
		if p.Hash == hash {
			return p.Person, nil
		}
	}
	return Person{}, ErrPersonAuth
}

// PersonByID finds a signed-in person again on each request, so a revoked
// person's session stops at once.
func (h *Hub) PersonByID(id string) (Person, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, p := range h.people {
		if p.ID == id {
			return p.Person, nil
		}
	}
	return Person{}, ErrPersonAuth
}

// SpaceKind tells whether a room is a community space a person may see.
func (h *Hub) SpaceKind(roomID string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[roomID]
	if !ok || rs.room.Kind == "" {
		return "", fmt.Errorf("공간 없음: %s", roomID)
	}
	return rs.room.Kind, nil
}

// PostAs is a person's message from another PC; by names them to the seats
// and on screen.
func (h *Hub) PostAs(roomID, text, target, by string) (*Message, error) {
	return h.postUser(roomID, text, target, nil, nil, by)
}
