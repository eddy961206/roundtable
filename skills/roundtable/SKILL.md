---
name: roundtable
description: 실행 중인 Roundtable 방에서 다른 공급사 모델(Claude·GPT·Gemini 등)에게 의견·리뷰·토론·작업을 맡기거나, 여러 모델의 작성·검수를 오케스트레이션하거나, 방 기록과 결정 카드를 읽을 때 사용한다. `roundtable rt` 셸 명령으로 방 목록 조회, 방 생성(기반 세션 포크 포함), 메시지 전송, 여러 방 대기, 토큰 사용량 조회, 방 메모·작업 폴더 수정, 토론, 결정 카드 확정을 한다.
---

# Roundtable 방 사용

Roundtable은 서로 다른 공급사의 AI CLI가 한 방에서 대화·리뷰·토론하는 로컬 서버다(기본 `http://127.0.0.1:8787`). 이 스킬은 셸에서 `roundtable rt`로 그 서버를 쓴다.

**자세한 안내서: `docs/AGENT_GUIDE.md`** (저장소 안 [../../docs/AGENT_GUIDE.md](../../docs/AGENT_GUIDE.md), 공개 사본 https://github.com/eddy961206/roundtable/blob/main/docs/AGENT_GUIDE.md). 기능 색인(`rt`·HTTP 전용·`tools/orchestrate`), 좌석 전달 규칙, 공급사·모드별 능력표, 오케스트레이터 플레이북, 실패 유형과 대처가 있다. 여러 방을 굴리거나, 좌석 추가·`max_chain`처럼 `rt`에 없는 기능이 필요하거나, 오류가 나면 먼저 읽는다.

- `roundtable`이 PATH에 없으면 사람이 알려 준 실행 파일의 절대 경로로 같은 명령을 쓴다.
- 서버 주소가 다르면 명령 이름 앞에 `--url`을 둔다: `roundtable rt --url http://127.0.0.1:18787 list_rooms`.
- 인자는 `roundtable rt <명령> --help`로 본다. 출력은 한 줄 JSON이다. 종료 코드가 0이 아니면 stderr 사유를 읽고 고친다. 같은 명령을 그대로 반복하지 않는다.

## 언제 쓰나
- 내 판단에 다른 모델의 반대 의견이나 독립 검토가 필요할 때
- 작업을 다른 모델의 작성 좌석과 다른 공급사의 검수 좌석에 맡길 때
- 사람이 만든 방의 진행 상황·결정 카드를 읽어야 할 때

같은 모델 하나로 충분한 작은 작업에는 쓰지 않는다. 방의 모든 좌석이 차례마다 토큰을 쓴다.

## 핵심 규칙
1. 작업 하나에 방 하나를 쓴다. 다른 작업을 같은 방에 넣지 않는다. 끝난 방에는 더 보내지 않는다.
2. 오케스트레이터는 말을 옮기지 않는다. 좌석끼리 `[[REQUEST:@좌석]]`으로 주고받게 하고, 판정 줄과 head만 읽는다.
3. 좌석은 이름이 아니라 id(`list_rooms`·`create_room` 결과의 `participants[].id`)로 부른다. `enabled:false` 좌석은 부를 수 없다.
4. `timed_out:true`는 실행 취소가 아니다. 다시 보내지 말고 결과의 `resume_after_seq`로 `rt read --after_seq`를 실행한다.
5. 서버 교체는 사람이 허락한 경우에만, `install` 다음 `restart`로만 한다.
6. 사람이 진행 중인 방에서 토론 시작·요청 취소·결정 확정처럼 흐름을 바꾸는 명령은 사람에게 확인받는다.

## 기본 흐름
1. `roundtable rt list_rooms --limit 5` — 방과 좌석 id 확인. 알맞은 방이 없으면 `create_room`(같은 프로젝트의 여러 작업이면 안내서의 기반 세션 포크).
2. `roundtable rt post --room <방> --target <좌석> --text - < 지시.md` — 보낸 메시지의 `seq`를 받는다. 한 방만 짧게 기다릴 때는 `--wait_seconds 300`.
3. `roundtable rt wait --rooms <방A>:<seqA>,<방B>:<seqB> --timeout_seconds 3600` — 먼저 끝난 방의 새 답(`ready[]`)과 남은 방(`waiting`)을 받는다. 셸 제한보다 길면 백그라운드로 실행한다. 폴링하지 않는다.
4. `roundtable rt read --room <방> --after_seq <seq>` — 답 원문. `roundtable rt usage --rooms <방>` — 좌석별 토큰.
5. `roundtable rt decisions --room <방>` → 근거 확인 뒤 `update_decision --status active|dropped`. AGREE는 검증이 아니다.
