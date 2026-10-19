// Package server exposes the hub over HTTP: JSON API, SSE stream, static UI.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"roundtable/internal/agent"
	"roundtable/internal/room"
)

type Server struct {
	hub *room.Hub
	reg *agent.Registry
	web fs.FS
	mux *http.ServeMux
}

func New(hub *room.Hub, reg *agent.Registry, web fs.FS) *Server {
	s := &Server{hub: hub, reg: reg, web: web, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// This unauthenticated service can start local CLI processes. Restrict both
	// the peer and Host to loopback, including when -addr is accidentally public.
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	peerIP := net.ParseIP(peer)
	if (host != "localhost" && (ip == nil || !ip.IsLoopback())) || err != nil || peerIP == nil || !peerIP.IsLoopback() {
		http.Error(w, "local access only", http.StatusForbidden)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != scheme || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			http.Error(w, "same-origin access only", http.StatusForbidden)
			return
		}
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
		http.Error(w, "same-origin access only", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.Handle("GET /", http.FileServer(http.FS(s.web)))
	s.mux.HandleFunc("GET /api/config", s.config)
	s.mux.HandleFunc("GET /api/providers", s.providers)
	s.mux.HandleFunc("GET /api/usage", s.usage)
	s.mux.HandleFunc("POST /api/usage/refresh", s.refreshUsage)
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.createRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}", s.getRoom)
	s.mux.HandleFunc("PATCH /api/rooms/{id}", s.patchRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants", s.addParticipants)
	s.mux.HandleFunc("DELETE /api/rooms/{id}/participants/{participant}", s.removeParticipant)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants/{participant}/stop", s.stopParticipant)
	s.mux.HandleFunc("GET /api/rooms/{id}/participants/{participant}/session", s.nativeSession)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants/{participant}/session/open", s.openNativeSession)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.deleteRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/messages", s.postMessage)
	s.mux.HandleFunc("POST /api/rooms/{id}/discussion", s.discussion)
	s.mux.HandleFunc("POST /api/rooms/{id}/stop", s.stop)
	s.mux.HandleFunc("POST /api/rooms/{id}/reset", s.reset)
	s.mux.HandleFunc("DELETE /api/rooms/{id}/turns/{turn}", s.cancelTurn)
	s.mux.HandleFunc("GET /api/rooms/{id}/trace/{msg}", s.trace)
	s.mux.HandleFunc("GET /api/rooms/{id}/stream", s.stream)
	s.mux.HandleFunc("GET /api/rooms/{id}/export.md", s.export)
	s.mux.HandleFunc("POST /api/rooms/{id}/work", s.createWork)
	s.mux.HandleFunc("PATCH /api/rooms/{id}/work", s.patchWork)
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/submit", s.submitPart)
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/approve", s.approvePart)
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/retry", s.retryPart)
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/abandon", s.abandonPart)
	s.mux.HandleFunc("GET /api/rooms/{id}/work/parts/{pid}/diff", s.partDiff)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20)).Decode(v)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	wd, _ := os.Getwd()
	writeJSON(w, 200, map[string]any{"default_workspace": wd, "version": Version, "max_participants": room.MaxParticipants})
}

// Version is stamped at build time with -ldflags "-X roundtable/internal/server.Version=…".
var Version = "dev"

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "true" {
		for _, provider := range s.reg.All() {
			if catalog, ok := provider.(interface{ InvalidateModelCatalog() }); ok {
				catalog.InvalidateModelCatalog()
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"providers": s.reg.Infos(), "limits": s.hub.Limits()})
}

func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.hub.Rooms())
}

func (s *Server) createRoom(w http.ResponseWriter, r *http.Request) {
	var spec room.RoomSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	rm, err := s.hub.CreateRoom(spec)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, rm)
}

func (s *Server) getRoom(w http.ResponseWriter, r *http.Request) {
	var before int64
	limit := 100
	if raw := r.URL.Query().Get("before"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			writeErr(w, 400, fmt.Errorf("before는 0 이상의 메시지 번호여야 함"))
			return
		}
		before = value
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 200 {
			writeErr(w, 400, fmt.Errorf("limit은 1~200이어야 함"))
			return
		}
		limit = value
	}
	b, err := s.hub.SnapshotPageJSON(r.PathValue("id"), before, limit)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) patchRoom(w http.ResponseWriter, r *http.Request) {
	var patch room.RoomPatch
	if err := decode(r, &patch); err != nil {
		writeErr(w, 400, err)
		return
	}
	b, err := s.hub.UpdateRoom(r.PathValue("id"), patch)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) addParticipants(w http.ResponseWriter, r *http.Request) {
	var spec room.ParticipantSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	b, err := s.hub.AddParticipants(r.PathValue("id"), spec)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(201)
	_, _ = w.Write(b)
}

func (s *Server) removeParticipant(w http.ResponseWriter, r *http.Request) {
	b, err := s.hub.RemoveParticipant(r.PathValue("id"), r.PathValue("participant"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(b)
}

func (s *Server) deleteRoom(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.DeleteRoom(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) postMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text   string `json:"text"`
		Target string `json:"target"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := s.hub.PostUser(r.PathValue("id"), body.Text, body.Target)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) discussion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rounds int `json:"rounds"`
	}
	_ = decode(r, &body)
	if err := s.hub.StartDiscussion(r.PathValue("id"), body.Rounds); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.Stop(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) reset(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.ResetSessions(r.PathValue("id")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) cancelTurn(w http.ResponseWriter, r *http.Request) {
	if err := s.hub.CancelTurn(r.PathValue("id"), r.PathValue("turn")); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) trace(w http.ResponseWriter, r *http.Request) {
	events, err := s.hub.ReadTrace(r.PathValue("id"), r.PathValue("msg"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if events == nil {
		events = []agent.Event{}
	}
	writeJSON(w, 200, events)
}

func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	md, err := s.hub.ExportMarkdown(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"roundtable-%s.md\"", r.PathValue("id")))
	_, _ = w.Write([]byte(md))
}

// writeWork answers a work-plan call; state conflicts (D6 approval mismatch,
// existing plan, wrong part state) are 409.
func writeWork(w http.ResponseWriter, code int, v any, err error) {
	switch {
	case errors.Is(err, room.ErrWorkConflict):
		writeErr(w, 409, err)
	case err != nil:
		writeErr(w, 400, err)
	default:
		writeJSON(w, code, v)
	}
}

func (s *Server) createWork(w http.ResponseWriter, r *http.Request) {
	var spec room.WorkSpec
	if err := decode(r, &spec); err != nil {
		writeErr(w, 400, err)
		return
	}
	plan, err := s.hub.CreateWork(r.PathValue("id"), spec)
	writeWork(w, 201, plan, err)
}

func (s *Server) patchWork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MergeMode string `json:"merge_mode"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	plan, err := s.hub.SetMergeMode(r.PathValue("id"), body.MergeMode)
	writeWork(w, 200, plan, err)
}

func (s *Server) submitPart(w http.ResponseWriter, r *http.Request) {
	plan, err := s.hub.SubmitPart(r.PathValue("id"), r.PathValue("pid"))
	writeWork(w, 200, plan, err)
}

func (s *Server) approvePart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SubmitSHA      string `json:"submit_sha"`
		IntegrationSHA string `json:"integration_sha"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	plan, err := s.hub.ApprovePart(r.PathValue("id"), r.PathValue("pid"), body.SubmitSHA, body.IntegrationSHA)
	writeWork(w, 200, plan, err)
}

func (s *Server) retryPart(w http.ResponseWriter, r *http.Request) {
	plan, err := s.hub.RetryPart(r.PathValue("id"), r.PathValue("pid"))
	writeWork(w, 200, plan, err)
}

func (s *Server) abandonPart(w http.ResponseWriter, r *http.Request) {
	plan, err := s.hub.AbandonPart(r.PathValue("id"), r.PathValue("pid"))
	writeWork(w, 200, plan, err)
}

func (s *Server) partDiff(w http.ResponseWriter, r *http.Request) {
	diff, err := s.hub.PartDiff(r.PathValue("id"), r.PathValue("pid"))
	writeWork(w, 200, diff, err)
}

// stream is the SSE feed of one room.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ch, unsub, err := s.hub.Subscribe(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	defer unsub()
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	send := func(format string, args ...any) bool {
		if err := controller.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return controller.Flush() == nil
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	if !send("event: hello\ndata: %s\n\n", strconv.FormatInt(time.Now().UnixMilli(), 10)) {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if !send(": ping\n\n") {
				return
			}
		case b, ok := <-ch:
			if !ok {
				return
			}
			if !send("data: %s\n\n", b) { // json.Marshal never emits raw newlines
				return
			}
		}
	}
}
