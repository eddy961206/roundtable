# Codex 입력 전달 경합 관측기

별도 포트·별도 데이터 폴더의 시험용 rt에서만 쓴다. 실제 Codex app-server의 stdio를 중계하며, 지정한 경계에만 지연·실패를 넣는다. 입력과 출력 JSON을 로컬 `wire-<PID>.jsonl`에 저장한다. 운영 서버·다른 좌석의 세션에는 연결하지 않는다.

```powershell
go build -o "$env:TEMP\codex-steer-proxy.exe" ./tools/probe/codex-steer-proxy
$env:ROUNDTABLE_CODEX_PROBE_REAL = 'C:\absolute\path\codex.exe'
$env:ROUNDTABLE_CODEX_PROBE_DIR = 'C:\absolute\path\disposable-probe'
Set-Content -LiteralPath "$env:ROUNDTABLE_CODEX_PROBE_DIR\mode" -Value 'delay-completed'
# 시험용 서버: -codex <위 proxy exe>, -addr <별도 포트>, -data <별도 데이터 폴더>
```

`mode` 파일은 각 app-server 프로세스 시작 때 읽는다.

| 값 | 조작 |
|---|---|
| `pass` | 그대로 중계하고 기록 |
| `delay-completed` | 실제 `turn/completed`를 받은 뒤 rt로 보내기 전 1.5초 지연. `gate.json` 생성 직후 rt에 보내면 실제 Codex는 이미 끝났지만 rt는 아직 실행 중인 경합을 재현 |
| `unsupported-steer` | `turn/steer`에만 `-32601`을 반환. 모델 실행은 실제 CLI가 담당 |
| `hold-steer-reply` | 실제 steer 응답을 받은 뒤 최대 25초 보류. `gate.json` 뒤 시험용 rt PID를 종료하면 receipt 저장 전 서버 중단을 재현 |
| `missing-appserver` | app-server 명령 자체가 없는 CLI 오류 재현. 모델 호출 없음 |

`gate.json`의 PID·시각·요청을 대조한다. 이전 gate를 새 사건으로 읽지 않는다. 사용한 PID만 정리하고 wire에는 프롬프트·응답이 포함되므로 로컬 관측 후 필요한 요약만 보존한다. 이 관측기는 공급사 동작을 검증하는 수단이며 자연 발생 경합 빈도나 모든 CLI 버전의 호환성을 증명하지 않는다.
