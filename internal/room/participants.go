package room

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"roundtable/internal/agent"
)

// ParticipantSpec creates independent seats, including repeated models/providers.
type ParticipantSpec struct {
	Provider string  `json:"provider"`
	Label    string  `json:"label"`
	Model    string  `json:"model"`
	Effort   *string `json:"effort"`
	Count    int     `json:"count"`
}

func validateMode(mode, workspace string) error {
	if mode != agent.ModeChat && mode != agent.ModeRead && mode != agent.ModeWrite {
		return fmt.Errorf("알 수 없는 모드: %s", mode)
	}
	if mode != agent.ModeChat && strings.TrimSpace(workspace) == "" {
		return fmt.Errorf("read/write 모드는 작업 폴더가 필요함")
	}
	return nil
}

type modelChoices struct {
	efforts []string
	byModel map[string][]string
}

func validateModel(provider, model, effort string, choices modelChoices) error {
	if strings.TrimSpace(model) == "" || strings.ContainsAny(model, "\x00\r\n") {
		return fmt.Errorf("모델 이름이 비어 있거나 올바르지 않음")
	}
	if effort == "" {
		return nil
	}
	allowed := choices.efforts
	if specific, ok := choices.byModel[model]; ok {
		allowed = specific
	}
	if !slices.Contains(allowed, effort) {
		return fmt.Errorf("%s 모델 %s에서 지원하지 않는 추론 수준: %s", provider, model, effort)
	}
	return nil
}

type preparedParticipant struct {
	spec                 ParticipantSpec
	label, model, effort string
	available            bool
	contextWindow        int
}

// Provider discovery can spawn a CLI and must happen outside the hub mutex.
func (h *Hub) prepareParticipant(spec ParticipantSpec) (preparedParticipant, error) {
	out := preparedParticipant{spec: spec}
	prov := h.reg.Get(spec.Provider)
	if prov == nil {
		return out, fmt.Errorf("알 수 없는 공급자: %s", spec.Provider)
	}
	if spec.Count == 0 {
		spec.Count = 1
	}
	if spec.Count < 1 || spec.Count > MaxParticipants {
		return out, fmt.Errorf("참가자 수는 1~%d명이어야 함", MaxParticipants)
	}
	defaultModel, defaultEffort := prov.DefaultModel()
	if spec.Model == "" {
		spec.Model = defaultModel
	}
	effort := ""
	if spec.Effort != nil {
		effort = *spec.Effort
	} else if spec.Model == defaultModel {
		effort = defaultEffort
	}
	choices := modelChoices{efforts: prov.Efforts()}
	if effort != "" {
		choices.byModel = prov.ModelEfforts()
	}
	if err := validateModel(prov.ID(), spec.Model, effort, choices); err != nil {
		return out, err
	}
	return preparedParticipant{spec: spec, label: prov.Label(), model: spec.Model, effort: effort, available: prov.Available(), contextWindow: prov.ContextWindow(spec.Model)}, nil
}

func newParticipants(r *Room, prepared preparedParticipant) ([]*Participant, error) {
	spec := prepared.spec
	active, ordinal := 0, 0
	for _, p := range r.Participants {
		if !p.Removed {
			active++
		}
		if p.Provider == spec.Provider {
			ordinal++
		}
	}
	if active+spec.Count > MaxParticipants {
		return nil, fmt.Errorf("참가자는 방마다 최대 %d명까지 추가할 수 있음", MaxParticipants)
	}
	var seats []*Participant
	for i := 0; i < spec.Count; i++ {
		label := strings.TrimSpace(spec.Label)
		if label == "" {
			label = fmt.Sprintf("%s %d", prepared.label, ordinal+i+1)
		} else if spec.Count > 1 {
			label = fmt.Sprintf("%s %d", label, i+1)
		}
		id := spec.Provider + "-" + newID()
		for r.participant(id) != nil || slices.ContainsFunc(seats, func(p *Participant) bool { return p.ID == id }) {
			id = spec.Provider + "-" + newID()
		}
		seats = append(seats, &Participant{ID: id, Provider: spec.Provider, Label: label, Enabled: prepared.available, Model: prepared.model, Effort: prepared.effort, Status: "idle", ContextWindow: prepared.contextWindow})
	}
	return seats, nil
}

func (h *Hub) AddParticipants(id string, spec ParticipantSpec) ([]byte, error) {
	prepared, err := h.prepareParticipant(spec)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	seats, err := newParticipants(rs.room, prepared)
	if err != nil {
		return nil, err
	}
	next := *rs.room
	next.Participants = append(slices.Clone(next.Participants), seats...)
	next.UpdatedAt = nowMs()
	if err := h.store.SaveRoom(&next); err != nil {
		return nil, err
	}
	*rs.room = next
	rs.broadcast("room", rs.room)
	return json.Marshal(rs.room)
}

// RemoveParticipant archives the seat so historical authors and usage stay readable.
// An in-flight invocation is cancelled but keeps its seat until it reports completion.
func (h *Hub) RemoveParticipant(id, seatID string) ([]byte, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	p := rs.room.participant(seatID)
	if p == nil {
		return nil, fmt.Errorf("참가자 없음: %s", seatID)
	}
	if p.Removed {
		return json.Marshal(rs.room)
	}
	next := *rs.room
	next.Participants = slices.Clone(next.Participants)
	archived := *p
	archived.Removed, archived.Enabled = true, false
	for i, participant := range next.Participants {
		if participant.ID == seatID {
			next.Participants[i] = &archived
		}
	}
	next.UpdatedAt = nowMs()
	if err := h.store.SaveRoom(&next); err != nil {
		return nil, err
	}
	*p = archived
	rs.room.UpdatedAt = next.UpdatedAt
	if run := rs.runningFor(seatID); run != nil {
		run.cancel()
	}
	rs.dropQueuedFor(h, seatID)
	rs.broadcast("room", rs.room)
	h.pump(rs)
	return json.Marshal(rs.room)
}

// Read provider identities briefly, then fetch model metadata without blocking turns.
func (h *Hub) patchModelChoices(id string, patches []ParticipantPatch) map[string]modelChoices {
	providers := map[string]agent.Provider{}
	h.mu.Lock()
	if rs := h.rooms[id]; rs != nil {
		for _, patch := range patches {
			if patch.Model == nil && patch.Effort == nil {
				continue
			}
			if p := rs.room.participant(patch.ID); p != nil {
				if prov := h.reg.Get(p.Provider); prov != nil {
					providers[p.Provider] = prov
				}
			}
		}
	}
	h.mu.Unlock()
	choices := map[string]modelChoices{}
	for id, prov := range providers {
		choices[id] = modelChoices{efforts: prov.Efforts(), byModel: prov.ModelEfforts()}
	}
	return choices
}
