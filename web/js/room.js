// Opening a room, the SSE stream and applying its events to state.
import { $, announce, DOM_CAP_STEP, STREAM_THROTTLE_MS, TRACE_EVENT_LIMIT } from "./util.js";
import { api } from "./api.js";
import { nextOpenSeq, openSeq, participantLabel, resetParticipantNames, state } from "./state.js";
import { refreshLimits, renderUsagePanel } from "./usage.js";
import { clearComposerError, clearDraftAttachments, closeLightbox, collectMessageRequests, mergeSnapshotRequests, renderRequests } from "./attachments.js";
import { renderTargetControls, updateHint } from "./composer.js";
import { closeDiffDrawer, refreshPartErrorMessages, renderWork } from "./work.js";
import { renderRoomHead, renderRoomList, scheduleRoomList } from "./rooms.js";
import { renderDiscussion, renderParticipants, renderQueue } from "./participants.js";
import { boundedTraceEvent, cacheTrace, expandTrace, liveFeed, seedLive, updateTraceCount } from "./trace.js";
import { cancelPendingScrollRestore, pruneMessageCaches, renderMessage, renderTimeline, scheduleRender, scheduleTraceRender, updateJumpBottomBtn, updateTimelineBanners } from "./timeline.js";

let panelTimer = null;
export function schedulePanels() {
  if (panelTimer || document.hidden) return;
  panelTimer = setTimeout(() => {
    panelTimer = null;
    if (!state.room || document.hidden) return;
    renderParticipants(); renderTargetControls(); renderUsagePanel(); renderRequests(); renderQueue(); renderDiscussion(); renderWork(); updateHint();
  }, STREAM_THROTTLE_MS);
}

// ---------- room ----------
let activeSyncUpdatedReqIds = null;
export async function openRoom(id, resync, latest = false) {
  const preserveHistory = !!(resync && !latest && state.roomId === id && state.browsingHistory && state.order.length);
  const before = preserveHistory ? state.messages.get(state.order[state.order.length - 1]).seq + 1 : 0;
  const my = nextOpenSeq();
  if (!resync) {
    if (state.es) {
      state.es.close();
      state.es = null;
    }
    state.roomId = id;
    state.target = "";
    state.work = null;
    state.workRevision = 0;
    state.requests.clear();
    state.partNotices.clear();
    state.partReviewOpen.clear();
    state.workBoardCollapsed = false;
    state.stickToBottom = true;
    state.unreadBelow = 0;
    cancelPendingScrollRestore();
    closeDiffDrawer();
    closeLightbox();
    clearDraftAttachments();
    state.draftMentions.clear();
    clearComposerError();
    state.expanded.clear();
    state.userCollapsed.clear();
    localStorage.setItem("rt.room", id);
    const pbox = $("#participants");
    if (pbox) pbox.innerHTML = "";
  }
  const workRev = state.workRevision;
  const updatedReqIds = new Set();
  activeSyncUpdatedReqIds = updatedReqIds;
  const snap = await api("GET", "/api/rooms/" + id + (preserveHistory ? "?before=" + before + "&limit=200" : ""));
  if (activeSyncUpdatedReqIds === updatedReqIds) activeSyncUpdatedReqIds = null;
  if (my !== openSeq || state.roomId !== id) return;

  const tl = $("#timeline");
  const priorTop = tl ? tl.scrollTop : 0;
  const shouldScrollBottom = !resync || latest || (state.stickToBottom && !preserveHistory);

  state.messages.clear();
  state.order = [];
  for (const timer of state.renderTimers.values()) clearTimeout(timer);
  for (const timer of state.traceTimers.values()) clearTimeout(timer);
  state.renderTimers.clear();
  state.traceTimers.clear();
  state.traces.clear();
  state.traceMeta.clear();
  state.tracePending.clear();
  for (const controller of state.traceControllers.values()) controller.abort();
  state.traceControllers.clear();
  state.traceFetching.clear();
  state.traceErrors.clear();
  state.live.clear();
  for (const m of snap.messages || []) {
    if (m.kind === "agent" && m.status === "streaming" && !(m.text || "").trim()) seedLive(id, m.id);
  }
  state.room = snap.room;
  if (state.workRevision === workRev) {
    state.workRevision++;
    state.work = snap.work;
  }
  state.requests = mergeSnapshotRequests(state.requests, snap.requests, updatedReqIds);
  state.history = snap.history;
  state.browsingHistory = preserveHistory;
  state.historyLoading = false;
  const prevLatestSeq = state.latestSeq;
  const seqDelta = prevLatestSeq > 0 ? Math.max(0, (snap.room && snap.room.seq ? snap.room.seq : 0) - prevLatestSeq) : 0;
  const userNewInSnap = prevLatestSeq > 0 ? (snap.messages || []).filter((m) => m.seq > prevLatestSeq && m.kind === "user").length : 0;
  const msgNewNonUser = prevLatestSeq > 0 ? (snap.messages || []).filter((m) => m.seq > prevLatestSeq && m.kind !== "user").length : 0;
  const newUnreadInResync = Math.max(seqDelta - userNewInSnap, msgNewNonUser);
  state.pendingMessages = preserveHistory ? state.pendingMessages + seqDelta : 0;
  if (shouldScrollBottom) {
    cancelPendingScrollRestore();
    state.stickToBottom = true;
    state.unreadBelow = 0;
  } else if (resync && !preserveHistory) {
    state.unreadBelow += newUnreadInResync;
  }
  state.latestSeq = snap.room.seq;
  state.queue = snap.queue || [];
  state.discussion = snap.discussion;
  for (const m of snap.messages || []) {
    state.messages.set(m.id, m);
    state.order.push(m.id);
    if (
      state.autoTrace &&
      !state.userCollapsed.has(m.id) &&
      m.kind === "agent" &&
      (m.status === "streaming" || m.trace > 0)
    ) {
      expandTrace(m.id);
    }
  }
  state.winEnd = state.order.length;
  state.winStart = Math.max(0, state.winEnd - DOM_CAP_STEP);

  $("#empty").hidden = true;
  const roomSec = $("#room");
  roomSec.hidden = false;
  roomSec.dataset.mode = state.room.mode || "read";

  renderRoomHead();
  renderWork();
  pruneMessageCaches();
  renderTimeline(shouldScrollBottom, priorTop);
  renderParticipants();
  renderRequests();
  renderQueue();
  renderDiscussion();
  renderRoomList();
  updateHint();
  refreshLimits(false, true);
  if (!resync) connect(id);
}

export function connect(id) {
  const es = new EventSource("/api/rooms/" + id + "/stream");
  state.es = es;
  state.lastEvent = Date.now();
  es.onmessage = (ev) => {
    if (state.es !== es || state.roomId !== id) return;
    state.lastEvent = Date.now();
    let msg;
    try {
      msg = JSON.parse(ev.data);
    } catch {
      return;
    }
    handle(msg.type, msg.data);
  };
  let connected = false;
  es.onerror = () => {
    if (state.es !== es || state.roomId !== id) return;
    const hint = $("#composer-hint");
    if (hint) {
      hint.className = "composer-hint warn";
      hint.textContent = "⚠ 서버와 SSE 연결이 끊겼습니다 — 재연결 시도 중…";
    }
  };
  es.addEventListener("hello", () => {
    if (state.es !== es || state.roomId !== id) return;
    updateHint();
    if (connected) openRoom(id, true);
    connected = true;
  });
}

function handle(type, data) {
  switch (type) {
    case "message":
      upsertMessage(data);
      break;
    case "request":
      upsertRequest(data);
      break;
    case "delta": {
      const m = state.messages.get(data.id);
      if (!m) return;
      m.text = (m.text || "") + (data.block && m.text ? "\n\n" : "") + data.text;
      scheduleRender(data.id);
      break;
    }
    case "trace": {
      const m = state.messages.get(data.msg_id);
      if (m) m.trace = Math.max(m.trace || 0, data.index);
      const pending = state.tracePending.get(data.msg_id);
      if (pending) {
        pending.set(data.index, boundedTraceEvent(data.event));
        if (pending.size > TRACE_EVENT_LIMIT) pending.delete(pending.keys().next().value);
      }
      if (state.traces.has(data.msg_id)) {
        const list = state.traces.get(data.msg_id);
        const metadata = state.traceMeta.get(data.msg_id);
        if (data.index > metadata.total) cacheTrace(data.msg_id, [...list, data.event], data.index);
      }
      if (m && m.status === "streaming") liveFeed(m.id, data.event);
      if (m) {
        // Fix #4: Only auto-expand if the user has not manually collapsed this message's trace
        if (state.autoTrace && !state.userCollapsed.has(m.id) && !state.expanded.has(m.id)) {
          expandTrace(m.id);
        }
        updateTraceCount(m.id);
        if (state.expanded.has(m.id)) {
          scheduleTraceRender(m.id);
        }
        if (m.status === "streaming") scheduleRender(m.id);
      }
      break;
    }
    case "participant": {
      if (!state.room) break;
      state.roomStateRevision++;
      const idx = state.room.participants.findIndex((p) => p.id === data.id);
      if (idx >= 0) state.room.participants[idx] = data;
      else state.room.participants.push(data);
      resetParticipantNames();
      schedulePanels();
      for (const id of state.order.slice(state.winStart, state.winEnd)) {
        const m = state.messages.get(id);
        if (!m) continue;
        const hasAttBySeat = Array.isArray(m.attachments) && m.attachments.some((a) => a && a.by === data.id);
        const hasReqSeat = collectMessageRequests(m).some((r) => r.from === data.id || r.to === data.id);
        if (m.author === data.id || hasAttBySeat || hasReqSeat) scheduleRender(id);
      }
      break;
    }
    case "queue":
      state.roomStateRevision++;
      state.queue = data || [];
      schedulePanels();
      scheduleRoomList();
      break;
    case "discussion":
      state.discussion = data;
      schedulePanels();
      break;
    case "work":
      state.roomStateRevision++;
      state.workRevision++;
      state.work = data;
      renderRoomHead();
      renderWork();
      refreshPartErrorMessages();
      break;
    case "limits":
      refreshLimits(false, true);
      break;
    case "room":
      state.roomStateRevision++;
      state.room = data;
      if ($("#room")) $("#room").dataset.mode = data.mode || "read";
      renderRoomHead();
      renderWork();
      renderParticipants();
      renderRequests();
      renderQueue();
      renderDiscussion();
      for (const id of state.order.slice(state.winStart, state.winEnd)) scheduleRender(id);
      updateHint();
      scheduleRoomList();
      break;
  }
}

function upsertMessage(m) {
  const prev = state.messages.get(m.id);
  const existed = !!prev;
  const isNewSequence = m.seq > state.latestSeq;
  if (isNewSequence) { state.latestSeq = m.seq; state.history.total++; }
  if (!existed && m.kind === "user" && !state.browsingHistory) {
    state.stickToBottom = true;
    state.unreadBelow = 0;
  }
  const atBottomNow = !state.browsingHistory && state.stickToBottom;
  if (!existed && !state.browsingHistory && state.order.length >= DOM_CAP_STEP && !atBottomNow) state.browsingHistory = true;
  if (!existed && (state.browsingHistory || (!isNewSequence && state.order.length))) {
    if (isNewSequence) state.pendingMessages++;
    updateTimelineBanners();
    updateJumpBottomBtn();
    scheduleRoomList();
    return;
  }
  if (!existed && isNewSequence && m.kind !== "user" && !atBottomNow) {
    state.unreadBelow++;
  }
  state.messages.set(m.id, m);
  if (!existed) {
    state.order.push(m.id);
    if (state.order.length > DOM_CAP_STEP) {
      const removed = state.order.shift();
      state.messages.delete(removed);
      $("#m-" + removed)?.remove();
      state.winStart = Math.max(0, state.winStart - 1);
      state.winEnd = Math.max(0, state.winEnd - 1);
      state.history.has_more = true;
      state.history.before_seq = state.messages.get(state.order[0]).seq;
      pruneMessageCaches();
    }
    if (state.autoTrace && m.kind === "agent" && !state.userCollapsed.has(m.id)) {
      expandTrace(m.id);
    }
  }
  if (!document.hidden) renderMessage(m.id, !existed);
  if (m.status === "done" && (m.part || m.review_step)) {
    refreshPartErrorMessages(m.part || "");
  }
  if (state.room) schedulePanels();
  if (!existed || prev.status !== m.status) scheduleRoomList();

  if (m.kind === "system" && !existed) {
    announce("시스템 알림: " + m.text);
  } else if (m.kind === "user" && !existed) {
    announce("내 메시지 전송됨");
  } else if (m.kind === "agent" && m.status !== "streaming" && (!prev || prev.status === "streaming")) {
    const who = participantLabel(m.author);
    if (m.status === "error") announce(who + " 턴 오류 발생: " + (m.error || ""));
    else if (m.status === "cancelled") announce(who + " 턴 중단됨");
    else announce(who + " 답변 완료");
  }
}

function upsertRequest(req) {
  if (!req || !req.id) return;
  if (activeSyncUpdatedReqIds) activeSyncUpdatedReqIds.add(req.id);
  state.requests.set(req.id, req);
  if (req.source_msg) {
    const srcMsg = state.messages.get(req.source_msg);
    if (srcMsg) {
      if (!Array.isArray(srcMsg.requests)) {
        srcMsg.requests = [req.id];
      } else if (!srcMsg.requests.includes(req.id)) {
        srcMsg.requests.push(req.id);
      }
      if (!document.hidden) renderMessage(req.source_msg, false);
    }
  }
  for (const id of state.order.slice(state.winStart, state.winEnd)) {
    if (id === req.source_msg) continue;
    const m = state.messages.get(id);
    if (m && Array.isArray(m.requests) && m.requests.includes(req.id)) {
      if (!document.hidden) renderMessage(id, false);
    }
  }
  if (!document.hidden) renderRequests();
}
