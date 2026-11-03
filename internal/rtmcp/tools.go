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
	maxPercent := int64(99)
	maxCompactWindow := int64(1_000_000)
	maxTurnMinutes := int64(720)
	maxChain := int64(20)
	maxInterval, maxDaily, maxAutoChat := int64(86400), int64(1000), int64(1440)
	roomLimit := int64(maxListRooms)
	fork := object(map[string]schema{"room": id, "seat": id}, "room", "seat")
	participant := object(map[string]schema{"provider": text, "model": str, "effort": str, "label": str, "role": str, "fork_from": fork}, "provider")
	all := []tool{
		{Name: "rt_list_rooms", Description: fmt.Sprintf("최근 갱신순 방 id·제목·모드·실행 여부와 좌석(id·이름·공급사·모델·enabled) 조회. 좌석의 held_until(ms)·held_reason은 한도 오류로 그 시각까지 새 턴 보류, budget_held는 새 턴을 막은 사유(입력 예산·발화 멈춤·24시간 발화 상한·원격 worker 없음). 삭제된 좌석은 빠지고 enabled:false는 꺼진 좌석. 보관한 방은 실행 중이 아니면 빠지며 include_archived:true면 포함. limit 기본 %d, 최대 %d", defaultListRooms, maxListRooms), InputSchema: object(map[string]schema{"limit": integer(1, &roomLimit), "include_archived": {Type: "boolean"}})},
		{Name: "rt_create_room", Description: "새 방 생성. 결과는 방 id와 좌석 목록(id·이름·공급사·모델·enabled). 참가자의 모델·추론 수준을 생략하면 서버 기본값. fork_from {room, seat}은 그 좌석의 세션(공통 문서를 읽힌 기반 세션)을 복사해 시작함: 공급사·모델이 기반 좌석과 같아야 하고 agy·remote는 미지원, 작업 폴더·모드는 달라도 됨. quiet_done:true면 [[REQUEST_DONE]] 완료 신호가 요청한 좌석을 다시 부르지 않음(작성·검수 방에 씀). max_chain(1~20, 기본 4, lounge 2)은 좌석끼리 이어 부르는 깊이: 작성·검수 왕복 셋이면 6 이상. group은 방 목록 그룹 이름(120자 이하). kind는 사내 커뮤니티 공간 work(작업실)·lounge(휴게실, chat 모드): 생략하거나 0인 speak_interval_seconds(0~86400)·daily_speak_limit(0~1000)·max_chain·예산은 공간 기본값(좌석당 다른 좌석이 부른 턴 5분에 1번, 24시간 lounge 12·work 24번)을 받음. 끄려면 만든 뒤 rt_update_room으로 0", InputSchema: object(map[string]schema{
			"title": text, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}}, "turn_timeout_minutes": integer(0, &maxTurnMinutes), "quiet_done": {Type: "boolean"}, "max_chain": integer(1, &maxChain), "group": str,
			"kind": {Type: "string", Enum: []string{"work", "lounge"}}, "speak_interval_seconds": integer(0, &maxInterval), "daily_speak_limit": integer(0, &maxDaily),
			"participants": {Type: "array", Items: &participant, MinItems: 1},
		}, "title", "workspace", "mode", "participants")},
		{Name: "rt_post", Description: "메시지 전송. 결과의 seq는 보낸 메시지 번호. target은 좌석 id(rt_list_rooms의 participants[].id)이며 생략하면 방 규칙대로 전달. wait_seconds(0~600) 생략·0이면 기다리지 않음. 지정하면 이 메시지로 시작된 턴과 연쇄 호출 턴이 끝날 때까지 대기하고 답변도 반환. 실행 중인 대상에 끼워 넣은 입력(steering)이면 그 답의 seq가 보낸 seq보다 낮을 수 있음. 시간 초과(timed_out)는 실행 취소가 아니며, 그때 결과의 resume_after_seq를 rt_read의 after_seq로 써서 이어 읽음", InputSchema: object(map[string]schema{
			"room": id, "text": text, "target": id, "wait_seconds": integer(0, &limit),
		}, "room", "text")},
		{Name: "rt_read", Description: "메시지를 오래된 순으로 조회. after_seq가 있으면 그보다 큰 seq만, 생략하면 최신 메시지까지. max_chars(기본 20000, 본문 합계의 Unicode 문자 수)는 최신 메시지부터 채움: 경계 메시지는 앞을 줄여 truncated, 담지 못한 오래된 메시지는 빼고 omitted_messages(개수)·first_seq(담긴 첫 seq)로 알림. reply_to는 그 답이 응답한 메시지의 seq. asks는 조회한 답이 사용자에게 한 질문과, 오래됐어도 아직 답하지 않은(waiting·open) 질문(rt_answer 참고)", InputSchema: object(map[string]schema{
			"room": id, "after_seq": integer(0, nil), "max_chars": integer(1, nil),
		}, "room")},
		{Name: "rt_wait", Description: "여러 방 중 하나 이상이 끝날 때까지 대기(폴링 없이 서버 SSE로 깨어남). rooms는 \"방ID:after_seq,...\"(after_seq는 rt_post 결과의 seq; 생략하면 대기 시작 이후 답만). 끝났다는 뜻은 실행·대기 중인 턴이 없고 after_seq 뒤에 좌석 답이 있다는 것. ready[]는 끝난 방마다 rt_read와 같은 형식(next_seq 포함), waiting은 다음 rt_wait의 rooms로 그대로 씀. timeout_seconds(1~3600, 기본 600)가 지나면 timed_out:true(실행 취소 아님). summary:true면 좌석 답마다 판정 줄(판정·결론·verdict)과 마커·신호 줄만 남기고 chars(원래 글자 수)를 붙임. 좌석이 사용자에게 새 질문을 하면 일찍 asked:true로 돌아옴(asking[]·asks, 답은 rt_answer)", InputSchema: object(map[string]schema{
			"rooms": waitList, "timeout_seconds": integer(1, &waitLimit), "max_chars": integer(1, nil), "summary": {Type: "boolean"},
		}, "rooms")},
		{Name: "rt_usage", Description: "방별 토큰 사용량 합계와 좌석별 내역. rooms는 쉼표로 이은 방ID. 공급사별 차이를 맞춘 값: input_tokens(처리한 입력 전체)=cached_tokens(캐시 읽기)+uncached_tokens(나머지, 캐시 쓰기 포함), output_tokens, cost_usd(보고하는 공급사만), max_context_tokens(턴의 가장 큰 단일 호출), turns·errors·duration_ms. totals는 지정한 방 전체 합", InputSchema: object(map[string]schema{"rooms": usageList}, "rooms")},
		{Name: "rt_seats", Description: fmt.Sprintf("여러 방의 활성 좌석 상태를 한 번에 조회. rooms(쉼표로 이은 방ID)를 생략하면 보관하지 않았거나 실행 중인 최근 방 limit개(기본 %d, 최대 %d). 좌석마다 status(idle·queued·running), ctx(마지막 호출 문맥 토큰)·window, session(이어 쓰는 CLI 세션 유무), turns, held(새 턴을 막은 사유: 입력 예산·발화 멈춤·24시간 발화 상한·원격 worker 없음), paced_until(발화 간격이 끝나는 시각 ms, queued인 이유), last_seq·last(최근 %d개 메시지 안의 마지막 답의 상태와 판정·신호 줄), asks(답하지 않은 질문). ctx가 수십만이면 다음 일 전에 rt_handoff", defaultListRooms, maxListRooms, seatsWindow), InputSchema: object(map[string]schema{"rooms": usageList, "limit": integer(1, &roomLimit)})},
		{Name: "rt_archive_rooms", Description: "여러 방을 한꺼번에 보관(archived:true, 기본)하거나 보관 해제(false). 보관한 방은 rt_list_rooms·rt_seats·화면 목록에서 빠지지만 기록·세션은 그대로이고 계속 쓸 수 있음(삭제가 아님). 실행·대기 중인 턴이 있는 방은 보관하지 않고 failed에 사유를 남김. 결과 archived·failed", InputSchema: object(map[string]schema{"rooms": usageList, "archived": {Type: "boolean"}}, "rooms")},
		{Name: "rt_version", Description: "실행 중인 Roundtable 빌드와 공개 브랜치 비교. state가 behind면 update_available:true이고 commits(최근 5개)가 새 커밋. 그 밖의 state는 up_to_date·ahead·diverged·unknown_revision·local_build·pending·off·error. 서버가 주기적으로 확인한 값을 읽기만 함", InputSchema: object(map[string]schema{})},
		{Name: "rt_discuss", Description: "토론 시작. blind_first 생략 시 방의 기본 설정 사용. 좌석들이 이미 같은 질문에 답했다면 블라인드 1라운드는 독립 답을 다시 받느라 호출을 좌석 수(N)만큼 더 쓰므로 blind_first:false를 고려", InputSchema: object(map[string]schema{
			"room": id, "rounds": integer(1, nil), "blind_first": {Type: "boolean"},
		}, "room", "rounds")},
		{Name: "rt_answer", Description: "좌석이 턴 중 사용자에게 한 질문(Codex request_user_input_async)에 답함. 질문은 rt_read·rt_wait·rt_seats의 asks에 id·seat·status·questions[{title, options}]로 나옴. status가 waiting(턴 실행 중, 좌석은 기다리지 않고 진행)·open(턴 끝, 답 없음)일 때만 답함. choice(1부터, 질문 하나에 선택지가 있을 때)와 text 중 하나만 지정. 답은 그 좌석 대상 메시지로 다음 턴에 전달되며 결과의 seq를 rt_wait의 after_seq로 씀", InputSchema: object(map[string]schema{
			"room": id, "id": id, "choice": integer(1, nil), "text": text,
		}, "room", "id")},
		{Name: "rt_requests", Description: "참가자 간 요청 목록과 상태 조회", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_cancel_request", Description: "참가자 간 요청 취소", InputSchema: object(map[string]schema{"room": id, "request": id}, "room", "request")},
		{Name: "rt_forward", Description: "다른 방의 메시지(from_room의 seq)를 원문 그대로 이 방(room)에 전달. 오케스트레이터가 검수 원문 등을 읽고 다시 쓰지 않아도 됨(그 토큰을 아낌). target(좌석 id)을 주면 그 좌석에게만, 생략하면 활성 좌석 전원. 받는 좌석의 이력에는 출처(방 제목·방ID#seq·작성자)와 note(선택, 300자 이내 한 줄 메모)가 붙음. 원문 속 @멘션과 [[REQUEST]] 등 마커는 인용으로 보고 실행하지 않음. 작업 폴더 점유·대상 확인은 rt_post와 같음. 결과의 seq는 rt_wait의 after_seq로 씀", InputSchema: object(map[string]schema{
			"room": id, "from_room": id, "seq": integer(1, nil), "target": id, "note": str,
		}, "room", "from_room", "seq")},
		{Name: "rt_handoff", Description: "좌석의 긴 세션을 인계 요약으로 정리하고 다음 턴부터 새 세션으로 바꿈. 좌석이 결정·근거 위치·남은 일·다음 할 일을 요약하는 턴을 돌리고, 다음 턴은 새 세션에서 그 요약과 이후 메시지만 받음(원문은 방 기록에 남음). 문맥 경보가 뜬 좌석이나 작업이 바뀌는 지점에서 입력 토큰을 줄임. 포크한 좌석은 기반을 다시 포크하고, fresh:true면 기반 없이 시작. note는 요약에 더 담을 것(300자 이내 한 줄). 조용한 좌석만 받고 아니면 사유와 함께 거절. 결과의 seq를 rt_wait의 after_seq로 씀", InputSchema: object(map[string]schema{"room": id, "seat": id, "note": str, "fresh": {Type: "boolean"}}, "room", "seat")},
		{Name: "rt_cancel_turn", Description: "좌석의 실행 중인 턴 하나만 취소. 좌석은 켜진 채로 남아 이후 [[REQUEST]]·멘션·대기 턴을 계속 받음(웹의 '작업 중단'은 좌석까지 끔). 실행 중인 턴이 없으면 오류. 결과의 cancelled는 취소한 턴 id", InputSchema: object(map[string]schema{"room": id, "seat": id}, "room", "seat")},
		{Name: "rt_apply_markers", Description: "오류로 끝난 좌석 답(seq)의 마커([[REQUEST]]·[[REQUEST_DONE]]·[[DECISION]]·[[ATTACH]])를 실행. 서버는 오류 턴의 마커를 스스로 처리하지 않고 시스템 안내로 알림. 답이 끝까지 쓰인 것을 확인했을 때만 호출(예: 답 뒤에 stream idle timeout). 한 번만 실행되며 결과의 message.requests가 새 요청", InputSchema: object(map[string]schema{"room": id, "seq": integer(1, nil)}, "room", "seq")},
		{Name: "rt_decisions", Description: "결정 카드 목록 조회. status: proposed(좌석이 제안, 사람 확정 대기)·active(확정, 좌석 안내문의 현재 결정. 과제 결과 카드는 승인해도 안내문에 넣지 않음)·superseded·dropped", InputSchema: object(map[string]schema{"room": id}, "room")},
		{Name: "rt_update_room", Description: "방 설정 수정. 생략한 항목은 그대로이며 하나 이상 필요. 실행 중인 턴에는 다음 턴부터 적용. workspace: 지워지거나 옮겨진 작업 폴더를 세션을 유지한 채 새 폴더로. input_warn_tokens(좌석 누적 입력 경고 간격, 0=1천만, -1=끔), context_warn_percent(문맥 경보 비율, 0=70, -1=끔), seat_input_budget·room_input_budget(누적 입력 상한, 0=없음; 넘으면 새 턴을 보류하고 사유를 남김), compact_window_tokens(Claude 좌석 자동 압축 기준, 0=CLI 기본, 100000~1000000), quiet_done·max_chain(rt_create_room 참고), archived, group. 발화 상한: speak_interval_seconds(다른 좌석이 부른 턴은 그 좌석의 마지막 답 뒤 이 초가 지나야 시작, 0=없음), daily_speak_limit(좌석의 24시간 답 수, 0=없음), agents_paused(좌석 턴을 하나도 시작하지 않음). 자율 잡담(켠 방이면 종류와 관계없이 동작): auto_chat_minutes(조용하면 이 분마다 가장 오래 말하지 않은 좌석 하나가 짧게 말함, 0=끔·기본, 10~1440), auto_chat_hours(\"9-18\", 서버 PC 시각, 빈 값이면 언제나). 자율 잡담은 방을 연 사람이 있을 때만 말하고, 한 명을 @로 부르면 그 좌석이 한 번 답하며, 사람이 답하지 않으면 간격이 2배씩 늘다 3번 뒤 멈추고, 좌석 하루 상한의 절반까지만 씀. 토큰을 쓰므로 사람에게 확인받고 켠다", InputSchema: object(map[string]schema{
			"room": id, "title": text, "note": str, "workspace": str, "mode": {Type: "string", Enum: []string{"chat", "read", "write"}},
			"turn_timeout_minutes": integer(0, &maxTurnMinutes), "input_warn_tokens": integer(-1, nil), "seat_input_budget": integer(0, nil), "room_input_budget": integer(0, nil), "context_warn_percent": integer(-1, &maxPercent),
			"compact_window_tokens": integer(0, &maxCompactWindow), "quiet_done": {Type: "boolean"}, "archived": {Type: "boolean"}, "max_chain": integer(1, &maxChain), "group": str,
			"speak_interval_seconds": integer(0, &maxInterval), "daily_speak_limit": integer(0, &maxDaily), "agents_paused": {Type: "boolean"},
			"auto_chat_minutes": integer(0, &maxAutoChat), "auto_chat_hours": str,
		}, "room")},
		{Name: "rt_assign", Description: "사람이 좌석 하나에 과제를 맡김(작업실의 과제 배정). to 좌석 대상 \"과제: …\" 메시지와 from user 요청을 만든다. 좌석이 [[REQUEST_DONE:요청ID]]로 끝내면 \"과제 결과\" 카드가 proposed로 생기고, 사람이 rt_update_decision status active(승인)·dropped(반려)로 정한다. 승인한 결과는 카드에만 남고 좌석 안내문에는 들어가지 않음. text는 한 줄. 결과의 seq를 rt_wait의 after_seq로 씀. 공간의 좌석은 다른 사람의 계정 토큰을 쓰므로 맡기기 전에 사람에게 확인받는다", InputSchema: object(map[string]schema{"room": id, "to": id, "text": text}, "room", "to", "text")},
		{Name: "rt_update_decision", Description: "결정 카드 수정·상태 변경. 좌석이 [[DECISION]]으로 남긴 proposed 카드는 status active로 확정, dropped로 거절. 생략한 항목은 그대로이며 하나 이상 필요", InputSchema: object(map[string]schema{
			"room": id, "id": id, "status": {Type: "string", Enum: []string{"active", "superseded", "dropped"}},
			"text": text, "evidence": str, "dissent": str, "unverified": str,
		}, "room", "id")},
	}
	out := make([]tool, 0, len(all))
	for _, t := range all {
		read := slices.Contains([]string{"rt_list_rooms", "rt_read", "rt_wait", "rt_usage", "rt_seats", "rt_version", "rt_requests", "rt_decisions"}, t.Name)
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
	Room               string             `json:"room"`
	Rooms              string             `json:"rooms"`
	Timeout            int                `json:"timeout_seconds"`
	Limit              int                `json:"limit"`
	Title              string             `json:"title"`
	Workspace          string             `json:"workspace"`
	Mode               string             `json:"mode"`
	Participants       []participantInput `json:"participants"`
	Text               string             `json:"text"`
	Target             string             `json:"target"`
	WaitSeconds        *int               `json:"wait_seconds"`
	AfterSeq           int64              `json:"after_seq"`
	Seq                int64              `json:"seq"`
	MaxChars           int64              `json:"max_chars"`
	Rounds             int                `json:"rounds"`
	BlindFirst         *bool              `json:"blind_first"`
	Request            string             `json:"request"`
	Seat               string             `json:"seat"`
	Summary            bool               `json:"summary"`
	FromRoom           string             `json:"from_room"`
	TurnTimeoutMinutes int                `json:"turn_timeout_minutes"`
	Note               string             `json:"note"`
	ID                 string             `json:"id"`
	Fresh              bool               `json:"fresh"`
	IncludeArchived    bool               `json:"include_archived"`
	QuietDone          bool               `json:"quiet_done"`
	MaxChain           int                `json:"max_chain"`
	Group              string             `json:"group"`
	Archived           *bool              `json:"archived"`
	Choice             int                `json:"choice"`
	To                 string             `json:"to"`
	Kind               string             `json:"kind"`
	SpeakInterval      int                `json:"speak_interval_seconds"`
	DailySpeakLimit    int                `json:"daily_speak_limit"`
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
		return s.listRooms(ctx, a.Limit, a.IncludeArchived)
	case "rt_create_room":
		if a.Mode != "chat" && strings.TrimSpace(a.Workspace) == "" {
			return nil, fmt.Errorf("read/write 모드는 workspace가 필요함")
		}
		var created struct {
			ID           string `json:"id"`
			Participants []seat `json:"participants"`
		}
		err := s.api(ctx, "POST", "/api/rooms", map[string]any{"title": a.Title, "workspace": a.Workspace, "mode": a.Mode, "participant_specs": a.Participants, "turn_timeout_minutes": a.TurnTimeoutMinutes, "quiet_done": a.QuietDone, "max_chain": a.MaxChain, "group": a.Group, "kind": a.Kind, "speak_interval_seconds": a.SpeakInterval, "daily_speak_limit": a.DailySpeakLimit}, &created)
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
		if a.Summary && a.MaxChars < summaryBudget {
			a.MaxChars = summaryBudget // read whole answers so no verdict line is cut before summarizing
		}
		res, err := s.wait(ctx, a.Rooms, time.Duration(a.Timeout)*time.Second, a.MaxChars)
		if err == nil && a.Summary {
			summarize(res.Ready)
		}
		return res, err
	case "rt_usage":
		return s.usage(ctx, a.Rooms)
	case "rt_seats":
		return s.seats(ctx, a.Rooms, a.Limit)
	case "rt_archive_rooms":
		archived := a.Archived == nil || *a.Archived
		done, failed := []string{}, map[string]string{}
		for _, id := range strings.Split(a.Rooms, ",") {
			var result json.RawMessage
			if err := s.api(ctx, "PATCH", "/api/rooms/"+id, map[string]any{"archived": archived}, &result); err != nil {
				failed[id] = err.Error()
				continue
			}
			done = append(done, id)
		}
		return map[string]any{"archived": archived, "rooms": done, "failed": failed}, nil
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
	case "rt_forward":
		var sent struct {
			Seq int64 `json:"seq"`
		}
		if err := s.api(ctx, "POST", base+"/forward", map[string]any{"from_room": a.FromRoom, "seq": a.Seq, "target": a.Target, "note": a.Note}, &sent); err != nil {
			return nil, err
		}
		return map[string]any{"seq": sent.Seq}, nil
	case "rt_handoff":
		var sent struct {
			Seq int64 `json:"seq"`
		}
		if err := s.api(ctx, "POST", base+"/participants/"+a.Seat+"/handoff", map[string]any{"note": a.Note, "fresh": a.Fresh}, &sent); err != nil {
			return nil, err
		}
		return map[string]any{"seq": sent.Seq}, nil
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
	case "rt_answer":
		var answered struct {
			Message struct {
				Seq int64 `json:"seq"`
			} `json:"message"`
			Ask   askView `json:"ask"`
			Error string  `json:"error"`
		}
		if err := s.api(ctx, "POST", base+"/asks/"+a.ID+"/answer", map[string]any{"choice": a.Choice, "text": a.Text}, &answered); err != nil {
			return nil, err
		}
		out := map[string]any{"seq": answered.Message.Seq, "ask": answered.Ask}
		if answered.Error != "" {
			out["error"] = answered.Error
		}
		return out, nil
	case "rt_assign":
		var assigned struct {
			Message struct {
				Seq int64 `json:"seq"`
			} `json:"message"`
			Request json.RawMessage `json:"request"`
		}
		if err := s.api(ctx, "POST", base+"/tasks", map[string]any{"to": a.To, "text": a.Text}, &assigned); err != nil {
			return nil, err
		}
		return map[string]any{"seq": assigned.Message.Seq, "request": assigned.Request}, nil
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
			return nil, fmt.Errorf("바꿀 항목(title·note·workspace·mode 등)이 하나 이상 필요함")
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
