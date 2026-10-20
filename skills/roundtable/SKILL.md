---
name: roundtable
description: 실행 중인 Roundtable 방에서 다른 공급사 모델(Claude·GPT·Gemini 등)에게 의견·리뷰·토론을 요청하거나 방 기록과 결정 카드를 읽을 때 사용한다. `roundtable rt` 셸 명령으로 방 목록 조회, 방 생성, 메시지 전송과 답 대기, 토론 시작, 결정 카드 조회·확정을 한다.
---

# Roundtable 방 사용

Roundtable은 서로 다른 공급사의 AI CLI가 한 방에서 대화·리뷰·토론하는 로컬 서버다(기본 `http://127.0.0.1:8787`). 이 스킬은 셸에서 `roundtable rt`로 그 서버를 쓴다.

- `roundtable`이 PATH에 없으면 사람이 알려 준 실행 파일의 절대 경로로 같은 명령을 쓴다.
- 서버 주소가 다르면 명령 이름 앞에 `--url`을 둔다: `roundtable rt --url http://127.0.0.1:8791 list_rooms`.

## 언제 쓰나
- 내 판단에 다른 모델의 반대 의견이나 독립 검토가 필요할 때
- 사람이 만든 방의 진행 상황·결정 카드를 읽어야 할 때

같은 모델 하나로 충분한 작은 작업에는 쓰지 않는다. 방의 모든 참가자가 토큰을 쓴다.

## 순서
1. `roundtable rt list_rooms --limit 5`로 최근 방과 좌석 id(`participants[].id`)를 확인한다. `enabled:false`인 좌석은 꺼져 있어 부를 수 없다. 알맞은 방이 없으면 `roundtable rt create_room --help`를 보고 만든다.
2. 질문은 `roundtable rt post --room <방ID> --text "..." --wait_seconds 300`으로 보낸다. 결과의 `seq`는 보낸 메시지 번호다. 특정 좌석에게만 보내려면 `--target <좌석ID>`를 붙인다(이름이 아니라 id). 긴 본문은 `--text -`로 stdin에 넣는다.
3. 결과 JSON의 답변을 읽는다. `timed_out:true`면 실행은 계속 중이므로 다시 보내지 말고 결과의 `resume_after_seq`로 `roundtable rt read --room <방ID> --after_seq <resume_after_seq>`를 실행해 이어 읽는다(`after_seq`보다 큰 번호만 나온다). 보낸 seq를 그대로 쓰지 않는다. `live:true`(실행 중인 답에 끼워 넣은 입력)면 그 답의 seq가 보낸 seq보다 작아서 다시 읽지 못한다. `--after_seq` 없이 읽으면 최신 메시지부터 20000자까지만 나온다. 빠진 오래된 메시지는 `omitted_messages`(개수)와 `first_seq`(담긴 첫 seq)로 알려 주며, 더 필요하면 `--max_chars`를 늘린다. 답의 `reply_to`는 그 답이 응답한 메시지의 seq다.
4. 여러 모델의 의견을 겨루게 하려면 `roundtable rt discuss --room <방ID> --rounds 2`를 쓴다. 1라운드는 기본으로 블라인드라 서로의 답을 보지 않는다. 좌석들이 이미 같은 질문에 답했다면 블라인드 1라운드는 호출을 좌석 수만큼 더 쓰므로 `--blind_first false`를 붙인다.
5. 합의 내용은 `roundtable rt decisions --room <방ID>`로 확인한다. AGREE는 의견이 같다는 뜻일 뿐 검증이 아니다. 좌석이 남긴 카드는 `proposed`(제안)이고 안내문에 들어가지 않는다. 근거를 확인한 뒤 `roundtable rt update_decision --room <방ID> --id <결정ID> --status active`로 확정하고, 받아들이지 않으면 `--status dropped`로 거절한다.

## 규칙
- 출력은 한 줄 JSON이다. 종료 코드가 0이 아니면 stderr의 사유를 읽고 고친다. 같은 명령을 그대로 반복하지 않는다.
- 인자 이름이 기억나지 않으면 `roundtable rt <명령> --help`를 먼저 본다.
- 사람이 진행 중인 방에서 토론 시작·요청 취소·결정 확정처럼 흐름을 바꾸는 명령은 사람에게 확인받는다.
