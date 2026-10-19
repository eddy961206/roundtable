package room

import (
	"fmt"
	"path/filepath"
	"slices"

	"roundtable/internal/agent"
)

func nativeSessionKey(provider, session string) string { return provider + "\x00" + session }

func (h *Hub) seatNativeOpen(rs *roomState, p *Participant) bool {
	if h.nativeSessions[nativeSessionKey(p.Provider, p.SessionID)] {
		return true
	}
	part := rs.seatPart(p.ID)
	return part != nil && h.nativeSessions[nativeSessionKey(p.Provider, part.SessionID)]
}

// NativeSessionOptions returns the original session and invocation settings.
// Legacy current sessions use the room's last saved settings; historical sessions
// without a recorded absolute working directory cannot be reopened through this API.
func (h *Hub) NativeSessionOptions(roomID, seatID, messageID string) (agent.Provider, agent.RunOptions, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nativeSessionOptionsLocked(roomID, seatID, messageID)
}

func (h *Hub) nativeSessionOptionsLocked(roomID, seatID, messageID string) (agent.Provider, agent.RunOptions, error) {
	var opts agent.RunOptions
	if h.closing {
		return nil, opts, fmt.Errorf("서버가 종료 중이야")
	}
	rs := h.rooms[roomID]
	if rs == nil {
		return nil, opts, fmt.Errorf("방 없음: %s", roomID)
	}
	p := rs.room.participant(seatID)
	if p == nil {
		return nil, opts, fmt.Errorf("참가자 없음: %s", seatID)
	}
	provider := p.Provider
	var selected *Message
	if messageID != "" {
		for _, m := range rs.msgs {
			if m.ID == messageID && m.Kind == "agent" && m.Author == seatID {
				selected = m
				break
			}
		}
		if selected == nil {
			return nil, opts, fmt.Errorf("참가자의 메시지가 아님: %s", messageID)
		}
	} else {
		if p.SessionID == "" {
			return nil, opts, fmt.Errorf("저장된 CLI 세션이 없음")
		}
		for i := len(rs.msgs) - 1; i >= 0; i-- {
			m := rs.msgs[i]
			if m.Kind == "agent" && m.Author == seatID && m.SessionID == p.SessionID && m.SessionDirectory != "" {
				selected = m
				break
			}
		}
	}
	if selected != nil {
		if selected.SessionID == "" {
			return nil, opts, fmt.Errorf("메시지에 저장된 CLI 세션이 없음")
		}
		if !filepath.IsAbs(selected.SessionDirectory) || selected.Mode == "" {
			return nil, opts, fmt.Errorf("과거 세션의 작업 폴더와 모드 기록이 없음")
		}
		provider = selected.Provider
		opts = agent.RunOptions{SessionID: selected.SessionID, Model: selected.Model, Effort: selected.Effort, Mode: selected.Mode, Workspace: selected.SessionDirectory, ScratchDir: selected.SessionDirectory}
	} else {
		// Pre-metadata releases persisted only the current seat session and room settings.
		opts = agent.RunOptions{SessionID: p.SessionID, Model: p.Model, Effort: p.Effort, Mode: rs.room.Mode, Workspace: rs.room.Workspace, ScratchDir: h.scratch}
	}
	prov := h.reg.Get(provider)
	if prov == nil {
		return nil, opts, fmt.Errorf("알 수 없는 공급자: %s", provider)
	}
	if h.nativeSessions[nativeSessionKey(provider, opts.SessionID)] {
		return nil, opts, fmt.Errorf("이 세션의 CLI 창이 이미 열려 있어. 창을 닫은 뒤 다시 시도해")
	}
	for _, state := range h.rooms {
		for _, run := range state.running {
			if run.msg.Provider == provider && run.msg.SessionID == opts.SessionID {
				return nil, opts, fmt.Errorf("이 CLI 세션은 현재 응답 중이야. 응답이 끝난 뒤 열어")
			}
		}
	}
	return prov, opts, nil
}

// DetachNativeSession pauses the current seat before handing its session to an
// interactive CLI. Opening an older, unrelated session leaves the current seat alone.
func (h *Hub) DetachNativeSession(roomID, seatID, messageID string) (agent.Provider, agent.RunOptions, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prov, opts, err := h.nativeSessionOptionsLocked(roomID, seatID, messageID)
	if err != nil {
		return nil, opts, false, err
	}
	rs := h.rooms[roomID]
	p := rs.room.participant(seatID)
	paused := p.SessionID == opts.SessionID && p.Provider == prov.ID()
	if part := rs.seatPart(p.ID); part != nil && part.SessionID == opts.SessionID && p.Provider == prov.ID() {
		paused = true
	}
	if paused && p.Enabled {
		next := *rs.room
		next.Participants = slices.Clone(next.Participants)
		disabled := *p
		disabled.Enabled = false
		for i, participant := range next.Participants {
			if participant.ID == seatID {
				next.Participants[i] = &disabled
			}
		}
		next.UpdatedAt = nowMs()
		if err := h.store.SaveRoom(&next); err != nil {
			return nil, opts, false, err
		}
		p.Enabled = false
		rs.room.UpdatedAt = next.UpdatedAt
		rs.dropQueuedFor(h, seatID)
		rs.broadcast("room", rs.room)
	}
	h.nativeSessions[nativeSessionKey(prov.ID(), opts.SessionID)] = true
	h.pump(rs)
	return prov, opts, paused, nil
}

// ReleaseNativeSession ends the reservation after the interactive console exits.
// The seat stays paused until the user explicitly enables it.
func (h *Hub) ReleaseNativeSession(providerID, sessionID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.nativeSessions, nativeSessionKey(providerID, sessionID))
}
