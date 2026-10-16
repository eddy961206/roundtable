// Package server exposes the hub over HTTP: JSON API, SSE stream, static UI.
package server

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
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

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

func (s *Server) routes() {
	s.mux.Handle("GET /", http.FileServer(http.FS(s.web)))
	s.mux.HandleFunc("GET /api/config", s.config)
	s.mux.HandleFunc("GET /api/providers", s.providers)
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.createRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}", s.getRoom)
	s.mux.HandleFunc("PATCH /api/rooms/{id}", s.patchRoom)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.deleteRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/messages", s.postMessage)
	s.mux.HandleFunc("POST /api/rooms/{id}/discussion", s.discussion)
	s.mux.HandleFunc("POST /api/rooms/{id}/stop", s.stop)
	s.mux.HandleFunc("POST /api/rooms/{id}/reset", s.reset)
	s.mux.HandleFunc("DELETE /api/rooms/{id}/turns/{turn}", s.cancelTurn)
	s.mux.HandleFunc("GET /api/rooms/{id}/trace/{msg}", s.trace)
	s.mux.HandleFunc("GET /api/rooms/{id}/stream", s.stream)
	s.mux.HandleFunc("GET /api/rooms/{id}/export.md", s.export)
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
	writeJSON(w, 200, map[string]any{"default_workspace": wd, "version": Version})
}

// Version is stamped at build time with -ldflags "-X roundtable/internal/server.Version=…".
var Version = "dev"

func (s *Server) providers(w http.ResponseWriter, r *http.Request) {
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
	snap, err := s.hub.Snapshot(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, snap)
}

func (s *Server) patchRoom(w http.ResponseWriter, r *http.Request) {
	var patch room.RoomPatch
	if err := decode(r, &patch); err != nil {
		writeErr(w, 400, err)
		return
	}
	rm, err := s.hub.UpdateRoom(r.PathValue("id"), patch)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, rm)
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
	events := s.hub.Trace(r.PathValue("id"), r.PathValue("msg"))
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

// stream is the SSE feed of one room.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ch, unsub, err := s.hub.Subscribe(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	defer unsub()
	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, fmt.Errorf("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprintf(w, "event: hello\ndata: %s\n\n", strconv.FormatInt(time.Now().UnixMilli(), 10))
	fl.Flush()
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case b, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", strings.ReplaceAll(string(b), "\n", "\\n"))
			fl.Flush()
		}
	}
}
