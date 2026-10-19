// Live progress of streaming replies and the work history (trace) with subagents.
import { $, el, esc, fmtK, TRACE_CACHE_LIMIT, TRACE_EVENT_LIMIT } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { openSeq, state } from "./state.js";
import { md } from "./markdown.js";

// ---------- live progress for replies that have no text yet ----------
// Codex narrates while it works; Claude and Antigravity often stay silent and
// delegate to subagents for minutes. Without this the bubble is just a cursor.
const firstLine = (s) => String(s || "").replace(/\s+/g, " ").trim().slice(0, 160);

export function liveFeed(id, e) {
  if (!e) return;
  let lv = state.live.get(id);
  if (!lv) {
    lv = { subs: new Map(), tools: 0, lastTool: "", thought: "", ver: 0 };
    state.live.set(id, lv);
  }
  const sub = (key, name) => {
    let s = lv.subs.get(key);
    if (!s) {
      s = { name: name || "서브에이전트", task: "", last: "", tools: 0, done: false };
      lv.subs.set(key, s);
    }
    return s;
  };
  switch (e.kind) {
    case "subagent_start": {
      const s = sub(e.parent, e.name);
      s.name = e.name || s.name;
      s.task = firstLine((e.meta && e.meta.description) || e.input);
      break;
    }
    case "subagent_msg": {
      const s = sub(e.parent);
      if (e.role === "assistant" && (e.text || "").trim()) s.last = firstLine(e.text);
      else if (e.role === "tool_call") { s.tools++; s.lastTool = e.name || "도구"; }
      else if (e.role === "status" && e.text) s.last = s.last || firstLine(e.text);
      break;
    }
    case "subagent_end": {
      const s = sub(e.parent);
      s.done = true;
      if ((e.output || "").trim()) s.last = firstLine(e.output);
      break;
    }
    case "tool_call":
      lv.tools++;
      lv.lastTool = e.name || "도구";
      break;
    case "thinking":
      if ((e.text || "").trim()) lv.thought = firstLine(e.text);
      break;
    default:
      return;
  }
  lv.ver++;
}

export function liveHtml(lv) {
  const subs = [...lv.subs.values()];
  const running = subs.filter((s) => !s.done).length;
  const head = [];
  if (subs.length) head.push("서브에이전트 " + subs.length + "개 (실행 중 " + running + ")");
  if (lv.tools) head.push("도구 " + lv.tools + "회" + (lv.lastTool ? " · 최근 " + lv.lastTool : ""));
  if (!head.length && !lv.thought) return '<p class="muted live-progress">아직 본문을 쓰지 않았어. 작업을 준비하는 중…</p>';
  let html = '<div class="live-progress" aria-live="off"><div class="muted">진행 요약 · ' + esc(head.join(" · ") || "생각 중") + "</div>";
  if (lv.thought) html += '<div class="live-thought">💭 ' + esc(lv.thought) + "</div>";
  for (const s of subs.slice(-6)) {
    html +=
      '<div class="live-sub' + (s.done ? " done" : "") + '"><span class="live-sub-name">' + (s.done ? "✓ " : "🧩 ") + esc(s.name) + "</span>" +
      (s.task ? ' <span class="muted">' + esc(s.task) + "</span>" : "") +
      (s.last ? '<div class="live-sub-last">↳ ' + esc(s.last) + "</div>" : s.tools ? '<div class="live-sub-last">↳ 도구 ' + s.tools + "회 · " + esc(s.lastTool || "") + "</div>" : "") +
      "</div>";
  }
  if (subs.length > 6) html += '<div class="muted">… 외 ' + (subs.length - 6) + "개</div>";
  return html + "</div>";
}

// A page opened mid-run has no live state yet: rebuild it once from the stored trace.
export async function seedLive(roomId, id) {
  try {
    const events = await api("GET", "/api/rooms/" + roomId + "/trace/" + id);
    if (state.roomId !== roomId) return;
    // The stored trace already contains anything SSE delivered meanwhile:
    // rebuild from it instead of adding to a partial state.
    state.live.delete(id);
    for (const e of events) liveFeed(id, e);
    hooks.scheduleRender(id);
  } catch (err) {
    console.warn("live progress seed failed", id, err.message);
  }
}

export function activeTraceActivity(id, author) {
  const list = state.traces.get(id);
  if (list && list.length) {
    const openTools = new Map();
    const openSubs = new Map();
    for (const e of list) {
      if (e.kind === "tool_call" && e.id) openTools.set(e.id, e.name || "도구");
      else if (e.kind === "tool_result" && e.id) openTools.delete(e.id);
      else if (e.kind === "subagent_start" && e.parent) openSubs.set(e.parent, e.name || e.parent);
      else if (e.kind === "subagent_end" && e.parent) openSubs.delete(e.parent);
    }
    if (openSubs.size > 0) {
      const lastSub = Array.from(openSubs.values()).pop();
      return "🧩 서브에이전트: " + lastSub;
    }
    if (openTools.size > 0) {
      const lastTool = Array.from(openTools.values()).pop();
      return "🔧 도구: " + lastTool;
    }
  }
  const p = state.room && state.room.participants.find((x) => x.id === author);
  return (p && p.status_text) || "작성 중";
}

export function updateTraceCount(id) {
  const b = $("#m-" + id + " .trace-btn");
  const m = state.messages.get(id);
  if (b && m) {
    const isExp = state.expanded.has(id);
    b.textContent = (isExp ? "작업 기록 접기" : "작업 기록") + (m.trace ? " · " + m.trace : "");
    b.setAttribute("aria-expanded", isExp ? "true" : "false");
  }
}

export function expandTrace(id) {
  state.expanded.add(id);
  while (state.expanded.size > TRACE_CACHE_LIMIT) {
    const oldest = state.expanded.values().next().value;
    state.expanded.delete(oldest); state.userCollapsed.add(oldest);
    state.traces.delete(oldest); state.traceMeta.delete(oldest);
    state.traceControllers.get(oldest)?.abort();
    const box = $("#t-" + oldest);
    if (box) { box.hidden = true; box.replaceChildren(); }
    updateTraceCount(oldest);
  }
}

export function boundedTraceEvent(event) {
  const item = { ...event };
  for (const key of ["text", "input", "output"]) {
    if (typeof item[key] === "string" && item[key].length > 8000) {
      item[key] = item[key].slice(0, 8000); item._displayTruncated = true;
    }
  }
  return item;
}

// The server already cuts the window (?tail=) and keeps the subagent start and
// model events a window needs; this cache only appends streamed events to it.
export function cacheTrace(id, events, total) {
  const bounded = events.map(boundedTraceEvent);
  state.traces.delete(id);
  state.traces.set(id, bounded);
  state.traceMeta.set(id, { total, shortened: bounded.some((event) => event._displayTruncated) });
  while (state.traces.size > TRACE_CACHE_LIMIT) {
    const oldest = state.traces.keys().next().value;
    state.traces.delete(oldest); state.traceMeta.delete(oldest); state.expanded.delete(oldest);
    const box = $("#t-" + oldest);
    if (box) { box.hidden = true; box.replaceChildren(); }
    updateTraceCount(oldest);
  }
}

// Opening a trace loads the last TRACE_EVENT_LIMIT events; full=true replaces
// that window with the whole trace when the user asks for the earlier part.
export async function ensureAndRenderTrace(id, full = false) {
  if (!state.expanded.has(id) || document.hidden) return;
  const roomId = state.roomId; const sequence = openSeq;
  if (full || !state.traces.has(id)) {
    if (state.traceFetching.has(id)) return;
    state.traceFetching.add(id);
    state.tracePending.set(id, new Map());
    const controller = new AbortController();
    state.traceControllers.set(id, controller);
    if (full) renderTrace(id);
    try {
      const url = "/api/rooms/" + roomId + "/trace/" + id + (full ? "" : "?tail=" + TRACE_EVENT_LIMIT);
      const res = await api("GET", url, undefined, controller.signal);
      if (state.roomId !== roomId || openSeq !== sequence || !state.messages.has(id) || !state.expanded.has(id)) return;
      const events = full ? res : res.events;
      const fetchedTotal = full ? res.length : res.total;
      const pending = state.tracePending.get(id);
      const later = [...pending.entries()].filter(([index]) => index > fetchedTotal).sort(([a], [b]) => a - b);
      const total = later.length ? later[later.length - 1][0] : fetchedTotal;
      cacheTrace(id, [...events, ...later.map(([, event]) => event)], total);
      state.messages.get(id).trace = Math.max(state.messages.get(id).trace || 0, total);
      state.traceErrors.delete(id);
    } catch (err) {
      if (controller.signal.aborted) return;
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (full) {
        if (!state.expanded.has(id)) return;
        // The window stays on screen; the failure is shown next to the button that retries it.
        state.traceMeta.get(id).fullError = err.message;
      } else {
        state.traceErrors.set(id, err.message);
        renderTraceError(id);
        updateTraceCount(id);
      }
    } finally {
      if (state.traceControllers.get(id) === controller) {
        state.traceFetching.delete(id);
        state.tracePending.delete(id);
        state.traceControllers.delete(id);
        // Rendered after the fetch flag is cleared so the window note no longer reads as loading.
        if (state.traces.has(id)) {
          renderTrace(id);
          updateTraceCount(id);
        }
        hooks.syncPendingScrollRestore();
      }
    }
    return;
  }
  renderTrace(id);
  updateTraceCount(id);
  hooks.syncPendingScrollRestore();
}

function renderTraceError(id) {
  const box = $("#t-" + id);
  if (!box) return;
  box.hidden = false;
  const msg = state.traceErrors.get(id) || "작업 기록을 불러오지 못했습니다.";
  box.innerHTML = "";
  const errDiv = el("div", "err-box");
  errDiv.appendChild(el("span", "", "⚠ 작업 기록 조회 실패: " + msg));
  const retryBtn = el("button", "retry-btn", "↻ 다시 불러오기");
  retryBtn.type = "button";
  retryBtn.onclick = () => ensureAndRenderTrace(id);
  errDiv.appendChild(retryBtn);
  box.appendChild(errDiv);
}

// Fix #4: Record manual collapse in state.userCollapsed so autoTrace doesn't override user choice
export async function toggleTrace(id) {
  const box = $("#t-" + id);
  if (state.expanded.has(id)) {
    state.expanded.delete(id);
    state.userCollapsed.add(id);
    if (box) { box.hidden = true; box.replaceChildren(); }
    state.traces.delete(id); state.traceMeta.delete(id);
    state.traceControllers.get(id)?.abort();
    updateTraceCount(id);
    return;
  }
  state.userCollapsed.delete(id);
  expandTrace(id);
  await ensureAndRenderTrace(id);
}

// ---------- Fix #5: Incremental Trace DOM Updates (preserve <details open> & focus) ----------
const pre = (s) => (s ? "<pre>" + esc(String(s).slice(0, 20000)) + "</pre>" : "");
const inputPretty = (s) => {
  try {
    const o = JSON.parse(s);
    return JSON.stringify(o, null, 1);
  } catch {
    return s;
  }
};
function summarize(s) {
  if (!s) return "";
  s = String(s).replace(/\s+/g, " ");
  return s.length > 90 ? s.slice(0, 90) + "…" : s;
}

// Built once per open trace and updated in place so focus on its controls survives renders.
function buildTraceNotice(box, id) {
  const notice = el("div", "trace-window-note muted");
  const more = el("button", "ghost trace-more-btn");
  more.type = "button";
  more.onclick = () => ensureAndRenderTrace(id, true);
  const full = el("a", "", "전체 기록 내려받기");
  full.href = "/api/rooms/" + state.roomId + "/trace/" + id;
  full.download = "work-history.json";
  notice.append(el("span", "trace-window-count"), more, " ", el("span", "trace-shortened"), el("span", "trace-more-error"), full);
  box.prepend(notice);
  return notice;
}

function renderTrace(id) {
  const box = $("#t-" + id);
  if (!box) return;
  const shouldPinBottom = !state.browsingHistory && state.stickToBottom;
  box.hidden = false;
  const events = state.traces.get(id);
  const metadata = state.traceMeta.get(id);
  // Events the window leaves out; the subagent context the server adds is counted as shown.
  const hiddenCount = metadata.total - events.length;
  const windowStart = hiddenCount;
  const moved = box.dataset.windowStart !== undefined && Number(box.dataset.windowStart) !== windowStart;
  const opened = new Set(moved ? [...box.querySelectorAll("details[open][data-trace-key]")].map((node) => node.dataset.traceKey) : []);
  const focused = moved && box.contains(document.activeElement) ? document.activeElement.closest("[data-trace-key]") : null;
  const focusedKey = focused && focused.dataset.traceKey;
  let notice = box.querySelector(".trace-window-note");
  if (!notice) notice = buildTraceNotice(box, id);
  if (moved) for (const child of [...box.children]) if (child !== notice) child.remove();
  box.dataset.windowStart = String(windowStart);
  const loading = state.traceFetching.has(id);
  const more = notice.querySelector(".trace-more-btn");
  const moreFocused = document.activeElement === more;
  notice.querySelector(".trace-window-count").textContent =
    hiddenCount > 0 ? "최근 " + events.length + "개 이벤트 표시 · 전체 " + metadata.total + "개. " : "";
  // Kept focusable while loading; a second click is dropped by the fetch guard.
  more.hidden = hiddenCount === 0;
  more.textContent = loading ? "이전 기록 불러오는 중…" : "이전 기록 " + hiddenCount + "개 더 보기";
  more.setAttribute("aria-disabled", String(loading));
  notice.querySelector(".trace-shortened").textContent = metadata.shortened ? "긴 출력은 화면에서 줄였습니다. " : "";
  notice.querySelector(".trace-more-error").textContent = metadata.fullError ? "⚠ 이전 기록 조회 실패: " + metadata.fullError + " " : "";
  if (moreFocused && more.hidden) notice.querySelector("a").focus({ preventScroll: true });
  const items = [];
  const byCall = new Map();
  const subs = new Map();
  let seq = windowStart;

  for (const e of events) {
    seq++;
    switch (e.kind) {
      case "tool_call": {
        const key = "tool:" + (e.id || seq);
        const it = { type: "tool", key, call: e };
        items.push(it);
        if (e.id) byCall.set(e.id, it);
        break;
      }
      case "tool_result": {
        const it = e.id && byCall.get(e.id);
        if (it) it.result = e;
        else items.push({ type: "tool", key: "tool:res:" + (e.id || seq), result: e });
        break;
      }
      case "subagent_start": {
        const key = "sub:" + (e.parent || seq);
        let s = subs.get(e.parent);
        if (!s) {
          s = { type: "sub", key, start: e, lines: [] };
          items.push(s);
          if (e.parent) subs.set(e.parent, s);
        } else {
          s.start = e;
        }
        // The model the caller asked for; subagent_meta reports the one that ran.
        if (e.meta && e.meta.model) s.requested = e.meta.model;
        break;
      }
      case "subagent_msg":
      case "subagent_meta": {
        let s = subs.get(e.parent);
        if (!s) {
          s = {
            type: "sub",
            key: "sub:" + (e.parent || seq),
            start: { name: e.parent, parent: e.parent },
            lines: [],
          };
          items.push(s);
          if (e.parent) subs.set(e.parent, s);
        }
        if (e.kind === "subagent_meta") {
          s.model = e.meta.model;
          s.effort = e.meta.effort;
        } else {
          s.lines.push(e);
        }
        break;
      }
      case "subagent_end": {
        const s = subs.get(e.parent);
        if (s) s.end = e;
        else items.push({ type: "plain", key: "plain:" + seq, e });
        break;
      }
      case "text_delta":
      case "text":
        break;
      default:
        items.push({ type: "plain", key: "plain:" + seq, e });
    }
  }

  if (!items.length) {
    if (!box.querySelector(".trace-empty")) {
      box.appendChild(el("div", "muted trace-empty", "표시할 도구 기록 없음"));
    }
    if (shouldPinBottom) {
      const tl = $("#timeline");
      if (tl) tl.scrollTop = tl.scrollHeight;
    }
    return;
  }
  const emptyEl = box.querySelector(".trace-empty");
  if (emptyEl) emptyEl.remove();
  const errEl = box.querySelector(".err-box");
  if (errEl) errEl.remove();

  const keys = new Set(items.map((it) => it.key));
  for (const child of [...box.children]) if (child.dataset.traceKey && !keys.has(child.dataset.traceKey)) child.remove();
  const existingItems = new Map([...box.children].filter((child) => child.dataset.traceKey).map((child) => [child.dataset.traceKey, child]));
  for (const it of items) {
    let existing = existingItems.get(it.key);
    if (!existing) {
      const created = createTraceItemElement(it);
      created.dataset.traceKey = it.key;
      if (opened.has(it.key) && created.tagName === "DETAILS") created.open = true;
      box.appendChild(created);
      if (focusedKey === it.key) created.querySelector("summary")?.focus({ preventScroll: true });
    } else {
      patchTraceItemElement(existing, it);
    }
  }
  if (shouldPinBottom) {
    const tl = $("#timeline");
    if (tl) tl.scrollTop = tl.scrollHeight;
  }
}

function toolSig(it) {
  const c = it.call || {};
  const r = it.result || {};
  return (c.name || "") + "|" + (c.input || "") + "|" + (r.kind || "") + "|" + (r.output ? r.output.length : 0);
}

function createTraceItemElement(it) {
  if (it.type === "tool") {
    const det = el("details", "item");
    det.dataset.sig = toolSig(it);
    det.innerHTML = buildToolInnerHtml(it);
    return det;
  }
  if (it.type === "sub") {
    const det = el("details", "item sub");
    det.open = true;
    const sum = el("summary");
    fillSubSummary(sum, it);
    det.appendChild(sum);
    const linesBox = el("div", "lines");
    det.appendChild(linesBox);
    syncSubTask(det, it);
    syncSubLines(linesBox, it.lines);
    const repBox = el("div", "sub-report");
    if (it.end && it.end.output) {
      repBox.innerHTML = '<div class="line"><span class="k">보고</span>' + pre(it.end.output) + "</div>";
    }
    det.appendChild(repBox);
    return det;
  }
  const wrap = el("div");
  wrap.innerHTML = renderPlainTraceHtml(it.e);
  return wrap.firstElementChild || wrap;
}

function patchTraceItemElement(node, it) {
  if (it.type === "tool") {
    const sig = toolSig(it);
    if (node.dataset.sig === sig) return;
    const wasOpen = node.open;
    node.dataset.sig = sig;
    node.innerHTML = buildToolInnerHtml(it);
    node.open = wasOpen;
    return;
  }
  if (it.type === "sub") {
    const sum = node.querySelector(":scope > summary");
    if (sum) fillSubSummary(sum, it);
    const linesBox = node.querySelector(":scope > .lines");
    if (linesBox) {
      syncSubTask(node, it);
      syncSubLines(linesBox, it.lines);
    }
    const repBox = node.querySelector(":scope > .sub-report");
    if (repBox && it.end && it.end.output && !repBox.hasChildNodes()) {
      repBox.innerHTML = '<div class="line"><span class="k">보고</span>' + pre(it.end.output) + "</div>";
    }
  }
}

function buildToolInnerHtml(it) {
  const c = it.call || {},
    r = it.result || {};
  const name = c.name || r.name || "도구";
  const err = r.meta && (r.meta.error || (r.meta.exit_code != null && r.meta.exit_code !== 0));
  const st = r.kind ? (err ? "⚠ 실패" : "✓ 완료") : "⏳ 실행 중";
  return (
    "<summary>🔧 <b>" +
    esc(name) +
    "</b> " +
    st +
    ' <span class="muted">' +
    esc(summarize(c.input || r.input)) +
    "</span></summary>" +
    (c.input || r.input ? '<div class="k">입력</div>' + pre(inputPretty(c.input || r.input)) : "") +
    (r.output ? '<div class="k">결과</div>' + pre(r.output) : "")
  );
}

// Model and effort come from the CLI and are shown only as far as it reported
// them: Claude never reports a subagent's effort, Antigravity neither value.
function fillSubSummary(sum, it) {
  const s = it.start || {};
  const name = s.name || s.parent || "subagent";
  const model = [it.model || it.requested, it.effort].filter(Boolean).join(" · ");
  const desc = (s.meta && s.meta.description) || "";
  const sig = JSON.stringify([name, model, !!it.end, desc]);
  if (sum.dataset.sig === sig) return;
  sum.dataset.sig = sig;
  const parts = ["🧩 서브에이전트 ", el("b", "", name)];
  if (model) parts.push(" ", el("span", "sub-model", model));
  parts.push(it.end ? " ✓ 종료" : " ⏳ 진행 중");
  if (desc) parts.push(" ", el("span", "muted", desc));
  sum.replaceChildren(...parts);
}

// A subagent first seen through its lines gets its task once subagent_start arrives.
function syncSubTask(det, it) {
  const input = it.start && it.start.input;
  if (!input || det.querySelector(":scope > .sub-task")) return;
  const taskDiv = el("div", "line sub-task");
  taskDiv.innerHTML = '<span class="k">과제</span>' + pre(input);
  det.insertBefore(taskDiv, det.querySelector(":scope > .lines"));
}

// Append only new subagent lines so open <details class="line"> inside subagents stay open
function syncSubLines(linesBox, lines) {
  const renderedCount = linesBox.children.length;
  for (let i = renderedCount; i < lines.length; i++) {
    const wrap = el("div");
    wrap.innerHTML = renderSubLine(lines[i]);
    const child = wrap.firstElementChild || wrap;
    linesBox.appendChild(child);
  }
}

function renderPlainTraceHtml(e) {
  switch (e.kind) {
    case "status":
      return '<div class="item status">· ' + esc(e.text) + "</div>";
    case "error":
      return '<div class="item error">⚠ ' + esc(e.text) + "</div>";
    case "thinking":
      return '<details class="item"><summary class="thinking">💭 추론</summary>' + pre(e.text) + "</details>";
    case "session":
      return (
        '<div class="item status">세션 ' +
        esc(e.text) +
        (e.meta && e.meta.model ? " · " + esc(e.meta.model) : "") +
        "</div>"
      );
    case "usage":
      return (
        '<div class="item status">사용량 ' +
        esc(
          Object.entries(e.meta || {})
            .map(([k, v]) => k + "=" + (typeof v === "number" ? fmtK(v) : v))
            .join(" ")
        ) +
        "</div>"
      );
    case "limits":
      return '<div class="item status">한도 갱신</div>';
    default:
      return '<div class="item status">' + esc(e.kind) + " " + esc(e.text || "") + "</div>";
  }
}

function renderSubLine(e) {
  switch (e.role) {
    case "assistant":
      return '<div class="line"><span class="k">에이전트</span><div>' + md(e.text) + "</div></div>";
    case "user":
      return '<div class="line"><span class="k">입력</span>' + pre(e.text) + "</div>";
    case "system":
      return '<div class="line"><span class="k">시스템</span>' + pre(e.text) + "</div>";
    case "message":
      return '<div class="line"><span class="k">메시지</span>' + esc(e.name) + pre(e.text) + "</div>";
    case "tool_call":
      return (
        '<details class="line"><summary>🔧 ' +
        esc(e.name) +
        ' <span class="muted">' +
        esc(summarize(e.input)) +
        "</span></summary>" +
        pre(inputPretty(e.input)) +
        "</details>"
      );
    case "tool_result":
      return (
        '<details class="line"><summary>↩ 결과' +
        (e.name ? " " + esc(e.name) : "") +
        "</summary>" +
        pre(e.output) +
        "</details>"
      );
    case "status":
      return '<div class="line status">· ' + esc(e.text) + "</div>";
    default:
      return '<div class="line">' + esc(e.text || e.output || "") + "</div>";
  }
}
