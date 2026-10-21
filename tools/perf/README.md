# 성능 재현 도구

실제 모델 대신 결정적인 이벤트·파일·CLI를 사용한다. 제품 실행 의존성이나 빌드 시스템을 추가하지 않는다. Python HTTP 도구는 매번 전용 임시 데이터 폴더를 만들고 삭제한다. 브라우저 live smoke에는 별도로 만든 임시 서버를 지정한다. 기존 `~/.roundtable`을 사용하지 않는다.

- 기준선: `8c9eda11c10ad16f6d4dbcb196bc1a01319cf5f7`
- 측정한 구현: `f45c9b1ee899fdfcecdb77c2d85bb4080854494e`
- [측정 결과·계약·검증 한계](../../docs/성능최적화_측정_20261021.md)
- [원자료](results/): 워밍업을 제외한 반복 실행 값. 시간은 중앙값과 최솟값/최댓값으로 비교한다.

## 도구 구성

| 도구 | 측정·검증 범위 |
|---|---|
| `internal/agent/jsonl_bench_test.go` | agy/Codex transcript의 1·4·16·32개 파일 폴링, 4/64/512KiB 행, 자원 반복 검사 |
| `internal/room/performance_test.go` | JSONL 순회, 전체/tail 기록 조회, SSE 1·4·16·32 구독자, 실제 `runTurn` 1·4·16·32개 동시 실행 |
| `fakecli/main.go` | Claude stream-json을 내보내는 로컬 fixture. 세션·도구·사용량·취소를 재현하며 모델을 호출하지 않음 |
| `stream_load.py` | 실제 HTTP → CLI → JSONL → SSE 경로, 4개 SSE 소비자, 전후 본문·기록 순서 대조 |
| `lifecycle_smoke.py` | CLI `rt post/read`, 세션 재개, 참가자별 중단, 독립 실행, 사용량, 정상 종료 후 재시작 |
| `frontend-trace.mjs` | 실제 SSE 처리 함수에 200+2,000개 이벤트/기록을 전달, 전체 내용·순서 대조 |
| `frontend-trace.test.mjs` | 전체/중첩 기록, 중복 이벤트, 표시 길이, 오류·LRU 보존 회귀 검사 |
| `frontend-browser.mjs` | 실제 HTML/CSS/marked/JS에서 200개 메시지와 동시 렌더, CDP CPU·layout 지표·UI 상태 검사 |
| `frontend-live-smoke.mjs` | 실제 앱 부팅·HTTP·합성 CLI로 방 생성, 1,000개 기록, 입력·키보드·방 전환·중단·테마 검사 |
| `summarize.py` | Go 원자료의 중앙값·범위·증감률 JSON. 서로 다른 benchmark 이름은 비교하지 않음 |

## 기준선 준비와 Go 측정

아래는 저장소 루트에서 실행하는 Linux/Bash 예시다. 측정 시 다른 성능 실험을 동시에 실행하지 않는다. 이번 환경은 Go 1.27.3, `GOMAXPROCS=4`였다. baseline에는 측정 코드 두 개만 복사하고 생산 코드는 바꾸지 않는다.

```bash
git worktree add --detach ../roundtable-perf-base 8c9eda11c10ad16f6d4dbcb196bc1a01319cf5f7
cp internal/agent/jsonl_bench_test.go ../roundtable-perf-base/internal/agent/
cp internal/room/performance_test.go ../roundtable-perf-base/internal/room/
perf_root=$PWD
perf_before=$(cd ../roundtable-perf-base && pwd)
perf_out=$(mktemp -d)
export GOMAXPROCS=4

for scope in agent room; do
  if [ "$scope" = agent ]; then
    regex='Benchmark(AgyTranscriptIdle|CodexRolloutIdle|TranscriptActive)$'
  else
    regex='Benchmark(JSONLScan|ReadTrace|TraceTail|BroadcastFanout|ConcurrentStreaming)$'
  fi
  for version in before after; do
    checkout=$perf_root
    if [ "$version" = before ]; then checkout=$perf_before; fi
    (cd "$checkout" && go test "./internal/$scope" -run '^$' -bench "$regex" -benchmem -benchtime=100ms -count=1) > "$perf_out/$scope-$version-warmup.txt"
    (cd "$checkout" && go test "./internal/$scope" -run '^$' -bench "$regex" -benchmem -benchtime=200ms -count=5) > "$perf_out/$scope-$version.txt"
  done
  python tools/perf/summarize.py "$perf_out/$scope-before.txt" "$perf_out/$scope-after.txt" --output "$perf_out/$scope-summary.json"
done

for version in before after; do
  checkout=$perf_root
  if [ "$version" = before ]; then checkout=$perf_before; fi
  (cd "$checkout" && go test ./internal/agent -run '^$' -bench '^BenchmarkTranscriptResourceSoak$' -benchmem -benchtime=1000x -count=3) > "$perf_out/agent-resources-$version.txt"
done
python tools/perf/summarize.py "$perf_out/agent-resources-before.txt" "$perf_out/agent-resources-after.txt" --output "$perf_out/agent-resources-summary.json"
```

자원 반복 검사 한 번은 agy 32개 파일과 Codex 32개 파일을 1,000주기 동안 확인한다(64,000번의 open/read/close). `fd-delta`는 Linux `/proc`가 있을 때만 출력한다. `live-heap-B`는 GC 이후의 Go heap이며 RSS와 다르다.

CPU·할당 프로파일도 같은 두 checkout에서 수집할 수 있다. 아래 명령을 각각 실행하고 `name`과 출력 경로를 구분한다. 이번 수집은 30회 반복이며 프로파일에는 fixture 준비 비용도 포함된다.

```bash
name=before  # 개선본에서는 after
# 해당 checkout으로 이동한 뒤 실행
go test ./internal/room -run '^$' -bench '^BenchmarkConcurrentStreaming/participants=32$' -benchtime=30x -count=1 \
  -cpuprofile "$perf_out/$name-cpu.pprof" -memprofile "$perf_out/$name-mem.pprof" -o "$perf_out/$name-room.test"
go tool pprof -top -nodecount=20 "$perf_out/$name-room.test" "$perf_out/$name-cpu.pprof"
go tool pprof -alloc_space -top -nodecount=20 "$perf_out/$name-room.test" "$perf_out/$name-mem.pprof"
```

## 실제 서버·합성 CLI

저장소 루트에서 실행한다. `stream_load.py`의 기본값은 참가자 1·4·16·32명, SSE 구독자 4명, 참가자마다 128개 text delta·도구 이벤트다. 각 규모를 한 번 워밍업하고 5회 기록한다. `--delay-us`로 CLI 조각 사이의 지연을 줄 수 있다. Linux CPU/RSS는 Roundtable 서버 프로세스만 관찰하고 fixture·Python 소비자의 비용은 제외한다.

```bash
go build -o "$perf_out/fakecli" ./tools/perf/fakecli
(cd "$perf_before" && go build -o "$perf_out/roundtable-before" ./cmd/roundtable)
go build -o "$perf_out/roundtable-after" ./cmd/roundtable
for version in before after; do
  python tools/perf/stream_load.py --binary "$perf_out/roundtable-$version" --fixture "$perf_out/fakecli" --output "$perf_out/stream-$version.json"
  python tools/perf/lifecycle_smoke.py --binary "$perf_out/roundtable-$version" --fixture "$perf_out/fakecli" --output "$perf_out/lifecycle-$version.json"
done
```

스크립트는 전달받은 fixture 외 공급사를 존재하지 않는 경로로 설정한다. 완료 메시지, 모든 delta, 연속 trace index, 디스크 전체 trace를 대조한다. 최종 메시지는 기존 서버 규칙에 따라 가장자리 공백을 제거하며, SSE 원문은 그 공백까지 대조한다. p50/p95는 한 번의 방 실행에서 참가자별 완료 시각의 분위수이고, 표의 값은 5회 실행의 중앙값이다. 32명 × 128개 조각을 지연 없이 생성하는 burst 실험으로, 실제 모델 응답 시간을 대표하지 않는다.

## 프런트엔드

Node v24.19.0을 사용했다. trace 수신 벤치는 별도 패키지 없이 실행한다.

```bash
node tools/perf/frontend-trace.test.mjs
node --expose-gc tools/perf/frontend-trace.mjs --root "$perf_before" --samples 7 --out "$perf_out/frontend-trace-before.json"
node --expose-gc tools/perf/frontend-trace.mjs --root . --samples 7 --out "$perf_out/frontend-trace-after.json"
```

브라우저 검증에는 **개발용** Playwright와 Chromium 경로가 필요하다. 제품에는 설치하지 않는다. 이번 환경은 Playwright 1.64.1, `@sparticuz/chromium` 153.0.0의 Chromium 153.0.8010.0이었다. 기존 QA 설치를 사용했으며 저장소에 `package.json`/lockfile을 추가하지 않았다. 다른 머신에서는 별도 임시 폴더에 Playwright를 설치하고 실행 가능한 Chromium 경로를 지정하면 된다.

```bash
# 이번 측정에 사용한 환경의 경로. 로컬 QA 설치에 맞게 바꾼다.
perf_modules=/tmp/forest-qa-runtime/node_modules
perf_browser=/tmp/chromium
for version in before after; do
  checkout=$perf_root
  if [ "$version" = before ]; then checkout=$perf_before; fi
  node tools/perf/frontend-browser.mjs --root "$checkout" --modules "$perf_modules" --browser "$perf_browser" --samples 5 --ticks 8 --out "$perf_out/frontend-browser-$version.json"
done
```

브라우저는 1440×1000 headless 모드와 `--no-sandbox --disable-dev-shm-usage`로 실행한다. 실제 제품의 125ms throttle을 사용하고 규모별 2회 렌더 워밍업 뒤 8회 렌더 × 5표본을 측정한다. 본문·마크다운·DOM 개수는 동일하게 유지한다. `main.js` 부팅을 fixture로 대체하므로 HTTP/CLI·백그라운드 polling을 제외한 렌더러 비용이다. 브라우저 벤치의 2,000개 history count는 합성 메타데이터이며 DOM은 200개다. 실제 앱 부팅과 저장된 긴 대화는 아래 live smoke의 1,000개 메시지로 별도 확인한다.

UI live smoke에는 **새 임시 데이터 폴더로 실행한 합성 CLI 전용 서버**를 지정한다. 기존 서버를 대상으로 실행하지 않는다. 테스트 시작 시 공급사 버전에 fixture 표시가 없으면 프롬프트를 보내지 않고 실패한다.

```bash
"$perf_out/roundtable-after" -addr 127.0.0.1:18788 -data "$perf_out/ui-data" \
  -claude "$perf_out/fakecli" -codex "$perf_out/missing" -agy "$perf_out/missing" -opencode "$perf_out/missing" > "$perf_out/ui-server.log" 2>&1 &
perf_server_pid=$!
python - <<'PY'
import time
import urllib.request
for attempt in range(200):
    try:
        with urllib.request.urlopen('http://127.0.0.1:18788/api/config', timeout=1) as response:
            response.read()
        break
    except OSError:
        time.sleep(.025)
else:
    raise SystemExit('fixture server startup timed out')
PY
node tools/perf/frontend-live-smoke.mjs --url http://127.0.0.1:18788 --modules "$perf_modules" --browser "$perf_browser" --out "$perf_out/frontend-live-smoke.json" --screenshots "$perf_out/screenshots"
kill "$perf_server_pid"
wait "$perf_server_pid"
```

## 회귀 검사

```bash
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
python tools/modcheck.py web/js
node tools/perf/frontend-trace.test.mjs
go test ./internal/room -run '^$' -fuzz '^FuzzJSONLineBoundaries$' -fuzztime=5s -parallel=2
```

기준선의 `go test ./...`는 Linux에서 기존 `TestFileURLToPath`가 실패했다. Windows 경로 판정을 Linux에도 적용한 테스트 문제였으며, 이번 PR에서 OS별 정확한 전체 경로 검증과 추가 사례로 고쳤다. 벤치마크는 `-run '^$'`로 일반 테스트와 분리한다. Windows는 실행 파일과 모든 테스트 패키지를 크로스 컴파일했지만 직접 실행하지 않았다.
