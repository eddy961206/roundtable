package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strings"
	"time"
)

type codexRateWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins int      `json:"windowDurationMins"`
	ResetsAt           int64    `json:"resetsAt"`
}

type codexRateBucket struct {
	LimitName            string           `json:"limitName"`
	Primary              *codexRateWindow `json:"primary"`
	Secondary            *codexRateWindow `json:"secondary"`
	PlanType             string           `json:"planType"`
	RateLimitReachedType string           `json:"rateLimitReachedType"`
	Credits              *struct {
		Unlimited bool   `json:"unlimited"`
		Balance   string `json:"balance"`
	} `json:"credits"`
}

// RefreshLimits uses the official account/rateLimits/read protocol. The CLI
// handles its own login; Roundtable never reads tokens or sends model requests.
// https://learn.chatgpt.com/docs/app-server#auth-endpoints
func (c *Codex) RefreshLimits(ctx context.Context) (Limits, error) {
	path, err := resolveCommand(c.cmd)
	if err != nil {
		return Limits{}, fmt.Errorf("Codex CLI 실행 파일을 찾지 못했어")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "app-server")
	hideWindow(cmd)
	cmd.WaitDelay = 2 * time.Second
	cmd.Stderr = io.Discard
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Limits{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return Limits{}, err
	}
	cmd.Cancel = func() error {
		_ = stdin.Close()
		_ = stdout.Close()
		killTree(cmd)
		return nil
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return Limits{}, fmt.Errorf("Codex CLI 사용량 조회를 시작하지 못했어")
	}
	defer func() {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	enc := json.NewEncoder(stdin)
	if err := enc.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "roundtable_usage", "version": "1"}}}); err != nil {
		return Limits{}, err
	}
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scan.Scan() {
		var reply struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if json.Unmarshal(scan.Bytes(), &reply) != nil || reply.ID == 0 {
			continue
		}
		if len(reply.Error) > 0 && string(reply.Error) != "null" {
			return Limits{}, fmt.Errorf("Codex CLI가 계정 사용량을 반환하지 않았어. CLI 로그인과 계정 사용량 조회 지원 여부를 확인해")
		}
		switch reply.ID {
		case 1:
			if err := enc.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}); err != nil {
				return Limits{}, err
			}
			if err := enc.Encode(map[string]any{"id": 2, "method": "account/rateLimits/read"}); err != nil {
				return Limits{}, err
			}
		case 2:
			return parseCodexRateLimits(reply.Result, now())
		}
	}
	if ctx.Err() != nil {
		return Limits{}, fmt.Errorf("Codex CLI 사용량 조회 시간이 초과됐어")
	}
	return Limits{}, fmt.Errorf("Codex CLI 사용량 응답을 읽지 못했어")
}

func parseCodexRateLimits(data []byte, observedAt int64) (Limits, error) {
	var reply struct {
		RateLimits          *codexRateBucket           `json:"rateLimits"`
		RateLimitsByLimitID map[string]codexRateBucket `json:"rateLimitsByLimitId"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return Limits{}, fmt.Errorf("Codex CLI 사용량 응답 형식이 올바르지 않아")
	}
	buckets := reply.RateLimitsByLimitID
	if len(buckets) == 0 && reply.RateLimits != nil {
		buckets = map[string]codexRateBucket{"codex": *reply.RateLimits}
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	l := Limits{Provider: "codex", At: observedAt, Source: "codex_cli_account"}
	for _, key := range keys {
		bucket := buckets[key]
		for _, w := range []*codexRateWindow{bucket.Primary, bucket.Secondary} {
			if w == nil || w.UsedPercent == nil || *w.UsedPercent < 0 || *w.UsedPercent > 100 {
				continue
			}
			label := windowLabel(w.WindowDurationMins)
			if len(buckets) > 1 {
				name := bucket.LimitName
				if name == "" {
					name = key
				}
				label = name + " · " + label
			}
			l.Windows = append(l.Windows, LimitWindow{Label: label, UsedPercent: *w.UsedPercent, ResetsAt: w.ResetsAt})
		}
		if key == "codex" || len(buckets) == 1 {
			l.Plan = bucket.PlanType
			if cr := bucket.Credits; cr != nil {
				if cr.Unlimited {
					l.Credits = "무제한"
				} else if cr.Balance != "" {
					l.Credits = cr.Balance
					if strings.Contains(l.Credits, ".") {
						l.Credits = strings.TrimRight(strings.TrimRight(l.Credits, "0"), ".")
					}
				}
			}
			if bucket.RateLimitReachedType != "" {
				l.Note = "한도 도달: " + bucket.RateLimitReachedType
			}
		}
	}
	if len(l.Windows) == 0 && l.Credits == "" {
		return Limits{}, fmt.Errorf("이 Codex 로그인 방식에서는 계정 사용량을 조회할 수 없거나 아직 관측값이 없어")
	}
	return l, nil
}
