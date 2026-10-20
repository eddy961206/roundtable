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
	roomLimit := int64(maxListRooms)
	participant := object(map[string]schema{"provider": text, "model": str, "effort": str, "label": str, "role": str}, "provider")
	all := []tool{
		{Name: "rt_list_rooms", Description: fmt.Sprintf("최근 갱신순 방 id·제목·모드·실행 여부와 좌석(id·이름·공급사·모델·enabled) 조회. 삭제된 좌석은 빠지고 enabled:false는 꺼진 좌석. limit 기본 %d, 최대 %d", defaultListRooms, maxListRooms), InputSchema: object(map[string]schema{"limit": integer(1, &roomLimit)})},
		{Name: "rt_create_room", Description: "새 방 생성. 참가자의 모델·추론 수준을 생략하면 HTTP API 기본값 사용", InputSchema: object(map[string]schema{
			"title": text, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}},
			"participants": {Type: "array", Items: &participant, MinItems: 1},
		}, "title", "workspace", "mode", "participants")},
		{Name: "rt_post", Description: "메시지 전송. 결과의 seq는 보낸 메시지 번호. target은 좌석 id(rt_list_rooms의 participants[].id)이며 생략하면 방 규칙대로 전달. wait_seconds(0~600) 생략·0이면 기다리지 않음. 지정하면 이 메시지로 시작된 턴과 연쇄 호출 턴이 끝날 때까지 대기하고 답변도 반환. 실행 중인 대상에 끼워 넣은 입력(steering)이면 그 답의 seq가 보낸 seq보다 낮을 수 있음. 시간 초과(timed_out)는 실행 취소가 아니며, 그때 결과의 resume_after_seq를 rt_read의 after_seq로 써서 이어 읽음", InputSchema: object(map[string]schema{
			"room": id, "text": text, "target": id, "wait_seconds": integer(0, &limit),
		}, "room", "text")},
		{Name: "rt_read", Description: "메시지를 오래된 순으로 조회. after_seq가 있으면 그보다 큰 seq만, 생략하면 최신 메시지까지. max_chars(기본 20000, 본문 합계의 Unicode 문자 수)는 최신 메시지부터 채움: 경계 메시지는 앞을 줄여 truncated, 담지 못한 오래된 메시지는 빼고 omitted_messages(개수)·first_seq(담긴 첫 seq)로 알림. reply_to는 그 답이 응답한 메시지의 seq", InputSchema: object(map[string]schema{
			"room": id, "after_seq": integer(0, nil), "max_chars": integer(1, nil),
		}, "room")},
		{Name: "rt_discuss", Description: "토론 시작. blind_first 생략 시 방의 기본 설정 사용. 좌석들이 이미 같은 질문에 답했다면 블라인드 1라운드는 독립 답을 다시 받느라 호출을 좌석 수(N)만큼 더 쓰므로 blind_first:false를 고려", InputSchema: object(map[string]schema{
			"room": id, "rounds": integer(1, nil), "blind_first": {Type: "boolean"},
		}, "room", "rounds")},
		{Name: "rt_requests", Description: "참가자 간 요청 목록과 상태 조회", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_cancel_request", Description: "참가자 간 요청 취소", InputSchema: object(map[string]schema{"room": id, "request": id}, "room", "request")},
		{Name: "rt_decisions", Description: "결정 카드 목록 조회. status: proposed(좌석이 제안, 확정 대기)·active(안내문의 현재 결정)·superseded·dropped", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_update_decision", Description: "결정 카드 수정·상태 변경. 좌석이 [[DECISION]]으로 남긴 proposed 카드는 status active로 확정, dropped로 거절. 생략한 항목은 그대로이며 하나 이상 필요", InputSchema: object(map[string]schema{
			"room": id, "id": id, "status": {Type: "string", Enum: []string{"active", "superseded", "dropped"}},
			"text": text, "evidence": str, "dissent": str, "unverified": str,
		}, "room", "id")},
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
	Limit        int                `json:"limit"`
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
	ID           string             `json:"id"`
}

const (
	defaultListRooms = 20
	maxListRooms     = 100
)

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
	if a.Limit == 0 {
		a.Limit = defaultListRooms
	}
	base := "/api/rooms/" + a.Room
	switch name {
	case "rt_list_rooms":
		return s.listRooms(ctx, a.Limit)
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
	case "rt_update_decision":
		// The validated input minus the path ids is the PATCH body; omitted
		// fields stay unchanged on the server.
		body := map[string]any{}
		for key, v := range value.(map[string]any) {
			if key != "room" && key != "id" {
				body[key] = v
			}
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("바꿀 항목(status·text·evidence·dissent·unverified)이 하나 이상 필요함")
		}
		var result json.RawMessage
		err := s.api(ctx, "PATCH", base+"/decisions/"+a.ID, body, &result)
		return result, err
	}
	return nil, fmt.Errorf("알 수 없는 도구: %s", name)
}
