import assert from "node:assert/strict";
import test from "node:test";

globalThis.localStorage = { getItem: () => null, setItem: () => {} };
globalThis.window = { matchMedia: () => ({ matches: true }) };

const mounted = new Map();
const tl = {
  children: [],
  appendChild(n) { tl.children.push(n); mounted.set(n.id, n); },
  insertBefore(n) { tl.appendChild(n); },
  querySelector(sel) { return mounted.get(sel.replace("#", "")) || null; },
  querySelectorAll() { return Array.from(mounted.values()); },
};
const banner = { hidden: false, textContent: "" };
globalThis.document = {
  hidden: false,
  createElement(tag) {
    const dummy = { dataset: {}, contains() { return false; }, appendChild(c) { return c; }, set textContent(_) {}, set innerHTML(_) {}, replaceChildren() {}, querySelector() { return dummy; }, querySelectorAll() { return []; }, setAttribute() {} };
    return {
      id: "",
      className: "",
      dataset: {},
      remove() {
        mounted.delete(this.id);
        const idx = tl.children.indexOf(this);
        if (idx >= 0) tl.children.splice(idx, 1);
      },
      querySelector() { return dummy; },
      querySelectorAll() { return []; },
      replaceChildren() {},
      setAttribute() {},
    };
  },
  getElementById(id) { return mounted.get(id) || null; },
  querySelector(sel) {
    if (sel === "#timeline") return tl;
    if (sel === "#history-banner" || sel === "#history-status") return banner;
    return mounted.get(sel.replace("#", "")) || null;
  },
  querySelectorAll() { return []; },
};

const { mergeUserRequests } = await import("../../web/js/user-requests.js");
const { state } = await import("../../web/js/state.js");
const { isUserRequestNoticeHidden, renderMessage } = await import("../../web/js/timeline.js");

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

test("isUserRequestNoticeHidden hides user_request notice only when original seat reply is in loaded messages", () => {
  const agent = { id: "m-agent", kind: "agent" };
  const notice = { id: "m-notice", kind: "system", notice_marker: "user_request", notice_msg: agent.id };
  const msgs = new Map([[agent.id, agent], [notice.id, notice]]);

  assert.equal(isUserRequestNoticeHidden(notice, msgs), true, "원본 좌석 답이 있으면 숨김");
  assert.equal(isUserRequestNoticeHidden(notice, new Map([[notice.id, notice]])), false, "원본이 없으면 유일한 표시이므로 노출");
  assert.equal(isUserRequestNoticeHidden({ kind: "system", notice_marker: "user_request" }, msgs), false);
  assert.equal(isUserRequestNoticeHidden({ kind: "system", user_request_reply: { id: "r1" } }, msgs), false, "확인 완료 공지는 노출");
  assert.equal(isUserRequestNoticeHidden({ kind: "system", notice_marker: "request", notice_msg: agent.id }, msgs), false, "마커 거절 공지는 노출");
});

test("timeline suppresses user_request notice when original seat reply is loaded", () => {
  mounted.clear(); tl.children = []; state.messages.clear(); state.order = []; state.roomId = "t";
  const agent = { id: "a1", seq: 2, kind: "agent", status: "done", text: "답변" };
  const notice = { id: "n1", seq: 3, kind: "system", notice_marker: "user_request", notice_msg: "a1", text: "공지" };
  state.messages.set(agent.id, agent);
  state.messages.set(notice.id, notice);
  state.order = [agent.id, notice.id];
  state.winStart = 0; state.winEnd = 10;

  renderMessage(notice.id, true, false);
  assert.equal(mounted.has("m-n1"), false, "공지는 타임라인에 마운트되지 않아야 함");
  assert.equal(state.messages.get("n1"), notice, "메시지 데이터는 그대로 보존");
});

test("SSE or streaming: notice arriving first is removed once original seat reply renders", () => {
  mounted.clear(); tl.children = []; state.messages.clear(); state.order = []; state.roomId = "t";
  const agent = { id: "a2", seq: 2, kind: "agent", author: "agy-flash", status: "done", text: "답변" };
  const notice = { id: "n2", seq: 3, kind: "system", notice_marker: "user_request", notice_msg: "a2", text: "공지" };

  // 공지 먼저 도착 (원본 미도착)
  state.messages.set(notice.id, notice);
  state.order = [notice.id];
  state.winStart = 0; state.winEnd = 10;
  renderMessage(notice.id, true, false);
  assert.equal(mounted.has("m-n2"), true, "원본 부재 시 공지가 먼저 마운트됨");

  // 나중에 원본 도착/렌더링
  state.messages.set(agent.id, agent);
  state.order = [agent.id, notice.id];
  renderMessage(agent.id, true, false);
  assert.equal(mounted.has("m-n2"), false, "원본 렌더링 후 기존 공지가 타임라인에서 제거됨");
});

test("when original seat reply is not in loaded messages, notice remains visible", () => {
  mounted.clear(); tl.children = []; state.messages.clear(); state.order = []; state.roomId = "t";
  const notice = { id: "n3", seq: 3, kind: "system", notice_marker: "user_request", notice_msg: "missing", text: "공지" };
  state.messages.set(notice.id, notice);
  state.order = [notice.id];
  state.winStart = 0; state.winEnd = 10;
  renderMessage(notice.id, true, false);
  assert.equal(mounted.has("m-n3"), true, "원본 미불러옴 시 공지가 타임라인에 표시됨");
});
