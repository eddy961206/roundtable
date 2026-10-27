package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
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
	listSubs        map[chan []byte]struct{} // room-list feed; every roomState shares this map
	limits          map[string]agent.Limits
	usageStates     map[string]UsageSource
	usageFlights    map[string]chan struct{}
	usageCancels    map[string]context.CancelFunc
	limitsVersion   uint64
	limitsSaving    bool
	limitsDone      chan struct{}
	limitsSaveError error
	nativeSessions  map[string]string // provider/session key -> reserved working directory
	mcp             *agent.MCPServer  // read-only tool server attached to read/write runs
	workRoot        string            // <data>/worktrees: part and integration worktrees live outside user repos
	Log             *log.Logger
}

type roomState struct {
	requests  []Request
	decisions []Decision
	room      *Room
	msgs      []*Message
	queue     []*Turn
	running   map[string]*running
	subs      map[chan []byte]struct{}
	listSubs  map[chan []byte]struct{} // Hub.listSubs; nil once the room is deleted
	listed    RoomSummary              // sidebar values last announced on the room-list feed
	disc      *discussion
	deleted   bool // late invocation callbacks must not recreate a deleted room
	stopGen   int  // prevents a stopped turn from scheduling follow-up work
	resetGen  int  // bumped by ResetSessions so a finishing turn does not resurrect the old session

	workBusy          map[string]bool   // plan creation, submit checks or cleanup running git outside h.mu
	participantEvents map[string]string // last serialized seat state sent to subscribers
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
	Failed   []string // seats excluded in the current round
	Blind    bool     // round 1 runs concurrently from the start cap (blind first)
}

func NewHub(store *Store, reg *agent.Registry, scratch string, logger *log.Logger) (*Hub, error) {
	if _, err := store.RolePresets(); err != nil {
		return nil, err
	}
	absoluteScratch, err := filepath.Abs(scratch)
	if err != nil {
		return nil, fmt.Errorf("작업 임시 폴더: %w", err)
	}
	workRoot, err := filepath.Abs(filepath.Join(store.Root(), "worktrees"))
	if err != nil {
		return nil, fmt.Errorf("worktree 폴더: %w", err)
	}
	h := &Hub{store: store, reg: reg, scratch: absoluteScratch, rooms: map[string]*roomState{}, listSubs: map[chan []byte]struct{}{}, limits: map[string]agent.Limits{}, nativeSessions: map[string]string{}, workRoot: workRoot, Log: logger}
	rooms, msgs, _, err := store.loadRooms(false, true)
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
		// Persist startup recovery before any queued work can run. Statistics
		// must see the same cancelled turns as the recovered room snapshot.
		for _, m := range msgs[r.ID] {
			if m.Status == "streaming" {
				m.Status = "cancelled"
				if err := store.AppendMessage(r.ID, m); err != nil {
					return nil, fmt.Errorf("방 %s 중단 메시지 복구: %w", r.ID, err)
				}
			}
		}
		if r.Workspace != "" {
			r.Workspace, err = filepath.Abs(r.Workspace)
			if err != nil {
				return nil, fmt.Errorf("방 %s 작업 폴더: %w", r.ID, err)
			}
		}
		rs := &roomState{room: r, msgs: msgs[r.ID], running: map[string]*running{}, subs: map[chan []byte]struct{}{}, listSubs: h.listSubs, workBusy: map[string]bool{}}
		backfillInput(rs)
		rs.requests, err = store.loadRequests(r.ID)
		if err != nil {
			return nil, err
		}
		rs.decisions, err = store.loadDecisions(r.ID)
		if err != nil {
			return nil, err
		}
		for _, request := range slices.Clone(rs.requests) {
			if request.Status == "in_progress" {
				request.Status = "pending"
				if err := h.saveRequest(rs, request); err != nil {
					return nil, err
				}
			}
		}
		if err := store.cleanupAttachments(r.ID, rs.msgs); err != nil {
			return nil, err
		}
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
		rs.listed = rs.summary()
		h.rooms[r.ID] = rs
	}
	if err := h.resumeRecoveredWork(); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Hub) resumeRecoveredWork() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, rs := range h.rooms {
		// Rooms awaiting cleanup resume their queue when that worker finishes.
		if len(h.worktreeCleanupTargets(rs)) == 0 {
			h.advanceQueue(rs)
		}
		if rs.room.Work != nil {
			if err := h.store.SaveRoom(rs.room); err != nil {
				return err
			}
		}
	}
	// Do not launch any cleanup worker until every startup save has succeeded.
	for _, rs := range h.rooms {
		h.startWorktreeCleanup(rs)
	}
	return nil
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
	// BlindFirstDefault is nil when omitted; new rooms then start discussions blind.
	BlindFirstDefault *bool `json:"blind_first_default"`
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
	r := &Room{ID: newID(), Title: spec.Title, CreatedAt: nowMs(), UpdatedAt: nowMs(), Workspace: spec.Workspace, Mode: spec.Mode, Note: spec.Note, MaxChain: spec.MaxChain, DiscussionRounds: spec.DiscussionRounds, Persona: spec.Persona, BlindFirstDefault: true}
	if spec.BlindFirstDefault != nil {
		r.BlindFirstDefault = *spec.BlindFirstDefault
	}
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
	rs := &roomState{room: r, decisions: []Decision{}, running: map[string]*running{}, subs: map[chan []byte]struct{}{}, listSubs: h.listSubs, workBusy: map[string]bool{}}
	rs.listed = rs.summary()
	h.rooms[r.ID] = rs
	signalRooms(h.listSubs)
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
		out = append(out, rs.summary())
	}
	sortSummaries(out)
	return out
}

func (rs *roomState) summary() RoomSummary {
	return RoomSummary{ID: rs.room.ID, Title: rs.room.Title, UpdatedAt: rs.room.UpdatedAt, Mode: rs.room.Mode, Messages: len(rs.msgs), Active: len(rs.running) > 0 || len(rs.queue) > 0}
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
	Requests   []Request               `json:"requests"`
	Decisions  []Decision              `json:"decisions"`
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
	Blind   bool              `json:"blind"` // the discussion started with a blind round 1
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
	return &DiscussionView{Total: rs.disc.Total, Round: rs.disc.Round, Signals: rs.disc.Signals, Blind: rs.disc.Blind}
}

// ParticipantPatch updates one seat.
type ParticipantPatch struct {
	ID      string  `json:"id"`
	Label   *string `json:"label,omitempty"`
	Enabled *bool   `json:"enabled,omitempty"`
	Model   *string `json:"model,omitempty"`
	Effort  *string `json:"effort,omitempty"`
	Role    *string `json:"role,omitempty"`
}

// RoomPatch is a partial update.
type RoomPatch struct {
	Title              *string            `json:"title,omitempty"`
	Note               *string            `json:"note,omitempty"`
	Mode               *string            `json:"mode,omitempty"`
	Workspace          *string            `json:"workspace,omitempty"`
	MaxChain           *int               `json:"max_chain,omitempty"`
	DiscussionRounds   *int               `json:"discussion_rounds,omitempty"`
	Persona            *bool              `json:"persona,omitempty"`
	BlindFirstDefault  *bool              `json:"blind_first_default,omitempty"`
	InputWarnTokens    *int64             `json:"input_warn_tokens,omitempty"`
	SeatInputBudget    *int64             `json:"seat_input_budget,omitempty"`
	RoomInputBudget    *int64             `json:"room_input_budget,omitempty"`
	ContextWarnPercent *int               `json:"context_warn_percent,omitempty"`
	Participants       []ParticipantPatch `json:"participants,omitempty"`
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
	if patch.InputWarnTokens != nil {
		next.InputWarnTokens = *patch.InputWarnTokens
	}
	if patch.SeatInputBudget != nil {
		next.SeatInputBudget = *patch.SeatInputBudget
	}
	if patch.RoomInputBudget != nil {
		next.RoomInputBudget = *patch.RoomInputBudget
	}
	if patch.ContextWarnPercent != nil {
		next.ContextWarnPercent = *patch.ContextWarnPercent
	}
	if err := validateGuards(&next); err != nil {
		return nil, err
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
	if patch.BlindFirstDefault != nil {
		next.BlindFirstDefault = *patch.BlindFirstDefault
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
		if pp.Role != nil {
			p.Role = strings.TrimSpace(*pp.Role)
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
				p.Unseen, p.Fork = nil, nil
				p.ContextWindow = prov.ContextWindow(p.Model)
			}
		}
	}
	if contextChanged {
		for _, p := range next.Participants {
			p.SessionID, p.LastSeen, p.ContextTokens = "", 0, 0
			p.Unseen, p.Fork = nil, nil
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

// dropQueuedFor removes a disabled participant's waiting turns; discussions exclude them.
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
	next := *rs.room
	next.Participants = make([]*Participant, len(rs.room.Participants))
	next.UpdatedAt = nowMs()
	for i, p := range rs.room.Participants {
		copy := *p
		copy.SessionID, copy.LastSeen, copy.ContextTokens = "", 0, 0
		copy.Unseen, copy.Fork = nil, nil
		next.Participants[i] = &copy
	}
	if err := h.store.SaveRoom(&next); err != nil {
		return h.reportPersistenceError(rs, "세션 초기화", err)
	}
	// Running turns retain participant pointers; apply only after the save commits.
	for i, p := range rs.room.Participants {
		*p = *next.Participants[i]
	}
	rs.room.UpdatedAt = next.UpdatedAt
	rs.resetGen++
	rs.broadcast("room", rs.room)
	h.addSystem(rs, "세션을 초기화했어. 다음 턴부터 각 에이전트는 대화 전체를 새로 읽어.")
	return nil // the reset is committed even if its explanatory note cannot be saved
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
	h.stopLocked(rs, "방 삭제") // deletion still cancels turns if the stop note cannot be saved
	delete(h.rooms, id)
	for ch := range rs.subs {
		close(ch)
	}
	rs.subs = map[chan []byte]struct{}{} // late broadcasts from a finishing turn find nobody
	rs.listSubs = nil
	signalRooms(h.listSubs)
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

// SubscribeRooms feeds a signal whenever a value of GET /api/rooms changes. The
// signal carries no room data. A channel holds one pending signal, so a burst of
// changes made before the client reads reaches it once.
func (h *Hub) SubscribeRooms() (<-chan []byte, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, nil, fmt.Errorf("서버가 종료 중이야")
	}
	ch := make(chan []byte, 1)
	h.listSubs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, still := h.listSubs[ch]; still {
			delete(h.listSubs, ch)
			close(ch)
		}
	}, nil
}

var roomsEvent = []byte(`{"type":"rooms"}`)

// Field order matches the old map envelopes so SSE bytes stay stable. Typed
// envelopes avoid rebuilding and sorting maps for every streamed event.
type roomEvent struct {
	Data any    `json:"data"`
	Type string `json:"type"`
}

type textDeltaEvent struct {
	Block bool   `json:"block,omitempty"`
	ID    string `json:"id"`
	Text  string `json:"text"`
}

type traceEvent struct {
	Event agent.Event `json:"event"`
	Index int         `json:"index"`
	MsgID string      `json:"msg_id"`
}

func signalRooms(subs map[chan []byte]struct{}) {
	for ch := range subs {
		select {
		case ch <- roomsEvent:
		default: // the pending signal already makes the client read the new list
		}
	}
}

func (rs *roomState) broadcast(typ string, payload any) {
	// Room events follow every change to this room's sidebar values.
	if summary := rs.summary(); summary != rs.listed {
		rs.listed = summary
		signalRooms(rs.listSubs)
	}
	if typ == "room" {
		// A full room event also delivers every seat; compare future participant
		// events against that state, including changes made by PATCH/reset.
		rs.participantEvents = map[string]string{}
		for _, p := range rs.room.Participants {
			encoded, err := json.Marshal(p)
			if err != nil {
				panic(err)
			}
			rs.participantEvents[p.ID] = string(encoded)
		}
	}
	if typ == "participant" {
		p := payload.(*Participant)
		encoded, err := json.Marshal(p)
		if err != nil {
			panic(err) // participant state must be JSON serializable
		}
		if rs.participantEvents == nil {
			rs.participantEvents = map[string]string{}
		}
		if rs.participantEvents[p.ID] == string(encoded) {
			return
		}
		rs.participantEvents[p.ID] = string(encoded)
		payload = json.RawMessage(encoded)
	}
	if len(rs.subs) == 0 {
		return
	}
	b, err := json.Marshal(roomEvent{Data: payload, Type: typ})
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
	if err := h.checkTraceMessage(id, msgID); err != nil {
		return nil, err
	}
	return h.store.ReadTrace(id, msgID)
}

func (h *Hub) checkTraceMessage(id, msgID string) error {
	h.mu.Lock()
	rs, ok := h.rooms[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("방 없음: %s", id)
	}
	found := slices.ContainsFunc(rs.msgs, func(message *Message) bool { return message.ID == msgID })
	h.mu.Unlock()
	if !found {
		return fmt.Errorf("메시지 없음: %s", msgID)
	}
	return nil
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

// reportPersistenceError also covers background transitions with no API caller.
// Use the existing message event so both live clients and later snapshots show
// the failure. Attempt to persist it once, without recursively reporting errors.
func (h *Hub) reportPersistenceError(rs *roomState, operation string, err error) error {
	if err == nil {
		return nil
	}
	err = fmt.Errorf("%s 저장 실패: %w", operation, err)
	if h.Log != nil {
		h.Log.Printf("room %s: %v", rs.room.ID, err)
	}
	m := rs.nextMessage("system", "system", "서버 저장 오류: "+err.Error())
	m.Status, m.Error = "error", err.Error()
	if noteErr := h.store.AppendMessage(rs.room.ID, m); noteErr != nil {
		m.Text += "\n이 오류 알림도 저장하지 못했어. 현재 서버 메모리에만 남아 있고 재시작하면 사라져."
		if h.Log != nil {
			h.Log.Printf("room %s: 오류 알림 저장 실패: %v", rs.room.ID, noteErr)
		}
	}
	rs.broadcast("message", m)
	return err
}

func (h *Hub) saveRoom(rs *roomState) error {
	return h.reportPersistenceError(rs, "방", h.store.SaveRoom(rs.room))
}

func (h *Hub) addSystem(rs *roomState, text string) (*Message, error) {
	m := rs.nextMessage("system", "system", text)
	if err := h.store.AppendMessage(rs.room.ID, m); err != nil {
		rs.msgs = rs.msgs[:len(rs.msgs)-1]
		// Keep the sequence reserved: a write/close error may follow a partial append.
		return nil, h.reportPersistenceError(rs, "시스템 메시지", err)
	}
	rs.broadcast("message", m)
	return m, nil
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
func (h *Hub) PostUser(id, text, target string, attachmentIDs ...string) (*Message, error) {
	text = strings.TrimSpace(text)
	if text == "" && len(attachmentIDs) == 0 {
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
	if other := h.workspaceRival(rs); other != nil {
		return nil, rivalError(other)
	}
	if p := rs.room.participant(target); target != "" && p.HoldUntil != 0 {
		// Addressing a held seat directly is the explicit retry.
		p.HoldUntil, p.HoldReason = 0, ""
		h.addSystem(rs, fmt.Sprintf("@%s 대상 지정 메시지로 한도 보류를 풀고 바로 다시 시도함", p.ID))
	}
	attachments, err := h.resolveAttachments(rs, attachmentIDs)
	if err != nil {
		return nil, err
	}
	m := rs.nextMessage("user", "user", text)
	m.Attachments = attachments
	m.Target = target
	mentioned := append([]string{}, ParseParticipantMentions(text, rs.room.Participants)...)
	m.Mentions = &mentioned
	var liveRun *running
	if target != "" && len(attachments) == 0 {
		if run := rs.runningFor(target); run != nil && run.steer != nil && len(run.steer) < cap(run.steer) {
			// h.mu serializes all senders; the provider can only free capacity.
			liveRun = run
			m.Live, m.Part = true, run.turn.Part
		}
	}
	if err := h.store.AppendMessage(id, m); err != nil {
		rs.msgs = rs.msgs[:len(rs.msgs)-1]
		return nil, h.reportPersistenceError(rs, "사용자 메시지", err)
	}
	// The message append is the acceptance boundary. Subsequent metadata or
	// explanatory-note failures must not tell the caller to resend accepted input.
	if liveRun != nil {
		liveRun.steer <- text
		liveRun.live = append(liveRun.live, m)
	}
	rs.broadcast("message", m)
	h.saveRoom(rs)
	if target != "" {
		if !m.Live {
			rs.enqueueFront(&Turn{ID: newID(), Agent: target, Reason: "mention", Group: m.ID, Cap: m.Seq, Created: nowMs(), Status: "queued"})
			h.pump(rs)
		}
		mc := *m
		return &mc, nil
	}
	ids := h.enabledIDs(rs)
	var targets []string
	for _, id := range mentioned {
		if slices.Contains(ids, id) {
			targets = append(targets, id)
		}
	}
	reason := "mention"
	switch {
	case len(mentioned) > 0 && len(targets) == 0:
		h.addSystem(rs, fmt.Sprintf("@%s 는 비활성 또는 삭제 상태라 아무도 답하지 않아. 다른 활성 참가자를 불러.", strings.Join(mentioned, ", @")))
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

// StartDiscussion runs up to `rounds` rounds over every enabled agent. With
// blindFirst (nil: the room's blind_first_default) round 1 runs concurrently
// from the start cap, so no seat sees another seat's round-1 answer; later
// rounds are sequential and read the current history.
func (h *Hub) StartDiscussion(id string, rounds int, blindFirst *bool) error {
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
	blind := rs.room.BlindFirstDefault
	if blindFirst != nil {
		blind = *blindFirst
	}
	order := "순서대로 발언"
	if blind {
		order = "1라운드 블라인드(동시 답변, 2라운드에 공개)"
	}
	if _, err := h.addSystem(rs, fmt.Sprintf("토론 시작 · 최대 %d라운드 · %s · 참가 %s", rounds, order, strings.Join(ids, ", "))); err != nil {
		return err
	}
	rs.disc = &discussion{Total: rounds, Round: 1, Signals: map[string]string{}, Blind: blind}
	rs.enqueueRound(ids)
	rs.broadcast("discussion", rs.discView())
	h.pump(rs)
	return nil
}

func (rs *roomState) enqueueRound(ids []string) {
	rs.disc.Expected, rs.disc.Done = len(ids), 0
	rs.disc.Signals = map[string]string{}
	rs.disc.Failed = nil
	blind := rs.disc.Blind && rs.disc.Round == 1
	group := newID()
	for _, id := range ids {
		t := &Turn{ID: newID(), Agent: id, Reason: "discussion", Round: rs.disc.Round, Group: newID(), Created: nowMs(), Status: "queued"}
		if blind {
			// Every round-1 seat shares one cap, like broadcast members.
			t.Group, t.Cap, t.Blind = group, rs.room.Seq, true
		}
		rs.enqueue(t)
	}
}

// historyCap is the inclusive message boundary a turn reads. Sequential
// discussion turns read the current history; blind round-1 turns keep the cap
// taken when the discussion started.
func (rs *roomState) historyCap(t *Turn) int64 {
	if t.Reason == "discussion" && !t.Blind {
		return rs.room.Seq
	}
	return t.Cap
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
	h.stopLocked(rs, "사용자가 중지함")
	return nil // cancellation is complete; notice failures are reported separately
}

func (h *Hub) stopLocked(rs *roomState, why string) error {
	dropped := len(rs.queue)
	rs.queue = nil
	rs.stopGen++
	for _, r := range rs.running {
		r.cancel()
	}
	rs.disc = nil
	rs.refreshParticipantStates()
	var err error
	if dropped > 0 || len(rs.running) > 0 {
		_, err = h.addSystem(rs, fmt.Sprintf("중지: %s (대기 %d건 취소, 실행 중 %d건 중단)", why, dropped, len(rs.running)))
	}
	rs.broadcast("queue", rs.queueView())
	rs.broadcast("discussion", nil)
	return err
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
		part := rs.seatPart(p.ID)
		partActive := part != nil && (part.State == "working" || part.State == "fixing")
		if t.Reason == "part" && !partActive {
			rs.queue = slices.Delete(rs.queue, i, i+1)
			skipped = true
			continue
		}
		// Room discussions can run outside an inactive part. Submission still
		// reserves the author until its state and worktree checks are settled.
		if t.Reason != "review" && part != nil && (rs.workBusy[part.ID] || (!partActive && t.Reason != "discussion")) {
			i++
			continue
		}
		// Ordinary priority turns must not block a later discussion turn for
		// the same participant, or the discussion would never finish.
		if rs.disc != nil && t.Reason != "discussion" {
			i++
			continue
		}
		// Sequential discussion turns run one at a time in queue order; blind
		// round-1 turns only wait for their own seat.
		sequential := t.Reason == "discussion" && !t.Blind
		if rs.runningFor(t.Agent) != nil || (sequential && discussionBlocked) {
			if sequential {
				discussionBlocked = true
			}
			i++
			continue
		}
		if seatHeld(p) {
			i++ // a limit hold; its timer pumps again when the hold ends
			continue
		}
		// Part and review turns run in their own worktree, not the room folder.
		if t.Reason != "part" && t.Reason != "review" && h.workspaceRival(rs) != nil {
			i++ // waits; pumpPeers starts it when the other room is done
			continue
		}
		rs.queue = slices.Delete(rs.queue, i, i+1)
		if why := rs.budgetHold(p); why != "" && t.Reason != "part" && t.Reason != "review" {
			if t.Reason == "discussion" {
				h.discussionStep(rs, t.Agent, "PASS", "cancelled")
			}
			h.holdTurn(rs, p, why)
			skipped = true
			continue
		}
		p.BudgetHeld = ""
		pr := h.buildTurn(rs, t)
		if pr == nil {
			skipped = true
			continue
		}
		h.prepareTurn(rs, pr)
		if sequential {
			discussionBlocked = true
		}
		h.turnWG.Add(1)
		go h.runTurn(rs, pr)
	}
	rs.refreshParticipantStates()
	rs.broadcast("queue", rs.queueView())
	if skipped {
		h.saveRoom(rs) // background failures are reported to the log and event stream
	}
}

// prepared contains a prompt rendered from live messages up to the turn's cap.
type prepared struct {
	started   time.Time
	notices   []*Message
	requests  []string
	inputErr  error
	forkCost  float64 // the base session's running cost; a fork's reported cost includes it
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
	cap := rs.historyCap(t)
	hist, seen, unseen := rs.deltaFor(p, first, cap)
	if len(hist) == 0 && t.Reason != "discussion" && len(rs.pendingNotices(p.ID, "")) == 0 {
		// No CLI call is needed to acknowledge own/filtered messages. Keep
		// unfinished replies as holes for a later cap to deliver exactly once.
		p.LastSeen, p.Unseen = seen, unseen
		return nil
	}
	decisions := rs.activeDecisionTexts(cap)
	brief := briefKey(standingBrief(rs.room, p, decisions, h.scratch))
	forked := first && p.Fork != nil
	tc := turnContext{Reason: t.Reason, Parallel: t.parallel, Round: t.Round, FirstTurn: first, Forked: forked, Briefed: !first && p.Brief == brief, Decisions: decisions, Scratch: h.scratch}
	if rs.disc != nil && t.Reason == "discussion" {
		tc.TotalRounds, tc.BlindFirst = rs.disc.Total, rs.disc.Blind
	}
	prompt := BuildPrompt(rs.room, p, hist, tc)
	opts := agent.RunOptions{Prompt: prompt, SessionID: p.SessionID, Model: p.Model, Effort: p.Effort, Mode: rs.room.Mode, Workspace: rs.room.Workspace, ScratchDir: h.scratch, Timeout: turnTimeout, MCP: h.mcp}
	h.addTurnImages(rs, p, hist, &opts, t.Reason)
	pr := &prepared{t: t, p: p, seen: seen, unseen: unseen, brief: brief, stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts}
	if forked {
		pr.opts.ForkSession = p.Fork.Session
		if p.Fork.Dir != sessionDir(rs.room) {
			// CLIs fork across directories; the copied context still names the base folder.
			pr.opts.Prompt = forkMovedNote(p.Fork.Dir, sessionDir(rs.room)) + pr.opts.Prompt
		}
		pr.forkCost, pr.inputErr = h.forkBaseCost(p.Fork)
	}
	return pr
}

// forkBaseCost is the base session's running cost. A running base turn would
// be copied half-written, so it fails this turn instead. Caller holds h.mu.
func (h *Hub) forkBaseCost(f *ForkSource) (float64, error) {
	base, ok := h.rooms[f.Room]
	if !ok {
		return 0, fmt.Errorf("기반 방 %s이 삭제되어 세션을 포크할 수 없음", f.Room)
	}
	if base.runningFor(f.Seat) != nil {
		return 0, fmt.Errorf("기반 좌석 %s가 실행 중이라 세션을 포크할 수 없음. 기반 턴이 끝난 뒤 다시 보내야 함", f.Seat)
	}
	return base.sessionCost(f.Session), nil
}

// buildPartTurn runs every turn of a seat with a working/fixing part in the
// part session and worktree (D9). The first part turn gets only plan context.
func (h *Hub) buildPartTurn(rs *roomState, t *Turn, p *Participant, prov agent.Provider, part *Part) *prepared {
	if part.WorktreeRemoved {
		h.workNote(rs, removedWorktreeError(part).Error())
		return nil
	}
	t.Part = part.ID
	seen, unseen := part.LastSeen, slices.Clone(part.Unseen)
	bootstrap := part.SessionID == ""
	var prompt string
	var hist []*Message
	switch {
	case part.SessionID == "":
		prompt = BuildPartPrompt(rs.room.Work, part, p.Role, h.scratch)
		if t.prompt != "" {
			prompt += "\n\n" + t.prompt
		}
	case t.prompt != "":
		prompt = t.prompt
	default:
		cap := rs.historyCap(t)
		cursor := *p
		cursor.LastSeen, cursor.Unseen = part.LastSeen, part.Unseen
		hist, seen, unseen = rs.deltaForContext(&cursor, false, cap, part.ID)
		if len(hist) == 0 && t.Reason != "discussion" && len(rs.pendingNotices(p.ID, part.ID)) == 0 {
			part.LastSeen, part.Unseen = seen, unseen
			return nil
		}
		tc := turnContext{Reason: t.Reason, Parallel: t.parallel, Round: t.Round, Part: part, Decisions: rs.activeDecisionTexts(cap), Scratch: h.scratch}
		if rs.disc != nil && t.Reason == "discussion" {
			tc.TotalRounds, tc.BlindFirst = rs.disc.Total, rs.disc.Blind
		}
		view := *rs.room
		view.Workspace = part.Worktree
		view.Mode = agent.ModeWrite
		prompt = BuildPrompt(&view, p, hist, tc)
	}
	opts := agent.RunOptions{Prompt: prompt, SessionID: part.SessionID, Model: p.Model, Effort: p.Effort, Mode: agent.ModeWrite, Workspace: part.Worktree, ScratchDir: h.scratch, Timeout: turnTimeout, MCP: h.mcp}
	h.addTurnImages(rs, p, hist, &opts, t.Reason)
	return &prepared{t: t, p: p, seen: seen, unseen: unseen, stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts, part: part, bootstrap: bootstrap}
}

// buildReviewTurn runs a ReviewState step: the reviewer in a fresh read-only
// session (D10), the author in the part session. Room history is never added.
func (h *Hub) buildReviewTurn(rs *roomState, t *Turn, p *Participant, prov agent.Provider) *prepared {
	part := rs.room.Work.part(t.Part)
	if part.State != "reviewing" {
		return nil
	}
	if part.WorktreeRemoved {
		h.workNote(rs, removedWorktreeError(part).Error())
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
	pr.started = time.Now()
	rs.addTurnNotices(pr)
	h.beginRequests(rs, pr)
	t, p, prov := pr.t, pr.p, pr.prov
	msg := rs.nextMessage("agent", p.ID, "")
	msg.Status, msg.TurnID, msg.Reason, msg.Round, msg.Model = "streaming", t.ID, t.Reason, t.Round, p.Model
	// Discussion/work grouping can use synthetic IDs with no trigger message.
	for _, trigger := range rs.msgs {
		if trigger.ID == t.Group {
			msg.ReplyTo = t.Group
			break
		}
	}
	msg.Part, msg.ReviewStep = t.Part, t.step
	msg.Provider, msg.SessionID, msg.Mode, msg.Effort = p.Provider, pr.opts.SessionID, pr.opts.Mode, pr.opts.Effort
	msg.Role = p.Role
	msg.Timing = &TurnTiming{QueuedMs: max(0, pr.started.UnixMilli()-t.Created)}
	if t.Reason == "review" && pr.part.Review != nil {
		msg.Round = pr.part.Review.Input.Ref.Round
	}
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
	started := pr.started
	firstOutput := false
	var textBuf strings.Builder
	var emitMu sync.Mutex
	var tracePersistenceError error
	compacted := false // a usage report showed the session's context shrink
	emit := func(e agent.Event) {
		emitMu.Lock()
		defer emitMu.Unlock()
		observed := time.Since(started).Milliseconds()
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
		if !firstOutput {
			msg.Timing.FirstOutputMs = observed
			firstOutput = true
		}
		previousStatusText, previousSession := p.StatusText, p.SessionID
		switch e.Kind {
		case agent.KindSession:
			if e.Text != "" {
				msg.SessionID = e.Text
				h.keepSession(rs, pr, e.Text)
				if err := h.store.AppendMessage(rs.room.ID, msg); err != nil {
					tracePersistenceError = h.reportPersistenceError(rs, "세션 메시지", err)
					cancel()
					return
				}
				// The session message is durable. Report metadata failures without
				// discarding the answer; completion saves the room again.
				h.saveRoom(rs)
				rs.broadcast("message", msg)
			}
		case agent.KindTextDelta:
			textBuf.WriteString(e.Text)
			msg.Text = textBuf.String()
			p.StatusText = "답변 중"
			rs.broadcast("delta", textDeltaEvent{ID: msg.ID, Text: e.Text})
		case agent.KindText:
			if textBuf.Len() > 0 {
				textBuf.WriteString("\n\n")
			}
			textBuf.WriteString(e.Text)
			msg.Text = textBuf.String()
			rs.broadcast("delta", textDeltaEvent{ID: msg.ID, Text: e.Text, Block: true})
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
			if c, ok := e.Meta["session_cost_usd"].(float64); ok {
				before := rs.sessionCost(opts.SessionID)
				if opts.ForkSession != "" {
					before = pr.forkCost
				}
				e.Meta["total_cost_usd"] = c - before
			}
			compacted = applyUsage(p, prov, e.Meta) || compacted
		}
		rs.broadcast("trace", traceEvent{MsgID: msg.ID, Index: msg.Trace, Event: e})
		// This switch changes only status text, session ID and usage. Other
		// participant mutations publish through their own setters. Preserve
		// every real transition while avoiding full participant JSON encoding
		// on each token/tool-result event merely to rediscover equal state.
		if p.StatusText != previousStatusText || p.SessionID != previousSession || e.Kind == agent.KindUsage {
			rs.broadcast("participant", p)
		}
	}

	var res agent.Result
	err := ctx.Err()
	if err == nil {
		err = pr.inputErr
	}
	if err == nil {
		err = workspaceMissing(opts)
	}
	if err == nil && len(opts.Images) > 0 && p.Provider != "codex" && p.Provider != "claude" && p.Provider != "agy" {
		err = fmt.Errorf("%s 공급사의 이미지 전달은 지원하지 않음", p.Provider)
	}
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
	msg.Timing.TotalMs = msg.Duration
	msg.Usage = res.Usage
	p.InputAll += normalizedInput(p.Provider, res.Usage)
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
	rawText := text
	var signal string
	if t.Reason == "discussion" {
		text, signal = ParseSignal(text)
	}
	workDone := false
	if pr.part != nil && t.Reason != "review" && t.Reason != "discussion" {
		text, workDone = ParseWorkDone(text)
		if workDone {
			msg.Signal = workDoneSignal
		}
	}
	msg.Text = text
	mentions := append([]string{}, ParseParticipantMentions(ordinaryMentionText(text), rs.room.Participants)...)
	msg.Mentions = &mentions
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
	missingDiscussionSignal := t.Reason == "discussion" && signal == ""
	if missingDiscussionSignal {
		signal = "CONTINUE"
	}
	// A turn that wrote output had its prompt delivered even when the CLI then
	// failed (e.g. a stream idle timeout after the answer). Advancing the cursor
	// keeps a waiting turn for the same messages from doing the work again. A
	// stop, or a failure before any output, leaves the messages unseen.
	delivered := msg.Status == "done" || (msg.Status == "error" && rawText != "")
	if delivered && t.Reason != "review" {
		if pr.part != nil {
			pr.part.LastSeen, pr.part.Unseen = pr.seen, pr.unseen
		} else if pr.resetGen == rs.resetGen {
			p.LastSeen, p.Unseen, p.Brief = pr.seen, pr.unseen, pr.brief
			if compacted { // the summary may have dropped the brief; send it again next turn
				p.Brief = ""
			}
		}
	}
	if msg.Status == "done" && t.Reason != "review" {
		if pr.bootstrap && t.Reason != "part" && pr.part.SessionID != "" && pr.stopGen == rs.stopGen {
			replay := *t
			replay.ID, replay.Status, replay.Created = newID(), "queued", nowMs()
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
	// A failed turn's markers wait for ApplyMarkers; count them so the room can say so.
	if msg.Status == "error" && pr.part == nil && t.Reason != "discussion" && t.Reason != "review" {
		msg.PendingMarkers = markerLines(rawText)
	}
	// D11: malformed output is not applied; allow one persisted correction per step.
	var nextStep *ReviewStep
	var outcome string
	if t.Reason == "review" && msg.Status == "done" && pr.part.State == "reviewing" {
		var applyErr error
		nextStep, outcome, applyErr = pr.part.Review.Apply(text)
		if applyErr != nil {
			msg.Status, msg.Error, msg.ErrorKind = "error", "리뷰 표 형식 오류: "+applyErr.Error(), "review_format"
			nextStep = pr.part.Review.RetryFormat(applyErr)
			if nextStep != nil {
				outcome = "format_retry"
			}
		}
	}
	if err := h.store.AppendMessage(rs.room.ID, msg); err != nil {
		msg.Status, msg.Error = "error", fmt.Sprintf("메시지 저장 실패: %v", err)
	}
	if err := h.finishMarkers(rs, pr, rawText, msg.Status == "done"); err != nil {
		msg.Status, msg.Error = "error", fmt.Sprintf("첨부·요청 저장 실패: %v", err)
	}
	p.Turns++
	rs.room.UpdatedAt = nowMs()
	if msg.Status == "done" {
		if err := h.acknowledgeNotices(rs, pr); err != nil {
			msg.Status, msg.Error = "error", fmt.Sprintf("공지 전달 기록 저장 실패: %v", err)
		}
	}
	if t.Reason == "discussion" && msg.Status == "done" {
		// Persist a vote only after all completion handlers have succeeded.
		msg.Signal = signal
	}
	if err := h.store.AppendMessage(rs.room.ID, msg); err != nil {
		msg.Status, msg.Error = "error", fmt.Sprintf("메시지 저장 실패: %v", err)
		if t.Reason == "discussion" {
			msg.Signal = ""
		}
		h.reportPersistenceError(rs, "메시지", err)
	}
	rs.broadcast("message", msg)
	h.usageNotices(rs, p)
	if msg.Status == "error" && err != nil && !interrupted {
		if hold, until, reason, estimated := classifyLimit(msg.Error, time.Now()); hold {
			h.holdSeat(rs, p, until, reason, estimated)
		}
	}
	if msg.PendingMarkers > 0 {
		h.addSystem(rs, fmt.Sprintf("@%s 턴 #%d이 오류로 끝나 답의 마커 %d개(요청·결정·첨부)를 처리하지 않음 · 답이 끝까지 쓰였다면 화면의 '마커 실행' 또는 rt apply_markers로 실행", p.ID, msg.Seq, msg.PendingMarkers))
	}
	if missingDiscussionSignal && msg.Status == "done" {
		// No completion handler after this point can invalidate the recorded vote.
		// A failed notice save reports its own storage error, not a failed answer.
		h.addSeatNotice(rs, pr, "discussion", fmt.Sprintf("@%s 토론 신호가 없어 CONTINUE로 처리함", p.ID))
	}
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
	h.pumpPeers(rs)
	h.saveRoom(rs)
	if p.Status == previousStatus && p.StatusText == previousText {
		// The state helper already publishes transitions. Publish unchanged
		// state here only to deliver the updated turn/session/usage counters.
		rs.broadcast("participant", p)
	}
}

// sessionCost is a resumed session's running cost before this turn: the newest
// session_cost_usd reported in that session. A new session starts at zero.
func (rs *roomState) sessionCost(session string) float64 {
	if session == "" {
		return 0
	}
	for i := len(rs.msgs) - 1; i >= 0; i-- {
		m := rs.msgs[i]
		if c, ok := m.Usage["session_cost_usd"].(float64); ok && m.SessionID == session {
			return c
		}
	}
	return 0
}

// applyUsage records one usage report and tells whether the session's context
// shrank, which means the CLI compacted it.
func applyUsage(p *Participant, prov agent.Provider, u map[string]any) bool {
	if u == nil {
		return false
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
	compacted := false
	if c := num("context_tokens"); c > 0 {
		compacted = int(c) < p.ContextTokens
		p.ContextTokens = int(c)
	}
	if w := num("context_window"); w > 0 {
		p.ContextWindow = int(w)
	} else if p.ContextWindow == 0 {
		p.ContextWindow = prov.ContextWindow(p.Model)
	}
	return compacted
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
		if m.NoticeFor == p.ID {
			continue
		} // delivered separately, regardless of cap or history trimming
		out = append(out, m)
	}
	return out, seen, unseen
}

func (h *Hub) chainMentions(rs *roomState, msg *Message, t *Turn) {
	if t.Reason == "discussion" || t.Reason == "review" {
		return
	}
	var targets []string
	for _, id := range msg.mentionIDs(rs.room.Participants) {
		if p := rs.room.participant(id); p != nil && p.Enabled && !p.Removed && p.ID != msg.Author {
			targets = append(targets, id)
		}
	}
	for _, id := range msg.Requests {
		if r := rs.request(id); r != nil && r.Status == "pending" && !slices.Contains(targets, r.To) {
			targets = append(targets, r.To)
		}
	}
	// A result is a new trigger for its requester, even without an explicit @.
	// Merge with ordinary mentions and other results before applying max_chain.
	for _, r := range rs.requests {
		if r.ResultMsg != msg.ID || (r.Status != "done" && r.Status != "rejected") || slices.Contains(targets, r.From) {
			continue
		}
		if p := rs.room.participant(r.From); p != nil && p.Enabled && !p.Removed && p.ID != msg.Author {
			targets = append(targets, p.ID)
		}
	}
	if len(targets) == 0 {
		return
	}
	if t.Depth+1 > rs.room.MaxChain {
		h.addSystem(rs, fmt.Sprintf("@%s 가 %s 를 호출했지만 연쇄 한도(%d)에 도달해 멈췄어. 이어가려면 직접 호출해.", msg.Author, strings.Join(targets, ", "), rs.room.MaxChain))
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
		if !slices.Contains(d.Failed, agentID) {
			d.Failed = append(d.Failed, agentID)
		}
	} else {
		d.Signals[agentID] = signal
	}
	d.Done++
	rs.broadcast("discussion", rs.discView())
	if d.Done < d.Expected {
		return
	}
	concluded, allAgree := false, len(d.Signals) > 0
	for _, s := range d.Signals {
		if s == "CONCLUDE" {
			concluded = true
		}
		if s != "AGREE" && s != "PASS" {
			allAgree = false
		}
	}
	// Nobody has seen a peer's answer at the end of a blind round 1, so its
	// CONCLUDE/AGREE cannot end the discussion; the reveal round always runs.
	revealPending := d.Blind && d.Round == 1 && d.Round < d.Total && len(rs.room.enabled()) >= 2
	var ending string
	switch {
	case revealPending:
		d.Round++
		rs.enqueueRound(h.enabledIDs(rs))
	case concluded:
		ending = fmt.Sprintf("토론 종료 · %d라운드에서 결론 제시(CONCLUDE)", d.Round)
	case allAgree:
		who := "전원"
		if len(d.Failed) > 0 {
			who = "성공한 참가자"
		}
		ending = fmt.Sprintf("토론 종료 · %d라운드에서 %s 동의/패스 (의견 일치일 뿐 검증은 아님)", d.Round, who)
	case d.Round >= d.Total:
		ending = fmt.Sprintf("토론 종료 · 최대 %d라운드 도달", d.Total)
	case len(rs.room.enabled()) < 2:
		ending = "토론 종료 · 활성 참가자가 2명 미만이야"
	default:
		d.Round++
		rs.enqueueRound(h.enabledIDs(rs))
	}
	if ending != "" {
		for _, id := range d.Failed {
			ending += " · @" + id + " 실패로 제외"
		}
		h.addSystem(rs, ending)
		rs.disc = nil
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
		} else if m.Kind == "agent" {
			who = "@" + m.Author
			if p := snapshot.Room.participant(m.Author); p != nil {
				who += " (" + p.Label + ")"
			}
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

// workspaceMissing reports a room or seat folder that disappeared after the
// room was created (for example a removed git worktree), naming the folder
// instead of letting the CLI fail with an OS process-start error.
func workspaceMissing(o agent.RunOptions) error {
	if o.Mode == agent.ModeChat || o.Workspace == "" {
		return nil
	}
	if info, err := os.Stat(o.Workspace); err == nil && info.IsDir() {
		return nil
	}
	return fmt.Errorf("작업 폴더가 없어 실행하지 않았어: %s (지워졌거나 옮겨졌으면 같은 경로에 다시 만들거나 새 방을 만들어)", o.Workspace)
}
