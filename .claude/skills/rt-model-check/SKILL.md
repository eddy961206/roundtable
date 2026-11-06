---
name: rt-model-check
description: roundtable의 TLA+ 모델 검사(tools/tla). rt wait 끝남 판정, 원격 좌석 임대, 작업 계획 병합, 턴 전달 커서를 바꾸거나 그 코드(internal/rtmcp/wait.go·api.go, internal/room/remote.go·work.go·work_cleanup.go·hub.go·history.go·lifecycle.go, internal/worker/worker.go)를 고칠 때 쓴다. 상태 결함(대기·신호·임대·병합·전달 순서) 재현과 재발 방지에도 쓴다.
---

# rt 모델 검사

roundtable에서 여러 주체가 엇갈리는 상태머신은 TLA+ 명세로 검사한다. 이 경우 단위 시험은 사람이 떠올린 순서만 보고, TLC는 모델 안의 모든 순서를 본다.

## 언제 돌리나

- 아래 표의 덮는 파일을 고쳤으면 그 명세를 돌린다. `python tools/check.py`가 바뀐 파일을 보고 알아서 돌린다(환경변수가 없으면 "건너뜀"을 출력한다).
- 상태 결함을 고칠 때는 명세에 결함을 재현하는 설정을 먼저 만든다. 옛 동작은 반례를 내고 새 동작은 통과해야 한다(CONTRIBUTING.md "결함 수정과 재발 방지").

## 실행

이 PC의 Java·TLA+ 도구 경로:

```bash
export ROUNDTABLE_JAVA="D:/Tools/jdk-21.0.12.1+1/bin/java.exe"
export ROUNDTABLE_TLA2TOOLS="D:/Tools/tla/tla2tools.jar"
python tools/tla/modelcheck.py            # 모든 명세
python tools/tla/modelcheck.py RtWait     # 하나만
```

다른 PC에서는 Java 11 이상과 tla2tools.jar(https://github.com/tlaplus/tlaplus/releases, v1.7.4로 확인)가 필요하다. 명세 하나는 수십 초 안에 끝난다.

## 파일 규칙

- `tools/tla/<Spec>.tla`의 머리 주석:
  - `\* covers:`는 이 명세가 모델링한 코드 경로다. `tools/check.py`가 이 경로의 변경을 보고 명세를 다시 돌린다.
  - `\* reach:`는 통과하는 설정이 반드시 도달해야 하는 상태다. 움직이지 못하는 모델도 모든 규칙을 통과하므로, 검사기가 도달 여부를 확인한다.
- `tools/tla/<Spec>_<case>.cfg`의 `\* expect:` 줄:
  - `pass`는 지금 코드다.
  - 위반 문구(예: `Invariant TipMatchesMerged is violated.`)는 옛 코드나 가드를 하나 끈 변형이다. 모든 규칙이 적어도 한 번은 실패하는 모습을 보여야 그 규칙을 믿을 수 있다.

## 명세와 덮는 코드

| 명세 | 지키는 규칙(한 문장) | 덮는 코드 | 변형(실패해야 함) |
|---|---|---|---|
| `RtWait` | 실행·대기 중인 턴과 작업 단계가 없고 대상 입력의 답 또는 전달 확인 실패가 확인되면 rt wait가 ready를 돌려주며, 실패 상태를 결과에 남기고 미처리 사용자 요청은 별도 알림 커서로 재알림을 막습니다 | `internal/rtmcp/wait.go` readyRooms, `api.go` readSelected, `internal/room/hub.go` busy·publishList, `history.go` Snapshot.Active, `lifecycle.go` idleLocked, `work.go`·`work_cleanup.go`의 publishList 호출, `user_requests.go`·`requests.go`의 미처리 요청과 알림 | `window`(5d6506c 이전), `replied`(5d6506c), `idle_steer`·`idle_work`(b3a7b76), `current_nopublish`(publishList 이전), `user_request_notice`(383e02d 공지 전용), `user_request_repeat`(수신 커서 무시), `unconfirmed_steer`(수신 확인 전 완료), `hidden_delivery_failure`(실패 상태 누락) |
| `RemoteLease` | 원격 턴은 결과를 한 번만 받고, 임대가 만료됐을 때만 두 번 실행되며, 가져가지 않았다고 끝낸 턴은 아무 worker도 실행하지 않았다 | `internal/room/remote.go` claim·RemoteNext·RemoteProgress·RemoteFinish·runRemote, `internal/worker/worker.go` deliver | `norecheck`·`noretry`(d69d988), `nolease`(b93c27c), `splittimeout`(claim 시간 초과의 확인과 drop이 다른 임계구역) |
| `TurnDelivery` | 좌석은 한 메시지를 한 번만 전달받고, 수신 전 Live를 표시하지 않으며, 방이 조용해지면 입력이 전달됐거나 눈에 보이게 실패했다 | `internal/room/hub.go` 전달 커서·큐, `steering.go` 입력 정산, `internal/agent/codex_appserver.go`·`claude.go` receipt | `nooutput`(a458c08 이전), `noholes`, `earlylive`(수신 전 표시), `replayaccepted`(수신 뒤 턴 실패 때 재전송) |
| `WorkMerge` | 한 번에 한 파트만 병합하고, 병합과 worktree 정리가 겹치지 않으며, 통합 branch는 병합 기록이 디스크에 있을 때만 움직이고, 재시작 뒤에도 branch 커밋 수가 merged 파트 수와 같다 | `internal/room/work.go` queuePart·advanceQueue·mergePart·completeMerge·recoverWork, `work_cleanup.go` startWorktreeCleanup, `hub.go` resumeRecoveredWork | `nointent`(D13 이전), `nocleangate`, `nohaltgate`(D5) |

마지막 대조: 2026-11-06 사용자 요청 수신 커서와 Codex 입력 receipt를 합친 이 스킬의 최신 커밋(`git log -1 --format=%h -- .claude/skills/rt-model-check/SKILL.md`). `RtWait`는 요청 알림의 수신 커서와 입력 저장·쓰기·수신·응답을 구분한다. `unconfirmed_steer`는 확인 전 완료, `user_request_notice`·`user_request_repeat`는 요청 누락·재알림 반례다. `user_request_delivery`는 요청 알림 커서와 실패한 답 전달이 함께 있는 상태에 도달해야 한다. 정확한 TurnID와 전달 실패 노출은 `internal/rtmcp/delivery_test.go`·`user_request_delivery_test.go`, 답의 재기동 복구는 `internal/room/user_request_receipt_test.go`로 함께 확인한다.

## 명세가 잡는 것과 못 잡는 것

- 명세는 Go 코드를 읽지 않는다. 덮는 코드를 바꿨는데 명세를 그대로 두면 명세는 계속 통과한다. 코드 변경이 설계(잠금 경계, 저장 순서, 신호)를 바꾸면 명세를 같은 커밋에서 고친다.
- 코드 회귀는 Go 시험이 잡는다. 명세 반례마다 같은 사건열의 Go 시험이 있다: `internal/rtmcp/user_requests_test.go`, `internal/rtmcp/wait_rooms_test.go`, `internal/room/busy_test.go`, `internal/room/turn_races_test.go`, `internal/room/work_races_test.go`. 2026-11-04에 각 수정을 되돌려 이 시험들이 실패하는 것을 확인했다.
- 원격 claim 시간 초과처럼 Go 시험으로 순서를 고정할 수 없는 경합은 명세만 막는다. 그 코드를 고칠 때는 명세 설정을 먼저 바꿔 본다.

## 결과 읽기

- 명세의 한 동작은 코드의 한 `h.mu` 임계구역이다. 코드가 잠금을 풀었다 다시 잡으면 명세도 동작을 둘로 나눠야 한다. 2026-11-04에 claim 시간 초과를 한 동작으로 묶었다가 그 사이의 경합을 놓친 일이 있었다(RemoteLease `splittimeout`).
- 반례가 나오면 먼저 그 사건열을 실제 코드에서 한 단계씩 재현한다(Go 시험). 재현되지 않으면 모델이 틀렸다고 보고 모델을 코드에 맞춘다. 규칙을 약하게 바꿔 통과시키지 않는다.
- 반례를 사람에게 보일 때는 pstack `/statechart` 스킬로 최단 경로를 그림으로 그린다.
- 통과는 설정의 상한(파트 2개, 재시작 1번 같은 CONSTANTS) 안에서만 참이다. 시간·페이지(200개)·디스크 저장 실패는 모델 밖이다. 각 명세 머리 주석에 무엇을 뺐는지 적혀 있다.
