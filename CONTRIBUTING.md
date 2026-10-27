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

2. 시험을 돌린다. `go test ./...`, 그리고 바꾼 영역의 추가 검사(예: `web/js`면 `python tools/modcheck.py web/js`, `tools/orchestrate`면 그 폴더의 unittest)다.
3. 원격 main에 올린다. fast-forward가 아니면 원격이 거절한다. 거절되면 1단계부터 다시 한다.

   ```bash
   git push origin HEAD:main
   ```

4. 저장소 본체 checkout을 쓰는 세션은 `git merge --ff-only origin/main`으로 따라간다.
5. 다 쓴 worktree와 브랜치는 지운다.

## 실행 중인 서버 교체

여러 세션이 같은 서버(예: `127.0.0.1:18787`)를 함께 쓴다. 프로세스를 직접 종료하거나 실행 중인 exe를 덮어쓰지 않는다. 절차는 README의 [새 빌드로 교체하기](README.md#새-빌드로-교체하기)를 따른다.

1. 서버 바이너리는 `<새 빌드>.exe install <서버 exe 경로> [<다른 사본 경로>...]`로만 교체한다.
2. 서버 재기동은 `<서버 exe 경로> restart --url http://127.0.0.1:<포트> [--timeout 30m]`만 쓴다. 이 명령은 모든 방에 실행·대기 중인 턴이 없을 때만 이전 서버를 내린다. 시간 안에 유휴가 되지 않으면 이전 서버를 그대로 두고 실패한다.
