/* Roundtable client: one page, SSE-driven, no framework. */
(() => {
  "use strict";

  const DOM_CAP_STEP = 200;
  const STREAM_THROTTLE_MS = 125; // max 8 renders/sec per message/trace
  const TRACE_EVENT_LIMIT = 200;
  const TRACE_CACHE_LIMIT = 8;

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
  const el = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  };
  const esc = (s) =>
    String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const fmtK = (n) => {
    n = Number(n) || 0;
    return n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1000 ? (n / 1000).toFixed(1) + "k" : String(Math.round(n));
  };
  const fmtTime = (ms) =>
    ms ? new Date(ms).toLocaleTimeString("ko-KR", { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "";
  const fmtDur = (ms) => (ms >= 60000 ? (ms / 60000).toFixed(1) + "분" : (ms / 1000).toFixed(1) + "초");
  // Each seat gets its own colour (8 slots, by position in the room) so a long
  // reply can be attributed at a glance even when two seats share a provider.
  const SEAT_COLORS = 8;
  function seatClass(pid) {
    const ps = (state.room && state.room.participants) || [];
    const i = ps.findIndex((p) => p.id === pid);
    return i < 0 ? "" : " seat-" + (i % SEAT_COLORS);
  }
  // Recent and average response time of one seat, from the messages loaded in this view.
  function seatTimes(pid) {
    const ds = [];
    for (const id of state.order) {
      const m = state.messages.get(id);
      if (m && m.kind === "agent" && m.author === pid && m.status === "done" && m.duration_ms > 0) ds.push(m.duration_ms);
    }
    if (!ds.length) return null;
    return { last: ds[ds.length - 1], avg: ds.reduce((a, b) => a + b, 0) / ds.length, n: ds.length };
  }
  const fmtElapsed = (ms) => {
    const sec = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
    const m = Math.floor(sec / 60);
    const s = sec % 60;
    return m > 0 ? m + "분 " + String(s).padStart(2, "0") + "초" : s + "초";
  };
  const fmtReset = (sec) => {
    if (!sec) return "";
    const diff = sec * 1000 - Date.now();
    if (diff <= 0) return "곧 리셋";
    const mins = Math.round(diff / 60000);
    if (mins < 60) return mins + "분 후";
    const h = Math.round(mins / 60);
    return h >= 24 ? Math.round(h / 24) + "일 후" : h + "시간 후";
  };

  const LABEL = { claude: "Claude", codex: "Codex", agy: "Antigravity", opencode: "OpenCode", user: "나", system: "시스템" };

  // ---------- Safe Markdown Rendering (Fix P1 #1: XSS & file:// handling) ----------
  function decodeHtmlEntities(str) {
    return String(str ?? "").replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (_, ent) => {
      if (ent[0] === "#") {
        const code = ent[1].toLowerCase() === "x" ? parseInt(ent.slice(2), 16) : parseInt(ent.slice(1), 10);
        return Number.isFinite(code) && code >= 0 && code <= 0x10ffff ? String.fromCodePoint(code) : "";
      }
      const map = { colon: ":", tab: "\t", newline: "\n", amp: "&", lt: "<", gt: ">", quot: '"', apos: "'" };
      return map[ent.toLowerCase()] || "";
    });
  }

  function classifyMarkdownUrl(rawHref) {
    const raw = String(rawHref ?? "").trim();
    if (!raw) return { kind: "unsafe", url: "" };
    const decoded = decodeHtmlEntities(raw).replace(/[\u0000-\u0020\u007f-\u009f]+/g, "");
    if (!decoded) return { kind: "unsafe", url: "" };
    if (/^file:/i.test(decoded)) {
      return { kind: "file", url: raw };
    }
    if (decoded.startsWith("//")) {
      return { kind: "unsafe", url: raw };
    }
    const schemeMatch = decoded.match(/^([a-zA-Z][a-zA-Z0-9+.-]*):/);
    if (schemeMatch) {
      const proto = schemeMatch[1].toLowerCase();
      if (proto === "http" || proto === "https" || proto === "mailto") {
        return { kind: "safe", proto, url: raw };
      }
      return { kind: "unsafe", url: raw };
    }
    if (
      decoded.startsWith("#") ||
      decoded.startsWith("/") ||
      decoded.startsWith("./") ||
      decoded.startsWith("../") ||
      !decoded.includes(":")
    ) {
      return { kind: "safe", proto: "relative", url: raw };
    }
    return { kind: "unsafe", url: raw };
  }

  if (typeof marked !== "undefined") {
    marked.use({
      gfm: true,
      breaks: true,
      renderer: {
        html({ text }) {
          return esc(text);
        },
        link(token) {
          const href = token && token.href != null ? String(token.href) : "";
          const text = token && token.text != null ? String(token.text) : "";
          const title = token && token.title != null ? String(token.title) : "";
          const check = classifyMarkdownUrl(href);
          if (check.kind === "file") {
            // Agents cite local files as file:///abs/path#L10-L20. A browser cannot open
            // them, and the absolute path drowns the sentence: show the agent's label
            // (or file name + lines) and keep the full path in the tooltip.
            const pathTxt = decodeURIComponent(href.replace(/^file:\/\/\/?/i, ""));
            const m = pathTxt.match(/^(.*?)(?:#L(\d+)(?:-L(\d+))?)?$/);
            const file = m[1].split(/[\\/]/).pop();
            const lines = m[2] ? ":" + m[2] + (m[3] ? "-" + m[3] : "") : "";
            const label = text.trim() && text.trim() !== href ? text.trim() : file + lines;
            return '<code class="file-path" title="' + esc(pathTxt) + '">' + esc(label) + "</code>";
          }
          if (check.kind !== "safe") {
            return esc(text || href);
          }
          const labelHtml =
            this && this.parser && Array.isArray(token.tokens)
              ? this.parser.parseInline(token.tokens)
              : esc(text || href);
          const titleAttr = title ? ' title="' + esc(title) + '"' : "";
          const extAttr = !href.trim().startsWith("#") ? ' target="_blank" rel="noopener noreferrer"' : "";
          return '<a href="' + esc(href.trim()) + '"' + titleAttr + extAttr + ">" + labelHtml + "</a>";
        },
        image(token) {
          const href = token && token.href != null ? String(token.href) : "";
          const text = token && token.text != null ? String(token.text) : "";
          const title = token && token.title != null ? String(token.title) : "";
          const check = classifyMarkdownUrl(href);
          if (check.kind !== "safe" || check.proto === "mailto") {
            return esc(text ? "[이미지: " + text + "]" : href);
          }
          const titleAttr = title ? ' title="' + esc(title) + '"' : "";
          return '<img src="' + esc(href.trim()) + '" alt="' + esc(text) + '"' + titleAttr + ' loading="lazy">';
        },
      },
    });
  }

  function friendlyMarkdownTokens(tokens) {
    for (const token of tokens) {
      if (["code", "codespan", "html", "link", "image", "def"].includes(token.type)) continue;
      if (token.tokens) friendlyMarkdownTokens(token.tokens);
      else if (token.type === "text") { token.text = visibleMentions(token.text); token.escaped = false; }
      if (token.items) for (const item of token.items) if (item.tokens) friendlyMarkdownTokens(item.tokens);
      if (token.header) for (const cell of token.header) friendlyMarkdownTokens(cell.tokens);
      if (token.rows) for (const row of token.rows) for (const cell of row) friendlyMarkdownTokens(cell.tokens);
    }
  }

  const md = (text) => {
    try {
      if (typeof marked === "undefined") return "<p>" + esc(text) + "</p>";
      const tokens = marked.lexer(text || "");
      friendlyMarkdownTokens(tokens);
      return marked.parser(tokens);
    } catch {
      return "<p>" + esc(text) + "</p>";
    }
  };

  const api = async (method, url, body, signal) => {
    const res = await fetch(url, {
      method,
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
      signal,
    });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
  };

  const state = {
    providers: [],
    limits: {},
    config: {},
    rooms: [],
    roomSearch: "",
    roomId: null,
    room: null,
    messages: new Map(),
    order: [],
    winStart: 0,
    winEnd: 0,
    queue: [],
    discussion: null,
    traces: new Map(),
    live: new Map(), // msgId -> live progress of a streaming reply (subagents, tools)
    traceFetching: new Set(),
    traceErrors: new Map(),
    expanded: new Set(),
    userCollapsed: new Set(),
    autoTrace: localStorage.getItem("rt.autoTrace") === "1",
    target: "",
    mentionCands: [],
    mentionIdx: 0,
    es: null,
    renderTimers: new Map(),
    traceTimers: new Map(),
    lastEvent: 0,
    draftMentions: new Map(),
    usageSources: {},
    usageBusy: false,
    usageError: "",
    usageChecked: 0,
    history: { has_more: false, before_seq: 0, total: 0 },
    browsingHistory: false,
    historyLoading: false,
    latestSeq: 0,
    pendingMessages: 0,
    traceMeta: new Map(),
    tracePending: new Map(),
    traceControllers: new Map(),
    catalogRevision: 0,
    roomStateRevision: 0,
  };

  const activeParticipants = () => state.room ? state.room.participants.filter((p) => !p.removed) : [];
  const participant = (id) => state.room && state.room.participants.find((p) => p.id === id);
  let namesRoom = null;
  let participantNames = new Map();
  const participantLabel = (id) => {
    const p = participant(id);
    if (!p) return LABEL[id] || id;
    if (namesRoom !== state.room) {
      const reserved = new Set(state.room.participants.map((seat) => seat.label));
      const counts = new Map();
      for (const seat of state.room.participants) counts.set(seat.label, (counts.get(seat.label) || 0) + 1);
      participantNames = new Map();
      for (const seat of state.room.participants) {
        let name = seat.label;
        if (counts.get(name) > 1) {
          let suffix = 1;
          while (reserved.has(name + " · " + suffix)) suffix++;
          name += " · " + suffix;
          reserved.add(name);
        }
        participantNames.set(seat.id, name);
      }
      namesRoom = state.room;
    }
    return participantNames.get(id);
  };
  function modelLabel(provider, model) {
    const info = state.providers.find((p) => p.id === provider);
    const option = info && info.models && info.models.find((m) => m.id === model);
    return option ? option.label : "직접 지정 모델";
  }
  function catalogMessage(info) {
    if (info.catalog_status === "loading") return "모델 목록을 불러오는 중입니다. 잠시 뒤 목록을 다시 불러오세요.";
    if (info.catalog_status === "unavailable") return "모델 목록을 가져오지 못했습니다." + (info.catalog_error ? " " + info.catalog_error : "");
    if (info.catalog_status === "ready" && (!info.models || !info.models.length)) return "사용할 수 있는 모델이 없습니다. OpenCode의 연결 설정을 확인하세요.";
    return "";
  }
  let catalogLoading = null;
  async function refreshProviderCatalog(button, afterRefresh) {
    button.disabled = true;
    try {
      if (!catalogLoading) catalogLoading = api("GET", "/api/providers?refresh=true").then((result) => {
        state.providers = result.providers; state.catalogRevision++;
        renderProviderStatus();
      }).finally(() => { catalogLoading = null; });
      await catalogLoading;
      afterRefresh();
      if (state.room) renderParticipants();
    } catch (err) { alert("모델 목록을 불러오지 못했습니다: " + err.message); }
    finally { button.disabled = false; }
  }
  function visibleMentions(text) {
    return String(text || "").replace(/(^|[\s([{])@([a-zA-Z0-9_-]+)/g, (match, prefix, id) => {
      if (LABEL[id] && id !== "user" && id !== "system") return prefix + "@" + LABEL[id] + " 전체";
      return participant(id) ? prefix + "@" + participantLabel(id) : match;
    });
  }
  function rewriteProseMentions(text, rewrite) {
    const ranges = [];
    const fences = /^(?:[ \t]*> ?)*[ \t]*(?:[-+*] |\d+[.)] )?(`{3,}|~{3,})[^\n]*(?:\n|$)/gm;
    let match;
    while ((match = fences.exec(text))) {
      const marker = match[1];
      const closing = new RegExp("^(?:[ \\t]*> ?)*[ \\t]*" + marker[0] + "{" + marker.length + ",}[ \\t]*(?:\\n|$)", "gm");
      closing.lastIndex = fences.lastIndex;
      const end = closing.exec(text);
      const stop = end ? closing.lastIndex : text.length;
      ranges.push([match.index, stop]); fences.lastIndex = stop;
    }
    const literal = /^(?: {4}|\t)[^\n]*(?:\n|$)|^ {0,3}\[[^\]\n]+\]:[^\n]*(?:\n|$)|<!--[^]*?-->|<(?:[^>"']|"[^"]*"|'[^']*')+>|(?:[a-zA-Z][a-zA-Z0-9+.-]*:\/\/|mailto:|\.{1,2}\/|\/)[^\s<>"`]+/gm;
    while ((match = literal.exec(text))) ranges.push([match.index, literal.lastIndex]);
    let linkStart = text.indexOf("](");
    while (linkStart >= 0) {
      let depth = 1; let end = linkStart + 2; let quote = "";
      for (; end < text.length && depth; end++) {
        const char = text[end];
        if (char === "\\") { end++; continue; }
        if (quote) { if (char === quote) quote = ""; continue; }
        if (char === '"' || char === "'") { quote = char; continue; }
        if (char === "(") depth++;
        else if (char === ")") depth--;
      }
      if (!depth) ranges.push([linkStart + 2, end - 1]);
      linkStart = text.indexOf("](", linkStart + 2);
    }
    const ticks = /`+/g;
    while ((match = ticks.exec(text))) {
      const protectedRange = ranges.find(([start, end]) => match.index >= start && match.index < end);
      if (protectedRange) { ticks.lastIndex = protectedRange[1]; continue; }
      const length = match[0].length;
      const closing = /`+/g; closing.lastIndex = ticks.lastIndex;
      let end;
      while ((end = closing.exec(text))) {
        const protectedClose = ranges.find(([start, stop]) => end.index >= start && end.index < stop);
        if (protectedClose) { closing.lastIndex = protectedClose[1]; continue; }
        if (end[0].length === length) break;
      }
      if (end) { ranges.push([match.index, closing.lastIndex]); ticks.lastIndex = closing.lastIndex; }
    }
    ranges.sort((a, b) => a[0] - b[0]);
    let result = ""; let cursor = 0;
    for (const [start, end] of ranges) {
      if (end <= cursor) continue;
      if (start > cursor) result += rewrite(text.slice(cursor, start));
      result += text.slice(Math.max(cursor, start), end); cursor = end;
    }
    return result + rewrite(text.slice(cursor));
  }
  function encodedMentions(text, strict = true) {
    return rewriteProseMentions(text, (prose) => prose.replace(/@\[([^\]\n]+)\]/g, (match, name) => {
      const mapped = state.draftMentions.get(match);
      const candidates = activeParticipants().filter((p) => mapped ? p.id === mapped : participantLabel(p.id) === name);
      if (candidates.length !== 1 || !candidates[0].enabled) {
        if (strict) throw new Error("'" + name + "' 대상을 확인할 수 없습니다. @ 목록에서 참여 중인 이름을 다시 선택하세요.");
        return match;
      }
      return "@" + candidates[0].id;
    }));
  }
  const authorProvider = (id) => {
    const p = participant(id);
    return p ? p.provider : id;
  };

  function announce(msg) {
    const box = $("#live-announcer");
    if (!box || !msg) return;
    box.textContent = "";
    setTimeout(() => {
      box.textContent = msg;
    }, 20);
  }

  // ---------- theme ----------
  function initTheme() {
    const saved = localStorage.getItem("rt.theme");
    if (saved === "light" || saved === "dark") {
      document.documentElement.dataset.theme = saved;
    }
    updateThemeBtn();
  }

  function currentTheme() {
    const explicit = document.documentElement.dataset.theme;
    if (explicit === "light" || explicit === "dark") return explicit;
    return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
  }

  function toggleTheme() {
    const next = currentTheme() === "dark" ? "light" : "dark";
    document.documentElement.dataset.theme = next;
    localStorage.setItem("rt.theme", next);
    updateThemeBtn();
    announce(next === "light" ? "라이트 테마로 전환됨" : "다크 테마로 전환됨");
  }

  function updateThemeBtn() {
    const btn = $("#btn-theme");
    if (!btn) return;
    const isLight = currentTheme() === "light";
    btn.textContent = isLight ? "☀️" : "🌙";
    btn.setAttribute("aria-label", isLight ? "다크 테마로 전환" : "라이트 테마로 전환");
    btn.title = isLight ? "현재 라이트 테마 (클릭하여 다크 테마로 전환)" : "현재 다크 테마 (클릭하여 라이트 테마로 전환)";
  }

  // ---------- boot ----------
  async function boot() {
    initTheme();
    const autoChk = $("#chk-auto-trace");
    if (autoChk) autoChk.checked = state.autoTrace;

    const [prov, cfg] = await Promise.all([api("GET", "/api/providers"), api("GET", "/api/config")]);
    state.providers = prov.providers || [];
    state.limits = prov.limits || {};
    state.config = cfg || {};
    renderProviderStatus();
    renderUsagePanel();
    await loadRooms();
    const last = localStorage.getItem("rt.room");
    if (last && state.rooms.some((r) => r.id === last)) {
      await openRoom(last);
    }
    bindUI();
    resumeBackground();
    document.addEventListener("visibilitychange", () => {
      stopBackground();
      if (!document.hidden) {
        resumeBackground();
        refreshLimits();
        if (state.room) { renderTimeline(false); schedulePanels(); }
      }
    });
    window.addEventListener("pagehide", () => { stopBackground(); if (state.es) state.es.close(); });
  }

  let usageTimer = null;
  let watchTimer = null;
  let elapsedTimer = null;
  function stopBackground() {
    clearTimeout(usageTimer); clearInterval(watchTimer); clearInterval(elapsedTimer);
    for (const controller of state.traceControllers.values()) controller.abort();
  }
  function resumeBackground() {
    if (document.hidden) return;
    watchTimer = setInterval(watchdog, 5000);
    elapsedTimer = setInterval(tickElapsed, 1000);
    scheduleUsageRefresh();
  }
  function scheduleUsageRefresh(delay = 60000) {
    clearTimeout(usageTimer);
    if (!document.hidden && usageProviders().length) usageTimer = setTimeout(() => refreshLimits(), delay);
  }

  function watchdog() {
    if (!state.roomId || document.hidden) return;
    if (state.es && state.es.readyState === 2) {
      connect(state.roomId);
      openRoom(state.roomId, true);
      return;
    }
    const streaming = activeParticipants().some((p) => p.status === "running");
    if (streaming && Date.now() - (state.lastEvent || 0) > 30000) {
      state.lastEvent = Date.now();
      openRoom(state.roomId, true);
    }
  }

  function tickElapsed() {
    if (!state.roomId || !state.room || document.hidden) return;
    $$("[data-msg-elapsed]").forEach((node) => {
      const id = node.getAttribute("data-msg-elapsed");
      const m = state.messages.get(id);
      if (m && m.status === "streaming") {
        node.textContent = "⏱ " + fmtElapsed(Date.now() - (m.ts || Date.now()));
      }
    });
    $$("[data-agent-elapsed]").forEach((node) => {
      const pid = node.getAttribute("data-agent-elapsed");
      const ms = getAgentElapsedMs(pid);
      if (ms != null) {
        node.textContent = "⏱ " + fmtElapsed(ms);
      }
    });
  }

  function getAgentElapsedMs(pid) {
    for (let i = state.order.length - 1; i >= 0; i--) {
      const m = state.messages.get(state.order[i]);
      if (m && m.kind === "agent" && m.author === pid && m.status === "streaming" && m.ts) {
        return Math.max(0, Date.now() - m.ts);
      }
    }
    const t = state.queue.find((q) => q.agent === pid && q.status === "running");
    if (t && t.created) return Math.max(0, Date.now() - t.created);
    return null;
  }

  async function refreshLimits(manual = false, cached = false) {
    const providers = usageProviders();
    if (!providers.length || state.usageBusy || document.hidden) return;
    const selection = providers.join(",");
    state.usageBusy = true;
    state.usageError = "";
    renderUsagePanel();
    try {
      const usage = cached
        ? await api("GET", "/api/usage?providers=" + encodeURIComponent(selection))
        : await api("POST", "/api/usage/refresh", { providers, force: manual });
      if (usageProviders().join(",") !== selection) return;
      state.limits = usage.limits;
      state.usageSources = usage.sources;
      state.usageChecked = Date.now();
    } catch (err) {
      if (usageProviders().join(",") === selection) state.usageError = "갱신 실패 · 다시 시도하세요";
    } finally {
      state.usageBusy = false;
      renderUsagePanel();
      scheduleUsageRefresh(usageProviders().join(",") === selection ? 60000 : 0);
    }
  }

  let panelTimer = null;
  function schedulePanels() {
    if (panelTimer || document.hidden) return;
    panelTimer = setTimeout(() => {
      panelTimer = null;
      if (!state.room || document.hidden) return;
      renderParticipants(); renderTargetControls(); renderUsagePanel(); renderQueue(); renderDiscussion(); updateHint();
    }, STREAM_THROTTLE_MS);
  }
  let roomsTimer = null;
  let roomsLoading = null;
  let roomsRefreshPending = false;
  function scheduleRoomList() {
    roomsRefreshPending = true;
    if (!roomsTimer) roomsTimer = setTimeout(() => { roomsTimer = null; loadRooms().catch((err) => announce("방 목록 갱신 실패: " + err.message)); }, 500);
  }

  async function loadRooms() {
    if (roomsLoading) { roomsRefreshPending = true; return roomsLoading; }
    roomsRefreshPending = false;
    roomsLoading = api("GET", "/api/rooms").then((rooms) => { state.rooms = rooms || []; renderRoomList(); }).finally(() => {
      roomsLoading = null;
      if (roomsRefreshPending) scheduleRoomList();
    });
    return roomsLoading;
  }

  function renderRoomList() {
    const ul = $("#room-list");
    if (!ul) return;
    ul.innerHTML = "";
    const q = state.roomSearch.trim().toLowerCase();
    const filtered = state.rooms.filter(
      (r) => !q || (r.title || "").toLowerCase().includes(q) || (r.mode || "").toLowerCase().includes(q)
    );
    if (!filtered.length) {
      const emptyLi = el("li", "muted small-note", q ? "검색 결과 없음" : "생성된 방이 없습니다");
      emptyLi.style.padding = "8px 10px";
      ul.appendChild(emptyLi);
      return;
    }
    for (const r of filtered) {
      const isAct = r.id === state.roomId;
      const li = el("li", isAct ? "active" : "");
      const btn = el("button", "room-item");
      btn.type = "button";
      if (isAct) btn.setAttribute("aria-current", "page");
      btn.appendChild(el("span", "name", r.title));
      const right = el("span", "room-meta");
      right.appendChild(el("span", "badge " + r.mode, r.mode));
      if (r.active) {
        const dot = el("span", "dot");
        dot.title = "실행 중";
        dot.setAttribute("aria-label", "실행 중");
        right.appendChild(dot);
      }
      btn.appendChild(right);
      btn.onclick = () => {
        closeDrawers();
        openRoom(r.id);
      };
      li.appendChild(btn);
      ul.appendChild(li);
    }
  }

  function renderProviderStatus() {
    const box = $("#limits");
    if (!box) return;
    box.innerHTML = "";
    for (const p of state.providers) {
      const row = el("div", "prov-mini");
      row.appendChild(el("span", "prov-name " + p.id, LABEL[p.id] || p.label || p.id));
      const statusText = !p.available ? "CLI 없음" : p.can_steer ? "CLI 있음 · 즉시 조정" : "CLI 있음";
      row.appendChild(el("span", p.available ? "muted" : "unavailable", statusText));
      box.appendChild(row);
    }
  }

  function usageGauge(usedPct, mini) {
    const pct = Math.min(100, Math.max(0, Number(usedPct) || 0));
    const cls = "gauge" + (pct >= 90 ? " bad" : pct >= 80 ? " warn" : "") + (mini ? " mini" : "");
    const g = el("div", cls);
    const i = el("i");
    i.style.width = pct.toFixed(0) + "%";
    g.appendChild(i);
    return g;
  }

  function remainGauge(remainPct, mini) {
    const rem = Math.min(100, Math.max(0, Number(remainPct) || 0));
    const cls = "gauge" + (rem <= 15 ? " bad" : rem <= 30 ? " warn" : "") + (mini ? " mini" : "");
    const g = el("div", cls);
    const i = el("i");
    i.style.width = rem.toFixed(0) + "%";
    g.appendChild(i);
    return g;
  }

  // ---------- room ----------
  let openSeq = 0;
  async function openRoom(id, resync, latest = false) {
    const preserveHistory = !!(resync && !latest && state.roomId === id && state.browsingHistory && state.order.length);
    const priorTop = $("#timeline").scrollTop;
    const before = preserveHistory ? state.messages.get(state.order[state.order.length - 1]).seq + 1 : 0;
    const my = ++openSeq;
    if (!resync) {
      if (state.es) {
        state.es.close();
        state.es = null;
      }
      state.roomId = id;
      state.target = "";
      state.draftMentions.clear();
      clearComposerError();
      state.expanded.clear();
      state.userCollapsed.clear();
      localStorage.setItem("rt.room", id);
      const pbox = $("#participants");
      if (pbox) pbox.innerHTML = "";
    }
    const snap = await api("GET", "/api/rooms/" + id + (preserveHistory ? "?before=" + before + "&limit=200" : ""));
    if (my !== openSeq || state.roomId !== id) return;

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
    state.history = snap.history;
    state.browsingHistory = preserveHistory;
    state.historyLoading = false;
    state.pendingMessages = preserveHistory ? state.pendingMessages + Math.max(0, snap.room.seq - state.latestSeq) : 0;
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
    pruneMessageCaches();
    renderTimeline(!preserveHistory);
    if (preserveHistory) $("#timeline").scrollTop = priorTop;
    renderParticipants();
    renderQueue();
    renderDiscussion();
    renderRoomList();
    updateHint();
    refreshLimits(false, true);
    if (!resync) connect(id);
  }

  function connect(id) {
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
      case "delta": {
        const m = state.messages.get(data.id);
        if (!m) return;
        m.text = (m.text || "") + (data.block && m.text ? "\n\n" : "") + data.text;
        scheduleRender(m.id);
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
        namesRoom = null;
        schedulePanels();
        for (const id of state.order.slice(state.winStart, state.winEnd)) {
          const m = state.messages.get(id);
          if (m && m.author === data.id) scheduleRender(id);
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
      case "limits":
        refreshLimits(false, true);
        break;
      case "room":
        state.roomStateRevision++;
        state.room = data;
        if ($("#room")) $("#room").dataset.mode = data.mode || "read";
        renderRoomHead();
        renderParticipants();
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
    const tl = $("#timeline");
    if (!existed && !state.browsingHistory && state.order.length >= DOM_CAP_STEP && tl.scrollHeight - tl.scrollTop - tl.clientHeight >= 80) state.browsingHistory = true;
    if (!existed && (state.browsingHistory || (!isNewSequence && state.order.length))) {
      if (isNewSequence) state.pendingMessages++;
      updateTimelineBanners();
      scheduleRoomList();
      return;
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

  function renderRoomHead() {
    const r = state.room;
    if (!r) return;
    $("#btn-add-participant").disabled = activeParticipants().length >= 32;
    $("#room-title").textContent = r.title;
    const badge = $("#room-mode");
    const modeLabel = r.mode === "write" ? "⚠ write" : r.mode;
    badge.textContent = modeLabel;
    badge.className = "badge " + r.mode;
    badge.title =
      r.mode === "write"
        ? "write 모드: CLI 작업 폴더 편집을 허용합니다"
        : r.mode === "read"
        ? "read 모드: 작업 폴더 읽기·검색만 허용됩니다"
        : "chat 모드: 대화 중심 · 프로젝트 폴더를 전달하지 않습니다";
    $("#room-ws").textContent = r.workspace || "(작업 폴더 없음)";
    $("#room-ws").title = r.workspace || "작업 폴더가 지정되지 않았습니다";

    const noteBadge = $("#room-note-badge");
    if (noteBadge) {
      if (r.note && r.note.trim()) {
        noteBadge.hidden = false;
        noteBadge.title = "방 공지: " + r.note.trim();
      } else {
        noteBadge.hidden = true;
      }
    }

    $("#rounds").value = r.discussion_rounds || 3;
    $("#btn-export").href = "/api/rooms/" + r.id + "/export.md";

    renderTargetControls();
    renderUsagePanel();
  }

  // ---------- steering target controls ----------
  function renderTargetControls() {
    const r = state.room;
    if (!r) return;
    const controlsKey = JSON.stringify([state.target, activeParticipants().map((p) => [p.id, p.label, p.enabled, p.status, p.provider])]);
    if (state.targetControlsKey === controlsKey) return;
    state.targetControlsKey = controlsKey;
    const sel = $("#target");
    if (sel) {
      sel.innerHTML = '<option value="">전체 (@멘션 규칙)</option>';
      for (const p of activeParticipants()) {
        const o = el("option", "", participantLabel(p.id) + "에게만");
        o.value = p.id;
        sel.appendChild(o);
      }
      if (state.target && !activeParticipants().some((p) => p.id === state.target)) {
        state.target = "";
      }
      sel.value = state.target;
    }

    const chipsBox = $("#target-chips");
    if (chipsBox) {
      const focusedId = chipsBox.contains(document.activeElement) ? document.activeElement.dataset.target : null;
      chipsBox.innerHTML = "";
      const allBtn = el("button", "target-chip-btn" + (state.target === "" ? " active" : ""), "전체");
      allBtn.type = "button";
      allBtn.dataset.target = "";
      allBtn.setAttribute("aria-pressed", state.target === "" ? "true" : "false");
      allBtn.title = "특정 대상 지정 없이 @멘션 규칙(또는 전원)으로 전송";
      allBtn.onclick = () => setTarget("");
      chipsBox.appendChild(allBtn);

      for (const p of activeParticipants()) {
        const isAct = state.target === p.id;
        const b = el("button", "target-chip-btn" + (isAct ? " active" : ""), participantLabel(p.id));
        b.type = "button";
        b.dataset.target = p.id;
        b.setAttribute("aria-pressed", isAct ? "true" : "false");
        b.title = participantLabel(p.id) + "에게만 보내기 · " + modelLabel(p.provider, p.model);
        b.onclick = () => setTarget(isAct ? "" : p.id, true);
        chipsBox.appendChild(b);
      }
      if (focusedId !== null) {
        const nextFocus = [...chipsBox.children].find((b) => b.dataset.target === focusedId);
        if (nextFocus) nextFocus.focus();
      }
    }

    const actChip = $("#target-active-chip");
    if (actChip) {
      if (!state.target) {
        actChip.hidden = true;
        actChip.innerHTML = "";
      } else {
        const hadFocus = actChip.contains(document.activeElement);
        const p = r.participants.find((x) => x.id === state.target);
        const info = state.providers.find((x) => x.id === p.provider) || {};
        const modeTxt =
          p && p.status === "running"
            ? info.can_steer
              ? "⚡ 실행 중 즉시 주입"
              : "⏳ 현재 턴 직후 전달"
            : "🎯 1회성 단독 전달";
        actChip.hidden = false;
        actChip.innerHTML = "";
        actChip.appendChild(el("span", "", participantLabel(p.id) + " · " + modeTxt));
        const clr = el("button", "", "× Esc");
        clr.type = "button";
        clr.setAttribute("aria-label", participantLabel(state.target) + " 대상 지정 해제 (Esc)");
        clr.onclick = () => setTarget("", true);
        actChip.appendChild(clr);
        if (hadFocus) clr.focus();
      }
    }
  }

  function setTarget(targetId, focusInput) {
    state.target = targetId || "";
    const sel = $("#target");
    if (sel) sel.value = state.target;
    renderTargetControls();
    updateHint();
    if (state.target) {
      announce(participantLabel(state.target) + " 대상으로 지정됨. Esc 키로 해제할 수 있습니다.");
    } else if (focusInput) {
      announce("대상 지정이 해제되어 전체 대상으로 돌아왔습니다.");
    }
    if (focusInput && $("#input")) $("#input").focus();
  }

  // ---------- Account usage in the persistent sidebar ----------
  function messageInputTokens(u) {
    if (!u || u.input_tokens == null) return 0;
    return (
      Number(u.input_tokens || 0) +
      Number(u.cache_read_input_tokens || u.cached_input_tokens || u.cache_read_tokens || 0)
    );
  }

  function usageProviders() {
    return [...new Set(activeParticipants().map((p) => p.provider))];
  }

  function renderUsagePanel() {
    const strip = $("#token-strip");
    const detail = $("#usage-detail-content");
    if (!strip || !detail) return;
    const selected = usageProviders();
    const selection = selected.join(",");
    if (state.usageSelection !== selection) {
      state.usageSelection = selection; state.usageChecked = 0; state.usageError = "";
      scheduleUsageRefresh(0);
    }
    const participants = state.room ? state.room.participants : [];
    const key = JSON.stringify([!!state.room, selection, state.limits, state.usageSources, state.usageBusy, state.usageError, state.usageChecked, participants.map((p) => p.totals)]);
    if (strip.dataset.key === key) return;
    strip.dataset.key = key;
    strip.replaceChildren();
    detail.replaceChildren();
    for (const provider of selected) {
      const limit = state.limits[provider];
      const windows = limit && limit.windows ? limit.windows.filter((w) => Number.isFinite(w.used_percent)) : [];
      const source = state.usageSources[provider];
      const unsupported = source && source.refresh_kind === "unsupported";
      const remaining = windows.map((w) => Math.max(0, Math.min(100, 100 - Number(w.used_percent))));
      const account = el("section", "usage-account");
      const heading = el("div", "usage-account-head");
      heading.appendChild(el("span", "prov-name " + provider, LABEL[provider] || provider));
      if (!unsupported) {
        const stale = !source || source.stale;
        const observed = source && source.observed_at || limit && limit.at;
        const quality = el("span", "usage-quality" + (stale ? " stale" : ""), !observed ? "미확인" : stale ? "이전 관측" : "최근 관측");
        quality.title = source && source.observed_at ? new Date(source.observed_at).toLocaleString("ko-KR") + "에 관측" : "새 관측을 아직 확인하지 못했어.";
        heading.appendChild(quality);
      }
      account.appendChild(heading);
      for (let i = 0; i < windows.length; i++) {
        const item = el("div", "usage-window" + (remaining[i] <= 15 ? " warn" : ""));
        const title = el("div", "usage-window-title");
        title.appendChild(el("span", "", windows[i].label));
        title.appendChild(el("b", "", Math.round(remaining[i]) + "%"));
        item.appendChild(title);
        const gauge = remainGauge(remaining[i], true);
        gauge.setAttribute("aria-hidden", "true");
        item.appendChild(gauge);
        account.appendChild(item);
      }
      if (!windows.length) account.appendChild(el("p", "usage-empty muted", unsupported ? "한도·잔액 조회 미지원" : "아직 관측값이 없어"));
      strip.appendChild(account);
      const group = el("section", "usage-provider");
      group.appendChild(el("b", "", LABEL[provider] || provider));
      for (let i = 0; i < windows.length; i++) {
        const w = windows[i];
        const row = el("div", "stat-row");
        row.appendChild(el("span", "", w.label));
        const resetText = !w.resets_at ? "" : w.resets_at * 1000 <= Date.now()
          ? " · 초기화 시각 지남 · 새 관측 필요"
          : " · " + fmtReset(w.resets_at) + " 초기화";
        row.appendChild(el("b", "", "남음 " + Math.round(remaining[i]) + "%" + resetText));
        group.appendChild(row);
      }
      if (unsupported) {
        group.appendChild(el("p", "small-note muted", "한도·잔액 조회는 지원하지 않습니다. 연결한 공급사의 관리 화면에서 확인하세요."));
      } else if (source) {
        const observed = source.observed_at ? "관측 " + new Date(source.observed_at).toLocaleString("ko-KR") : "관측 시각 없음";
        group.appendChild(el("p", "small-note muted", observed + (source.refresh_kind === "vendor" ? " · 공급사 조회" : " · 저장된 기록")));
        if (source.note) group.appendChild(el("p", "small-note muted", source.note));
        if (source.error) group.appendChild(el("p", "small-note warn", "갱신하지 못했습니다: " + source.error));
      }
      detail.appendChild(group);
    }
    if (!selected.length) strip.appendChild(el("p", "usage-empty muted", state.room ? "이 방에 공급사를 추가하면 사용량을 보여줘." : "방을 선택하면 해당 공급사의 사용량을 보여줘."));
    const totals = participants.reduce((sum, p) => {
      const t = p.totals || {};
      sum.input += Number(t.input || 0); sum.output += Number(t.output || 0); sum.cost += Number(t.cost_usd || 0);
      return sum;
    }, { input: 0, output: 0, cost: 0 });
    if (state.room) detail.appendChild(el("p", "small-note muted", "이 방 누적 · 입력 " + fmtK(totals.input) + " · 출력 " + fmtK(totals.output) + (totals.cost ? " · 보고된 비용 $" + totals.cost.toFixed(3) : "")));
    detail.appendChild(el("p", "small-note muted", "이 방에 추가한 공급사만 화면을 보는 동안 1분마다 갱신해. 참가자를 꺼도 사용량은 남아 있어. 공급사마다 조회 방식과 관측 시각은 달라."));
    $("#btn-refresh-usage").disabled = state.usageBusy || !selected.length;
    $("#usage-status").textContent = !selected.length ? "" : state.usageBusy ? "갱신 중…" : state.usageError ? state.usageError : state.usageChecked ? "확인 " + fmtTime(state.usageChecked) : "";
  }

  // ---------- timeline (Fix P2 #2: Strict 200 DOM cap in all scroll positions) ----------
  function renderTimeline(scrollToBottom) {
    const tl = $("#timeline");
    if (!tl) return;
    tl.innerHTML = "";

    const total = state.order.length;
    if (!total) {
      state.winStart = 0;
      state.winEnd = 0;
      updateTimelineBanners(tl);
      renderRoomEmptyGuide(tl);
      return;
    }

    if (scrollToBottom || state.winEnd > total || state.winEnd <= state.winStart) {
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

    if (scrollToBottom) tl.scrollTop = tl.scrollHeight;
  }

  function updateTimelineBanners(tl = $("#timeline")) {
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
  }

  function pruneMessageCaches() {
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
    state.historyLoading = true; state.browsingHistory = true;
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
      if (restored) tl.scrollTop += restored.getBoundingClientRect().top - anchorTop;
      announce("이전 대화 " + page.messages.length + "개를 불러왔습니다. 최신 대화는 아래 버튼으로 돌아갈 수 있습니다.");
    } catch (err) {
      if (state.roomId === roomId && openSeq === sequence) announce("이전 대화 조회 실패: " + err.message);
    } finally {
      if (state.roomId === roomId && openSeq === sequence) { state.historyLoading = false; updateTimelineBanners(); }
    }
  }

  async function jumpToLatestMessages() {
    if (!state.roomId) return;
    try { await openRoom(state.roomId, true, true); }
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

  function scheduleRender(id) {
    if (state.renderTimers.has(id) || document.hidden) return;
    state.renderTimers.set(
      id,
      setTimeout(() => {
        state.renderTimers.delete(id);
        renderMessage(id, false);
      }, STREAM_THROTTLE_MS)
    );
  }

  function scheduleTraceRender(id) {
    if (state.traceTimers.has(id) || document.hidden) return;
    state.traceTimers.set(
      id,
      setTimeout(() => {
        state.traceTimers.delete(id);
        ensureAndRenderTrace(id);
      }, STREAM_THROTTLE_MS)
    );
  }

  // ---------- live progress for replies that have no text yet ----------
  // Codex narrates while it works; Claude and Antigravity often stay silent and
  // delegate to subagents for minutes. Without this the bubble is just a cursor.
  const firstLine = (s) => String(s || "").replace(/\s+/g, " ").trim().slice(0, 160);

  function liveFeed(id, e) {
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

  function liveHtml(lv) {
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
  async function seedLive(roomId, id) {
    try {
      const events = await api("GET", "/api/rooms/" + roomId + "/trace/" + id);
      if (state.roomId !== roomId) return;
      // The stored trace already contains anything SSE delivered meanwhile:
      // rebuild from it instead of adding to a partial state.
      state.live.delete(id);
      for (const e of events) liveFeed(id, e);
      scheduleRender(id);
    } catch (err) {
      console.warn("live progress seed failed", id, err.message);
    }
  }

  function activeTraceActivity(id, author) {
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

  function renderMessage(id, isNew, batch, insertBeforeNode) {
    const m = state.messages.get(id);
    if (!m) return;
    const tl = $("#timeline");
    if (!tl) return;

    const emptyGuide = $("#timeline-empty");
    if (emptyGuide) emptyGuide.remove();

    const atBottom = tl.scrollHeight - tl.scrollTop - tl.clientHeight < 80;
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
      node.innerHTML =
        '<div class="bubble"><span>ℹ ' +
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
        meta.push('<span class="resp-time" title="이 답변에 걸린 시간">⏱ ' + esc(fmtElapsed(m.duration_ms)) + "</span>");
      }
      if (m.reason === "discussion") meta.push('<span class="badge">🗣 토론 ' + m.round + "R</span>");
      else if (m.reason === "mention") meta.push('<span class="badge">@호출</span>');
      meta.push('<span class="muted">' + fmtTime(m.ts) + "</span>");

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
      }
      if (m.signal) {
        meta.push('<span class="signal ' + esc(m.signal) + '">' + esc(m.signal) + "</span>");
      }

      const live = m.kind === "agent" && m.status === "streaming" && !(m.text || "").trim() ? state.live.get(m.id) : null;
      if (m.status !== "streaming") state.live.delete(m.id);
      const bodyHtml =
        m.kind === "user" ? "<p>" + esc(rewriteProseMentions(m.text, visibleMentions)).replace(/\n/g, "<br>") + "</p>" : live ? liveHtml(live) : md(m.text);
      const bodyKey = live ? "live:" + live.ver : visibleMentions(m.text);

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
          esc(participantLabel(m.author)[0]) +
          "</div>" +
          '<div class="bubble">' +
          '<div class="meta"></div>' +
          '<div class="body"></div>' +
          '<div class="err-slot"></div>' +
          '<div class="foot"></div>' +
          '<div class="trace" id="t-' +
          esc(id) +
          '" hidden></div>' +
          "</div>";
        bubble = node.querySelector(".bubble");
      }

      bubble.querySelector(".meta").innerHTML = meta.join("");
      const bodyEl = bubble.querySelector(".body");
      bodyEl.className = "body" + (m.status === "streaming" ? " cursor" : "");
      if (bodyEl.dataset.rawText !== bodyKey || isInitialMount) {
        bodyEl.dataset.rawText = bodyKey;
        bodyEl.innerHTML = bodyHtml;
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

      const errSlot = bubble.querySelector(".err-slot");
      errSlot.innerHTML = "";
      if (m.error || m.status === "error") {
        const errBox = el("div", "err-box");
        errBox.appendChild(el("span", "", "⚠ 원인: " + (m.error || "에이전트 실행 실패")));
        if (m.kind === "agent" && m.status === "error") {
          const rBtn = el("button", "retry-btn", "↻ 다시 시도");
          rBtn.type = "button";
          rBtn.title = participantLabel(m.author) + "에게 직전 사용자 메시지를 다시 요청합니다";
          rBtn.onclick = () => retryTurn(m.id, rBtn);
          errBox.appendChild(rBtn);
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
      tl.scrollTop = tl.scrollHeight;
    }
  }

  function updateTraceCount(id) {
    const b = $("#m-" + id + " .trace-btn");
    const m = state.messages.get(id);
    if (b && m) {
      const isExp = state.expanded.has(id);
      b.textContent = (isExp ? "작업 기록 접기" : "작업 기록") + (m.trace ? " · " + m.trace : "");
      b.setAttribute("aria-expanded", isExp ? "true" : "false");
    }
  }

  function expandTrace(id) {
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

  function boundedTraceEvent(event) {
    const item = { ...event };
    for (const key of ["text", "input", "output"]) {
      if (typeof item[key] === "string" && item[key].length > 8000) {
        item[key] = item[key].slice(0, 8000); item._displayTruncated = true;
      }
    }
    return item;
  }

  function cacheTrace(id, events, total) {
    const bounded = events.slice(-TRACE_EVENT_LIMIT).map(boundedTraceEvent);
    const clipped = total > TRACE_EVENT_LIMIT || events.length > TRACE_EVENT_LIMIT || bounded.some((event) => event._displayTruncated);
    const previous = state.traceMeta.get(id);
    state.traces.delete(id);
    state.traces.set(id, bounded);
    state.traceMeta.set(id, { total, clipped: clipped || !!(previous && previous.clipped) });
    while (state.traces.size > TRACE_CACHE_LIMIT) {
      const oldest = state.traces.keys().next().value;
      state.traces.delete(oldest); state.traceMeta.delete(oldest); state.expanded.delete(oldest);
      const box = $("#t-" + oldest);
      if (box) { box.hidden = true; box.replaceChildren(); }
      updateTraceCount(oldest);
    }
  }

  async function ensureAndRenderTrace(id) {
    if (!state.expanded.has(id) || document.hidden) return;
    const roomId = state.roomId; const sequence = openSeq;
    if (!state.traces.has(id)) {
      if (state.traceFetching.has(id)) return;
      state.traceFetching.add(id);
      state.tracePending.set(id, new Map());
      const controller = new AbortController();
      state.traceControllers.set(id, controller);
      try {
        const events = await api("GET", "/api/rooms/" + roomId + "/trace/" + id, undefined, controller.signal);
        if (state.roomId !== roomId || openSeq !== sequence || !state.messages.has(id) || !state.expanded.has(id)) return;
        const pending = state.tracePending.get(id);
        const later = [...pending.entries()].filter(([index]) => index > events.length).sort(([a], [b]) => a - b);
        const total = later.length ? later[later.length - 1][0] : events.length;
        cacheTrace(id, [...events.slice(-TRACE_EVENT_LIMIT), ...later.map(([, event]) => event)], total);
        state.messages.get(id).trace = Math.max(state.messages.get(id).trace || 0, total);
        state.traceErrors.delete(id);
      } catch (err) {
        if (controller.signal.aborted) return;
        if (state.roomId !== roomId || openSeq !== sequence) return;
        state.traceErrors.set(id, err.message);
        renderTraceError(id); updateTraceCount(id); return;
      } finally {
        if (state.traceControllers.get(id) === controller) {
          state.traceFetching.delete(id); state.tracePending.delete(id); state.traceControllers.delete(id);
        }
      }
    }
    renderTrace(id); updateTraceCount(id);
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
  async function toggleTrace(id) {
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

  function renderTrace(id) {
    const box = $("#t-" + id);
    if (!box) return;
    box.hidden = false;
    const events = state.traces.get(id) || [];
    const metadata = state.traceMeta.get(id);
    const windowStart = metadata ? metadata.total - events.length : 0;
    const moved = box.dataset.windowStart !== undefined && Number(box.dataset.windowStart) !== windowStart;
    const opened = new Set(moved ? [...box.querySelectorAll("details[open][data-trace-key]")].map((node) => node.dataset.traceKey) : []);
    const focused = moved && box.contains(document.activeElement) ? document.activeElement.closest("[data-trace-key]") : null;
    const focusedKey = focused && focused.dataset.traceKey;
    if (moved) box.replaceChildren();
    box.dataset.windowStart = String(windowStart);
    let notice = box.querySelector(".trace-window-note");
    if (!notice) { notice = el("div", "trace-window-note muted"); box.prepend(notice); }
    notice.replaceChildren();
    notice.appendChild(el("span", "", metadata && metadata.clipped ? "최근 " + events.length + "개 이벤트 표시 · 전체 " + metadata.total + "개. 긴 출력은 화면에서 줄였습니다. " : ""));
    const full = el("a", "", "전체 기록 내려받기");
    full.href = "/api/rooms/" + state.roomId + "/trace/" + id;
    full.download = "work-history.json";
    notice.appendChild(full);
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
          break;
        }
        case "subagent_msg": {
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
          s.lines.push(e);
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
      sum.innerHTML = buildSubSummaryHtml(it);
      det.appendChild(sum);
      const s = it.start || {};
      if (s.input) {
        const taskDiv = el("div", "line sub-task");
        taskDiv.innerHTML = '<span class="k">과제</span>' + pre(s.input);
        det.appendChild(taskDiv);
      }
      const linesBox = el("div", "lines");
      det.appendChild(linesBox);
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
      const newSumHtml = buildSubSummaryHtml(it);
      if (sum && sum.innerHTML !== newSumHtml) {
        sum.innerHTML = newSumHtml;
      }
      const linesBox = node.querySelector(":scope > .lines");
      if (linesBox) {
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

  function buildSubSummaryHtml(it) {
    const s = it.start || {},
      e = it.end;
    return (
      "🧩 서브에이전트 <b>" +
      esc(s.name || s.parent || "subagent") +
      "</b> " +
      (e ? "✓ 종료" : "⏳ 진행 중") +
      (s.meta && s.meta.description ? ' <span class="muted">' + esc(s.meta.description) + "</span>" : "")
    );
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

  // ---------- Right Panel: In-place Participant Cards (Fix #6 & V5) ----------
  function buildSessionActions(participantId, messageId = "") {
    const actions = el("div", "session-actions");
    const roomId = state.roomId;
    for (const action of ["open", "copy"]) {
      const button = el("button", "ghost", action === "open" ? "CLI에서 열기" : "명령 복사");
      button.type = "button";
      button.dataset.action = action;
      button.onclick = async () => {
        const sequence = openSeq;
        button.disabled = true;
        button.dataset.busy = "true";
        try {
          const url = "/api/rooms/" + roomId + "/participants/" + participantId + "/session";
          const session = await api("GET", url + (messageId ? "?message=" + encodeURIComponent(messageId) : ""));
          if (state.roomId !== roomId || openSeq !== sequence) return;
          if (action === "copy") {
            await navigator.clipboard.writeText(session.command);
            if (state.roomId !== roomId || openSeq !== sequence) return;
            announce("CLI 이어보기 명령을 복사했습니다.");
            button.textContent = "복사됨";
          } else {
            if (!session.can_open) throw new Error("이 환경에서는 CLI 창을 바로 열 수 없습니다. 명령을 복사해서 실행하세요.");
            const result = await api("POST", url + "/open", messageId ? { message_id: messageId } : {});
            if (state.roomId !== roomId || openSeq !== sequence) return;
            announce("기존 대화를 여는 CLI 실행 요청을 전달했습니다." + (result.paused ? " 참가자는 비활성화되었습니다. CLI 창을 닫은 뒤 다시 켜세요." : ""));
            button.textContent = "열기 요청됨";
          }
        } catch (e) {
          if (state.roomId === roomId && openSeq === sequence) alert(e.message);
        }
        finally {
          delete button.dataset.busy;
          button.disabled = actions.dataset.available !== "true";
          setTimeout(() => { button.textContent = action === "open" ? "CLI에서 열기" : "명령 복사"; }, 1800);
        }
      };
      actions.appendChild(button);
    }
    return actions;
  }

  function updateSessionActions(actions, sessionId, running) {
    const available = !!sessionId && !running;
    actions.dataset.available = String(available);
    for (const button of actions.children) {
      button.disabled = !available || button.dataset.busy === "true";
      button.title = !sessionId ? "첫 실행에서 CLI 세션이 저장되면 사용할 수 있습니다"
        : running ? "같은 세션의 실행이 끝난 뒤 열 수 있습니다"
        : (button.dataset.action === "open"
          ? "기존 CLI 대화를 엽니다. 이 참가자는 비활성화됩니다. CLI 창을 닫은 뒤 다시 켜세요."
          : "이어보기 명령을 복사합니다. 직접 실행하기 전에 이 참가자를 끄세요.") + " · 세션 " + sessionId;
    }
  }

  function getEffortsForModel(info, modelId) {
    if (info && info.model_efforts && Array.isArray(info.model_efforts[modelId])) {
      return info.model_efforts[modelId];
    }
    return info && Array.isArray(info.efforts) ? info.efforts : [];
  }

  function fillModelSelect(select, models, selected, query = "", allowCustom = false) {
    const q = query.trim().toLowerCase();
    const filtered = models.filter((m) => !q || (m.label + " " + (m.group || "") + " " + m.id).toLowerCase().includes(q) || m.id === selected);
    select.replaceChildren();
    if (allowCustom && selected === "") {
      const placeholder = el("option", "", "모델을 선택하세요");
      placeholder.value = ""; select.appendChild(placeholder);
    }
    const groups = new Map();
    for (const model of filtered) {
      let parent = select;
      if (model.group) {
        if (!groups.has(model.group)) {
          const group = document.createElement("optgroup"); group.label = model.group;
          groups.set(model.group, group); select.appendChild(group);
        }
        parent = groups.get(model.group);
      }
      const option = el("option", "", model.label);
      option.value = model.id; parent.appendChild(option);
    }
    if (allowCustom) {
      const option = el("option", "", "모델 ID 직접 입력…"); option.value = "__custom__"; select.appendChild(option);
    }
    if ([...select.options].some((o) => o.value === selected)) select.value = selected;
  }

  function renderParticipants() {
    const box = $("#participants");
    if (!box || !state.room) return;

    const validIds = new Set(activeParticipants().map((p) => p.id));
    Array.from(box.children).forEach((child) => {
      if (child.dataset && child.dataset.pid && !validIds.has(child.dataset.pid)) {
        child.remove();
      }
    });

    for (const p of activeParticipants()) {
      const info = state.providers.find((x) => x.id === p.provider) || {};
      let card = $("#pcard-" + p.id, box);
      if (!card) {
        card = buildParticipantCardSkeleton(p, info);
        box.appendChild(card);
      }
      updateParticipantCardInPlace(card, p, info);
    }
  }

  function buildParticipantCardSkeleton(p, info) {
    const card = el("div", "pcard");
    card.id = "pcard-" + p.id;
    card.dataset.pid = p.id;

    const top = el("div", "top");
    top.appendChild(el("span", "name " + p.provider, participantLabel(p.id)));

    const steerBtn = el("button", "ghost steer-btn", "선택");
    steerBtn.type = "button";
    steerBtn.style.padding = "2px 8px";
    steerBtn.style.minHeight = "24px";
    steerBtn.style.fontSize = "11px";
    steerBtn.title = participantLabel(p.id) + "에게만 다음 메시지 보내기";
    steerBtn.onclick = () => {
      closeDrawers();
      setTarget(p.id, true);
    };
    top.appendChild(steerBtn);

    const sw = el("button", "switch");
    sw.type = "button";
    sw.setAttribute("role", "switch");
    sw.setAttribute("aria-label", p.label + " 참여 켜기/끄기");
    sw.onclick = () => {
      const cur = state.room && state.room.participants.find((x) => x.id === p.id);
      if (cur) patchParticipant(p.id, { enabled: !cur.enabled });
    };
    top.appendChild(sw);
    card.appendChild(top);
    card.appendChild(el("div", "participant-model muted"));

    const edit = el("details", "participant-edit");
    edit.appendChild(el("summary", "muted", "설정 · 상세"));
    edit.ontoggle = () => {
      const current = participant(p.id);
      if (edit.open && current) { delete card.dataset.signature; updateParticipantCardInPlace(card, current, info); }
    };
    const nameLabel = el("label", "", "참가자 이름");
    const nameInput = el("input", "participant-name");
    nameInput.maxLength = 80;
    nameInput.value = p.label;
    nameLabel.appendChild(nameInput);
    edit.appendChild(nameLabel);
    const rename = el("button", "", "이름 저장");
    rename.type = "button";
    rename.onclick = () => {
      if (!nameInput.value.trim()) return nameInput.focus();
      patchParticipant(p.id, { label: nameInput.value.trim() });
    };
    edit.appendChild(rename);
    const customLabel = el("label", "", "모델 ID 직접 입력");
    const custom = el("input", "custom-model");
    custom.placeholder = "CLI가 지원하는 정확한 모델 ID";
    custom.value = p.model;
    customLabel.appendChild(custom);
    edit.appendChild(customLabel);
    edit.appendChild(el("p", "small-note muted", "목록에 없는 모델은 CLI가 지원하는 ID를 입력하세요. 지원 여부는 실행 시 확인됩니다."));
    const apply = el("button", "apply-custom-model", "모델 적용");
    apply.type = "button";
    apply.onclick = () => {
      const model = custom.value.trim();
      if (!model) return custom.focus();
      const efforts = getEffortsForModel(info, model);
      const cur = participant(p.id);
      patchParticipant(p.id, { model, effort: efforts.includes(cur.effort) ? cur.effort : "" });
    };
    edit.appendChild(apply);
    const remove = el("button", "danger ghost remove-participant", "참가자 제거");
    remove.type = "button";
    remove.onclick = async () => {
      if (!confirm(participantLabel(p.id) + " 참가자를 제거할까요? 이전 대화와 사용량은 보존됩니다.")) return;
      const roomId = state.roomId;
      const sequence = openSeq;
      try {
        const room = await api("DELETE", "/api/rooms/" + roomId + "/participants/" + p.id);
        if (state.roomId !== roomId || openSeq !== sequence) return;
        state.room = room;
        renderRoomHead();
        renderParticipants();
        updateHint();
      } catch (e) {
        if (state.roomId === roomId && openSeq === sequence) alert(e.message);
      }
    };
    edit.appendChild(remove);
    card.appendChild(edit);

    const unavailSlot = el("div", "unavail-slot");
    card.appendChild(unavailSlot);

    const selRow = el("div", "sel");
    edit.appendChild(el("p", "catalog-note small-note warn"));
    const catalogRefresh = el("button", "catalog-refresh", p.provider === "opencode" ? "연결·모델 새로고침" : "모델 목록 새로고침");
    catalogRefresh.type = "button";
    catalogRefresh.onclick = () => refreshProviderCatalog(catalogRefresh, () => {});
    edit.appendChild(catalogRefresh);
    const filterLabel = el("label", "model-filter-label", "모델 검색");
    const filter = el("input", "model-filter");
    filter.type = "search"; filter.placeholder = "공급사 또는 모델 이름";
    filterLabel.appendChild(filter); edit.appendChild(filterLabel);
    const ms = el("select", "model-sel");
    ms.title = "모델 선택";
    ms.setAttribute("aria-label", p.label + " 모델");
    selRow.appendChild(ms);

    const es = el("select", "effort-sel");
    es.title = "추론 노력 선택";
    es.setAttribute("aria-label", p.label + " 추론 노력");
    selRow.appendChild(es);
    edit.appendChild(selRow);
    filter.oninput = () => { delete card.dataset.signature; updateParticipantCardInPlace(card, participant(p.id), info); };
    edit.appendChild(el("div", "small-note muted", "모델·추론 수준 변경 시 새 대화 세션으로 시작합니다."));
    edit.appendChild(buildSessionActions(p.id));
    edit.appendChild(el("div", "small-note muted", "CLI 창을 닫은 뒤 참가자를 다시 켜세요. 복사한 명령은 참가자를 끈 뒤 실행하세요."));
    edit.appendChild(el("div", "participant-id muted mono", "참가자 ID: " + p.id));

    es.onchange = () => patchParticipant(p.id, { effort: es.value });
    ms.onchange = () => {
      const curInfo = state.providers.find((x) => x.id === p.provider) || info;
      const curPart = (state.room && state.room.participants.find((x) => x.id === p.id)) || p;
      const newModel = ms.value;
      const curEff = es.hidden ? curPart.effort : es.value;
      const effList = getEffortsForModel(curInfo, newModel);
      syncEffortSelect(es, effList, curEff);
      const selected = effList.length ? es.value : "";
      patchParticipant(p.id, { model: newModel, effort: selected });
    };

    const stLine = el("div", "status-line");
    card.appendChild(stLine);
    card.appendChild(el("div", "resp-times muted"));
    const stop = el("button", "danger stop-participant", "작업 중단");
    stop.type = "button";
    stop.onclick = async () => {
      const roomId = state.roomId;
      const sequence = openSeq;
      const roomStateRevision = state.roomStateRevision;
      stop.dataset.pending = "true";
      stop.disabled = true;
      stop.textContent = "중단 요청 중…";
      try {
        const result = await api("POST", "/api/rooms/" + roomId + "/participants/" + p.id + "/stop", {});
        if (state.roomId !== roomId || openSeq !== sequence) return;
        stop.dataset.requested = "true";
        if (state.roomStateRevision === roomStateRevision) state.room = result.room;
        renderRoomHead(); renderParticipants(); updateHint(); scheduleRoomList();
        announce(participantLabel(p.id) + "의 작업 중단을 요청하고 참여를 껐습니다. 다시 참여하려면 참가자를 켜세요.");
      } catch (err) {
        if (state.roomId === roomId && openSeq === sequence) alert("작업을 중단하지 못했습니다: " + err.message);
      } finally {
        delete stop.dataset.pending;
        if (state.roomId === roomId && openSeq === sequence) {
          delete card.dataset.signature;
          const current = participant(p.id);
          if (current) updateParticipantCardInPlace(card, current, state.providers.find((provider) => provider.id === current.provider) || {});
        }
      }
    };
    card.appendChild(stop);

    const stats = el("div", "stats");
    edit.appendChild(stats);
    card.appendChild(edit);

    return card;
  }

  function syncEffortSelect(es, effList, currentEffort) {
    if (!effList.length) {
      es.hidden = true;
      es.dataset.effKey = "";
      es.innerHTML = "";
      return "";
    }
    es.hidden = false;
    const key = effList.join("|");
    if (es.dataset.effKey !== key) {
      es.dataset.effKey = key;
      es.innerHTML = "";
      const automatic = el("option", "", "CLI 기본값");
      automatic.value = "";
      es.appendChild(automatic);
      for (const e of effList) {
        const o = el("option", "", e);
        o.value = e;
        es.appendChild(o);
      }
    }
    const selected = effList.includes(currentEffort) ? currentEffort : "";
    if (document.activeElement !== es && es.value !== selected) {
      es.value = selected;
    } else if (es.value !== "" && !effList.includes(es.value)) {
      es.value = selected;
    }
    return es.value;
  }

  function updateParticipantCardInPlace(card, p, info) {
    const signature = JSON.stringify([p, participantLabel(p.id), info.available, info.catalog_status, info.catalog_error, state.catalogRevision, seatTimes(p.id), seatClass(p.id)]);
    if (card.dataset.signature === signature) return;
    card.dataset.signature = signature;
    card.className = "pcard" + seatClass(p.id) + (p.enabled ? "" : " off");
    card.querySelector(".name").textContent = participantLabel(p.id);
    card.querySelector(".participant-model").textContent = modelLabel(p.provider, p.model) + (p.effort ? " · " + p.effort : "");
    const catalogNote = card.querySelector(".catalog-note");
    catalogNote.textContent = catalogMessage(info);
    catalogNote.hidden = !catalogNote.textContent;
    card.querySelector(".catalog-refresh").hidden = p.provider !== "opencode" && !catalogNote.textContent;
    const nameInput = card.querySelector(".participant-name");
    const custom = card.querySelector(".custom-model");
    if (!card.querySelector(".participant-edit").open) {
      nameInput.value = p.label;
      custom.value = p.model;
    }
    for (const control of card.querySelectorAll(".model-sel, .effort-sel, .custom-model, .apply-custom-model, .remove-participant")) {
      control.disabled = p.status === "running";
      control.title = p.status === "running" ? "현재 턴을 중지하거나 완료한 뒤 변경하세요" : "";
    }
    card.querySelector(".model-sel").setAttribute("aria-label", participantLabel(p.id) + " 모델");
    card.querySelector(".effort-sel").setAttribute("aria-label", participantLabel(p.id) + " 추론 노력");
    updateSessionActions(card.querySelector(".session-actions"), p.session_id, p.status === "running");
    const stop = card.querySelector(".stop-participant");
    const hasWork = p.status === "running" || p.status === "queued";
    if (!hasWork || p.enabled) delete stop.dataset.requested;
    stop.hidden = !hasWork;
    stop.setAttribute("aria-label", participantLabel(p.id) + " 작업 중단");
    stop.title = "이 참가자의 현재 작업과 대기열을 중단하고 참여를 끕니다.";
    stop.disabled = stop.dataset.pending === "true" || stop.dataset.requested === "true";
    stop.textContent = stop.dataset.pending === "true" ? "중단 요청 중…" : stop.dataset.requested === "true" ? "중단 요청됨 · 참여 꺼짐" : "작업 중단";

    const sw = card.querySelector(".switch");
    if (sw) {
      sw.className = "switch" + (p.enabled ? " on" : "");
      sw.setAttribute("aria-checked", p.enabled ? "true" : "false");
      sw.setAttribute("aria-label", participantLabel(p.id) + " 참여 켜기/끄기");
      sw.title = p.enabled ? "참여 중 (클릭하여 끄기)" : "꺼짐 (클릭하여 켜기)";
    }

    const unavailSlot = card.querySelector(".unavail-slot");
    if (unavailSlot) {
      if (info.available === false) {
        unavailSlot.innerHTML = '<div class="unavailable">⚠ CLI를 찾지 못함 (' + esc(info.command || p.id) + ")</div>";
      } else if (unavailSlot.hasChildNodes()) {
        unavailSlot.innerHTML = "";
      }
    }

    const ms = card.querySelector(".model-sel");
    if (ms) {
      const models = [...(info.models || [])];
      if (p.model && !models.some((m) => m.id === p.model)) {
        models.push({ id: p.model, label: "직접 지정 모델" });
      }
      const filter = card.querySelector(".model-filter");
      card.querySelector(".model-filter-label").hidden = models.length < 30;
      const mKey = JSON.stringify([models, filter.value]);
      if (ms.dataset.mKey !== mKey) {
        ms.dataset.mKey = mKey;
        fillModelSelect(ms, models, p.model, filter.value);
      }
      if (document.activeElement !== ms && ms.value !== p.model) {
        ms.value = p.model;
      }
    }

    const es = card.querySelector(".effort-sel");
    if (es) {
      const effList = getEffortsForModel(info, p.model);
      syncEffortSelect(es, effList, p.effort);
    }

    const rt = card.querySelector(".resp-times");
    if (rt) {
      const t = seatTimes(p.id);
      rt.textContent = t ? "응답 시간 · 최근 " + fmtElapsed(t.last) + " · 평균 " + fmtElapsed(t.avg) + " (" + t.n + "회)" : "응답 시간 · 아직 완료한 답변 없음";
    }
    const stLine = card.querySelector(".status-line");
    if (stLine) {
      stLine.className = "status-line " + (p.status || "idle");
      stLine.innerHTML = "";
      const stMain = el("span", "status-main");
      if (p.status === "running") {
        stMain.appendChild(el("span", "spin"));
        stMain.appendChild(el("span", "", "실행 중 · " + (p.status_text || "생각 중")));
        stLine.appendChild(stMain);
        const elapMs = getAgentElapsedMs(p.id);
        const elapSpan = el("span", "elapsed", "⏱ " + fmtElapsed(elapMs || 0));
        elapSpan.setAttribute("data-agent-elapsed", p.id);
        stLine.appendChild(elapSpan);
      } else if (p.status === "queued") {
        stMain.textContent = "⏳ 대기열에서 차례 대기 중";
        stLine.appendChild(stMain);
      } else {
        stMain.textContent = p.enabled ? "대기 중" : "참여 꺼짐";
        stLine.appendChild(stMain);
      }
    }

    const t = p.totals || {};
    const stats = card.querySelector(".stats");
    if (stats && card.querySelector(".participant-edit").open) {
      stats.innerHTML = "";
      const addStatRow = (k, v) => {
        const r = el("div", "stat-row");
        r.appendChild(el("span", "", k));
        r.appendChild(el("b", "", v));
        stats.appendChild(r);
      };
      if (p.context_window > 0) {
        const pct = (p.context_tokens / p.context_window) * 100;
        addStatRow(
          "컨텍스트",
          fmtK(p.context_tokens) + " / " + fmtK(p.context_window) + " (" + pct.toFixed(0) + "%)"
        );
        stats.appendChild(usageGauge(pct, false));
      }
      addStatRow(
        "이 방 누적",
        "입력 " + fmtK(t.input) + " · 출력 " + fmtK(t.output) + " · 캐시 " + fmtK(t.cached)
      );
      if (t.cost_usd) addStatRow("누적 비용", "$" + t.cost_usd.toFixed(3));
      addStatRow(
        "완료 턴",
        (p.turns || 0) + "턴" + (p.session_id ? " · 세션 " + p.session_id.slice(0, 8) + "…" : "")
      );
    }

  }

  async function patchParticipant(id, patch) {
    if (!state.roomId) return;
    try {
      await api("PATCH", "/api/rooms/" + state.roomId, { participants: [{ id, ...patch }] });
    } catch (e) {
      alert(e.message);
    }
  }

  function renderQueue() {
    const ul = $("#queue");
    const cnt = $("#queue-count");
    if (cnt) cnt.textContent = String(state.queue.length);
    if (!ul) return;
    ul.innerHTML = "";
    if (!state.queue.length) {
      ul.appendChild(el("li", "muted", "대기 중인 턴 없음"));
      return;
    }
    for (const t of state.queue) {
      const li = el("li", t.status);
      const reasonTxt =
        { mention: "@호출", broadcast: "전체", discussion: "토론 " + t.round + "R" }[t.reason] || t.reason;
      li.appendChild(
        el("span", "", (t.status === "running" ? "실행 · " : "대기 · ") + participantLabel(t.agent) + " · " + reasonTxt)
      );
      const b = el("button", t.status === "running" ? "danger" : "", t.status === "running" ? "중단" : "취소");
      b.type = "button";
      b.setAttribute("aria-label", participantLabel(t.agent) + " 턴 " + (t.status === "running" ? "중단" : "취소"));
      b.onclick = () =>
        api("DELETE", "/api/rooms/" + state.roomId + "/turns/" + t.id).catch((e) => alert(e.message));
      li.appendChild(b);
      ul.appendChild(li);
    }
  }

  function renderDiscussion() {
    const bar = $("#discussion-bar");
    const btn = $("#btn-discuss");
    if (!bar || !btn) return;
    const d = state.discussion;
    if (!d) {
      bar.hidden = true;
      btn.disabled = false;
      return;
    }
    bar.hidden = false;
    btn.disabled = true;
    bar.innerHTML = "";
    bar.appendChild(el("b", "", "🗣 토론 진행 중 · " + d.round + " / " + d.total + " 라운드"));
    const sigWrap = el("div", "disc-signals");
    const entries = Object.entries(d.signals || {});
    if (!entries.length) {
      sigWrap.appendChild(el("span", "muted", "발언 및 신호 대기 중…"));
    } else {
      for (const [k, v] of entries) {
        sigWrap.appendChild(el("span", "signal " + v, participantLabel(k) + ": " + v));
      }
    }
    bar.appendChild(sigWrap);
  }

  // ---------- composer, mentions, warnings & shortcuts ----------
  let drawerReturnFocus = null;
  function closeDrawers() {
    document.body.classList.remove("show-left", "show-right");
    const bd = $("#drawer-backdrop");
    if (bd) bd.hidden = true;
    const target = drawerReturnFocus;
    drawerReturnFocus = null;
    if (target && target.offsetParent) target.focus();
  }

  function closeMoreMenu() {
    const more = $("#more-actions");
    if (more && more.open) more.open = false;
  }

  function closeMentionPop() {
    const pop = $("#mention-pop");
    const input = $("#input");
    if (pop) pop.hidden = true;
    if (input) input.removeAttribute("aria-activedescendant");
    state.mentionCands = [];
  }

  function toggleDrawer(side) {
    const cls = side === "left" ? "show-left" : "show-right";
    const other = side === "left" ? "show-right" : "show-left";
    document.body.classList.remove(other);
    document.body.classList.toggle(cls);
    const bd = $("#drawer-backdrop");
    if (bd) {
      bd.hidden = !document.body.classList.contains("show-left") && !document.body.classList.contains("show-right");
    }
  }

  function bindUI() {
    let participantDialogSequence = 0;
    $("#btn-refresh-usage").onclick = () => refreshLimits(true);
    $("#btn-usage").onclick = () => {
      drawerReturnFocus = $("#btn-usage");
      document.body.classList.remove("show-right");
      document.body.classList.add("show-left");
      $("#drawer-backdrop").hidden = false;
      $("#provider-panel").scrollTop = 0;
      $("#usage-title").focus();
    };
    $("#btn-latest-history").onclick = jumpToLatestMessages;
    $("#btn-theme").onclick = toggleTheme;
    $("#btn-new-room").onclick = () => openRoomDialog();
    const onbNew = $("#btn-onboard-new");
    if (onbNew) onbNew.onclick = () => openRoomDialog();

    $$(".prompt-example").forEach((btn) => {
      btn.onclick = () => {
        const promptText = btn.getAttribute("data-prompt") || "";
        if (state.roomId && $("#input")) {
          $("#input").value = promptText;
          $("#input").focus();
          updateHint();
        } else {
          openRoomDialog(null, promptText);
        }
      };
    });

    const searchInput = $("#room-search");
    if (searchInput) {
      searchInput.addEventListener("input", () => {
        state.roomSearch = searchInput.value;
        renderRoomList();
      });
    }

    $("#dlg-room-cancel").onclick = () => $("#dlg-room").close();
    $("#form-room").onsubmit = onRoomSubmit;
    $("#btn-add-seat-spec").onclick = () => addSeatSpec($("#room-seat-specs"));
    $("#btn-add-participant").onclick = () => {
      participantDialogSequence++;
      $("#participant-spec").replaceChildren();
      addSeatSpec($("#participant-spec"), "", false);
      $('#form-participant [type="submit"]').disabled = false;
      $("#dlg-participant").showModal();
    };
    $("#dlg-participant").addEventListener("close", () => { participantDialogSequence++; });
    $("#dlg-participant-cancel").onclick = () => $("#dlg-participant").close();
    $("#form-participant").onsubmit = async (e) => {
      e.preventDefault();
      const button = e.target.querySelector('[type="submit"]');
      const roomId = state.roomId;
      const sequence = openSeq;
      const dialogSequence = participantDialogSequence;
      button.disabled = true;
      try {
        const [spec] = readSeatSpecs($("#participant-spec"));
        if (activeParticipants().length + spec.count > 32) throw new Error("한 방에는 최대 32명까지 참가할 수 있습니다.");
        const room = await api("POST", "/api/rooms/" + roomId + "/participants", spec);
        if (state.roomId !== roomId || openSeq !== sequence || participantDialogSequence !== dialogSequence) return;
        state.room = room;
        renderRoomHead();
        renderParticipants();
        $("#dlg-participant").close();
        announce(spec.count + "명 참가자 추가됨");
      } catch (err) {
        if (state.roomId === roomId && openSeq === sequence && participantDialogSequence === dialogSequence) alert(err.message);
      } finally {
        if (participantDialogSequence === dialogSequence) button.disabled = false;
      }
    };

    $("#composer").onsubmit = (e) => {
      e.preventDefault();
      send();
    };

    const input = $("#input");
    input.addEventListener("keydown", (e) => {
      const pop = $("#mention-pop");
      if (pop && !pop.hidden && state.mentionCands.length > 0) {
        if (e.key === "ArrowDown" || e.key === "ArrowRight") {
          e.preventDefault();
          state.mentionIdx = (state.mentionIdx + 1) % state.mentionCands.length;
          renderMentionButtons();
          announce(state.mentionCands[state.mentionIdx].label);
          return;
        }
        if (e.key === "ArrowUp" || e.key === "ArrowLeft") {
          e.preventDefault();
          state.mentionIdx = (state.mentionIdx - 1 + state.mentionCands.length) % state.mentionCands.length;
          renderMentionButtons();
          announce(state.mentionCands[state.mentionIdx].label);
          return;
        }
        if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && !e.isComposing)) {
          e.preventDefault();
          applyMention(state.mentionCands[state.mentionIdx]);
          return;
        }
        if (e.key === "Escape") {
          e.preventDefault();
          closeMentionPop();
          return;
        }
      }
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
        e.preventDefault();
        send();
      } else if (e.key === "Escape" && state.target) {
        e.preventDefault();
        setTarget("", true);
      }
    });

    input.addEventListener("input", () => {
      clearComposerError();
      mentionPopup();
      updateHint();
    });

    $("#target").onchange = (e) => setTarget(e.target.value, false);

    const autoChk = $("#chk-auto-trace");
    if (autoChk) {
      autoChk.onchange = () => {
        state.autoTrace = autoChk.checked;
        localStorage.setItem("rt.autoTrace", state.autoTrace ? "1" : "0");
        if (state.autoTrace && state.room) {
          state.userCollapsed.clear();
          for (const id of state.order) {
            const m = state.messages.get(id);
            if (m && m.kind === "agent" && (m.status === "streaming" || m.trace > 0)) {
              expandTrace(id);
              if ($("#m-" + id)) ensureAndRenderTrace(id);
            }
          }
        }
      };
    }

    $("#btn-discuss").onclick = () =>
      api("POST", "/api/rooms/" + state.roomId + "/discussion", {
        rounds: Number($("#rounds").value) || 3,
      }).catch((e) => alert(e.message));
    $("#btn-stop").onclick = () =>
      state.roomId && api("POST", "/api/rooms/" + state.roomId + "/stop").catch((e) => alert(e.message));
    $("#btn-reset").onclick = () => {
      closeMoreMenu();
      if (
        state.roomId &&
        confirm("각 에이전트의 CLI 세션을 버릴까요? 다음 턴부터 대화 전체를 새로 읽습니다.")
      ) {
        api("POST", "/api/rooms/" + state.roomId + "/reset").catch((e) => alert(e.message));
      }
    };
    $("#btn-export").onclick = () => closeMoreMenu();
    $("#btn-settings").onclick = () => {
      closeMoreMenu();
      openRoomDialog(state.room);
    };

    // Close more-actions menu when clicking outside
    document.addEventListener("click", (e) => {
      const more = $("#more-actions");
      if (more && more.open && !more.contains(e.target)) {
        more.open = false;
      }
    });

    // Responsive drawers
    $("#btn-right").onclick = () => toggleDrawer("right");
    const btnLeft = $("#btn-left");
    if (btnLeft) btnLeft.onclick = () => toggleDrawer("left");
    const btnEmptyLeft = $("#btn-empty-left");
    if (btnEmptyLeft) btnEmptyLeft.onclick = () => toggleDrawer("left");
    const btnEmptyRight = $("#btn-empty-right");
    if (btnEmptyRight) btnEmptyRight.onclick = () => toggleDrawer("right");
    const btnCloseLeft = $("#btn-close-left");
    if (btnCloseLeft) btnCloseLeft.onclick = closeDrawers;
    const btnCloseRight = $("#btn-close-right");
    if (btnCloseRight) btnCloseRight.onclick = closeDrawers;
    const backdrop = $("#drawer-backdrop");
    if (backdrop) backdrop.onclick = closeDrawers;

    $("#btn-delete").onclick = async () => {
      closeMoreMenu();
      if (!confirm("방과 대화 기록을 영구 삭제할까요? 되돌릴 수 없습니다.")) return;
      await api("DELETE", "/api/rooms/" + state.roomId);
      state.roomId = null;
      state.room = null;
      renderUsagePanel();
      $("#btn-add-participant").disabled = true;
      $("#participants").replaceChildren();
      localStorage.removeItem("rt.room");
      if (state.es) state.es.close();
      $("#room").hidden = true;
      $("#empty").hidden = false;
      loadRooms();
    };

    // Global keyboard shortcuts
    document.addEventListener("keydown", (e) => {
      if (document.querySelector("dialog[open]")) return;
      if ((e.ctrlKey || e.metaKey) && e.key === ".") {
        if (state.roomId) {
          e.preventDefault();
          api("POST", "/api/rooms/" + state.roomId + "/stop").catch((err) => alert(err.message));
          announce("모두 중지 요청됨");
        }
        return;
      }
      if (e.key === "Escape") {
        const pop = $("#mention-pop");
        if (pop && !pop.hidden) {
          closeMentionPop();
          return;
        }
        const more = $("#more-actions");
        if (more && more.open) {
          more.open = false;
          return;
        }
        if (document.body.classList.contains("show-left") || document.body.classList.contains("show-right")) {
          closeDrawers();
          return;
        }
        if (state.target) {
          setTarget("", false);
          return;
        }
      }
      if (e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey) {
        const tag = (document.activeElement && document.activeElement.tagName) || "";
        if (tag !== "INPUT" && tag !== "TEXTAREA" && tag !== "SELECT" && !$("#room").hidden) {
          e.preventDefault();
          $("#input").focus();
        }
      }
    });
  }

  function findDisabledMentions(text) {
    if (!state.room || !text) return [];
    const disabled = new Set(activeParticipants().filter((p) => !p.enabled).map((p) => p.id));
    const found = new Set();
    const re = /(?:^|\s)@([a-zA-Z0-9_-]+)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const id = m[1].toLowerCase();
      const group = activeParticipants().filter((p) => p.provider === id);
      if (group.length ? group.every((p) => !p.enabled) : disabled.has(id)) found.add(id);
    }
    return Array.from(found);
  }

  function updateHint() {
    const hint = $("#composer-hint");
    if (!hint || !state.room) return;
    const text = ($("#input") && $("#input").value) || "";
    const disabledMentions = findDisabledMentions(encodedMentions(text, false));
    const t = state.target;
    const targetParticipant = t ? state.room.participants.find((x) => x.id === t) : null;

    if (targetParticipant && !targetParticipant.enabled) {
      hint.className = "composer-hint warn";
      hint.textContent =
        participantLabel(t) + "의 참여가 꺼져 있습니다. 참가자를 켜거나 대상을 해제하세요.";
      return;
    }
    if (disabledMentions.length > 0) {
      hint.className = "composer-hint warn";
      hint.textContent =
        "⚠ 꺼진 참가자(" +
        disabledMentions.map((id) => participantLabel(id)).join(", ") +
        ")가 멘션되었습니다. 켜지 않으면 해당 에이전트는 답하지 않습니다.";
      return;
    }

    hint.className = "composer-hint muted";
    if (!t) {
      hint.textContent = "이름을 선택하면 한 명에게 · @로 여러 명 선택 · Enter 전송";
      return;
    }
    const info = state.providers.find((x) => x.id === targetParticipant.provider) || {};
    if (targetParticipant && targetParticipant.status === "running") {
      hint.textContent = info.can_steer
        ? participantLabel(t) + " 작업 중 · 다음 메시지를 즉시 전달합니다."
        : participantLabel(t) + " 작업 중 · 현재 답변 뒤에 다음 메시지를 전달합니다.";
    } else {
      hint.textContent =
        participantLabel(t) + "에게만 다음 메시지를 전달합니다. Esc로 해제";
    }
  }

  // Fix #7: Connect mention candidates to accessibility tree via option IDs & aria-activedescendant
  function mentionPopup() {
    const input = $("#input");
    const pop = $("#mention-pop");
    if (!input || !pop || !state.room) return;
    const before = input.value.slice(0, input.selectionStart);
    const m = before.match(/(?:^|\s)@([^\s@\[\]]*)$/u);
    if (!m) {
      closeMentionPop();
      return;
    }
    const q = m[1].toLowerCase();
    const cands = [
      { id: "all", label: "전체 참가자", search: "all 전체", enabled: true },
      ...state.providers.filter((provider) => activeParticipants().some((p) => p.provider === provider.id)).map((provider) => ({
        id: provider.id,
        label: provider.label + " 전체",
        search: provider.id + " " + provider.label,
        enabled: true,
      })),
      ...activeParticipants().filter((p) => !state.providers.some((provider) => provider.id === p.id)).map((p) => ({
        id: p.id,
        label: participantLabel(p.id) + " · " + modelLabel(p.provider, p.model) + (p.enabled ? "" : " · 꺼짐"),
        search: participantLabel(p.id) + " " + modelLabel(p.provider, p.model),
        enabled: p.enabled,
      })),
    ].filter((item) => item.search.toLowerCase().includes(q));

    if (!cands.length) {
      closeMentionPop();
      return;
    }
    state.mentionCands = cands;
    if (state.mentionIdx >= cands.length) state.mentionIdx = 0;
    pop.hidden = false;
    renderMentionButtons();
  }

  function renderMentionButtons() {
    const pop = $("#mention-pop");
    const input = $("#input");
    if (!pop) return;
    pop.innerHTML = "";
    state.mentionCands.forEach((cand, idx) => {
      const optId = "mention-opt-" + idx;
      const isSelected = idx === state.mentionIdx;
      const b = el("button", isSelected ? "active" : "", cand.label);
      b.type = "button";
      b.id = optId;
      b.setAttribute("role", "option");
      b.setAttribute("aria-selected", isSelected ? "true" : "false");
      b.onclick = () => applyMention(cand);
      pop.appendChild(b);
      if (isSelected && input && !pop.hidden) {
        input.setAttribute("aria-activedescendant", optId);
      }
    });
    const selected = pop.querySelector('[aria-selected="true"]');
    if (selected) selected.scrollIntoView({ block: "nearest" });
  }

  function applyMention(cand) {
    if (!cand) return;
    clearComposerError();
    const input = $("#input");
    const before = input.value.slice(0, input.selectionStart);
    const after = input.value.slice(input.selectionStart);
    let token = "@" + cand.id;
    if (participant(cand.id) && !state.providers.some((p) => p.id === cand.id)) {
      const name = participantLabel(cand.id).replace(/[\[\]\n]/g, " ");
      token = "@[" + name + "]";
      let suffix = 2;
      while (state.draftMentions.has(token) && state.draftMentions.get(token) !== cand.id) token = "@[" + name + " · " + suffix++ + "]";
      state.draftMentions.set(token, cand.id);
    }
    const replaced = before.replace(/@[^\s@\[\]]*$/u, token + " ");
    input.value = replaced + after;
    input.selectionStart = input.selectionEnd = replaced.length;
    closeMentionPop();
    input.focus();
    updateHint();
  }

  function clearComposerError() {
    const error = $("#composer-error");
    error.hidden = true;
    error.textContent = "";
    $("#input").removeAttribute("aria-invalid");
  }

  async function send() {
    const input = $("#input");
    const draft = input.value.trim();
    let text;
    try { text = encodedMentions(draft); }
    catch (err) {
      const error = $("#composer-error");
      error.hidden = false;
      error.textContent = err.message;
      input.setAttribute("aria-invalid", "true");
      input.focus();
      return;
    }
    clearComposerError();
    if (!text || !state.roomId) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    const target = state.target;
    input.value = "";
    closeMentionPop();
    try {
      await api("POST", "/api/rooms/" + roomId + "/messages", { text, target });
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (!input.value) state.draftMentions.clear();
      if (target) {
        setTarget("", false);
      } else {
        updateHint();
      }
    } catch (e) {
      if (state.roomId !== roomId || openSeq !== sequence) return;
      alert(e.message);
      if (!input.value) input.value = draft;
      updateHint();
    }
  }

  // ---------- room dialog ----------
  function addSeatSpec(box, providerId = "", removable = true) {
    const row = el("div", "seat-spec");
    function field(text, control) {
      const label = el("label", "", text);
      label.appendChild(control);
      row.appendChild(label);
      return control;
    }
    const provider = field("공급사", el("select", "seat-provider"));
    provider.required = true;
    const providerPlaceholder = el("option", "", "이용 중인 공급사 선택");
    providerPlaceholder.value = "";
    provider.appendChild(providerPlaceholder);
    for (const info of state.providers) {
      const option = el("option", "", info.label + (info.available === false ? " (CLI 없음)" : ""));
      option.value = info.id;
      option.disabled = info.available === false;
      provider.appendChild(option);
    }
    provider.value = providerId;
    const catalogNote = el("p", "catalog-note small-note warn");
    catalogNote.setAttribute("role", "status");
    row.appendChild(catalogNote);
    const catalogRefresh = el("button", "catalog-refresh", "모델 목록 새로고침");
    catalogRefresh.type = "button";
    catalogRefresh.onclick = () => refreshProviderCatalog(catalogRefresh, () => provider.onchange());
    row.appendChild(catalogRefresh);
    const filter = field("모델 검색", el("input", "seat-model-filter"));
    filter.type = "search"; filter.placeholder = "공급사 또는 모델 이름";
    const model = field("모델", el("select", "seat-model"));
    model.required = true;
    const custom = field("직접 입력할 모델 ID", el("input", "seat-custom-model"));
    custom.placeholder = "CLI가 지원하는 정확한 모델 ID";
    const effort = field("추론 수준", el("select", "seat-effort"));
    const count = field("인원", el("input", "seat-count"));
    count.type = "number";
    count.min = "1";
    count.max = "32";
    count.value = "1";
    count.required = true;
    const label = field("이름 (선택)", el("input", "seat-label"));
    label.maxLength = 80;
    label.placeholder = "예: 설계 검토자";
    const syncEffort = () => {
      const info = state.providers.find((p) => p.id === provider.value);
      const selected = model.value === "__custom__" ? custom.value.trim() : model.value;
      const efforts = selected ? getEffortsForModel(info, selected) : [];
      syncEffortSelect(effort, efforts, info.default_effort);
      effort.parentElement.hidden = !efforts.length;
    };
    const syncModel = () => {
      custom.parentElement.hidden = model.value !== "__custom__";
      custom.required = model.value === "__custom__";
      syncEffort();
    };
    provider.onchange = () => {
      model.disabled = !provider.value;
      effort.disabled = !provider.value;
      custom.disabled = !provider.value;
      if (!provider.value) {
        const modelPlaceholder = el("option", "", "공급사를 먼저 선택하세요");
        modelPlaceholder.value = ""; model.replaceChildren(modelPlaceholder);
        const effortPlaceholder = el("option", "", "공급사 선택 후 설정");
        effortPlaceholder.value = ""; effort.replaceChildren(effortPlaceholder);
        effort.hidden = false; effort.parentElement.hidden = false;
        delete effort.dataset.effKey;
        custom.required = false; custom.parentElement.hidden = true;
        filter.parentElement.hidden = true;
        catalogNote.hidden = true; catalogRefresh.hidden = true;
        return;
      }
      const info = state.providers.find((p) => p.id === provider.value);
      catalogNote.textContent = catalogMessage(info);
      catalogNote.hidden = !catalogNote.textContent;
      catalogRefresh.textContent = provider.value === "opencode" ? "연결·모델 새로고침" : "모델 목록 새로고침";
      catalogRefresh.hidden = provider.value !== "opencode" && !catalogNote.textContent;
      filter.value = "";
      filter.parentElement.hidden = !info.models || info.models.length < 30;
      fillModelSelect(model, info.models || [], info.default_model, "", true);
      syncModel();
    };
    filter.oninput = () => {
      const info = state.providers.find((p) => p.id === provider.value);
      fillModelSelect(model, info.models || [], model.value, filter.value, true);
    };
    model.onchange = syncModel;
    custom.oninput = syncEffort;
    provider.onchange();
    if (removable) {
      const remove = el("button", "ghost", "구성 제거");
      remove.type = "button";
      remove.onclick = () => row.remove();
      row.appendChild(remove);
    }
    box.appendChild(row);
  }

  function readSeatSpecs(box) {
    return $$(".seat-spec", box).map((row) => ({
      provider: $(".seat-provider", row).value,
      model: $(".seat-model", row).value === "__custom__" ? $(".seat-custom-model", row).value.trim() : $(".seat-model", row).value,
      effort: $(".seat-effort", row).hidden ? "" : $(".seat-effort", row).value,
      label: $(".seat-label", row).value.trim(),
      count: Number($(".seat-count", row).value),
    }));
  }

  let pendingInitialPrompt = "";
  function openRoomDialog(room, initialPrompt = "") {
    pendingInitialPrompt = room ? "" : initialPrompt;
    const f = $("#form-room");
    $("#dlg-room-title").textContent = room ? "방 설정" : "새 방";
    $("#dlg-room-ok").textContent = room ? "저장" : "만들기";
    f.title.value = room ? room.title : "";
    f.workspace.value = room ? room.workspace : state.config.default_workspace || "";
    f.mode.value = room ? room.mode : "read";
    f.note.value = room ? room.note : "";
    f.max_chain.value = room ? room.max_chain : 4;
    f.discussion_rounds.value = room ? room.discussion_rounds : 3;
    f.persona.checked = room ? !!room.persona : false;
    $("#room-seats-fieldset").hidden = !!room;
    $("#room-seat-specs").replaceChildren();
    if (!room) {
      addSeatSpec($("#room-seat-specs"));
    }
    f.dataset.editing = room ? room.id : "";
    $("#dlg-room").showModal();
  }

  async function onRoomSubmit(e) {
    e.preventDefault();
    const f = e.target;
    const submit = $("#dlg-room-ok");
    submit.disabled = true;
    const body = {
      title: f.title.value.trim(),
      workspace: f.workspace.value.trim(),
      mode: f.mode.value,
      note: f.note.value,
      max_chain: Number(f.max_chain.value),
      discussion_rounds: Number(f.discussion_rounds.value),
      persona: f.persona.checked,
    };
    try {
      if (f.dataset.editing) {
        await api("PATCH", "/api/rooms/" + f.dataset.editing, body);
      } else {
        body.participant_specs = readSeatSpecs($("#room-seat-specs"));
        const total = body.participant_specs.reduce((sum, spec) => sum + spec.count, 0);
        if (total < 1 || total > 32) throw new Error("참가자를 1명 이상, 총 32명 이하로 구성하세요.");
        const r = await api("POST", "/api/rooms", body);
        await loadRooms();
        await openRoom(r.id);
        if (pendingInitialPrompt && $("#input")) {
          $("#input").value = pendingInitialPrompt;
          $("#input").focus();
          updateHint();
          pendingInitialPrompt = "";
        }
      }
      $("#dlg-room").close();
    } catch (err) {
      alert(err.message);
    } finally {
      submit.disabled = false;
    }
  }

  boot().catch((e) => {
    document.body.innerHTML = "<pre style='padding:20px'>시작 실패: " + esc(e.message) + "</pre>";
  });
})();
