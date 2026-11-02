package room

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"roundtable/internal/agent"
)

// Remote seats (docs/사내에이전트_커뮤니티_설계.md): the seat's turns are built
// and recorded here like any other seat, but a worker on another person's PC
// runs them with that person's own CLI and login. The worker polls for the
// seat's job with the seat's connection token and posts back the answer. The
// server never starts a process for the seat and never sees the CLI's login.
const (
	RemoteProvider    = "remote"
	remoteAwayReason  = "원격 좌석의 worker가 연결돼 있지 않음 · worker가 다시 연결되면 남은 메시지로 한 번 이어 답함"
	remoteOnlineGrace = 2 * time.Minute // a worker re-polls at once; a gap this long means it is gone
	// A turn whose job no worker takes in this time fails before any output,
	// so its messages are delivered by the seat's next turn.
	remoteClaimTimeout = 2 * time.Minute
	MaxRemoteWait      = 60 * time.Second // longest single job poll
	maxRemoteText      = 256 << 10
	remoteTokenPrefix  = "rtc_"
)

// ErrRemoteAuth is a missing, unknown or revoked connection token.
var ErrRemoteAuth = errors.New("원격 좌석 연결 토큰이 없거나 맞지 않음")

// remoteProvider registers the remote seat kind. Its turns never call Run: the
// hub hands them to the seat's worker (runRemote).
type remoteProvider struct{}

func NewRemoteProvider() agent.Provider { return remoteProvider{} }

func (remoteProvider) ID() string                        { return RemoteProvider }
func (remoteProvider) Label() string                     { return "원격 에이전트" }
func (remoteProvider) Command() string                   { return "" }
func (remoteProvider) Models() []agent.ModelOption       { return nil }
func (remoteProvider) ModelEfforts() map[string][]string { return nil }
func (remoteProvider) Efforts() []string                 { return nil }
func (remoteProvider) DefaultModel() (string, string)    { return "worker", "" }
func (remoteProvider) Available() bool                   { return true }
func (remoteProvider) CanSteer() bool                    { return false }
func (remoteProvider) CanFork() bool                     { return false }
func (remoteProvider) ContextWindow(string) int          { return 0 }
func (remoteProvider) Run(context.Context, agent.RunOptions, func(agent.Event)) (agent.Result, error) {
	return agent.Result{}, errors.New("원격 좌석의 턴은 worker가 실행함")
}

// RemoteJob is one seat turn handed to a worker.
type RemoteJob struct {
	Turn      string `json:"turn"`
	Room      string `json:"room"`
	RoomTitle string `json:"room_title"`
	Seat      string `json:"seat"`
	Label     string `json:"label"`
	Prompt    string `json:"prompt"`
	SessionID string `json:"session_id"` // the worker's CLI session this seat resumes; "" starts one
	Mode      string `json:"mode"`       // the room's mode; the worker never exceeds its own -mode
	// TimeoutSeconds is how long the worker may run: the room's turn limit
	// less a minute to post the answer before this server gives up.
	TimeoutSeconds int `json:"timeout_seconds"`
	done           chan RemoteResult
}

// RemoteResult is what the worker reports for a job.
type RemoteResult struct {
	Text      string         `json:"text"`
	SessionID string         `json:"session_id"`
	Usage     map[string]any `json:"usage"`
	Error     string         `json:"error"`
}

type remoteSlot struct {
	job     *RemoteJob
	claimed bool
	polling int
	wake    chan struct{} // closed when a job is offered
}

type remoteToken struct {
	Room string `json:"room"`
	Seat string `json:"seat"`
	At   int64  `json:"at"`
}

func remoteKey(room, seat string) string { return room + "/" + seat }

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (s *Store) remoteTokensPath() string { return filepath.Join(s.root, "remote_tokens.json") }

func (s *Store) loadRemoteTokens() (map[string]remoteToken, error) {
	out := map[string]remoteToken{}
	b, err := os.ReadFile(s.remoteTokensPath())
	if os.IsNotExist(err) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	return out, json.Unmarshal(b, &out)
}

func (s *Store) saveRemoteTokens(tokens map[string]remoteToken) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := json.MarshalIndent(tokens, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.remoteTokensPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.remoteTokensPath())
}

func (h *Hub) slot(key string) *remoteSlot {
	sl := h.remote[key]
	if sl == nil {
		sl = &remoteSlot{wake: make(chan struct{})}
		h.remote[key] = sl
	}
	return sl
}

// remoteOnline tells whether the seat's worker is polling or polled recently.
// Caller holds h.mu.
func (h *Hub) remoteOnline(rs *roomState, p *Participant) bool {
	sl := h.remote[remoteKey(rs.room.ID, p.ID)]
	if sl != nil && (sl.polling > 0 || sl.job != nil) {
		return true
	}
	return p.RemoteSeenAt > 0 && nowMs()-p.RemoteSeenAt < remoteOnlineGrace.Milliseconds()
}

// IssueRemoteToken makes the seat's connection token, replacing any earlier
// one. Only its hash is stored; the caller shows the token once.
func (h *Hub) IssueRemoteToken(roomID, seat string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[roomID]
	if !ok {
		return "", fmt.Errorf("방 없음: %s", roomID)
	}
	p := rs.room.participant(seat)
	if p == nil || p.Removed {
		return "", fmt.Errorf("참가자 없음: %s", seat)
	}
	if p.Provider != RemoteProvider {
		return "", fmt.Errorf("@%s는 원격 좌석이 아님 (공급사 %s)", seat, p.Provider)
	}
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := remoteTokenPrefix + hex.EncodeToString(raw)
	next := map[string]remoteToken{}
	for hash, t := range h.remoteTokens {
		if t.Room != roomID || t.Seat != seat {
			next[hash] = t
		}
	}
	next[hashToken(token)] = remoteToken{Room: roomID, Seat: seat, At: nowMs()}
	if err := h.store.saveRemoteTokens(next); err != nil {
		return "", err
	}
	h.remoteTokens = next
	if !p.RemoteToken {
		p.RemoteToken = true
		rs.broadcast("participant", p)
		h.saveRoom(rs)
	}
	return token, nil
}

// remoteSeat resolves a token to its live remote seat. Caller holds h.mu.
func (h *Hub) remoteSeat(token string) (*roomState, *Participant, error) {
	t, ok := h.remoteTokens[hashToken(token)]
	if !ok {
		return nil, nil, ErrRemoteAuth
	}
	rs := h.rooms[t.Room]
	if rs == nil {
		return nil, nil, ErrRemoteAuth
	}
	p := rs.room.participant(t.Seat)
	if p == nil || p.Removed || p.Provider != RemoteProvider {
		return nil, nil, ErrRemoteAuth
	}
	return rs, p, nil
}

// RemoteSeatView is what a worker learns about its seat.
type RemoteSeatView struct {
	Room      string `json:"room"`
	RoomTitle string `json:"room_title"`
	Kind      string `json:"kind"`
	Mode      string `json:"mode"`
	Seat      string `json:"seat"`
	Label     string `json:"label"`
	Enabled   bool   `json:"enabled"`
}

func (h *Hub) RemoteMe(token string) (RemoteSeatView, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, p, err := h.remoteSeat(token)
	if err != nil {
		return RemoteSeatView{}, err
	}
	return RemoteSeatView{Room: rs.room.ID, RoomTitle: rs.room.Title, Kind: rs.room.Kind, Mode: rs.room.Mode, Seat: p.ID, Label: p.Label, Enabled: p.Enabled}, nil
}

// claim hands the slot's job to the polling worker. A job already claimed is
// handed again: its worker restarted while it ran. Caller holds h.mu.
func (sl *remoteSlot) claim() *RemoteJob {
	if sl.job == nil {
		return nil
	}
	sl.claimed = true
	job := *sl.job
	job.done = nil
	return &job
}

// RemoteNext is one worker poll: the seat's job, or nil after wait. A poll
// marks the worker online, and the first poll after turns were dropped while
// it was away queues one catch-up turn for the messages it missed.
func (h *Hub) RemoteNext(ctx context.Context, token string, wait time.Duration) (*RemoteJob, error) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return nil, fmt.Errorf("서버가 종료 중이야")
	}
	rs, p, err := h.remoteSeat(token)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	key := remoteKey(rs.room.ID, p.ID)
	sl := h.slot(key)
	h.remoteSeen(rs, p)
	if job := sl.claim(); job != nil {
		h.mu.Unlock()
		return job, nil
	}
	if p.RemoteMissed && p.Enabled && rs.runningFor(p.ID) == nil && !rs.queued(p.ID) {
		p.RemoteMissed = false
		// Depth 1: the catch-up answers messages the seat was called by while
		// away, so the speak interval applies to it as to any chained turn.
		rs.enqueue(&Turn{ID: newID(), Agent: p.ID, Reason: "mention", Group: newID(), Cap: rs.room.Seq, Depth: 1, Created: nowMs(), Status: "queued"})
		h.pump(rs)
		h.saveRoom(rs)
	}
	sl.polling++
	wake := sl.wake
	h.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-wake:
	case <-timer.C:
	case <-ctx.Done():
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	sl.polling--
	if h.rooms[rs.room.ID] == rs && !p.Removed {
		h.remoteSeen(rs, p)
	}
	return sl.claim(), nil
}

func (rs *roomState) queued(seat string) bool {
	for _, t := range rs.queue {
		if t.Agent == seat {
			return true
		}
	}
	return false
}

// remoteSeen records a worker poll. The seat is broadcast at most every 30 s
// so a polling worker does not flood the page. Caller holds h.mu.
func (h *Hub) remoteSeen(rs *roomState, p *Participant) {
	now := nowMs()
	if now-p.RemoteSeenAt < 30_000 {
		return
	}
	p.RemoteSeenAt = now
	rs.broadcast("participant", p)
}

// RemoteFinish takes a worker's answer for the job it claimed.
func (h *Hub) RemoteFinish(token, turn string, res RemoteResult) error {
	if len(res.Text) > maxRemoteText {
		return fmt.Errorf("답이 너무 김: 최대 %dKB", maxRemoteText>>10)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, p, err := h.remoteSeat(token)
	if err != nil {
		return err
	}
	sl := h.remote[remoteKey(rs.room.ID, p.ID)]
	if sl == nil || sl.job == nil || sl.job.Turn != turn || !sl.claimed {
		return fmt.Errorf("이 좌석이 가져간 진행 중 턴이 아님: %s (취소됐거나 시간이 지났을 수 있음)", turn)
	}
	sl.job.done <- res // buffered; the slot drops the job once runRemote reads it
	sl.job = nil
	return nil
}

// runRemote offers the turn to the seat's worker and waits for its answer,
// the turn's own deadline or cancellation, or the claim timeout.
func (h *Hub) runRemote(ctx context.Context, job *RemoteJob) (agent.Result, error) {
	job.done = make(chan RemoteResult, 1)
	h.mu.Lock()
	sl := h.slot(remoteKey(job.Room, job.Seat))
	sl.job, sl.claimed = job, false
	close(sl.wake)
	sl.wake = make(chan struct{})
	h.mu.Unlock()
	drop := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if sl.job == job {
			sl.job = nil
		}
	}
	claimTimer := time.NewTimer(remoteClaimTimeout)
	defer claimTimer.Stop()
	for {
		select {
		case res := <-job.done:
			if res.Error != "" {
				return agent.Result{Text: res.Text, SessionID: res.SessionID, Usage: res.Usage}, errors.New("원격 worker 실행 오류: " + res.Error)
			}
			return agent.Result{Text: res.Text, SessionID: res.SessionID, Usage: res.Usage}, nil
		case <-ctx.Done():
			drop()
			return agent.Result{}, ctx.Err()
		case <-claimTimer.C:
			h.mu.Lock()
			claimed := sl.claimed && sl.job == job
			h.mu.Unlock()
			if !claimed {
				drop()
				return agent.Result{}, fmt.Errorf("원격 worker가 %s 안에 턴을 가져가지 않음", remoteClaimTimeout)
			}
		}
	}
}
