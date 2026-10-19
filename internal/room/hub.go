package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"roundtable/internal/agent"
)

const (
	MaxParticipants = 32
	defaultMaxChain = 4
	defaultRounds   = 3
	turnTimeout     = 45 * time.Minute
)

// Hub owns every room, runs agent turns and fans events out to SSE clients.
type Hub struct {
	mu              sync.Mutex
	closing         bool
	turnWG          sync.WaitGroup
	store           *Store
	reg             *agent.Registry
	scratch         string
	rooms           map[string]*roomState
	limits          map[string]agent.Limits
	usageStates     map[string]UsageSource
	usageFlights    map[string]chan struct{}
	usageCancels    map[string]context.CancelFunc
	limitsVersion   uint64
	limitsSaving    bool
	limitsDone      chan struct{}
	limitsSaveError error
	nativeSessions  map[string]bool
	mcp             *agent.MCPServer // read-only tool server attached to read/write runs
	workRoot        string           // <data>/worktrees: part and integration worktrees live outside user repos
	Log             *log.Logger
}

type roomState struct {
	room     *Room
	msgs     []*Message
	queue    []*Turn
	running  map[string]*running
	subs     map[chan []byte]struct{}
	disc     *discussion
	deleted  bool // late invocation callbacks must not recreate a deleted room
	stopGen  int  // prevents a stopped turn from scheduling follow-up work
	resetGen int  // bumped by ResetSessions so a finishing turn does not resurrect the old session

	workBusy map[string]bool // plan creation or part submit checks running git outside h.mu
}

type running struct {
	turn             *Turn
	msg              *Message
	cancel           context.CancelFunc
	steer            chan string
	live             []*Message // accepted steers belonging to this invocation only
	stopGen          int
	discardFollowups bool // participant stop must not replay old steers after re-enable
}

type discussion struct {
	Total    int
	Round    int
	Expected int
	Done     int
	Signals  map[string]string
}

func NewHub(store *Store, reg *agent.Registry, scratch string, logger *log.Logger) (*Hub, error) {
	absoluteScratch, err := filepath.Abs(scratch)
	if err != nil {
		return nil, fmt.Errorf("작업 임시 폴더: %w", err)
	}
	workRoot, err := filepath.Abs(filepath.Join(store.Root(), "worktrees"))
	if err != nil {
		return nil, fmt.Errorf("worktree 폴더: %w", err)
	}
	h := &Hub{store: store, reg: reg, scratch: absoluteScratch, rooms: map[string]*roomState{}, limits: map[string]agent.Limits{}, nativeSessions: map[string]bool{}, workRoot: workRoot, Log: logger}
	rooms, msgs, err := store.LoadRooms()
	if err != nil {
		return nil, err
	}
	if err := store.LoadLimits(&h.limits); err != nil {
		return nil, fmt.Errorf("limits.json: %w", err)
	}
	if h.limits == nil {
		h.limits = map[string]agent.Limits{}
	}
	for _, r := range rooms {
		if r.Workspace != "" {
			r.Workspace, err = filepath.Abs(r.Workspace)
			if err != nil {
				return nil, fmt.Errorf("방 %s 작업 폴더: %w", r.ID, err)
			}
		}
		rs := &roomState{room: r, msgs: msgs[r.ID], running: map[string]*running{}, subs: map[chan []byte]struct{}{}, workBusy: map[string]bool{}}
		for _, m := range rs.msgs {
			if m.Kind != "agent" {
				continue
			}
			if p := r.participant(m.Author); p != nil && m.Provider == "" {
				m.Provider = p.Provider
			}
		}
		rs.refreshParticipantStates()
		if r.Work != nil {
			if err := h.recoverWork(rs); err != nil {
				return nil, err
			}
		}
		h.rooms[r.ID] = rs
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rs := range h.rooms {
		h.advanceQueue(rs)
		if rs.room.Work != nil {
			if err := h.store.SaveRoom(rs.room); err != nil {
				return nil, err
			}
		}
	}
	return h, nil
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nowMs() int64 { return time.Now().UnixMilli() }

// ---- room management -------------------------------------------------------

// RoomSpec is the editable part of a room.
type RoomSpec struct {
	Title            string            `json:"title"`
	Workspace        string            `json:"workspace"`
	Mode             string            `json:"mode"`
	Note             string            `json:"note"`
	Participants     []string          `json:"participants"`
	ParticipantSpecs []ParticipantSpec `json:"participant_specs"`
	MaxChain         int               `json:"max_chain"`
	DiscussionRounds int               `json:"discussion_rounds"`
	Persona          bool              `json:"persona"`
}

func (h *Hub) CreateRoom(spec RoomSpec) (*Room, error) {
	if strings.TrimSpace(spec.Title) == "" {
		return nil, fmt.Errorf("방 이름이 비어 있음")
	}
	if spec.Mode == "" {
		spec.Mode = agent.ModeRead
	}
	if err := validateMode(spec.Mode, spec.Workspace); err != nil {
		return nil, err
	}
	if spec.Workspace != "" {
		absolute, err := filepath.Abs(spec.Workspace)
		if err != nil {
			return nil, fmt.Errorf("작업 폴더: %w", err)
		}
		spec.Workspace = absolute
	}
	r := &Room{ID: newID(), Title: spec.Title, CreatedAt: nowMs(), UpdatedAt: nowMs(), Workspace: spec.Workspace, Mode: spec.Mode, Note: spec.Note, MaxChain: spec.MaxChain, DiscussionRounds: spec.DiscussionRounds, Persona: spec.Persona}
	if r.MaxChain <= 0 {
		r.MaxChain = defaultMaxChain
	}
	if r.DiscussionRounds <= 0 {
		r.DiscussionRounds = defaultRounds
	}
	if len(spec.Participants) > 0 && spec.ParticipantSpecs != nil {
		return nil, fmt.Errorf("participants와 participant_specs는 함께 지정할 수 없음")
	}
	if spec.ParticipantSpecs != nil {
		if len(spec.ParticipantSpecs) == 0 {
			return nil, fmt.Errorf("참가자가 1명 이상 필요함")
		}
		for _, ps := range spec.ParticipantSpecs {
			prepared, err := h.prepareParticipant(ps)
			if err != nil {
				return nil, err
			}
			seats, err := newParticipants(r, prepared)
			if err != nil {
				return nil, err
			}
			r.Participants = append(r.Participants, seats...)
		}
	} else {
		ids := spec.Participants
		if len(ids) == 0 {
			return nil, fmt.Errorf("참가자를 선택해")
		}
		for _, id := range ids {
			prepared, err := h.prepareParticipant(ParticipantSpec{Provider: id})
			if err != nil {
				return nil, err
			}
			seats, err := newParticipants(r, prepared)
			if err != nil {
				return nil, err
			}
			// Preserve the legacy create API's provider-shaped IDs for its first seats.
			if r.participant(id) == nil {
				seats[0].ID = id
			}
			r.Participants = append(r.Participants, seats...)
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, fmt.Errorf("서버가 종료 중이야")
	}
	if err := h.store.SaveRoom(r); err != nil {
		return nil, err
	}
	h.rooms[r.ID] = &roomState{room: r, running: map[string]*running{}, subs: map[chan []byte]struct{}{}, workBusy: map[string]bool{}}
	return r, nil
}

// RoomSummary is the sidebar view.
type RoomSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	UpdatedAt int64  `json:"updated_at"`
	Mode      string `json:"mode"`
	Messages  int    `json:"messages"`
	Active    bool   `json:"active"`
}

func (h *Hub) Rooms() []RoomSummary {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []RoomSummary
	for _, rs := range h.rooms {
		out = append(out, RoomSummary{ID: rs.room.ID, Title: rs.room.Title, UpdatedAt: rs.room.UpdatedAt, Mode: rs.room.Mode, Messages: len(rs.msgs), Active: len(rs.running) > 0 || len(rs.queue) > 0})
	}
	sortSummaries(out)
	return out
}

func sortSummaries(s []RoomSummary) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].UpdatedAt > s[j-1].UpdatedAt; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Snapshot is everything the page needs to render a room.
type Snapshot struct {
	Room       *Room                   `json:"room"`
	Messages   []*Message              `json:"messages"`
	Queue      []*Turn                 `json:"queue"`
	Discussion *DiscussionView         `json:"discussion"`
	Limits     map[string]agent.Limits `json:"limits"`
	History    HistoryPage             `json:"history"`
	Work       *WorkPlan               `json:"work"`
}

type DiscussionView struct {
	Total   int               `json:"total"`
	Round   int               `json:"round"`
	Signals map[string]string `json:"signals"`
}

func (h *Hub) Snapshot(id string) (*Snapshot, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	return h.snapshotLocked(rs, 0, len(rs.msgs)), nil
}

// SnapshotJSON returns the initial bounded history page.
func (h *Hub) SnapshotJSON(id string) ([]byte, error) {
	return h.SnapshotPageJSON(id, 0, DefaultHistoryLimit)
}

func (h *Hub) limitsCopy() map[string]agent.Limits {
	out := map[string]agent.Limits{}
	for k, v := range h.limits {
		v.Windows = slices.Clone(v.Windows)
		out[k] = v
	}
	return out
}

func (h *Hub) Limits() map[string]agent.Limits {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.limitsCopy()
}

func (rs *roomState) queueView() []*Turn {
	out := make([]*Turn, 0, len(rs.queue)+len(rs.running))
	for _, r := range rs.running {
		out = append(out, r.turn)
	}
	out = append(out, rs.queue...)
	return out
}

func (rs *roomState) discView() *DiscussionView {
	if rs.disc == nil {
		return nil
	}
	return &DiscussionView{Total: rs.disc.Total, Round: rs.disc.Round, Signals: rs.disc.Signals}
}

// ParticipantPatch updates one seat.
type ParticipantPatch struct {
	ID      string  `json:"id"`
	Label   *string `json:"label,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	Model   *string `json:"model,omitempty"`
	Effort  *string `json:"effort,omitempty"`
}

// RoomPatch is a partial update.
type RoomPatch struct {
	Title            *string            `json:"title,omitempty"`
	Note             *string            `json:"note,omitempty"`
	Mode             *string            `json:"mode,omitempty"`
	Workspace        *string            `json:"workspace,omitempty"`
	MaxChain         *int               `json:"max_chain,omitempty"`
	DiscussionRounds *int               `json:"discussion_rounds,omitempty"`
	Persona          *bool              `json:"persona,omitempty"`
	Participants     []ParticipantPatch `json:"participants,omitempty"`
}

func (h *Hub) UpdateRoom(id string, patch RoomPatch) ([]byte, error) {
	choices := h.patchModelChoices(id, patch.Participants)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	// Validate and persist a candidate before changing any live room state.
	next := *rs.room
	next.Participants = make([]*Participant, len(rs.room.Participants))
	for i, p := range rs.room.Participants {
		cp := *p
		next.Participants[i] = &cp
	}
	if patch.Title != nil {
		if strings.TrimSpace(*patch.Title) == "" {
			return nil, fmt.Errorf("방 이름이 비어 있음")
		}
		next.Title = *patch.Title
	}
	if patch.Note != nil {
		next.Note = *patch.Note
	}
	if patch.Mode != nil {
		next.Mode = *patch.Mode
	}
	if patch.Workspace != nil {
		next.Workspace = *patch.Workspace
		if next.Workspace != "" {
			absolute, err := filepath.Abs(next.Workspace)
			if err != nil {
				return nil, fmt.Errorf("작업 폴더: %w", err)
			}
			next.Workspace = absolute
		}
	}
	if err := validateMode(next.Mode, next.Workspace); err != nil {
		return nil, err
	}
	contextChanged := next.Mode != rs.room.Mode || next.Workspace != rs.room.Workspace
	if contextChanged && len(rs.running) > 0 {
		return nil, fmt.Errorf("실행 중에는 모드와 작업 폴더를 변경할 수 없음")
	}
	if contextChanged && (rs.room.Work != nil || rs.workBusy[busyPlan]) {
		return nil, fmt.Errorf("작업 계획이 있는 방은 모드와 작업 폴더를 변경할 수 없음")
	}
	if patch.MaxChain != nil {
		if *patch.MaxChain < 1 {
			return nil, fmt.Errorf("연쇄 한도는 1 이상이어야 함")
		}
		next.MaxChain = *patch.MaxChain
	}
	if patch.DiscussionRounds != nil {
		if *patch.DiscussionRounds < 1 {
			return nil, fmt.Errorf("토론 라운드는 1 이상이어야 함")
		}
		next.DiscussionRounds = *patch.DiscussionRounds
	}
	if patch.Persona != nil {
		next.Persona = *patch.Persona
	}
	patched := map[string]bool{}
	for _, pp := range patch.Participants {
		p := next.participant(pp.ID)
		if p == nil || p.Removed {
			return nil, fmt.Errorf("참가자 없음: %s", pp.ID)
		}
		if patched[pp.ID] {
			return nil, fmt.Errorf("중복 참가자 변경: %s", pp.ID)
		}
		patched[pp.ID] = true
		if pp.Label != nil {
			if strings.TrimSpace(*pp.Label) == "" {
				return nil, fmt.Errorf("참가자 이름이 비어 있음")
			}
			p.Label = strings.TrimSpace(*pp.Label)
		}
		if pp.Enabled != nil {
			p.Enabled = *pp.Enabled
		}
		if pp.Model != nil {
			p.Model = *pp.Model
		}
		if pp.Effort != nil {
			p.Effort = *pp.Effort
		}
		original := rs.room.participant(p.ID)
		if p.Model != original.Model || p.Effort != original.Effort {
			if rs.runningFor(p.ID) != nil {
				return nil, fmt.Errorf("%s 실행이 끝난 뒤 모델과 추론 수준을 변경해야 함", p.Label)
			}
			prov := h.reg.Get(p.Provider)
			if prov == nil {
				return nil, fmt.Errorf("알 수 없는 공급자: %s", p.Provider)
			}
			if err := validateModel(p.Provider, p.Model, p.Effort, choices[p.Provider]); err != nil {
				return nil, err
			}
			// Each CLI takes the effort per invocation, so an effort change resumes
			// the same session instead of re-sending the room history.
			if p.Model != original.Model {
				p.SessionID, p.LastSeen, p.ContextTokens = "", 0, 0
				p.Unseen = nil
				p.ContextWindow = prov.ContextWindow(p.Model)
			}
		}
	}
	if contextChanged {
		for _, p := range next.Participants {
			p.SessionID, p.LastSeen, p.ContextTokens = "", 0, 0
			p.Unseen = nil
		}
	}
	for _, p := range next.Participants {
		if p.Enabled && h.seatNativeOpen(rs, p) {
			return nil, fmt.Errorf("%s의 CLI 창을 닫은 뒤 다시 켜야 함", p.Label)
		}
	}
	next.UpdatedAt = nowMs()
	if err := h.store.SaveRoom(&next); err != nil {
		return nil, err
	}
	// Preserve participant pointers held by running invocations.
	for i, p := range rs.room.Participants {
		*p = *next.Participants[i]
		next.Participants[i] = p
	}
	*rs.room = next
	for _, pp := range patch.Participants {
		p := rs.room.participant(pp.ID)
		if !p.Enabled {
			rs.dropQueuedFor(h, p.ID)
		}
	}
	rs.broadcast("room", rs.room)
	h.pump(rs)
	return json.Marshal(rs.room)
}

// dropQueuedFor removes a disabled participant's waiting turns; a discussion counts them as PASS.
func (rs *roomState) dropQueuedFor(h *Hub, agentID string) {
	var dropped []*Turn
	kept := rs.queue[:0]
	for _, t := range rs.queue {
		if t.Agent != agentID {
			kept = append(kept, t)
			continue
		}
		dropped = append(dropped, t)
	}
	rs.queue = kept
	for _, t := range dropped {
		if t.Reason == "discussion" {
			h.discussionStep(rs, t.Agent, "PASS", "cancelled")
		}
		if t.Reason == "review" {
			h.reviewTurnLost(rs, t, "리뷰 턴을 맡은 좌석이 비활성화됨")
		}
	}
	rs.refreshParticipantStates()
	rs.broadcast("queue", rs.queueView())
}

// ResetSessions forgets the CLI sessions so every agent starts with a fresh context.
func (h *Hub) ResetSessions(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		return fmt.Errorf("방 없음: %s", id)
	}
	for _, p := range rs.room.Participants {
		p.SessionID, p.LastSeen, p.ContextTokens = "", 0, 0
		p.Unseen = nil
	}
	rs.resetGen++
	rs.addSystem("세션을 초기화했어. 다음 턴부터 각 에이전트는 대화 전체를 새로 읽어.")
	rs.broadcast("room", rs.room)
	return h.store.SaveRoom(rs.room)
}

func (h *Hub) DeleteRoom(id string) error {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("방 없음: %s", id)
	}
	rs.deleted = true
	rs.stopLocked("방 삭제")
	delete(h.rooms, id)
	for ch := range rs.subs {
		close(ch)
	}
	rs.subs = map[chan []byte]struct{}{} // late broadcasts from a finishing turn find nobody
	h.mu.Unlock()
	return h.store.DeleteRoom(id)
}

// ---- events ----------------------------------------------------------------

func (h *Hub) Subscribe(id string) (<-chan []byte, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		return nil, nil, fmt.Errorf("방 없음: %s", id)
	}
	ch := make(chan []byte, 512)
	rs.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, still := rs.subs[ch]; still {
			delete(rs.subs, ch)
			close(ch)
		}
	}, nil
}

func (rs *roomState) broadcast(typ string, payload any) {
	if len(rs.subs) == 0 {
		return
	}
	b, err := json.Marshal(map[string]any{"type": typ, "data": payload})
	if err != nil {
		return
	}
	for ch := range rs.subs {
		select {
		case ch <- b:
		default:
			// A gap must force reconnection; silently dropping a final message
			// leaves the client permanently stale while the connection stays open.
			delete(rs.subs, ch)
			close(ch)
		}
	}
}

func (h *Hub) ReadTrace(id, msgID string) ([]agent.Event, error) {
	h.mu.Lock()
	rs, ok := h.rooms[id]
	if !ok {
		h.mu.Unlock()
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	found := slices.ContainsFunc(rs.msgs, func(message *Message) bool { return message.ID == msgID })
	h.mu.Unlock()
	if !found {
		return nil, fmt.Errorf("메시지 없음: %s", msgID)
	}
	return h.store.ReadTrace(id, msgID)
}

// Trace keeps the original in-process API; storage failures remain visible as
// error events. HTTP callers use ReadTrace to return a failed status instead.
func (h *Hub) Trace(id, msgID string) []agent.Event {
	events, err := h.ReadTrace(id, msgID)
	if err != nil {
		return []agent.Event{{Kind: agent.KindError, Text: err.Error()}}
	}
	return events
}

// ---- messages and routing --------------------------------------------------

func (rs *roomState) nextMessage(kind, author, text string) *Message {
	rs.room.Seq++
	rs.room.UpdatedAt = nowMs()
	m := &Message{ID: newID(), Seq: rs.room.Seq, TS: nowMs(), Kind: kind, Author: author, Text: text, Status: "done"}
	rs.msgs = append(rs.msgs, m)
	return m
}

func (rs *roomState) addSystem(text string) *Message {
	m := rs.nextMessage("system", "system", text)
	rs.broadcast("message", m)
	return m
}

func (h *Hub) enabledIDs(rs *roomState) []string {
	var ids []string
	for _, p := range rs.room.enabled() {
		ids = append(ids, p.ID)
	}
	return ids
}

// PostUser adds a user message and routes it: mentions, or everyone when none.
// target addresses one agent; if that agent is running and can steer, the text
// is injected live, otherwise a priority turn is queued.
func (h *Hub) PostUser(id, text, target string) (*Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("빈 메시지")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	if target != "" {
		p := rs.room.participant(target)
		if p == nil || p.Removed || !p.Enabled {
			return nil, fmt.Errorf("활성 대상 없음: %s", target)
		}
	}
	m := rs.nextMessage("user", "user", text)
	if target != "" {
		m.Target = target
		if run := rs.runningFor(target); run != nil && run.steer != nil {
			select {
			case run.steer <- text:
				m.Live = true
				m.Part = run.turn.Part
				run.live = append(run.live, m)
			default:
			}
		}
		rs.broadcast("message", m)
		_ = h.store.AppendMessage(id, m)
		_ = h.store.SaveRoom(rs.room)
		if !m.Live {
			rs.enqueueFront(&Turn{ID: newID(), Agent: target, Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
			h.pump(rs)
		}
		mc := *m
		return &mc, nil
	}
	rs.broadcast("message", m)
	_ = h.store.AppendMessage(id, m)
	_ = h.store.SaveRoom(rs.room)
	ids := h.enabledIDs(rs)
	mentioned := ParseParticipantMentions(text, rs.room.Participants)
	var targets []string
	for _, id := range mentioned {
		if slices.Contains(ids, id) {
			targets = append(targets, id)
		}
	}
	reason := "mention"
	switch {
	case len(mentioned) > 0 && len(targets) == 0:
		rs.addSystem(fmt.Sprintf("@%s 는 비활성 또는 삭제 상태라 아무도 답하지 않아. 다른 활성 참가자를 불러.", strings.Join(mentioned, ", @")))
		mc := *m
		return &mc, nil
	case len(mentioned) == 0:
		targets, reason = ids, "broadcast"
	}
	for _, t := range targets {
		rs.enqueue(&Turn{ID: newID(), Agent: t, Reason: reason, Group: m.ID, Cap: m.Seq, parallel: len(targets) > 1, Created: nowMs(), Status: "queued"})
	}
	h.pump(rs)
	mc := *m
	return &mc, nil
}

func (rs *roomState) runningFor(agentID string) *running {
	for _, r := range rs.running {
		if r.turn.Agent == agentID {
			return r
		}
	}
	return nil
}

func (rs *roomState) enqueue(t *Turn) {
	rs.queue = append(rs.queue, t)
	rs.broadcast("queue", rs.queueView())
}

func (rs *roomState) enqueueFront(t *Turn) {
	// Targeted messages keep priority over ordinary waiting turns, but earlier
	// targeted messages for this participant must remain ahead of this one.
	t.priority = true
	at := 0
	for i, queued := range rs.queue {
		if queued.Agent == t.Agent && queued.priority {
			at = i + 1
		}
	}
	rs.queue = slices.Insert(rs.queue, at, t)
	rs.broadcast("queue", rs.queueView())
}

// StartDiscussion runs up to `rounds` sequential rounds over every enabled agent.
func (h *Hub) StartDiscussion(id string, rounds int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[id]
	if !ok {
		return fmt.Errorf("방 없음: %s", id)
	}
	if rs.disc != nil {
		return fmt.Errorf("이미 토론 중")
	}
	if rounds <= 0 {
		rounds = rs.room.DiscussionRounds
	}
	ids := h.enabledIDs(rs)
	if len(ids) < 2 {
		return fmt.Errorf("토론에는 활성 참가자가 2명 이상 필요함")
	}
	rs.disc = &discussion{Total: rounds, Round: 1, Signals: map[string]string{}}
	rs.addSystem(fmt.Sprintf("토론 시작 · 최대 %d라운드 · 참가 %s", rounds, strings.Join(ids, ", ")))
	rs.enqueueRound(ids)
	rs.broadcast("discussion", rs.discView())
	h.pump(rs)
	return nil
}

func (rs *roomState) enqueueRound(ids []string) {
	rs.disc.Expected, rs.disc.Done = len(ids), 0
	rs.disc.Signals = map[string]string{}
	for _, id := range ids {
		rs.enqueue(&Turn{ID: newID(), Agent: id, Reason: "discussion", Round: rs.disc.Round, Group: newID(), Created: nowMs(), Status: "queued"})
	}
}

// Stop cancels running turns and empties the queue.
func (h *Hub) Stop(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return fmt.Errorf("방 없음: %s", id)
	}
	for _, t := range rs.queue {
		if t.Reason == "review" {
			h.reviewTurnLost(rs, t, "사용자가 중지해 리뷰 턴이 취소됨")
		}
	}
	rs.stopLocked("사용자가 중지함")
	return nil
}

func (rs *roomState) stopLocked(why string) {
	dropped := len(rs.queue)
	rs.queue = nil
	rs.stopGen++
	for _, r := range rs.running {
		r.cancel()
	}
	rs.disc = nil
	rs.refreshParticipantStates()
	if dropped > 0 || len(rs.running) > 0 {
		rs.addSystem(fmt.Sprintf("중지: %s (대기 %d건 취소, 실행 중 %d건 중단)", why, dropped, len(rs.running)))
	}
	rs.broadcast("queue", rs.queueView())
	rs.broadcast("discussion", nil)
}

// CancelTurn removes one queued turn or cancels a running one.
func (h *Hub) CancelTurn(id, turnID string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return fmt.Errorf("방 없음: %s", id)
	}
	if r, ok := rs.running[turnID]; ok {
		r.cancel()
		rs.refreshParticipantStates()
		return nil
	}
	for i, t := range rs.queue {
		if t.ID == turnID {
			rs.queue = append(rs.queue[:i], rs.queue[i+1:]...)
			if t.Reason == "discussion" {
				h.discussionStep(rs, t.Agent, "PASS", "cancelled")
			}
			if t.Reason == "review" {
				h.reviewTurnLost(rs, t, "사용자가 리뷰 턴을 취소함")
			}
			rs.broadcast("queue", rs.queueView())
			h.pump(rs)
			return nil
		}
	}
	return fmt.Errorf("턴 없음: %s", turnID)
}

// ---- turn execution --------------------------------------------------------

// refreshParticipantStates is the only runtime writer of participant Status.
// Caller holds h.mu; running reservations take precedence over waiting turns.
func (rs *roomState) refreshParticipantStates() {
	states := map[string]string{}
	for _, t := range rs.queue {
		states[t.Agent] = "queued"
	}
	for _, r := range rs.running {
		states[r.turn.Agent] = "running"
	}
	for _, p := range rs.room.Participants {
		status := states[p.ID]
		if status == "" {
			status = "idle"
		}
		statusText := ""
		if status == "running" {
			statusText = p.StatusText
			if p.Status != "running" || rs.runningFor(p.ID).msg.Trace == 0 {
				statusText = "생각 중"
			}
		}
		if p.Status == status && p.StatusText == statusText {
			continue
		}
		p.Status, p.StatusText = status, statusText
		rs.broadcast("participant", p)
	}
}

// pump reserves runnable participants and builds only the turns that can start.
// Broadcast members share a cap, not a copy of mutable streaming messages.
// Caller holds h.mu throughout prompt construction and participant reservation.
func (h *Hub) pump(rs *roomState) {
	if h.closing {
		rs.refreshParticipantStates()
		return
	}
	discussionBlocked := false
	for _, run := range rs.running {
		if run.turn.Reason == "discussion" && run.stopGen == rs.stopGen {
			discussionBlocked = true
		}
	}
	skipped := false
	for i := 0; i < len(rs.queue); {
		t := rs.queue[i]
		p := rs.room.participant(t.Agent)
		if p == nil || !p.Enabled || p.Removed || h.reg.Get(p.Provider) == nil || h.seatNativeOpen(rs, p) {
			rs.queue = slices.Delete(rs.queue, i, i+1)
			if t.Reason == "discussion" {
				h.discussionStep(rs, t.Agent, "PASS", "cancelled")
			}
			if t.Reason == "review" {
				h.reviewTurnLost(rs, t, "리뷰 턴을 맡은 좌석을 실행할 수 없음")
			}
			continue
		}
		if part := rs.seatPart(p.ID); t.Reason != "review" && part != nil && (rs.workBusy[part.ID] || (part.State != "working" && part.State != "fixing")) {
			i++
			continue
		}
		// Ordinary priority turns must not block a later discussion turn for
		// the same participant, or the discussion would never finish.
		if rs.disc != nil && t.Reason != "discussion" {
			i++
			continue
		}
		if rs.runningFor(t.Agent) != nil || (t.Reason == "discussion" && discussionBlocked) {
			if t.Reason == "discussion" {
				discussionBlocked = true
			}
			i++
			continue
		}
		rs.queue = slices.Delete(rs.queue, i, i+1)
		pr := h.buildTurn(rs, t)
		if pr == nil {
			skipped = true
			continue
		}
		h.prepareTurn(rs, pr)
		if t.Reason == "discussion" {
			discussionBlocked = true
		}
		h.turnWG.Add(1)
		go h.runTurn(rs, pr)
	}
	rs.refreshParticipantStates()
	rs.broadcast("queue", rs.queueView())
	if skipped {
		_ = h.store.SaveRoom(rs.room)
	}
}

// prepared contains a prompt rendered from live messages up to the turn's cap.
type prepared struct {
	t         *Turn
	p         *Participant
	seen      int64
	unseen    []int64
	brief     string
	stopGen   int
	resetGen  int
	prov      agent.Provider
	msg       *Message
	opts      agent.RunOptions
	ctx       context.Context
	cancel    context.CancelFunc
	part      *Part // the turn runs in this part's worktree (D9, review)
	reviewer  bool  // fresh reviewer session that must not be kept (D10)
	bootstrap bool  // first part turn contains plan context only
}

// buildTurn renders a prompt without reserving a participant. Caller holds h.mu.
func (h *Hub) buildTurn(rs *roomState, t *Turn) *prepared {
	p := rs.room.participant(t.Agent)
	if p == nil || !p.Enabled || p.Removed {
		return nil
	}
	prov := h.reg.Get(p.Provider)
	if prov == nil {
		return nil
	}
	if t.Reason == "review" {
		return h.buildReviewTurn(rs, t, p, prov)
	}
	if part := rs.activePart(p.ID); part != nil {
		return h.buildPartTurn(rs, t, p, prov, part)
	}
	if t.Reason == "part" {
		return nil // the part left working/fixing before its turn started
	}
	first := p.SessionID == ""
	cap := t.Cap
	if t.Reason == "discussion" {
		cap = rs.room.Seq
	}
	hist, seen, unseen := rs.deltaFor(p, first, cap)
	if len(hist) == 0 && t.Reason != "discussion" {
		// No CLI call is needed to acknowledge own/filtered messages. Keep
		// unfinished replies as holes for a later cap to deliver exactly once.
		p.LastSeen, p.Unseen = seen, unseen
		return nil
	}
	brief := briefKey(standingBrief(rs.room, p))
	tc := turnContext{Reason: t.Reason, Parallel: t.parallel, Round: t.Round, FirstTurn: first, Briefed: !first && p.Brief == brief}
	if rs.disc != nil && t.Reason == "discussion" {
		tc.TotalRounds = rs.disc.Total
	}
	prompt := BuildPrompt(rs.room, p, hist, tc)
	opts := agent.RunOptions{Prompt: prompt, SessionID: p.SessionID, Model: p.Model, Effort: p.Effort, Mode: rs.room.Mode, Workspace: rs.room.Workspace, ScratchDir: h.scratch, Timeout: turnTimeout, MCP: h.mcp}
	return &prepared{t: t, p: p, seen: seen, unseen: unseen, brief: brief, stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts}
}

// buildPartTurn runs every turn of a seat with a working/fixing part in the
// part session and worktree (D9). The first part turn gets only plan context.
func (h *Hub) buildPartTurn(rs *roomState, t *Turn, p *Participant, prov agent.Provider, part *Part) *prepared {
	t.Part = part.ID
	seen, unseen := part.LastSeen, slices.Clone(part.Unseen)
	bootstrap := part.SessionID == ""
	var prompt string
	switch {
	case part.SessionID == "":
		prompt = BuildPartPrompt(rs.room.Work, part)
		if t.prompt != "" {
			prompt += "\n\n" + t.prompt
		}
	case t.prompt != "":
		prompt = t.prompt
	default:
		cap := t.Cap
		if t.Reason == "discussion" {
			cap = rs.room.Seq
		}
		var hist []*Message
		cursor := *p
		cursor.LastSeen, cursor.Unseen = part.LastSeen, part.Unseen
		hist, seen, unseen = rs.deltaForContext(&cursor, false, cap, part.ID)
		if len(hist) == 0 && t.Reason != "discussion" {
			part.LastSeen, part.Unseen = seen, unseen
			return nil
		}
		tc := turnContext{Reason: t.Reason, Parallel: t.parallel, Round: t.Round, Part: part}
		if rs.disc != nil && t.Reason == "discussion" {
			tc.TotalRounds = rs.disc.Total
		}
		view := *rs.room
		view.Workspace = part.Worktree
		prompt = BuildPrompt(&view, p, hist, tc)
	}
	opts := agent.RunOptions{Prompt: prompt, SessionID: part.SessionID, Model: p.Model, Effort: p.Effort, Mode: agent.ModeWrite, Workspace: part.Worktree, ScratchDir: h.scratch, Timeout: turnTimeout, MCP: h.mcp}
	return &prepared{t: t, p: p, seen: seen, unseen: unseen, stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts, part: part, bootstrap: bootstrap}
}

// buildReviewTurn runs a ReviewState step: the reviewer in a fresh read-only
// session (D10), the author in the part session. Room history is never added.
func (h *Hub) buildReviewTurn(rs *roomState, t *Turn, p *Participant, prov agent.Provider) *prepared {
	part := rs.room.Work.part(t.Part)
	if part.State != "reviewing" {
		return nil
	}
	reviewer := p.ID == part.Reviewer
	opts := agent.RunOptions{Prompt: t.prompt, SessionID: part.SessionID, Model: p.Model, Effort: p.Effort, Mode: agent.ModeWrite, Workspace: part.Worktree, ScratchDir: h.scratch, Timeout: turnTimeout, MCP: h.mcp}
	if reviewer {
		opts.SessionID, opts.Mode = "", agent.ModeRead
	}
	return &prepared{t: t, p: p, seen: p.LastSeen, unseen: slices.Clone(p.Unseen), stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts, part: part, reviewer: reviewer}
}

// keepSession stores a CLI session id in the slot the turn ran in. Caller holds h.mu.
func (h *Hub) keepSession(rs *roomState, pr *prepared, id string) {
	switch {
	case pr.reviewer:
	case pr.part != nil:
		pr.part.SessionID = id
	case pr.resetGen == rs.resetGen:
		pr.p.SessionID = id
	}
}

// prepareTurn reserves one participant and opens its reply. Caller holds h.mu.
func (h *Hub) prepareTurn(rs *roomState, pr *prepared) {
	t, p, prov := pr.t, pr.p, pr.prov
	msg := rs.nextMessage("agent", p.ID, "")
	msg.Status, msg.TurnID, msg.Reason, msg.Round, msg.Model = "streaming", t.ID, t.Reason, t.Round, p.Model
	msg.Part, msg.ReviewStep = t.Part, t.step
	msg.Provider, msg.SessionID, msg.Mode, msg.Effort = p.Provider, pr.opts.SessionID, pr.opts.Mode, pr.opts.Effort
	msg.SessionDirectory = pr.opts.Workspace
	if pr.opts.Mode == agent.ModeChat || msg.SessionDirectory == "" {
		msg.SessionDirectory = pr.opts.ScratchDir
	}
	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	var steer chan string
	if prov.CanSteer() && t.Reason != "review" {
		steer = make(chan string, 8)
	}
	t.Status = "running"
	rs.running[t.ID] = &running{turn: t, msg: msg, cancel: cancel, steer: steer, stopGen: rs.stopGen}
	rs.broadcast("message", msg)
	pr.opts.Steer = steer
	pr.msg, pr.ctx, pr.cancel = msg, ctx, cancel
}

func (h *Hub) runTurn(rs *roomState, pr *prepared) {
	defer h.turnWG.Done()
	t, p, prov, msg, opts, ctx, cancel := pr.t, pr.p, pr.prov, pr.msg, pr.opts, pr.ctx, pr.cancel
	started := time.Now()
	var textBuf strings.Builder
	var emitMu sync.Mutex
	var tracePersistenceError error
	emit := func(e agent.Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		h.mu.Lock()
		if rs.deleted || tracePersistenceError != nil {
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		// Event writes stay ordered within this invocation without holding up
		// every room while a large tool result is appended to disk.
		writeErr := h.store.AppendTrace(rs.room.ID, TraceLine{MsgID: msg.ID, Event: e})
		h.mu.Lock()
		defer h.mu.Unlock()
		if rs.deleted {
			return
		}
		if writeErr != nil {
			tracePersistenceError = writeErr
			cancel()
			return
		}
		msg.Trace++
		switch e.Kind {
		case agent.KindSession:
			if e.Text != "" {
				msg.SessionID = e.Text
				h.keepSession(rs, pr, e.Text)
				_ = h.store.AppendMessage(rs.room.ID, msg)
				_ = h.store.SaveRoom(rs.room)
				rs.broadcast("message", msg)
			}
		case agent.KindTextDelta:
			textBuf.WriteString(e.Text)
			msg.Text = textBuf.String()
			p.StatusText = "답변 중"
			rs.broadcast("delta", map[string]any{"id": msg.ID, "text": e.Text})
		case agent.KindText:
			if textBuf.Len() > 0 {
				textBuf.WriteString("\n\n")
			}
			textBuf.WriteString(e.Text)
			msg.Text = textBuf.String()
			rs.broadcast("delta", map[string]any{"id": msg.ID, "text": e.Text, "block": true})
		case agent.KindToolCall:
			p.StatusText = "도구: " + e.Name
		case agent.KindSubagentStart:
			p.StatusText = "서브에이전트: " + e.Name
		case agent.KindSubagentEnd:
			p.StatusText = "서브에이전트 결과 수신"
		case agent.KindThinking:
			p.StatusText = "추론 중"
		case agent.KindLimits:
			if raw, ok := e.Meta["limits"].(agent.Limits); ok {
				if h.recordLimitsLocked(raw) {
					rs.broadcast("limits", h.limitsCopy())
				}
			}
		case agent.KindUsage:
			applyUsage(p, prov, e.Meta)
		}
		rs.broadcast("trace", map[string]any{"msg_id": msg.ID, "index": msg.Trace, "event": e})
		rs.broadcast("participant", p)
	}

	var res agent.Result
	err := ctx.Err()
	if err == nil {
		res, err = prov.Run(ctx, opts, emit)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	interrupted := ctx.Err() != nil
	cancel()
	if rs.deleted {
		return
	}
	run := rs.running[t.ID]
	delete(rs.running, t.ID)
	msg.Duration = time.Since(started).Milliseconds()
	msg.Usage = res.Usage
	if res.SessionID != "" {
		msg.SessionID = res.SessionID
	}
	if res.SessionID != "" {
		h.keepSession(rs, pr, res.SessionID)
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		text = strings.TrimSpace(textBuf.String())
	}
	var signal string
	if t.Reason == "discussion" {
		text, signal = ParseSignal(text)
		if signal == "" {
			signal = "CONTINUE"
		}
		msg.Signal = signal
	}
	workDone := false
	if pr.part != nil && t.Reason != "review" && t.Reason != "discussion" {
		text, workDone = ParseWorkDone(text)
		if workDone {
			msg.Signal = workDoneSignal
		}
	}
	msg.Text = text
	switch {
	case tracePersistenceError != nil:
		msg.Status, msg.Error = "error", fmt.Sprintf("실행 기록 저장 실패: %v", tracePersistenceError)
	case interrupted:
		msg.Status, msg.Error = "cancelled", "중단됨"
	case err != nil:
		msg.Status, msg.Error = "error", err.Error()
	default:
		msg.Status = "done"
	}
	if msg.Status == "done" && t.Reason != "review" {
		if pr.part != nil {
			pr.part.LastSeen, pr.part.Unseen = pr.seen, pr.unseen
		} else if pr.resetGen == rs.resetGen {
			p.LastSeen, p.Unseen, p.Brief = pr.seen, pr.unseen, pr.brief
		}
		if pr.bootstrap && t.Reason != "part" && pr.part.SessionID != "" && pr.stopGen == rs.stopGen {
			replay := *t
			replay.ID, replay.Status = newID(), "queued"
			rs.enqueueFront(&replay)
		}
	}
	// Only this invocation's steers count against Result.Steered. Earlier
	// accepted steers may still lie above this turn's delivery boundary.
	for i, m := range run.live {
		if msg.Status != "done" || i >= res.Steered {
			m.Live = false
			if err := h.store.AppendMessage(rs.room.ID, m); err != nil {
				msg.Status, msg.Error = "error", fmt.Sprintf("미전달 조정 메시지 저장 실패: %v", err)
			}
			rs.broadcast("message", m)
			// Stop still cancels all pending work. Persist the undelivered input,
			// but never restart a turn from an older stop generation.
			if pr.stopGen == rs.stopGen && p.Enabled && !run.discardFollowups {
				rs.enqueueFront(&Turn{ID: newID(), Agent: p.ID, Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
			}
		}
	}
	// D11: malformed output is not applied; allow one persisted correction per step.
	var nextStep *ReviewStep
	var outcome string
	if t.Reason == "review" && msg.Status == "done" && pr.part.State == "reviewing" {
		var applyErr error
		nextStep, outcome, applyErr = pr.part.Review.Apply(text)
		if applyErr != nil {
			msg.Status, msg.Error = "error", "리뷰 표 형식 오류: "+applyErr.Error()
			nextStep = pr.part.Review.RetryFormat(applyErr)
			if nextStep != nil {
				outcome = "format_retry"
			}
		}
	}
	p.Turns++
	rs.room.UpdatedAt = nowMs()
	_ = h.store.AppendMessage(rs.room.ID, msg)
	rs.broadcast("message", msg)
	if h.Log != nil {
		h.Log.Printf("turn %s %s/%s status=%s %dms err=%v", rs.room.ID, p.ID, p.Model, msg.Status, msg.Duration, err)
	}

	if t.Reason == "review" {
		h.finishReview(rs, pr.part, msg, nextStep, outcome)
	}
	if workDone && msg.Status == "done" && pr.stopGen == rs.stopGen {
		h.submitAsync(rs, pr.part.ID)
	}
	if pr.stopGen == rs.stopGen && !p.Removed && msg.Status == "done" && t.Reason != "discussion" && t.Reason != "review" {
		h.chainMentions(rs, msg, t)
	}
	if pr.stopGen == rs.stopGen && t.Reason == "discussion" {
		h.discussionStep(rs, p.ID, signal, msg.Status)
	}
	previousStatus, previousText := p.Status, p.StatusText
	h.pump(rs)
	_ = h.store.SaveRoom(rs.room)
	if p.Status == previousStatus && p.StatusText == previousText {
		// The state helper already publishes transitions. Publish unchanged
		// state here only to deliver the updated turn/session/usage counters.
		rs.broadcast("participant", p)
	}
}

func applyUsage(p *Participant, prov agent.Provider, u map[string]any) {
	if u == nil {
		return
	}
	p.Usage = u
	num := func(k string) float64 {
		switch v := u[k].(type) {
		case float64:
			return v
		case int:
			return float64(v)
		}
		return 0
	}
	p.Totals.Turns++
	p.Totals.Input += num("input_tokens")
	p.Totals.Output += num("output_tokens")
	p.Totals.Cached += num("cache_read_input_tokens") + num("cached_input_tokens") + num("cache_read_tokens")
	p.Totals.Cost += num("total_cost_usd")
	if c := num("context_tokens"); c > 0 {
		if int(c) < p.ContextTokens { // the CLI compacted this session; the brief may be gone
			p.Brief = ""
		}
		p.ContextTokens = int(c)
	}
	if w := num("context_window"); w > 0 {
		p.ContextWindow = int(w)
	} else if p.ContextWindow == 0 {
		p.ContextWindow = prov.ContextWindow(p.Model)
	}
}

// deltaFor reads current message contents up to cap. Unfinished peer replies
// remain holes below LastSeen; own replies never become undelivered holes.
// Delivery is committed only after success (or when no CLI input is needed).
func (rs *roomState) deltaFor(p *Participant, first bool, cap int64) ([]*Message, int64, []int64) {
	return rs.deltaForContext(p, first, cap, "")
}

func (rs *roomState) deltaForContext(p *Participant, first bool, cap int64, partID string) ([]*Message, int64, []int64) {
	last := p.LastSeen
	if first {
		last = 0
	}
	seen := last
	var unseen []int64
	var out []*Message
	for _, seq := range p.Unseen {
		if seq > cap {
			unseen = append(unseen, seq)
		}
	}
	for _, m := range rs.msgs {
		if m.Seq > cap {
			break
		}
		if m.Seq <= last && !slices.Contains(p.Unseen, m.Seq) {
			continue
		}
		if m.Seq > seen {
			seen = m.Seq
		}
		if (m.Target != "" && m.Target != p.ID) || (!first && (m.Author == p.ID || (m.Live && m.Part == partID))) {
			continue
		}
		if m.Status == "streaming" {
			unseen = append(unseen, m.Seq)
			continue
		}
		if m.Status == "cancelled" && m.Text == "" {
			continue
		}
		out = append(out, m)
	}
	return out, seen, unseen
}

func (h *Hub) chainMentions(rs *roomState, msg *Message, t *Turn) {
	var others []*Participant
	for _, p := range rs.room.enabled() {
		if p.ID != msg.Author {
			others = append(others, p)
		}
	}
	targets := ParseParticipantMentions(msg.Text, others)
	if len(targets) == 0 {
		return
	}
	if t.Depth+1 > rs.room.MaxChain {
		rs.addSystem(fmt.Sprintf("@%s 가 %s 를 호출했지만 연쇄 한도(%d)에 도달해 멈췄어. 이어가려면 직접 호출해.", msg.Author, strings.Join(targets, ", "), rs.room.MaxChain))
		return
	}
	for _, id := range targets {
		rs.enqueue(&Turn{ID: newID(), Agent: id, Reason: "mention", Group: msg.ID, Cap: msg.Seq, parallel: len(targets) > 1, Depth: t.Depth + 1, Created: nowMs(), Status: "queued"})
	}
}

func (h *Hub) discussionStep(rs *roomState, agentID, signal, status string) {
	d := rs.disc
	if d == nil {
		return
	}
	if status != "done" {
		signal = "PASS"
	}
	d.Signals[agentID] = signal
	d.Done++
	rs.broadcast("discussion", rs.discView())
	if d.Done < d.Expected {
		return
	}
	concluded, allAgree := false, true
	for _, s := range d.Signals {
		if s == "CONCLUDE" {
			concluded = true
		}
		if s != "AGREE" && s != "PASS" {
			allAgree = false
		}
	}
	switch {
	case concluded:
		rs.addSystem(fmt.Sprintf("토론 종료 · %d라운드에서 결론 제시(CONCLUDE)", d.Round))
		rs.disc = nil
	case allAgree:
		rs.addSystem(fmt.Sprintf("토론 종료 · %d라운드에서 전원 동의/패스", d.Round))
		rs.disc = nil
	case d.Round >= d.Total:
		rs.addSystem(fmt.Sprintf("토론 종료 · 최대 %d라운드 도달", d.Total))
		rs.disc = nil
	case len(rs.room.enabled()) < 2:
		rs.addSystem("토론 종료 · 활성 참가자가 2명 미만이야")
		rs.disc = nil
	default:
		d.Round++
		rs.enqueueRound(h.enabledIDs(rs))
	}
	rs.broadcast("discussion", rs.discView())
}

// ---- export ----------------------------------------------------------------

func (h *Hub) ExportMarkdown(id string) (string, error) {
	snapshot, err := h.Snapshot(id)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", snapshot.Room.Title)
	fmt.Fprintf(&b, "- 작업 폴더: %s\n- 모드: %s\n- 내보낸 시각: %s\n\n", snapshot.Room.Workspace, snapshot.Room.Mode, time.Now().Format(time.RFC3339))
	for _, m := range snapshot.Messages {
		ts := time.UnixMilli(m.TS).Format("15:04:05")
		who := m.Author
		if m.Kind == "user" {
			who = "사용자"
		}
		fmt.Fprintf(&b, "### %s · %s", who, ts)
		if m.Model != "" {
			fmt.Fprintf(&b, " · %s", m.Model)
		}
		if m.Status != "done" {
			fmt.Fprintf(&b, " · %s", m.Status)
		}
		b.WriteString("\n\n")
		b.WriteString(m.Text)
		if m.Error != "" {
			fmt.Fprintf(&b, "\n\n> 오류: %s", m.Error)
		}
		b.WriteString("\n\n")
	}
	return b.String(), nil
}

// SetMCP attaches a read-only MCP server to every read/write run started after
// the call (nil detaches). Chat mode never gets it.
func (h *Hub) SetMCP(m *agent.MCPServer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.mcp = m
}
