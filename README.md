# Roundtable — 세 공급사 코딩 에이전트 단톡방

Claude Code · Codex · Antigravity(agy) CLI를 한 채팅방에 모아 놓고, 사람이 끼어들면서 같이 일하게 하는 로컬 웹 서비스다.
Go 단일 바이너리 + 정적 HTML 한 장. 외부 의존성 없음(마크다운 렌더러 `marked`만 동봉).

- 각 에이전트는 **자기 CLI 세션을 재개(resume)** 하며, 자기 마지막 발언 이후 새로 올라온 메시지만 받는다. 토큰을 아끼고 각자의 도구 호출 맥락을 유지한다.
- `@claude @codex @agy @all` 로 호출한다. 멘션이 없으면 전원이 **동시에** 같은 스냅샷을 받아 독립적으로 답한다. 에이전트끼리도 `@`로 서로 호출할 수 있고, 연쇄 호출 깊이는 방 설정(`max_chain`)으로 막는다.
- **토론 모드**: N라운드 동안 순서대로 발언하며 마지막 줄에 `[[DISCUSSION:CONTINUE|AGREE|PASS|CONCLUDE]]` 를 붙인다. 전원 AGREE/PASS 이거나 누가 CONCLUDE 하면 끝난다. 사람은 언제든 끼어들거나 중지할 수 있다.
- **작업 기록**: 각 답변 아래 "작업 기록"을 펼치면 도구 호출·결과, 추론 요약, **서브에이전트**(어떤 서브에이전트를 어떤 과제로 띄웠고 그 안에서 무슨 도구를 썼고 뭐라고 보고했는지)가 중첩 블록으로 보인다.
- **steering**: 대상 선택 후 메시지를 보내면 그 에이전트에게만 전달된다. Claude는 실행 중에도 stdin으로 바로 주입된다(실제로 끼어드는지는 모델이 결정). Codex·Antigravity는 실행 중 주입 수단이 없어 현재 턴이 끝난 직후 우선 전달된다.
- **한도·토큰**: 왼쪽 아래에 공급사별 5시간/7일 사용률(Claude·Codex), 오른쪽 참가자 카드와 상단 칩에 방 누적 토큰·비용·컨텍스트 사용량이 실시간으로 보인다. 참가자별 켜기/끄기, 모델, 추론 노력도 거기서 바꾼다.

## 실행

```bash
go build -o roundtable.exe ./cmd/roundtable
./roundtable.exe -open          # http://127.0.0.1:8787
```

옵션: `-addr`(기본 `127.0.0.1:8787`), `-data`(기본 `~/.roundtable`), `-claude/-codex/-agy`(CLI 명령 이름 또는 절대 경로), `-open`(브라우저 열기).

세 CLI가 PATH에 있고 각각 로그인돼 있어야 한다(`claude`, `codex login`, `agy`). 없는 CLI는 참가자 목록에 "CLI를 찾지 못함"으로 표시되고 꺼진 상태로 시작한다.

## 권한 모드 (방 단위)

| 모드 | Claude | Codex | Antigravity |
|---|---|---|---|
| `chat` | `--tools ""` | `sandbox_mode=read-only`, cwd=스크래치 | `--mode plan --sandbox` |
| `read` (기본) | `--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch --add-dir <ws>` | `sandbox_mode=read-only --cd <ws>` | `--mode plan --add-dir <ws>` |
| `write` | `--permission-mode acceptEdits --add-dir <ws>` | `sandbox_mode=workspace-write` | `--mode accept-edits --add-dir <ws>` |

어떤 모드에서도 `--dangerously-*` 류 우회 플래그는 쓰지 않는다. `read`에서 Claude에 `Agent/Task`를 남겨 둔 이유는 서브에이전트 사용을 관찰하기 위해서다(서브에이전트도 같은 도구 제한을 받는다).

## 데이터

`~/.roundtable/rooms/<id>/` 에 `room.json`(설정·참가자·세션 ID·누적 사용량), `messages.jsonl`(메시지, 마지막 줄 우선), `events.jsonl`(작업 기록) 이 쌓인다. 비밀번호·토큰은 저장하지 않지만 에이전트가 읽은 파일 내용이 작업 기록에 남으므로, 비밀이 든 폴더를 작업 폴더로 주지 않는다. `vms.yaml` 같은 파일이 있는 폴더는 `chat` 모드로 쓰거나 폴더를 좁혀라.

## 각 CLI에서 뭘 읽는지 (관측 한계)

| | 호출 | 스트림 | 세션 재개 | 서브에이전트 관측 | 한도 |
|---|---|---|---|---|---|
| Claude Code | `claude -p --input-format stream-json --output-format stream-json` | 텍스트 델타, 도구 호출/결과 | `--resume <session_id>` | 스트림의 `parent_tool_use_id` + `task_started/task_notification` 이벤트 | 스트림의 `rate_limit_event` (5시간/7일) |
| Codex | `codex exec - --json` | 완성된 메시지 블록, 명령 실행 | `codex exec resume <thread_id> -` | `~/.codex/sessions/.../rollout-*.jsonl` 중 `parent_thread_id`가 이 스레드인 파일을 1.5초마다 tail | 같은 rollout의 마지막 `token_count.rate_limits` (7일·5시간, 크레딧, 플랜) |
| Antigravity | `agy --output-format stream-json -p=<prompt>` | 텍스트 델타, 도구 단계 | `--conversation <id>` | `invoke_subagent` 단계의 `log_uri` → `transcript.jsonl` 읽기 | CLI가 노출하지 않음(표시 안 됨) |

- agy는 프롬프트를 명령줄 인자로만 받아서 24KB를 넘으면 앞뒤만 남기고 가운데를 생략한다(세션 재개 덕분에 평소엔 작다).
- Codex가 `Code Mode is unavailable … code-mode host` 를 내면 그 설치본에 `codex-code-mode-host.exe`가 없는 것이다. 보통 셸 도구로 계속 진행하지만, 모델이 포기하는 턴도 있었다.
- 컨텍스트 사용량은 마지막 API 호출의 입력 토큰(캐시 포함)이다. 창 크기는 Codex는 CLI가 알려주는 값(`model_context_window`), Claude는 200k(`[1m]` 모델 1M), Gemini는 1M으로 둔다.

## 테스트

```bash
go test ./...
```

`internal/agent/testdata/` 는 실제 CLI 출력에서 캡처한 픽스처다. 실제 CLI를 붙인 통합 시나리오(브로드캐스트·멘션·세션 재개·서브에이전트·steering·토론·중지)는 `docs/통합테스트_20261016.md` 에 결과를 기록했다.

## 참고한 것

- [nokryong/CodePet](https://github.com/nokryong/CodePet) (MIT): 단톡방 프롬프트 구조(참가자 명단·규칙·`@`호출·토론 신호), 공급사별 argv, 한도 데이터 위치. Electron 대신 Go 서버로 다시 썼다.
- [openai/codex-plugin-cc](https://github.com/openai/codex-plugin-cc): 리뷰/위임을 다른 공급사에 넘기는 발상.
