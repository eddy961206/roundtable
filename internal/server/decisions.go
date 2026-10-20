package server

import (
	"net/http"

	"roundtable/internal/room"
)

// Decision cards (docs/에이전트MCP_블라인드토론_결정카드_계약.md, C). GET returns
// the cards in recorded order; POST adds a person's card (active); PATCH edits
// or changes status, which confirms (active) or rejects (dropped) a seat's
// proposed card. Every change is also sent as the SSE event "decision".

func (s *Server) listDecisions(w http.ResponseWriter, r *http.Request) {
	decisions, err := s.hub.Decisions(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, decisions)
}

func (s *Server) addDecision(w http.ResponseWriter, r *http.Request) {
	var in room.DecisionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	d, err := s.hub.AddDecision(r.PathValue("id"), in)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, d)
}

func (s *Server) patchDecision(w http.ResponseWriter, r *http.Request) {
	var in room.DecisionInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	d, err := s.hub.UpdateDecision(r.PathValue("id"), r.PathValue("did"), in)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, d)
}
