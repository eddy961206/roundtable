package room

import (
	"context"
	"fmt"
	"strings"
	"time"

	"roundtable/internal/agent"
)

// Community spaces (docs/사내에이전트_커뮤니티_설계.md): rooms where seats of
// several people's agents work (workroom) or chat (lounge) while people assign
// tasks, approve results and watch. Every seat turn costs its owner tokens, so
// a space starts with conservative speak guards and input budgets.
const (
	KindWork   = "work"
	KindLounge = "lounge"

	// DefaultSpeakInterval is the 2026-11-02 proposal: one reply per seat in
	// five minutes, for turns another seat started.
	DefaultSpeakInterval = 300
	maxSpeakInterval     = 24 * 60 * 60
	maxDailySpeakLimit   = 1000
	speakWindow          = 24 * time.Hour
)

// spaceDefaults are the values a new space gets for settings its spec leaves 0.
type spaceDefaults struct {
	mode                      string
	persona                   bool
	maxChain, interval, daily int
	seatBudget, roomBudget    int64
}

var spaceKinds = map[string]spaceDefaults{
	// A lounge is talk only: no project folder, short replies, a shallow chain.
	KindLounge: {mode: agent.ModeChat, persona: true, maxChain: 2, interval: DefaultSpeakInterval, daily: 12, seatBudget: 1_000_000, roomBudget: 5_000_000},
	KindWork:   {maxChain: defaultMaxChain, interval: DefaultSpeakInterval, daily: 24, seatBudget: 5_000_000, roomBudget: 20_000_000},
}

// SpaceKinds is what the page shows and prefills for a new space.
func SpaceKinds() map[string]map[string]any {
	out := map[string]map[string]any{}
	for kind, d := range spaceKinds {
		mode := d.mode
		if mode == "" {
			mode = agent.ModeRead
		}
		out[kind] = map[string]any{"mode": mode, "persona": d.persona, "max_chain": d.maxChain, "speak_interval_seconds": d.interval, "daily_speak_limit": d.daily, "seat_input_budget": d.seatBudget, "room_input_budget": d.roomBudget}
	}
	return out
}

// applySpaceDefaults fills a space's unset guards. An ordinary room keeps no guard.
func applySpaceDefaults(r *Room, spec RoomSpec) error {
	if spec.Kind == "" {
		return nil
	}
	d, ok := spaceKinds[spec.Kind]
	if !ok {
		return fmt.Errorf("알 수 없는 공간 종류: %s (work 또는 lounge)", spec.Kind)
	}
	r.Kind = spec.Kind // the mode default is applied with the other mode checks in CreateRoom
	if d.persona {
		r.Persona = true
	}
	if spec.MaxChain == 0 {
		r.MaxChain = d.maxChain
	}
	if spec.SpeakIntervalSeconds == 0 {
		r.SpeakIntervalSeconds = d.interval
	}
	if spec.DailySpeakLimit == 0 {
		r.DailySpeakLimit = d.daily
	}
	r.SeatInputBudget, r.RoomInputBudget = d.seatBudget, d.roomBudget
	return nil
}

func validateSpeakGuards(r *Room) error {
	if r.SpeakIntervalSeconds < 0 || r.SpeakIntervalSeconds > maxSpeakInterval {
		return fmt.Errorf("speak_interval_seconds는 0(없음)~%d초여야 함", maxSpeakInterval)
	}
	if r.DailySpeakLimit < 0 || r.DailySpeakLimit > maxDailySpeakLimit {
		return fmt.Errorf("daily_speak_limit는 0(없음)~%d건이어야 함", maxDailySpeakLimit)
	}
	if m := r.AutoChatMinutes; m != 0 && (m < minAutoChat || m > maxAutoChat) {
		return fmt.Errorf("auto_chat_minutes는 0(끔) 또는 %d~%d분이어야 함", minAutoChat, maxAutoChat)
	}
	if _, _, err := chatHours(r.AutoChatHours); err != nil {
		return err
	}
	return nil
}

// ---------- auto chat ----------
// A lounge where people only watch needs someone to speak. Auto chat is off
// until a person turns it on for the space; then, whenever the space has been
// quiet for AutoChatMinutes within AutoChatHours, the seat that spoke least
// recently says a line. It obeys the pause, the daily limit, the speak
// interval and the input budgets.
//
// Seats talk among themselves a little (2026-11-03, "구경하는 맛"): an auto
// line may call one other seat with @, and that seat answers once
// ("auto-reply", paced like any called turn). The answer calls nobody, so a
// quiet interval costs at most two turns per space.
//
// Lines nobody answers cost tokens for nothing (review 2026-11-03, after AI
// Town, which stops its world without viewers): auto chat speaks only while
// someone has the space open, each unanswered line doubles the quiet time
// before the next, and after autoChatStreak of them it waits for a person or
// a called turn. The seats' own answers to auto lines do not count as an
// answer: only a person or a called turn ends the streak. Auto lines and
// their answers use at most half of a seat's daily limit, so a person can
// still call the seat.
const (
	minAutoChat     = 10
	maxAutoChat     = 24 * 60
	autoChatStreak  = 3
	autoChatHistory = 12 // newest messages an auto chat turn reads
	// AutoChatPrompt follows the new messages of an auto chat turn.
	AutoChatPrompt = "(자율 잡담) 사람이 부르지 않았지만 공간이 조용해서 당신 차례가 왔습니다. 위 대화에 이어 이 공간의 규칙대로 짧게 말하세요. 직전 잡담과 같은 화제는 되풀이하지 마세요. 다른 참가자에게 묻거나 농담을 걸고 싶으면 한 명만 @좌석ID로 부르세요. 그 참가자가 한 번 답합니다. 일은 맡기지 마세요. 새로 할 말이 없으면 \"커피 마시는 중\"처럼 한마디만 하세요."
	// autoReplyPrompt follows the new messages of a seat an auto line called.
	autoReplyPrompt = "(자율 대화) @%s가 잡담 중에 당신에게 말을 걸었습니다. 이 공간의 규칙대로 짧게 답하세요. 이번 턴에는 다른 참가자를 부르거나 일을 맡기지 마세요."
)

// autoChain lets an auto line's first @mention answer once. Caller holds h.mu.
func (h *Hub) autoChain(rs *roomState, msg *Message, t *Turn) {
	if t.Depth+1 > rs.room.MaxChain {
		return
	}
	for _, id := range msg.mentionIDs(rs.room.Participants) {
		if p := rs.room.participant(id); p != nil && p.Enabled && !p.Removed && p.ID != msg.Author {
			rs.enqueue(&Turn{ID: newID(), Agent: id, Reason: "auto-reply", Group: msg.ID, Cap: msg.Seq, Depth: t.Depth + 1, Created: nowMs(), Status: "queued", prompt: fmt.Sprintf(autoReplyPrompt, msg.Author)})
			return
		}
	}
}

// autoReason tells a reply that auto chat started (a line or its answer).
func autoReason(reason string) bool { return reason == "auto" || reason == "auto-reply" }

// chatHours reads "9-18" (from 9:00 to before 18:00); "" is any time.
func chatHours(spec string) (from, to int, err error) {
	if spec == "" {
		return 0, 24, nil
	}
	if _, err := fmt.Sscanf(spec, "%d-%d", &from, &to); err != nil || from < 0 || to > 24 || from >= to {
		return 0, 0, fmt.Errorf("auto_chat_hours는 \"9-18\"처럼 시작-끝 시(0~24, 시작<끝)여야 함: %q", spec)
	}
	return from, to, nil
}

// AutoChatTick starts at most one auto chat turn in each quiet space.
func (h *Hub) AutoChatTick(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return
	}
	for _, rs := range h.rooms {
		r := rs.room
		from, to, _ := chatHours(r.AutoChatHours)
		if r.AutoChatMinutes == 0 || r.AgentsPaused || len(rs.subs) == 0 || len(rs.running) > 0 || len(rs.queue) > 0 || now.Hour() < from || now.Hour() >= to {
			continue
		}
		streak := rs.autoStreak()
		if streak >= autoChatStreak {
			continue
		}
		last := r.CreatedAt
		if n := len(rs.msgs); n > 0 {
			last = rs.msgs[n-1].TS
		}
		if now.UnixMilli()-last < int64(r.AutoChatMinutes)*60_000<<streak {
			continue
		}
		if p := h.autoChatSeat(rs, now.UnixMilli()); p != nil {
			rs.enqueue(&Turn{ID: newID(), Agent: p.ID, Reason: "auto", Group: newID(), Cap: r.Seq, Depth: 1, Created: nowMs(), Status: "queued", prompt: AutoChatPrompt})
			h.pump(rs)
		}
	}
}

// autoChatSeat is the seat that may speak and spoke least recently, or nil.
// Caller holds h.mu.
func (h *Hub) autoChatSeat(rs *roomState, now int64) *Participant {
	var pick *Participant
	var pickAt int64
	for _, p := range rs.room.enabled() {
		// A seat with a part at work would get the line in its part session.
		if seatHeld(p) || rs.budgetHold(p) != "" || rs.speakHold(p) != "" || rs.activePart(p.ID) != nil || (p.Provider == RemoteProvider && !h.remoteOnline(rs, p)) {
			continue
		}
		if limit := rs.room.DailySpeakLimit; limit > 0 && rs.autoRepliesSince(p.ID, now-24*time.Hour.Milliseconds())*2 >= limit {
			continue
		}
		last := rs.lastReplyAt(p.ID)
		if gap := int64(rs.room.SpeakIntervalSeconds) * 1000; last > 0 && now-last < gap {
			continue
		}
		if pick == nil || last < pickAt {
			pick, pickAt = p, last
		}
	}
	return pick
}

// RunAutoChat checks every space each minute until ctx ends.
func (h *Hub) RunAutoChat(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			h.AutoChatTick(now)
		}
	}
}

// autoStreak counts the auto chat lines at the end of the space, with no
// other reply or person's message after them.
func (rs *roomState) autoStreak() int {
	n := 0
	for i := len(rs.msgs) - 1; i >= 0; i-- {
		m := rs.msgs[i]
		switch {
		case m.Kind == "agent" && m.Reason == "auto":
			n++
		case m.Kind == "agent" && m.Reason == "auto-reply": // a seat's own answer is not an answer from people
		case m.Kind == "agent" || m.Kind == "user":
			return n
		}
	}
	return n
}

// autoRepliesSince counts the seat's auto chat lines started at or after since (ms).
func (rs *roomState) autoRepliesSince(seat string, since int64) int {
	n := 0
	for i := len(rs.msgs) - 1; i >= 0 && rs.msgs[i].TS >= since; i-- {
		if m := rs.msgs[i]; m.Kind == "agent" && m.Author == seat && autoReason(m.Reason) {
			n++
		}
	}
	return n
}

// lastReplyAt is when the seat's latest reply started (ms); 0 when it has none.
func (rs *roomState) lastReplyAt(seat string) int64 {
	for i := len(rs.msgs) - 1; i >= 0; i-- {
		if m := rs.msgs[i]; m.Kind == "agent" && m.Author == seat {
			return m.TS
		}
	}
	return 0
}

// repliesSince counts the seat's replies started at or after since (ms). A
// reply cancelled before any output cost nothing and is not counted.
func (rs *roomState) repliesSince(seat string, since int64) int {
	n := 0
	for i := len(rs.msgs) - 1; i >= 0 && rs.msgs[i].TS >= since; i-- {
		m := rs.msgs[i]
		if m.Kind == "agent" && m.Author == seat && !(m.Status == "cancelled" && m.Text == "") {
			n++
		}
	}
	return n
}

// paceUntil is when a turn may start under the speak interval (ms), 0 when
// now. Only turns another seat started wait: a person's message (depth 0), a
// discussion and work-plan turns run as before.
func (rs *roomState) paceUntil(p *Participant, t *Turn) int64 {
	interval := rs.room.SpeakIntervalSeconds
	if interval == 0 || t.Depth == 0 || (t.Reason != "mention" && t.Reason != "auto-reply") {
		return 0
	}
	last := rs.lastReplyAt(p.ID)
	if until := last + int64(interval)*1000; last > 0 && nowMs() < until {
		return until
	}
	return 0
}

// speakHold tells why the seat may not start any turn now, "" when it may.
// The held turn is dropped like a budget hold; its messages stay unseen.
func (rs *roomState) speakHold(p *Participant) string {
	if rs.room.AgentsPaused {
		return "이 공간의 에이전트 발화가 꺼져 있음"
	}
	if limit := rs.room.DailySpeakLimit; limit > 0 && rs.repliesSince(p.ID, nowMs()-speakWindow.Milliseconds()) >= limit {
		return fmt.Sprintf("좌석의 24시간 발화가 상한 %d건에 도달", limit)
	}
	return ""
}

// armPace pumps the room when the earliest paced turn may start. One timer per
// room: pump runs often and must not stack timers. Caller holds h.mu.
func (h *Hub) armPace(rs *roomState, until int64) {
	if rs.paceTimer != nil && rs.paceAt <= until && rs.paceAt > nowMs() {
		return
	}
	if rs.paceTimer != nil {
		rs.paceTimer.Stop()
	}
	rs.paceAt = until
	rs.paceTimer = time.AfterFunc(time.Until(time.UnixMilli(until))+50*time.Millisecond, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closing || h.rooms[rs.room.ID] != rs {
			return
		}
		rs.paceTimer = nil
		h.pump(rs)
	})
}

// spaceBrief is the community part of a seat's standing brief.
func spaceBrief(r *Room) string {
	var b strings.Builder
	switch r.Kind {
	case KindLounge:
		b.WriteString("- 이곳은 여러 사람의 AI 에이전트가 모인 사내 휴게실입니다. 가볍게 잡담하되 두세 문장으로 짧게 답하세요. 일을 맡거나 다른 참가자를 부르는 것은 꼭 필요할 때만 하세요.\n")
	case KindWork:
		b.WriteString("- 이곳은 여러 사람의 AI 에이전트가 함께 일하는 사내 작업실입니다. 사람이 맡긴 과제(요청 from @user)를 끝내면 응답 끝 신호 블록에 [[REQUEST_DONE:요청ID]]를 쓰세요. 결과는 사람이 승인해야 확정됩니다.\n")
	default:
		return ""
	}
	if r.SpeakIntervalSeconds > 0 || r.DailySpeakLimit > 0 {
		b.WriteString("- 토큰을 아끼려고 발화 수를 제한합니다.")
		if r.SpeakIntervalSeconds > 0 {
			fmt.Fprintf(&b, " 다른 참가자가 부른 턴은 좌석마다 %s에 한 번만 시작합니다.", speakIntervalText(r.SpeakIntervalSeconds))
		}
		if r.DailySpeakLimit > 0 {
			fmt.Fprintf(&b, " 좌석마다 24시간에 %d번까지 답합니다.", r.DailySpeakLimit)
		}
		b.WriteString(" 호출은 아껴 쓰세요.\n")
	}
	return b.String()
}

func speakIntervalText(seconds int) string {
	if seconds%60 == 0 {
		return fmt.Sprintf("%d분", seconds/60)
	}
	return fmt.Sprintf("%d초", seconds)
}

// AssignTask is a person's task for one seat: a message addressed to the seat
// and a request from "user" the seat closes with [[REQUEST_DONE]]. The done
// result becomes a proposed decision card the person confirms or drops.
func (h *Hub) AssignTask(roomID, to, text string) (*Message, Request, error) {
	return h.AssignTaskAs(roomID, to, text, "")
}

// AssignTaskAs is a task from a person on another PC (people.go).
func (h *Hub) AssignTaskAs(roomID, to, text, by string) (*Message, Request, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, Request{}, fmt.Errorf("과제 내용이 비어 있음")
	}
	if strings.ContainsAny(text, "\r\n") {
		return nil, Request{}, fmt.Errorf("과제는 한 줄로 써야 함: 자세한 설명은 과제 뒤에 그 좌석에게 따로 보내")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return nil, Request{}, fmt.Errorf("서버가 종료 중이야")
	}
	rs, ok := h.rooms[roomID]
	if !ok {
		return nil, Request{}, fmt.Errorf("방 없음: %s", roomID)
	}
	if to == "" {
		return nil, Request{}, fmt.Errorf("과제를 맡을 좌석(to)이 필요함")
	}
	if part := rs.seatPart(to); part != nil {
		return nil, Request{}, fmt.Errorf("@%s은 파트 %s 작업 중이라 과제를 받을 수 없음", to, part.ID)
	}
	task := Request{ID: newID(), From: "user", To: to, Text: text, Status: "pending"}
	m, err := h.acceptUser(rs, "과제: "+text, to, nil, nil, &task, by)
	if err != nil {
		return nil, Request{}, err
	}
	return m, *rs.request(task.ID), nil
}

// taskResultCard asks the person to approve a finished task. Caller holds h.mu.
func (h *Hub) taskResultCard(rs *roomState, r Request, msg *Message) error {
	d := Decision{ID: newID(), Text: "과제 결과: " + clipText(r.Text, maxDecisionText-len([]rune("과제 결과: "))), Evidence: fmt.Sprintf("#%d @%s의 답", msg.Seq, msg.Author), By: msg.Author, SourceMsg: msg.ID, Status: "proposed", Task: r.ID, At: nowMs()}
	if err := h.saveDecision(rs, d); err != nil {
		return err
	}
	msg.Decisions = append(msg.Decisions, d.ID)
	return nil
}

func clipText(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
