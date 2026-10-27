# 다중 모델 오케스트레이션 도우미

roundtable 방(작성·검수 좌석)과 Gitea PR·리뷰를 오케스트레이터 한 명이 이어 붙일 때 쓰는 Bash·Python 스크립트다. 2026-10-27 회고에서 반복된 실수를 막으려고 만들었다.

- 인라인 셸 인용에서 백틱 값이 사라진 댓글이 게시됐다 → 본문은 항상 파일에서 읽고, 게시 전에 `glint.py`로 검사한다.
- PR 본문에 작성자 초안 문구·로컬 경로가 남았다 → `glint.py`가 막는다.
- 자동 병합이 "상대 개발자 승인 뒤 병합" 약속을 어겼다 → 이 폴더의 어떤 스크립트도 병합하지 않는다.
- 30초 폴링으로 방의 답을 기다렸다 → `rtwait.sh`가 서버 SSE(`rt wait`)로 기다린다.
- PR 하나가 병합될 때마다 다른 열린 PR이 충돌해 재기반을 8번 했다. 줄이 겹치지 않는 의미 충돌은 PR을 합쳐 시험하고서야 찾았다. 스키마 번호를 열린 PR 셋이 같이 썼다 → `pr_overlap.py`가 병합 전에, `after_merge.py`가 병합 직후에 알린다.
- 같은 worktree에서 한 방이 고치는 동안 다른 방이 검수를 돌렸다 → `worktree_claims.py`가 보내기 전에 막는다. `assign_fix.sh`·`queue_review.sh`·`after_merge.py --assign`이 이 검사를 쓴다.

## 지켜야 할 규칙

- **제품 코드 PR에는 자동 병합을 쓰지 않는다.** CI 통과·검수 방 승인은 병합 근거가 아니다. 상대 개발자가 승인한 것을 확인한 뒤 사람이 손으로 병합한다. `ci_wait.sh`는 CI 상태만 알려 준다.
- **push 뒤 게시는 push를 확인한 뒤에만 한다.** push 종료 코드가 0이고 원격 브랜치 head가 로컬 HEAD와 같을 때만 PR·댓글·재리뷰 요청을 보낸다. `gpost.sh pr`과 `push_and_notify.sh`가 이 순서를 지킨다. 손으로 push했다면 `git ls-remote origin refs/heads/<branch>`로 확인한 뒤 게시한다(2026-10-27 VMS-next #66: 인증 실패로 끝난 force push 뒤 "새 head로 반영" 댓글이 먼저 나갔다).
- **`rt wait`에는 after_seq를 준다.** `rtwait.sh`는 `방:seq` 형식만 받는다. seq는 `rt post` 결과의 `seq`다. after_seq를 빼면 서버는 대기를 시작한 뒤 생긴 답만 센다. 그래서 대기 전에 이미 도착한 답은 보이지 않고 timeout까지 기다린다.
- Gitea 게시물에 `Generated with`·`Co-Authored-By`·대리 작성 표식을 넣지 않는다. `glint.py`가 막는다.
- Windows에서 한글이 든 JSON은 인라인 인자(`curl -d '{...}'`) 대신 UTF-8로 저장한 파일을 `curl --data-binary @파일`로 보낸다. 인라인 인자는 콘솔 코드 페이지 바이트로 넘어갈 수 있다. roundtable 서버는 UTF-8이 아닌 본문을 400으로 거절한다.
- 토큰을 이 폴더·설정 파일·명령 인자에 넣지 않는다. `tea`는 자기 로그인 설정을, `git push`는 Git Credential Manager를 쓴다.
- 한 worktree에는 한 번에 한 방만 작업을 보낸다. 작성 방이 실행 중이면 검수를 보내지 않는다. 작성 방 workspace가 상위 폴더면 `worktree_claims.py claim`으로 점유를 등록한다.
- roundtable 자체를 고칠 때는 저장소의 [동시 작업 규칙](../../CONTRIBUTING.md)을 따른다. 세션마다 worktree·브랜치를 쓴다. main에는 시험 통과 뒤 fast-forward로만 올린다. 서버는 `roundtable install`로 바이너리를 바꾸고 `roundtable restart`로만 재기동한다.

## 준비

1. `tea`(Gitea 공식 CLI)에 대상 Gitea 로그인이 되어 있어야 한다. `roundtable`과 Python 3, Bash(Windows는 Git Bash)가 PATH에 있어야 한다.
2. `orchestrate.env.example`을 `local/orchestrate.env`로 복사하고 값을 채운다. `local/`은 gitignore 대상이다. 다른 위치를 쓰려면 `ORCH_ENV=<파일>`을 지정한다.
3. `templates/`의 지시문을 `local/`로 복사해 프로젝트의 기준 문서·검사 명령을 채운다. `ORCH_WRITER_RULES`·`ORCH_REVIEW_TEMPLATE`이 그 사본을 가리키게 한다.

설정 우선순위: 실행 전에 지정한 `ORCH_*` 환경변수 → `ORCH_ENV` 파일 → 기본값. 파일은 Bash로 source하므로 `$HOME`·`$ORCH_DIR`(이 폴더)를 쓸 수 있다. Python 도구(`orchlib.py`)는 같은 파일의 `NAME=값` 줄만 읽고 `$VAR`·`${VAR}`만 치환한다. 작은따옴표 값은 그대로 둔다. 셸 스크립트가 부른 Python 도구는 셸이 푼 값을 환경변수로 받는다. 예: `ORCH_REPO=owner/other tools/orchestrate/gpost.sh comment 3 body.md`는 그 호출만 다른 저장소에 게시한다.

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
| `ORCH_REPO_DIR` | pr_overlap·after_merge가 읽는 로컬 clone, worktree_claims `--branch` 조회 | 없음(`--repo-dir`, 없으면 현재 폴더) |
| `ORCH_REMOTE` | PR head·base를 fetch할 원격 | `origin` |
| `ORCH_BRANCH_ROOMS` | 브랜치 → 작성 방(·worktree) 대응표 JSON | `local/branch_rooms.json`(없으면 빈 표) |
| `ORCH_CLAIMS` | worktree 점유 등록 파일 | `$ORCH_STATE/claims.json` |
| `ORCH_SCHEMA_REGEX` | 스키마 번호 정규식(1번 그룹이 번호) | `const schemaVersion = (\d+)` |
| `ORCH_INTEGRATE_TEST` | `pr_overlap.py --integrate` 시험 명령 | `go test ./...` |
| `ORCH_REBASE_TEMPLATE` | after_merge 재기반 지시문 | `templates/rebase-notice.md` |

## 도구

모든 셸 스크립트는 인자·필수 설정이 틀리면 아무 호출도 하지 않고 종료 코드 2로 끝난다.

### `rtwait.sh` — 방의 답 기다리기

```bash
tools/orchestrate/rtwait.sh <방A>:<seqA> <방B>:<seqB>
```

- 하나 이상의 방이 끝나면 돌아온다. 끝난 방마다 `READY <방> <상태> next_seq <n>`과 마지막 좌석 답의 끝부분(`ORCH_WAIT_TAIL`, 기본 1500자)을 출력한다. `WAITING` 줄은 남은 방이며 다음 호출 인자로 그대로 쓴다(쉼표를 공백으로 바꾼다).
- `ORCH_WAIT_TIMEOUT`(기본 3600초)이 지나면 `TIMEOUT`을 출력한다. 시간 초과는 실행 중인 턴을 취소하지 않는다.
- `ORCH_WAIT_SUMMARY=1`이면 `rt wait --summary true`로 기다린다. 좌석 답 본문 대신 판정 줄(`판정`·`결론`·`verdict` 뒤 `:`)과 마커·신호 줄(`[[...]]`, `WORK:DONE`)만 받는다. 원문은 `rt read`로 읽는다. 오케스트레이터가 긴 검수 원문을 읽지 않고 다음 단계를 정할 때 쓴다.
- 관측 한계: "끝남"은 실행·대기 중인 턴이 없고 after_seq 뒤에 좌석 답이 있다는 뜻이다. READY는 답이 성공했다는 뜻이 아니므로 상태 칸과 오류 문구를 확인한다. 답 전체는 `rt read --room <방> --after_seq <seq>`로 읽는다. 서버가 재기동되면 SSE가 끊겨 오류로 끝난다. 이때 답이 없는 것으로 보지 말고 다시 실행한다.
- 오래 걸리므로 백그라운드 명령으로 실행한다.

### `gpost.sh` + `glint.py` — Gitea 게시

```bash
tools/orchestrate/gpost.sh comment <이슈·PR 번호> <본문.md>
tools/orchestrate/gpost.sh pr <worktree> <branch> "<제목>" <본문.md>
python tools/orchestrate/glint.py <본문.md> [...]
```

- `comment`: 본문 파일을 그대로 JSON으로 만들어 `tea api`로 댓글을 단다. 출력은 `comment <id> on #<번호>`.
- `pr`: worktree가 깨끗한지 확인하고 branch를 push한다. push가 실패하거나 원격 브랜치 head가 로컬 HEAD와 다르면 PR을 만들지 않고 종료 코드 1로 끝난다. 그다음 PR을 만든다(base는 `ORCH_BASE`). `ORCH_REVIEWER`가 있으면 리뷰를 요청한다. 리뷰 요청이 실패하면 PR 번호를 출력한 뒤 종료 코드 1로 끝난다. 병합은 하지 않는다.
- `glint.py`는 아래를 찾으면 `LINT: 파일:줄: 이유: 일치 문자열`을 출력하고 종료 코드 1을 낸다. 빈 본문도 막는다.
  - 작성자 초안 문구(`push·PR`, `PR 게시`, `게시하지 않`, `로컬 초안`, `사용자가 별도로` 등)
  - 로컬 절대 경로(`C:\…`, `C:/…`, `/c/…/`, `/home/<u>/`, `/Users/<u>/`, `ORCH_LINT_EXTRA`)
  - 빈 코드 조각: 단독 ``` `` ```, `head`·`base`·`커밋`·`commit`·`sha` 뒤의 연속 공백
  - 대리 작성 표식: `Generated with`, `Co-Authored-By`, `대신 작성`
- 관측 한계: 정규식 검사다. 다른 표현의 초안 문구, 공백 없이 사라진 값, 상대 경로는 잡지 못한다. 규칙마다 첫 일치만 보고한다. PR 제목은 검사하지 않는다. `tea`가 저장소에 맞는 로그인을 찾지 못하면 기본 로그인으로 게시하며 `NOTE:` 줄을 남긴다. 게시 뒤 원문은 Gitea에서 다시 조회해 확인한다.

### `push_and_notify.sh` — 재기반·수정 뒤 push하고 PR에 알리기

```bash
tools/orchestrate/push_and_notify.sh <worktree> <branch> <PR 번호> <본문.md> [--force]
```

- 순서: 본문 검사(`glint.py`) → push → 원격 브랜치 head 확인 → PR head 확인 → 댓글 → 재리뷰 요청(`ORCH_REVIEWER`).
- 앞 단계가 하나라도 실패하면 뒤 단계는 하지 않고 종료 코드 1로 끝난다. 인자 오류는 2다.
- `--force`는 `--force-with-lease`로 push한다(재기반 뒤). 원격 추적 브랜치가 오래됐으면 lease가 거절되어 아무것도 게시하지 않는다. 그때는 fetch한 뒤 다시 실행한다.
- worktree는 그 branch를 체크아웃한 깨끗한 상태여야 한다.
- 본문의 `{{HEAD}}`는 확인한 짧은 head로 바뀐다. 예: 본문에 `새 head {{HEAD}}로 반영했습니다.`라고 쓰면 `새 head 7bc1ffb로 반영했습니다.`로 게시된다.
- PR head는 Gitea가 push 뒤 비동기로 갱신하므로 `ORCH_PR_HEAD_TRIES`(기본 10)회, `ORCH_PR_HEAD_INTERVAL`(기본 3초) 간격으로 확인한다.
- 관측 한계: 댓글은 게시했는데 재리뷰 요청이 실패하면 종료 코드 1과 함께 그 사실만 알린다(댓글은 이미 게시됨). fork PR처럼 원격이 `origin`이 아닌 경우는 다루지 않는다.

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

### `pr_overlap.py` — 열린 PR 겹침 검사(병합 전)

```bash
python tools/orchestrate/pr_overlap.py [--repo-dir <clone>] [--prs 61,64] [--json]
python tools/orchestrate/pr_overlap.py --integrate [--order 61,64,66] [--test-cmd "go test ./..."]
```

- `ORCH_REPO`의 열린 PR을 읽는다. 각 PR의 `refs/pull/<n>/head`와 base 브랜치를 로컬 clone의 전용 이름공간 `refs/orch/pull/<n>`·`refs/orch/base/<branch>`로 fetch한다. 로컬 브랜치, `origin/*`, 원격은 바꾸지 않는다.
- 출력 표는 아래와 같다. `--json`은 같은 내용을 JSON으로 낸다.
  - 열린 PR: 브랜치, fetch한 head, base 대비 바뀐 파일 수, Gitea `mergeable`. API head와 fetch한 head가 다르면 비고에 적는다.
  - 줄 충돌: PR 둘씩 `git merge-tree --write-tree`로 합쳐 충돌 파일을 적는다.
  - 같은 파일: 두 PR이 각자 base와의 merge-base 대비 함께 바꾼 파일이다.
  - 같은 Markdown 절: 바뀐 줄이 속한 제목 경로(`# 문서 > ## 절`)가 같은 쌍이다. 코드 블록 안의 `#` 줄은 제목으로 보지 않는다. 첫 제목 앞은 `(머리말)`이다.
  - 스키마 번호: 바뀐 파일마다 head에서 `ORCH_SCHEMA_REGEX`의 1번 그룹을 읽는다. base 값과 다른 PR만 적는다. 같은 파일에 같은 새 값을 쓰는 다른 PR을 "같은 값을 쓰는 PR"에 적는다.
  - 쌓인 브랜치: 다른 열린 PR 위에 만든 열린 PR과 로컬 브랜치다. "근거" 칸에 아래 셋 중 무엇으로 찾았는지 적는다.
    - `head`: 그 PR의 현재 head를 품었다.
    - `중간 커밋`: 그 PR 자신의 커밋 범위 첫 커밋을 품었다(중간에서 갈라짐). 그 PR 아래에 있는 다른 열린 PR의 커밋은 범위에서 뺀다.
    - `재기반 전 head`: 그 PR 브랜치의 로컬 reflog(`refs/heads/<브랜치>`, `refs/remotes/<원격>/<브랜치>`)에 남은 이전 tip을 품었다. 2026-10-27 VMS-next의 clip·health·enroll이 이 경우였다.
    - 이미 base에 들어간 head는 빼고, 그 PR 자신의 브랜치와 현재 head와 같은 커밋을 가리키는 브랜치도 뺀다.
- `--integrate`: 임시 폴더에 base를 detached worktree로 만든다. `--order` 순서(기본 PR 번호순)로 `merge --no-ff --no-verify`한다. 충돌한 PR은 충돌 파일을 적고 `merge --abort`로 건너뛴다. 그다음 시험 명령을 `--test-timeout`(기본 1800초) 안에서 실행하고 끝 30줄을 남긴다. 성공이든 오류든 임시 worktree를 지우고 `worktree prune`하며, 삭제를 확인한 결과를 표에 적는다.
- 관측 한계:
  - 줄 충돌은 두 PR 사이만 본다. 세 PR 이상이 함께일 때만 생기는 충돌은 `--integrate`로 확인한다.
  - 의미 충돌은 같은 파일·절·스키마 번호라는 단서까지만 준다. 실제 동작 충돌은 `--integrate`의 시험 결과나 사람의 판단이 필요하다. 같은 파일 표에는 변경이 서로 먼 쌍도 나온다.
  - 스키마 번호는 PR이 바꾼 파일 안에서만 찾는다. 정규식이 파일에서 여러 번 맞으면 첫 값만 본다.
  - 로컬 브랜치는 `--repo-dir` 저장소의 `refs/heads`만 본다. 다른 clone의 브랜치는 보이지 않는다.
  - 재기반 전 head는 그 clone의 reflog에 남은 것만 안다. 다른 곳에서 재기반·push했고 이 clone이 그 사이 fetch하지 않았으면 놓친다. reflog가 만료되거나 같은 브랜치 이름을 다른 작업에 다시 썼으면 결과가 틀릴 수 있다.
  - 하나의 원격에 있는 PR만 다룬다. fork에서 온 PR도 Gitea의 `refs/pull/<n>/head`로 fetch되지만 시험하지 않았다.
  - `--integrate`의 병합 커밋은 저장소 객체 DB에 참조 없는 객체로 남았다가 git gc 때 지워진다.

### `after_merge.py` — 병합 직후 영향 알림

```bash
python tools/orchestrate/after_merge.py <병합된 PR 번호> [--json]
python tools/orchestrate/after_merge.py <병합된 PR 번호> --assign [--dry-run]
```

- 병합된 PR이 바꾼 내용을 구한다. 범위는 `merge-base(병합 커밋^1, PR head)`부터 PR head까지다. 내용은 파일, Markdown 절, 스키마 값이다.
- 열린 PR마다 아래 이유를 모아 표로 낸다.
  - 같은 파일·절, 같은 스키마 파일
  - 새 base와의 줄 충돌(merge-tree)
  - Gitea `mergeable=false`
  - 병합된 PR 브랜치 위에 쌓였는지. 병합된 head를 품었지만 병합 커밋은 품지 않은 경우만 센다. 병합 뒤 base로 재기반한 브랜치는 병합 커밋 방식에서도 병합된 head를 품기 때문이다.
- 같은 기준으로 병합된 PR 브랜치 위에 쌓인 로컬 브랜치도 함께 적는다.
- `--assign`은 영향받은 브랜치 가운데 `ORCH_BRANCH_ROOMS`에 있는 것에만 재기반 지시를 보낸다(`ORCH_REBASE_TEMPLATE`, `$merged_pr`·`$branch`·`$reasons`·`$worktree` 등을 치환한다). 보낸 것마다 `방:seq`를 출력한다.
- 보내기 전에 대상 worktree의 점유를 검사한다. worktree는 대응표의 `worktree`, 없으면 `--repo-dir`의 worktree 목록에서 그 브랜치가 체크아웃된 폴더다. 점유 중이거나 worktree를 모르면 보내지 않고 `SKIP`을 출력하며 종료 코드 1로 끝난다. `--dry-run`은 보내지 않고 지시문을 출력한다.
- 관측 한계:
  - 병합 커밋 방식과 squash 방식을 가정한다(병합 커밋의 첫 부모가 병합 전 base다). rebase 병합이면 바뀐 범위가 틀린다.
  - Gitea의 `mergeable`은 서버가 비동기로 다시 계산한다. 병합 직후에는 이전 값일 수 있어서 로컬 merge-tree 결과를 함께 적는다.
  - "mergeable=false로 바뀐" PR은 이전 상태와 비교하지 않는다. 현재 false인 PR을 모두 적는다.

### `worktree_claims.py` — 방-worktree 점유 확인

```bash
python tools/orchestrate/worktree_claims.py check --worktree <폴더> [--room <보낼 방>]
python tools/orchestrate/worktree_claims.py check --branch <브랜치> [--room <보낼 방>]
python tools/orchestrate/worktree_claims.py claim <폴더> --room <방> [--note "PR #64 수정"]
python tools/orchestrate/worktree_claims.py release <폴더> --room <방>
python tools/orchestrate/worktree_claims.py list
```

- 어떤 방이 폴더를 점유한다는 것은, 그 방이 실행 중(`/api/rooms`의 `active`)이고 아래 중 하나로 폴더에 묶였다는 뜻이다.
  - 방 workspace가 그 폴더다.
  - `ORCH_BRANCH_ROOMS`에서 그 방의 브랜치가 그 폴더로 등록됐다.
  - 그 방이 `claim`으로 점유를 등록했다.
- `--room`(작업을 받을 방) 자신은 막지 않는다.
- `check`는 점유 중이면 `BUSY <폴더>: room <id> (<제목>) is running there [이유]`를 출력하고 종료 코드 1, 비어 있으면 `FREE`와 0을 낸다.
- `--branch`는 `ORCH_REPO_DIR` worktree 목록에서 폴더를 찾는다. `ORCH_REPO_DIR`가 없으면 `--room`의 workspace를 쓴다.
- `claim`은 다른 방의 등록이 있거나 다른 실행 중인 방이 그 폴더를 쓰면 거부하고 그 방을 알린다. `release`는 등록한 방만 할 수 있다. `--force`는 둘 다 무시한다.
- 연동:
  - `assign_fix.sh`는 `check --branch <branch> --room <작성 방>`을 쓴다.
  - `queue_review.sh`는 rebase 전에 `check --worktree <W> --room <검수 방>`을 쓴다. 작성 방이 아직 실행 중이면 검수를 보내지 않는다.
  - `after_merge.py --assign`도 같은 검사를 쓴다.
- 관측 한계:
  - 실행 중이 아닌 방의 등록은 막지 않는다. 등록은 상위 폴더 workspace 방의 위치를 알려 주는 용도다. 실행이 끝난 뒤의 dirty 파일은 따로 보지 않는다.
  - 검사와 전송 사이에 다른 방이 시작될 수 있다. 원자적 잠금이 아니다.
  - 경로는 문자열로 비교한다. Windows에서는 `/d/…`와 `D:\…`, 대소문자, 끝 `/`를 같게 본다. 심볼릭 링크와 상대 경로는 풀지 않는다.
  - roundtable 서버에 닿지 못하면 비어 있다고 보지 않고 오류(종료 코드 1, 메시지 `roundtable … failed`)로 끝난다.

## 템플릿과 로컬 프로필

- `templates/writer-rules.md`: `assign_fix.sh`가 붙이는 작성 공통 규칙의 일반형이다.
- `templates/rebase-notice.md`: `after_merge.py --assign`이 보내는 재기반 지시다. `$merged_pr`, `$merged_title`, `$merged_head`, `$base`, `$base_head`, `$branch`, `$pr`, `$worktree`, `$reasons`를 치환한다.
- `templates/review-template.md`: `queue_review.sh`가 보내는 검수 지시의 일반형이다. 기준 문서 자리를 프로젝트에 맞게 채운다.
- `local/`: 프로젝트 전용 설정·지시문·충돌 해결 명령을 둔다. 커밋되지 않으므로 다른 PC에서는 다시 만든다.

## 시험

```bash
python -m unittest discover -s tools/orchestrate -p "test_*.py"
```

- `glint.py` 규칙별 일치·비일치, 줄 번호, 종료 코드를 확인한다.
- `reviewer_check.py`의 인자·기본값 처리를 확인한다.
- 셸 스크립트는 가짜 `tea`·`rt` 실행 파일로 돌린다. 인자 오류에서 아무 호출도 하지 않는지, 필수 설정 누락, 설정 파일 적용, 본문 원문 보존(백틱·`$`·따옴표), lint 실패 시 게시 중단, `rt wait` 인자 조립, CI context 선택을 확인한다.
- `test_overlap.py`는 임시 git 저장소(bare origin + `refs/pull/<n>/head` + 로컬 clone)에서 아래 반례를 확인한다. Gitea는 `orchlib.tea_api` 대체, roundtable은 로컬 가짜 HTTP 서버다.
  - 줄 충돌 쌍과 충돌 없는 쌍
  - 줄 충돌 없이 같은 절을 고친 쌍, 코드 블록 안 `#` 줄
  - 스키마 번호 충돌, 번호를 바꾸지 않은 스키마 파일 수정
  - 쌓인 PR·로컬 브랜치, 중간 커밋에서 갈라진 브랜치, 재기반 전 head 위의 브랜치, 이미 base에 들어간 열린 PR
  - `--integrate`의 건너뜀·시험 결과·임시 worktree 삭제(시험 명령이 예외를 내도 삭제)
  - 로컬 브랜치·원격이 바뀌지 않음
  - 병합 직후 영향 이유와 재기반 지시 전송, 병합 뒤 base로 재기반한 브랜치를 쌓임으로 보지 않음, 점유 중·worktree 미상 거부
  - 점유 판정 세 방식(workspace·대응표·등록), 실행 중이 아닌 방, 등록·해제 거부
  - `queue_review.sh`가 rebase 전에 거부, `assign_fix.sh`가 게시 전에 거부
- 가드를 하나씩 지운 변형 17개를 각각 해당 시험이 잡는 것을 확인했다(2026-10-27).
- `test_push.py`는 임시 bare 저장소에 실제로 push해서 확인한다: push 실패, push는 성공했지만 원격 head가 다름(`pushurl`이 다른 저장소), PR head가 늦음, 재기반 뒤 `--force` 필요, 본문 검사가 push보다 먼저, 모든 확인 뒤 댓글·재리뷰 요청과 `{{HEAD}}` 치환. 가드를 지운 변형 8개를 모두 잡았다.
- 시험하지 않는 것: 실제 Gitea·roundtable 서버 호출, 실제 Gitea 원격으로의 push, `queue_review.sh`의 rebase·충돌 해결, 폴링 반복. Windows에서 전체 시험은 git 호출이 많아 약 2분 걸린다.
- Windows에서 `bash`가 WSL 실행기로 잡히면 Git Bash를 찾아 쓴다. 다른 Bash를 쓰려면 `ORCH_BASH`를 지정한다. Bash가 없으면 셸 시험은 건너뛴다.
