// Timeline: windowed message list, scroll pinning, history paging and message rendering.
import { $, announce, avatarContent, DOM_CAP_STEP, el, esc, fmtDur, fmtElapsed, fmtK, fmtTime, STREAM_THROTTLE_MS } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { activeParticipants, authorProvider, modelLabel, openSeq, participant, participantLabel, seatClass, state } from "./state.js";
import { applyRejectedMarkerChips, md, prepareAgentMessageText, rewriteProseMentions, visibleMentions } from "./markdown.js";
import { closeDrawers } from "./layout.js";
import { messageInputTokens } from "./usage.js";
import { renderMessageAttachments, renderMessageRequests } from "./attachments.js";
import { updateHint } from "./composer.js";
import { findWorkPart, focusWorkPartCard, isPartErrorAbandoned, isPartErrorResolved, reviewStepLabel } from "./work.js";
import { buildSessionActions, updateSessionActions } from "./participants.js";
import { timingTitle } from "./stats.js";
import { activeTraceActivity, ensureAndRenderTrace, liveHtml, toggleTrace } from "./trace.js";

// ---------- timeline (Fix P2 #2: Strict 200 DOM cap in all scroll positions) ----------
let isRenderingTimeline = false;
let suppressScrollStick = false;
let pendingRestoreScrollTop = null;
let lastTimelineScrollTop = 0;
let lastTimelineScrollHeight = 0;

function isTimelineAtBottom(tl = $("#timeline")) {
  if (!tl) return true;
  return tl.scrollHeight - tl.scrollTop - tl.clientHeight < 60;
}

function recordTimelineScrollMetrics(tl = $("#timeline")) {
  if (!tl) return;
  lastTimelineScrollTop = tl.scrollTop;
  lastTimelineScrollHeight = tl.scrollHeight;
}

function restoreTimelineScrollTop(targetTop) {
  const tl = $("#timeline");
  if (!tl) return;
  suppressScrollStick = true;
  tl.scrollTop = targetTop;
  recordTimelineScrollMetrics(tl);
  if (state.traceFetching.size > 0 && !state.stickToBottom) {
    pendingRestoreScrollTop = targetTop;
  } else {
    pendingRestoreScrollTop = null;
    requestAnimationFrame(() => {
      suppressScrollStick = false;
      recordTimelineScrollMetrics(tl);
    });
  }
}

export function syncPendingScrollRestore() {
  if (pendingRestoreScrollTop === null) return;
  if (state.stickToBottom) {
    pendingRestoreScrollTop = null;
    return;
  }
  const tl = $("#timeline");
  if (tl) {
    suppressScrollStick = true;
    tl.scrollTop = pendingRestoreScrollTop;
    recordTimelineScrollMetrics(tl);
  }
  if (state.traceFetching.size === 0) {
    pendingRestoreScrollTop = null;
    requestAnimationFrame(() => {
      suppressScrollStick = false;
      recordTimelineScrollMetrics(tl);
      updateJumpBottomBtn();
    });
  }
}

export function onTimelineImageLoad(ev) {
  const target = ev && ev.target;
  if (!target || target.tagName !== "IMG") return;
  const tl = $("#timeline");
  if (!tl || document.hidden) return;
  if (!state.browsingHistory && state.stickToBottom) {
    suppressScrollStick = true;
    tl.scrollTop = tl.scrollHeight;
    recordTimelineScrollMetrics(tl);
    requestAnimationFrame(() => {
      if (!state.browsingHistory && state.stickToBottom) {
        tl.scrollTop = tl.scrollHeight;
        recordTimelineScrollMetrics(tl);
      }
      suppressScrollStick = false;
      updateJumpBottomBtn();
    });
  } else if (pendingRestoreScrollTop !== null) {
    suppressScrollStick = true;
    tl.scrollTop = pendingRestoreScrollTop;
    recordTimelineScrollMetrics(tl);
    requestAnimationFrame(() => {
      suppressScrollStick = false;
      recordTimelineScrollMetrics(tl);
      updateJumpBottomBtn();
    });
  } else {
    recordTimelineScrollMetrics(tl);
  }
}

export async function focusTimelineMessage(msgId) {
  if (!msgId || !state.roomId) return;
  const roomId = state.roomId;
  closeDrawers();
  if (!state.messages.has(msgId) && (state.browsingHistory || state.pendingMessages > 0)) {
    await jumpToLatestMessages();
    if (state.roomId !== roomId) return;
  }
  const sequence = openSeq;
  while (!state.messages.has(msgId) && state.history && state.history.has_more && state.order.length > 0) {
    if (state.historyLoading) break;
    const prevOldest = state.order[0];
    await loadOlderMessages();
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if (!state.order.length || state.order[0] === prevOldest) break;
  }
  const idx = state.order.indexOf(msgId);
  if (idx >= 0 && (idx < state.winStart || idx >= state.winEnd)) {
    const half = Math.floor(DOM_CAP_STEP / 2);
    state.winStart = Math.max(0, Math.min(idx - half, state.order.length - DOM_CAP_STEP));
    state.winEnd = Math.min(state.order.length, state.winStart + DOM_CAP_STEP);
    renderTimeline(false);
  }
  const node = $("#m-" + msgId);
  if (!node) {
    announce("해당 메시지를 현재 대화 범위에서 찾지 못했습니다.");
    return;
  }
  node.scrollIntoView({ behavior: "smooth", block: "center" });
  node.classList.remove("msg-flash");
  void node.offsetWidth;
  node.classList.add("msg-flash");
  setTimeout(() => {
    node.classList.remove("msg-flash");
  }, 1600);
  announce("메시지로 이동했습니다.");
}

export function updateJumpBottomBtn() {
  if (isRenderingTimeline) return;
  const btn = $("#btn-jump-bottom");
  const badge = $("#jump-bottom-count");
  const tl = $("#timeline");
  if (!btn || !tl || !state.roomId) {
    if (btn) btn.hidden = true;
    return;
  }
  const atBottom = !state.browsingHistory && state.stickToBottom;
  if (atBottom) {
    state.unreadBelow = 0;
    btn.hidden = true;
    if (badge) badge.hidden = true;
    return;
  }
  btn.hidden = false;
  const count = (state.browsingHistory ? state.pendingMessages : 0) + state.unreadBelow;
  if (badge) {
    if (count > 0) {
      badge.hidden = false;
      badge.textContent = count > 99 ? "99+" : String(count);
    } else {
      badge.hidden = true;
      badge.textContent = "0";
    }
  }
  const label = count > 0 ? "맨 아래로 이동 (새 메시지 " + count + "개)" : "맨 아래로 이동";
  btn.setAttribute("aria-label", label);
  btn.title = label;
}

export async function scrollToTimelineBottom() {
  const tl = $("#timeline");
  if (!tl) return;
  pendingRestoreScrollTop = null;
  suppressScrollStick = false;
  state.stickToBottom = true;
  state.unreadBelow = 0;
  if (state.browsingHistory || state.pendingMessages > 0 || state.winEnd < state.order.length) {
    await jumpToLatestMessages();
    return;
  }
  tl.scrollTop = tl.scrollHeight;
  recordTimelineScrollMetrics(tl);
  updateJumpBottomBtn();
}

export function renderTimeline(scrollToBottom, explicitPriorTop) {
  const tl = $("#timeline");
  if (!tl) return;
  const priorTop = explicitPriorTop !== undefined ? explicitPriorTop : tl.scrollTop;
  const shouldPinBottom = scrollToBottom || (state.stickToBottom && !state.browsingHistory);
  isRenderingTimeline = true;
  try {
    tl.innerHTML = "";

    const total = state.order.length;
    if (!total) {
      state.winStart = 0;
      state.winEnd = 0;
      updateTimelineBanners(tl);
      renderRoomEmptyGuide(tl);
      recordTimelineScrollMetrics(tl);
      return;
    }

    if (shouldPinBottom || state.winEnd > total || state.winEnd <= state.winStart) {
      state.winEnd = total;
      state.winStart = Math.max(0, total - DOM_CAP_STEP);
    }
    if (state.winEnd - state.winStart > DOM_CAP_STEP) {
      state.winStart = state.winEnd - DOM_CAP_STEP;
    }

    updateTimelineBanners(tl);
    const newerBtn = $("#timeline-newer", tl);
    for (let i = state.winStart; i < state.winEnd; i++) {
      renderMessage(state.order[i], true, true, newerBtn);
    }
    updateTimelineBanners(tl);

    if (shouldPinBottom) {
      pendingRestoreScrollTop = null;
      suppressScrollStick = false;
      tl.scrollTop = tl.scrollHeight;
      recordTimelineScrollMetrics(tl);
      state.stickToBottom = true;
      state.unreadBelow = 0;
    } else {
      restoreTimelineScrollTop(priorTop);
    }
  } finally {
    isRenderingTimeline = false;
  }
  updateJumpBottomBtn();
}

export function updateTimelineBanners(tl = $("#timeline")) {
  if (!tl) return;
  let older = $("#timeline-more", tl);
  if (state.history.has_more) {
    if (!older) {
      older = el("button", "load-more-btn"); older.type = "button"; older.id = "timeline-more";
      older.onclick = loadOlderMessages; tl.prepend(older);
    }
    older.textContent = state.historyLoading ? "이전 대화 불러오는 중…" : "이전 대화 더 보기";
    older.disabled = state.historyLoading;
  } else if (older) older.remove();
  $("#history-banner").hidden = !(state.browsingHistory || state.pendingMessages);
  $("#history-status").textContent = "이전 대화 보는 중" + (state.pendingMessages ? " · 새 메시지 " + state.pendingMessages + "개" : "");
  updateJumpBottomBtn();
}

export function pruneMessageCaches() {
  const ids = new Set(state.order);
  for (const map of [state.traces, state.traceMeta, state.traceErrors]) for (const id of map.keys()) if (!ids.has(id)) map.delete(id);
  for (const set of [state.expanded, state.userCollapsed]) for (const id of set) if (!ids.has(id)) set.delete(id);
  for (const [id, controller] of state.traceControllers) if (!ids.has(id)) controller.abort();
}

async function loadOlderMessages() {
  if (state.historyLoading || !state.history.has_more || !state.order.length) return;
  const roomId = state.roomId; const sequence = openSeq;
  const before = state.messages.get(state.order[0]).seq;
  const tl = $("#timeline");
  const anchor = tl.querySelector(".msg");
  const anchorId = anchor && anchor.id;
  const anchorTop = anchor ? anchor.getBoundingClientRect().top : 0;
  state.historyLoading = true; state.browsingHistory = true; state.stickToBottom = false;
  updateTimelineBanners(tl);
  try {
    const page = await api("GET", "/api/rooms/" + roomId + "?before=" + before + "&limit=100");
    if (state.roomId !== roomId || openSeq !== sequence) return;
    for (const message of page.messages) if (!state.messages.has(message.id)) state.messages.set(message.id, message);
    const messages = [...state.messages.values()].sort((a, b) => a.seq - b.seq).slice(0, DOM_CAP_STEP);
    state.messages = new Map(messages.map((m) => [m.id, m]));
    state.order = messages.map((m) => m.id);
    state.history = page.history;
    state.winStart = 0; state.winEnd = messages.length;
    pruneMessageCaches();
    renderTimeline(false);
    const restored = anchorId && document.getElementById(anchorId);
    if (restored) {
      const nextTop = tl.scrollTop + (restored.getBoundingClientRect().top - anchorTop);
      restoreTimelineScrollTop(nextTop);
    }
    updateJumpBottomBtn();
    announce("이전 대화 " + page.messages.length + "개를 불러왔습니다. 최신 대화는 아래 버튼으로 돌아갈 수 있습니다.");
  } catch (err) {
    if (state.roomId === roomId && openSeq === sequence) announce("이전 대화 조회 실패: " + err.message);
  } finally {
    if (state.roomId === roomId && openSeq === sequence) { state.historyLoading = false; updateTimelineBanners(); }
  }
}

export async function jumpToLatestMessages() {
  if (!state.roomId) return;
  pendingRestoreScrollTop = null;
  suppressScrollStick = false;
  state.stickToBottom = true;
  state.unreadBelow = 0;
  try { await hooks.openRoom(state.roomId, true, true); }
  catch (err) { announce("최신 대화 조회 실패: " + err.message); }
}

function renderRoomEmptyGuide(tl) {
  const box = el("div", "room-empty-guide");
  box.id = "timeline-empty";
  const selected = activeParticipants().filter((p) => p.enabled);
  const openCodeOnly = selected.length > 0 && selected.every((p) => p.provider === "opencode");
  box.appendChild(el("h3", "", "이 방의 첫 대화를 시작해 보세요"));
  box.appendChild(
    el(
      "p",
      "",
      openCodeOnly
        ? "연결한 OpenCode 모델에게 질문하세요. 구독이나 연결을 바꿨다면 참가자 설정에서 연결·모델을 새로 불러올 수 있습니다."
        : "이름을 선택해 한 명에게 질문하거나, 멘션 없이 참여 중인 참가자 모두에게 질문할 수 있습니다. 아래 예시를 누르면 입력창에 채워집니다."
    )
  );
  const chips = el("div", "prompt-chips");
  const examples = [
    "이 저장소의 핵심 구조와 데이터 흐름을 각자 읽고 3줄로 요약해줘.",
    "최근 변경 사항에서 잠재적 버그나 경합 조건이 있는지 검토해줘.",
    "이 설계의 장단점을 분석하고, 여러 참가자가 함께한다면 서로의 의견도 검토해줘.",
  ];
  for (const ex of examples) {
    const b = el("button", "prompt-example", "“" + ex + "”");
    b.type = "button";
    b.onclick = () => {
      const inp = $("#input");
      if (inp) {
        inp.value = ex;
        inp.focus();
        updateHint();
      }
    };
    chips.appendChild(b);
  }
  box.appendChild(chips);
  tl.appendChild(box);
}

export function scheduleRender(id) {
  if (state.renderTimers.has(id) || document.hidden) return;
  state.renderTimers.set(
    id,
    setTimeout(() => {
      state.renderTimers.delete(id);
      renderMessage(id, false);
    }, STREAM_THROTTLE_MS)
  );
}

export function scheduleTraceRender(id) {
  if (state.traceTimers.has(id) || document.hidden) return;
  state.traceTimers.set(
    id,
    setTimeout(() => {
      state.traceTimers.delete(id);
      ensureAndRenderTrace(id);
    }, STREAM_THROTTLE_MS)
  );
}

function findPrevUserMessage(msgId) {
  const idx = state.order.indexOf(msgId);
  const start = idx >= 0 ? idx - 1 : state.order.length - 1;
  const m = state.messages.get(msgId);
  const agentId = m ? m.author : "";
  for (let i = start; i >= 0; i--) {
    const cand = state.messages.get(state.order[i]);
    if (cand && cand.kind === "user" && (!cand.target || cand.target === agentId)) {
      return cand;
    }
  }
  for (let i = start; i >= 0; i--) {
    const cand = state.messages.get(state.order[i]);
    if (cand && cand.kind === "user") return cand;
  }
  return null;
}

async function retryTurn(msgId, btn) {
  const m = state.messages.get(msgId);
  if (!m || !state.roomId) return;
  const prevUser = findPrevUserMessage(msgId);
  if (!prevUser || !prevUser.text) {
    alert("재요청할 직전 사용자 메시지를 찾지 못했습니다.");
    return;
  }
  if (btn) btn.disabled = true;
  try {
    await api("POST", "/api/rooms/" + state.roomId + "/messages", {
      text: prevUser.text,
      target: m.author,
    });
    announce(participantLabel(m.author) + "에게 직전 메시지로 다시 시도 요청함");
  } catch (e) {
    alert(e.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

export function renderMessage(id, isNew, batch, insertBeforeNode) {
  const m = state.messages.get(id);
  if (!m) return;
  const tl = $("#timeline");
  if (!tl) return;

  const emptyGuide = $("#timeline-empty");
  if (emptyGuide) emptyGuide.remove();

  const atBottom = !state.browsingHistory && state.stickToBottom;
  const total = state.order.length;
  const idx = state.order.indexOf(id);

  // Fix #2: Maintain strict [winStart, winEnd) window of max DOM_CAP_STEP (200)
  if (!batch && isNew) {
    const wasAtEnd = state.winEnd >= total - 1;
    if (atBottom || m.kind === "user") {
      state.winEnd = total;
      state.winStart = Math.max(0, state.winEnd - DOM_CAP_STEP);
    } else if (wasAtEnd && state.winEnd - state.winStart < DOM_CAP_STEP) {
      state.winEnd = total;
    } else {
      // User is scrolled up reading older messages and window is full (200 items):
      // Do not mount new bottom DOM node; update bottom banner instead.
      updateTimelineBanners(tl);
      return;
    }
  }

  if (idx !== -1 && (idx < state.winStart || idx >= state.winEnd)) {
    if (!batch) updateTimelineBanners(tl);
    return;
  }

  let node = $("#m-" + id);
  if (!node) {
    node = el("div");
    node.id = "m-" + id;
    const newerBtn = insertBeforeNode || $("#timeline-newer", tl);
    if (newerBtn) tl.insertBefore(node, newerBtn);
    else tl.appendChild(node);
  }
  node.className =
    "msg " +
    m.kind +
    (m.author && m.kind === "agent" ? " " + authorProvider(m.author) + seatClass(m.author) : "") +
    (m.status === "error" ? " error" : "");

  if (m.kind === "system") {
    const sysBadges = [];
    if (m.part) sysBadges.push('<span class="badge msg-part-badge">🧩 ' + esc(m.part) + "</span> ");
    if (m.review_step) {
      sysBadges.push(
        '<span class="badge msg-review-step-badge ' +
          esc(m.review_step) +
          '">' +
          esc(reviewStepLabel(m.review_step)) +
          "</span> "
      );
    }
    node.innerHTML =
      '<div class="bubble">' +
      sysBadges.join("") +
      "<span>ℹ " +
      esc(rewriteProseMentions(m.text, visibleMentions)) +
      '</span> <span class="muted">' +
      fmtTime(m.ts) +
      "</span></div>";
  } else {
    const who = m.kind === "user" ? "나" : participantLabel(m.author);
    const meta = [];
    meta.push('<span class="who ' + esc(authorProvider(m.author)) + '">' + esc(who) + "</span>");
    if (m.kind === "user" && m.target) {
      meta.push(
        '<span class="steer-tag">→ ' +
          esc(participantLabel(m.target)) +
          (m.live ? " (즉시 주입됨)" : " (다음 턴 전달)") +
          "</span>"
      );
    }
    if (m.model) meta.push('<span class="muted">' + esc(modelLabel(authorProvider(m.author), m.model)) + "</span>");
    if (m.effort) meta.push('<span class="muted">' + esc(m.effort) + "</span>");
    if (m.kind === "agent" && m.status !== "streaming" && m.duration_ms > 0) {
      meta.push('<span class="resp-time" title="' + esc(timingTitle(m)) + '">⏱ ' + esc(fmtElapsed(m.duration_ms)) + "</span>");
    }
    if (m.reason === "discussion") meta.push('<span class="badge">🗣 토론 ' + m.round + "R</span>");
    else if (m.reason === "mention") meta.push('<span class="badge">@호출</span>');
    else if (m.reason === "part") meta.push('<span class="badge">🔨 파트 작업</span>');
    else if (m.reason === "review") meta.push('<span class="badge">🔍 교차 리뷰</span>');
    if (m.part) {
      const pInfo = findWorkPart(m.part);
      const pTitleAttr = pInfo ? ' title="' + esc(pInfo.title) + '"' : "";
      meta.push('<span class="badge msg-part-badge"' + pTitleAttr + ">🧩 " + esc(m.part) + "</span>");
    }
    if (m.review_step) {
      meta.push(
        '<span class="badge msg-review-step-badge ' +
          esc(m.review_step) +
          '">' +
          esc(reviewStepLabel(m.review_step)) +
          "</span>"
      );
    }

    if (m.status === "streaming") {
      const act = activeTraceActivity(m.id, m.author);
      const elapsedStr = fmtElapsed(Date.now() - (m.ts || Date.now()));
      meta.push(
        '<span class="live-banner"><span class="spin" aria-hidden="true"></span><span>' +
          esc(act) +
          '</span> <span class="elapsed" data-msg-elapsed="' +
          esc(m.id) +
          '">⏱ ' +
          esc(elapsedStr) +
          "</span></span>"
      );
    } else if (m.status === "cancelled") {
      meta.push('<span class="badge">⏹ 중단됨</span>');
    } else if (m.status === "error") {
      meta.push('<span class="badge" style="color:var(--danger);border-color:var(--danger)">⚠ 실패</span>');
      if (isPartErrorResolved(m)) {
        meta.push('<span class="badge muted resolved-after-badge">이후 해결됨</span>');
      } else if (isPartErrorAbandoned(m)) {
        meta.push('<span class="badge muted abandoned-after-badge">파트 포기됨</span>');
      }
    }
    if (m.signal) {
      meta.push('<span class="signal ' + esc(m.signal) + '">' + esc(m.signal) + "</span>");
    }

    const live = m.kind === "agent" && m.status === "streaming" && !(m.text || "").trim() ? state.live.get(m.id) : null;
    if (m.status !== "streaming") state.live.delete(m.id);
    const agentPrepared = m.kind === "agent" ? prepareAgentMessageText(m.text, m) : { text: "", rejectedMarkers: [] };
    const agentDisplayText = agentPrepared.text;
    const hasVisibleAgentText = m.kind === "agent" && agentDisplayText.trim() !== "";
    const bodyKey =
      live
        ? "live:" + live.ver
        : m.kind === "agent"
        ? "agent:" + JSON.stringify([visibleMentions(agentDisplayText), agentPrepared.rejectedMarkers])
        : m.kind + ":" + visibleMentions(m.text);

    const foot = [];
    if (m.duration_ms) foot.push("⏱ " + fmtDur(m.duration_ms));
    const u = m.usage || {};
    const tin = u.input_tokens != null ? messageInputTokens(u) : null;
    if (tin != null) foot.push("입력 " + fmtK(tin) + " · 출력 " + fmtK(u.output_tokens));
    if (u.total_cost_usd) foot.push("$" + Number(u.total_cost_usd).toFixed(3));
    if (u.context_tokens) foot.push("ctx " + fmtK(u.context_tokens));

    let bubble = node.querySelector(".bubble");
    let isInitialMount = false;
    if (!bubble) {
      isInitialMount = true;
      node.innerHTML =
        '<div class="avatar ' +
        esc(authorProvider(m.author)) +
        '" aria-hidden="true">' +
        avatarContent(m.kind === "user" ? "user" : authorProvider(m.author), who) +
        "</div>" +
        '<div class="bubble">' +
        '<div class="meta"></div>' +
        '<div class="body"></div>' +
        '<div class="msg-attachments" hidden></div>' +
        '<div class="msg-requests" hidden></div>' +
        '<div class="err-slot"></div>' +
        '<div class="foot"></div>' +
        '<div class="trace" id="t-' +
        esc(id) +
        '" hidden></div>' +
        "</div>" +
        '<div class="msg-time"></div>';
      bubble = node.querySelector(".bubble");
    }
    node.querySelector(".msg-time").textContent = fmtTime(m.ts);

    bubble.querySelector(".meta").innerHTML = meta.join("");
    const bodyEl = bubble.querySelector(".body");
    bodyEl.hidden =
      (m.kind === "user" && !m.text) ||
      (m.kind === "agent" && !live && !hasVisibleAgentText && m.status !== "streaming");
    bodyEl.className = "body" + (m.status === "streaming" ? " cursor" : "");
    if (bodyEl.dataset.rawText !== bodyKey || isInitialMount) {
      bodyEl.dataset.rawText = bodyKey;
      bodyEl.innerHTML =
        m.kind === "user"
          ? m.text
            ? "<p>" + esc(rewriteProseMentions(m.text, visibleMentions)).replace(/\n/g, "<br>") + "</p>"
            : ""
          : live
          ? liveHtml(live)
          : m.kind === "agent"
          ? hasVisibleAgentText
            ? md(agentDisplayText)
            : ""
          : md(m.text);
      if (m.kind === "agent" && !live) {
        applyRejectedMarkerChips(bodyEl, agentPrepared.rejectedMarkers);
      }
      bodyEl.querySelectorAll("pre").forEach((preEl) => {
        const b = el("button", "copy", "복사");
        b.type = "button";
        b.setAttribute("aria-label", "코드 블록 복사");
        b.onclick = () => {
          const codeNode = preEl.querySelector("code");
          const raw = codeNode ? codeNode.innerText : preEl.innerText.replace(/^복사\n?/, "");
          navigator.clipboard.writeText(raw);
          b.textContent = "복사됨";
          setTimeout(() => {
            b.textContent = "복사";
          }, 1200);
        };
        preEl.appendChild(b);
      });
    }

    renderMessageAttachments(bubble.querySelector(".msg-attachments"), m);
    renderMessageRequests(bubble.querySelector(".msg-requests"), m);

    const errSlot = bubble.querySelector(".err-slot");
    errSlot.innerHTML = "";
    if (m.error || m.status === "error") {
      const errBox = el("div", "err-box");
      errBox.appendChild(el("span", "", "⚠ 원인: " + (m.error || "에이전트 실행 실패")));
      if (m.kind === "agent" && m.status === "error") {
        if (m.part || m.review_step) {
          const pBtn = el("button", "retry-btn part-retry-guide-btn", "파트 보드에서 재시도");
          pBtn.type = "button";
          pBtn.title = (m.part ? m.part + " " : "") + "파트 보드 카드로 이동하여 재시도합니다";
          pBtn.onclick = () => focusWorkPartCard(m.part);
          errBox.appendChild(pBtn);
        } else {
          const rBtn = el("button", "retry-btn", "↻ 다시 시도");
          rBtn.type = "button";
          rBtn.title = participantLabel(m.author) + "에게 직전 사용자 메시지를 다시 요청합니다";
          rBtn.onclick = () => retryTurn(m.id, rBtn);
          errBox.appendChild(rBtn);
        }
      }
      errSlot.appendChild(errBox);
    }

    const footEl = bubble.querySelector(".foot");
    let tb = footEl.querySelector(".trace-btn");
    let detail = footEl.querySelector(".message-detail");
    const focusedAction = footEl.contains(document.activeElement) ? document.activeElement : null;
    footEl.replaceChildren();
    if (m.kind === "agent") {
      const isExp = state.expanded.has(id);
      if (!tb) {
        tb = el("button", "trace-btn");
        tb.type = "button";
        tb.onclick = () => toggleTrace(id);
      }
      tb.textContent = (isExp ? "작업 기록 접기" : "작업 기록") + (m.trace ? " · " + m.trace : "");
      tb.setAttribute("aria-expanded", String(isExp));
      footEl.appendChild(tb);
      if (!detail) {
        detail = el("details", "message-detail");
        detail.appendChild(el("summary", "muted", "상세"));
        detail.appendChild(el("div", "message-metrics muted"));
        detail.appendChild(el("div", "message-identifiers muted mono"));
      }
      detail.querySelector(".message-metrics").textContent = foot.join(" · ");
      detail.querySelector(".message-identifiers").textContent = "모델: " + (m.model || "미확인") + " · 참가자: " + m.author + (m.session_id ? " · 세션: " + m.session_id : "");
      if (m.session_id) {
        let actions = detail.querySelector(".session-actions");
        if (!actions) { actions = buildSessionActions(m.author, m.id); detail.appendChild(actions); }
        const seat = participant(m.author);
        updateSessionActions(actions, m.session_id, m.status === "streaming" || (seat && seat.status === "running" && seat.session_id === m.session_id));
      }
      footEl.appendChild(detail);
    }
    if (focusedAction && footEl.contains(focusedAction)) focusedAction.focus({ preventScroll: true });

    // Fix #5: Only trigger trace render on initial mount if expanded (not on every text delta!)
    const traceBox = $("#t-" + id, bubble);
    if (state.expanded.has(id) && traceBox && (isInitialMount || traceBox.hidden)) {
      ensureAndRenderTrace(id);
    }
  }

  // Fix #2: Always enforce DOM_CAP_STEP (200) regardless of atBottom
  if (!batch) {
    const msgNodes = tl.querySelectorAll(".msg");
    if (msgNodes.length > DOM_CAP_STEP) {
      const excess = msgNodes.length - DOM_CAP_STEP;
      for (let i = 0; i < excess; i++) {
        msgNodes[i].remove();
      }
    }
    updateTimelineBanners(tl);
  }

  if (!batch && (atBottom || (isNew && m.kind === "user"))) {
    pendingRestoreScrollTop = null;
    suppressScrollStick = false;
    tl.scrollTop = tl.scrollHeight;
    recordTimelineScrollMetrics(tl);
    state.stickToBottom = true;
    state.unreadBelow = 0;
  }
  if (!batch) updateJumpBottomBtn();
}

export function cancelPendingScrollRestore() {
  pendingRestoreScrollTop = null;
  suppressScrollStick = false;
}

export function bindTimeline() {
  const timelineEl = $("#timeline");
  if (timelineEl) {
    timelineEl.addEventListener("wheel", cancelPendingScrollRestore, { passive: true });
    timelineEl.addEventListener("touchmove", cancelPendingScrollRestore, { passive: true });
    timelineEl.addEventListener("pointerdown", cancelPendingScrollRestore, { passive: true });
    timelineEl.addEventListener("keydown", cancelPendingScrollRestore);
    timelineEl.addEventListener("load", onTimelineImageLoad, true);
    timelineEl.addEventListener(
      "scroll",
      () => {
        if (isRenderingTimeline || suppressScrollStick || pendingRestoreScrollTop !== null || document.hidden) return;
        if (!state.browsingHistory && state.stickToBottom) {
          const heightGrew = timelineEl.scrollHeight > lastTimelineScrollHeight;
          const scrolledUp = timelineEl.scrollTop < lastTimelineScrollTop - 4;
          if (heightGrew && !scrolledUp) {
            suppressScrollStick = true;
            timelineEl.scrollTop = timelineEl.scrollHeight;
            recordTimelineScrollMetrics(timelineEl);
            suppressScrollStick = false;
            updateJumpBottomBtn();
            return;
          }
        }
        const atBottom = !state.browsingHistory && isTimelineAtBottom(timelineEl);
        state.stickToBottom = atBottom;
        if (atBottom) state.unreadBelow = 0;
        recordTimelineScrollMetrics(timelineEl);
        updateJumpBottomBtn();
      },
      { passive: true }
    );
  }
}
