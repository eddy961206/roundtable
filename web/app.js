/* Roundtable UI v2 client: one page, SSE-driven, WCAG 2.2 AA, no framework. */
(() => {
  "use strict";

  const DOM_CAP_STEP = 200;
  const STREAM_THROTTLE_MS = 125; // max 8 renders/sec per message

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

  const LABEL = { claude: "Claude", codex: "Codex", agy: "Antigravity", user: "나", system: "시스템" };

  if (typeof marked !== "undefined") {
    marked.use({ gfm: true, breaks: true, renderer: { html({ text }) { return esc(text); } } });
  }
  const md = (text) => {
    try {
      return typeof marked !== "undefined" ? marked.parse(text || "") : "<p>" + esc(text) + "</p>";
    } catch {
      return "<p>" + esc(text) + "</p>";
    }
  };

  const api = async (method, url, body) => {
    const res = await fetch(url, {
      method,
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
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
    visibleCount: DOM_CAP_STEP,
    queue: [],
    discussion: null,
    traces: new Map(),
    traceFetching: new Set(),
    expanded: new Set(),
    autoTrace: localStorage.getItem("rt.autoTrace") === "1",
    target: "",
    mentionCands: [],
    mentionIdx: 0,
    es: null,
    renderTimers: new Map(),
    lastEvent: 0,
  };

  function announce(msg) {
    const box = $("#live-announcer");
    if (!box || !msg) return;
    box.textContent = "";
    setTimeout(() => { box.textContent = msg; }, 20);
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
    renderLimits();
    await loadRooms();
    const last = localStorage.getItem("rt.room");
    if (last && state.rooms.some((r) => r.id === last)) {
      await openRoom(last);
    }
    bindUI();
    setInterval(refreshLimits, 60000);
    setInterval(watchdog, 5000);
    setInterval(tickElapsed, 1000);
  }

  // Watchdog: reopen closed EventSource and resync stalled streaming turns
  function watchdog() {
    if (!state.roomId) return;
    if (state.es && state.es.readyState === 2) {
      connect(state.roomId);
      openRoom(state.roomId, true);
      return;
    }
    const streaming = state.order.some((id) => (state.messages.get(id) || {}).status === "streaming");
    if (streaming && Date.now() - (state.lastEvent || 0) > 30000) {
      state.lastEvent = Date.now();
      openRoom(state.roomId, true);
    }
  }

  // Update live elapsed time badges and 10m burn rate without full DOM rebuilds
  function tickElapsed() {
    if (!state.roomId || !state.room) return;
    let anyRunning = false;
    $$("[data-msg-elapsed]").forEach((node) => {
      const id = node.getAttribute("data-msg-elapsed");
      const m = state.messages.get(id);
      if (m && m.status === "streaming") {
        anyRunning = true;
        node.textContent = "⏱ " + fmtElapsed(Date.now() - (m.ts || Date.now()));
      }
    });
    $$("[data-agent-elapsed]").forEach((node) => {
      const pid = node.getAttribute("data-agent-elapsed");
      const ms = getAgentElapsedMs(pid);
      if (ms != null) {
        anyRunning = true;
        node.textContent = "⏱ " + fmtElapsed(ms);
      }
    });
    if (anyRunning) {
      const rateNode = $("#room-burn-rate");
      if (rateNode) rateNode.textContent = fmtK(calcRecentBurnRate()) + " 토큰/분";
    }
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

  async function refreshLimits() {
    try {
      const prov = await api("GET", "/api/providers");
      state.providers = prov.providers || state.providers;
      state.limits = prov.limits || {};
      renderLimits();
      if (state.room) {
        renderTokenStrip();
        renderParticipants();
      }
    } catch {}
  }

  async function loadRooms() {
    state.rooms = (await api("GET", "/api/rooms")) || [];
    renderRoomList();
  }

  function renderRoomList() {
    const ul = $("#room-list");
    if (!ul) return;
    ul.innerHTML = "";
    const q = state.roomSearch.trim().toLowerCase();
    const filtered = state.rooms.filter((r) => !q || (r.title || "").toLowerCase().includes(q) || (r.mode || "").toLowerCase().includes(q));
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

  // Compact provider CLI summary in left sidebar (detailed gauges live in #token-strip and #participants)
  function renderLimits() {
    const box = $("#limits");
    if (!box) return;
    box.innerHTML = "";
    for (const p of state.providers) {
      const row = el("div", "prov-mini");
      const name = el("span", "prov-name " + p.id, LABEL[p.id] || p.label || p.id);
      row.appendChild(name);
      const statusText = !p.available
        ? "CLI 없음"
        : p.can_steer
        ? "준비됨 · 즉시 조정"
        : "준비됨";
      const st = el("span", p.available ? "muted" : "unavailable", statusText);
      row.appendChild(st);
      box.appendChild(row);
    }
  }

  // Gauge helpers:
  // usageGauge(usedPct): higher % is worse (>=90 bad, >=80 warn)
  function usageGauge(usedPct, mini) {
    const pct = Math.min(100, Math.max(0, Number(usedPct) || 0));
    const cls = "gauge" + (pct >= 90 ? " bad" : pct >= 80 ? " warn" : "") + (mini ? " mini" : "");
    const g = el("div", cls);
    const i = el("i");
    i.style.width = pct.toFixed(0) + "%";
    g.appendChild(i);
    return g;
  }

  // remainGauge(remainPct): lower remaining % is worse (<=15 bad, <=30 warn)
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
  async function openRoom(id, resync) {
    const my = ++openSeq;
    if (!resync) {
      if (state.es) {
        state.es.close();
        state.es = null;
      }
      state.roomId = id;
      state.visibleCount = DOM_CAP_STEP;
      state.target = "";
      localStorage.setItem("rt.room", id);
    }
    const snap = await api("GET", "/api/rooms/" + id);
    if (my !== openSeq || state.roomId !== id) return; // race guard: a newer room selection won
    state.messages.clear();
    state.order = [];
    state.traces.clear();
    state.traceFetching.clear();
    if (!resync) state.expanded.clear();
    state.room = snap.room;
    state.queue = snap.queue || [];
    state.discussion = snap.discussion;
    if (snap.limits) {
      state.limits = snap.limits;
      renderLimits();
    }
    for (const m of snap.messages || []) {
      state.messages.set(m.id, m);
      state.order.push(m.id);
      if (state.autoTrace && m.kind === "agent" && (m.status === "streaming" || m.trace > 0)) {
        state.expanded.add(m.id);
      }
    }
    $("#empty").hidden = true;
    const roomSec = $("#room");
    roomSec.hidden = false;
    roomSec.dataset.mode = state.room.mode || "read";

    renderRoomHead();
    renderTimeline(true);
    renderParticipants();
    renderQueue();
    renderDiscussion();
    renderRoomList();
    updateHint();
    if (!resync) connect(id);
  }

  function connect(id) {
    const es = new EventSource("/api/rooms/" + id + "/stream");
    state.es = es;
    state.lastEvent = Date.now();
    es.onmessage = (ev) => {
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
      const hint = $("#composer-hint");
      if (hint) {
        hint.className = "composer-hint warn";
        hint.textContent = "⚠ 서버와 SSE 연결이 끊겼습니다 — 재연결 시도 중…";
      }
    };
    es.addEventListener("hello", () => {
      updateHint();
      if (connected) openRoom(id, true); // reconnected: reload snapshot so missed events are recovered
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
        if (state.traces.has(data.msg_id)) {
          const list = state.traces.get(data.msg_id);
          list.push(data.event);
          if (m) m.trace = list.length;
        } else if (m) {
          m.trace = (m.trace || 0) + 1;
        }
        if (m) {
          if (state.autoTrace && !state.expanded.has(m.id)) {
            state.expanded.add(m.id);
          }
          if (state.expanded.has(m.id)) {
            ensureAndRenderTrace(m.id);
          } else {
            updateTraceCount(m.id);
          }
          if (m.status === "streaming") scheduleRender(m.id);
        }
        break;
      }
      case "participant": {
        if (!state.room) break;
        const idx = state.room.participants.findIndex((p) => p.id === data.id);
        if (idx >= 0) state.room.participants[idx] = data;
        renderParticipants();
        renderTokenStrip();
        updateHint();
        for (const id of state.order) {
          const m = state.messages.get(id);
          if (m && m.status === "streaming" && m.author === data.id) scheduleRender(id);
        }
        break;
      }
      case "queue":
        state.queue = data || [];
        renderQueue();
        break;
      case "discussion":
        state.discussion = data;
        renderDiscussion();
        break;
      case "limits":
        state.limits = data || {};
        renderLimits();
        if (state.room) {
          renderTokenStrip();
          renderParticipants();
        }
        break;
      case "room":
        state.room = data;
        if ($("#room")) $("#room").dataset.mode = data.mode || "read";
        renderRoomHead();
        renderParticipants();
        updateHint();
        loadRooms();
        break;
    }
  }

  function upsertMessage(m) {
    const prev = state.messages.get(m.id);
    const existed = !!prev;
    state.messages.set(m.id, m);
    if (!existed) {
      state.order.push(m.id);
      if (m.kind === "agent" && m.status === "streaming" && !m.trace) {
        state.traces.set(m.id, []);
      }
      if (state.autoTrace && m.kind === "agent") {
        state.expanded.add(m.id);
      }
    }
    renderMessage(m.id, !existed);
    if (state.room) renderTokenStrip();
    if (!existed) loadRooms();

    // Announce to screen readers only on completion or non-streaming messages (Section 7)
    if (m.kind === "system" && !existed) {
      announce("시스템 알림: " + m.text);
    } else if (m.kind === "user" && !existed) {
      announce("내 메시지 전송됨");
    } else if (m.kind === "agent" && m.status !== "streaming" && (!prev || prev.status === "streaming")) {
      const who = LABEL[m.author] || m.author;
      if (m.status === "error") announce(who + " 턴 오류 발생: " + (m.error || ""));
      else if (m.status === "cancelled") announce(who + " 턴 중단됨");
      else announce(who + " 답변 완료");
    }
  }

  function renderRoomHead() {
    const r = state.room;
    if (!r) return;
    $("#room-title").textContent = r.title;
    const badge = $("#room-mode");
    const modeLabel = r.mode === "write" ? "⚠ write (수정 허용)" : r.mode;
    badge.textContent = modeLabel;
    badge.className = "badge " + r.mode;
    badge.title =
      r.mode === "write"
        ? "write 모드: 샌드박스 내 파일 수정이 허용됩니다"
        : r.mode === "read"
        ? "read 모드: 작업 폴더 읽기·검색만 허용됩니다"
        : "chat 모드: 도구 없이 대화만 진행합니다";
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
    renderTokenStrip();
  }

  // ---------- steering target controls (1-shot chip + Esc + select sync) ----------
  function renderTargetControls() {
    const r = state.room;
    if (!r) return;
    const sel = $("#target");
    if (sel) {
      sel.innerHTML = '<option value="">전체 (@멘션 규칙)</option>';
      for (const p of r.participants) {
        const o = el("option", "", "@" + p.id + " 에게만");
        o.value = p.id;
        sel.appendChild(o);
      }
      if (state.target && !r.participants.some((p) => p.id === state.target)) {
        state.target = "";
      }
      sel.value = state.target;
    }

    const chipsBox = $("#target-chips");
    if (chipsBox) {
      chipsBox.innerHTML = "";
      const allBtn = el("button", "target-chip-btn" + (state.target === "" ? " active" : ""), "전체");
      allBtn.type = "button";
      allBtn.setAttribute("aria-pressed", state.target === "" ? "true" : "false");
      allBtn.title = "특정 대상 지정 없이 @멘션 규칙(또는 전원)으로 전송";
      allBtn.onclick = () => setTarget("");
      chipsBox.appendChild(allBtn);

      for (const p of r.participants) {
        const isAct = state.target === p.id;
        const b = el("button", "target-chip-btn" + (isAct ? " active" : ""), "@" + p.id);
        b.type = "button";
        b.setAttribute("aria-pressed", isAct ? "true" : "false");
        b.title = "@" + p.id + "에게만 1회성 조정 메시지 보내기 (전송 후 자동 해제)";
        b.onclick = () => setTarget(isAct ? "" : p.id, true);
        chipsBox.appendChild(b);
      }
    }

    const actChip = $("#target-active-chip");
    if (actChip) {
      if (!state.target) {
        actChip.hidden = true;
        actChip.innerHTML = "";
      } else {
        const p = r.participants.find((x) => x.id === state.target);
        const info = state.providers.find((x) => x.id === state.target) || {};
        const modeTxt =
          p && p.status === "running"
            ? info.can_steer
              ? "⚡ 실행 중 즉시 주입"
              : "⏳ 현재 턴 직후 전달"
            : "🎯 1회성 단독 전달";
        actChip.hidden = false;
        actChip.innerHTML = "";
        actChip.appendChild(el("span", "", "@" + state.target + " 조정 · " + modeTxt));
        const clr = el("button", "", "× Esc");
        clr.type = "button";
        clr.setAttribute("aria-label", "@" + state.target + " 대상 지정 해제 (Esc)");
        clr.onclick = () => setTarget("", true);
        actChip.appendChild(clr);
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
      announce("@" + state.target + " 1회성 조정 대상으로 지정됨. Esc 키로 해제할 수 있습니다.");
    } else if (focusInput) {
      announce("대상 지정이 해제되어 전체 대상으로 돌아왔습니다.");
    }
    if (focusInput && $("#input")) $("#input").focus();
  }

  // ---------- Usage Dashboard Strip (Brief Section 4) ----------
  function messageInputTokens(u) {
    if (!u || u.input_tokens == null) return 0;
    return (
      Number(u.input_tokens || 0) +
      Number(u.cache_read_input_tokens || u.cached_input_tokens || u.cache_read_tokens || 0)
    );
  }

  function maxTurnInputForAgent(pid) {
    let maxTok = 0;
    for (const m of state.messages.values()) {
      if (m.kind === "agent" && m.author === pid && m.usage) {
        const tin = messageInputTokens(m.usage);
        if (tin > maxTok) maxTok = tin;
        const ctx = Number(m.usage.context_tokens || 0);
        if (ctx > maxTok) maxTok = ctx;
      }
    }
    const p = state.room && state.room.participants.find((x) => x.id === pid);
    if (p) {
      if (p.context_tokens > maxTok) maxTok = p.context_tokens;
      if (p.usage) {
        const uin = messageInputTokens(p.usage);
        if (uin > maxTok) maxTok = uin;
      }
    }
    return maxTok;
  }

  // Recent 10-minute token burn rate (tokens / min)
  function calcRecentBurnRate() {
    const cutoff = Date.now() - 10 * 60 * 1000;
    let recentTokens = 0;
    for (const m of state.messages.values()) {
      if (m.ts >= cutoff && m.usage) {
        const tin = messageInputTokens(m.usage);
        const tout = Number(m.usage.output_tokens || 0);
        recentTokens += tin + tout;
      }
    }
    return Math.round(recentTokens / 10);
  }

  function findWindow(windows, type) {
    if (!Array.isArray(windows)) return null;
    if (type === "5h") {
      return windows.find((w) => /5\s*시간|5\s*h|300/i.test(w.label || ""));
    }
    if (type === "7d") {
      return windows.find((w) => /7\s*일|주간|7\s*d|week/i.test(w.label || ""));
    }
    return null;
  }

  function renderTokenStrip() {
    const strip = $("#token-strip");
    if (!strip || !state.room) return;
    strip.innerHTML = "";

    const providerOrder = state.providers.length
      ? state.providers.map((p) => p.id)
      : state.room.participants.map((p) => p.id);

    // 1. One chip per provider (Section 4)
    for (const pid of providerOrder) {
      const part = state.room.participants.find((x) => x.id === pid);
      const lim = state.limits[pid];
      const wins = (lim && lim.windows) || [];
      const warns = [];
      let isBad = false;

      // Warning condition 1: remaining limit <= 15%
      for (const w of wins) {
        const rem = Math.max(0, 100 - Number(w.used_percent || 0));
        if (rem <= 15) {
          warns.push(w.label + " 남은 한도 " + Math.round(rem) + "% (≤15%)");
          if (rem <= 5) isBad = true;
        }
      }
      // Warning condition 2: context >= 80%
      if (part && part.context_window > 0) {
        const ctxPct = (part.context_tokens / part.context_window) * 100;
        if (ctxPct >= 80) {
          warns.push("컨텍스트 사용률 " + Math.round(ctxPct) + "% (≥80%)");
          if (ctxPct >= 90) isBad = true;
        }
      }
      // Warning condition 3: single-turn input >= 150,000 tokens
      const maxTurnIn = maxTurnInputForAgent(pid);
      if (maxTurnIn >= 150000) {
        warns.push("한 턴 입력 " + fmtK(maxTurnIn) + " 토큰 (≥150k)");
      }

      const chip = el("div", "chip" + (warns.length ? (isBad ? " bad" : " warn") : "") + (part && !part.enabled ? " muted" : ""));
      if (warns.length) {
        chip.appendChild(el("span", "chip-warn-icon", "⚠"));
      }
      chip.appendChild(el("b", pid, LABEL[pid] || pid));

      const tipLines = [];
      if (!lim || !wins.length) {
        chip.appendChild(el("span", "muted", "아직 관측 안 됨"));
        tipLines.push((LABEL[pid] || pid) + ": 한도 데이터 아직 관측 안 됨");
      } else {
        const w5 = findWindow(wins, "5h");
        const w7 = findWindow(wins, "7d");

        const appendWinMetric = (label, w) => {
          const span = el("span", "win-metric");
          if (w) {
            const rem = Math.max(0, Math.min(100, 100 - Number(w.used_percent || 0)));
            span.appendChild(el("span", "muted", label));
            span.appendChild(remainGauge(rem, true));
            span.appendChild(el("span", "", Math.round(rem) + "%"));
            const rtxt = w.resets_at ? " (리셋: " + fmtReset(w.resets_at) + ")" : "";
            tipLines.push(w.label + ": 남음 " + rem.toFixed(0) + "% / 사용 " + Number(w.used_percent || 0).toFixed(0) + "%" + rtxt);
          } else {
            span.appendChild(el("span", "muted", label + " -"));
          }
          chip.appendChild(span);
        };

        appendWinMetric("5시간", w5);
        appendWinMetric("주간", w7);

        for (const w of wins) {
          if (w !== w5 && w !== w7) {
            const rem = Math.max(0, Math.min(100, 100 - Number(w.used_percent || 0)));
            tipLines.push(w.label + ": 남음 " + rem.toFixed(0) + "%" + (w.resets_at ? " (리셋: " + fmtReset(w.resets_at) + ")" : ""));
          }
        }
        if (lim.plan) tipLines.push("플랜: " + lim.plan);
        if (lim.credits) tipLines.push("크레딧: " + lim.credits);
        if (lim.note) tipLines.push("비고: " + lim.note);
        if (lim.at) tipLines.push("관측 시각: " + fmtTime(lim.at));
      }

      if (part && part.context_window > 0) {
        const ctxPct = (part.context_tokens / part.context_window) * 100;
        const ctxSpan = el("span", "win-metric muted", "ctx " + Math.round(ctxPct) + "%");
        chip.appendChild(ctxSpan);
        tipLines.push("컨텍스트: " + fmtK(part.context_tokens) + " / " + fmtK(part.context_window) + " (" + ctxPct.toFixed(0) + "%)");
      }

      if (warns.length) {
        tipLines.unshift("⚠ 경고: " + warns.join(" · "));
      }
      chip.title = tipLines.join("\n");
      strip.appendChild(chip);
    }

    // 2. Room Total Chip (입력·출력 합계, 비용, 최근 10분 소모 속도 토큰/분)
    let sumIn = 0;
    let sumOut = 0;
    let sumCost = 0;
    let roomMaxTurnIn = 0;
    for (const p of state.room.participants) {
      const t = p.totals || {};
      sumIn += t.input || 0;
      sumOut += t.output || 0;
      sumCost += t.cost_usd || 0;
      const mIn = maxTurnInputForAgent(p.id);
      if (mIn > roomMaxTurnIn) roomMaxTurnIn = mIn;
    }
    const burnRate = calcRecentBurnRate();
    const roomWarns = [];
    if (roomMaxTurnIn >= 150000) {
      roomWarns.push("한 턴 입력 최대 " + fmtK(roomMaxTurnIn) + " 토큰 (≥150k)");
    }

    const totalChip = el("div", "chip" + (roomWarns.length ? " warn" : ""));
    if (roomWarns.length) {
      totalChip.appendChild(el("span", "chip-warn-icon", "⚠"));
    }
    totalChip.appendChild(el("b", "", "방 합계"));
    totalChip.appendChild(
      el(
        "span",
        "",
        "입력 " +
          fmtK(sumIn) +
          " · 출력 " +
          fmtK(sumOut) +
          (sumCost > 0 ? " · $" + sumCost.toFixed(2) : "")
      )
    );
    const rateWrap = el("span", "muted");
    rateWrap.appendChild(document.createTextNode("최근 10분 "));
    const rateVal = el("b", "", fmtK(burnRate) + " 토큰/분");
    rateVal.id = "room-burn-rate";
    rateWrap.appendChild(rateVal);
    totalChip.appendChild(rateWrap);

    const totalTips = [
      "방 누적 입력 토큰: " + Math.round(sumIn).toLocaleString("ko-KR"),
      "방 누적 출력 토큰: " + Math.round(sumOut).toLocaleString("ko-KR"),
      sumCost > 0 ? "누적 비용(보고된 공급사 합산): $" + sumCost.toFixed(4) : "비용: CLI가 비용을 보고한 턴이 아직 없음",
      "최근 10분 소모 속도: 분당 약 " + burnRate.toLocaleString("ko-KR") + " 토큰",
    ];
    if (roomWarns.length) totalTips.unshift("⚠ 경고: " + roomWarns.join(" · "));
    totalChip.title = totalTips.join("\n");
    strip.appendChild(totalChip);
  }

  // ---------- timeline (with 200 DOM cap & upward scroll pagination) ----------
  function renderTimeline(scrollToBottom) {
    const tl = $("#timeline");
    if (!tl) return;
    tl.innerHTML = "";

    if (!state.order.length) {
      renderRoomEmptyGuide(tl);
      return;
    }

    const total = state.order.length;
    const startIdx = Math.max(0, total - state.visibleCount);
    if (startIdx > 0) {
      const moreBtn = el(
        "button",
        "load-more-btn",
        "↑ 이전 메시지 더 보기 (" + startIdx + "개 숨겨짐 · 위로 스크롤 또는 클릭)"
      );
      moreBtn.type = "button";
      moreBtn.id = "timeline-more";
      moreBtn.onclick = () => loadMoreMessages();
      tl.appendChild(moreBtn);
    }

    for (let i = startIdx; i < total; i++) {
      renderMessage(state.order[i], true, true);
    }
    if (scrollToBottom) tl.scrollTop = tl.scrollHeight;
  }

  function renderRoomEmptyGuide(tl) {
    const box = el("div", "room-empty-guide");
    box.id = "timeline-empty";
    box.appendChild(el("h3", "", "이 방의 첫 대화를 시작해 보세요"));
    box.appendChild(
      el(
        "p",
        "",
        "@claude @codex @agy 로 특정 에이전트를 호출하거나, 멘션 없이 전원에게 동시에 질문할 수 있습니다. 아래 예시를 누르면 입력창에 채워집니다."
      )
    );
    const chips = el("div", "prompt-chips");
    const examples = [
      "이 저장소의 핵심 구조와 데이터 흐름을 각자 읽고 3줄로 요약해줘.",
      "@claude @codex 최근 변경 사항에서 잠재적 버그나 경합 조건이 있는지 검토해줘.",
      "세 에이전트가 각자 장단점을 제시한 뒤 토론 모드로 결론을 내자.",
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

  function loadMoreMessages() {
    const tl = $("#timeline");
    if (!tl) return;
    const total = state.order.length;
    if (state.visibleCount >= total) return;
    const prevHeight = tl.scrollHeight;
    const prevTop = tl.scrollTop;
    state.visibleCount = Math.min(total, state.visibleCount + DOM_CAP_STEP);
    renderTimeline(false);
    tl.scrollTop = prevTop + (tl.scrollHeight - prevHeight);
  }

  function scheduleRender(id) {
    if (state.renderTimers.has(id)) return;
    state.renderTimers.set(
      id,
      setTimeout(() => {
        state.renderTimers.delete(id);
        renderMessage(id, false);
      }, STREAM_THROTTLE_MS)
    );
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
      announce("@" + m.author + " 에게 직전 메시지로 다시 시도 요청함");
    } catch (e) {
      alert(e.message);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function renderMessage(id, isNew, batch) {
    const m = state.messages.get(id);
    if (!m) return;
    const tl = $("#timeline");
    if (!tl) return;

    const emptyGuide = $("#timeline-empty");
    if (emptyGuide) emptyGuide.remove();

    const idx = state.order.indexOf(id);
    const startIdx = Math.max(0, state.order.length - state.visibleCount);
    if (idx !== -1 && idx < startIdx) return;

    const atBottom = tl.scrollHeight - tl.scrollTop - tl.clientHeight < 80;
    let node = $("#m-" + id);
    if (!node) {
      node = el("div");
      node.id = "m-" + id;
      tl.appendChild(node);
    }
    node.className =
      "msg " +
      m.kind +
      (m.author && m.kind === "agent" ? " " + m.author : "") +
      (m.status === "error" ? " error" : "");

    if (m.kind === "system") {
      node.innerHTML =
        '<div class="bubble"><span>ℹ ' +
        esc(m.text) +
        '</span> <span class="muted">' +
        fmtTime(m.ts) +
        "</span></div>";
    } else {
      const who = m.kind === "user" ? "나" : (LABEL[m.author] || m.author) + " @" + m.author;
      const meta = [];
      meta.push('<span class="who ' + esc(m.author) + '">' + esc(who) + "</span>");
      if (m.kind === "user" && m.target) {
        meta.push(
          '<span class="steer-tag">🎯 → @' +
            esc(m.target) +
            (m.live ? " (즉시 주입됨)" : " (다음 턴 전달)") +
            "</span>"
        );
      }
      if (m.model) meta.push('<span class="muted mono">' + esc(m.model) + "</span>");
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

      const bodyHtml =
        m.kind === "user" ? "<p>" + esc(m.text).replace(/\n/g, "<br>") + "</p>" : md(m.text);

      const foot = [];
      if (m.duration_ms) foot.push("⏱ " + fmtDur(m.duration_ms));
      const u = m.usage || {};
      const tin = u.input_tokens != null ? messageInputTokens(u) : null;
      if (tin != null) foot.push("입력 " + fmtK(tin) + " · 출력 " + fmtK(u.output_tokens));
      if (u.total_cost_usd) foot.push("$" + Number(u.total_cost_usd).toFixed(3));
      if (u.context_tokens) foot.push("ctx " + fmtK(u.context_tokens));

      // Preserve existing trace container so open <details> aren't lost during streaming deltas
      let bubble = node.querySelector(".bubble");
      if (!bubble) {
        node.innerHTML =
          '<div class="avatar ' +
          esc(m.author) +
          '" aria-hidden="true">' +
          esc((LABEL[m.author] || m.author || "?")[0]) +
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
          setTimeout(() => { b.textContent = "복사"; }, 1200);
        };
        preEl.appendChild(b);
      });

      const errSlot = bubble.querySelector(".err-slot");
      errSlot.innerHTML = "";
      if (m.error || m.status === "error") {
        const errBox = el("div", "err-box");
        errBox.appendChild(el("span", "", "⚠ 원인: " + (m.error || "에이전트 실행 실패")));
        if (m.kind === "agent" && m.status === "error") {
          const rBtn = el("button", "retry-btn", "↻ 다시 시도");
          rBtn.type = "button";
          rBtn.title = "@" + m.author + " 에게 직전 사용자 메시지를 다시 요청합니다";
          rBtn.onclick = () => retryTurn(m.id, rBtn);
          errBox.appendChild(rBtn);
        }
        errSlot.appendChild(errBox);
      }

      const footEl = bubble.querySelector(".foot");
      footEl.innerHTML = foot.map((f) => "<span>" + esc(f) + "</span>").join("");
      if (m.kind === "agent") {
        const isExp = state.expanded.has(id);
        const tb = el(
          "button",
          "trace-btn",
          (isExp ? "▾ 작업 기록 접기 (" : "▸ 작업 기록 (") + (m.trace || 0) + ")"
        );
        tb.type = "button";
        tb.dataset.id = id;
        tb.setAttribute("aria-expanded", isExp ? "true" : "false");
        tb.onclick = () => toggleTrace(id);
        footEl.appendChild(tb);
      }

      if (state.expanded.has(id)) {
        ensureAndRenderTrace(id);
      }
    }

    // Enforce DOM cap when at bottom and new messages arrive
    if (!batch && atBottom) {
      const msgNodes = tl.querySelectorAll(".msg");
      if (msgNodes.length > state.visibleCount) {
        const excess = msgNodes.length - state.visibleCount;
        for (let i = 0; i < excess; i++) {
          msgNodes[i].remove();
        }
      }
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
      b.textContent = (isExp ? "▾ 작업 기록 접기 (" : "▸ 작업 기록 (") + (m.trace || 0) + ")";
      b.setAttribute("aria-expanded", isExp ? "true" : "false");
    }
  }

  async function ensureAndRenderTrace(id) {
    if (!state.traces.has(id)) {
      if (state.traceFetching.has(id)) return;
      state.traceFetching.add(id);
      try {
        const events = await api("GET", "/api/rooms/" + state.roomId + "/trace/" + id);
        state.traces.set(id, events || []);
      } catch {
        state.traces.set(id, []);
      } finally {
        state.traceFetching.delete(id);
      }
    }
    renderTrace(id);
    updateTraceCount(id);
  }

  async function toggleTrace(id) {
    const box = $("#t-" + id);
    if (state.expanded.has(id)) {
      state.expanded.delete(id);
      if (box) box.hidden = true;
      updateTraceCount(id);
      return;
    }
    state.expanded.add(id);
    await ensureAndRenderTrace(id);
  }

  // Trace: tool calls merged with their results, sub-agents as nested blocks.
  function renderTrace(id) {
    const box = $("#t-" + id);
    if (!box) return;
    box.hidden = false;
    const events = state.traces.get(id) || [];
    const items = [];
    const byCall = new Map();
    const subs = new Map();
    for (const e of events) {
      switch (e.kind) {
        case "tool_call": {
          const it = { type: "tool", call: e };
          items.push(it);
          if (e.id) byCall.set(e.id, it);
          break;
        }
        case "tool_result": {
          const it = e.id && byCall.get(e.id);
          if (it) it.result = e;
          else items.push({ type: "tool", result: e });
          break;
        }
        case "subagent_start": {
          const s = { type: "sub", start: e, lines: [] };
          items.push(s);
          subs.set(e.parent, s);
          break;
        }
        case "subagent_msg": {
          let s = subs.get(e.parent);
          if (!s) {
            s = { type: "sub", start: { name: e.parent, parent: e.parent }, lines: [] };
            items.push(s);
            subs.set(e.parent, s);
          }
          s.lines.push(e);
          break;
        }
        case "subagent_end": {
          const s = subs.get(e.parent);
          if (s) s.end = e;
          else items.push({ type: "plain", e });
          break;
        }
        case "text_delta":
        case "text":
          break;
        default:
          items.push({ type: "plain", e });
      }
    }
    const html = items.map(renderItem).join("");
    box.innerHTML = html || '<div class="muted">기록 없음</div>';
  }

  const pre = (s) => (s ? "<pre>" + esc(String(s).slice(0, 20000)) + "</pre>" : "");
  const inputPretty = (s) => {
    try {
      const o = JSON.parse(s);
      return JSON.stringify(o, null, 1);
    } catch {
      return s;
    }
  };

  function renderItem(it) {
    if (it.type === "tool") {
      const c = it.call || {},
        r = it.result || {};
      const name = c.name || r.name || "도구";
      const err = r.meta && (r.meta.error || (r.meta.exit_code != null && r.meta.exit_code !== 0));
      const st = r.kind ? (err ? "⚠ 실패" : "✓ 완료") : "⏳ 실행 중";
      return (
        '<details class="item"><summary>🔧 <b>' +
        esc(name) +
        "</b> " +
        st +
        ' <span class="muted">' +
        esc(summarize(c.input || r.input)) +
        "</span></summary>" +
        (c.input || r.input ? '<div class="k">입력</div>' + pre(inputPretty(c.input || r.input)) : "") +
        (r.output ? '<div class="k">결과</div>' + pre(r.output) : "") +
        "</details>"
      );
    }
    if (it.type === "sub") {
      const s = it.start,
        e = it.end;
      const title =
        "🧩 서브에이전트 <b>" +
        esc(s.name || s.parent) +
        "</b> " +
        (e ? "✓ 종료" : "⏳ 진행 중") +
        (s.meta && s.meta.description ? ' <span class="muted">' + esc(s.meta.description) + "</span>" : "");
      const lines = it.lines.map(renderSubLine).join("");
      return (
        '<details class="item sub" open><summary>' +
        title +
        "</summary>" +
        (s.input ? '<div class="line"><span class="k">과제</span>' + pre(s.input) + "</div>" : "") +
        '<div class="lines">' +
        lines +
        "</div>" +
        (e && e.output ? '<div class="line"><span class="k">보고</span>' + pre(e.output) + "</div>" : "") +
        "</details>"
      );
    }
    const e = it.e;
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

  function summarize(s) {
    if (!s) return "";
    s = String(s).replace(/\s+/g, " ");
    return s.length > 90 ? s.slice(0, 90) + "…" : s;
  }

  // ---------- Right Panel: Participant Cards & Queue (Brief Section 3 & 5) ----------
  function getEffortsForModel(info, modelId) {
    if (info && info.model_efforts && Array.isArray(info.model_efforts[modelId])) {
      return info.model_efforts[modelId];
    }
    return info && Array.isArray(info.efforts) ? info.efforts : [];
  }

  function pickFallbackEffort(effList) {
    if (!effList || !effList.length) return "";
    return effList.length >= 2 ? effList[effList.length - 2] : effList[0];
  }

  function renderParticipants() {
    const box = $("#participants");
    if (!box || !state.room) return;
    box.innerHTML = "";

    for (const p of state.room.participants) {
      const info = state.providers.find((x) => x.id === p.id) || {};
      const card = el("div", "pcard" + (p.enabled ? "" : " off"));

      // 1. Header: Name + Steer shortcut + Switch
      const top = el("div", "top");
      top.appendChild(el("span", "name " + p.id, (LABEL[p.id] || p.label || p.id) + " @" + p.id));
      const steerBtn = el("button", "ghost", "🎯 조정");
      steerBtn.type = "button";
      steerBtn.style.padding = "2px 8px";
      steerBtn.style.minHeight = "24px";
      steerBtn.style.fontSize = "11px";
      steerBtn.title = "@" + p.id + " 에게만 1회성 조정 메시지 보내기";
      steerBtn.onclick = () => {
        closeDrawers();
        setTarget(p.id, true);
      };
      top.appendChild(steerBtn);

      const sw = el("button", "switch" + (p.enabled ? " on" : ""));
      sw.type = "button";
      sw.setAttribute("role", "switch");
      sw.setAttribute("aria-checked", p.enabled ? "true" : "false");
      sw.setAttribute("aria-label", (LABEL[p.id] || p.id) + " 참여 켜기/끄기");
      sw.title = p.enabled ? "참여 중 (클릭하여 끄기)" : "꺼짐 (클릭하여 켜기)";
      sw.onclick = () => patchParticipant(p.id, { enabled: !p.enabled });
      top.appendChild(sw);
      card.appendChild(top);

      if (info.available === false) {
        card.appendChild(el("div", "unavailable", "⚠ CLI를 찾지 못함 (" + (info.command || p.id) + ")"));
      }

      // 2. Model & Reasoning Effort selectors (Brief Section 5: model_efforts[model] -> efforts)
      const selRow = el("div", "sel");
      const ms = el("select");
      ms.title = "모델 선택";
      ms.setAttribute("aria-label", (LABEL[p.id] || p.id) + " 모델");
      for (const m of info.models || []) {
        const o = el("option", "", m.label);
        o.value = m.id;
        ms.appendChild(o);
      }
      if (p.model && ![...ms.options].some((o) => o.value === p.model)) {
        const o = el("option", "", p.model);
        o.value = p.model;
        ms.appendChild(o);
      }
      ms.value = p.model;
      selRow.appendChild(ms);

      const es = el("select");
      es.title = "추론 노력 선택";
      es.setAttribute("aria-label", (LABEL[p.id] || p.id) + " 추론 노력");

      const populateEfforts = (modelId, currentEffort) => {
        const effList = getEffortsForModel(info, modelId);
        es.innerHTML = "";
        if (!effList.length) {
          es.hidden = true;
          return { effList, selected: "" };
        }
        es.hidden = false;
        for (const e of effList) {
          const o = el("option", "", e);
          o.value = e;
          es.appendChild(o);
        }
        const selected = effList.includes(currentEffort)
          ? currentEffort
          : pickFallbackEffort(effList);
        es.value = selected;
        return { effList, selected };
      };

      populateEfforts(p.model, p.effort || info.default_effort);
      es.onchange = () => patchParticipant(p.id, { effort: es.value });

      ms.onchange = () => {
        const newModel = ms.value;
        const curEff = es.hidden ? p.effort : es.value;
        const { selected } = populateEfforts(newModel, curEff);
        patchParticipant(p.id, { model: newModel, effort: selected });
      };

      selRow.appendChild(es);
      card.appendChild(selRow);

      // 3. Single-line Status · Current Tool · Elapsed Time (Brief J1 & Section 2.1)
      const stLine = el("div", "status-line " + (p.status || "idle"));
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
        stMain.textContent = p.enabled ? "✓ 대기 (idle)" : "⏸ 비활성 (꺼짐)";
        stLine.appendChild(stMain);
      }
      card.appendChild(stLine);

      // 4. Stats: Context gauge + Room cumulative tokens
      const t = p.totals || {};
      const stats = el("div", "stats");
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
      addStatRow("완료 턴", (p.turns || 0) + "턴" + (p.session_id ? " · 세션 " + p.session_id.slice(0, 8) + "…" : ""));
      card.appendChild(stats);

      // 5. Provider 5h & Weekly Remaining Limits inside participant card (Brief Section 3)
      const limBox = el("div", "pcard-limits");
      const lim = state.limits[p.id];
      const wins = (lim && lim.windows) || [];
      if (!lim || !wins.length) {
        const r = el("div", "stat-row");
        r.appendChild(el("span", "muted", "공급사 한도"));
        r.appendChild(el("span", "muted", "아직 관측 안 됨"));
        limBox.appendChild(r);
      } else {
        for (const w of wins) {
          const rem = Math.max(0, Math.min(100, 100 - Number(w.used_percent || 0)));
          const wRow = el("div", "limit-win-row");
          wRow.appendChild(el("span", "muted", w.label));
          wRow.appendChild(remainGauge(rem, false));
          wRow.appendChild(
            el("span", "", "남음 " + rem.toFixed(0) + "%" + (w.resets_at ? " · " + fmtReset(w.resets_at) : ""))
          );
          limBox.appendChild(wRow);
        }
        const extra = [lim.plan ? "플랜 " + lim.plan : "", lim.credits ? "크레딧 " + lim.credits : "", lim.note || ""]
          .filter(Boolean)
          .join(" · ");
        if (extra) {
          limBox.appendChild(el("div", "small-note muted", extra));
        }
      }
      card.appendChild(limBox);

      box.appendChild(card);
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
        el("span", "", (t.status === "running" ? "▶ 실행 " : "⏳ 대기 ") + "@" + t.agent + " · " + reasonTxt)
      );
      const b = el("button", t.status === "running" ? "danger" : "", t.status === "running" ? "중단" : "취소");
      b.type = "button";
      b.setAttribute("aria-label", "@" + t.agent + " 턴 " + (t.status === "running" ? "중단" : "취소"));
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
        sigWrap.appendChild(el("span", "signal " + v, "@" + k + ": " + v));
      }
    }
    bar.appendChild(sigWrap);
  }

  // ---------- composer, mentions, warnings & shortcuts ----------
  function closeDrawers() {
    document.body.classList.remove("show-left", "show-right");
    const bd = $("#drawer-backdrop");
    if (bd) bd.hidden = true;
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
          return;
        }
        if (e.key === "ArrowUp" || e.key === "ArrowLeft") {
          e.preventDefault();
          state.mentionIdx = (state.mentionIdx - 1 + state.mentionCands.length) % state.mentionCands.length;
          renderMentionButtons();
          return;
        }
        if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && !e.isComposing)) {
          e.preventDefault();
          applyMention(state.mentionCands[state.mentionIdx]);
          return;
        }
        if (e.key === "Escape") {
          e.preventDefault();
          pop.hidden = true;
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
          for (const id of state.order) {
            const m = state.messages.get(id);
            if (m && m.kind === "agent" && (m.status === "streaming" || m.trace > 0)) {
              state.expanded.add(id);
              ensureAndRenderTrace(id);
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
    $("#btn-reset").onclick = () =>
      state.roomId &&
      confirm("각 에이전트의 CLI 세션을 버릴까요? 다음 턴부터 대화 전체를 새로 읽습니다.") &&
      api("POST", "/api/rooms/" + state.roomId + "/reset").catch((e) => alert(e.message));
    $("#btn-settings").onclick = () => openRoomDialog(state.room);

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
      if (!confirm("방과 대화 기록을 영구 삭제할까요? 되돌릴 수 없습니다.")) return;
      await api("DELETE", "/api/rooms/" + state.roomId);
      state.roomId = null;
      state.room = null;
      localStorage.removeItem("rt.room");
      if (state.es) state.es.close();
      $("#room").hidden = true;
      $("#empty").hidden = false;
      loadRooms();
    };

    // Timeline scroll-up pagination for >200 messages
    const tl = $("#timeline");
    if (tl) {
      tl.addEventListener("scroll", () => {
        if (tl.scrollTop <= 36 && state.order.length > state.visibleCount) {
          loadMoreMessages();
        }
      });
    }

    // Global keyboard shortcuts (Section 2 Principle 7: Ctrl+. stop all, / focus input, Esc clear target/drawers)
    document.addEventListener("keydown", (e) => {
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
          pop.hidden = true;
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
    const disabled = new Set(state.room.participants.filter((p) => !p.enabled).map((p) => p.id));
    const found = new Set();
    const re = /(?:^|\s)@([a-zA-Z0-9_-]+)/g;
    let m;
    while ((m = re.exec(text)) !== null) {
      const id = m[1].toLowerCase();
      if (disabled.has(id)) found.add(id);
    }
    return Array.from(found);
  }

  function updateHint() {
    const hint = $("#composer-hint");
    if (!hint || !state.room) return;
    const text = ($("#input") && $("#input").value) || "";
    const disabledMentions = findDisabledMentions(text);
    const t = state.target;
    const targetParticipant = t ? state.room.participants.find((x) => x.id === t) : null;

    // Warn immediately while typing if a disabled participant is mentioned or targeted (Section 2 Principle 5)
    if (targetParticipant && !targetParticipant.enabled) {
      hint.className = "composer-hint warn";
      hint.textContent =
        "⚠ 조정 대상 @" + t + " 가 현재 꺼져 있습니다. 오른쪽 참가자 카드에서 켜거나 대상을 해제(Esc)하세요.";
      return;
    }
    if (disabledMentions.length > 0) {
      hint.className = "composer-hint warn";
      hint.textContent =
        "⚠ 꺼진 참가자(" +
        disabledMentions.map((id) => "@" + id).join(", ") +
        ")가 멘션되었습니다. 켜지 않으면 해당 에이전트는 답하지 않습니다.";
      return;
    }

    hint.className = "composer-hint muted";
    if (!t) {
      hint.textContent = "팁: @로 특정 에이전트 호출 · 대상 조정 칩으로 1회성 단독 지시 (Esc 해제, Ctrl+. 모두 중지)";
      return;
    }
    const info = state.providers.find((x) => x.id === t) || {};
    if (targetParticipant && targetParticipant.status === "running") {
      hint.textContent = info.can_steer
        ? "⚡ @" + t + " 실행 중 — 보내면 진행 중인 작업에 즉시 주입됩니다(steering). 전송 후 전체 대상으로 자동 복귀."
        : "⏳ @" + t + " 실행 중 — 이 CLI는 즉시 주입을 지원하지 않아 현재 턴이 끝난 직후 우선 전달됩니다.";
    } else {
      hint.textContent =
        "🎯 @" + t + " 에게만 1회성으로 전달됩니다 (다른 참가자는 보지 않음 · 전송 후 자동 해제 · Esc 취소).";
    }
  }

  function mentionPopup() {
    const input = $("#input");
    const pop = $("#mention-pop");
    if (!input || !pop || !state.room) return;
    const before = input.value.slice(0, input.selectionStart);
    const m = before.match(/(?:^|\s)@([\w-]*)$/);
    if (!m) {
      pop.hidden = true;
      state.mentionCands = [];
      return;
    }
    const q = m[1].toLowerCase();
    const cands = [
      { id: "all", label: "@all (활성 전원)", enabled: true },
      ...state.room.participants.map((p) => ({
        id: p.id,
        label: "@" + p.id + " (" + (LABEL[p.id] || p.id) + (p.enabled ? "" : " · 꺼짐") + ")",
        enabled: p.enabled,
      })),
    ].filter((item) => item.id.startsWith(q));

    if (!cands.length) {
      pop.hidden = true;
      state.mentionCands = [];
      return;
    }
    state.mentionCands = cands;
    if (state.mentionIdx >= cands.length) state.mentionIdx = 0;
    renderMentionButtons();
    pop.hidden = false;
  }

  function renderMentionButtons() {
    const pop = $("#mention-pop");
    if (!pop) return;
    pop.innerHTML = "";
    state.mentionCands.forEach((cand, idx) => {
      const b = el("button", idx === state.mentionIdx ? "active" : "", cand.label);
      b.type = "button";
      b.setAttribute("role", "option");
      b.setAttribute("aria-selected", idx === state.mentionIdx ? "true" : "false");
      b.onclick = () => applyMention(cand);
      pop.appendChild(b);
    });
  }

  function applyMention(cand) {
    if (!cand) return;
    const input = $("#input");
    const pop = $("#mention-pop");
    const before = input.value.slice(0, input.selectionStart);
    const after = input.value.slice(input.selectionStart);
    const replaced = before.replace(/@[\w-]*$/, "@" + cand.id + " ");
    input.value = replaced + after;
    input.selectionStart = input.selectionEnd = replaced.length;
    pop.hidden = true;
    state.mentionCands = [];
    input.focus();
    updateHint();
  }

  async function send() {
    const input = $("#input");
    const text = input.value.trim();
    if (!text || !state.roomId) return;
    const target = state.target;
    input.value = "";
    $("#mention-pop").hidden = true;
    state.mentionCands = [];
    try {
      await api("POST", "/api/rooms/" + state.roomId + "/messages", { text, target });
      if (target) {
        setTarget("", false); // one-shot: automatically reset target after sending
      } else {
        updateHint();
      }
    } catch (e) {
      alert(e.message);
      input.value = text;
      updateHint();
    }
  }

  // ---------- room dialog ----------
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
    f.querySelectorAll('input[name="p"]').forEach((c) => {
      c.disabled = !!room;
      c.checked = room ? room.participants.some((p) => p.id === c.value) : true;
    });
    f.dataset.editing = room ? room.id : "";
    $("#dlg-room").showModal();
  }

  async function onRoomSubmit(e) {
    e.preventDefault();
    const f = e.target;
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
        body.participants = [...f.querySelectorAll('input[name="p"]:checked')].map((c) => c.value);
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
    }
  }

  boot().catch((e) => {
    document.body.innerHTML = "<pre style='padding:20px'>시작 실패: " + esc(e.message) + "</pre>";
  });
})();
