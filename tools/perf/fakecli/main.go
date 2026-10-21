// Command fakecli emits a deterministic Claude-compatible stream for local
// performance and lifecycle tests. It never connects to an AI provider.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

type scenario struct {
	Tokens  int `json:"tokens"`
	DelayUS int `json:"delay_us"`
	Tools   int `json:"tools"`
}

func main() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println("roundtable deterministic perf fixture 1")
			return
		}
	}
	var input struct {
		Message struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(bufio.NewReader(os.Stdin)).Decode(&input); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := scenario{Tokens: 128, Tools: 4}
	for _, block := range input.Message.Content {
		// Use the last marker: resumed prompts also contain prior messages.
		if at := strings.LastIndex(block.Text, "PERF_JSON "); at >= 0 {
			if err := json.NewDecoder(strings.NewReader(block.Text[at+10:])).Decode(&cfg); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
		}
	}
	if cfg.Tokens < 1 || cfg.Tokens > 10000 || cfg.DelayUS < 0 || cfg.DelayUS > 100000 || cfg.Tools < 0 || cfg.Tools > 1000 {
		fmt.Fprintln(os.Stderr, "fixture workload out of bounds")
		os.Exit(1)
	}
	session := fmt.Sprintf("perf-session-%d", os.Getpid())
	for i, arg := range os.Args[1:] {
		if arg == "--resume" && i+2 < len(os.Args) {
			session = os.Args[i+2]
		}
	}
	enc := json.NewEncoder(os.Stdout)
	emit := func(v any) {
		if err := enc.Encode(v); err != nil {
			os.Exit(1)
		}
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": session, "model": "perf-fixture"})
	for i := 0; i < cfg.Tools; i++ {
		id := fmt.Sprintf("tool-%d", i)
		emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Read", "input": map[string]any{"file_path": "synthetic.txt"}}}}})
		emit(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": "synthetic result"}}}})
	}
	var answer strings.Builder
	for i := 0; i < cfg.Tokens; i++ {
		text := fmt.Sprintf("piece-%04d: 검증용 **stream** data.\n", i)
		answer.WriteString(text)
		emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": text}}})
		if cfg.DelayUS > 0 {
			time.Sleep(time.Duration(cfg.DelayUS) * time.Microsecond)
		}
	}
	emit(map[string]any{"type": "assistant", "message": map[string]any{"model": "perf-fixture", "content": []any{map[string]any{"type": "text", "text": answer.String()}}}})
	emit(map[string]any{"type": "result", "subtype": "success", "session_id": session, "is_error": false, "result": answer.String(), "usage": map[string]any{"input_tokens": 100, "output_tokens": cfg.Tokens}})
}
