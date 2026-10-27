// Package server exposes the hub over HTTP: JSON API, SSE stream, static UI.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"roundtable/internal/agent"
	"roundtable/internal/room"
	"roundtable/internal/update"
)

type Server struct {
	hub     *room.Hub
	reg     *agent.Registry
	web     fs.FS
	mux     *http.ServeMux
	stopped chan struct{} // closed once POST /api/shutdown closed the hub
	updates interface{ Status() update.Status }
}

func New(hub *room.Hub, reg *agent.Registry, web fs.FS) *Server {
	s := &Server{hub: hub, reg: reg, web: web, mux: http.NewServeMux(), stopped: make(chan struct{})}
	s.routes()
	if reg != nil {
		go func() { _ = reg.RefreshVersions(context.Background()) }()
	}
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
	s.mux.Handle("GET /", staticFiles(s.web))
	s.mux.HandleFunc("GET /api/config", s.config)
	s.mux.HandleFunc("GET /api/version", s.version)
	s.mux.HandleFunc("GET /api/providers", s.providers)
	s.mux.HandleFunc("POST /api/providers/versions", s.refreshVersions)
	s.mux.HandleFunc("GET /api/role-presets", s.rolePresets)
	s.mux.HandleFunc("GET /api/stats/models", s.modelStats)
	s.mux.HandleFunc("GET /api/usage", s.usage)
	s.mux.HandleFunc("GET /api/stream", s.streamRooms)
	s.mux.HandleFunc("POST /api/usage/refresh", s.refreshUsage)
	s.mux.HandleFunc("GET /api/rooms", s.listRooms)
	s.mux.HandleFunc("POST /api/rooms", s.createRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}", s.getRoom)
	s.mux.HandleFunc("GET /api/rooms/{id}/usage", s.roomUsage)
	s.mux.HandleFunc("POST /api/shutdown", s.shutdown)
	s.mux.HandleFunc("PATCH /api/rooms/{id}", s.patchRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants", s.addParticipants)
	s.mux.HandleFunc("DELETE /api/rooms/{id}/participants/{participant}", s.removeParticipant)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants/{participant}/stop", s.stopParticipant)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants/{participant}/handoff", s.handoff)
	s.mux.HandleFunc("GET /api/rooms/{id}/participants/{participant}/session", s.nativeSession)
	s.mux.HandleFunc("POST /api/rooms/{id}/participants/{participant}/session/open", s.openNativeSession)
	s.mux.HandleFunc("DELETE /api/rooms/{id}", s.deleteRoom)
	s.mux.HandleFunc("POST /api/rooms/{id}/messages", s.postMessage)
	s.mux.HandleFunc("POST /api/rooms/{id}/forward", s.forwardMessage)
	s.mux.HandleFunc("POST /api/rooms/{id}/attachments", s.uploadAttachment)
	s.mux.HandleFunc("GET /api/rooms/{id}/attachments/{aid}", s.getAttachment)
	s.mux.HandleFunc("POST /api/rooms/{id}/requests/{rid}/cancel", s.cancelRequest)
	s.mux.HandleFunc("POST /api/rooms/{id}/markers", s.applyMarkers)
	s.mux.HandleFunc("GET /api/rooms/{id}/decisions", s.listDecisions)
	s.mux.HandleFunc("POST /api/rooms/{id}/decisions", s.addDecision)
	s.mux.HandleFunc("PATCH /api/rooms/{id}/decisions/{did}", s.patchDecision)
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
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/resolve", s.resolvePart)
	s.mux.HandleFunc("POST /api/rooms/{id}/work/parts/{pid}/abandon", s.abandonPart)
	s.mux.HandleFunc("GET /api/rooms/{id}/work/parts/{pid}/diff", s.partDiff)
}

// staticFiles serves the web client. The page loads ES modules, which browsers
// run only with a JavaScript MIME type under nosniff. Windows can map .js to
// text/plain through the registry, so .js gets its type here, not from mime.
func staticFiles(web fs.FS) http.Handler {
	files := http.FileServer(http.FS(web))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".js") {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		}
		files.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// decode reads one JSON value from the request body. encoding/json would turn
// bytes that are not UTF-8 (e.g. an inline curl argument in a Windows console
// code page) into U+FFFD and store the damage, so such a body is refused.
func decode(r *http.Request, v any) error {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 4<<20))
	if err != nil {
		return err
	}
	if !utf8.Valid(body) {
		return errors.New("요청 본문이 UTF-8이 아님: 한글 등은 UTF-8로 저장한 파일을 curl --data-binary @파일로 보내")
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(v)
}

func (s *Server) config(w http.ResponseWriter, r *http.Request) {
	wd, _ := os.Getwd()
	writeJSON(w, 200, map[string]any{"default_workspace": wd, "version": Version, "max_participants": room.MaxParticipants})
}

// SetUpdates attaches the update checker read by GET /api/version. Call it
// before serving; without it the endpoint reports state "off".
func (s *Server) SetUpdates(u interface{ Status() update.Status }) { s.updates = u }

func (s *Server) version(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, 200, update.Status{State: "off", Running: update.ReadBuild(), Commits: []update.Commit{}})
		return
	}
	writeJSON(w, 200, s.updates.Status())
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
		Attachments []string `json:"attachments"`
		Text        string   `json:"text"`
		Target      string   `json:"target"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := s.hub.PostUser(r.PathValue("id"), body.Text, body.Target, body.Attachments...)
	if errors.Is(err, room.ErrWorkspaceBusy) {
		writeErr(w, 409, err)
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, m)
}

// forwardMessage copies another room's message here, text unchanged.
func (s *Server) forwardMessage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FromRoom string `json:"from_room"`
		Seq      int64  `json:"seq"`
		Target   string `json:"target"`
		Note     string `json:"note"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := s.hub.Forward(r.PathValue("id"), body.FromRoom, body.Seq, body.Target, body.Note)
	if errors.Is(err, room.ErrWorkspaceBusy) {
		writeErr(w, 409, err)
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) handoff(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Note  string `json:"note"`
		Fresh bool   `json:"fresh"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := s.hub.Handoff(r.PathValue("id"), r.PathValue("participant"), body.Note, body.Fresh)
	if errors.Is(err, room.ErrWorkspaceBusy) {
		writeErr(w, 409, err)
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, m)
}

func (s *Server) discussion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Rounds     int   `json:"rounds"`
		BlindFirst *bool `json:"blind_first"` // omitted: the room's blind_first_default
	}
	// An empty body starts with the room defaults; a malformed one is rejected.
	if err := decode(r, &body); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, err)
		return
	}
	if err := s.hub.StartDiscussion(r.PathValue("id"), body.Rounds, body.BlindFirst); err != nil {
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
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if values, present := query["tail"]; present {
		limit, err := strconv.Atoi(values[0])
		if err != nil || len(values) != 1 || limit < 1 || limit > 5000 {
			writeErr(w, 400, fmt.Errorf("tail은 1~5000 사이 정수여야 함"))
			return
		}
		page, err := s.hub.ReadTraceTail(r.PathValue("id"), r.PathValue("msg"), limit)
		if err != nil {
			writeErr(w, 500, err)
			return
		}
		writeJSON(w, 200, page)
		return
	}
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

func (s *Server) resolvePart(w http.ResponseWriter, r *http.Request) {
	plan, err := s.hub.ResolvePart(r.PathValue("id"), r.PathValue("pid"))
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
	serveSSE(w, r, ch)
}

// streamRooms is the SSE feed of room-list changes. Its events are signals only;
// the client reads GET /api/rooms after each one.
func (s *Server) streamRooms(w http.ResponseWriter, r *http.Request) {
	ch, unsub, err := s.hub.SubscribeRooms()
	if err != nil {
		writeErr(w, 503, err)
		return
	}
	defer unsub()
	serveSSE(w, r, ch)
}

// serveSSE sends hello, then each event of ch, with a ping every 20 seconds. It
// returns when the client leaves, a write fails or the hub closes ch.
func serveSSE(w http.ResponseWriter, r *http.Request, ch <-chan []byte) {
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
