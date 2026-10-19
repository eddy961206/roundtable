# 에이전트용 MCP·블라인드 첫 라운드·결정 카드 계약 (v1)

2026-10-20 웹 조사(GPT-6.1 Sol: 오픈소스, GPT-6 Astra: 연구·제품·커뮤니티)에서 고른 세 가지다. 기준은 roundtable의 본질(특성이 다른 모델 간 협업·대화·의견 공유), 앱 무게, 토큰 비용이다. 사람과 AI 에이전트 모두 roundtable의 사용자다.

근거: PAL MCP(clink·consensus), Ensemble, A2A(에이전트용 진입점) / PAL 이슈 #162·LLM Council(앞 의견에 끌려가는 앵커링, 독립 답 뒤 공개) / Free-MAD·Voting or Consensus?(전원 동의 ≠ 검증, 라운드 증가 ≠ 개선) / LangGraph 명시적 상태(결정 기록).

## A. 에이전트용 Roundtable MCP (`roundtable mcp`)

- `roundtable mcp --url http://127.0.0.1:8787` stdio MCP 서버 하위 명령. 내부는 기존 HTTP API만 호출한다(서버 로직 중복 금지). 기존 `gitea-mcp`의 stdio JSON-RPC 구현 방식을 따른다.
- 도구(이름·입력은 이대로):
  - `rt_list_rooms` {} → 방 id·제목·모드·참가자(좌석 id·이름·공급사·모델·역할)·실행 중 여부.
  - `rt_create_room` {title, workspace, mode, participants:[{provider, model, effort, label, role}]} → room id.
  - `rt_post` {room, text, target?, wait_seconds?} → 보낸 메시지 seq. `wait_seconds`(0~600)가 있으면 그 메시지로 시작된 턴들이 끝나거나 시간이 다할 때까지 기다린 뒤 아래 `rt_read`와 같은 형식으로 새 답을 돌려준다.
  - `rt_read` {room, after_seq?, max_chars?} → after_seq 이후 메시지(작성자 이름·역할·상태·본문). 본문 합계가 max_chars(기본 20000)를 넘으면 오래된 것부터 줄이고 줄였다고 표시한다(토큰 절약).
  - `rt_discuss` {room, rounds, blind_first?} → 토론 시작.
  - `rt_requests` {room} → 요청 목록과 상태. `rt_cancel_request` {room, request}.
  - `rt_decisions` {room} → 결정 카드 목록(C).
- 쓰기 도구는 기본 켜짐. `--read-only`면 조회 도구만 노출. 서버가 loopback 전용인 기존 보안 경계는 그대로다.
- README에 Claude Code·Codex·agy에서 이 MCP를 등록하는 예시 한 줄씩.

## B. 블라인드 첫 라운드 토론

- `StartDiscussion`에 `blind_first`(bool) 옵션. 켜면 1라운드는 모든 활성 좌석이 같은 cap(토론 시작 시점)으로 동시에 답한다. 서로의 1라운드 답을 보지 않는다(기존 broadcast의 독립 응답과 같은 방식).
- 2라운드부터는 기존 순차 토론. 2라운드 프롬프트에는 "1라운드 답들이 공개됐다. 합의된 점은 반복하지 말고, 갈린 쟁점과 새 근거에만 답하라"를 넣는다.
- 방 설정 `blind_first_default`(bool, 기본 true)와 UI의 "토론 시작" 옆 토글. 호출 수는 기존과 같다.
- 결론 규칙: `[[DISCUSSION:CONCLUDE]]`·`AGREE`를 낼 때 "확인한 근거"와 "확인하지 못한 주장"을 한 줄씩 밝히게 안내한다. AGREE는 의견 일치일 뿐 검증이 아니다(화면 문구에도 반영).

## C. 결정 카드

- 마커: 응답 안의 줄 `[[DECISION: <결정 한 줄> | 근거: <URL·파일·메시지> | 반대: <남은 반대 의견 또는 없음> | 미확인: <확인 못 한 것 또는 없음>]]`. 정상 종료한 응답에서만, chat·read·write 모두. 형식 오류는 기존 마커 거절 경로(다음 프롬프트로 사유 전달)를 따른다.
- 사람도 화면에서 결정 카드를 추가·수정·폐기할 수 있다. 폐기는 지우지 않고 상태만 바꾼다.
- 저장: `<data>/rooms/<id>/decisions.jsonl`(요청 기록과 같은 방식, 상태 변화마다 한 줄). `Decision{id, text, evidence, dissent, unverified, by, source_msg, status: active|superseded|dropped, at}`.
- 프롬프트: standingBrief에 "현재 결정"으로 active 결정의 `text`만 한 줄씩(최대 10개, 최신순). 근거·반대·미확인은 넣지 않는다(토큰). 결정이 바뀌면 안내문 키가 바뀌어 다음 턴에 다시 간다.
- API: `GET /api/rooms/{id}/decisions`, `POST`(사람 추가), `PATCH /api/rooms/{id}/decisions/{did}`(수정·상태). SSE `decision`. 화면: 오른쪽 패널 "결정" 섹션(요청 섹션 위), 각 카드에 근거·반대·미확인 펼침, 원 메시지로 이동.

## 파트 분담

| 파트 | 담당 | 파일 범위 | 리뷰어 |
| --- | --- | --- | --- |
| A MCP | Sol | `cmd/roundtable/main.go`의 하위 명령 연결, 새 `internal/rtmcp/`, README의 MCP 절 | Opus |
| B·C 토론·결정 카드 | Opus | `internal/room`, `internal/server`, `web/` | Sol |

A는 C의 `GET /api/rooms/{id}/decisions` 모양을 위 계약대로 가정한다. 공통: fallback·조용한 우회 금지, 공개 레포 위생, 한국어 커밋, `python tools/modcheck.py web/js` 0건.
