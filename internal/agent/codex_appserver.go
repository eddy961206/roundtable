package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func codexInputs(text string, images []Image) []map[string]any {
	input := []map[string]any{{"type": "text", "text": text}}
	for _, image := range images {
		input = append(input, map[string]any{"type": "localImage", "path": image.Path})
	}
	return input
}

func codexSandbox(mode, windowsSandbox string) string {
	if windowsSandbox == "" {
		return "danger-full-access"
	}
	if mode == ModeWrite {
		return "workspace-write"
	}
	return "read-only"
}

func codexThreadParams(o RunOptions, windowsSandbox string) (string, map[string]any) {
	method := "thread/start"
	params := map[string]any{"cwd": workDir(o), "approvalPolicy": "never", "sandbox": codexSandbox(o.Mode, windowsSandbox)}
	if o.Model != "" {
		params["model"] = o.Model
	}
	if o.SessionID != "" {
		method, params["threadId"] = "thread/resume", o.SessionID
	} else if o.ForkSession != "" {
		method, params["threadId"] = "thread/fork", o.ForkSession
	}
	return method, params
}

func (c *Codex) Run(ctx context.Context, o RunOptions, emit func(Event)) (res Result, runErr error) {
	if err := validateImages(o.Images); err != nil {
		return res, err
	}
	path, err := resolveCommand(c.cmd)
	if err != nil {
		return res, fmt.Errorf("codex CLI를 찾지 못함: %w", err)
	}
	cmd := exec.Command(path, codexArgs(o, c.windowsSandbox)...)
	cmd.Dir, cmd.Env = workDir(o), os.Environ()
	if o.Mode == ModeWrite {
		if !filepath.IsAbs(o.ScratchDir) {
			return res, fmt.Errorf("Codex 쓰기 모드에는 절대 경로 ScratchDir가 필요함")
		}
		cache := filepath.Join(o.ScratchDir, "codex", "go-build")
		if err := os.MkdirAll(cache, 0700); err != nil {
			return res, fmt.Errorf("Codex Go 빌드 캐시 생성: %w", err)
		}
		cmd.Env = append(cmd.Env, "GOCACHE="+cache)
	}
	p := newCodexParser()
	p.before = map[string]any{}
	// Capture BEFORE starting or resuming. turn_context may be written more
	// than once in one turn; neither it nor a steer may move this baseline.
	if base := firstNonEmpty(o.SessionID, o.ForkSession); base != "" {
		p.before, err = c.threadTotals(base)
		if err != nil {
			if o.ForkSession != "" {
				return res, err
			}
			x := ev(KindStatus)
			x.Text = "Codex 사용량 시작 기준을 읽지 못해 이번 턴 토큰 집계를 생략함: " + err.Error()
			emit(x)
		}
	}
	rpc, err := startCodexRPC(cmd)
	if err != nil {
		return res, err
	}
	observe := func(packet codexPacket) { p.packet(packet, emit) }
	watcher := newRolloutWatcher(c.sessionsDir, time.Now().Add(-5*time.Second))
	stopWatch, watchDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(1500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-ticker.C:
				if id := p.threadID(); id != "" {
					watcher.scan(id, emit)
				}
			}
		}
	}()
	defer func() {
		if ctx.Err() != nil && p.turn != "" {
			_, _ = rpc.send("turn/interrupt", map[string]any{"threadId": p.threadID(), "turnId": p.turn})
		}
		stderr, waitErr := rpc.close(ctx)
		close(stopWatch)
		<-watchDone
		if id := p.threadID(); id != "" {
			watcher.scan(id, emit)
			watcher.finish(emit)
			if file := watcher.pathOf(id); file != "" {
				codexLimitsFromRollout(file, p, emit)
			}
		}
		fin := p.Finish(emit)
		res.Text, res.SessionID, res.Usage, res.Stderr = fin.Text, fin.SessionID, fin.Usage, stderr
		if runErr == nil {
			runErr = waitErr
		}
		if ctx.Err() != nil {
			runErr = ctx.Err()
		}
		runErr = finishError("codex app-server", fin, runErr, filterCodexStderr(stderr))
	}()
	if err := rpc.initialize(ctx, observe); err != nil {
		return res, err
	}
	method, params := codexThreadParams(o, c.windowsSandbox)
	data, err := rpc.call(ctx, method, params, observe)
	if err != nil {
		return res, err
	}
	var thread struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(data, &thread); err != nil || thread.Thread.ID == "" {
		return res, fmt.Errorf("Codex %s 응답에 thread id가 없음", method)
	}
	p.setThread(thread.Thread.ID, emit)
	turnParams := map[string]any{"threadId": p.threadID(), "input": codexInputs(codexPrompt(o), o.Images)}
	if o.Model != "" {
		turnParams["model"] = o.Model
	}
	if o.Effort != "" {
		turnParams["effort"] = o.Effort
	}
	data, err = rpc.call(ctx, "turn/start", turnParams, observe)
	if err != nil {
		return res, err
	}
	var started struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	if err := json.Unmarshal(data, &started); err != nil || started.Turn.ID == "" {
		return res, fmt.Errorf("Codex turn/start 응답에 turn id가 없음")
	}
	p.turn = started.Turn.ID
	steers := o.Steer
	unsupported := false
	for !p.completed {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case packet, ok := <-rpc.packets:
			if !ok {
				return res, fmt.Errorf("Codex app-server가 턴 완료 전에 종료됨")
			}
			if packet.err != nil {
				return res, packet.err
			}
			if packet.Method != "" && len(packet.ID) > 0 {
				if err := rpc.serverRequest(packet, observe); err != nil {
					return res, err
				}
			} else {
				observe(packet)
			}
		case input, ok := <-steers:
			if !ok {
				steers = nil
				continue
			}
			receipt := SteerReceipt{ID: input.ID, SessionID: p.threadID(), TurnID: p.turn, ClientID: input.ID, Source: "codex_rpc"}
			if imageErr := validateImages(input.Images); imageErr != nil || unsupported {
				receipt.State, receipt.Detail = InputRejected, "이 CLI는 turn/steer를 지원하지 않음"
				if imageErr != nil {
					receipt.Detail = imageErr.Error()
				}
				res.Receipts = append(res.Receipts, receipt)
				emitReceipt(emit, receipt)
				continue
			}
			receipt.State = InputSending
			receiptIndex := len(res.Receipts)
			res.Receipts = append(res.Receipts, receipt)
			emitReceipt(emit, receipt) // the room persists this before any bytes are sent
			if ctx.Err() != nil {
				return res, ctx.Err()
			}
			data, steerErr := rpc.call(ctx, "turn/steer", map[string]any{
				"threadId": p.threadID(), "expectedTurnId": p.turn, "clientUserMessageId": receipt.ClientID,
				"input": codexInputs(input.Text, input.Images),
			}, observe)
			var accepted struct {
				TurnID string `json:"turnId"`
			}
			var rejected *codexRPCError
			switch {
			case errors.As(steerErr, &rejected):
				receipt.State, receipt.Detail = InputRejected, rejected.Error()
				unsupported = rejected.Code == -32601 || strings.Contains(rejected.Message, "unknown variant") || strings.Contains(rejected.Message, "unknown method")
			case steerErr != nil:
				receipt.State, receipt.Detail = InputUncertain, steerErr.Error()
			case json.Unmarshal(data, &accepted) != nil || accepted.TurnID != p.turn:
				steerErr = fmt.Errorf("Codex 주입 응답의 turn id가 요청과 다름")
				receipt.State, receipt.Detail = InputUncertain, steerErr.Error()
			default:
				receipt.State = InputAccepted
			}
			res.Receipts[receiptIndex] = receipt
			emitReceipt(emit, receipt)
			if receipt.State == InputUncertain {
				return res, steerErr
			}
		}
	}
	return res, nil
}

func (c *Codex) ResolveSteer(ctx context.Context, receipt SteerReceipt) (SteerReceipt, error) {
	if receipt.SessionID == "" || receipt.TurnID == "" || receipt.ClientID == "" {
		return receipt, fmt.Errorf("Codex 전달 확인에 필요한 세션·턴·입력 id가 없음")
	}
	path, err := resolveCommand(c.cmd)
	if err != nil {
		return receipt, err
	}
	rpc, err := startCodexRPC(exec.Command(path, "app-server", "-c", "features.plugins=false"))
	if err != nil {
		return receipt, err
	}
	defer rpc.close(ctx)
	if err := rpc.initialize(ctx, nil); err != nil {
		return receipt, err
	}
	cursor := ""
	for {
		params := map[string]any{"threadId": receipt.SessionID, "turnId": receipt.TurnID, "limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		data, err := rpc.call(ctx, "thread/items/list", params, nil)
		if err != nil {
			return receipt, err
		}
		var page struct {
			Data []struct {
				Item struct {
					Type     string `json:"type"`
					ClientID string `json:"clientId"`
				} `json:"item"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := json.Unmarshal(data, &page); err != nil {
			return receipt, err
		}
		for _, entry := range page.Data {
			if entry.Item.Type == "userMessage" && entry.Item.ClientID == receipt.ClientID {
				receipt.State, receipt.Source, receipt.Detail = InputAccepted, "codex_history", "저장된 턴 기록에서 수신을 확인함"
				return receipt, nil
			}
		}
		if page.NextCursor == nil {
			receipt.State, receipt.Detail = InputRejected, "저장된 턴 기록에 입력이 없어 다음 턴으로 전달"
			return receipt, nil
		}
		if *page.NextCursor == "" || *page.NextCursor == cursor {
			return receipt, fmt.Errorf("Codex 입력 기록 페이지 커서가 진행하지 않음")
		}
		cursor = *page.NextCursor
	}
}
