package room

import (
	"io"
	"log"
	"strings"
	"testing"
	"unicode/utf8"

	"roundtable/internal/agent"
)

func roomSummary(t *testing.T, h *Hub, id string) RoomSummary {
	t.Helper()
	for _, s := range h.Rooms() {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("방 요약 없음: %s", id)
	return RoomSummary{}
}

func TestRoomGroupPatchAndSummary(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	if s := roomSummary(t, h, r.ID); s.Group != "" || s.Workspace != r.Workspace {
		t.Fatalf("초기 요약: %+v", s)
	}
	if _, err := h.UpdateRoom(r.ID, RoomPatch{Group: ref("  설계 검토 ")}); err != nil {
		t.Fatal(err)
	}
	if s := roomSummary(t, h, r.ID); s.Group != "설계 검토" {
		t.Fatalf("그룹 지정: %q", s.Group)
	}
	// Other patches leave the group alone.
	if _, err := h.UpdateRoom(r.ID, RoomPatch{Title: ref("새 이름")}); err != nil {
		t.Fatal(err)
	}
	if s := roomSummary(t, h, r.ID); s.Group != "설계 검토" {
		t.Fatalf("다른 변경 뒤 그룹: %q", s.Group)
	}
	// A blank group returns the room to its folder group.
	if _, err := h.UpdateRoom(r.ID, RoomPatch{Group: ref("   ")}); err != nil {
		t.Fatal(err)
	}
	if s := roomSummary(t, h, r.ID); s.Group != "" {
		t.Fatalf("그룹 해제: %q", s.Group)
	}
}

func TestRoomGroupPersists(t *testing.T) {
	a := &fakeProvider{id: "claude"}
	store, _ := NewStore(t.TempDir())
	hub, _ := NewHub(store, agent.NewRegistry(a), t.TempDir(), log.New(io.Discard, "", 0))
	r, _ := hub.CreateRoom(RoomSpec{Title: "p", Mode: agent.ModeChat, Participants: []string{"claude"}})
	if _, err := hub.UpdateRoom(r.ID, RoomPatch{Group: ref("핫픽스")}); err != nil {
		t.Fatal(err)
	}
	created, _ := hub.CreateRoom(RoomSpec{Title: "c", Mode: agent.ModeChat, Participants: []string{"claude"}, Group: "생성"})
	hub2, err := NewHub(store, agent.NewRegistry(a), t.TempDir(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if s := roomSummary(t, hub2, r.ID); s.Group != "핫픽스" {
		t.Fatalf("재시작 뒤 그룹: %q", s.Group)
	}
	if s := roomSummary(t, hub2, created.ID); s.Group != "생성" {
		t.Fatalf("재시작 뒤 생성 시 그룹: %q", s.Group)
	}
}

// The list input stops at 120 characters; the API must not store more either.
func TestRoomGroupTooLong(t *testing.T) {
	h, r := newTestHub(t, &fakeProvider{id: "codex"})
	if _, err := h.UpdateRoom(r.ID, RoomPatch{Group: ref("기존")}); err != nil {
		t.Fatal(err)
	}
	for _, tooLong := range []string{strings.Repeat("a", 121), strings.Repeat("😀", 121)} {
		if _, err := h.UpdateRoom(r.ID, RoomPatch{Group: ref(tooLong), Title: ref("바뀌면 안 됨")}); err == nil {
			t.Fatalf("%d자 그룹이 저장됨", utf8.RuneCountInString(tooLong))
		}
		if s := roomSummary(t, h, r.ID); s.Group != "기존" || s.Title == "바뀌면 안 됨" {
			t.Fatalf("거절 뒤 요약: %+v", s)
		}
	}
	// The limit counts characters, not bytes, after trimming.
	max := strings.Repeat("가", 120)
	if _, err := h.UpdateRoom(r.ID, RoomPatch{Group: ref("  " + max + "  ")}); err != nil {
		t.Fatal(err)
	}
	if s := roomSummary(t, h, r.ID); s.Group != max {
		t.Fatalf("120자 그룹: %q", s.Group)
	}
}

// A new room takes its group through the same check as PATCH.
func TestRoomGroupOnCreate(t *testing.T) {
	h, _ := newTestHub(t, &fakeProvider{id: "codex"})
	spec := func(group string) RoomSpec {
		return RoomSpec{Title: "g", Mode: agent.ModeChat, Participants: []string{"codex"}, Group: group}
	}
	max := strings.Repeat("가", 120)
	for group, want := range map[string]string{"  " + max + "  ": max, "": ""} {
		r, err := h.CreateRoom(spec(group))
		if err != nil {
			t.Fatal(err)
		}
		if s := roomSummary(t, h, r.ID); s.Group != want {
			t.Fatalf("생성 시 그룹 %q: %q", group, s.Group)
		}
	}
	before := len(h.Rooms())
	if _, err := h.CreateRoom(spec(strings.Repeat("a", 121))); err == nil {
		t.Fatal("121자 그룹으로 방이 만들어짐")
	}
	if n := len(h.Rooms()); n != before {
		t.Fatalf("거절 뒤 방 수 %d, 이전 %d", n, before)
	}
}
