# Roundtable

**Claude Code · Codex · Antigravity를 한 단톡방에.** 세 공급사의 코딩 에이전트 CLI를 같은 대화에 앉혀 놓고, 사람이 지켜보다가 필요할 때 끼어드는 로컬 웹 서비스다. 같은 질문에 서로 다른 모델이 독립적으로 답하고, 서로의 답을 반박하고, 서브에이전트까지 띄워 일하는 과정을 한 화면에서 본다.

Go 단일 바이너리 + 정적 HTML. 프레임워크·빌드 도구·외부 서비스 없음. 각 CLI는 당신이 이미 로그인한 공식 CLI를 그대로 실행한다. 인증 정보는 읽지도 저장하지도 않는다.

## 할 수 있는 것

| | |
|---|---|
| **단톡방** | `@claude` `@codex` `@agy` `@all`로 부른다. 멘션이 없으면 전원이 같은 대화 스냅샷을 받아 **동시에, 독립적으로** 답한다. 에이전트끼리도 `@`로 서로 부르며, 연쇄 깊이는 방 설정으로 제한한다. |
| **토론** | N라운드 동안 순서대로 발언한다. 각자 마지막 줄에 `CONTINUE / AGREE / PASS / CONCLUDE` 신호를 붙이고, 전원 동의·결론 제시·라운드 소진 중 먼저 오는 쪽에서 끝난다. |
| **끼어들기(steering)** | 대상을 고르고 보내면 그 에이전트에게만 간다. Claude는 실행 중인 턴에 즉시 주입되고, Codex·Antigravity는 지금 턴이 끝난 직후 우선 전달된다. 각 참가자는 자기 대기열을 갖고 서로를 기다리지 않는다. |
| **작업 기록** | 답변마다 도구 호출·결과·추론 요약을 펼쳐 본다. **서브에이전트**는 어떤 종류를 어떤 과제로 띄웠고, 그 안에서 무슨 도구를 썼고, 뭐라고 보고했는지까지 중첩으로 보인다. 진행 중에도 실시간으로 자란다. |
| **사용량** | 공급사별 5시간·주간 남은 한도(Claude·Codex·Gemini), 방 누적 입력·출력 토큰과 비용, 최근 소모 속도, 참가자별 컨텍스트 사용률. 임계치에서 경고한다. |
| **모델·추론 노력** | 참가자마다 켜기/끄기, 모델, 추론 노력(low~max)을 고른다. 모델마다 허용 노력이 다르면 그 목록만 보인다. |
| **권한 모드** | 방 단위 `chat`(도구 없음) / `read`(읽기·검색만) / `write`(작업 폴더 수정). 우회 플래그는 어떤 모드에서도 쓰지 않는다. |

## 빠른 시작

전제: `claude`, `codex`, `agy` 중 쓸 CLI가 PATH에 있고 각각 로그인돼 있어야 한다. 없는 CLI는 "CLI를 찾지 못함"으로 표시되고 꺼진 채 시작한다.

```bash
go build -o roundtable.exe ./cmd/roundtable
./roundtable.exe -open
```

브라우저가 `http://127.0.0.1:8787` 로 열린다. "새 방"에서 작업 폴더와 권한 모드를 정하고 질문을 던진다.

옵션: `-addr`(기본 `127.0.0.1:8787`), `-data`(기본 `~/.roundtable`), `-claude` `-codex` `-agy`(명령 이름 또는 절대 경로), `-agy-status`(Gemini 한도 파일), `-open`.

### Gemini 한도 보이게 하기 (선택)

Antigravity CLI는 [공식 statusLine 기능](https://antigravity.google/docs/cli/statusline)으로, 상태표시줄을 그릴 때마다(비대화형 `-p` 실행 포함) 남은 할당량을 JSON으로 명령에 넘긴다. Roundtable은 그 명령 앞에 끼어들어 값을 받아 적고, 원래 명령은 그대로 이어 실행한다.

```bash
./roundtable.exe agy-hook install     # ~/.gemini/antigravity-cli/settings.json 의 statusLine 한 키만 바꿈, 원본 백업
./roundtable.exe agy-hook status      # 수집된 값 확인
./roundtable.exe agy-hook uninstall   # 원래대로
```

저장하는 것은 `quota`·`context_window`·`model`·`plan_tier`·`version` 뿐이고 이메일·경로는 버린다. **OAuth 토큰으로 Google API를 직접 부르는 방식은 쓰지 않는다.** Antigravity 추가 약관 6항은 서드파티 도구가 Antigravity OAuth로 서비스에 접근하는 것을 계정 정지 사유로 명시한다.

## 권한 모드

| 모드 | Claude Code | Codex | Antigravity |
|---|---|---|---|
| `chat` | `--tools ""` | `sandbox_mode=read-only`, 스크래치 폴더 | `--mode plan --sandbox` |
| `read` (기본) | `--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch --add-dir <ws>` | `sandbox_mode=read-only --cd <ws>` | `--mode plan --sandbox --add-dir <ws>` |
| `write` | `--permission-mode acceptEdits --add-dir <ws>` | `sandbox_mode=workspace-write` | `--mode accept-edits --sandbox --disable-slash-commands --add-dir <ws>` |

세 CLI × 세 모드 9가지를 실제 호출로 검증했다: read·chat은 파일을 만들지 못했고 write는 만들었다. Antigravity는 `--disable-slash-commands`와 `--mode plan`을 같이 주면 plan이 무시되어 read 모드에서도 파일이 써지는 것을 확인했기 때문에, 그 플래그는 write 모드에만 붙인다.

## 데이터

`~/.roundtable/rooms/<id>/` 에 `room.json`(설정·참가자·세션 ID·누적 사용량), `messages.jsonl`, `events.jsonl`(작업 기록)이 쌓인다. 에이전트가 읽은 파일 내용은 작업 기록에 남는다. **비밀이 든 폴더는 작업 폴더로 주지 마라.**

## 각 CLI에서 무엇을 어떻게 읽는가

| | 호출 | 세션 재개 | 서브에이전트 관측 | 한도 |
|---|---|---|---|---|
| Claude Code | `claude -p --input-format stream-json --output-format stream-json` | `--resume <session>` | 스트림의 `parent_tool_use_id`, `task_started/task_notification` | 스트림의 `rate_limit_event` |
| Codex | `codex exec - --json` | `codex exec resume … <thread> -` | `~/.codex/sessions` 에서 `parent_thread_id` 가 같은 rollout 파일을 추적 | rollout 의 `token_count.rate_limits` |
| Antigravity | `agy --output-format stream-json -p=<prompt>` | `--conversation <id>` | `invoke_subagent` 의 `log_uri` → `transcript.jsonl` | statusLine 훅(위) |

- 각 에이전트는 자기 CLI 세션을 이어 가며 **자기 마지막 발언 이후 새 메시지만** 받는다. 매번 전체 대화를 보내지 않아 토큰이 덜 들고, 각자의 도구 맥락이 유지된다. 턴이 실패하면 그 메시지는 다음 턴에 다시 전달된다.
- agy는 프롬프트를 명령줄 인자로만 받는다. 이스케이프 후 24KB를 넘으면 앞뒤만 남긴다.
- Windows에서 npm으로 설치한 Codex는 `codex.cmd` 뒤의 실제 `codex.exe`를 찾아 실행한다(별도 설치본에 code-mode host가 빠진 경우가 있었다).
- Codex의 Windows 샌드박스가 간헐적으로 `setup refresh had errors` 로 명령 실행에 실패하는 턴이 있다. CLI 쪽 문제이며 다시 시키면 대개 통과한다.

## 개발

```bash
go test ./...
```

- `internal/agent` — CLI 실행과 스트림 파서. `testdata/` 는 실제 CLI 출력 캡처(로컬 경로 정제).
- `internal/room` — 방·메시지·대기열·토론·SSE 팬아웃, JSONL 저장.
- `internal/agyhook` — Antigravity statusLine 훅 설치·제거.
- `internal/server` — JSON API + SSE + 정적 UI.
- `web/` — 단일 페이지 UI. 설계 기준은 [디자인 브리프](docs/디자인_브리프.md).

## 어떻게 만들었나

이 저장소 자체가 세 모델의 협업으로 만들어졌다. Claude가 설계·통합·검증을 맡아 오케스트레이션하고, Codex(GPT-6.1 Sol)가 백엔드 스케줄러와 웹 조사·결함 리뷰를, Gemini 4 Argon이 UI를 맡았다. 작업 지시도 Roundtable의 write 모드 방으로 했다. 과정과 검증 결과는 [통합 테스트 기록](docs/통합테스트_20261016.md)과 [협업 기록](docs/협업기록_20261017.md)에 있다.

## 라이선스

MIT. 동봉한 `marked` 와 참고한 프로젝트는 [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
