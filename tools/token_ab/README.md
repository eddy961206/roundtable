# token_ab — 토큰 사용 A/B 측정 도구

같은 과제를 두 방식으로 돌린 방들의 토큰 사용량과 호출 수를 비교한다. 결과 해석과 채택 기록은 `docs/토큰효율_측정_20261028.md`에 있다.

## measure.py

```bash
python -X utf8 tools/token_ab/measure.py <방ID>=A <방ID>=B
```

- 방마다 다음을 JSON으로 출력한다.
  - 사용량: 입력 전체(`input` = `cached` + `uncached`), 출력, 턴 수, 턴 시간 합(`minutes`).
  - 도구 호출 종류별 수: `wait`, `edit`, `test`, `other`.
- `wait`는 실행 중인 명령을 기다리기만 하는 호출이다. Codex는 빈 `write_stdin`, agy는 `manage_task status`·`list`를 센다.
- Codex 좌석은 턴의 세션 로그(`~/.codex/sessions/**/rollout-*<세션>.jsonl`)에서 모델 호출 수와 `wait` 호출의 입력 합(`wait_input`)도 센다.
- agy·Claude 좌석은 방의 `events.jsonl`(도구 호출 이벤트)에서 센다.
- 서버 주소는 `RT_URL`(기본 `http://127.0.0.1:18787`)이다.

## codex_rollout.py

```bash
python -X utf8 tools/token_ab/codex_rollout.py --session <세션ID>
python -X utf8 tools/token_ab/codex_rollout.py <rollout.jsonl>
```

- Codex 세션의 작업(턴)마다 모델 호출 수를 출력한다.
- 첫 호출, 마지막 호출, 가장 큰 호출의 입력도 출력한다.
- 입력 합이 앞선 도구 호출의 종류(`wait`·`edit`·`test`·`other`·`answer`)별로 어떻게 나뉘는지 출력한다.
- 모델 호출은 매번 문맥 전체를 다시 읽는다. 그래서 비용은 "호출 수 × 그때의 문맥"으로 정해진다.

## 관측 한계

- agy의 사용량 보고는 CLI가 주는 값만 쓴다. 캐시 구분이 Codex·Claude와 다르다.
- `events.jsonl`에는 roundtable이 받은 도구 호출 이벤트만 있다. 하위 에이전트 내부 호출은 빠질 수 있다.
- 한 번의 A/B는 모델 출력의 편차를 다 덮지 못한다. 차이가 작으면 반복해서 돌린다.
