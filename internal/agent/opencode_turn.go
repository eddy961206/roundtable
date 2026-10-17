package agent

import (
	"context"
	"fmt"
	"strings"
)

type openCodeMessage struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	Text  string `json:"text"`
	Agent string `json:"agent"`
	Time  struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
	Finish string         `json:"finish"`
	Cost   *float64       `json:"cost"`
	Tokens map[string]any `json:"tokens"`
}

func (p *openCodeParser) readTurn(ctx context.Context, path string, opts RunOptions, started int64) error {
	if p.session == "" {
		return fmt.Errorf("OpenCode가 이번 실행의 세션 ID를 반환하지 않았어")
	}
	parameters := []string{"sessionID=" + p.session, "limit=100", "order=desc"}
	var assistants []openCodeMessage
	for {
		var page struct {
			Data   []openCodeMessage `json:"data"`
			Cursor struct {
				Next string `json:"next"`
			} `json:"cursor"`
		}
		if err := openCodeReadAPI(ctx, path, "session.message.list", &page, parameters...); err != nil {
			return err
		}
		for _, message := range page.Data {
			if message.Time.Created < started {
				return fmt.Errorf("OpenCode 저장 기록에서 이번 입력을 확인하지 못했어")
			}
			if message.Type == "user" {
				if strings.TrimSpace(message.Text) != strings.TrimSpace(opts.Prompt) {
					return fmt.Errorf("OpenCode 저장 기록의 최신 입력이 이번 요청과 달라")
				}
				return p.applyTurn(assistants, opts)
			}
			if message.Type == "assistant" {
				assistants = append(assistants, message)
			}
		}
		if len(page.Data) == 0 || page.Cursor.Next == "" {
			return fmt.Errorf("OpenCode 저장 기록에서 이번 입력을 확인하지 못했어")
		}
		parameters = []string{"sessionID=" + p.session, "limit=100", "cursor=" + page.Cursor.Next}
	}
}

func (p *openCodeParser) applyTurn(messages []openCodeMessage, opts RunOptions) error {
	if len(messages) == 0 {
		return fmt.Errorf("OpenCode 저장 기록에 이번 응답이 없어")
	}
	latest := messages[0]
	if latest.Time.Completed == 0 || !openCodeFinished(latest.Finish) {
		return fmt.Errorf("OpenCode 저장 기록에서 이번 응답의 완료를 확인하지 못했어")
	}
	for _, message := range messages {
		if message.Agent != openCodeAgent(opts.Mode) {
			return fmt.Errorf("OpenCode 저장 기록의 응답 에이전트가 이번 요청과 달라")
		}
	}
	// Replace streamed counters rather than count the same response twice.
	p.usage = map[string]any{}
	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		part := map[string]any{"tokens": message.Tokens}
		if message.Cost != nil {
			part["cost"] = *message.Cost
		}
		p.addUsage(part)
	}
	p.completed = true
	return nil
}
