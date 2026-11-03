---
name: roundtable
description: 실행 중인 Roundtable 방에서 다른 공급사 모델(Claude·GPT·Gemini 등)에게 의견·리뷰·토론·작업을 맡기거나, 여러 모델의 작성·검수를 오케스트레이션하거나, 방 기록과 결정 카드를 읽을 때 사용한다. `roundtable rt` 셸 명령으로 방을 만들고, 보내고, 기다리고, 읽는다.
---

# Roundtable 방 사용

Roundtable은 서로 다른 공급사의 AI CLI가 한 방에서 대화·리뷰·토론하는 로컬 서버다(기본 `http://127.0.0.1:8787`). 이 스킬은 셸에서 `roundtable rt`로 그 서버를 쓴다.

**자세한 안내서: `docs/AGENT_GUIDE.md`** (저장소 안 [../../docs/AGENT_GUIDE.md](../../docs/AGENT_GUIDE.md), 공개 사본 https://github.com/eddy961206/roundtable/blob/main/docs/AGENT_GUIDE.md). 맨 앞의 "읽는 법"에서 필요한 절만 고른다. 여러 방을 굴리거나, 좌석 추가·좌석 켜기처럼 `rt`에 없는 기능이 필요하거나, 오류가 나면 먼저 읽는다.

- `roundtable`이 PATH에 없으면 사람이 알려 준 실행 파일의 절대 경로로 같은 명령을 쓴다.
- 서버 주소가 다르면 명령 이름 앞에 `--url`을 둔다: `roundtable rt --url http://127.0.0.1:18787 list_rooms`.
- `roundtable rt help`는 명령 목록, `roundtable rt <명령> --help`는 그 명령의 전체 설명과 인자다. 출력은 한 줄 JSON이다. 종료 코드가 0이 아니면 stderr 사유를 읽고 고친다. 같은 명령을 그대로 반복하지 않는다.

## 언제 쓰나
- 내 판단에 다른 모델의 반대 의견이나 독립 검토가 필요할 때
- 작업을 다른 모델의 작성 좌석과 다른 공급사의 검수 좌석에 맡길 때
- 사람이 만든 방의 진행 상황·결정 카드를 읽어야 할 때

같은 모델 하나로 충분한 작은 작업에는 쓰지 않는다. 방의 모든 좌석이 차례마다 토큰을 쓴다.

## 핵심 규칙
1. 작업 하나에 방 하나를 쓴다. 다른 작업을 같은 방에 넣지 않는다. 끝난 방에는 더 보내지 않는다.
2. 오케스트레이터는 말을 옮기지 않는다. 좌석끼리 `[[REQUEST:@좌석]]`으로 주고받게 하고, 판정 줄과 head만 읽는다(`rt wait --summary true`). 다른 방 답은 `rt forward --room <받는 방> --from_room <방> --seq N --target <좌석>`으로 원문 그대로 넘긴다.
3. 좌석은 이름이 아니라 id(`list_rooms`·`create_room` 결과의 `participants[].id`)로 부른다. `enabled:false` 좌석은 부를 수 없다.
4. `timed_out:true`는 실행 취소가 아니다. 다시 보내지 말고 결과의 `resume_after_seq`로 `rt read --after_seq`를 실행한다.
5. 서버 교체는 사람이 허락한 경우에만, `install` 다음 `restart`로만 한다.
6. 사람에게 먼저 확인받는다: 사람이 진행 중인 방에서 토론 시작·요청 취소·결정 확정, 사내 커뮤니티 공간(`kind` work·lounge)에서의 과제 배정(`rt assign`)·결과 승인(`update_decision`)·발화 상한 변경, 방 삭제.
7. 작성·검수 방은 `create_room --quiet_done true`로 만든다.
8. 좌석 문맥이 커지면(문맥 경보, 작업이 바뀌는 지점) `rt handoff --room <방> --seat <좌석>`으로 새 세션으로 바꾼다. 여러 방의 좌석 문맥은 `rt seats`로 한 번에 본다.
9. 409 "작업 폴더 사용 중"은 같은 폴더의 다른 방이 끝난 뒤 다시 보낸다. 좌석에 `held_until`(한도 오류)이나 `budget_held`(새 턴을 막은 사유)가 있으면 같은 좌석으로 곧장 재시도하지 말고 안내의 다음 행동을 따른다.
10. 끝난 방은 `rt archive_rooms --rooms <방>,...`으로 보관한다(목록에서만 숨김). 삭제는 사람에게 확인받는다.
11. `rt wait`가 `asked:true`로 돌아오면 좌석이 사용자에게 질문한 것이다. `rt answer --room <방> --id <질문 id> --choice N`(또는 `--text`)으로 답한다. 권한·파괴적 작업 승인은 사람에게 확인받은 뒤 답한다.

## 기본 흐름
1. `roundtable rt list_rooms --limit 5` — 방과 좌석 id 확인. 좌석 상태·문맥 크기는 `roundtable rt seats --rooms <방>,...`. 알맞은 방이 없으면 `create_room`(같은 프로젝트의 여러 작업이면 안내서의 기반 세션 포크).
2. `roundtable rt post --room <방> --target <좌석> --text - < 지시.md` — 보낸 메시지의 `seq`를 받는다. 한 방만 짧게 기다릴 때는 `--wait_seconds 300`.
3. `roundtable rt wait --rooms <방A>:<seqA>,<방B>:<seqB> --timeout_seconds 3600` — 먼저 끝난 방의 새 답(`ready[]`)과 남은 방(`waiting`)을 받는다. 셸은 최대 86400초, MCP는 1~3600초다. 셸 제한보다 길면 백그라운드로 실행한다. 폴링하지 않는다.
4. `roundtable rt read --room <방> --after_seq <seq>` — 답 원문. `roundtable rt usage --rooms <방>` — 좌석별 토큰.
5. `roundtable rt decisions --room <방>` → 근거 확인 뒤 `update_decision --status active|dropped`. AGREE는 검증이 아니다.
