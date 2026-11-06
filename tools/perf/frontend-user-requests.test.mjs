import assert from "node:assert/strict";
import test from "node:test";

globalThis.localStorage = { getItem: () => null };
const { mergeUserRequests } = await import("../../web/js/user-requests.js");

test("a late snapshot cannot reopen a user request resolved by SSE", () => {
  const pending = { id: "request-1", seq: 5, status: "pending", text: "확인" };
  const answered = { ...pending, status: "answered", answer: "완료", resolved_seq: 9 };
  const acknowledged = { id: "request-2", seq: 6, status: "acknowledged", resolved_seq: 10 };
  const initial = new Map([[pending.id, pending]]);
  const afterSSE = mergeUserRequests(initial, [answered, acknowledged]);
  const late = mergeUserRequests(afterSSE, [pending, { ...acknowledged, status: "pending" }]);
  assert.equal(late.get(pending.id), answered);
  assert.equal(late.get(acknowledged.id), acknowledged);
  assert.equal(initial.get(pending.id), pending, "snapshot merge must not mutate the prior view");
});

test("a new pending request joins resolved history without losing either", () => {
  const resolved = { id: "old", seq: 1, status: "acknowledged", resolved_seq: 2 };
  const pending = { id: "new", seq: 3, status: "pending" };
  const result = mergeUserRequests(new Map([[resolved.id, resolved]]), [pending]);
  assert.deepEqual(Array.from(result.values()), [resolved, pending]);
});
