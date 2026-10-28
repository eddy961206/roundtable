package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"time"

	"roundtable/internal/room"
)

func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, room.MaxAttachmentBytes+64*1024)
	reader, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	part, err := reader.NextPart()
	if err != nil {
		writeErr(w, 400, fmt.Errorf("file 필드가 필요함"))
		return
	}
	defer part.Close()
	if part.FormName() != "file" || part.FileName() == "" {
		writeErr(w, 400, fmt.Errorf("file 파일 필드 하나만 허용"))
		return
	}
	// Buffer at most one bounded image so extra multipart fields cannot leave
	// a successful upload behind while the API reports a rejected request.
	b, err := io.ReadAll(io.LimitReader(part, room.MaxAttachmentBytes+1))
	if err != nil || int64(len(b)) > room.MaxAttachmentBytes {
		writeErr(w, 400, fmt.Errorf("이미지 읽기 실패 또는 파일당 10MB 초과"))
		return
	}
	if _, err := reader.NextPart(); err != io.EOF {
		writeErr(w, 400, fmt.Errorf("file 파일 필드 하나만 허용"))
		return
	}
	a, err := s.hub.UploadAttachment(r.PathValue("id"), part.FileName(), bytes.NewReader(b))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, a)
}

func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request) {
	f, a, err := s.hub.OpenAttachment(r.PathValue("id"), r.PathValue("aid"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", a.Mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, a.Name, time.UnixMilli(a.At), f)
}

// applyMarkers runs the markers of a failed turn whose answer was complete.
func (s *Server) applyMarkers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Seq int64 `json:"seq"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	msg, err := s.hub.ApplyMarkers(r.PathValue("id"), body.Seq)
	if err != nil && msg == nil {
		writeErr(w, 400, err)
		return
	}
	if err != nil { // applied, but a marker could not be stored; the room has the notice
		writeJSON(w, 200, map[string]any{"message": msg, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"message": msg})
}

// answerAsk answers a seat's question (room/asks.go): the answer goes to the
// seat as a user message addressed to it.
func (s *Server) answerAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Choice int    `json:"choice"`
		Text   string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	msg, ask, err := s.hub.AnswerAsk(r.PathValue("id"), r.PathValue("ask"), body.Choice, body.Text)
	if err != nil && msg == nil {
		writeErr(w, 400, err)
		return
	}
	if err != nil { // the answer was sent, but the question's status could not be stored
		writeJSON(w, 200, map[string]any{"message": msg, "ask": ask, "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"message": msg, "ask": ask})
}

func (s *Server) cancelRequest(w http.ResponseWriter, r *http.Request) {
	request, err := s.hub.CancelRequest(r.PathValue("id"), r.PathValue("rid"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, request)
}
