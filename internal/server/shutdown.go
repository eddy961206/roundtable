package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

// ShutdownRequest asks the server to exit once no turn runs or waits. There is
// no forced form; an interrupt signal already stops running turns.
type ShutdownRequest struct {
	WhenIdle       bool `json:"when_idle"`
	TimeoutSeconds int  `json:"timeout_seconds"`
}

// ShutdownResult tells `roundtable restart` how to start the same server again.
type ShutdownResult struct {
	PID  int      `json:"pid"`
	Exe  string   `json:"exe"`
	Args []string `json:"args"` // os.Args of the stopped server
	Dir  string   `json:"dir"`
	// Error reports a failure after the hub had already closed (stop notes not saved).
	Error string `json:"error,omitempty"`
}

// Stopped is closed after POST /api/shutdown closed the hub; the process then
// stops serving HTTP.
func (s *Server) Stopped() <-chan struct{} { return s.stopped }

func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	var req ShutdownRequest
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !req.WhenIdle || req.TimeoutSeconds < 1 || req.TimeoutSeconds > 24*3600 {
		writeErr(w, 400, fmt.Errorf("when_idle:true와 timeout_seconds(1~86400)가 필요함"))
		return
	}
	exe, err := os.Executable()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutSeconds)*time.Second)
	defer cancel()
	closed, err := s.hub.CloseWhenIdle(ctx)
	if !closed {
		writeErr(w, 409, err)
		return
	}
	result := ShutdownResult{PID: os.Getpid(), Exe: exe, Args: os.Args, Dir: dir}
	if err != nil {
		result.Error = err.Error()
	}
	writeJSON(w, 200, result)
	close(s.stopped)
}
