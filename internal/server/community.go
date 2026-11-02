package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"roundtable/internal/room"
)

// Community is the network listener for remote seat workers
// (docs/사내에이전트_커뮤니티_설계.md). It serves only /community/v1, never the
// local /api, so a colleague's PC can run its seat's turns but cannot read
// other rooms, start CLIs here or change settings. Every request needs the
// seat's connection token, and the peer address must be on the allow list.
type Community struct {
	hub   *room.Hub
	allow []*net.IPNet
	mux   *http.ServeMux
}

// DefaultCommunityAllow is loopback plus the private ranges of a company LAN.
const DefaultCommunityAllow = "127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7"

// ParseAllow reads a comma-separated CIDR list.
func ParseAllow(list string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		_, n, err := net.ParseCIDR(item)
		if err != nil {
			return nil, fmt.Errorf("허용 주소 범위 %q: %w", item, err)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("허용 주소 범위가 비어 있음")
	}
	return out, nil
}

func NewCommunity(hub *room.Hub, allow []*net.IPNet) *Community {
	c := &Community{hub: hub, allow: allow, mux: http.NewServeMux()}
	c.mux.HandleFunc("GET /community/v1/me", c.me)
	c.mux.HandleFunc("GET /community/v1/job", c.job)
	c.mux.HandleFunc("POST /community/v1/job/{turn}/result", c.result)
	c.mux.HandleFunc("POST /community/v1/job/{turn}/progress", c.progress)
	return c
}

func (c *Community) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	ip := net.ParseIP(peer)
	if err != nil || ip == nil || !c.allowed(ip) {
		http.Error(w, "허용되지 않은 주소", http.StatusForbidden)
		return
	}
	// Workers are programs, not pages: a browser request from any site is refused.
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "브라우저 요청은 받지 않음", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	c.mux.ServeHTTP(w, r)
}

func (c *Community) allowed(ip net.IP) bool {
	for _, n := range c.allow {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

func bearer(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	return strings.TrimSpace(token)
}

func writeRemoteErr(w http.ResponseWriter, err error) {
	if errors.Is(err, room.ErrRemoteAuth) {
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	writeErr(w, http.StatusConflict, err)
}

func (c *Community) me(w http.ResponseWriter, r *http.Request) {
	view, err := c.hub.RemoteMe(bearer(r))
	if err != nil {
		writeRemoteErr(w, err)
		return
	}
	writeJSON(w, 200, view)
}

// job is a worker's long poll: 200 with the seat's turn, or 204 after wait.
func (c *Community) job(w http.ResponseWriter, r *http.Request) {
	wait := 30 * time.Second
	if raw := r.URL.Query().Get("wait"); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 || time.Duration(seconds)*time.Second > room.MaxRemoteWait {
			writeErr(w, 400, fmt.Errorf("wait는 0~%d초여야 함", int(room.MaxRemoteWait.Seconds())))
			return
		}
		wait = time.Duration(seconds) * time.Second
	}
	job, err := c.hub.RemoteNext(r.Context(), bearer(r), wait)
	if err != nil {
		writeRemoteErr(w, err)
		return
	}
	if job == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, 200, job)
}

func (c *Community) result(w http.ResponseWriter, r *http.Request) {
	var res room.RemoteResult
	if err := decode(r, &res); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := c.hub.RemoteFinish(bearer(r), r.PathValue("turn"), res); err != nil {
		writeRemoteErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// progress takes the answer a worker has written so far; 409 tells it the
// turn is gone and it should stop its CLI.
func (c *Community) progress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := c.hub.RemoteProgress(bearer(r), r.PathValue("turn"), body.Text); err != nil {
		writeRemoteErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// assignTask is a person's task for one seat (room.AssignTask).
func (s *Server) assignTask(w http.ResponseWriter, r *http.Request) {
	var body struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, task, err := s.hub.AssignTask(r.PathValue("id"), body.To, body.Text)
	if errors.Is(err, room.ErrWorkspaceBusy) {
		writeErr(w, 409, err)
		return
	}
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, map[string]any{"message": m, "request": task})
}

// remoteToken issues a remote seat's connection token; it is shown only in
// this response.
func (s *Server) remoteToken(w http.ResponseWriter, r *http.Request) {
	token, err := s.hub.IssueRemoteToken(r.PathValue("id"), r.PathValue("participant"))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 201, map[string]string{"token": token, "community_url": s.communityURL})
}
