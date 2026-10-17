package server

import "net/http"

func (s *Server) stopParticipant(w http.ResponseWriter, r *http.Request) {
	result, err := s.hub.StopParticipant(r.PathValue("id"), r.PathValue("participant"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(result)
}
