package server

import "net/http"

func (s *Server) rolePresets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	presets, err := s.hub.RolePresets()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, presets)
}

func (s *Server) modelStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	stats, err := s.hub.ModelStats()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, stats)
}

func (s *Server) refreshVersions(w http.ResponseWriter, r *http.Request) {
	if err := s.reg.RefreshVersions(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.providers(w, r)
}

func (s *Server) roomUsage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	usage, err := s.hub.RoomUsage(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, usage)
}
