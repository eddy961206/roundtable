package rtmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
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
	hint                 string            // the validation message for Pattern
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
	id := schema{Type: "string", MinLength: 1, Pattern: regexpID.String(), hint: "id는 영문·숫자·밑줄·하이픈만 허용"}
	waitList := schema{Type: "string", MinLength: 1, Pattern: waitRooms.String(), hint: "rooms는 방ID 또는 방ID:after_seq를 쉼표로 이은 값"}
	usageList := schema{Type: "string", MinLength: 1, Pattern: roomList.String(), hint: "rooms는 방ID를 쉼표로 이은 값"}
	waitLimit := int64(3600)
	text := schema{Type: "string", MinLength: 1}
	str := schema{Type: "string"}
	limit := int64(600)
	roomLimit := int64(maxListRooms)
	fork := object(map[string]schema{"room": id, "seat": id}, "room", "seat")
	participant := object(map[string]schema{"provider": text, "model": str, "effort": str, "label": str, "role": str, "fork_from": fork}, "provider")
	all := []tool{
		{Name: "rt_list_rooms", Description: fmt.Sprintf("최근 갱신순 방 id·제목·모드·실행 여부와 좌석(id·이름·공급사·모델·enabled) 조회. 삭제된 좌석은 빠지고 enabled:false는 꺼진 좌석. limit 기본 %d, 최대 %d", defaultListRooms, maxListRooms), InputSchema: object(map[string]schema{"limit": integer(1, &roomLimit)})},
		{Name: "rt_create_room", Description: "새 방 생성. 결과는 방 id와 좌석 목록(id·이름·공급사·모델·enabled). 참가자의 모델·추론 수준을 생략하면 HTTP API 기본값 사용. fork_from {room, seat}을 주면 그 좌석의 세션(공통 문서를 읽힌 기반 세션)을 복사해 첫 턴을 시작하므로 같은 문서를 다시 읽지 않음. 포크 좌석은 공급사·모델이 기반 좌석과 같아야 함(생략 시 기반 좌석 값). 작업 폴더·모드는 달라도 됨(다른 worktree면 첫 턴에 두 폴더를 알려 줌). 포크마다 문맥이 따로라 작성·검수 좌석을 같은 기반에서 각각 포크해도 서로의 작업을 보지 않음. agy는 포크 미지원. OpenCode 좌석은 read 모드에서 셸이 없고 작업 폴더 밖을 읽지 못하므로, 시험을 직접 돌리는 검수는 write 모드에 검수 대상 폴더를 작업 폴더로 준다", InputSchema: object(map[string]schema{
			"title": text, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}},
			"participants": {Type: "array", Items: &participant, MinItems: 1},
		}, "title", "workspace", "mode", "participants")},
		{Name: "rt_post", Description: "메시지 전송. 결과의 seq는 보낸 메시지 번호. target은 좌석 id(rt_list_rooms의 participants[].id)이며 생략하면 방 규칙대로 전달. wait_seconds(0~600) 생략·0이면 기다리지 않음. 지정하면 이 메시지로 시작된 턴과 연쇄 호출 턴이 끝날 때까지 대기하고 답변도 반환. 실행 중인 대상에 끼워 넣은 입력(steering)이면 그 답의 seq가 보낸 seq보다 낮을 수 있음. 시간 초과(timed_out)는 실행 취소가 아니며, 그때 결과의 resume_after_seq를 rt_read의 after_seq로 써서 이어 읽음", InputSchema: object(map[string]schema{
			"room": id, "text": text, "target": id, "wait_seconds": integer(0, &limit),
		}, "room", "text")},
		{Name: "rt_read", Description: "메시지를 오래된 순으로 조회. after_seq가 있으면 그보다 큰 seq만, 생략하면 최신 메시지까지. max_chars(기본 20000, 본문 합계의 Unicode 문자 수)는 최신 메시지부터 채움: 경계 메시지는 앞을 줄여 truncated, 담지 못한 오래된 메시지는 빼고 omitted_messages(개수)·first_seq(담긴 첫 seq)로 알림. reply_to는 그 답이 응답한 메시지의 seq", InputSchema: object(map[string]schema{
			"room": id, "after_seq": integer(0, nil), "max_chars": integer(1, nil),
		}, "room")},
		{Name: "rt_wait", Description: "여러 방 중 하나 이상이 끝날 때까지 대기. rooms는 \"방ID:after_seq,방ID:after_seq\"(after_seq는 rt_post 결과의 seq; 생략하면 대기 시작 시점 이후 답만 셈). 방이 끝났다는 뜻은 실행·대기 중인 턴이 없고 after_seq 뒤에 좌석 답이 있다는 것. 결과 ready[]는 끝난 방마다 rt_read와 같은 형식(after_seq 뒤 메시지, 방마다 max_chars 예산, next_seq), waiting은 남은 방을 다음 rt_wait의 rooms로 그대로 쓸 수 있는 값. timeout_seconds(1~3600, 기본 600)가 지나면 timed_out:true. 폴링 없이 서버 SSE로 깨어남. 긴 대기는 셸 백그라운드 실행을 권장", InputSchema: object(map[string]schema{
			"rooms": waitList, "timeout_seconds": integer(1, &waitLimit), "max_chars": integer(1, nil),
		}, "rooms")},
		{Name: "rt_usage", Description: "방별 토큰 사용량 합계와 좌석별 내역. rooms는 쉼표로 이은 방ID. 공급사별 차이를 맞춘 값: input_tokens(처리한 입력 전체)=cached_tokens(캐시 읽기)+uncached_tokens(나머지, 캐시 쓰기 포함), output_tokens, cost_usd(보고하는 공급사만), max_context_tokens(턴의 가장 큰 단일 호출), turns·errors·duration_ms. totals는 지정한 방 전체 합", InputSchema: object(map[string]schema{"rooms": usageList}, "rooms")},
		{Name: "rt_version", Description: "실행 중인 Roundtable 빌드와 공개 브랜치 비교(GET /api/version). state: behind면 update_available:true이고 behind_by·commits(최근 5개, 최신순)가 새 커밋. up_to_date·ahead·diverged·unknown_revision(원격에 없는 로컬 커밋)·local_build(빌드에 커밋 정보 없음)·pending·off·error. running.modified는 커밋하지 않은 변경이 있던 빌드. checked_at은 마지막 확인 시각(ms). 서버가 주기적으로 확인한 값을 읽기만 하며 GitHub를 직접 부르지 않음", InputSchema: object(map[string]schema{})},
		{Name: "rt_discuss", Description: "토론 시작. blind_first 생략 시 방의 기본 설정 사용. 좌석들이 이미 같은 질문에 답했다면 블라인드 1라운드는 독립 답을 다시 받느라 호출을 좌석 수(N)만큼 더 쓰므로 blind_first:false를 고려", InputSchema: object(map[string]schema{
			"room": id, "rounds": integer(1, nil), "blind_first": {Type: "boolean"},
		}, "room", "rounds")},
		{Name: "rt_requests", Description: "참가자 간 요청 목록과 상태 조회", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_cancel_request", Description: "참가자 간 요청 취소", InputSchema: object(map[string]schema{"room": id, "request": id}, "room", "request")},
		{Name: "rt_cancel_turn", Description: "좌석의 실행 중인 턴 하나만 취소. 좌석은 켜진 채로 남아 이후 [[REQUEST]]·멘션·대기 턴을 계속 받음(웹의 '작업 중단'은 좌석까지 끔). 실행 중인 턴이 없으면 오류. 결과의 cancelled는 취소한 턴 id", InputSchema: object(map[string]schema{"room": id, "seat": id}, "room", "seat")},
		{Name: "rt_apply_markers", Description: "오류로 끝난 좌석 답(seq)의 마커([[REQUEST]]·[[REQUEST_DONE]]·[[DECISION]]·[[ATTACH]])를 실행. 서버는 오류 턴의 마커를 스스로 처리하지 않고 시스템 안내로 알림. 답이 끝까지 쓰인 것을 확인했을 때만 호출(예: 답 뒤에 stream idle timeout). 한 번만 실행되며 결과의 message.requests가 새 요청", InputSchema: object(map[string]schema{"room": id, "seq": integer(1, nil)}, "room", "seq")},
		{Name: "rt_decisions", Description: "결정 카드 목록 조회. status: proposed(좌석이 제안, 확정 대기)·active(안내문의 현재 결정)·superseded·dropped", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_update_room", Description: "방 설정 수정. 생략한 항목은 그대로이며 하나 이상 필요. 작업 폴더(git worktree)가 지워지거나 옮겨졌을 때 방과 좌석 세션을 유지한 채 workspace를 새 폴더로 바꾼다. 실행 중인 턴에는 다음 턴부터 적용", InputSchema: object(map[string]schema{
			"room": id, "title": text, "note": str, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}},
		}, "room")},
		{Name: "rt_update_decision", Description: "결정 카드 수정·상태 변경. 좌석이 [[DECISION]]으로 남긴 proposed 카드는 status active로 확정, dropped로 거절. 생략한 항목은 그대로이며 하나 이상 필요", InputSchema: object(map[string]schema{
			"room": id, "id": id, "status": {Type: "string", Enum: []string{"active", "superseded", "dropped"}},
			"text": text, "evidence": str, "dissent": str, "unverified": str,
		}, "room", "id")},
	}
	out := make([]tool, 0, len(all))
	for _, t := range all {
		read := slices.Contains([]string{"rt_list_rooms", "rt_read", "rt_wait", "rt_usage", "rt_version", "rt_requests", "rt_decisions"}, t.Name)
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
			return bad(s.hint)
		}
	case "integer":
		n, ok := value.(json.Number)
		if !ok {
			return bad("정수여야 함")
		}
		v, err := n.Int64()
		if err != nil || (s.Minimum != nil && v < *s.Minimum) || (s.Maximum != nil && v > *s.Maximum) {
			return bad("정수 허용 범위를 벗어남" + s.rangeHint())
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
	ForkFrom *struct {
		Room string `json:"room"`
		Seat string `json:"seat"`
	} `json:"fork_from,omitempty"`
}

type arguments struct {
	Room         string             `json:"room"`
	Rooms        string             `json:"rooms"`
	Timeout      int                `json:"timeout_seconds"`
	Limit        int                `json:"limit"`
	Title        string             `json:"title"`
	Workspace    string             `json:"workspace"`
	Mode         string             `json:"mode"`
	Participants []participantInput `json:"participants"`
	Text         string             `json:"text"`
	Target       string             `json:"target"`
	WaitSeconds  *int               `json:"wait_seconds"`
	AfterSeq     int64              `json:"after_seq"`
	Seq          int64              `json:"seq"`
	MaxChars     int64              `json:"max_chars"`
	Rounds       int                `json:"rounds"`
	BlindFirst   *bool              `json:"blind_first"`
	Request      string             `json:"request"`
	Seat         string             `json:"seat"`
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
			ID           string `json:"id"`
			Participants []seat `json:"participants"`
		}
		err := s.api(ctx, "POST", "/api/rooms", map[string]any{"title": a.Title, "workspace": a.Workspace, "mode": a.Mode, "participant_specs": a.Participants}, &created)
		if err != nil {
			return nil, err
		}
		if created.ID == "" {
			return nil, fmt.Errorf("방 생성 응답에 id가 없음")
		}
		// Seat ids are what post targets and a later fork_from name.
		seats := make([]listSeat, 0, len(created.Participants))
		for _, sp := range created.Participants {
			seats = append(seats, listSeat{ID: sp.ID, Label: sp.Label, Provider: sp.Provider, Model: sp.Model, Enabled: sp.Enabled})
		}
		return map[string]any{"room": created.ID, "participants": seats}, nil
	case "rt_post":
		return s.post(ctx, a)
	case "rt_read":
		return s.read(ctx, a.Room, a.AfterSeq, a.MaxChars)
	case "rt_wait":
		if a.Timeout == 0 {
			a.Timeout = 600
		}
		return s.wait(ctx, a.Rooms, time.Duration(a.Timeout)*time.Second, a.MaxChars)
	case "rt_usage":
		return s.usage(ctx, a.Rooms)
	case "rt_version":
		var result json.RawMessage
		err := s.api(ctx, "GET", "/api/version", nil, &result)
		return result, err
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
	case "rt_cancel_turn":
		page, err := s.snapshot(ctx, a.Room, 0, 1)
		if err != nil {
			return nil, err
		}
		for _, turn := range page.Queue {
			if turn.Agent == a.Seat && turn.Status == "running" {
				var ok json.RawMessage
				if err := s.api(ctx, "DELETE", base+"/turns/"+turn.ID, nil, &ok); err != nil {
					return nil, err
				}
				return map[string]any{"cancelled": turn.ID, "seat": a.Seat, "seat_enabled": true}, nil
			}
		}
		return nil, fmt.Errorf("@%s에 실행 중인 턴이 없음", a.Seat)
	case "rt_apply_markers":
		var result json.RawMessage
		err := s.api(ctx, "POST", base+"/markers", map[string]any{"seq": a.Seq}, &result)
		return result, err
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
	case "rt_update_room":
		body := map[string]any{}
		for key, v := range value.(map[string]any) {
			if key != "room" {
				body[key] = v
			}
		}
		if len(body) == 0 {
			return nil, fmt.Errorf("바꿀 항목(title·note·workspace·mode)이 하나 이상 필요함")
		}
		var result json.RawMessage
		err := s.api(ctx, "PATCH", base, body, &result)
		return result, err
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

// rangeHint names the accepted bounds so a caller can correct the value
// without reading the schema (for example list_rooms limit).
func (s schema) rangeHint() string {
	switch {
	case s.Minimum != nil && s.Maximum != nil:
		return fmt.Sprintf(" (%d~%d)", *s.Minimum, *s.Maximum)
	case s.Minimum != nil:
		return fmt.Sprintf(" (%d 이상)", *s.Minimum)
	case s.Maximum != nil:
		return fmt.Sprintf(" (%d 이하)", *s.Maximum)
	}
	return ""
}
