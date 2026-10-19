package room

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

//go:embed role_presets.json
var defaultRolePresets []byte

type RolePreset struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Directive string   `json:"directive"`
	Tags      []string `json:"tags"`
}

type ModelSuggestion struct {
	Match  string `json:"match"`
	Preset string `json:"preset"`
}

type RolePresets struct {
	Presets          []RolePreset      `json:"presets"`
	ModelSuggestions []ModelSuggestion `json:"model_suggestions"`
	Path             string            `json:"path"`
}

// RolePresets rereads the editable file on every request; malformed user data
// never falls back to embedded recommendations.
func (s *Store) RolePresets() (*RolePresets, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filename := filepath.Join(s.root, "role-presets.json")
	data, err := os.ReadFile(filename)
	if os.IsNotExist(err) {
		f, createErr := os.OpenFile(filename, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if createErr != nil {
			return nil, fmt.Errorf("role-presets.json 생성: %w", createErr)
		}
		_, writeErr := f.Write(defaultRolePresets)
		closeErr := f.Close()
		if writeErr != nil {
			return nil, fmt.Errorf("role-presets.json 저장: %w", writeErr)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		data, err = defaultRolePresets, nil
	}
	if err != nil {
		return nil, fmt.Errorf("role-presets.json 읽기: %w", err)
	}
	var presets RolePresets
	if err := json.Unmarshal(data, &presets); err != nil {
		return nil, fmt.Errorf("role-presets.json: %w", err)
	}
	if presets.Presets == nil || presets.ModelSuggestions == nil {
		return nil, fmt.Errorf("role-presets.json: presets와 model_suggestions 배열이 필요함")
	}
	ids := map[string]bool{}
	for _, p := range presets.Presets {
		if strings.TrimSpace(p.ID) == "" || ids[p.ID] || strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Directive) == "" || p.Tags == nil {
			return nil, fmt.Errorf("role-presets.json: 역할의 id, name, directive, tags가 올바르지 않음 (%q)", p.ID)
		}
		ids[p.ID] = true
	}
	for _, suggestion := range presets.ModelSuggestions {
		if _, err := path.Match(suggestion.Match, ""); err != nil || suggestion.Match == "" || !ids[suggestion.Preset] {
			return nil, fmt.Errorf("role-presets.json: 모델 추천이 올바르지 않음 (%q)", suggestion.Match)
		}
	}
	presets.Path = filename
	return &presets, nil
}

func (h *Hub) RolePresets() (*RolePresets, error) { return h.store.RolePresets() }
