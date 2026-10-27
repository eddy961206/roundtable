# 다중 모델 오케스트레이션 도우미

roundtable 방(작성·검수 좌석)과 Gitea PR·리뷰를 오케스트레이터 한 명이 이어 붙일 때 쓰는 Bash·Python 스크립트다. 2026-10-27 회고에서 반복된 실수를 막으려고 만들었다.

- 인라인 셸 인용에서 백틱 값이 사라진 댓글이 게시됐다 → 본문은 항상 파일에서 읽고, 게시 전에 `glint.py`로 검사한다.
- PR 본문에 작성자 초안 문구·로컬 경로가 남았다 → `glint.py`가 막는다.
- 자동 병합이 "상대 개발자 승인 뒤 병합" 약속을 어겼다 → 이 폴더의 어떤 스크립트도 병합하지 않는다.
- 30초 폴링으로 방의 답을 기다렸다 → `rtwait.sh`가 서버 SSE(`rt wait`)로 기다린다.

## 지켜야 할 규칙

- **제품 코드 PR에는 자동 병합을 쓰지 않는다.** CI 통과·검수 방 승인은 병합 근거가 아니다. 상대 개발자가 승인한 것을 확인한 뒤 사람이 손으로 병합한다. `ci_wait.sh`는 CI 상태만 알려 준다.
- **`rt wait`에는 after_seq를 준다.** `rtwait.sh`는 `방:seq` 형식만 받는다. seq는 `rt post` 결과의 `seq`다. after_seq를 빼면 서버는 대기를 시작한 뒤 생긴 답만 센다. 그래서 대기 전에 이미 도착한 답은 보이지 않고 timeout까지 기다린다.
- Gitea 게시물에 `Generated with`·`Co-Authored-By`·대리 작성 표식을 넣지 않는다. `glint.py`가 막는다.
- 토큰을 이 폴더·설정 파일·명령 인자에 넣지 않는다. `tea`는 자기 로그인 설정을, `git push`는 Git Credential Manager를 쓴다.

## 준비

1. `tea`(Gitea 공식 CLI)에 대상 Gitea 로그인이 되어 있어야 한다. `roundtable`과 Python 3, Bash(Windows는 Git Bash)가 PATH에 있어야 한다.
2. `orchestrate.env.example`을 `local/orchestrate.env`로 복사하고 값을 채운다. `local/`은 gitignore 대상이다. 다른 위치를 쓰려면 `ORCH_ENV=<파일>`을 지정한다.
3. `templates/`의 지시문을 `local/`로 복사해 프로젝트의 기준 문서·검사 명령을 채운다. `ORCH_WRITER_RULES`·`ORCH_REVIEW_TEMPLATE`이 그 사본을 가리키게 한다.

설정 우선순위: 실행 전에 지정한 `ORCH_*` 환경변수 → `ORCH_ENV` 파일 → 기본값. 파일은 Bash로 source하므로 `$HOME`·`$ORCH_DIR`(이 폴더)를 쓸 수 있다. 예: `ORCH_REPO=owner/other tools/orchestrate/gpost.sh comment 3 body.md`는 그 호출만 다른 저장소에 게시한다.

| 변수 | 쓰는 곳 | 기본값 |
|---|---|---|
| `ORCH_REPO` | gpost·assign_fix·ci_wait·reviewer_check (`owner/name`) | 없음(필수) |
| `ORCH_REVIEWER` | gpost pr의 리뷰 요청, reviewer_check | 없음(gpost는 생략 가능) |
| `ORCH_WATCH_REPOS` | reviewer_check 감시 저장소(쉼표) | `ORCH_REPO` |
| `ORCH_BASE` | PR base, queue_review rebase 대상 | `main` |
| `ORCH_WORKTREES` | queue_review에 이름만 줄 때의 상위 폴더 | 없음 |
| `ORCH_WRITER_RULES` / `ORCH_REVIEW_TEMPLATE` | assign_fix / queue_review 지시문 | 없음(필수) |
| `ORCH_CONFLICT_HOOK` | queue_review rebase 충돌 해결 명령 | 없음(충돌 시 중단) |
| `ORCH_CI_CONTEXT` | ci_wait가 볼 상태 context 부분 문자열 | 없음(통합 state) |
| `ORCH_LINT_EXTRA` | glint가 로컬 경로로 볼 추가 정규식 | 없음 |
| `ORCH_TEA` / `ORCH_RT_BIN` / `ORCH_RT_URL` | 실행 파일·서버 | `tea` / `roundtable` / `http://127.0.0.1:18787` |
| `ORCH_STATE` | 게시 임시 JSON·리뷰 원문 저장 | `$HOME/.rt-orchestrate` |
| `ORCH_PYTHON` | Python 실행 파일 | `python`, 없으면 `python3` |

## 도구

모든 셸 스크립트는 인자·필수 설정이 틀리면 아무 호출도 하지 않고 종료 코드 2로 끝난다.

### `rtwait.sh` — 방의 답 기다리기

```bash
tools/orchestrate/rtwait.sh <방A>:<seqA> <방B>:<seqB>
```

- 하나 이상의 방이 끝나면 돌아온다. 끝난 방마다 `READY <방> <상태> next_seq <n>`과 마지막 좌석 답의 끝부분(`ORCH_WAIT_TAIL`, 기본 1500자)을 출력한다. `WAITING` 줄은 남은 방이며 다음 호출 인자로 그대로 쓴다(쉼표를 공백으로 바꾼다).
- `ORCH_WAIT_TIMEOUT`(기본 3600초)이 지나면 `TIMEOUT`을 출력한다. 시간 초과는 실행 중인 턴을 취소하지 않는다.
- 관측 한계: "끝남"은 실행·대기 중인 턴이 없고 after_seq 뒤에 좌석 답이 있다는 뜻이다. READY는 답이 성공했다는 뜻이 아니므로 상태 칸과 오류 문구를 확인한다. 답 전체는 `rt read --room <방> --after_seq <seq>`로 읽는다. 서버가 재기동되면 SSE가 끊겨 오류로 끝난다. 이때 답이 없는 것으로 보지 말고 다시 실행한다.
- 오래 걸리므로 백그라운드 명령으로 실행한다.

### `gpost.sh` + `glint.py` — Gitea 게시

```bash
tools/orchestrate/gpost.sh comment <이슈·PR 번호> <본문.md>
tools/orchestrate/gpost.sh pr <worktree> <branch> "<제목>" <본문.md>
python tools/orchestrate/glint.py <본문.md> [...]
```

- `comment`: 본문 파일을 그대로 JSON으로 만들어 `tea api`로 댓글을 단다. 출력은 `comment <id> on #<번호>`.
- `pr`: worktree가 깨끗한지 확인하고 branch를 push한 뒤 PR을 만든다(base는 `ORCH_BASE`). `ORCH_REVIEWER`가 있으면 리뷰를 요청한다. 리뷰 요청이 실패하면 PR 번호를 출력한 뒤 종료 코드 1로 끝난다. 병합은 하지 않는다.
- `glint.py`는 아래를 찾으면 `LINT: 파일:줄: 이유: 일치 문자열`을 출력하고 종료 코드 1을 낸다. 빈 본문도 막는다.
  - 작성자 초안 문구(`push·PR`, `PR 게시`, `게시하지 않`, `로컬 초안`, `사용자가 별도로` 등)
  - 로컬 절대 경로(`C:\…`, `C:/…`, `/c/…/`, `/home/<u>/`, `/Users/<u>/`, `ORCH_LINT_EXTRA`)
  - 빈 코드 조각: 단독 ``` `` ```, `head`·`base`·`커밋`·`commit`·`sha` 뒤의 연속 공백
  - 대리 작성 표식: `Generated with`, `Co-Authored-By`, `대신 작성`
- 관측 한계: 정규식 검사다. 다른 표현의 초안 문구, 공백 없이 사라진 값, 상대 경로는 잡지 못한다. 규칙마다 첫 일치만 보고한다. PR 제목은 검사하지 않는다. `tea`가 저장소에 맞는 로그인을 찾지 못하면 기본 로그인으로 게시하며 `NOTE:` 줄을 남긴다. 게시 뒤 원문은 Gitea에서 다시 조회해 확인한다.

### `assign_fix.sh` — 리뷰를 작성 방에 배정

```bash
tools/orchestrate/assign_fix.sh <PR 번호> <리뷰 id> <작성 방> <branch> ["추가 지시"]
```

- 먼저 방의 작업 폴더(workspace)가 있는지 확인한다. 없으면 게시하지 않고 `rt update_room`으로 폴더를 바꾸라고 알린다.
- Gitea에서 리뷰 원문을 가져와 `$ORCH_STATE/rv<리뷰id>_pr<번호>.md`에 저장한다. 고정 지시 + `ORCH_WRITER_RULES` + 리뷰 원문을 작성 방에 보내고 `방:seq`를 출력한다. 이 값을 `rtwait.sh`에 넘긴다.
- 관측 한계: 리뷰 본문만 보낸다. 줄 단위 리뷰 댓글(`pulls/<n>/reviews/<id>/comments`)은 포함하지 않는다. 작업 폴더의 존재만 확인하며 그 폴더가 해당 branch에 있는지는 확인하지 않는다.

### `queue_review.sh` — 검수 방에 검수 요청

```bash
NOREBASE=1 PRIME=<기반 문맥.md> tools/orchestrate/queue_review.sh <label> <worktree> <작성 방> <검수 방> "<검수 초점>"
```

- `<worktree>`는 폴더 경로 또는 `ORCH_WORKTREES` 아래 이름이다.
- 작성 방의 마지막 답 끝부분을 출력하고, worktree를 fetch한 뒤 `origin/$ORCH_BASE`로 rebase한다. `NOREBASE=1`이면 rebase하지 않는다. 다른 열린 PR 위에 쌓인 branch에 쓴다.
- rebase 충돌은 `ORCH_CONFLICT_HOOK`이 있으면 충돌 파일 목록을 인자로 그 명령을 실행한다(최대 4회). 명령은 충돌을 풀고 `git add`해야 한다. 없거나 실패하면 `CONFLICT <파일>`을 출력하고 종료 코드 2로 끝난다. 이때 rebase는 진행 중 상태로 남으므로 손으로 마무리하거나 `git rebase --abort`한다.
- `PRIME` + 검수 머리말(worktree 경로·branch·초점·판정 줄 형식) + `ORCH_REVIEW_TEMPLATE`을 검수 방에 보내고 `queued <label> 방:seq`를 출력한다.
- 관측 한계: rebase 결과를 push하지 않는다. 열린 PR과 원격 branch는 이전 head 그대로다. 검수 머리말의 worktree 경로는 Git Bash에서 Windows 형식으로 바꿔 적는다(모델이 Windows에서 실행된다고 가정).

### `ci_wait.sh` — CI 결과 기다리기(병합 없음)

```bash
tools/orchestrate/ci_wait.sh <PR 번호>
```

- PR의 현재 head 커밋 상태를 `ORCH_CI_INTERVAL`(기본 30초)마다 최대 `ORCH_CI_TRIES`(기본 80)회 조회한다. pending·none을 벗어나면 `#<번호> head=<sha7> <context>=<상태>`를 출력한다. 끝까지 pending이면 종료 코드 3이다.
- 관측 한계: Gitea 상태 알림을 받을 수단이 없어 폴링한다. 백그라운드 명령으로 실행한다. 대기 중 head가 바뀌면 다음 조회부터 새 head를 본다. `ORCH_CI_CONTEXT`가 없으면 통합 state를 쓴다. 통합 state는 상태가 하나라도 pending이면 pending이다.

### `reviewer_check.py`·`reviewer_watch.sh` — 상대 개발자 반응 감시

```bash
python tools/orchestrate/reviewer_check.py 2026-10-27T05:00:00Z [--user <login>] [--repos owner/a,owner/b]
tools/orchestrate/reviewer_watch.sh 2026-10-27T05:00:00Z
```

- `reviewer_check.py`는 지정 시각 뒤 그 사용자의 이슈·PR 댓글, PR 리뷰(APPROVED·REQUEST_CHANGES·COMMENT), 새 이슈·PR을 `시각 | 저장소 | 종류 | 번호 | id | 앞 400자`로 출력한다. 없으면 아무것도 출력하지 않는다. `tea` 호출이 실패하면 "반응 없음"이 아니라 오류로 끝난다.
- `reviewer_watch.sh`는 `ORCH_WATCH_INTERVAL`(기본 300초)마다 최대 `ORCH_WATCH_TRIES`(기본 24)회 확인한다. 첫 반응을 출력하고 끝난다. 반응이 없으면 종료 코드 3이다.
- 관측 한계: 저장소마다 최근 갱신 PR 15개, 댓글 50개, 이슈 30개만 본다. 그보다 많이 움직인 구간은 빠질 수 있다. 줄 단위 리뷰 댓글과 반응(이모지)은 보지 않는다. 시각 비교는 ISO-8601 문자열 비교라서 UTC `Z` 형식으로 준다. 다음 감시의 기준 시각은 호출한 쪽이 기억해야 한다.

## 템플릿과 로컬 프로필

- `templates/writer-rules.md`: `assign_fix.sh`가 붙이는 작성 공통 규칙의 일반형이다.
- `templates/review-template.md`: `queue_review.sh`가 보내는 검수 지시의 일반형이다. 기준 문서 자리를 프로젝트에 맞게 채운다.
- `local/`: 프로젝트 전용 설정·지시문·충돌 해결 명령을 둔다. 커밋되지 않으므로 다른 PC에서는 다시 만든다.

## 시험

```bash
python -m unittest discover -s tools/orchestrate -p "test_*.py"
```

- `glint.py` 규칙별 일치·비일치, 줄 번호, 종료 코드를 확인한다.
- `reviewer_check.py`의 인자·기본값 처리를 확인한다.
- 셸 스크립트는 가짜 `tea`·`rt` 실행 파일로 돌린다. 인자 오류에서 아무 호출도 하지 않는지, 필수 설정 누락, 설정 파일 적용, 본문 원문 보존(백틱·`$`·따옴표), lint 실패 시 게시 중단, `rt wait` 인자 조립, CI context 선택을 확인한다.
- 시험하지 않는 것: 실제 Gitea·roundtable 서버 호출, `gpost.sh pr`의 push, `queue_review.sh`의 rebase·충돌 해결, 폴링 반복.
- Windows에서 `bash`가 WSL 실행기로 잡히면 Git Bash를 찾아 쓴다. 다른 Bash를 쓰려면 `ORCH_BASH`를 지정한다. Bash가 없으면 셸 시험은 건너뛴다.
