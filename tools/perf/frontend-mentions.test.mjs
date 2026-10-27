// Run with: node --test tools/perf/frontend-mentions.test.mjs
import assert from "node:assert/strict";
import test from "node:test";
import { createRequire } from "node:module";

globalThis.localStorage = { getItem: () => null, setItem() {} };
globalThis.document = { hidden: true, querySelector: () => null };
globalThis.window = { matchMedia: () => ({ matches: true }) };
globalThis.marked = createRequire(import.meta.url)("../../web/vendor/marked.min.js");
const { state } = await import("../../web/js/state.js");
const { resolveMention, parseMarkerNotice } = await import("../../web/js/mentions.js");
const { md, mentionTextHtml, markerNoticeHtml, requestRouteHtml } = await import("../../web/js/markdown.js");

const seats = [
  { id: "agy-a9423db9afb1", label: "Argon-writer", provider: "agy", enabled: false },
  { id: "codex-0769a4444dd5", label: "Terra-reviewer", provider: "codex", enabled: true },
  { id: "codex-2", label: "검수", provider: "codex", enabled: true },
  { id: "claude-1", label: "<img src=x>", provider: "claude", enabled: true },
];
state.room = { id: "r", participants: seats };
state.messages = new Map();
const pills = (html) => html.match(/class="mention-pill[^"]*"/g) || [];

test("mentions resolve like the server", () => {
  const r = (t) => resolveMention(t, seats);
  assert.deepEqual([r("Terra-reviewer").kind, r("Terra-reviewer").ids], ["seat", ["codex-0769a4444dd5"]]);
  assert.deepEqual(r("Terra-reviewer한테").ids, ["codex-0769a4444dd5"], "Korean particle after a label");
  assert.deepEqual(r("검수에게").ids, ["codex-2"]);
  assert.deepEqual([r("Argon-writer").kind, r("Argon-writer").enabled], ["seat", []], "an off seat matches but is not called");
  assert.deepEqual([r("codex").kind, r("codex").ids], ["group", ["codex-0769a4444dd5", "codex-2"]]);
  assert.deepEqual(r("codex-2").ids, ["codex-2"]);
  assert.equal(r("전원").kind, "all");
  assert.equal(r("Ghost").kind, "none");
  assert.equal(r("Terra-reviewerx").kind, "none", "a label must end at a particle");
});

test("Markdown bodies get pills outside code only", () => {
  const html = md("@Terra-reviewer 확인해 줘\n\n```\n@codex inside fence\n```\n\n그리고 `@codex` 인라인, 메일 a@b.com");
  assert.equal(pills(html).length, 1, html);
  assert.match(html, /class="mention-pill seat-1 codex"[^>]*>@Terra-reviewer</);
  assert.match(html, /<code[^>]*>@codex inside fence/);
  assert.match(html, /<code>@codex<\/code>/);
  assert.match(html, /a@b\.com/);
  // Not autolinked by Markdown, so only the mention pattern keeps these plain.
  assert.equal(pills(md("user@codex 와 경로/@codex, x.@codex")).length, 0);
  assert.equal(pills(mentionTextHtml("user@codex 경로/@codex")).length, 0);
});

test("pill styles tell called, off, group, everyone and unmatched apart", () => {
  const html = md("@Argon-writer @codex @all @Ghost");
  const cls = pills(html);
  assert.equal(cls.length, 4, html);
  assert.match(cls[0], /seat-0 agy off/);
  assert.match(cls[1], /group codex/);
  assert.match(cls[2], /group all/);
  assert.match(cls[3], /unmatched/);
  assert.match(html, /꺼진 좌석이라 호출되지 않음/);
  assert.match(html, /일치하는 좌석 없음 · 호출되지 않음/);
});

test("labels are escaped once and plain text keeps code untouched", () => {
  assert.match(md("@claude-1"), /@&lt;img src=x&gt;</);
  assert.doesNotMatch(md("@claude-1"), /<img/);
  const plain = mentionTextHtml("@Terra-reviewer `@codex` <b>x</b>");
  assert.equal(pills(plain).length, 1, plain);
  assert.match(plain, /`@codex`/);
  assert.match(plain, /&lt;b&gt;x&lt;\/b&gt;/);
});

// Room 8a864d16ac29 #31 read as if @Terra-reviewer were the receiver; it is the sender.
const notice31 = {
  id: "n31", kind: "system", notice_for: "codex-0769a4444dd5", notice_marker: "request", notice_msg: "m30",
  text: "@codex-0769a4444dd5 마커 거절 ([[REQUEST:@Argon-writer]] 위 두 경로의 순서 고정 반례를 먼저 재현하고 수정해 줘.): @Argon-writer와 일치하는 활성 좌석 없음 · 다른 활성 좌석의 ID나 고유한 이름이 필요함",
};

test("a marker rejection notice reads sender → receiver · not delivered: reason", () => {
  const n = parseMarkerNotice(notice31.text);
  assert.equal(n.target, "Argon-writer");
  assert.match(n.reason, /^@Argon-writer와 일치하는 활성 좌석 없음/);
  const html = markerNoticeHtml(notice31);
  const sender = html.indexOf("Terra-reviewer</span>");
  const arrow = html.indexOf("route-arrow");
  const receiver = html.indexOf("Argon-writer (꺼짐)");
  assert.ok(sender > 0 && sender < arrow && arrow < receiver, html);
  assert.match(html, /aria-label="보낸 좌석 Terra-reviewer → 받는 좌석 Argon-writer, 요청 마커, 전달 안 됨: @Argon-writer와/);
  assert.match(html, /route-fail">전달 안 됨: @Argon-writer와 일치하는 활성 좌석 없음/);
  assert.match(html, /<details class="route-raw"><summary>원문<\/summary><code>\[\[REQUEST:@Argon-writer\]\]/);
  assert.equal(markerNoticeHtml({ kind: "system", text: "토론 시작" }), null);
  assert.equal(markerNoticeHtml({ ...notice31, notice_marker: "discussion" }), null);
});

test("a rejected request line in a reply becomes the same route with the notice's reason", () => {
  const raw = "[[REQUEST:@Argon-writer]] 위 두 경로의 순서 고정 반례를 먼저 재현하고 수정해 줘.";
  const reply = { id: "m30", kind: "agent", author: "codex-0769a4444dd5" };
  assert.match(requestRouteHtml(raw, reply), /처리되지 않음/);
  state.messages.set("n31", notice31);
  const html = requestRouteHtml(raw, reply);
  assert.match(html, /전달 안 됨: @Argon-writer와 일치하는 활성 좌석 없음/);
  assert.ok(html.indexOf("Terra-reviewer</span>") < html.indexOf("Argon-writer (꺼짐)"));
  assert.equal(requestRouteHtml("[[DECISION: x]]", reply), null);
});
