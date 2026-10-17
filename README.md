# Roundtable

**Claude Code · Codex · Antigravity를 한 단톡방에.** 세 공급사의 코딩 에이전트 CLI를 같은 대화에 앉혀 놓고, 사람이 지켜보다가 필요할 때 끼어드는 로컬 웹 서비스다. 같은 질문에 서로 다른 모델이 독립적으로 답하고, 서로의 답을 반박하고, 서브에이전트까지 띄워 일하는 과정을 한 화면에서 본다.

Go 단일 바이너리 + 정적 HTML. 프레임워크·빌드 도구·외부 서비스 없음. 각 CLI는 당신이 이미 로그인한 공식 CLI를 그대로 실행한다. 인증 정보는 읽지도 저장하지도 않는다.

## 할 수 있는 것

| | |
|---|---|
| **단톡방** | `@claude` `@codex` `@agy`는 해당 공급사의 활성 참가자 전원, 카드의 고유 `@ID`는 한 명을 부른다. `@all` 또는 멘션 없이 보내면 전원이 같은 대화 범위를 받아 **동시에, 독립적으로** 답한다. 연쇄 호출 깊이는 방 설정으로 제한한다. |
| **토론** | N라운드 동안 순서대로 발언한다. 각자 마지막 줄에 `CONTINUE / AGREE / PASS / CONCLUDE` 신호를 붙이고, 전원 동의·결론 제시·라운드 소진 중 먼저 오는 쪽에서 끝난다. |
| **끼어들기(steering)** | 대상을 고르고 보내면 그 에이전트에게만 간다. Claude는 실행 중인 턴에 즉시 주입되고, Codex·Antigravity는 지금 턴이 끝난 직후 우선 전달된다. 각 참가자는 자기 대기열을 갖고 서로를 기다리지 않는다. |
| **작업 기록** | 답변마다 도구 호출·결과·추론 요약을 펼쳐 본다. **서브에이전트**는 어떤 종류를 어떤 과제로 띄웠고, 그 안에서 무슨 도구를 썼고, 뭐라고 보고했는지까지 중첩으로 보인다. 진행 중에도 실시간으로 자란다. |
| **사용량** | 공급사별 5시간·주간 남은 한도(Claude·Codex·Gemini), 방 누적 입력·출력 토큰과 비용, 최근 소모 속도, 참가자별 컨텍스트 사용률. 임계치에서 경고한다. |
| **모델·인원·추론 수준** | 같은 공급사와 모델도 여러 명 추가한다. 방마다 최대 32명이며 이름·모델·추론 수준·세션·대기열·사용량은 참가자별로 분리된다. 모델별 지원 수준과 CLI 기본값을 선택할 수 있다. |
| **원본 CLI 대화** | 참가자 카드와 답변의 `CLI에서 열기`로 원래 세션을 이어 연다(Windows). 다른 OS에서는 정확한 이어보기 명령을 복사한다. 모델을 바꾼 뒤에도 이전 답변의 세션 정보는 남는다. |
| **권한 모드** | 방 단위 `chat`(프로젝트 폴더를 전달하지 않는 대화) / `read`(읽기 중심) / `write`(편집 허용). 실제 보호는 각 CLI의 도구·권한·샌드박스 설정에 따른다. |

## 빠른 시작

전제: `claude`, `codex`, `agy` 중 쓸 CLI가 PATH에 있고 각각 로그인돼 있어야 한다. 없는 CLI는 "CLI를 찾지 못함"으로 표시되고 꺼진 채 시작한다.

```bash
go build -o roundtable.exe ./cmd/roundtable
./roundtable.exe -open
```

브라우저가 `http://127.0.0.1:8787` 로 열린다. "새 방"에서 작업 폴더와 권한 모드를 정하고 질문을 던진다.

옵션: `-addr`(기본 `127.0.0.1:8787`), `-data`(기본 `~/.roundtable`), `-claude` `-codex` `-agy`(명령 이름 또는 절대 경로), `-agy-status`(Gemini 한도 파일), `-open`.

### 여러 모델을 함께 쓰기

1. **새 방**에서 공급사·모델·추론 수준·인원을 고른다. **다른 모델 구성**으로 Opus 2명, Fable 1명, GPT 2명, Gemini 2명 같은 조합을 만든다.
2. 기존 방에서는 참가자 패널의 **추가**를 누른다. 모델 목록에 없는 모델은 CLI가 지원하는 정확한 ID를 직접 입력할 수 있다. 목록에 있다고 계정의 사용 권한이나 잔여 한도까지 보장되는 것은 아니다.
3. 특정 한 명에게 지시할 때는 대상 조정 칩이나 카드의 고유 멘션을 쓴다. `@claude`는 Claude 참가자 전체를 부른다. 이름을 바꿔도 고유 ID는 유지된다.

참가자를 제거해도 이전 답변과 누적 사용량은 남는다. 모델·추론 수준 변경은 실행 중에는 거절하며, 변경된 참가자의 다음 턴은 새 CLI 세션으로 시작한다. 공급사 한도는 계정 단위이므로 참가자 수만큼 늘어나지 않는다.

### 원본 CLI에서 이어 보기

Roundtable은 각 공급사의 일반 세션 저장 기능을 사용한다. 대화가 끝난 참가자 카드 또는 답변 아래에서 **CLI에서 열기**를 누르면 같은 세션 ID를 사용해 원본 CLI를 연다. 현재 세션을 열면 해당 참가자를 비활성화한다. CLI 창이 열려 있는 동안 같은 세션을 다시 열거나 참가자를 다시 켜는 요청은 거절한다. CLI 창을 닫은 뒤 참가자를 다시 켜면 된다. **명령 복사**만으로는 참가자를 비활성화하지 않으므로, 복사한 명령을 실행하기 전에는 직접 꺼야 한다.

| 공급사 | 원본 세션을 지정하는 명령 |
|---|---|
| Claude Code | `claude --resume <session_id>` |
| Codex | `codex resume --include-non-interactive --cd <directory> <session_id>` |
| Antigravity | `agy --conversation <session_id>` |

화면에서 제공하는 명령에는 당시 모델·추론 수준·권한 옵션도 포함된다. 새 메시지에는 원본 세션 ID와 작업 폴더를 함께 보존한다. 이전 버전의 기록에는 작업 폴더 정보가 없어서 과거 세션의 열기가 제한될 수 있다. 원본 CLI에서 별도로 나눈 새 대화가 Roundtable 기록으로 역수입되지는 않는다. 각 공급사 데스크톱 앱의 대화 목록 동기화는 앱별 동작에 따른다. 열린 CLI 창의 추적은 현재 서버 프로세스에서만 유지되므로 서버를 재시작하기 전에는 해당 창을 닫는다.

### Gemini 한도 보이게 하기 (선택)

Antigravity CLI는 [공식 statusLine 기능](https://antigravity.google/docs/cli/statusline)으로, 상태표시줄을 그릴 때마다(비대화형 `-p` 실행 포함) 남은 할당량을 JSON으로 명령에 넘긴다. Roundtable은 그 명령 앞에 끼어들어 값을 받아 적고, 원래 명령은 그대로 이어 실행한다.

```bash
./roundtable.exe agy-hook install     # ~/.gemini/antigravity-cli/settings.json 의 statusLine 한 키만 바꿈, 원본 백업
./roundtable.exe agy-hook status      # 수집된 값 확인
./roundtable.exe agy-hook uninstall   # 원래대로
```

저장하는 것은 유효한 한도를 포함한 상태의 `quota`·`context_window`·`model`·`plan_tier`·`version` 뿐이고 이메일·경로는 버린다. 한도가 없거나 비어 있는 갱신은 파일을 쓰지 않는다. 여러 Gemini 참가자가 동시에 실행돼도 한도 없는 갱신이 이전 값을 복사해 새 값을 덮어쓰지 않는다. **OAuth 토큰으로 Google API를 직접 부르는 방식은 쓰지 않는다.** Antigravity 추가 약관 6항은 서드파티 도구가 Antigravity OAuth로 서비스에 접근하는 것을 계정 정지 사유로 명시한다.

## 권한 모드

| 모드 | Claude Code | Codex | Antigravity |
|---|---|---|---|
| `chat` | `--tools ""` | `sandbox_mode=read-only`, 스크래치 폴더 | `--mode plan --sandbox` |
| `read` (기본) | `--tools Read,Grep,Glob,Agent,Task,WebSearch,WebFetch --add-dir <ws>` | `sandbox_mode=read-only --cd <ws>` | `--mode plan --sandbox --add-dir <ws>` |
| `write` | `--permission-mode acceptEdits --add-dir <ws>` | `sandbox_mode=workspace-write` | `--mode accept-edits --sandbox --disable-slash-commands --add-dir <ws>` |

2026-10-17의 9가지 호출에서는 read·chat의 요청 대상 파일 생성이 거절되고 write의 생성이 허용됐다. 이 관측은 모든 도구 호출·읽기 범위·서브에이전트 동작에 대한 보장은 아니다. 특히 Codex·Antigravity의 chat은 읽기 또는 계획 도구가 남아 있을 수 있다. Claude의 도구 목록과 `--add-dir`도 운영체제 수준의 파일 격리를 의미하지 않는다. Antigravity는 `--disable-slash-commands`와 `--mode plan`을 같이 주면 plan이 무시되는 것을 확인했기 때문에, 그 플래그는 write 모드에만 붙인다.

서버는 인증 없는 로컬 도구다. loopback 클라이언트와 로컬 Host만 허용하고, 다른 웹사이트의 Origin 또는 교차 사이트 요청을 거절한다. `-addr`를 외부 주소로 열더라도 원격 클라이언트는 허용하지 않는다.

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
