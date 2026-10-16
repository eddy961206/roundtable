package agent

import (
	"encoding/json"
	"strings"
)

const maxCapture = 64 * 1024

func str(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func obj(m map[string]any, key string) map[string]any {
	if m == nil {
		return nil
	}
	if o, ok := m[key].(map[string]any); ok {
		return o
	}
	return nil
}

func arr(m map[string]any, key string) []any {
	if m == nil {
		return nil
	}
	if a, ok := m[key].([]any); ok {
		return a
	}
	return nil
}

func compactJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return clip(string(b), maxCapture)
}

// contentText flattens Anthropic/Codex style content (string or list of
// {type:text|input_text|output_text, text}) into plain text.
func contentText(v any) string {
	switch c := v.(type) {
	case string:
		return clip(c, maxCapture)
	case []any:
		var parts []string
		for _, p := range c {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t := str(pm, "text"); t != "" {
				parts = append(parts, t)
			}
		}
		return clip(strings.Join(parts, "\n"), maxCapture)
	case map[string]any:
		if t := str(c, "text"); t != "" {
			return clip(t, maxCapture)
		}
		return compactJSON(c)
	case nil:
		return ""
	default:
		return compactJSON(c)
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "\n…[잘림]"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func parseLine(line string) map[string]any {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		return nil
	}
	return m
}
