package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type openCodeCatalog struct {
	loadMu  sync.Mutex
	mu      sync.RWMutex
	loaded  time.Time
	models  []ModelOption
	efforts map[string][]string
	windows map[string]int
	err     string
}

// InvalidateModelCatalog lets a newly connected subscription or API provider
// appear on an explicit refresh without waiting for the metadata cache to age.
func (o *OpenCode) InvalidateModelCatalog() {
	o.catalog.mu.Lock()
	o.catalog.loaded = time.Time{}
	o.catalog.mu.Unlock()
}

func (o *OpenCode) Models() []ModelOption {
	o.catalog.loadMu.Lock()
	defer o.catalog.loadMu.Unlock()
	o.catalog.mu.RLock()
	cacheFor := 10 * time.Minute
	if o.catalog.err != "" {
		cacheFor = 2 * time.Second
	}
	if !o.catalog.loaded.IsZero() && time.Since(o.catalog.loaded) < cacheFor {
		out := slices.Clone(o.catalog.models)
		o.catalog.mu.RUnlock()
		return out
	}
	o.catalog.mu.RUnlock()
	var options []ModelOption
	efforts, windows := map[string][]string{}, map[string]int{}
	catalogError := ""
	defer func() {
		o.catalog.mu.Lock()
		o.catalog.loaded = time.Now()
		o.catalog.models, o.catalog.efforts, o.catalog.windows, o.catalog.err = options, efforts, windows, catalogError
		o.catalog.mu.Unlock()
	}()
	path, err := resolveCommand(o.cmd)
	if err != nil {
		catalogError = "OpenCode CLI를 찾지 못했어"
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var providers struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
	}
	var models struct {
		Data []struct {
			ID         string `json:"id"`
			ProviderID string `json:"providerID"`
			Name       string `json:"name"`
			Enabled    bool   `json:"enabled"`
			Variants   []struct {
				ID string `json:"id"`
			} `json:"variants"`
			Capabilities struct {
				Tools  bool     `json:"tools"`
				Output []string `json:"output"`
			} `json:"capabilities"`
			Limit struct {
				Context int `json:"context"`
				Output  int `json:"output"`
			} `json:"limit"`
		} `json:"data"`
	}
	// The documented v2 snapshot can precede initial plugin settlement. Query
	// the same shared service until both metadata snapshots settle, bounded above.
	for attempt := 0; attempt < 4; attempt++ {
		if err := openCodeReadAPI(ctx, path, "provider.list", &providers); err != nil {
			catalogError = err.Error()
			return nil
		}
		if err := openCodeReadAPI(ctx, path, "model.list", &models); err != nil {
			catalogError = err.Error()
			return nil
		}
		if len(providers.Data) > 0 && len(models.Data) > 0 {
			break
		}
		select {
		case <-ctx.Done():
			catalogError = "OpenCode 모델 목록 준비 시간이 초과됐어"
			return nil
		case <-time.After(400 * time.Millisecond):
		}
	}
	names := map[string]string{}
	for _, p := range providers.Data {
		names[p.ID] = p.Name
	}
	for _, m := range models.Data {
		name, connected := names[m.ProviderID]
		// The native catalog also contains embeddings, image and realtime models.
		// This picker is for tool-capable workers with text-only generation.
		if !connected || !m.Enabled || !m.Capabilities.Tools || len(m.Capabilities.Output) != 1 || m.Capabilities.Output[0] != "text" || m.Limit.Output <= 0 {
			continue
		}
		if name == "" {
			name = m.ProviderID
		}
		id := m.ProviderID + "/" + m.ID
		label := m.Name
		if label == "" {
			label = m.ID
		}
		options = append(options, ModelOption{ID: id, Label: name + " · " + label, Group: name})
		levels := []string{}
		for _, variant := range m.Variants {
			if variant.ID != "" {
				levels = append(levels, variant.ID)
			}
		}
		efforts[id] = levels
		windows[id] = m.Limit.Context
	}
	sort.SliceStable(options, func(i, j int) bool {
		a, b := options[i], options[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Label < b.Label
	})
	if len(options) == 0 {
		catalogError = "OpenCode가 아직 선택 가능한 텍스트 모델을 반환하지 않았어. OpenCode의 공급사 연결과 모델 구성을 확인해."
	}
	return slices.Clone(options)
}

func openCodeReadAPI(ctx context.Context, path, operation string, into any, parameters ...string) error {
	args := []string{"api", operation}
	for _, parameter := range parameters {
		args = append(args, "--param", parameter)
	}
	cmd := exec.CommandContext(ctx, path, args...)
	hideWindow(cmd)
	cmd.Env = openCodeEnv()
	cmd.Cancel = func() error { killTree(cmd); return nil }
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("OpenCode 기록을 읽지 못했어. CLI 연결 상태를 확인해")
	}
	data, readErr := io.ReadAll(io.LimitReader(stdout, 16*1024*1024+1))
	if readErr != nil || len(data) > 16*1024*1024 {
		killTree(cmd)
	}
	err = cmd.Wait()
	if len(data) > 16*1024*1024 {
		return fmt.Errorf("OpenCode 응답이 너무 커서 읽지 못했어")
	}
	if readErr != nil || err != nil {
		return fmt.Errorf("OpenCode 기록을 읽지 못했어. CLI 연결 상태를 확인해")
	}
	if err := json.Unmarshal(data, into); err != nil {
		return fmt.Errorf("OpenCode 응답 형식이 올바르지 않아")
	}
	return nil
}

func (o *OpenCode) ModelEfforts() map[string][]string {
	o.Models()
	o.catalog.mu.RLock()
	defer o.catalog.mu.RUnlock()
	out := map[string][]string{}
	for id, levels := range o.catalog.efforts {
		out[id] = slices.Clone(levels)
	}
	return out
}
func (o *OpenCode) Efforts() []string {
	byModel := o.ModelEfforts()
	out := []string{}
	for _, levels := range byModel {
		for _, level := range levels {
			if !slices.Contains(out, level) {
				out = append(out, level)
			}
		}
	}
	sort.Strings(out)
	return out
}
func (o *OpenCode) ContextWindow(model string) int {
	o.catalog.mu.RLock()
	defer o.catalog.mu.RUnlock()
	return o.catalog.windows[strings.SplitN(model, "#", 2)[0]]
}
func (o *OpenCode) CatalogStatus() (string, string) {
	o.catalog.mu.RLock()
	defer o.catalog.mu.RUnlock()
	if o.catalog.err != "" {
		return "unavailable", o.catalog.err
	}
	if o.catalog.loaded.IsZero() {
		return "loading", ""
	}
	return "ready", ""
}
