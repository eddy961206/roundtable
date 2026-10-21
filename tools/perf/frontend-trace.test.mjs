// Run with: node --test tools/perf/frontend-trace.test.mjs
import assert from "node:assert/strict";
import test from "node:test";

globalThis.localStorage = { getItem: () => null };
globalThis.document = { hidden: true, querySelector: () => null };
globalThis.window = { matchMedia: () => ({ matches: true }) };
globalThis.EventSource = class { addEventListener() {} close() {} };
const { state } = await import("../../web/js/state.js");
const { cacheTrace } = await import("../../web/js/trace.js");
const { connect } = await import("../../web/js/room.js");

function reset() {
  state.traces.clear(); state.traceMeta.clear(); state.expanded.clear(); state.messages.clear();
  state.roomId = "test";
  connect(state.roomId);
}
function send(id, index, event) {
  state.es.onmessage({ data: JSON.stringify({ type: "trace", data: { msg_id: id, index, event } }) });
}

test("trace SSE retains ordered full records after the initial tail and ignores duplicates", () => {
  reset();
  const seed = Array.from({ length: 200 }, (_, i) => ({ kind: "thinking", text: "initial " + i }));
  cacheTrace("m", seed, 500);
  const expected = [...seed];
  for (let i = 501; i <= 2500; i++) {
    const event = { kind: "subagent_msg", parent: "child", role: "assistant", text: "message " + i, meta: { model: "synthetic" } };
    expected.push(event);
    send("m", i, event);
  }
  send("m", 2500, { kind: "error", text: "duplicate" });
  send("m", 100, { kind: "error", text: "stale" });
  send("m", undefined, { kind: "error", text: "missing index" });
  assert.deepEqual(state.traces.get("m"), expected);
  assert.equal(state.traceMeta.get("m").total, 2500);
  assert.equal(state.traceMeta.get("m").total - state.traces.get("m").length, 300);
});

test("full history remains complete when new tool and subagent records arrive", () => {
  reset();
  const full = Array.from({ length: 1000 }, (_, i) => ({ kind: "status", text: String(i) }));
  cacheTrace("m", full, full.length);
  const streamed = [
    { kind: "subagent_start", parent: "sub", name: "review", input: "task" },
    { kind: "subagent_meta", parent: "sub", meta: { model: "synthetic", effort: "high" } },
    { kind: "subagent_msg", parent: "sub", role: "tool_call", id: "t", name: "Read", input: "file" },
    { kind: "subagent_msg", parent: "sub", role: "tool_result", id: "t", output: "result" },
    { kind: "subagent_end", parent: "sub", output: "report" },
  ];
  streamed.forEach((event, i) => send("m", full.length + i + 1, event));
  assert.deepEqual(state.traces.get("m"), [...full, ...streamed]);
  assert.equal(state.traceMeta.get("m").total, state.traces.get("m").length);
});

test("display bounds and the full-history retry error survive subsequent events", () => {
  reset();
  cacheTrace("m", [], 0);
  const metadata = state.traceMeta.get("m");
  metadata.fullError = "stored history temporarily unavailable";
  const long = { kind: "tool_result", text: "t".repeat(9000), input: "i".repeat(9001), output: "o".repeat(9002), meta: { exit_code: 1 } };
  send("m", 1, long);
  send("m", 2, { kind: "status", text: "next" });
  const bounded = state.traces.get("m")[0];
  for (const field of ["text", "input", "output"]) assert.equal(bounded[field].length, 8000);
  assert.deepEqual(bounded.meta, { exit_code: 1 });
  assert.equal(bounded._displayTruncated, true);
  assert.equal(state.traceMeta.get("m").shortened, true);
  assert.equal(state.traceMeta.get("m").fullError, metadata.fullError);
  assert.equal(long.output.length, 9002);
});

test("streamed trace recency preserves the existing eight-trace eviction policy", () => {
  reset();
  for (let i = 0; i < 8; i++) { cacheTrace("m" + i, [], 0); state.expanded.add("m" + i); }
  send("m0", 1, { kind: "status", text: "active" });
  cacheTrace("m8", [], 0);
  assert.equal(state.traces.size, 8);
  assert(state.traces.has("m0"));
  assert(!state.traces.has("m1"));
  assert(!state.traceMeta.has("m1"));
  assert(!state.expanded.has("m1"));
  send("closed", 100, { kind: "status", text: "not cached" });
  assert.equal(state.traces.size, 8);
  assert(!state.traceMeta.has("closed"));
});
