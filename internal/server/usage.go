package server

import (
	"net/http"
	"strings"
)

func (s *Server) usage(w http.ResponseWriter, r *http.Request) {
	var providers []string
	if query := r.URL.Query().Get("providers"); query != "" {
		providers = strings.Split(query, ",")
	}
	view, err := s.hub.Usage(providers)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}

func (s *Server) refreshUsage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Providers []string `json:"providers"`
		Force     bool     `json:"force"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	view, err := s.hub.RefreshUsage(r.Context(), body.Providers, body.Force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, view)
}
