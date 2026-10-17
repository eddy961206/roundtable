package server

import (
	"fmt"
	"net/http"

	"roundtable/internal/agent"
)

type nativeSessionView struct {
	agent.NativeSession
	Command string `json:"command"`
	CanOpen bool   `json:"can_open"`
}

func (s *Server) nativeSession(w http.ResponseWriter, r *http.Request) {
	provider, options, err := s.hub.NativeSessionOptions(r.PathValue("id"), r.PathValue("participant"), r.URL.Query().Get("message"))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	plan, err := agent.NativeSessionCommand(provider, options)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, nativeSessionView{NativeSession: plan, Command: nativeCommand(plan), CanOpen: canOpenNativeSession})
}

func (s *Server) openNativeSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MessageID string `json:"message_id"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if !canOpenNativeSession {
		http.Error(w, "이 운영체제에서는 이어보기 명령을 복사해서 CLI에서 실행하세요", http.StatusNotImplemented)
		return
	}
	// Resolve the executable before pausing, then recheck the session under the
	// hub lock. The detached options remain the source of truth for the launch.
	provider, options, err := s.hub.NativeSessionOptions(r.PathValue("id"), r.PathValue("participant"), body.MessageID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	if _, err := agent.NativeSessionCommand(provider, options); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	provider, options, paused, err := s.hub.DetachNativeSession(r.PathValue("id"), r.PathValue("participant"), body.MessageID)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	launched := false
	release := func() { s.hub.ReleaseNativeSession(provider.ID(), options.SessionID) }
	defer func() {
		if !launched {
			release()
		}
	}()
	launchError := func(err error) {
		if paused {
			err = fmt.Errorf("%w. 참가자는 비활성화 상태야. 원인을 해결한 뒤 다시 켜거나 CLI 열기를 재시도해", err)
		}
		writeErr(w, http.StatusInternalServerError, err)
	}
	plan, err := agent.NativeSessionCommand(provider, options)
	if err != nil {
		launchError(err)
		return
	}
	pid, err := openNativeSession(plan, release)
	if err != nil {
		launchError(err)
		return
	}
	launched = true
	writeJSON(w, http.StatusOK, map[string]any{"launched": true, "session_id": plan.SessionID, "pid": pid, "paused": paused})
}
