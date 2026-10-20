# Codex 좌석의 Gitea 조회 경로와 Windows launcher 진단

## 조회 계약

`-gitea-repos`, `-gitea-login`, `-tea`를 설정하면 read·write Codex 실행마다 읽기 전용 `gitea` MCP를 붙인다. 새 세션과 `exec resume`에 같은 설정을 적용한다. chat에는 붙이지 않는다.

- `enabled=true`, `required=true`: Gitea MCP가 초기화되지 않으면 Codex 시작을 실패시킨다.
- 시작 제한 10초, Codex 도구 제한 25초.
- MCP 내부 조회 제한 20초. PR 본문·파일·리뷰와 댓글의 모든 페이지가 같은 제한 시간을 공유한다.
- 제한 초과 시 launcher와 자식 프로세스를 종료한다. Windows의 프로세스 트리 종료 대기와 출력 파이프 정리에도 각각 1초 상한을 둔다.
- 인증 도구 stderr는 응답에 포함하지 않는다. 실패 시 종료 코드와 로그인·launcher·권한 확인 안내를 반환한다.
- Gitea 도구 이름과 실패 보고 안내는 세션 첫 턴에만 두 문장으로 전달한다. 매 턴 반복해야 한다는 비교 실측 근거는 없다. 재개 시 MCP 설정은 계속 전달하지만 안내문을 덧붙이지 않는다.
- 2026-10-20 후속 사용자 결정에 따라 기본 실행은 OS 샌드박스를 사용하지 않는다. 아래의 폴더 거부 재현은 샌드박스가 켜진 이전 실행에 대한 기록이다. 현재 공급사별 read 제한과 재활성화 옵션은 README의 실행 권한 절을 따른다.

Codex의 `required`, `startup_timeout_sec`, `tool_timeout_sec` 의미는 [공식 MCP 설정 문서](https://developers.openai.com/codex/mcp/)를 기준으로 확인했다. 실제 실행은 로컬 Codex 0.162.1에서 확인했다. 다른 버전의 호환성은 미검증이다.

## 2026-10-20 관측

모든 시간은 KST다. 기존 8787 서버(PID 22540)의 명령줄은 다음과 같았다. 이 서버와 저장소 밖 launcher·Codex 설정은 변경하지 않았다.

```powershell
roundtable.exe -addr 127.0.0.1:8787 `
  -gitea-repos Owner/RepoA,Owner/RepoB `
  -gitea-login <login> `
  -tea $env:LOCALAPPDATA/Programs/GiteaTea/tea.exe `
  -codex-windows-sandbox unelevated
```

### 멈춤 경로: 폴더 쓰기 거부와 Python tempfile 반복

원래 방 `634ee08d3a35`의 `events.jsonl`에서 `item_10`은 `tea api --login <login> repos/Owner/RepoA/pulls/11`이다. 호출과 결과 사이 간격은 **104.108초**다. 결과는 `exit_code:1`, `status:failed`이고 오류 본문이 없다. 해당 방에는 Gitea MCP 호출 기록이 없다.

현재 Codex unelevated 샌드박스에서도 같은 launcher가 15초 동안 끝나지 않았다. 프로세스를 한정해 종료했다. 비밀값이나 로컬 변수를 출력하지 않고 Python 호출 스택만 수집한 별도 진단에서는, 8초 시점에 다음 경로가 확인됐다.

```text
tea-vault.py:40 main
  tempfile.TemporaryDirectory(..., dir=<LOCALAPPDATA>/GiteaTea/sessions)
tempfile.py:882 __init__
tempfile.py:384 mkdtemp
  os.mkdir(file, 0o700)
```

같은 `sessions` 폴더는 `isdir=True`, `os.access(W_OK)=True`였지만, 무작위 이름의 빈 하위 폴더를 **한 번만** 생성해 보면 `PermissionError`, `winerror=5`, `errno=13`이 반환됐다. 폴더는 생성되지 않았다.

설치된 Python 3.12의 `tempfile.py`는 Windows에서 `PermissionError`를 받았을 때 부모가 존재하고 `os.access(W_OK)`가 참이면 이름 충돌로 간주해 다음 무작위 이름으로 재시도한다. 샌드박스 거부를 실제 쓰기 가능 여부 검사로 구분하지 못해 긴 반복이 발생한다.

이 스택은 launcher의 `access_token()` 호출 이후, `whoami`·ACL 설정과 native tea 로그인·API 호출 이전이다. 따라서 **이 재현의 직접 대기 원인은 임시 폴더 생성 반복**이다. Credential Manager나 mutex에서 기다리고 있다는 근거는 없었다. 샌드박스 네트워크 차단은 이번 대기 원인으로 관측되지 않았다. native tea의 PR 조회에는 도달하지 않았고, 앞선 `access_token()` 내부의 OAuth 갱신 통신 여부는 수집하지 않았다. 삭제된 원래 detached checkout에서 동일 스택을 수집한 것은 아니다.

### 도구 배선과 발견

기존 코드에도 `RunOptions.MCP`를 Codex 인자로 전달하는 경로가 있었다. 수정 전 바이너리를 8791에 띄워 만든 read 방 `3c3fa2cfe5eb`에서, 같은 `gpt-6-astra/high`가 Gitea 도구 5개를 발견하고 `gitea/gitea_pull`로 PR #11을 **2.855초**에 조회했다. 따라서 배선 자체의 누락은 재현되지 않았다.

원래 세션 `01a1572d-85b5-75e1-a1e6-162031f3fb3b`는 도구 이름·설명에서 `/gitea|tea|search/i`를 검색한 뒤 `.slice(0,70)`만 출력했다. 결과 70개에 Gitea가 없었다. 현재 같은 검색은 401개를 반환하며 Gitea 도구는 0 기준 395~399번째다. 광범위한 검색 후 앞부분만 자르는 방식은 Gitea 도구를 놓친다. **원래 실행의 전체 도구 목록은 저장되어 있지 않아 그때도 Gitea가 노출됐다고 단정할 수는 없다.**

## 저장소 밖 변경 제안 — 이번 작업에서는 수정하지 않음

대상: `%LOCALAPPDATA%/Programs/GiteaTea/tea-vault.py`와 그 native launcher.

1. 기존 ACL 제한 `sessions` 경로를 유지하고, `TemporaryDirectory`가 `TMP_MAX`까지 권한 거부를 반복하는 경로를 제거한다. 무작위 이름에 직접 `os.mkdir`을 한 번 수행하고, 실제 이름 충돌만 제한된 횟수로 재시도한다. `winerror=5`는 즉시 실패시킨다. 오류에는 단계·경로·오류 코드만 남긴다. 토큰과 설정 내용은 출력하지 않는다.
2. 최초 인증 조회부터 native API 종료까지 전체 실행 제한 시간을 둔다. `whoami`, `icacls`, 최종 `subprocess.call`에도 남은 시간을 전달한다. 현재 최종 API 실행과 앞의 보조 명령에는 명시적 시간 제한이 없다.
3. 현재 인증 provider의 mutex 대기 제한은 45초, OAuth HTTP 제한은 30초이고 launcher 로그인 제한은 40초다. 이들은 이번 폴더 반복과 다른 경로다. 전체 제한 시간과 일관되게 맞추고, 실행 취소 시 해당 실행의 자식만 종료한다. 공유 mutex를 강제로 풀거나 인증 경로·저장 위치를 바꾸지 않는다.

## 검증

실제 CLI는 `127.0.0.1:8791`, 데이터는 `~/.roundtable-test2`를 사용했다. 운영 8787을 재시작하지 않았다. fixture 검증에서는 이 별도 서버의 `-tea`만 검사 실행 파일로 바꿨다. 실제 토큰이나 Gitea 쓰기를 사용하지 않는다.

- 실제 tea: read 방 `ce37bc3a580f`, write 방 `05b82e92a14e`. 각각 새 세션과 동일 세션 재개에서 PR #11 조회 성공. 네 번 모두 `gitea/gitea_pull`을 사용했으며 셸 호출은 없었다. 당시 PR 상태 `open`, head `977dc59`.
- 시간 초과 주입: read 방 `07d9d94bec78`의 두 번째 턴. 검사 실행 파일을 `-tea`로 지정하고 `-gitea-login roundtable-timeout-fixture`로 30초 대기를 주입했다. 도구 호출부터 **20.365초** 뒤 `Gitea 조회 제한 20s 초과` 오류가 반환됐다. 좌석은 재시도 없이 미확인 범위를 보고하고 idle로 돌아왔다. launcher·자식 검사 프로세스가 남지 않은 것도 확인했다. 첫 번째 턴은 환경 변수에 의존한 fixture가 MCP에 전달되지 않아 JSON 해석 오류를 반환했으며, 시간 초과 검증에 포함하지 않았다.
- 프로세스 검사: 성공 응답, 비밀 문자열을 포함한 stderr의 비노출, 실행 취소 시 launcher와 자식의 종료. 자식이 남기는 주기 기록이 취소 뒤 증가하지 않는지도 검사한다.
- 도구 검사: PR과 파일 조회가 같은 deadline을 사용하고, 시간 초과를 `isError:true`로 반환한 뒤 다음 `ping`에 응답한다.
- Codex 인자 검사: read·write·chat과 새 세션·재개 조합에서 Gitea 설정과 안내의 적용 범위를 확인한다.
- 기존 `go test ./...`와 새 검사 모두 통과했다. 실제 Gitea 쓰기, 고의적인 인증 정보 삭제, mutex 점유, 실제 네트워크 장애 주입은 수행하지 않았다.
- Windows 실행 파일 빌드와 Linux/amd64 교차 컴파일에 성공했다. Linux에서 실행한 것은 아니다. 8791 검사 서버는 종료했고 방 기록은 별도 데이터 폴더에 보존했다.

검사 명령:

```powershell
$env:GOCACHE = "$env:USERPROFILE\.roundtable\scratch\codex\go-build"
go test ./...
go build -o .cache/gitea-fixed/roundtable.exe ./cmd/roundtable
```

진단용 바이너리·로그는 무시되는 `.cache/gitea-fixed`에 있고 방 기록은 별도 데이터 폴더에 있다. 이 문서의 PR 상태와 프로세스 번호는 관측 당시 값이다.

## 후속 결정: 기본 OS 샌드박스 해제와 첫 턴 안내

2026-10-20 후속 사용자 결정으로 기본 Codex 실행은 `sandbox_mode="danger-full-access"`, `approval_policy="never"`를 사용한다. `windows.sandbox`는 기본 실행에 전달하지 않는다. 기존 `-codex-windows-sandbox` 옵션을 명시하면 read·chat은 `read-only`, write는 `workspace-write`가 된다. 원본 세션 열기도 같은 설정 함수를 사용한다.

Claude는 기존 read 도구 목록과 write의 `acceptEdits`·셸 허용을 유지하고, 실행별 설정으로 OS 샌드박스를 끈다. Antigravity는 `--sandbox`만 제거하고 plan·accept-edits 모드를 유지한다. OS 샌드박스 없는 Codex read는 파일 쓰기를 기술적으로 막지 못하며, README에 이 한계를 명시했다.

검사는 동일한 8791 별도 서버와 `.roundtable-test2`에서 진행했다. 이번 서버에는 `-codex-windows-sandbox`를 지정하지 않았다.

| 공급사·모드 | 방 | 관측 결과 |
|---|---|---|
| Codex read | `801884423d81` | 셸 tea 조회 성공. PR #11, open. 실제 rollout 권한 `danger-full-access`, approval `never`. |
| Codex write | `0106ca47cb57` | 셸 tea 조회 성공, 종료 코드 0. PR #11, open. 실제 rollout 권한 `danger-full-access`, approval `never`. |
| Claude read | `cf11b390ffbc` | 셸 도구가 없어 실행하지 않음. 다른 도구로 우회하지 않음. |
| Claude write | `5c08149c2dfd` | PowerShell에서 tea 조회 성공, 종료 코드 0. PR #11, open. |
| Antigravity read | `a0ce4c8e415c` | plan 모드에서도 해당 셸 조회를 허용했고 성공. PR #11, open. |
| Antigravity write | `d8ce5bbff496` | 셸 tea 조회 성공, 종료 코드 0. PR #11, open. |

각 셸 실행은 `tea api --login <login> repos/Owner/RepoA/pulls/11`을 한 번 호출했다. 진단용 코드로 인증 토큰을 직접 읽거나 출력하지 않았다. 여기서 성공은 해당 GET 조회의 결과이며, 다른 명령의 권한이나 실제 프로젝트 쓰기 차단을 증명하지 않는다.

Gitea 안내는 두 문장으로 줄여 **451자·UTF-8 783바이트 → 128자·218바이트**가 됐다. 문자 수는 개행을 포함하며 토큰 수 실측은 아니다. 세션 재개에는 안내문을 추가하지 않는다. 두 Codex 방의 재개 턴에서 안내문 추가 0회와 MCP PR 조회 성공을 각각 확인했다. 이전 기록은 매 턴 안내가 붙은 조건의 성공이므로 반복이 필요하다는 비교 근거가 아니다. 새 검사는 첫 턴만 안내하는 경로가 동작함을 확인하며 장기 압축 이후의 유지까지 증명하지 않는다.

여섯 좌석의 원본 세션 명령 생성 API도 조회했다. Codex는 sandbox 설정이 기본 `danger-full-access`, Claude는 sandbox false와 read 도구 목록·write 셸 허용, Antigravity는 `--sandbox` 없이 기존 모드를 전달했다. 원본 콘솔을 실제로 열지는 않았다. 기본·옵트인·chat·read·write의 인자 일관성은 `TestNativeAndWorkerPermissionPolicies`와 기존 인자 검사로 확인한다. 진단 산출물은 `.cache/unsandbox`에 보관한다.

후속 변경에서도 `go test ./...`, Windows 빌드, Linux/amd64 교차 컴파일이 통과했다. 8791 검사 서버는 종료했다. chat의 실제 CLI 실행, 샌드박스 옵트인의 실제 실행, 원본 콘솔 열기, Linux 실행, 장기 문맥 압축 후 안내 유지 여부는 이번 후속 검사에서 확인하지 않았다. 커밋·push·8787 재시작은 수행하지 않았다.
