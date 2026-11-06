import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import test from "node:test";

const source = readFileSync(new URL("../../web/js/delivery.js", import.meta.url), "utf8");
const { deliveryLabel } = await import("data:text/javascript;base64," + Buffer.from(source).toString("base64"));

test("enqueued input is never labelled as confirmed injection", () => {
  for (const state of ["pending", "sending"]) assert.equal(deliveryLabel({ state }), "전송 중");
  assert.equal(deliveryLabel({ state: "accepted" }), "현재 턴 전달됨");
  assert.equal(deliveryLabel({ state: "queued" }), "다음 턴 대기");
  assert.equal(deliveryLabel({ state: "uncertain" }), "전달 확인 중");
});

test("legacy Live and explicit stops remain distinct from delivery failure", () => {
  assert.equal(deliveryLabel(null, true), "현재 턴 전달 · 이전 기록");
  assert.equal(deliveryLabel(null, false), "다음 턴 전달");
  assert.equal(deliveryLabel({ state: "queued", halted: true }), "중단됨 · 입력 보존");
  assert.equal(deliveryLabel({ state: "failed" }), "전달 확인 실패");
});
