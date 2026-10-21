// Dependency-free SSE/trace ingestion benchmark. No browser, network or model calls.
// Run the same script against either checkout: node --expose-gc tools/perf/frontend-trace.mjs --root PATH
import assert from "node:assert/strict";
import { writeFile } from "node:fs/promises";
import { cpus, platform, arch } from "node:os";
import { resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { performance } from "node:perf_hooks";

const args = process.argv.slice(2);
const option = (key, fallback) => args.includes(key) ? args[args.indexOf(key) + 1] : fallback;
const root = resolve(option("--root", "."));
const samples = Number(option("--samples", "7"));
const added = Number(option("--events", "2000"));
const initial = 200;
assert(samples >= 1 && added >= 1);

globalThis.localStorage = { getItem: () => null };
globalThis.document = { hidden: true, querySelector: () => null };
globalThis.window = { matchMedia: () => ({ matches: true }) };
globalThis.EventSource = class {
  addEventListener() {}
  close() {}
};

const moduleURL = (name) => pathToFileURL(resolve(root, "web/js", name)).href;
const { state } = await import(moduleURL("state.js"));
const { cacheTrace } = await import(moduleURL("trace.js"));
const { connect } = await import(moduleURL("room.js"));
state.roomId = "synthetic";
connect(state.roomId);

function fixture(index) {
  const parent = "sub-" + Math.floor(index / 8);
  const kind = ["subagent_start", "subagent_meta", "subagent_msg", "tool_call", "tool_result", "thinking", "subagent_msg", "subagent_end"][index % 8];
  return { kind, parent, id: "tool-" + Math.floor(index / 8), role: "assistant", name: "synthetic", text: "검증 가능한 작업 기록 " + index, input: "문맥과 도구 입력 " + index, output: "도구 결과 " + index, meta: { model: "synthetic", exit_code: 0 } };
}
const seed = Array.from({ length: initial }, (_, i) => fixture(i));
const events = Array.from({ length: added }, (_, i) => fixture(initial + i));

function prepare(count) {
  state.traces.clear(); state.traceMeta.clear(); state.expanded.clear(); state.messages.clear(); state.live.clear();
  for (let seat = 0; seat < count; seat++) cacheTrace("m" + seat, seed, initial);
}
function packets(count) {
  const frames = [];
  for (let index = 0; index < added; index++) {
    for (let seat = 0; seat < count; seat++) {
      frames.push({ data: JSON.stringify({ type: "trace", data: { msg_id: "m" + seat, index: initial + index + 1, event: events[index] } }) });
    }
  }
  return frames;
}
function verify(count) {
  for (let seat = 0; seat < count; seat++) {
    const list = state.traces.get("m" + seat);
    assert.equal(list.length, initial + added, "no recorded event may disappear");
    assert.deepEqual(list, [...seed, ...events], "event order/content must be unchanged");
    assert.equal(state.traceMeta.get("m" + seat).total, initial + added);
  }
}
const summary = (values) => {
  const sorted = [...values].sort((a, b) => a - b);
  return { median: sorted[Math.floor(sorted.length / 2)], min: sorted[0], max: sorted.at(-1), samples: values };
};

const results = [];
for (const count of [1, 4, 8]) {
  const frames = packets(count);
  // Warm the real production SSE callback, then discard its data.
  prepare(count);
  for (const frame of frames) state.es.onmessage(frame);
  verify(count);
  const times = [];
  for (let sample = 0; sample < samples; sample++) {
    prepare(count);
    globalThis.gc?.();
    const start = performance.now();
    for (const frame of frames) state.es.onmessage(frame);
    times.push(performance.now() - start);
    verify(count);
  }
  results.push({ cached_traces: count, initial_events_per_trace: initial, appended_events_per_trace: added, incoming_sse_events: frames.length, elapsed_ms: summary(times) });
}
const result = {
  workload: "Production connect().onmessage trace ingestion, hidden DOM; includes JSON.parse, excludes network/DOM rendering and fixture creation. Initial 200 + all 2000 newly received events retained. Eight cached traces matches the existing cache limit.",
  environment: { node: process.version, os: platform(), arch: arch(), cpu: cpus()[0]?.model, gc_between_samples: typeof globalThis.gc === "function" },
  samples, results,
};
const json = JSON.stringify(result, null, 2) + "\n";
if (args.includes("--out")) await writeFile(option("--out"), json);
process.stdout.write(json);
