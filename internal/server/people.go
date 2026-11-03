package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"roundtable/internal/room"
)

// People on other PCs (docs/사내에이전트_커뮤니티_설계.md 4.7) open the
// community listener in a browser. An invite token signs them in once and
// becomes an HttpOnly, SameSite=Strict session cookie. They see community
// spaces only, never ordinary rooms, the host's folders, CLI sessions or work
// traces (a trace holds file contents). A member may also send, assign tasks
// and approve or drop task results.
const (
	personCookie  = "rt_person"
	personSession = 12 * time.Hour
	// A state-changing request must carry this header. A page on another site
	// cannot add it without a CORS preflight, which this server never allows.
	personHeader   = "X-RT-Person"
	spaceMessages  = 60
	streamCoalesce = 500 * time.Millisecond
	// One person signs in from a few browsers; repeated logins with one
	// invite replace the oldest session instead of filling the memory.
	maxPersonSessions = 8
)

type session struct {
	person  string
	expires time.Time
	n       uint64 // login order; clock ticks can tie
}

type people struct {
	hub      *room.Hub
	web      fs.FS
	mu       sync.Mutex
	sessions map[string]session
	logins   uint64
}

// peopleFiles are the only static files the community listener serves.
var peopleFiles = map[string]string{
	"/":                 "people.html",
	"/style.css":        "style.css",
	"/js/people.js":     "js/people.js",
	"/js/space-view.js": "js/space-view.js",
	"/js/util.js":       "js/util.js",
}

func (pp *people) routes(mux *http.ServeMux) {
	for path := range peopleFiles {
		pattern := "GET " + path
		if path == "/" {
			pattern = "GET /{$}" // the page itself, not every path
		}
		mux.HandleFunc(pattern, pp.static)
	}
	mux.HandleFunc("POST /people/login", pp.login)
	mux.HandleFunc("POST /people/logout", pp.logout)
	mux.HandleFunc("GET /people/api/me", pp.auth(false, pp.me))
	mux.HandleFunc("GET /people/api/spaces", pp.auth(false, pp.spaces))
	mux.HandleFunc("GET /people/api/spaces/{id}", pp.auth(false, pp.space))
	mux.HandleFunc("GET /people/api/spaces/{id}/stream", pp.auth(false, pp.stream))
	mux.HandleFunc("POST /people/api/spaces/{id}/messages", pp.auth(true, pp.message))
	mux.HandleFunc("POST /people/api/spaces/{id}/tasks", pp.auth(true, pp.task))
	mux.HandleFunc("PATCH /people/api/spaces/{id}/decisions/{did}", pp.auth(true, pp.decide))
}

func (pp *people) static(w http.ResponseWriter, r *http.Request) {
	name := peopleFiles[r.URL.Path]
	b, err := fs.ReadFile(pp.web, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	_, _ = w.Write(b)
}

// sameSite refuses a state change that did not come from this listener's own
// page: the custom header, and an Origin (when sent) equal to this host.
func sameSite(r *http.Request) bool {
	if r.Header.Get(personHeader) != "1" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		if err != nil || u.Host != r.Host {
			return false
		}
	}
	site := r.Header.Get("Sec-Fetch-Site")
	return site == "" || site == "same-origin"
}

func (pp *people) newSession(person string) (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	pp.mu.Lock()
	defer pp.mu.Unlock()
	now := time.Now()
	oldest, own := "", 0
	for k, s := range pp.sessions { // drop expired sessions as new ones come
		switch {
		case now.After(s.expires):
			delete(pp.sessions, k)
		case s.person == person:
			own++
			if oldest == "" || s.n < pp.sessions[oldest].n {
				oldest = k
			}
		}
	}
	if own >= maxPersonSessions {
		delete(pp.sessions, oldest)
	}
	pp.logins++
	pp.sessions[id] = session{person: person, expires: now.Add(personSession), n: pp.logins}
	return id, nil
}

func setCookie(w http.ResponseWriter, r *http.Request, value string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: personCookie, Value: value, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: maxAge})
}

func (pp *people) login(w http.ResponseWriter, r *http.Request) {
	if !sameSite(r) {
		writeErr(w, http.StatusForbidden, errors.New("이 화면에서만 로그인할 수 있음"))
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	p, err := pp.hub.PersonByToken(strings.TrimSpace(body.Token))
	if err != nil {
		time.Sleep(300 * time.Millisecond) // slows guessing; a token has 192 random bits anyway
		writeErr(w, http.StatusUnauthorized, err)
		return
	}
	id, err := pp.newSession(p.ID)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	setCookie(w, r, id, int(personSession.Seconds()))
	writeJSON(w, 200, p)
}

func (pp *people) logout(w http.ResponseWriter, r *http.Request) {
	if !sameSite(r) {
		writeErr(w, http.StatusForbidden, errors.New("이 화면에서만 로그아웃할 수 있음"))
		return
	}
	if c, err := r.Cookie(personCookie); err == nil {
		pp.mu.Lock()
		delete(pp.sessions, c.Value)
		pp.mu.Unlock()
	}
	setCookie(w, r, "", -1)
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// auth runs h for a signed-in person; write needs a member and this site.
func (pp *people) auth(write bool, h func(http.ResponseWriter, *http.Request, room.Person)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(personCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, room.ErrPersonAuth)
			return
		}
		pp.mu.Lock()
		s, ok := pp.sessions[c.Value]
		if ok && time.Now().After(s.expires) {
			delete(pp.sessions, c.Value)
			ok = false
		}
		pp.mu.Unlock()
		if !ok {
			writeErr(w, http.StatusUnauthorized, room.ErrPersonAuth)
			return
		}
		p, err := pp.hub.PersonByID(s.person)
		if err != nil { // revoked: the session ends with the person
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		if write && (!sameSite(r) || p.Role != room.RoleMember) {
			writeErr(w, http.StatusForbidden, errors.New("참여 권한(member)이 있는 사람만 이 화면에서 할 수 있음"))
			return
		}
		if id := r.PathValue("id"); id != "" {
			if _, err := pp.hub.SpaceKind(id); err != nil {
				writeErr(w, http.StatusNotFound, err)
				return
			}
		}
		h(w, r, p)
	}
}

func (pp *people) me(w http.ResponseWriter, _ *http.Request, p room.Person) {
	writeJSON(w, 200, p)
}

func (pp *people) spaces(w http.ResponseWriter, _ *http.Request, _ room.Person) {
	out := []room.RoomSummary{}
	for _, s := range pp.hub.Rooms() {
		if s.Kind != "" && !s.Archived {
			s.Workspace = "" // a host folder path is not the person's business
			out = append(out, s)
		}
	}
	writeJSON(w, 200, out)
}

// The fields of a space a person sees. Workspace, CLI sessions, usage, roles
// and traces stay on the host.
type spaceSeat struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Enabled      bool   `json:"enabled"`
	Removed      bool   `json:"removed,omitempty"`
	Status       string `json:"status"`
	PacedUntil   int64  `json:"paced_until,omitempty"`
	BudgetHeld   string `json:"budget_held,omitempty"`
	HoldUntil    int64  `json:"hold_until,omitempty"`
	RemoteSeenAt int64  `json:"remote_seen_at,omitempty"`
}

type spaceRoom struct {
	ID                   string      `json:"id"`
	Title                string      `json:"title"`
	Kind                 string      `json:"kind"`
	Mode                 string      `json:"mode"`
	AgentsPaused         bool        `json:"agents_paused,omitempty"`
	SpeakIntervalSeconds int         `json:"speak_interval_seconds,omitempty"`
	DailySpeakLimit      int         `json:"daily_speak_limit,omitempty"`
	AutoChatMinutes      int         `json:"auto_chat_minutes,omitempty"`
	AutoChatHours        string      `json:"auto_chat_hours,omitempty"`
	Participants         []spaceSeat `json:"participants"`
}

type spaceMessage struct {
	ID     string `json:"id"`
	Seq    int64  `json:"seq"`
	TS     int64  `json:"ts"`
	Kind   string `json:"kind"`
	Author string `json:"author"`
	By     string `json:"by,omitempty"`
	Text   string `json:"text"`
	Status string `json:"status"`
	Target string `json:"target,omitempty"`
	Reason string `json:"reason,omitempty"`
	Error  string `json:"error,omitempty"`
	// Mentions are the seats a reply called; the 3D view draws who talks to whom.
	Mentions *[]string `json:"mentions,omitempty"`
}

func (pp *people) space(w http.ResponseWriter, r *http.Request, _ room.Person) {
	b, err := pp.hub.SnapshotPageJSON(r.PathValue("id"), 0, spaceMessages)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	var snap room.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		writeErr(w, 500, err)
		return
	}
	rm := snap.Room
	out := spaceRoom{ID: rm.ID, Title: rm.Title, Kind: rm.Kind, Mode: rm.Mode, AgentsPaused: rm.AgentsPaused, SpeakIntervalSeconds: rm.SpeakIntervalSeconds, DailySpeakLimit: rm.DailySpeakLimit, AutoChatMinutes: rm.AutoChatMinutes, AutoChatHours: rm.AutoChatHours, Participants: []spaceSeat{}}
	for _, p := range rm.Participants {
		out.Participants = append(out.Participants, spaceSeat{ID: p.ID, Label: p.Label, Provider: p.Provider, Model: p.Model, Enabled: p.Enabled, Removed: p.Removed, Status: p.Status, PacedUntil: p.PacedUntil, BudgetHeld: p.BudgetHeld, HoldUntil: p.HoldUntil, RemoteSeenAt: p.RemoteSeenAt})
	}
	msgs := []spaceMessage{}
	for _, m := range snap.Messages {
		if m.UserOnly {
			continue // usage warnings with token counts are for the host
		}
		msgs = append(msgs, spaceMessage{ID: m.ID, Seq: m.Seq, TS: m.TS, Kind: m.Kind, Author: m.Author, By: m.By, Text: m.Text, Status: m.Status, Target: m.Target, Reason: m.Reason, Error: m.Error, Mentions: m.Mentions})
	}
	decisions := snap.Decisions
	if decisions == nil {
		decisions = []room.Decision{}
	}
	writeJSON(w, 200, map[string]any{"room": out, "messages": msgs, "decisions": decisions})
}

// stream tells the page that the space changed; the page reads the space
// again. Room events themselves are not forwarded: they carry work traces.
func (pp *people) stream(w http.ResponseWriter, r *http.Request, p room.Person) {
	events, unsub, err := pp.hub.Subscribe(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	defer unsub()
	signals := make(chan []byte, 1)
	go func() {
		defer close(signals)
		var due <-chan time.Time
		for {
			select {
			case <-r.Context().Done():
				return
			case _, ok := <-events:
				if !ok {
					return
				}
				if due == nil {
					due = time.After(streamCoalesce)
				}
			case <-due:
				due = nil
				if _, err := pp.hub.PersonByID(p.ID); err != nil {
					return // revoked while watching: the open stream ends too
				}
				select {
				case signals <- []byte(`{"type":"changed"}`):
				default:
				}
			}
		}
	}()
	serveSSE(w, r, signals)
}

func (pp *people) message(w http.ResponseWriter, r *http.Request, p room.Person) {
	var body struct {
		Text   string `json:"text"`
		Target string `json:"target"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, err := pp.hub.PostAs(r.PathValue("id"), body.Text, body.Target, p.Name)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, map[string]any{"seq": m.Seq})
}

func (pp *people) task(w http.ResponseWriter, r *http.Request, p room.Person) {
	var body struct {
		To   string `json:"to"`
		Text string `json:"text"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	m, req, err := pp.hub.AssignTaskAs(r.PathValue("id"), body.To, body.Text, p.Name)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 201, map[string]any{"seq": m.Seq, "request": req.ID})
}

// decide confirms (approves) or drops a proposed card; a person cannot edit
// the card's text or touch a card already decided.
func (pp *people) decide(w http.ResponseWriter, r *http.Request, p room.Person) {
	var body struct {
		Status string `json:"status"`
	}
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, err)
		return
	}
	if body.Status != "active" && body.Status != "dropped" {
		writeErr(w, 400, fmt.Errorf("status는 active(승인) 또는 dropped(반려)여야 함"))
		return
	}
	list, err := pp.hub.Decisions(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	for _, d := range list {
		if d.ID == r.PathValue("did") && d.Status != "proposed" {
			writeErr(w, http.StatusConflict, fmt.Errorf("이미 정해진 카드: %s", d.Status))
			return
		}
	}
	d, err := pp.hub.UpdateDecision(r.PathValue("id"), r.PathValue("did"), room.DecisionInput{Status: &body.Status, DecidedBy: &p.Name})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, d)
}
