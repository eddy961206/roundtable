# Roundtable

**Claude Code · Codex · Antigravity · OpenCode를 한 단톡방에.** 사용하는 CLI만 골라 같은 대화에 참여시키는 로컬 웹 서비스다. 같은 질문에 여러 모델이 독립적으로 답하고 서로의 답을 검토한다. OpenCode에 연결한 API 공급사와 구독의 모델도 별도 OpenCode 화면 없이 워커로 쓸 수 있다.

Go 단일 바이너리 + 정적 HTML. 프레임워크·빌드 도구·외부 서비스 없음. 각 CLI는 당신이 이미 로그인한 공식 CLI를 그대로 실행한다. 인증 정보는 읽지도 저장하지도 않는다.

## 할 수 있는 것

| | |
|---|---|
| **단톡방** | 대상 칩이나 이름 멘션으로 한 명을 고른다. `@claude` `@codex` `@agy` `@opencode`는 해당 공급사의 활성 참가자 전원, `@all` 또는 멘션 없는 전송은 활성 참가자 전원을 부른다. 각자 같은 대화 범위를 받아 **동시에, 독립적으로** 답한다. 연쇄 호출 깊이는 방 설정으로 제한한다. |
| **토론** | N라운드 동안 순서대로 발언한다. 각자 마지막 줄에 `CONTINUE / AGREE / PASS / CONCLUDE` 신호를 붙이고, 전원 동의·결론 제시·라운드 소진 중 먼저 오는 쪽에서 끝난다. |
| **끼어들기(steering)** | 대상을 고르고 보내면 그 에이전트에게만 간다. Claude는 실행 중인 턴에 즉시 주입되고, Codex·Antigravity는 지금 턴이 끝난 직후 우선 전달된다. 각 참가자는 자기 대기열을 갖고 서로를 기다리지 않는다. |
| **개별 중단** | 참가자 카드에 두 가지가 있다. **현재 턴만 취소**는 실행 중인 턴 하나만 멈추고 참여는 켜 둔다(이후 요청·멘션을 계속 받음, `rt_cancel_turn`). **작업 중단·참여 끄기**는 해당 참가자의 현재 실행·대기 요청을 취소하고 참여를 끈다(꺼진 좌석으로 가는 요청은 거절). 다른 참가자는 계속 실행한다. 다시 쓰려면 참여를 켜고 새 메시지를 보낸다. **모두 중지**와 대기열의 개별 턴 취소도 유지한다. |
| **작업 기록** | 답변마다 도구 호출·결과·추론 요약을 펼쳐 본다. **서브에이전트**는 어떤 종류를 어떤 과제로 띄웠고, 그 안에서 무슨 도구를 썼고, 뭐라고 보고했는지까지 중첩으로 보인다. 진행 중에도 실시간으로 자란다. |
| **사용량** | 좌하단에 이 방에 추가한 공급사의 기간별 남은 한도를 고정 표시한다. 참가자를 꺼도 표시를 유지한다. 상세 관측 정보와 CLI 설치 상태는 펼쳐서 확인한다. 수동 갱신과 보이는 탭의 60초 자동 갱신을 제공한다. Codex는 공식 CLI 계정 조회, Claude와 Gemini는 저장된 관측 기록 재조회다. OpenCode의 구독 한도와 API 잔액은 조회하지 않는다. |
| **모델·인원·추론 수준** | 같은 공급사와 모델도 여러 명 추가한다. 방마다 최대 32명이며 이름·모델·추론 수준·세션·대기열·사용량은 참가자별로 분리된다. 모델별 지원 수준과 CLI 기본값을 선택할 수 있다. |
| **원본 CLI 대화** | 참가자 카드와 답변의 `CLI에서 열기`로 원래 세션을 이어 연다(Windows). 다른 OS에서는 정확한 이어보기 명령을 복사한다. 모델을 바꾼 뒤에도 이전 답변의 세션 정보는 남는다. |
| **작업 분배** | write 방에서 "작업 계획"으로 목표를 파트로 나눠 좌석별로 맡긴다. 각 파트는 같은 기준 커밋에서 만든 자기 worktree·branch에서만 작업하고, 응답 끝 `WORK:DONE` 또는 제출 버튼으로 낸다. 병합은 방 전용 통합 branch(`rt/<방ID>/integration`)로 `--no-ff` 병합 후 verify 명령을 돌린다. **자동모드**는 승인 없이, **확인모드**는 사람이 승인해야 병합한다. 충돌·검증 실패·미해결 이견은 두 모드 모두 대기열을 멈추고 보여 준다. main 반영은 사람이 한다. 계약: [docs/작업분배_적대적리뷰_계약.md](docs/작업분배_적대적리뷰_계약.md) |
| **적대적 리뷰** | 제출된 파트를 다른 공급사 좌석이 diff만 보고 `번호 · 심각도 · 파일:줄 · 재현 시나리오 · 확신도` 표로 지적한다. 작성자는 항목별 수용·반박, 리뷰어는 반박을 유지·철회, 수용 항목은 새 SHA에서 수정 확인한다. 표 형식이 틀리면 한 번 되묻고, 이견·라운드 상한에서는 사람이 라운드 추가 또는 사람 판정을 고른다. |
| **이미지** | 입력창에 붙여넣기·끌어놓기·📎로 PNG·JPEG·WEBP를 붙이면 모든 참가자가 다음 발언 때 본다(Claude는 이미지 블록, Codex는 `--image`, agy는 경로). 에이전트는 작업 폴더의 이미지를 `[[ATTACH:경로]]` 한 줄로 방에 올린다. 직접 캡처는 write 방에서만 된다. |
| **참가자 간 요청** | 에이전트가 `[[REQUEST:@좌석ID 또는 @이름]] 내용`으로 다른 참가자에게 일을 맡기고, 받은 쪽은 응답 끝에 `[[REQUEST_DONE:ID]]`·`[[REQUEST_REJECT:ID]] 이유`를 붙인다. 요청 카드와 오른쪽 "요청" 패널에서 상태를 추적하고 사람이 취소할 수 있다. 서버가 거절한 마커는 그 사유가 해당 에이전트의 다음 프롬프트로 돌아간다. 계약: [docs/이미지첨부_요청추적_계약.md](docs/이미지첨부_요청추적_계약.md) |
| **결정 카드** | 에이전트가 결정·근거·반대·미확인을 담은 `[[DECISION: …]]` 한 줄로 남긴 카드는 반대 의견과 상관없이 제안(`proposed`)으로 오른쪽 "결정" 패널에 쌓인다. 사람이 확정(`active`)해야 모든 좌석 안내문의 "현재 결정"에 들어가고, 거절은 `dropped`다. 화면에서 사람이 직접 추가한 카드는 바로 `active`다. 외부 에이전트는 `rt_update_decision`으로 확정·거절한다. 계약: [docs/에이전트MCP_블라인드토론_결정카드_계약.md](docs/에이전트MCP_블라인드토론_결정카드_계약.md) |
| **권한 모드** | 방 단위 `chat`(프로젝트 폴더를 전달하지 않는 대화) / `read`(읽기 중심) / `write`(편집 허용). OS 샌드박스는 기본으로 끈다. 실제 제한은 아래 공급사별 도구·모드 설정을 따른다. Codex의 기본 read는 쓰기를 기술적으로 차단하지 않는다. |

## 빠른 시작

전제: `claude`, `codex`, `agy`, `opencode` 중 쓸 CLI가 PATH에 있고 필요한 계정 또는 API 공급사에 연결돼 있어야 한다. 새 방에서는 사용할 공급사를 직접 고른다. CLI가 설치돼 있어도 구독이나 모델 접근 권한이 확인된 것은 아니다.

```bash
go build -o roundtable.exe ./cmd/roundtable
./roundtable.exe -open
```

에이전트가 셸에서 `roundtable rt`(아래)를 쓰게 하려면 `roundtable.exe`가 있는 폴더를 PATH에 넣는다. 빌드한 폴더 자체를 넣으면 다시 빌드해도 따로 복사할 필요가 없다. Windows 예: `[Environment]::SetEnvironmentVariable('Path', [Environment]::GetEnvironmentVariable('Path','User') + ';<roundtable 폴더>', 'User')` 뒤 새 터미널을 연다.

브라우저가 `http://127.0.0.1:8787` 로 열린다. "새 방"에서 작업 폴더와 권한 모드를 정하고 질문을 던진다.

Windows에서는 Hyper-V·WinNAT 등이 기본 포트 8787을 예약 범위로 잡는 경우가 있다(이 경우 bind가 거부된다). 서버는 그 예약 범위를 찾아 오류에 적는다. 그때는 `-addr 127.0.0.1:18787`처럼 범위 밖 포트를 쓰고 `rt`·MCP의 `--url`도 같은 포트로 준다. 예약 범위는 `netsh interface ipv4 show excludedportrange protocol=tcp`로 확인한다.

옵션: `-addr`(기본 `127.0.0.1:8787`), `-data`(기본 `~/.roundtable`), `-claude` `-codex` `-agy` `-opencode`(명령 이름 또는 절대 경로), `-agy-status`(Gemini 한도 파일), `-open`, `-codex-windows-sandbox`(`elevated` | `unelevated`).

### 새 빌드로 교체하기

실행 중인 서버를 멈추지 않고 새 빌드로 바꾸려면 새로 빌드한 실행 파일로 아래 두 명령을 차례로 실행한다.

1. `<새 빌드>.exe install <서버 exe 경로> [<다른 사본 경로>...]`: 대상마다 기존 파일을 `<대상>.old-<시각>`으로 이름만 바꾸고 새 파일을 복사한다. Windows는 실행 중인 exe를 덮어쓰지 못하지만 이름은 바꿀 수 있다. 그 파일로 이미 떠 있는 프로세스(예: agy가 사용자 MCP 설정으로 띄운 `roundtable gitea-mcp`)는 이름이 바뀐 이전 사본으로 계속 돌고, 다음 시작부터 새 파일을 쓴다. 지우지 못한 이전 사본은 출력에 남는다.
2. `<서버 exe 경로> restart --url http://127.0.0.1:<포트> [--timeout 30m]`: 실행 중인 서버에 `POST /api/shutdown {"when_idle":true}`를 보낸다. 서버는 모든 방에 실행·대기 중인 턴과 작업 계획 단계가 없는 순간에 새 요청을 거절하고 종료한다(유휴 확인과 종료가 한 잠금 안에서 일어나 그 사이의 요청이 끼어들지 않는다). 그러면 `restart`를 실행한 프로세스가 이전 서버의 인자와 작업 폴더로 같은 주소에 서버를 띄운다. 시간 안에 유휴가 되지 않으면 이전 서버는 그대로 두고 오류로 끝난다. 강제 종료는 기존처럼 Ctrl+C다.

`restart`를 실행한 셸이 새 서버 프로세스가 되므로, 서버를 원래 띄우던 방식(백그라운드 셸, 서비스 등)으로 실행한다.

### 새 업데이트 알림

서버는 시작할 때 한 번, 그 뒤로 6시간마다 실행 중인 빌드의 커밋과 공개 저장소의 `main`을 비교한다. 비교에는 GitHub API `GET /repos/eddy961206/roundtable/compare/<실행 커밋>...main`을 인증 없이 한 번 부른다. 앞 응답의 `ETag`를 `If-None-Match`로 보내 바뀌지 않았으면 304를 받는다. 밖으로 나가는 것은 이 요청 하나다(헤더는 `User-Agent: roundtable-update-check`와 GitHub API 버전뿐). 네트워크나 API가 실패해도 서버 동작은 바뀌지 않고 상태에 오류만 남는다.

- 화면: `main`에 새 커밋이 있으면 위쪽에 "새 업데이트 N개" 배너가 뜬다. 닫으면 그 커밋까지는 다시 뜨지 않고, `main`이 더 움직이면 다시 뜬다. "변경 보기"는 GitHub 비교 화면을 연다. 적용은 위의 `install`과 `restart`로 한다.
- API: `GET /api/version`은 `state`, `running`(`revision`·`modified`·`time`), `latest`, `behind_by`, `ahead_by`, `commits`(최근 5개, 최신순), `checked_at`(ms), `update_available`, `error`를 돌려준다. `state`는 `behind`(업데이트 있음)·`up_to_date`·`ahead`(원격에 없는 커밋이 더 있음)·`diverged`·`unknown_revision`(실행 커밋이 원격에 없음, 예: push 전 로컬 커밋)·`local_build`(빌드에 커밋 정보가 없음)·`pending`·`off`·`error` 중 하나다. `error`이면 직전 비교 결과는 그대로 남는다.
- 에이전트: `roundtable rt version`(MCP `rt_version`)이 같은 값을 읽는다. 서버가 저장한 값을 읽을 뿐 GitHub를 부르지 않는다.
- `running.modified:true`는 커밋하지 않은 변경(추적하지 않는 파일 포함)이 있는 작업 폴더에서 빌드했다는 뜻이다. 이때도 기준 커밋으로 비교한다. `go run`이나 `-buildvcs=false` 빌드처럼 커밋 정보가 없으면 확인하지 않는다.
- 설정(플래그, 괄호는 같은 기본값을 주는 환경변수):
  - `-update-check on|off`(`ROUNDTABLE_UPDATE_CHECK`): `off`면 아무 요청도 보내지 않는다.
  - `-update-interval 6h`(`ROUNDTABLE_UPDATE_INTERVAL`): 10분 미만은 거부한다. 인증 없는 GitHub API 한도는 주소당 시간당 60회다.
  - `-update-repo eddy961206/roundtable`(`ROUNDTABLE_UPDATE_REPO`), `-update-branch main`(`ROUNDTABLE_UPDATE_BRANCH`)
  - `-update-api https://api.github.com`(`ROUNDTABLE_UPDATE_API`): https 주소만 받는다. 시험용으로 `http://127.0.0.1`·`http://localhost`도 받는다.
- 한계: 브랜치가 250커밋 넘게 앞서면 GitHub가 오래된 쪽 250개만 주므로 `commits`가 최신 커밋이 아닐 수 있다(`behind_by`는 정확하다). 사용자 화면은 30분마다 `/api/version`을 다시 읽는다.

### 실행 권한

2026-10-20 사용자 결정에 따라 roundtable이 실행하는 Codex·Antigravity·Claude는 기본으로 OS 샌드박스를 사용하지 않는다. 새 턴, 세션 재개, `CLI에서 열기`에 같은 정책을 적용한다. CLI 전역 설정 파일은 변경하지 않는다.

| 공급사 | read | write | chat |
|---|---|---|---|
| Codex | `danger-full-access` + `approval_policy="never"`. 파일을 쓰지 말라는 지시만 전달하며, 실제 쓰기와 네트워크 접근을 막지 못한다. | 같은 정책으로 셸·편집 허용 | scratch에서 실행하고 프로젝트 경로를 전달하지 않지만, 도구·파일 쓰기를 기술적으로 차단하지 못한다. |
| Claude Code | `--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch` 유지. 직접 셸·편집 도구는 제공하지 않는다. | `acceptEdits` + `Bash,PowerShell` 허용 유지 | `--tools ""`로 내장 도구 제외 |
| Antigravity | `--mode plan` 유지. 편집·셸 실행 허용 여부는 CLI가 판단한다. | `--mode accept-edits` 유지 | scratch에서 `--mode plan` |

Claude에는 실행별 `--settings '{"sandbox":{"enabled":false}}'`를 전달하고, Antigravity에는 `--sandbox`를 전달하지 않는다. OpenCode는 기존 도구 권한 정책(작업 폴더 안 파일 도구만 허용)을 유지하되, 쓰기 모드에서는 다른 공급사처럼 빌드·시험을 돌리도록 v2 `shell` 도구를 허용한다(2026-10-27). OpenCode 셸은 OS 샌드박스가 아니며 읽기·채팅 모드에서는 계속 막는다. CLI의 관리자 정책이나 외부 실행 환경이 별도로 강제하는 제한을 roundtable이 해제하지는 못한다.

Codex 샌드박스를 다시 켜려면 기존 옵션을 **명시**한다. `-codex-windows-sandbox elevated` 또는 `unelevated`는 Windows 실행 방식을 선택하면서 read·chat을 `read-only`, write를 `workspace-write`로 되돌린다. 비우면 `windows.sandbox`를 덮어쓰지 않고 `sandbox_mode="danger-full-access"`로 실행한다. 따라서 종전 실행 명령에 이 옵션이 남아 있으면 계속 샌드박스를 사용한다. 이 옵션은 `CLI에서 열기`에도 적용된다. 설정 의미는 [Codex 공식 문서](https://learn.chatgpt.com/docs/sandboxing)와 [Claude 공식 문서](https://code.claude.com/docs/en/sandboxing)를 참고한다.

Claude·Antigravity용 샌드박스 서버 옵션은 현재 없다. 다시 켜야 할 운영 요구가 생기면 공급사별 명시 옵션과 원본 세션 열기의 일관성을 함께 설계해야 한다. 이번 변경에는 새 옵션을 추가하지 않았다.

### 에이전트에서 Roundtable 사용하기

실행 중인 Roundtable HTTP 서버에 `roundtable mcp --url http://127.0.0.1:8787`로 연결한다. MCP 프로세스는 별도 방 서버를 띄우지 않고 기존 HTTP API만 호출한다. `roundtable`이 PATH에 있다는 전제의 등록 예시다.

- Claude Code: `claude mcp add --transport stdio roundtable -- roundtable mcp --url http://127.0.0.1:8787`
- Codex: `codex mcp add roundtable -- roundtable mcp --url http://127.0.0.1:8787`
- agy: `agy mcp add roundtable roundtable mcp --url http://127.0.0.1:8787`

기본 도구는 `rt_list_rooms`, `rt_create_room`, `rt_post`, `rt_read`, `rt_wait`, `rt_usage`, `rt_version`, `rt_discuss`, `rt_requests`, `rt_cancel_request`, `rt_cancel_turn`, `rt_apply_markers`, `rt_decisions`, `rt_update_decision`, `rt_update_room`이다. 명령 끝에 `--read-only`를 추가하면 방 목록·메시지·대기·사용량·버전·요청·결정 카드 조회 7개만 노출하고 쓰기 도구의 직접 호출도 거절한다. 연결 주소는 loopback만 허용하며 HTTP 리다이렉트와 프록시는 사용하지 않는다.

`rt_post`의 `wait_seconds`는 0~600초다. 생략하면 전송한 메시지의 `seq`를 반환한다. 지정하면 전송 전에 기존 HTTP SSE 구독을 열고, 그 메시지에서 시작된 턴과 연쇄 호출된 턴이 끝날 때까지 기다린다. 독립적으로 시작된 턴은 기다리지 않는다. 기존 턴에 주입된 steering 입력은 `live:true`로 표시한다. 이 경우 기존 턴도 기다리고, 입력보다 seq가 낮은 기존 응답도 결과에 포함한다. 기존 실행 응답이 최신 페이지 밖에 있으면 전송 전에 이력을 조회해 턴 id를 확인한다. 대기 중 HTTP 스냅샷은 `limit=1`로 조회하고 결과에 넣을 본문은 대기 종료 후 읽는다. SSE가 끊기거나 연결 정보를 확인할 수 없으면 완료로 처리하지 않고 오류를 반환한다. 전송 후 오류에는 전송된 `seq`를 표시하므로 재전송 전에 `rt_read`로 확인한다.

`timed_out:true`는 대기 시간 초과이며 실행 취소가 아니다. 긴 대기를 쓸 때는 MCP 클라이언트의 도구 제한 시간도 그보다 길게 설정해야 한다. MCP의 `notifications/cancelled`는 해당 요청의 HTTP 조회와 대기만 취소한다. 클라이언트가 stdin을 닫으면 진행 중인 MCP 요청을 모두 취소하고 MCP 서버를 종료한다. 방에서 실행 중인 에이전트 턴은 취소하지 않는다. 대기 중에도 `ping`과 다른 도구 호출을 처리하며, 응답은 요청 id로 구분한다. MCP `initialize`의 instructions에는 핵심 규칙(작업 하나에 방 하나, 좌석 id로 부르기, `rt_wait` 사용, 흐름을 바꾸는 호출은 사람 확인)과 [에이전트 안내서](docs/AGENT_GUIDE.md) 경로를 싣는다. 그중 `timed_out` 규칙은 이렇다. `timed_out`이면 다시 보내지 말고 `rt_read`로 이어 읽는다. 이때 `after_seq`에는 `timed_out:true` 결과에만 붙는 `resume_after_seq`를 쓴다. 이 값은 보낸 seq와, 아직 `streaming`이거나 예산 밖으로 빠진 답 중 가장 작은 seq−1 가운데 작은 값이다. 빠진 답은 배열에 없어도 이 값에 들어간다. `live:true`면 기존 답의 seq가 보낸 seq보다 작으므로 보낸 seq를 그대로 쓰면 그 답이 빠진다. 사람이 진행 중인 방에서 토론 시작·요청 취소·결정 확정처럼 흐름을 바꾸는 호출은 먼저 사람에게 확인받는다.

`rt_list_rooms`는 최근 갱신순으로 `limit`개(기본 20, 최대 100)를 반환한다. 방마다 id·제목·모드·실행 여부와 좌석의 id·이름·공급사·모델·`enabled`만 담는다. 삭제된 좌석은 호출할 수 없으므로 빼고, 꺼진 좌석은 `enabled:false`로 남긴다. 좌석 역할 같은 나머지 정보는 방 화면에서 본다.

`rt_wait`(`rooms`, 선택 `timeout_seconds` 1~3600·기본 600, `max_chars`)는 여러 방 중 하나 이상이 끝날 때까지 한 번의 호출로 기다린다. `rooms`는 `방ID:after_seq`를 쉼표로 이은 값이고, after_seq에는 `rt_post` 결과의 seq를 쓴다(생략하면 대기 시작 시점 이후의 답만 센다). 방이 끝났다는 것은 실행·대기 중인 턴이 없고 after_seq 뒤에 좌석 답이 있다는 뜻이다. 서버의 방 목록 SSE(`GET /api/stream`)로 깨어나므로 주기 조회가 없다. 결과 `ready`에는 끝난 방마다 `rt_read`와 같은 형식의 새 메시지가, `waiting`에는 남은 방이 다음 `rt_wait`의 `rooms` 값 그대로 들어간다. 시간이 지나면 `timed_out:true`다. 셸 도구의 실행 제한 시간이 더 짧으면 백그라운드로 실행한다.

`rt_usage`(`rooms`)는 `GET /api/rooms/{id}/usage`를 방마다 불러 좌석별·방별 합계와 전체 합을 돌려준다. 공급사마다 다른 보고 방식을 맞춰 `input_tokens`(처리한 입력 전체) = `cached_tokens`(캐시 읽기) + `uncached_tokens`(나머지, 캐시 쓰기 포함)로 계산한다. Codex는 입력에 캐시가 포함되어 있고, Claude·agy·OpenCode는 캐시를 따로 보고한다. `output_tokens`, `cost_usd`(보고하는 공급사만), `max_context_tokens`(한 턴에서 가장 큰 단일 호출), `turns`·`errors`·`duration_ms`도 준다.

`rt_create_room`은 방 id와 만든 좌석의 id·이름·공급사·모델·`enabled`를 반환한다. 좌석 id는 `rt_post`의 `target`과 다른 방 좌석의 `fork_from.seat`에 쓴다.

`rt_read`는 메시지를 오래된 순으로 반환한다. `after_seq`를 주면 그보다 큰 seq만, 생략하면 최신 메시지부터 예산까지만 담는다. `max_chars`는 본문 합계의 Unicode 문자 수이며 기본값은 20000이다. 예산은 최신 메시지부터 채운다. 예산 경계에 걸린 메시지는 앞부분을 줄이고 그 메시지와 전체에 `truncated:true`, 전체에 `omitted_chars`(줄인 문자 수)를 표시한다. 예산 밖의 오래된 메시지는 배열에 넣지 않고 `omitted_messages`(개수)와 `first_seq`(담긴 첫 seq)로 알린다. 빠진 메시지가 필요하면 `max_chars`를 늘린다. `after_seq`를 생략하면 예산을 다 쓴 뒤의 이전 페이지는 조회하지 않는다. 메시지마다 seq·작성자·이름·역할·상태·본문을 넣고, `kind`·`target`·`attachments`(`id`·`name`)·`requests`·`decisions`·`reply_to`는 값이 있을 때만 넣는다. `reply_to`는 그 답이 응답한 계기 메시지의 seq다. 계기 메시지가 대화에 없으면 빈 값으로 넘기지 않고 오류를 반환한다. 이어 읽기는 `next_seq`를 사용하되, 시간 초과 시 아직 `streaming`인 메시지는 같은 `after_seq`로 다시 조회해야 갱신된 본문을 볼 수 있다.

`rt_discuss`의 `blind_first`를 생략하면 방 설정을 따른다. 좌석들이 이미 같은 질문에 답한 뒤라면 블라인드 1라운드는 독립 답을 다시 받느라 호출을 좌석 수(N)만큼 더 쓴다. 이때는 `blind_first:false`로 순차 1라운드가 기존 답을 보고 시작하게 한다.

오류로 끝난 턴(예: 답을 다 쓴 뒤 `stream idle timeout`)의 `[[REQUEST]]`·`[[REQUEST_DONE]]`·`[[DECISION]]`·`[[ATTACH]]` 마커는 서버가 스스로 실행하지 않는다. 답이 잘렸을 수 있기 때문이다. 대신 메시지에 `pending_markers`(남은 마커 수)를 남기고 시스템 안내를 올린다. 답이 끝까지 쓰인 것을 확인하면 화면의 **마커 실행** 버튼이나 `rt_apply_markers`(`room`, `seq`, 쓰기 도구)로 한 번 실행한다. 실행한 답은 이후 좌석 이력에 본문과 함께 "CLI 오류 뒤 완료로 확인된 답"으로 전달된다. 출력을 낸 뒤 실패한 턴은 그 턴이 받은 메시지를 전달한 것으로 기록하므로, 같은 메시지를 기다리던 대기 턴이 일을 반복하지 않는다. 출력 전에 실패하거나 사용자가 중단한 턴의 메시지는 다음 턴에 다시 전달된다.

`rt_decisions`는 결정 카드 목록을 반환하며 API 오류를 빈 목록으로 바꾸지 않는다. 좌석이 마커로 남긴 카드는 `proposed`이고 안내문에 들어가지 않는다. `rt_update_decision`(`room`, `id`, 선택 `status`·`text`·`evidence`·`dissent`·`unverified`)은 기존 `PATCH /api/rooms/{id}/decisions/{did}`를 감싼다. `status:"active"`는 확정, `"dropped"`는 거절, `"superseded"`는 대체됨이다. 생략한 항목은 바뀌지 않고, 바꿀 항목이 하나도 없으면 HTTP를 부르지 않고 거절한다. 카드를 `proposed`로 되돌릴 수는 없다. 쓰기 도구라 `--read-only`에서는 노출하지 않는다.

`rt_update_room`(`room`, 선택 `title`·`note`·`workspace`·`mode`)은 기존 `PATCH /api/rooms/{id}`를 감싼다. 작업 폴더(git worktree)가 PR 병합 정리로 지워지거나 옮겨졌을 때 방과 좌석 세션을 유지한 채 `workspace`를 새 폴더로 바꾼다. 바꿀 항목이 없으면 HTTP를 부르지 않고 거절한다. 작업 폴더가 없는 방의 턴은 공급사를 띄우기 전에 "작업 폴더가 없어 실행하지 않았어"로 실패한다.

#### MCP 없이 셸로 쓰기: `roundtable rt`

MCP를 등록하지 않은 에이전트도 셸만 있으면 같은 도구를 쓸 수 있다. 명령 이름은 MCP 도구에서 `rt_`를 뺀 것이고, 인자 이름·검증·결과도 MCP와 같다. MCP는 도구 설명이 매 턴 문맥에 들어가지만, 셸 명령은 에이전트가 필요할 때 `help`만 읽으므로 평소 문맥 비용이 없다.

```
roundtable rt help                                   # 명령 목록
roundtable rt post --help                            # 명령별 인자
roundtable rt list_rooms --limit 5                   # 최근 갱신순 5개
roundtable rt post --room <방ID> --text "검토해 줘" --wait_seconds 300
cat 긴_설명.md | roundtable rt post --room <방ID> --text -   # 값 - 는 stdin에서 읽음
roundtable rt read --room <방ID> --after_seq 12
roundtable rt update_decision --room <방ID> --id <결정ID> --status active   # 제안 카드 확정
roundtable rt create_room --title "작업 1" --workspace <폴더> --mode write --participants '[{"provider":"codex","fork_from":{"room":"<기반방ID>","seat":"<기반좌석ID>"}}]'   # 기반 세션 포크
roundtable rt wait --rooms <방A>:<seq>,<방B>:<seq> --timeout_seconds 3600   # 먼저 끝난 방의 새 답을 받음
roundtable rt usage --rooms <방A>,<방B>                                       # 방별·좌석별 토큰 합계
```

결과는 stdout에 한 줄 JSON으로 나온다. 실패하면 사유를 stderr에 쓰고, 검증·실행 실패는 종료 코드 1, 명령 형식 오류는 2를 돌려준다. 정수는 `--rounds 2`, 참·거짓은 `--blind_first false`, 배열은 JSON(`--participants '[{"provider":"agy"}]'`)으로 쓴다. 다른 서버는 `roundtable rt --url http://127.0.0.1:8791 ...`로 지정한다(loopback만 허용). 에이전트에게 사용법을 알려 줄 때는 [skills/roundtable/SKILL.md](skills/roundtable/SKILL.md)를 스킬로 설치하거나 지침에 붙인다. 이 짧은 진입점은 오케스트레이션 플레이북·좌석 전달 규칙·공급사별 능력표·실패 대처를 담은 [에이전트 안내서](docs/AGENT_GUIDE.md)로 이어진다.

등록 명령은 각 CLI의 `mcp add --help`와 [Codex 공식 MCP 문서](https://developers.openai.com/codex/mcp)를 기준으로 확인했다. stdio는 기존 `gitea-mcp`와 같은 줄 단위 JSON-RPC이며 [MCP stdio 규격](https://modelcontextprotocol.io/specification/2025-11-25/basic/transports#stdio)을 따른다.

### 여러 모델을 함께 쓰기

1. **새 방**에서 공급사·모델·추론 수준·인원을 고른다. **다른 모델 구성**으로 Opus 2명, Fable 1명, GPT 2명, Gemini 2명 같은 조합을 만든다.
2. 기존 방에서는 참가자 패널의 **추가**를 누른다. 모델 목록에 없는 모델은 CLI가 지원하는 정확한 ID를 직접 입력할 수 있다. 목록에 있다고 계정의 사용 권한이나 잔여 한도까지 보장되는 것은 아니다.
3. 특정 한 명에게 지시할 때는 이름으로 된 대상 칩이나 **이름 넣기**를 쓴다. 같은 이름은 화면에서 번호로 구별한다. 내부 ID와 정확한 모델 ID는 **상세**에서 확인한다. 이름을 바꿔도 내부 ID는 유지된다.

참가자를 제거해도 이전 답변과 누적 사용량은 남는다. 모델·추론 수준 변경은 실행 중에는 거절하며, 변경된 참가자의 다음 턴은 새 CLI 세션으로 시작한다. 공급사 한도는 계정 단위이므로 참가자 수만큼 늘어나지 않는다.

### 공통 문맥을 한 번만 읽히기: 기반 세션 포크

같은 프로젝트의 작업을 여러 방에 나눠 맡길 때는 공통 문서를 **기반 방**의 좌석에 한 번만 읽힌다. 작업 방의 좌석은 `participant_specs[].fork_from`(`{"room","seat"}`)으로 그 세션을 복사해 첫 턴을 시작하므로 같은 문서를 다시 읽지 않는다. 포크마다 세션이 따로라 작성 좌석과 검수 좌석을 같은 기반에서 각각 포크해도 문맥이 섞이지 않는다. 공급사·모델은 기반 좌석과 같아야 하고 agy는 지원하지 않는다. 작업 폴더는 달라도 된다. 작업마다 다른 git worktree를 쓰면 기반 방은 저장소 본체에 두고 작업 방은 각자의 worktree를 작업 폴더로 준다. 폴더가 다르면 포크 첫 턴에 두 폴더를 알려 준다. 계약은 [docs/기반문맥_포크_계약.md](docs/기반문맥_포크_계약.md), 실측은 [docs/기반문맥_포크_측정_20261027.md](docs/기반문맥_포크_측정_20261027.md)에 있다. 지금은 HTTP API·`rt`·MCP에서만 지정할 수 있다.

### OpenCode API와 구독 모델

OpenCode v2의 공식 [비대화형 CLI](https://opencode.ai/v2/docs/cli)를 사용한다. Roundtable은 API 키를 별도로 입력받지 않는다. OpenCode에 이미 연결한 계정·공급사·모델 구성을 CLI가 사용한다. 새 공급사나 [OpenCode Go 구독](https://opencode.ai/v2/docs/console/go/)을 OpenCode에서 연결한 뒤, Roundtable의 **연결·모델 새로고침**을 누르고 해당 공급사의 모델을 선택한다. 계정 연결·구독 결제는 OpenCode에서 진행한다. Roundtable은 다른 모델이나 과금 방식으로 자동 전환하지 않는다.

목록은 CLI가 반환하는 연결된 공급사 중 도구 호출을 지원하고 텍스트를 출력하는 모델로 구성한다. 공급사별로 묶고 이름으로 검색한다. 모델은 직접 선택해야 한다. 목록에 없는 지원 모델은 정확한 ID로 입력할 수 있다. 목록 조회 성공은 계정의 실제 호출 권한이나 남은 한도를 보장하지 않는다. Go는 같은 연결 경로를 사용하도록 구현했으나 이 작업 환경에 구독이 없어 실제 Go 호출은 확인하지 않았다.

각 실행은 `--standalone`으로 독립 CLI 서버를 사용한다. 공용 설정 파일을 바꾸지 않고 프로세스에 전달한 권한 설정만 추가한다. `chat`은 도구를 차단하고, `read`는 파일 읽기·검색, `write`는 파일 읽기·검색·편집을 허용한다. shell·하위 에이전트·외부 도구와 폴더 밖 접근은 허용하지 않는다. 이는 CLI 권한 규칙이며 운영체제 샌드박스는 아니다. 토큰과 CLI가 보고한 비용은 기록하지만 구독 한도·API 잔액과 실제 청구액은 별개다.

OpenCode 2.0.26은 응답 텍스트를 복원하면서 완료 이벤트를 생략할 수 있다. 따라서 각 실행 후 공식 저장 메시지에서 이번 입력 원문·실행 시각·에이전트·완료 상태를 대조해 완료와 사용량을 확정한다. 취소나 실행 오류를 성공으로 바꾸지 않는다. 같은 세션은 OpenCode의 원본 대화 기록에도 남는다.

### 원본 CLI에서 이어 보기

Roundtable은 각 공급사의 일반 세션 저장 기능을 사용한다. 대화가 끝난 참가자 카드 또는 답변 아래에서 **CLI에서 열기**를 누르면 같은 세션 ID를 사용해 원본 CLI를 연다. 현재 세션을 열면 해당 참가자를 비활성화한다. CLI 창이 열려 있는 동안 같은 세션을 다시 열거나 참가자를 다시 켜는 요청은 거절한다. CLI 창을 닫은 뒤 참가자를 다시 켜면 된다. **명령 복사**만으로는 참가자를 비활성화하지 않으므로, 복사한 명령을 실행하기 전에는 직접 꺼야 한다.

| 공급사 | 원본 세션을 지정하는 명령 |
|---|---|
| Claude Code | `claude --resume <session_id>` |
| Codex | `codex resume --include-non-interactive --cd <directory> <session_id>` |
| Antigravity | `agy --conversation <session_id>` |
| OpenCode v2 | `opencode mini --standalone --session <session_id> --agent roundtable-<mode> --replay` |

화면에서 제공하는 명령에는 당시 모델·추론 수준·권한 옵션도 포함된다. 새 메시지에는 원본 세션 ID와 작업 폴더를 함께 보존한다. 이전 버전의 기록에는 작업 폴더 정보가 없어서 과거 세션의 열기가 제한될 수 있다. 원본 CLI에서 별도로 나눈 새 대화가 Roundtable 기록으로 역수입되지는 않는다. 각 공급사 데스크톱 앱의 대화 목록 동기화는 앱별 동작에 따른다. 열린 CLI 창의 추적은 현재 서버 프로세스에서만 유지되므로 서버를 재시작하기 전에는 해당 창을 닫는다.

### Gitea 이슈·PR 읽기 (선택)

read·write 모드의 에이전트가 Gitea 이슈·PR을 실시간으로 조회하게 한다. roundtable은 **읽기 전용 MCP 서버**(`roundtable gitea-mcp`)를 붙인다. 이 서버는 허용한 저장소에 대해 고정된 GET 조회만 하며, 내부적으로 사용자의 `tea` 로그인을 쓴다. 도구가 URL·HTTP 메서드·본문을 받지 않으므로 이 MCP를 통한 댓글·병합·다른 저장소 접근은 구조적으로 불가능하다. 셸 사용이 허용된 좌석의 직접 `tea` 실행에는 이 MCP의 허용 저장소·GET 제한·시간 제한이 적용되지 않는다.

```bash
./roundtable.exe -gitea-repos Owner/A,Owner/B -gitea-login <tea 로그인> -tea <tea 경로>
```

- 도구: `gitea_list`(이슈·PR 목록), `gitea_search`(키워드), `gitea_issue`(본문·전체 댓글), `gitea_pull`(상태·변경 파일·리뷰), `gitea_pull_diff`(diff, 최대 120KB).
- Claude Code: 실행마다 `--mcp-config` 로 붙이고 `--allowedTools mcp__gitea` 로 이 서버 도구만 허용한다. `--strict-mcp-config` 때문에 사용자의 다른 MCP 서버는 붙지 않는다.
- Codex: 실행마다 `-c mcp_servers.gitea.*` 로 붙이고 `default_tools_approval_mode="approve"` 로 이 서버 도구만 승인한다(그렇지 않으면 `approval_policy="never"` 에서 거절된다). `enabled=true`, `required=true`로 연결을 필수화하고, 시작 제한 10초·클라이언트 도구 제한 25초를 지정한다. Gitea 도구 이름과 실패 보고 안내는 세션 첫 턴에 두 문장으로 전달한다. 재개 턴에는 반복하지 않는다.
- Antigravity: 실행마다 붙이는 옵션이 없어 한 번 전역 등록한다. `agy mcp add roundtable-gitea <roundtable.exe 경로> gitea-mcp --tea <tea 경로> --login <로그인> --repos Owner/A,Owner/B` / 해제는 `agy mcp remove roundtable-gitea`.

Gitea MCP는 도구 한 번의 전체 조회를 20초로 제한한다. 댓글 페이지와 PR·파일·리뷰 조회가 이 시간을 공유한다. 제한을 넘으면 launcher와 자식 프로세스를 종료하고 오류를 반환한다. 인증 도구의 stderr 원문은 노출하지 않고 종료 코드를 알린다. Windows 인증 launcher의 폴더 권한 문제와 재현·검증 근거는 [Codex Gitea 조회 진단](docs/codex-gitea.md)에 정리했다.
- `chat` 모드에는 붙이지 않는다.

### Gemini 한도 보이게 하기 (선택)

Antigravity CLI는 [공식 statusLine 기능](https://antigravity.google/docs/cli/statusline)으로, 상태표시줄을 그릴 때마다(비대화형 `-p` 실행 포함) 남은 할당량을 JSON으로 명령에 넘긴다. Roundtable은 그 명령 앞에 끼어들어 값을 받아 적고, 원래 명령은 그대로 이어 실행한다.

```bash
./roundtable.exe agy-hook install     # ~/.gemini/antigravity-cli/settings.json 의 statusLine 한 키만 바꿈, 원본 백업
./roundtable.exe agy-hook status      # 수집된 값 확인
./roundtable.exe agy-hook uninstall   # 원래대로
```

저장하는 것은 유효한 한도를 포함한 상태의 `quota`·`context_window`·`model`·`plan_tier`·`version` 뿐이고 이메일·경로는 버린다. 한도가 없거나 비어 있는 갱신은 파일을 쓰지 않는다. 여러 Gemini 참가자가 동시에 실행돼도 한도 없는 갱신이 이전 값을 복사해 새 값을 덮어쓰지 않는다. **OAuth 토큰으로 Google API를 직접 부르는 방식은 쓰지 않는다.** Antigravity 추가 약관 6항은 서드파티 도구가 Antigravity OAuth로 서비스에 접근하는 것을 계정 정지 사유로 명시한다.

## 권한 모드

현재 공급사별 기본값과 제한은 [실행 권한](#실행-권한) 표를 따른다. read는 파일을 쓰지 말라는 작업 지시이며 공급사의 도구·모드 제한을 함께 사용한다. Codex의 기본 read에는 쓰기 차단 수단이 없다. Claude의 도구 목록과 `--add-dir`, Antigravity의 plan 모드도 OS 수준의 파일 격리를 뜻하지 않는다.

2026-10-17의 9가지 호출에서 read·chat의 파일 생성 거절을 관측한 것은 샌드박스를 기본 사용하던 이전 구성의 기록이다. 현재 Codex read의 쓰기 차단 근거로 사용할 수 없다. Antigravity는 `--disable-slash-commands`와 `--mode plan`을 같이 주면 plan이 무시되는 것을 확인했기 때문에, 그 플래그는 write 모드에만 붙인다.

서버는 인증 없는 로컬 도구다. loopback 클라이언트와 로컬 Host만 허용하고, 다른 웹사이트의 Origin 또는 교차 사이트 요청을 거절한다. `-addr`를 외부 주소로 열더라도 원격 클라이언트는 허용하지 않는다.

## 데이터

`~/.roundtable/rooms/<id>/` 에 `room.json`(설정·참가자·세션 ID·누적 사용량), `messages.jsonl`, `events.jsonl`(작업 기록)이 쌓인다. 에이전트가 읽은 파일 내용은 작업 기록에 남는다. **비밀이 든 폴더는 작업 폴더로 주지 마라.**

화면은 최근 100개부터 불러오며 한 번에 최대 200개 메시지를 유지한다. 이전 기록은 페이지로 이동한다. 작업 기록은 처음 펼칠 때 최근 200개 이벤트와 필요한 서브에이전트 문맥을 디스크에서 읽는다. 브라우저는 최대 8개 답변의 기록을 캐시하며, 열린 기록에는 새 이벤트를 계속 추가한다. 이전 기록을 요청하면 전체 기록을 불러오므로 답변 하나의 캐시는 200개를 넘을 수 있다. 전체 기록 다운로드와 대화 내보내기는 유지한다. 서버의 메시지 본문과 디스크 기록은 여전히 누적되므로 무기한 보관에 대한 용량 제한은 별도로 필요하다. 서버는 작업 기록 전체를 상시 메모리에 보관하지 않는다.

스트리밍·작업 기록 조회·화면 갱신의 성능 측정과 재현 방법은 [2026-10-21 성능 측정](docs/성능최적화_측정_20261021.md)과 [tools/perf](tools/perf/README.md)에 정리했다.

## 각 CLI에서 무엇을 어떻게 읽는가

| | 호출 | 세션 재개 (기반 포크) | 서브에이전트 관측 | 한도 |
|---|---|---|---|---|
| Claude Code | `claude -p --input-format stream-json --output-format stream-json` | `--resume <session>` (`--resume <기반> --fork-session`) | 스트림의 `parent_tool_use_id`, `task_started/task_notification` | 스트림의 `rate_limit_event` |
| Codex | `codex exec - --json -c features.plugins=false` | `codex exec resume … <thread> -` (`codex exec fork … <기반> -`) | `~/.codex/sessions` 에서 `parent_thread_id` 가 같은 rollout 파일을 추적 | rollout 관측 + `codex app-server`의 `account/rateLimits/read` |
| Antigravity | `agy --output-format stream-json --input-format stream-json -p=` (프롬프트는 stdin) | `--conversation <id>` (포크 없음) | `invoke_subagent` 의 `log_uri` → `transcript.jsonl` | statusLine 훅(위) |
| OpenCode v2 | `opencode run --standalone --format json` | `--session <id>` (`--session <기반> --fork`) | Roundtable 권한 모드에서는 하위 에이전트 실행을 허용하지 않음 | 응답의 토큰·비용만 기록, 계정 잔액 조회 없음 |

- 각 에이전트는 자기 CLI 세션을 이어 가며 **자기 마지막 발언 이후 새 메시지만** 받는다. 매번 전체 대화를 보내지 않아 토큰이 덜 들고, 각자의 도구 맥락이 유지된다. 턴이 실패하면 그 메시지는 다음 턴에 다시 전달된다.
- 재개 턴은 방 참가자·공지·규칙을 다시 보내지 않고 한 줄 알림만 보낸다. CLI가 문맥을 압축한 것이 보이면 다음 턴에 전체 안내문을 다시 보낸다. 추론 수준만 바꾸면 세션을 유지한다(그 턴은 Gemini·Codex에서 캐시를 쓰지 못한다). 실측: [docs/토큰절감_측정_20261020.md](docs/토큰절감_측정_20261020.md).
- agy는 프롬프트를 stdin stream-json으로 받으므로 명령줄 길이 한도가 없다.
- Codex는 사용자 플러그인을 끄고 실행한다. 플러그인 스킬 목록(수만 자)을 재개 턴마다 다시 보내는 것을 실측했고, Roundtable은 플러그인 도구를 쓰지 않는다. 사용자 메모리·전역 AGENTS.md·개인 스킬은 그대로 읽는다.
- 답변별 토큰·비용은 그 턴의 값이다. agy와 Codex는 대화 전체 누적 토큰을 보고하므로 agy는 단계별 사용량을 더하고, Codex는 rollout의 턴 시작 전 누적값을 빼서 계산한다. Claude의 비용은 세션 누적값이라 `session_cost_usd`로 보관하고 직전 값과의 차이를 턴 비용으로 쓴다. 포크 첫 턴은 기반 세션의 누적값(Codex는 기반 rollout, Claude는 기반 방 기록)을 빼서 계산한다. 문맥 크기는 캐시를 포함한 마지막 호출의 입력이다. 이 수정 전 빌드로 쌓인 기록은 `tools/fix_cumulative_usage.py`로 한 번 보정한다(서버를 멈추고 실행, 먼저 미리보기).
- Windows에서 npm으로 설치한 Codex는 `codex.cmd` 뒤의 실제 `codex.exe`를 찾아 실행한다(별도 설치본에 code-mode host가 빠진 경우가 있었다).
- Codex Windows 샌드박스를 명시적으로 켠 실행에서는 `setup refresh had errors`가 발생할 수 있다. 샌드박스 설정과 다른 프로세스의 Codex 런타임 파일 잠금을 확인한다.

## 개발

```bash
go test ./...
```

- `internal/agent` — CLI 실행과 스트림 파서. `testdata/` 는 실제 CLI 출력 캡처(로컬 경로 정제).
- `internal/room` — 방·메시지·대기열·토론·SSE 팬아웃, JSONL 저장.
- `internal/agyhook` — Antigravity statusLine 훅 설치·제거.
- `internal/server` — JSON API + SSE + 정적 UI.
- `web/` — 단일 페이지 UI. 설계 기준은 [디자인 브리프](docs/디자인_브리프.md).

여러 세션이 동시에 고칠 때는 [동시 작업 규칙](CONTRIBUTING.md)을 따른다. 세션마다 worktree·브랜치를 쓰고, main에는 시험 통과 뒤 fast-forward로만 올리며, 서버 교체는 `install`과 `restart`로만 한다.

web 화면은 `web/js/` 아래 ES 모듈이다. 모듈을 고친 뒤에는 `python tools/modcheck.py web/js`로 다른 모듈의 이름을 import 없이 쓰는 곳이 없는지(0건) 확인한다.

## 어떻게 만들었나

이 저장소 자체가 세 모델의 협업으로 만들어졌다. Claude가 설계·통합·검증을 맡아 오케스트레이션하고, Codex(GPT-6.1 Sol)가 백엔드 스케줄러와 웹 조사·결함 리뷰를, Gemini 4 Argon이 UI를 맡았다. 작업 지시도 Roundtable의 write 모드 방으로 했다. 과정과 검증 결과는 [통합 테스트 기록](docs/통합테스트_20261016.md)과 [협업 기록](docs/협업기록_20261017.md)에 있다.

## 라이선스

MIT. 동봉한 `marked` 와 참고한 프로젝트는 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
