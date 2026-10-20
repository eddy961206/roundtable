# Codex 쓰기 모드 Go 캐시

Roundtable은 Codex의 쓰기 모드 실행 전에 `<server scratch>/codex/go-build`를 생성한다. 같은 서버의 새 세션과 재개 세션이 이 경로를 재사용한다.

- Codex 프로세스 환경변수 `GOCACHE`를 해당 절대 경로로 설정한다.
- `shell_environment_policy.set.GOCACHE`도 전달해 Codex의 환경변수 필터와 관계없이 셸 명령에서 같은 경로를 사용한다.
- 기존 `-codex-windows-sandbox` 옵션으로 샌드박스를 명시했을 때만 `sandbox_workspace_write.writable_roots`에 해당 캐시 경로를 전달한다. 기본 실행에는 샌드박스 쓰기 경로 설정을 넣지 않는다.
- 2026-10-20 사용자 결정으로 기본은 `danger-full-access`, `approval_policy="never"`다. 샌드박스를 명시하면 쓰기 모드는 `workspace-write`로 실행한다. 전역 Codex 설정과 `go env -w`는 변경하지 않는다.
- 읽기 모드와 채팅 모드에는 캐시 설정을 넣지 않고 디렉터리도 만들지 않는다. 캐시 생성 실패는 실행 전 오류로 반환한다.

`exec resume`에는 `--add-dir` 옵션이 없으므로 새 실행과 재개 실행 모두 설정 인자로 전달한다. 설정 의미는 [공식 Codex 설정 문서](https://learn.chatgpt.com/docs/config-file/config-reference)를 참고한다.

## 이전 샌드박스 실행 확인

2026-10-19 21:20~21:22 KST, Windows / Codex CLI 0.162.1 / Go 1.27.2에서 변경한 `agent.Codex.Run`으로 확인했다. 모델은 `gpt-6-astra`, 추론은 `low`, Windows sandbox는 `unelevated`다.

Go 저장소는 이 `feat/trace-tail` worktree를 사용했다. 캐시를 작업 폴더 밖에 두어 workspace 쓰기 권한만으로 통과하는 조건을 배제했다.

```text
<서버 scratch 폴더>\codex\go-build
```

Codex에 환경변수와 소스를 변경하지 않고 `go env GOCACHE`, `go build ./...` 두 명령만 실행하도록 요청했다. 새 세션과 같은 세션을 재개한 실행 모두 위 캐시 경로를 반환했고 빌드 명령의 실제 종료 코드는 0이었다. CLI 기록에서도 모델, 추론 수준, `workspace-write`, 캐시 `writable_roots`를 대조했다.

`internal/agent/codex_cache_test.go`에서 새 세션 및 재개 인자, read/chat의 설정과 디렉터리 미변경, 캐시 생성 실패, 상대 scratch 경로 거부를 확인한다.

실제 실행 검증은 Windows unelevated에 한정한다. elevated와 다른 운영체제에서의 실제 Codex 실행은 확인하지 않았다. Go 모듈 다운로드 캐시나 네트워크 권한은 이 변경의 대상이 아니다. 실행 중인 8787 서버에는 배포하거나 재기동하지 않았다.
