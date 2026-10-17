package room

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"roundtable/internal/agent"
)

const (
	defaultMaxChain = 4
	defaultRounds   = 3
	turnTimeout     = 45 * time.Minute
)

// Hub owns every room, runs agent turns and fans events out to SSE clients.
type Hub struct {
	mu      sync.Mutex
	store   *Store
	reg     *agent.Registry
	scratch string
	rooms   map[string]*roomState
	limits  map[string]agent.Limits
	Log     *log.Logger
}

type roomState struct {
	room     *Room
	msgs     []*Message
	traces   map[string][]agent.Event
	queue    []*Turn
	running  map[string]*running
	subs     map[chan []byte]struct{}
	disc     *discussion
	stopGen  int // prevents a stopped turn from scheduling follow-up work
	resetGen int // bumped by ResetSessions so a finishing turn does not resurrect the old session
}

type running struct {
	turn    *Turn
	msg     *Message
	cancel  context.CancelFunc
	steer   chan string
	live    []*Message // accepted steers belonging to this invocation only
	stopGen int
}

type discussion struct {
	Total    int
	Round    int
	Expected int
	Done     int
	Signals  map[string]string
}

func NewHub(store *Store, reg *agent.Registry, scratch string, logger *log.Logger) (*Hub, error) {
	h := &Hub{store: store, reg: reg, scratch: scratch, rooms: map[string]*roomState{}, limits: map[string]agent.Limits{}, Log: logger}
	rooms, msgs, traces, err := store.LoadAll()
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
		rs := &roomState{room: r, msgs: msgs[r.ID], traces: map[string][]agent.Event{}, running: map[string]*running{}, subs: map[chan []byte]struct{}{}}
		for _, t := range traces[r.ID] {
			rs.traces[t.MsgID] = append(rs.traces[t.MsgID], t.Event)
		}
		for _, p := range r.Participants {
			p.Status, p.StatusText = "idle", ""
		}
		h.rooms[r.ID] = rs
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
	Title            string   `json:"title"`
	Workspace        string   `json:"workspace"`
	Mode             string   `json:"mode"`
	Note             string   `json:"note"`
	Participants     []string `json:"participants"`
	MaxChain         int      `json:"max_chain"`
	DiscussionRounds int      `json:"discussion_rounds"`
	Persona          bool     `json:"persona"`
}

func (h *Hub) CreateRoom(spec RoomSpec) (*Room, error) {
	if strings.TrimSpace(spec.Title) == "" {
		return nil, fmt.Errorf("방 이름이 비어 있음")
	}
	if spec.Mode == "" {
		spec.Mode = agent.ModeRead
	}
	if spec.Mode != agent.ModeChat && spec.Workspace == "" {
		return nil, fmt.Errorf("read/write 모드는 작업 폴더가 필요함")
	}
	r := &Room{ID: newID(), Title: spec.Title, CreatedAt: nowMs(), UpdatedAt: nowMs(), Workspace: spec.Workspace, Mode: spec.Mode, Note: spec.Note, MaxChain: spec.MaxChain, DiscussionRounds: spec.DiscussionRounds, Persona: spec.Persona}
	if r.MaxChain <= 0 {
		r.MaxChain = defaultMaxChain
	}
	if r.DiscussionRounds <= 0 {
		r.DiscussionRounds = defaultRounds
	}
	ids := spec.Participants
	if len(ids) == 0 {
		for _, p := range h.reg.All() {
			ids = append(ids, p.ID())
		}
	}
	for _, id := range ids {
		prov := h.reg.Get(id)
		if prov == nil {
			return nil, fmt.Errorf("알 수 없는 공급자: %s", id)
		}
		model, effort := defaultsFor(id)
		r.Participants = append(r.Participants, &Participant{ID: id, Label: prov.Label(), Enabled: prov.Available(), Model: model, Effort: effort, Status: "idle", ContextWindow: prov.ContextWindow(model)})
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rooms[r.ID] = &roomState{room: r, traces: map[string][]agent.Event{}, running: map[string]*running{}, subs: map[chan []byte]struct{}{}}
	return r, h.store.SaveRoom(r)
}

func defaultsFor(id string) (string, string) {
	switch id {
	case "claude":
		return "opus", "high"
	case "codex":
		return "gpt-6.1-sol", "high"
	case "agy":
		return "gemini-3.8-flash-high", ""
	}
	return "", ""
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
	return &Snapshot{Room: rs.room, Messages: rs.msgs, Queue: rs.queueView(), Discussion: rs.discView(), Limits: h.limitsCopy()}, nil
}

// SnapshotJSON serialises under the lock so a running turn cannot mutate what the encoder reads.
func (h *Hub) SnapshotJSON(id string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	return json.Marshal(&Snapshot{Room: rs.room, Messages: rs.msgs, Queue: rs.queueView(), Discussion: rs.discView(), Limits: h.limitsCopy()})
}

func (h *Hub) limitsCopy() map[string]agent.Limits {
	out := map[string]agent.Limits{}
	for k, v := range h.limits {
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
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	r := rs.room
	if patch.Title != nil && strings.TrimSpace(*patch.Title) != "" {
		r.Title = *patch.Title
	}
	if patch.Note != nil {
		r.Note = *patch.Note
	}
	if patch.Mode != nil {
		r.Mode = *patch.Mode
	}
	if patch.Workspace != nil {
		r.Workspace = *patch.Workspace
	}
	if patch.MaxChain != nil && *patch.MaxChain > 0 {
		r.MaxChain = *patch.MaxChain
	}
	if patch.DiscussionRounds != nil && *patch.DiscussionRounds > 0 {
		r.DiscussionRounds = *patch.DiscussionRounds
	}
	if patch.Persona != nil {
		r.Persona = *patch.Persona
	}
	for _, pp := range patch.Participants {
		p := r.participant(pp.ID)
		if p == nil {
			continue
		}
		if pp.Enabled != nil {
			p.Enabled = *pp.Enabled
			if !p.Enabled {
				rs.dropQueuedFor(h, p.ID)
			}
		}
		if pp.Model != nil && *pp.Model != "" {
			p.Model = *pp.Model
			if prov := h.reg.Get(p.ID); prov != nil {
				p.ContextWindow = prov.ContextWindow(p.Model)
			}
		}
		if pp.Effort != nil {
			p.Effort = *pp.Effort
		}
	}
	r.UpdatedAt = nowMs()
	if err := h.store.SaveRoom(r); err != nil {
		return nil, err
	}
	rs.broadcast("room", r)
	h.pump(rs)
	return json.Marshal(r)
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
	}
	if p := rs.room.participant(agentID); p != nil && p.Status == "queued" {
		p.Status = "idle"
		rs.broadcast("participant", p)
	}
	rs.broadcast("queue", rs.queueView())
}

// ResetSessions forgets the CLI sessions so every agent starts with a fresh context.
func (h *Hub) ResetSessions(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return fmt.Errorf("방 없음: %s", id)
	}
	for _, p := range rs.room.Participants {
		p.SessionID, p.LastSeen, p.ContextTokens = "", 0, 0
		p.Unseen = nil
	}
	rs.resetGen++
	for _, t := range rs.queue {
		t.input = nil // the next session must read a fresh group snapshot
	}
	rs.addSystem("세션을 초기화했어. 다음 턴부터 각 에이전트는 대화 전체를 새로 읽어.")
	rs.broadcast("room", rs.room)
	return h.store.SaveRoom(rs.room)
}

func (h *Hub) DeleteRoom(id string) error {
	h.mu.Lock()
	rs, ok := h.rooms[id]
	if !ok {
		h.mu.Unlock()
		return fmt.Errorf("방 없음: %s", id)
	}
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
	b, err := json.Marshal(map[string]any{"type": typ, "data": payload})
	if err != nil {
		return
	}
	for ch := range rs.subs {
		select {
		case ch <- b:
		default: // slow client: drop; it re-syncs from the snapshot
		}
	}
}

func (h *Hub) Trace(id, msgID string) []agent.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil
	}
	out := make([]agent.Event, len(rs.traces[msgID]))
	copy(out, rs.traces[msgID])
	return out
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
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	if target != "" && rs.room.participant(target) == nil {
		return nil, fmt.Errorf("대상 없음: %s", target)
	}
	m := rs.nextMessage("user", "user", text)
	if target != "" {
		m.Target = target
		if run := rs.runningFor(target); run != nil && run.steer != nil {
			select {
			case run.steer <- text:
				m.Live = true
				run.live = append(run.live, m)
			default:
			}
		}
		rs.broadcast("message", m)
		_ = h.store.AppendMessage(id, m)
		_ = h.store.SaveRoom(rs.room)
		if !m.Live {
			rs.enqueueFront(&Turn{ID: newID(), Agent: target, Reason: "mention", Group: m.ID, Created: nowMs(), Status: "queued"})
			h.pump(rs)
		}
		mc := *m
		return &mc, nil
	}
	rs.broadcast("message", m)
	_ = h.store.AppendMessage(id, m)
	_ = h.store.SaveRoom(rs.room)
	ids := h.enabledIDs(rs)
	var all []string
	for _, p := range rs.room.Participants {
		all = append(all, p.ID)
	}
	mentioned := ParseMentions(text, all)
	var targets []string
	for _, id := range mentioned {
		if slices.Contains(ids, id) {
			targets = append(targets, id)
		}
	}
	reason := "mention"
	switch {
	case len(mentioned) > 0 && len(targets) == 0:
		rs.addSystem(fmt.Sprintf("@%s 는 꺼져 있어서 아무도 답하지 않아. 오른쪽 패널에서 켜거나 다른 참가자를 불러.", strings.Join(mentioned, ", @")))
		mc := *m
		return &mc, nil
	case len(mentioned) == 0:
		targets, reason = ids, "broadcast"
	}
	for _, t := range targets {
		rs.enqueue(&Turn{ID: newID(), Agent: t, Reason: reason, Group: m.ID, Created: nowMs(), Status: "queued"})
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
	if p := rs.room.participant(t.Agent); p != nil && p.Status == "idle" {
		p.Status = "queued"
		rs.broadcast("participant", p)
	}
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
	if p := rs.room.participant(t.Agent); p != nil && p.Status == "idle" {
		p.Status = "queued"
		rs.broadcast("participant", p)
	}
	rs.broadcast("queue", rs.queueView())
}

// StartDiscussion runs up to `rounds` sequential rounds over every enabled agent.
func (h *Hub) StartDiscussion(id string, rounds int) error {
	h.mu.Lock()
	defer h.mu.Unlock()
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
	for _, p := range rs.room.Participants {
		if p.Status == "queued" {
			p.Status = "idle"
			rs.broadcast("participant", p)
		}
	}
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
		return nil
	}
	for i, t := range rs.queue {
		if t.ID == turnID {
			rs.queue = append(rs.queue[:i], rs.queue[i+1:]...)
			if t.Reason == "discussion" {
				h.discussionStep(rs, t.Agent, "PASS", "cancelled")
			}
			if p := rs.room.participant(t.Agent); p != nil && p.Status == "queued" && rs.runningFor(t.Agent) == nil {
				p.Status = "idle"
				rs.broadcast("participant", p)
			}
			rs.broadcast("queue", rs.queueView())
			h.pump(rs)
			return nil
		}
	}
	return fmt.Errorf("턴 없음: %s", turnID)
}

// ---- turn execution --------------------------------------------------------

// turnInput freezes a group's conversation before any member opens a reply.
// Busy members keep this same snapshot until their own participant is free.
type turnInput struct {
	messages []*Message
	parallel bool
}

// pump schedules every runnable participant. Caller holds h.mu; reserving the
// participant and building every group member's prompt happen under this lock.
func (h *Hub) pump(rs *roomState) {
	inputs := map[string]*turnInput{}
	for _, t := range rs.queue {
		if t.input != nil {
			inputs[t.Group] = t.input
		}
	}
	for _, t := range rs.queue {
		if t.input != nil || t.Reason == "discussion" {
			continue
		}
		input := inputs[t.Group]
		if input == nil {
			input = &turnInput{}
			for _, m := range rs.msgs {
				copy := *m
				input.messages = append(input.messages, &copy)
			}
			inputs[t.Group] = input
		}
		t.input = input
	}
	counts := map[*turnInput]int{}
	for _, t := range rs.queue {
		counts[t.input]++
	}
	// Render all members together, including those still waiting for their own
	// previous turn. A waiting member is rendered again against the frozen
	// conversation when its session/delivery state has settled.
	drafts := map[*Turn]*prepared{}
	for _, t := range rs.queue {
		if t.Reason != "discussion" {
			if counts[t.input] > 1 {
				t.input.parallel = true
			}
			drafts[t] = h.buildTurn(rs, t, t.input.messages, t.input.parallel)
		}
	}
	blocked := map[string]bool{}
	discussionBlocked := false
	for _, run := range rs.running {
		if run.turn.Reason == "discussion" && run.stopGen == rs.stopGen {
			discussionBlocked = true
		}
	}
	for i := 0; i < len(rs.queue); {
		t := rs.queue[i]
		if blocked[t.Agent] || rs.runningFor(t.Agent) != nil ||
			(t.Reason == "discussion" && discussionBlocked) {
			blocked[t.Agent] = true
			if t.Reason == "discussion" {
				discussionBlocked = true
			}
			i++
			continue
		}
		rs.queue = slices.Delete(rs.queue, i, i+1)
		pr := drafts[t]
		if t.Reason == "discussion" {
			// Discussion history must include the preceding speaker's answer.
			pr = h.buildTurn(rs, t, rs.msgs, false)
		}
		if pr == nil {
			if t.Reason == "discussion" {
				h.discussionStep(rs, t.Agent, "PASS", "cancelled")
			}
			continue
		}
		h.prepareTurn(rs, pr)
		if t.Reason == "discussion" {
			discussionBlocked = true
		}
		go h.runTurn(rs, pr)
	}
}

// prepared is a turn whose prompt was built under the lock, so every member of
// a parallel batch sees the same snapshot of the conversation.
type prepared struct {
	t        *Turn
	p        *Participant
	seen     int64
	unseen   []int64
	stopGen  int
	resetGen int
	prov     agent.Provider
	msg      *Message
	opts     agent.RunOptions
	ctx      context.Context
	cancel   context.CancelFunc
}

// buildTurn renders a prompt without reserving a participant. Caller holds h.mu.
func (h *Hub) buildTurn(rs *roomState, t *Turn, messages []*Message, parallel bool) *prepared {
	p := rs.room.participant(t.Agent)
	prov := h.reg.Get(t.Agent)
	if p == nil || !p.Enabled || prov == nil {
		return nil
	}
	first := p.SessionID == ""
	hist, seen, unseen := rs.deltaFor(p, first, messages)
	tc := turnContext{Reason: t.Reason, Parallel: parallel && t.Reason != "discussion", Round: t.Round, FirstTurn: first}
	if rs.disc != nil && t.Reason == "discussion" {
		tc.TotalRounds = rs.disc.Total
	}
	prompt := BuildPrompt(rs.room, p, hist, tc)
	opts := agent.RunOptions{Prompt: prompt, SessionID: p.SessionID, Model: p.Model, Effort: p.Effort, Mode: rs.room.Mode, Workspace: rs.room.Workspace, ScratchDir: h.scratch, Timeout: turnTimeout}
	return &prepared{t: t, p: p, seen: seen, unseen: unseen, stopGen: rs.stopGen, resetGen: rs.resetGen, prov: prov, opts: opts}
}

// prepareTurn reserves one participant and opens its reply. Caller holds h.mu.
func (h *Hub) prepareTurn(rs *roomState, pr *prepared) {
	t, p, prov := pr.t, pr.p, pr.prov
	msg := rs.nextMessage("agent", p.ID, "")
	msg.Status, msg.TurnID, msg.Reason, msg.Round, msg.Model = "streaming", t.ID, t.Reason, t.Round, p.Model
	ctx, cancel := context.WithTimeout(context.Background(), turnTimeout)
	var steer chan string
	if prov.CanSteer() {
		steer = make(chan string, 8)
	}
	t.Status = "running"
	rs.running[t.ID] = &running{turn: t, msg: msg, cancel: cancel, steer: steer, stopGen: rs.stopGen}
	p.Status, p.StatusText = "running", "생각 중"
	rs.broadcast("message", msg)
	rs.broadcast("participant", p)
	rs.broadcast("queue", rs.queueView())
	pr.opts.Steer = steer
	pr.msg, pr.ctx, pr.cancel = msg, ctx, cancel
}

func (h *Hub) runTurn(rs *roomState, pr *prepared) {
	t, p, prov, msg, opts, ctx, cancel := pr.t, pr.p, pr.prov, pr.msg, pr.opts, pr.ctx, pr.cancel
	started := time.Now()
	var textBuf strings.Builder
	emit := func(e agent.Event) {
		h.mu.Lock()
		defer h.mu.Unlock()
		rs.traces[msg.ID] = append(rs.traces[msg.ID], e)
		msg.Trace++
		_ = h.store.AppendTrace(rs.room.ID, TraceLine{MsgID: msg.ID, Event: e})
		switch e.Kind {
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
				h.limits[raw.Provider] = raw
				_ = h.store.SaveLimits(h.limits)
				rs.broadcast("limits", h.limitsCopy())
			}
		case agent.KindUsage:
			applyUsage(p, prov, e.Meta)
		}
		rs.broadcast("trace", map[string]any{"msg_id": msg.ID, "event": e})
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
	run := rs.running[t.ID]
	delete(rs.running, t.ID)
	msg.Duration = time.Since(started).Milliseconds()
	msg.Usage = res.Usage
	if res.SessionID != "" && pr.resetGen == rs.resetGen {
		p.SessionID = res.SessionID
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
	msg.Text = text
	switch {
	case interrupted:
		msg.Status, msg.Error = "cancelled", "중단됨"
	case err != nil:
		msg.Status, msg.Error = "error", err.Error()
	default:
		msg.Status = "done"
	}
	if msg.Status == "done" && pr.resetGen == rs.resetGen {
		p.LastSeen, p.Unseen = pr.seen, pr.unseen
	}
	// Only this invocation's steers count against Result.Steered. Earlier
	// accepted steers may still lie above this turn's delivery boundary.
	for i, m := range run.live {
		if msg.Status != "done" || i >= res.Steered {
			m.Live = false
		}
	}
	p.Turns++
	p.Status, p.StatusText = "idle", ""
	if rs.runningFor(p.ID) != nil || rs.queuedFor(p.ID) {
		p.Status = "queued"
	}
	rs.room.UpdatedAt = nowMs()
	_ = h.store.AppendMessage(rs.room.ID, msg)
	_ = h.store.SaveRoom(rs.room)
	rs.broadcast("message", msg)
	rs.broadcast("participant", p)
	rs.broadcast("queue", rs.queueView())
	if h.Log != nil {
		h.Log.Printf("turn %s %s/%s status=%s %dms err=%v", rs.room.ID, p.ID, p.Model, msg.Status, msg.Duration, err)
	}

	if pr.stopGen == rs.stopGen && msg.Status == "done" && t.Reason != "discussion" {
		h.chainMentions(rs, msg, t)
	}
	if pr.stopGen == rs.stopGen && t.Reason == "discussion" {
		h.discussionStep(rs, p.ID, signal, msg.Status)
	}
	h.pump(rs)
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
		p.ContextTokens = int(c)
	}
	if w := num("context_window"); w > 0 {
		p.ContextWindow = int(w)
	} else if p.ContextWindow == 0 {
		p.ContextWindow = prov.ContextWindow(p.Model)
	}
}

func (rs *roomState) queuedFor(agentID string) bool {
	for _, t := range rs.queue {
		if t.Agent == agentID {
			return true
		}
	}
	return false
}

// deltaFor reads past unfinished replies without losing them. Unseen retains
// holes below LastSeen; delivery is committed only after a successful turn.
func (rs *roomState) deltaFor(p *Participant, first bool, messages []*Message) ([]*Message, int64, []int64) {
	last := p.LastSeen
	if first {
		last = 0
	}
	seen := last
	var unseen []int64
	var out []*Message
	var snapshotEnd int64
	if len(messages) > 0 {
		snapshotEnd = messages[len(messages)-1].Seq
	}
	// An intervening discussion may have advanced delivery beyond this frozen
	// snapshot. Keep any later holes until a snapshot can actually include them.
	for _, seq := range p.Unseen {
		if seq > snapshotEnd {
			unseen = append(unseen, seq)
		}
	}
	live := map[int64]bool{}
	for _, m := range rs.msgs {
		if m.Live {
			live[m.Seq] = true
		}
	}
	for _, m := range messages {
		if m.Seq <= last && !slices.Contains(p.Unseen, m.Seq) {
			continue
		}
		if m.Seq > seen {
			seen = m.Seq
		}
		if m.Status == "streaming" {
			unseen = append(unseen, m.Seq)
			continue
		}
		// Live acknowledgement can settle after the group snapshot was frozen.
		if m.Author == p.ID || (m.Live && live[m.Seq]) {
			continue
		}
		if m.Target != "" && m.Target != p.ID {
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
	ids := h.enabledIDs(rs)
	var others []string
	for _, id := range ids {
		if id != msg.Author {
			others = append(others, id)
		}
	}
	targets := ParseMentions(msg.Text, others)
	if len(targets) == 0 {
		return
	}
	if t.Depth+1 > rs.room.MaxChain {
		rs.addSystem(fmt.Sprintf("@%s 가 %s 를 호출했지만 연쇄 한도(%d)에 도달해 멈췄어. 이어가려면 직접 호출해.", msg.Author, strings.Join(targets, ", "), rs.room.MaxChain))
		return
	}
	for _, id := range targets {
		rs.enqueue(&Turn{ID: newID(), Agent: id, Reason: "mention", Group: msg.ID, Depth: t.Depth + 1, Created: nowMs(), Status: "queued"})
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
	default:
		d.Round++
		rs.enqueueRound(h.enabledIDs(rs))
	}
	rs.broadcast("discussion", rs.discView())
}

// ---- export ----------------------------------------------------------------

func (h *Hub) ExportMarkdown(id string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return "", fmt.Errorf("방 없음: %s", id)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", rs.room.Title)
	fmt.Fprintf(&b, "- 작업 폴더: %s\n- 모드: %s\n- 내보낸 시각: %s\n\n", rs.room.Workspace, rs.room.Mode, time.Now().Format(time.RFC3339))
	for _, m := range rs.msgs {
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
