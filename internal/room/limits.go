package room

import (
	"context"
	"fmt"
	"slices"
	"time"

	"roundtable/internal/agent"
)

const UsageRefreshIntervalSeconds = 60

// UsageSource separates checking a source from observing new account data.
// Local rereads retain observed_at; passing a reset time never resets usage.
type UsageSource struct {
	Source      string `json:"source"`
	RefreshKind string `json:"refresh_kind"`
	ObservedAt  int64  `json:"observed_at"`
	CheckedAt   int64  `json:"checked_at"`
	Stale       bool   `json:"stale"`
	Error       string `json:"error,omitempty"`
	Note        string `json:"note"`
}

type UsageView struct {
	Limits                 map[string]agent.Limits `json:"limits"`
	Sources                map[string]UsageSource  `json:"sources"`
	Refreshing             bool                    `json:"refreshing"`
	RefreshIntervalSeconds int                     `json:"refresh_interval_seconds"`
}

func usageSource(provider string) UsageSource {
	switch provider {
	case "codex":
		return UsageSource{Source: "codex_cli_account", RefreshKind: "vendor", Note: "선택한 Codex CLI의 계정 사용량 조회. 모델은 호출하지 않아."}
	case "claude":
		return UsageSource{Source: "claude_turn_event", RefreshKind: "local", Note: "Roundtable에서 받은 최근 Claude 사용량 기록이야. 새 계정 조회는 아니며, 최신 값은 Claude CLI의 /usage에서 확인할 수 있어."}
	case "agy":
		return UsageSource{Source: "agy_statusline", RefreshKind: "local", Note: "Antigravity 상태표시줄이 저장한 기록을 다시 읽어. CLI가 새 사용량을 보내기 전에는 관측 시각이 바뀌지 않아."}
	case "opencode":
		return UsageSource{Source: "opencode_turn_usage", RefreshKind: "unsupported", Note: "OpenCode에 연결한 구독의 남은 한도와 API 잔액 조회는 아직 지원하지 않아. 토큰과 CLI가 보고한 비용은 각 답변과 이 방 누적 사용량에 기록돼. 표시 비용은 청구액과 다를 수 있어."}
	default:
		return UsageSource{Source: "saved_record", RefreshKind: "local", Note: "저장된 사용량 기록이야."}
	}
}

func (h *Hub) usageProviders(ids []string) ([]string, error) {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if h.reg.Get(id) == nil {
			return nil, fmt.Errorf("알 수 없는 공급자: %s", id)
		}
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// Usage is memory-only. An empty provider list never implies all providers.
func (h *Hub) Usage(ids []string) (UsageView, error) {
	ids, err := h.usageProviders(ids)
	if err != nil {
		return UsageView{}, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usageLocked(ids), nil
}

func (h *Hub) usageLocked(ids []string) UsageView {
	out := UsageView{Limits: map[string]agent.Limits{}, Sources: map[string]UsageSource{}, RefreshIntervalSeconds: UsageRefreshIntervalSeconds}
	now := nowMs()
	for _, id := range ids {
		state, ok := h.usageStates[id]
		if !ok {
			state = usageSource(id)
		}
		if l, ok := h.limits[id]; ok {
			l.Windows = slices.Clone(l.Windows)
			out.Limits[id] = l
			state.ObservedAt = l.At
			state.Source = l.Source
			if state.Source == "" {
				state.Source = "saved_record"
			}
			for _, w := range l.Windows {
				if w.ResetsAt > 0 && w.ResetsAt <= now/1000 {
					state.Stale = true
				}
			}
		}
		state.Stale = state.Stale || state.ObservedAt <= 0 || now-state.ObservedAt > int64(2*time.Minute/time.Millisecond) || state.Error != ""
		if id == "codex" && (state.Source != "codex_cli_account" || state.CheckedAt == 0 || state.Error != "") {
			state.RefreshKind = "local"
			state.Note = "저장된 Codex 관측값을 표시해. 선택한 공급사의 갱신이 성공하면 새 계정 조회 결과로 바뀌어."
		}
		out.Sources[id] = state
		out.Refreshing = out.Refreshing || h.usageFlights[id] != nil
	}
	return out
}

// RefreshUsage shares one bounded operation per provider across tabs and rooms.
// Only the explicit selection is contacted. Caller cancellation stops waiting,
// while the shared operation has its own timeout so other callers can finish.
func (h *Hub) RefreshUsage(ctx context.Context, ids []string, force bool) (UsageView, error) {
	ids, err := h.usageProviders(ids)
	if err != nil {
		return UsageView{}, err
	}
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return UsageView{}, fmt.Errorf("서버 종료 중에는 사용량을 갱신할 수 없어")
	}
	if h.usageStates == nil {
		h.usageStates = map[string]UsageSource{}
		h.usageFlights = map[string]chan struct{}{}
		h.usageCancels = map[string]context.CancelFunc{}
	}
	var waits []chan struct{}
	for _, id := range ids {
		if done := h.usageFlights[id]; done != nil {
			waits = append(waits, done)
			continue
		}
		state := h.usageStates[id]
		if !force && state.CheckedAt > 0 && nowMs()-state.CheckedAt < int64(45*time.Second/time.Millisecond) {
			continue
		}
		done := make(chan struct{})
		refreshCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		h.usageFlights[id] = done
		h.usageCancels[id] = cancel
		waits = append(waits, done)
		go h.refreshUsageProvider(refreshCtx, id, done)
	}
	h.mu.Unlock()
	for _, done := range waits {
		select {
		case <-ctx.Done():
			return UsageView{}, ctx.Err()
		case <-done:
		}
	}
	return h.Usage(ids)
}

func (h *Hub) refreshUsageProvider(ctx context.Context, id string, done chan struct{}) {
	state := usageSource(id)
	var l agent.Limits
	var err error
	if source, ok := h.reg.Get(id).(agent.LimitRefresher); ok {
		l, err = source.RefreshLimits(ctx)
	} else {
		// Claude's batch CLI has no documented non-inference quota command.
		// Read the already recorded observation without implying a vendor refresh.
		h.mu.Lock()
		l = h.limits[id]
		h.mu.Unlock()
		if id != "opencode" && len(l.Windows) == 0 && l.Credits == "" {
			err = fmt.Errorf("아직 이 공급사의 사용량 관측 기록이 없어")
		}
	}
	state.CheckedAt = nowMs()
	if err != nil {
		state.Error = err.Error()
	}
	h.mu.Lock()
	if err == nil {
		h.recordLimitsLocked(l)
	}
	h.usageStates[id] = state
	h.usageCancels[id]()
	delete(h.usageCancels, id)
	delete(h.usageFlights, id)
	close(done)
	h.mu.Unlock()
}

// Caller holds h.mu. Persist through one coalescing writer outside that lock.
func (h *Hub) recordLimitsLocked(l agent.Limits) bool {
	if l.Provider == "" || (len(l.Windows) == 0 && l.Credits == "") {
		return false
	}
	if current, ok := h.limits[l.Provider]; ok && l.At <= current.At {
		return false
	}
	l.Windows = slices.Clone(l.Windows)
	h.limits[l.Provider] = l
	h.limitsVersion++
	if !h.limitsSaving {
		h.limitsSaving = true
		h.limitsDone = make(chan struct{})
		go h.persistLimits()
	}
	return true
}

func (h *Hub) persistLimits() {
	for {
		h.mu.Lock()
		version, snapshot := h.limitsVersion, h.limitsCopy()
		h.mu.Unlock()
		err := h.store.SaveLimits(snapshot)
		h.mu.Lock()
		done := err != nil || version == h.limitsVersion
		if done {
			h.limitsSaving = false
			h.limitsSaveError = err
			close(h.limitsDone)
		}
		h.mu.Unlock()
		if err != nil && h.Log != nil {
			h.Log.Printf("사용량 기록 저장 실패: %v", err)
		}
		if done {
			return
		}
	}
}

// Called after model turns stop: cancel metadata requests and drain the single
// limits writer without holding the room mutex over I/O or waiting.
func (h *Hub) waitLimits(ctx context.Context) error {
	for {
		h.mu.Lock()
		var pending []chan struct{}
		for id, done := range h.usageFlights {
			h.usageCancels[id]()
			pending = append(pending, done)
		}
		if h.limitsSaving {
			pending = append(pending, h.limitsDone)
		}
		err := h.limitsSaveError
		h.mu.Unlock()
		if len(pending) == 0 {
			return err
		}
		for _, done := range pending {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-done:
			}
		}
	}
}
