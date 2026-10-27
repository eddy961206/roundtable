package room

import (
	"fmt"
	"strings"
)

// ForwardSource names where a forwarded message came from
// (docs/다중에이전트_도구조사_20261027.md item 1).
type ForwardSource struct {
	Room   string `json:"room"`
	Title  string `json:"title"`
	Seq    int64  `json:"seq"`
	Author string `json:"author"`
	Note   string `json:"note,omitempty"` // one line from the sender, shown before the text
}

// Forward copies message seq of fromRoom, text unchanged, into toRoom as a user
// message for target (or every active seat). The orchestrator moves a review or
// a result without reading and re-typing it. Quoted @mentions and markers do not
// start turns: only the named target (or the broadcast) does.
func (h *Hub) Forward(toRoom, fromRoom string, seq int64, target, note string) (*Message, error) {
	if toRoom == fromRoom {
		return nil, fmt.Errorf("같은 방으로는 전달하지 않음")
	}
	if strings.ContainsAny(note, "\r\n") || len([]rune(note)) > 300 {
		return nil, fmt.Errorf("전달 메모는 300자 이내 한 줄")
	}
	h.mu.Lock()
	src, ok := h.rooms[fromRoom]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("보낼 방 없음: %s", fromRoom)
	}
	var orig *Message
	for _, m := range src.msgs {
		if m.Seq == seq {
			orig = m
		}
	}
	var fwd ForwardSource
	var text string
	if orig != nil {
		fwd, text = ForwardSource{Room: fromRoom, Title: src.room.Title, Seq: seq, Author: orig.Author, Note: strings.TrimSpace(note)}, orig.Text
	}
	streaming := orig != nil && orig.Status == "streaming"
	h.mu.Unlock()
	switch {
	case orig == nil:
		return nil, fmt.Errorf("방 %s에 #%d 메시지 없음", fromRoom, seq)
	case streaming:
		return nil, fmt.Errorf("#%d은 아직 쓰는 중인 답이라 전달하지 않음 · 끝난 뒤 다시", seq)
	case strings.TrimSpace(text) == "":
		return nil, fmt.Errorf("#%d은 본문이 없어 전달할 것이 없음", seq)
	}
	return h.postUser(toRoom, text, target, &fwd, nil)
}

// forwardLabel is how a receiving seat sees the source in its history.
func forwardLabel(f *ForwardSource) string {
	s := fmt.Sprintf("전달: 방 %q %s#%d @%s", f.Title, f.Room, f.Seq, f.Author)
	if f.Note != "" {
		s += " · 메모: " + f.Note
	}
	return s
}
