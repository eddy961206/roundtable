package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Official reference: https://antigravity.google/docs/cli/statusline
// Antigravity hands its statusLine command a JSON document on stdin every time
// it redraws the status line, in interactive and in print (-p) mode alike:
//
//	{"model":{...},"context_window":{...},
//	 "quota":{"gemini-5h":{"remaining_fraction":0.72,"reset_in_seconds":9660},
//	          "gemini-weekly":{...},"3p-5h":{...},"3p-weekly":{...}}}
//
// `roundtable agy-statusline` stores that document; this file reads it back.
// No credential is touched: the numbers come from agy itself.

// AgyStatus is the last statusLine payload containing a usable quota observation.
type AgyStatus struct {
	ObservedAt int64           `json:"observed_at"` // unix ms when agy sent it
	QuotaAt    int64           `json:"quota_at"`    // unix ms of the payload the quota came from
	Data       json.RawMessage `json:"data"`
}

// agyKeptKeys are the only statusLine fields roundtable stores. The payload also
// carries the account e-mail, working directory and transcript path; those are
// dropped before anything touches the disk.
var agyKeptKeys = []string{"model", "context_window", "quota", "plan_tier", "version"}

// WriteAgyStatus stores complete quota observations. Quota-less redraws do not
// write: concurrent CLI processes must never copy an old quota over a newer one.
func WriteAgyStatus(path string, payload []byte, now time.Time) error {
	var full map[string]json.RawMessage
	if err := json.Unmarshal(payload, &full); err != nil {
		return fmt.Errorf("agy statusLine 입력이 JSON 객체가 아님: %w", err)
	}
	if full == nil {
		return fmt.Errorf("agy statusLine 입력이 JSON 객체가 아님")
	}
	kept := map[string]json.RawMessage{}
	for _, k := range agyKeptKeys {
		if v, ok := full[k]; ok {
			kept[k] = v
		}
	}
	st := AgyStatus{ObservedAt: now.UnixMilli(), QuotaAt: now.UnixMilli()}
	var quota map[string]struct {
		Remaining *float64 `json:"remaining_fraction"`
	}
	if raw, ok := kept["quota"]; ok {
		if err := json.Unmarshal(raw, &quota); err != nil {
			return fmt.Errorf("agy quota 형식 오류: %w", err)
		}
	}
	usable := false
	for _, w := range agyQuotaWindows {
		if q, ok := quota[w.key]; ok && q.Remaining != nil {
			if *q.Remaining < 0 || *q.Remaining > 1 {
				return fmt.Errorf("quota.%s.remaining_fraction은 0~1이어야 함", w.key)
			}
			usable = true
		}
	}
	if !usable {
		return nil
	}
	data, err := json.Marshal(kept)
	if err != nil {
		return err
	}
	st.Data = data
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agy-status-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

var agyQuotaWindows = []struct{ key, label string }{
	{"gemini-5h", "Gemini 5시간"},
	{"gemini-weekly", "Gemini 주간"},
	{"3p-5h", "외부 모델 5시간"},
	{"3p-weekly", "외부 모델 주간"},
}

// ParseAgyLimits converts a stored status into the common Limits shape.
// remaining_fraction is what is left; Limits carries what is used.
func ParseAgyLimits(st AgyStatus) (Limits, error) {
	var d struct {
		PlanTier string `json:"plan_tier"`
		Quota    map[string]struct {
			Remaining *float64 `json:"remaining_fraction"`
			ResetTime string   `json:"reset_time"` // UTC ISO 8601 (official docs)
			ResetIn   *float64 `json:"reset_in_seconds"`
		} `json:"quota"`
	}
	if err := json.Unmarshal(st.Data, &d); err != nil {
		return Limits{}, err
	}
	l := Limits{Provider: "agy", At: st.QuotaAt, Source: "agy_statusline", Plan: d.PlanTier}
	for _, w := range agyQuotaWindows {
		q, ok := d.Quota[w.key]
		if !ok || q.Remaining == nil {
			continue
		}
		if *q.Remaining < 0 || *q.Remaining > 1 {
			return Limits{}, fmt.Errorf("quota.%s.remaining_fraction은 0~1이어야 함", w.key)
		}
		win := LimitWindow{Label: w.label, UsedPercent: (1 - *q.Remaining) * 100}
		switch {
		case q.ResetTime != "":
			t, err := time.Parse(time.RFC3339, q.ResetTime)
			if err != nil {
				return Limits{}, fmt.Errorf("quota.%s.reset_time 형식 오류: %w", w.key, err)
			}
			win.ResetsAt = t.Unix()
		case q.ResetIn != nil && st.QuotaAt > 0:
			win.ResetsAt = st.QuotaAt/1000 + int64(*q.ResetIn)
		}
		l.Windows = append(l.Windows, win)
	}
	if len(l.Windows) == 0 {
		return Limits{}, fmt.Errorf("agy 상태에 quota 항목이 없음")
	}
	return l, nil
}

// CurrentLimits reads the latest status file. ok is false when the hook is not
// installed yet or agy has not drawn its status line since.
func (a *Agy) CurrentLimits() (Limits, bool) {
	l, err := a.RefreshLimits(context.Background())
	return l, err == nil
}

func (a *Agy) RefreshLimits(ctx context.Context) (Limits, error) {
	if err := ctx.Err(); err != nil {
		return Limits{}, err
	}
	if a.statusPath == "" {
		return Limits{}, fmt.Errorf("Antigravity 사용량 기록 경로가 설정되지 않았어")
	}
	st, err := readAgyStatus(a.statusPath)
	if err != nil {
		return Limits{}, fmt.Errorf("Antigravity 상태표시줄의 사용량 기록을 읽지 못했어. CLI와 사용량 기록 연결을 확인해")
	}
	return ParseAgyLimits(st)
}

func readAgyStatus(path string) (AgyStatus, error) {
	var st AgyStatus
	b, err := os.ReadFile(path)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s 손상: %w", path, err)
	}
	return st, nil
}
