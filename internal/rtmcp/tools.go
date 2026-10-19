package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The same schema describes tools/list and validates tools/call. Only the
// JSON Schema subset used by these tools is evaluated here.
type schema struct {
	Type                 string            `json:"type"`
	Properties           map[string]schema `json:"properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	AdditionalProperties bool              `json:"additionalProperties"`
	Items                *schema           `json:"items,omitempty"`
	Enum                 []string          `json:"enum,omitempty"`
	Minimum              *int64            `json:"minimum,omitempty"`
	Maximum              *int64            `json:"maximum,omitempty"`
	MinLength            int               `json:"minLength,omitempty"`
	MinItems             int               `json:"minItems,omitempty"`
	Pattern              string            `json:"pattern,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema schema          `json:"inputSchema"`
	Annotations map[string]bool `json:"annotations"`
}

var regexpID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func object(props map[string]schema, required ...string) schema {
	return schema{Type: "object", Properties: props, Required: required}
}

func integer(minimum int64, maximum *int64) schema {
	return schema{Type: "integer", Minimum: &minimum, Maximum: maximum}
}

func (s *Server) tools() []tool {
	id := schema{Type: "string", MinLength: 1, Pattern: regexpID.String()}
	text := schema{Type: "string", MinLength: 1}
	str := schema{Type: "string"}
	limit := int64(600)
	participant := object(map[string]schema{"provider": text, "model": str, "effort": str, "label": str, "role": str}, "provider")
	all := []tool{
		{Name: "rt_list_rooms", Description: "방 id·제목·모드·참가자·실행 여부 조회. 좌석의 enabled·removed로 활성 여부 구분", InputSchema: object(map[string]schema{})},
		{Name: "rt_create_room", Description: "새 방 생성. 참가자의 모델·추론 수준을 생략하면 HTTP API 기본값 사용", InputSchema: object(map[string]schema{
			"title": text, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}},
			"participants": {Type: "array", Items: &participant, MinItems: 1},
		}, "title", "workspace", "mode", "participants")},
		{Name: "rt_post", Description: "메시지 전송. wait_seconds(0~600)를 지정하면 steering 대상과 연쇄 호출 턴까지 대기하고 답변도 반환. steering 응답은 전송 seq보다 낮을 수 있음. 시간 초과는 실행 취소가 아님", InputSchema: object(map[string]schema{
			"room": id, "text": text, "target": id, "wait_seconds": integer(0, &limit),
		}, "room", "text")},
		{Name: "rt_read", Description: "after_seq 이후 메시지를 오래된 순으로 조회. max_chars(기본 20000)는 본문 합계의 Unicode 문자 수이며 오래된 본문부터 줄이고 truncated로 표시", InputSchema: object(map[string]schema{
			"room": id, "after_seq": integer(0, nil), "max_chars": integer(1, nil),
		}, "room")},
		{Name: "rt_discuss", Description: "토론 시작. blind_first 생략 시 방의 기본 설정 사용", InputSchema: object(map[string]schema{
			"room": id, "rounds": integer(1, nil), "blind_first": {Type: "boolean"},
		}, "room", "rounds")},
		{Name: "rt_requests", Description: "참가자 간 요청 목록과 상태 조회", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_cancel_request", Description: "참가자 간 요청 취소", InputSchema: object(map[string]schema{"room": id, "request": id}, "room", "request")},
		{Name: "rt_decisions", Description: "결정 카드 목록 조회", InputSchema: object(map[string]schema{"room": id}, "room")},
	}
	out := make([]tool, 0, len(all))
	for _, t := range all {
		read := slices.Contains([]string{"rt_list_rooms", "rt_read", "rt_requests", "rt_decisions"}, t.Name)
		if s.readOnly && !read {
			continue
		}
		t.Annotations = map[string]bool{"readOnlyHint": read, "openWorldHint": false}
		out = append(out, t)
	}
	return out
}

func (s schema) validate(value any, path string) error {
	bad := func(reason string) error { return fmt.Errorf("%s: %s", path, reason) }
	switch s.Type {
	case "object":
		v, ok := value.(map[string]any)
		if !ok {
			return bad("객체여야 함")
		}
		for _, key := range s.Required {
			if _, ok := v[key]; !ok {
				return bad(key + " 필요")
			}
		}
		for key, value := range v {
			prop, ok := s.Properties[key]
			if !ok {
				return bad("알 수 없는 입력: " + key)
			}
			if err := prop.validate(value, path+"."+key); err != nil {
				return err
			}
		}
	case "array":
		v, ok := value.([]any)
		if !ok || len(v) < s.MinItems {
			return bad("항목이 있는 배열이어야 함")
		}
		for i, value := range v {
			if err := s.Items.validate(value, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		v, ok := value.(string)
		if !ok || (s.MinLength > 0 && strings.TrimSpace(v) == "") {
			return bad("문자열이 필요하며 필수 값은 비울 수 없음")
		}
		if len(s.Enum) > 0 && !slices.Contains(s.Enum, v) {
			return bad("허용 값: " + strings.Join(s.Enum, ", "))
		}
		if s.Pattern != "" && !regexp.MustCompile(s.Pattern).MatchString(v) {
			return bad("id는 영문·숫자·밑줄·하이픈만 허용")
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return bad("정수여야 함")
		}
		v, err := n.Int64()
		if err != nil || (s.Minimum != nil && v < *s.Minimum) || (s.Maximum != nil && v > *s.Maximum) {
			return bad("정수 허용 범위를 벗어남")
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return bad("bool이어야 함")
		}
	}
	return nil
}

type participantInput struct {
	Provider string  `json:"provider"`
	Model    string  `json:"model,omitempty"`
	Effort   *string `json:"effort,omitempty"`
	Label    string  `json:"label,omitempty"`
	Role     string  `json:"role,omitempty"`
}

type arguments struct {
	Room         string             `json:"room"`
	Title        string             `json:"title"`
	Workspace    string             `json:"workspace"`
	Mode         string             `json:"mode"`
	Participants []participantInput `json:"participants"`
	Text         string             `json:"text"`
	Target       string             `json:"target"`
	WaitSeconds  *int               `json:"wait_seconds"`
	AfterSeq     int64              `json:"after_seq"`
	MaxChars     int64              `json:"max_chars"`
	Rounds       int                `json:"rounds"`
	BlindFirst   *bool              `json:"blind_first"`
	Request      string             `json:"request"`
}

func (s *Server) call(ctx context.Context, name string, raw json.RawMessage) (any, error) {
	var selected *tool
	for _, t := range s.tools() {
		if t.Name == name {
			selected = &t
			break
		}
	}
	if selected == nil {
		return nil, fmt.Errorf("사용할 수 없는 도구: %s", name)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("입력 JSON 오류: %w", err)
	}
	if err := selected.InputSchema.validate(value, name); err != nil {
		return nil, err
	}
	var a arguments
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("입력 오류: %w", err)
	}
	if a.MaxChars == 0 {
		a.MaxChars = 20000
	}
	base := "/api/rooms/" + a.Room
	switch name {
	case "rt_list_rooms":
		return s.listRooms(ctx)
	case "rt_create_room":
		if a.Mode != "chat" && strings.TrimSpace(a.Workspace) == "" {
			return nil, fmt.Errorf("read/write 모드는 workspace가 필요함")
		}
		var created struct {
			ID string `json:"id"`
		}
		err := s.api(ctx, "POST", "/api/rooms", map[string]any{"title": a.Title, "workspace": a.Workspace, "mode": a.Mode, "participant_specs": a.Participants}, &created)
		if err != nil {
			return nil, err
		}
		if created.ID == "" {
			return nil, fmt.Errorf("방 생성 응답에 id가 없음")
		}
		return map[string]string{"room": created.ID}, nil
	case "rt_post":
		return s.post(ctx, a)
	case "rt_read":
		return s.read(ctx, a.Room, a.AfterSeq, a.MaxChars)
	case "rt_discuss":
		body := map[string]any{"rounds": a.Rounds}
		if a.BlindFirst != nil {
			body["blind_first"] = *a.BlindFirst
		}
		var result json.RawMessage
		err := s.api(ctx, "POST", base+"/discussion", body, &result)
		return result, err
	case "rt_requests":
		page, err := s.snapshot(ctx, a.Room, 0, 1)
		if err != nil {
			return nil, err
		}
		if page.Requests == nil {
			page.Requests = []json.RawMessage{}
		}
		return map[string]any{"requests": page.Requests}, nil
	case "rt_cancel_request":
		var result json.RawMessage
		err := s.api(ctx, "POST", base+"/requests/"+a.Request+"/cancel", map[string]any{}, &result)
		return result, err
	case "rt_decisions":
		var result []json.RawMessage
		if err := s.api(ctx, "GET", base+"/decisions", nil, &result); err != nil {
			return nil, err
		}
		if result == nil {
			result = []json.RawMessage{}
		}
		return map[string]any{"decisions": result}, nil
	}
	return nil, fmt.Errorf("알 수 없는 도구: %s", name)
}
