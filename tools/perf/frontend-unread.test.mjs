// Run with: node --test tools/perf/frontend-unread.test.mjs
import assert from "node:assert/strict";
import test from "node:test";
import { createUnreadTracker } from "../../web/js/unread.js";

function memoryStorage(init = {}) {
  const data = { ...init };
  return { data, getItem: (k) => (k in data ? data[k] : null), setItem: (k, v) => { data[k] = String(v); } };
}
const room = (id, messages, active = false) => ({ id, messages, active });

test("a room seen for the first time has no dot", () => {
  const t = createUnreadTracker(memoryStorage());
  assert.deepEqual(t.marks([room("a", 5), room("b", 9)], ""), { a: "", b: "" });
});

test("new messages in a closed room show the dot; opening it clears the dot", () => {
  const t = createUnreadTracker(memoryStorage());
  t.marks([room("a", 5), room("b", 2)], "b");
  assert.equal(t.marks([room("a", 7), room("b", 2)], "b").a, "unread");
  assert.equal(t.marks([room("a", 7), room("b", 2)], "a").a, "", "open room");
  assert.equal(t.marks([room("a", 7), room("b", 2)], "b").a, "", "stays read after leaving it");
  assert.equal(t.marks([room("a", 8), room("b", 2)], "b").a, "unread", "a later message shows it again");
});

test("the open room never has a dot, even while messages arrive", () => {
  const t = createUnreadTracker(memoryStorage());
  t.marks([room("a", 1)], "a");
  assert.equal(t.marks([room("a", 4)], "a").a, "");
  assert.equal(t.marks([room("a", 4)], "").a, "", "what arrived while open was seen");
});

test("a running room shows only the spinner", () => {
  const t = createUnreadTracker(memoryStorage());
  t.marks([room("a", 1)], "");
  assert.equal(t.marks([room("a", 3, true)], "").a, "active");
  assert.equal(t.marks([room("a", 3, true)], "a").a, "active", "open and running");
  assert.equal(t.marks([room("a", 4, false)], "").a, "unread", "the answer is unread once the turn ends");
});

test("seen counts survive a reload and deleted rooms are forgotten", () => {
  const storage = memoryStorage();
  createUnreadTracker(storage).marks([room("a", 3), room("gone", 1)], "");
  const t = createUnreadTracker(storage);
  assert.equal(t.marks([room("a", 4)], "").a, "unread");
  assert.deepEqual(Object.keys(JSON.parse(storage.data["rt.seenMessages"])), ["a"]);
});

test("a shrinking count (room reset) resets the seen count", () => {
  const t = createUnreadTracker(memoryStorage());
  t.marks([room("a", 10)], "");
  assert.equal(t.marks([room("a", 2)], "").a, "");
  assert.equal(t.marks([room("a", 3)], "").a, "unread");
});

test("storage that throws or holds garbage does not break the marks", () => {
  const throwing = { getItem() { throw new Error("denied"); }, setItem() { throw new Error("quota"); } };
  for (const storage of [throwing, null, memoryStorage({ "rt.seenMessages": "{not json" }), memoryStorage({ "rt.seenMessages": '{"a":"x","b":-1}' })]) {
    const t = createUnreadTracker(storage);
    assert.deepEqual(t.marks([room("a", 2), room("b", 2)], ""), { a: "", b: "" });
    assert.equal(t.marks([room("a", 3), room("b", 2)], "").a, "unread", "works in memory for this page");
  }
});
