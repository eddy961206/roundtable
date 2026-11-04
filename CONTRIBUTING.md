# 동시 작업 규칙

여러 세션(사람·에이전트)이 이 저장소를 동시에 고칠 때 지킨다. 2026-10-27에 두 세션이 main에 각자 직접 커밋해 이력이 엇갈린 일이 있었다.

## 브랜치와 worktree

- 세션마다 별도 worktree와 브랜치를 쓴다. 저장소 본체 checkout의 main에서 직접 고치거나 커밋하지 않는다.
- 다른 세션의 worktree·브랜치·dirty 파일은 건드리지 않는다.

```bash
git worktree add ../roundtable-<작업명> -b <종류>/<작업명> origin/main
```

## main 반영

main에는 시험이 통과한 브랜치를 fast-forward로만 올린다. 병합 커밋과 강제 push는 쓰지 않는다.

1. 작업 브랜치를 최신 main 위로 옮긴다.

   ```bash
   git fetch origin
   git rebase origin/main
   ```

2. 시험을 돌린다: `python tools/check.py`. 기준 브랜치(기본 `origin/main`) 대비 바뀐 영역에 맞춰 `gofmt`·`go vet`·`go test ./...`, `web/js`면 `tools/modcheck.py`와 `tools/perf/*.test.mjs`, `tools/orchestrate`면 그 폴더의 unittest, 안내서·스크립트의 `rt` 예시 검사를 차례로 돌리고 처음 실패에서 멈춘다. 전부 돌리려면 `--all`.
   - 이 저장소에는 CI가 없다. checkout마다 한 번 `git config core.hooksPath .githooks`를 해 두면 `git push` 전에 같은 검사가 돈다.
3. 원격 main에 올린다. fast-forward가 아니면 원격이 거절한다. 거절되면 1단계부터 다시 한다.

   ```bash
   git push origin HEAD:main
   ```

4. 저장소 본체 checkout을 쓰는 세션은 `git merge --ff-only origin/main`으로 따라간다.
5. 다 쓴 worktree와 브랜치는 지운다.

## 결함 수정과 재발 방지

fix 하나에 재발 방지 장치 하나를 붙인다. 2026-11-02 기준 재병합이 아닌 커밋 188개 중 86개(46%)가 fix였고, 마커 파싱·턴 순서처럼 같은 자리가 여러 번 다시 깨졌다.

1. 고치기 전에 결함을 재현하는 장치를 먼저 만든다. 시험(`*_test.go`, fuzz), 정적 검사(`tools/modcheck.py`류), 게시 검사(`tools/orchestrate/glint.py`), 모델 검사(`tools/tla`) 중 결함에 맞는 것을 고른다.
   - 턴·대기·신호처럼 여러 주체가 엇갈리는 상태 결함은 상태머신을 TLA+로 쓰고 TLC로 검사한다. 옛 동작이 반례를 내고 새 동작이 통과해야 한다. 예: `tools/tla/RtWait.tla`(`python tools/tla/modelcheck.py`).
2. 그 장치가 수정 전 코드에서 실패하고 수정 뒤 통과하는 것을 확인한다. 수정 전에도 통과하는 시험은 결함을 막지 못한다.
3. 커밋 본문에 장치의 위치를 적는다. 예: `재발 방지: internal/room/marker_fuzz_test.go FuzzQuotedMarkersStayText`.
4. 장치를 만들 수 없으면(외부 CLI의 일회성 동작 등) 그 이유를 커밋 본문에 적는다.
5. 원인이 공급사 CLI의 동작이면 그 CLI를 roundtable과 같은 인자·stdin으로 직접 실행해 재현하고, 명령과 관측을 커밋 본문에 적는다. 다른 공급사에서 같은 명령의 결과도 적는다. 장치는 roundtable이 바꿀 수 있는 경계(좌석 안내문, 관측)에 붙인다.
   - 예: agy의 `run_command` 셸은 stdin이 닫히지 않아 경로 없는 `rg`가 1시간 54분 멈췄다(2026-11-04). agy에는 바꿀 옵션이 없어서 `internal/room/wait_rule_test.go`가 agy 안내문의 규칙을, `internal/rtmcp/seats_test.go`가 `rt seats`의 오래 열린 도구 호출 경고를 지킨다.

예: 안내서의 `rt wait --timeout_seconds 7200`이 도구 상한과 어긋나 실패한 일(9671ae1) 뒤로 `internal/rtmcp/docs_examples_test.go`가 README·AGENT_GUIDE·SKILL·`tools/orchestrate`의 모든 `rt` 예시를 도구 스키마와 대조한다.

## 실행 중인 서버 교체

여러 세션이 같은 서버(예: `127.0.0.1:18787`)를 함께 쓴다. 프로세스를 직접 종료하거나 실행 중인 exe를 덮어쓰지 않는다. 절차는 README의 [새 빌드로 교체하기](README.md#새-빌드로-교체하기)를 따른다.

1. 서버 바이너리는 `<새 빌드>.exe install <서버 exe 경로> [<다른 사본 경로>...]`로만 교체한다.
2. 서버 재기동은 `<서버 exe 경로> restart --url http://127.0.0.1:<포트> [--timeout 30m]`만 쓴다. 이 명령은 모든 방에 실행·대기 중인 턴이 없을 때만 이전 서버를 내린다. 시간 안에 유휴가 되지 않으면 이전 서버를 그대로 두고 실패한다.
