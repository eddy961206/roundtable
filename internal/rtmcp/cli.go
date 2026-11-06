package rtmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
)

// RunCLI runs one MCP tool from a shell: `<tool> --key value ...`. The tool
// list and argument names are the MCP ones; wait permits longer shell waits.
func (s *Server) RunCLI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		s.cliUsage(stdout)
		return 0
	}
	var selected *tool
	for _, t := range s.tools() {
		if t.Name == "rt_"+args[0] {
			selected = &t
			break
		}
	}
	if selected == nil {
		fmt.Fprintf(stderr, "알 수 없는 명령: %s (`roundtable rt help`로 목록 확인)\n", args[0])
		return 2
	}
	if slices.Contains(args[1:], "--help") || slices.Contains(args[1:], "-h") {
		toolUsage(stdout, *selected)
		return 0
	}
	input, err := cliArguments(selected.InputSchema, args[1:], stdin)
	if err != nil {
		fmt.Fprintf(stderr, "입력 오류: %v (`roundtable rt %s --help`)\n", err, args[0])
		return 2
	}
	raw, err := json.Marshal(input)
	if err != nil {
		panic(err) // built from strings, numbers, booleans and decoded JSON
	}
	var result any
	if selected.Name == "rt_wait" {
		result, err = s.cliWait(ctx, input, 3600)
	} else {
		result, err = s.call(ctx, selected.Name, raw)
	}
	if err != nil {
		fmt.Fprintf(stderr, "오류: %v\n", err)
		return 1
	}
	out, err := json.Marshal(result)
	if err != nil {
		fmt.Fprintf(stderr, "결과 직렬화 실패: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s\n", out)
	return 0
}

// cliWait keeps the MCP limit on each call while allowing a longer shell wait.
// chunkSeconds is an argument so tests can exercise multiple chunks quickly.
func (s *Server) cliWait(ctx context.Context, input map[string]any, chunkSeconds int64) (any, error) {
	return cliWaitChunks(ctx, input, chunkSeconds, s.call)
}

func cliWaitChunks(ctx context.Context, input map[string]any, chunkSeconds int64, call func(context.Context, string, json.RawMessage) (any, error)) (any, error) {
	total, specified := input["timeout_seconds"].(int64)
	if !specified {
		total = 600
	}
	if total < 1 || total > 86400 {
		return nil, fmt.Errorf("rt_wait.timeout_seconds: 정수 허용 범위를 벗어남 (1~86400)")
	}
	for remaining := total; ; {
		chunk := min(remaining, chunkSeconds)
		input["timeout_seconds"] = chunk
		raw, err := json.Marshal(input)
		if err != nil {
			return nil, err
		}
		value, err := call(ctx, "rt_wait", raw)
		if err != nil {
			return nil, err
		}
		result := value.(*waitResult)
		remaining -= chunk
		if !result.TimedOut || len(result.Ready) != 0 || result.Asked || result.UserRequested || remaining == 0 {
			return result, nil
		}
		input["rooms"] = result.Waiting
	}
}

// cliArguments turns `--key value` / `--key=value` pairs into tool input using
// the schema types. A string value `-` is read from stdin (once), for text that
// does not fit a command line.
func cliArguments(sc schema, args []string, stdin io.Reader) (map[string]any, error) {
	input := map[string]any{}
	stdinUsed := false
	for i := 0; i < len(args); i++ {
		key, value, inline := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") || key == "" {
			return nil, fmt.Errorf("%q: --이름 값 형식이어야 함", args[i])
		}
		if !inline {
			if i+1 == len(args) {
				return nil, fmt.Errorf("--%s 값이 없음", key)
			}
			i++
			value = args[i]
		}
		prop, ok := sc.Properties[key]
		if !ok {
			return nil, fmt.Errorf("알 수 없는 인자 --%s", key)
		}
		if _, dup := input[key]; dup {
			return nil, fmt.Errorf("--%s를 두 번 지정함", key)
		}
		switch prop.Type {
		case "string":
			if value == "-" {
				if stdinUsed {
					return nil, fmt.Errorf("stdin(-)은 한 인자에만 쓸 수 있음")
				}
				b, err := io.ReadAll(stdin)
				if err != nil {
					return nil, fmt.Errorf("stdin 읽기 실패: %w", err)
				}
				value, stdinUsed = string(b), true
			}
			input[key] = value
		case "integer":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("--%s는 정수여야 함: %q", key, value)
			}
			input[key] = n
		case "boolean":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return nil, fmt.Errorf("--%s는 true 또는 false여야 함: %q", key, value)
			}
			input[key] = b
		default: // array, object: JSON text
			var v any
			if err := json.Unmarshal([]byte(value), &v); err != nil {
				return nil, fmt.Errorf("--%s는 JSON이어야 함: %w", key, err)
			}
			input[key] = v
		}
	}
	return input, nil
}

func (s *Server) cliUsage(w io.Writer) {
	fmt.Fprintln(w, "사용법: roundtable rt [--url http://127.0.0.1:8787] <명령> [--인자 값 ...]")
	fmt.Fprintln(w, "결과는 한 줄 JSON(stdout). 실패하면 사유를 stderr에 쓰고 종료 코드 1(검증·실행) 또는 2(명령 형식). 문자열 값 - 는 stdin에서 읽음.")
	fmt.Fprintln(w, "명령:")
	// One line a command keeps the list cheap to read (the full descriptions
	// are about as long as the MCP tool list); <명령> --help has the rest.
	for _, t := range s.tools() {
		first, _, _ := strings.Cut(t.Description, ". ")
		fmt.Fprintf(w, "  %-15s %s\n", strings.TrimPrefix(t.Name, "rt_"), first)
	}
	fmt.Fprintln(w, "명령별 설명·인자: roundtable rt <명령> --help")
}

func toolUsage(w io.Writer, t tool) {
	fmt.Fprintf(w, "%s: %s\n인자:\n", strings.TrimPrefix(t.Name, "rt_"), t.Description)
	keys := make([]string, 0, len(t.InputSchema.Properties))
	for k := range t.InputSchema.Properties {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		p := t.InputSchema.Properties[k]
		kind := p.Type
		if kind == "array" || kind == "object" {
			kind += "(JSON)"
		}
		if len(p.Enum) > 0 {
			kind = strings.Join(p.Enum, "|")
		}
		need := ""
		if slices.Contains(t.InputSchema.Required, k) {
			need = " (필수)"
		}
		fmt.Fprintf(w, "  --%s %s%s\n", k, kind, need)
	}
	if t.Name == "rt_create_room" {
		fmt.Fprintln(w, `  participants 예: [{"provider":"agy","model":"gemini-4-argon","effort":"high","label":"Argon","role":"문구 검토"}]`)
		fmt.Fprintln(w, `  기반 세션 포크 예: [{"provider":"codex","label":"작성","fork_from":{"room":"<기반 방ID>","seat":"<기반 좌석ID>"}}]`)
	}
	if t.Name == "rt_wait" {
		fmt.Fprintln(w, "  셸은 최대 86400초(1시간 조각으로 이어 대기), MCP는 1~3600초.")
	}
}
