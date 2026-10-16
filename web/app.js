/* Roundtable client: one page, SSE-driven. No framework. */
(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);
  const el = (tag, cls, text) => { const n = document.createElement(tag); if (cls) n.className = cls; if (text != null) n.textContent = text; return n; };
  const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
  const fmtK = (n) => { n = Number(n) || 0; return n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1000 ? (n / 1000).toFixed(1) + "k" : String(Math.round(n)); };
  const fmtTime = (ms) => new Date(ms).toLocaleTimeString("ko-KR", { hour: "2-digit", minute: "2-digit", second: "2-digit" });
  const fmtDur = (ms) => ms >= 60000 ? (ms / 60000).toFixed(1) + "분" : (ms / 1000).toFixed(1) + "초";
  const fmtReset = (sec) => { if (!sec) return ""; const d = new Date(sec * 1000); const diff = d - Date.now(); const h = Math.max(0, Math.round(diff / 3600000)); return h >= 24 ? Math.round(h / 24) + "일 후" : h + "시간 후"; };
  const LABEL = { claude: "Claude", codex: "Codex", agy: "Antigravity", user: "나", system: "시스템" };

  marked.use({ gfm: true, breaks: true, renderer: { html({ text }) { return esc(text); } } });
  const md = (text) => {
    try { return marked.parse(text || ""); } catch { return "<p>" + esc(text) + "</p>"; }
  };

  const api = async (method, url, body) => {
    const res = await fetch(url, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    return data;
  };

  const state = {
    providers: [], limits: {}, config: {},
    rooms: [], roomId: null, room: null, messages: new Map(), order: [], queue: [], discussion: null,
    traces: new Map(), expanded: new Set(), es: null, renderTimers: new Map(),
  };

  // ---------- boot ----------
  async function boot() {
    const [prov, cfg] = await Promise.all([api("GET", "/api/providers"), api("GET", "/api/config")]);
    state.providers = prov.providers; state.limits = prov.limits || {}; state.config = cfg;
    renderLimits();
    await loadRooms();
    const last = localStorage.getItem("rt.room");
    if (last && state.rooms.some((r) => r.id === last)) openRoom(last);
    bindUI();
    setInterval(refreshLimits, 60000);
  }

  async function refreshLimits() {
    try { const prov = await api("GET", "/api/providers"); state.limits = prov.limits || {}; renderLimits(); } catch {}
  }

  async function loadRooms() {
    state.rooms = await api("GET", "/api/rooms") || [];
    renderRoomList();
  }

  function renderRoomList() {
    const ul = $("#room-list"); ul.innerHTML = "";
    for (const r of state.rooms) {
      const li = el("li", r.id === state.roomId ? "active" : "");
      li.appendChild(el("span", "name", r.title));
      const right = el("span");
      right.appendChild(el("span", "badge " + r.mode, r.mode));
      if (r.active) right.appendChild(el("span", "dot"));
      li.appendChild(right);
      li.onclick = () => openRoom(r.id);
      ul.appendChild(li);
    }
  }

  function renderLimits() {
    const box = $("#limits"); box.innerHTML = "";
    for (const p of state.providers) {
      const wrap = el("div", "limit");
      const head = el("div", "head");
      head.appendChild(el("b", p.id, LABEL[p.id]));
      head.appendChild(el("span", "muted", p.available ? (p.can_steer ? "즉시 조정 가능" : "") : "CLI 없음"));
      wrap.appendChild(head);
      const lim = state.limits[p.id];
      if (!lim) {
        wrap.appendChild(el("div", "gauge-row", p.id === "agy" ? "한도: CLI가 노출하지 않음" : "한도: 첫 턴 이후 표시"));
      } else {
        for (const w of lim.windows || []) {
          const row = el("div", "gauge-row");
          row.appendChild(el("span", "", w.label + " 사용 " + w.used_percent.toFixed(0) + "%"));
          row.appendChild(el("span", "", "남음 " + (100 - w.used_percent).toFixed(0) + "% · 리셋 " + fmtReset(w.resets_at)));
          wrap.appendChild(row);
          wrap.appendChild(gauge(w.used_percent));
        }
        const extra = [lim.plan ? "플랜 " + lim.plan : "", lim.credits ? "크레딧 " + lim.credits : "", lim.note || ""].filter(Boolean).join(" · ");
        if (extra) wrap.appendChild(el("div", "gauge-row", extra));
        wrap.appendChild(el("div", "gauge-row muted", "관측 " + fmtTime(lim.at)));
      }
      box.appendChild(wrap);
    }
  }

  function gauge(pct, small) {
    const g = el("div", "gauge" + (pct >= 90 ? " bad" : pct >= 70 ? " warn" : "") + (small ? " ctx" : ""));
    const i = el("i"); i.style.width = Math.min(100, Math.max(0, pct)) + "%"; g.appendChild(i);
    return g;
  }

  // ---------- room ----------
  async function openRoom(id) {
    if (state.es) { state.es.close(); state.es = null; }
    state.roomId = id; localStorage.setItem("rt.room", id);
    state.messages.clear(); state.order = []; state.traces.clear(); state.expanded.clear();
    const snap = await api("GET", "/api/rooms/" + id);
    state.room = snap.room; state.queue = snap.queue || []; state.discussion = snap.discussion;
    if (snap.limits) { state.limits = snap.limits; renderLimits(); }
    for (const m of snap.messages || []) { state.messages.set(m.id, m); state.order.push(m.id); }
    $("#empty").hidden = true; $("#room").hidden = false;
    renderRoomHead(); renderTimeline(true); renderParticipants(); renderQueue(); renderDiscussion(); renderRoomList();
    connect(id);
  }

  function connect(id) {
    const es = new EventSource("/api/rooms/" + id + "/stream");
    state.es = es;
    es.onmessage = (ev) => {
      let msg; try { msg = JSON.parse(ev.data.replace(/\\n/g, "\n")); } catch { return; }
      handle(msg.type, msg.data);
    };
    es.onerror = () => { $("#composer-hint").textContent = "연결 끊김 — 다시 연결 중…"; };
    es.addEventListener("hello", () => { $("#composer-hint").textContent = ""; });
  }

  function handle(type, data) {
    switch (type) {
      case "message": upsertMessage(data); break;
      case "delta": {
        const m = state.messages.get(data.id); if (!m) return;
        m.text = (m.text || "") + (data.block && m.text ? "\n\n" : "") + data.text;
        scheduleRender(m.id); break;
      }
      case "trace": {
        const list = state.traces.get(data.msg_id) || []; list.push(data.event); state.traces.set(data.msg_id, list);
        const m = state.messages.get(data.msg_id); if (m) { m.trace = list.length; if (state.expanded.has(m.id)) renderTrace(m.id); else updateTraceCount(m.id); }
        break;
      }
      case "participant": {
        const idx = state.room.participants.findIndex((p) => p.id === data.id);
        if (idx >= 0) state.room.participants[idx] = data; renderParticipants(); renderTokenStrip();
        for (const id of state.order) { const m = state.messages.get(id); if (m && m.status === "streaming" && m.author === data.id) scheduleRender(id); }
        break;
      }
      case "queue": state.queue = data || []; renderQueue(); break;
      case "discussion": state.discussion = data; renderDiscussion(); break;
      case "limits": state.limits = data || {}; renderLimits(); if (state.room) renderTokenStrip(); break;
      case "room": state.room = data; renderRoomHead(); renderParticipants(); loadRooms(); break;
    }
  }

  function upsertMessage(m) {
    const existed = state.messages.has(m.id);
    state.messages.set(m.id, m);
    if (!existed) state.order.push(m.id);
    renderMessage(m.id, !existed);
    if (!existed) loadRooms();
  }

  function renderRoomHead() {
    const r = state.room;
    $("#room-title").textContent = r.title;
    const badge = $("#room-mode"); badge.textContent = r.mode; badge.className = "badge " + r.mode;
    $("#room-ws").textContent = r.workspace || "(작업 폴더 없음)";
    $("#rounds").value = r.discussion_rounds || 3;
    $("#btn-export").href = "/api/rooms/" + r.id + "/export.md";
    const sel = $("#target"); const cur = sel.value; sel.innerHTML = '<option value="">전체 (@멘션 규칙)</option>';
    for (const p of r.participants) { const o = el("option", "", "@" + p.id + " 에게만"); o.value = p.id; sel.appendChild(o); }
    sel.value = cur;
    renderTokenStrip();
  }

  function renderTokenStrip() {
    const strip = $("#token-strip"); strip.innerHTML = "";
    let sumIn = 0, sumOut = 0, sumCost = 0;
    for (const p of state.room.participants) {
      const t = p.totals || {}; sumIn += t.input || 0; sumOut += t.output || 0; sumCost += t.cost_usd || 0;
      const chip = el("span", "chip" + (p.enabled ? "" : " muted"));
      chip.appendChild(el("b", p.id, "@" + p.id));
      chip.appendChild(el("span", "", "입력 " + fmtK(t.input) + " · 출력 " + fmtK(t.output) + (t.cost_usd ? " · $" + t.cost_usd.toFixed(2) : "")));
      if (p.context_window) {
        const pct = (p.context_tokens / p.context_window) * 100;
        chip.appendChild(el("span", "muted", "ctx " + fmtK(p.context_tokens) + "/" + fmtK(p.context_window)));
        chip.appendChild(gauge(pct, true));
      }
      strip.appendChild(chip);
    }
    const warns = [];
    for (const p of state.room.participants) {
      if (!p.enabled) continue;
      if (p.context_window && p.context_tokens / p.context_window >= 0.8) warns.push("@" + p.id + " 컨텍스트 " + Math.round(p.context_tokens / p.context_window * 100) + "%");
      for (const w of (state.limits[p.id] || {}).windows || []) if (w.used_percent >= 85) warns.push("@" + p.id + " " + w.label + " 한도 " + Math.round(w.used_percent) + "%");
    }
    if (warns.length) { const wchip = el("span", "chip warn", "⚠ " + warns.join(" · ")); strip.appendChild(wchip); }
    const total = el("span", "chip");
    total.appendChild(el("b", "", "방 누적"));
    total.appendChild(el("span", "", "입력 " + fmtK(sumIn) + " · 출력 " + fmtK(sumOut) + (sumCost ? " · $" + sumCost.toFixed(2) : "")));
    strip.appendChild(total);
  }

  // ---------- timeline ----------
  function renderTimeline(scroll) {
    const tl = $("#timeline"); tl.innerHTML = "";
    for (const id of state.order) renderMessage(id, true, true);
    if (scroll) tl.scrollTop = tl.scrollHeight;
  }

  function scheduleRender(id) {
    if (state.renderTimers.has(id)) return;
    state.renderTimers.set(id, setTimeout(() => { state.renderTimers.delete(id); renderMessage(id, false); }, 120));
  }

  function renderMessage(id, isNew, batch) {
    const m = state.messages.get(id); if (!m) return;
    const tl = $("#timeline");
    const atBottom = tl.scrollHeight - tl.scrollTop - tl.clientHeight < 80;
    let node = $("#m-" + id);
    if (!node) { node = el("div"); node.id = "m-" + id; tl.appendChild(node); }
    node.className = "msg " + m.kind + (m.author && m.kind === "agent" ? " " + m.author : "") + (m.status === "error" ? " error" : "");
    if (m.kind === "system") {
      node.innerHTML = '<div class="bubble">' + esc(m.text) + ' <span class="muted">' + fmtTime(m.ts) + "</span></div>";
    } else {
      const who = m.kind === "user" ? "나" : "@" + m.author;
      const meta = [];
      meta.push('<span class="who ' + esc(m.author) + '">' + esc(who) + "</span>");
      if (m.kind === "user" && m.target) meta.push('<span class="steer-tag">→ @' + esc(m.target) + (m.live ? " (실행 중 주입)" : " (다음 턴)") + "</span>");
      if (m.model) meta.push('<span class="muted">' + esc(m.model) + "</span>");
      if (m.reason === "discussion") meta.push('<span class="badge">토론 ' + m.round + "R</span>");
      else if (m.reason === "mention") meta.push('<span class="badge">호출</span>');
      meta.push('<span class="muted">' + fmtTime(m.ts) + "</span>");
      if (m.status === "streaming") {
        const p = state.room.participants.find((x) => x.id === m.author);
        meta.push('<span class="live">● ' + esc((p && p.status_text) || "작성 중") + "</span>");
      }
      if (m.status === "cancelled") meta.push('<span class="badge">중단됨</span>');
      if (m.signal) meta.push('<span class="signal ' + m.signal + '">' + m.signal + "</span>");
      const body = m.kind === "user" ? "<p>" + esc(m.text).replace(/\n/g, "<br>") + "</p>" : md(m.text);
      const foot = [];
      if (m.duration_ms) foot.push(fmtDur(m.duration_ms));
      const u = m.usage || {};
      const tin = u.input_tokens != null ? Number(u.input_tokens) + Number(u.cache_read_input_tokens || u.cached_input_tokens || u.cache_read_tokens || 0) : null;
      if (tin != null) foot.push("입력 " + fmtK(tin) + " · 출력 " + fmtK(u.output_tokens));
      if (u.total_cost_usd) foot.push("$" + Number(u.total_cost_usd).toFixed(3));
      if (u.context_tokens) foot.push("ctx " + fmtK(u.context_tokens));
      node.innerHTML =
        '<div class="avatar ' + esc(m.author) + '">' + esc((LABEL[m.author] || m.author || "?")[0]) + "</div>" +
        '<div class="bubble"><div class="meta">' + meta.join("") + "</div>" +
        '<div class="body' + (m.status === "streaming" ? " cursor" : "") + '">' + body + "</div>" +
        (m.error ? '<div class="err">⚠ ' + esc(m.error) + "</div>" : "") +
        '<div class="foot">' + foot.map((f) => "<span>" + esc(f) + "</span>").join("") +
        (m.kind === "agent" ? '<button class="trace-btn" data-id="' + id + '">작업 기록 ' + (m.trace || 0) + "</button>" : "") +
        "</div>" + '<div class="trace" id="t-' + id + '" hidden></div></div>';
      node.querySelectorAll("pre").forEach((pre) => {
        const b = el("button", "copy", "복사");
        b.onclick = () => navigator.clipboard.writeText(pre.innerText.replace(/^복사\n?/, ""));
        pre.appendChild(b);
      });
      const tb = node.querySelector(".trace-btn");
      if (tb) tb.onclick = () => toggleTrace(id);
      if (state.expanded.has(id)) renderTrace(id);
    }
    if (!batch && (atBottom || (isNew && m.kind === "user"))) tl.scrollTop = tl.scrollHeight;
  }

  function updateTraceCount(id) {
    const b = $("#m-" + id + " .trace-btn"); const m = state.messages.get(id);
    if (b && m) b.textContent = "작업 기록 " + (m.trace || 0);
  }

  async function toggleTrace(id) {
    if (state.expanded.has(id)) { state.expanded.delete(id); $("#t-" + id).hidden = true; return; }
    state.expanded.add(id);
    if (!state.traces.has(id)) {
      const events = await api("GET", "/api/rooms/" + state.roomId + "/trace/" + id);
      state.traces.set(id, events);
    }
    renderTrace(id);
  }

  // Trace: tool calls merged with their results, sub-agents as nested blocks.
  function renderTrace(id) {
    const box = $("#t-" + id); if (!box) return;
    box.hidden = false;
    const events = state.traces.get(id) || [];
    const items = []; const byCall = new Map(); const subs = new Map();
    for (const e of events) {
      switch (e.kind) {
        case "tool_call": { const it = { type: "tool", call: e }; items.push(it); if (e.id) byCall.set(e.id, it); break; }
        case "tool_result": { const it = e.id && byCall.get(e.id); if (it) it.result = e; else items.push({ type: "tool", result: e }); break; }
        case "subagent_start": { const s = { type: "sub", start: e, lines: [] }; items.push(s); subs.set(e.parent, s); break; }
        case "subagent_msg": { let s = subs.get(e.parent); if (!s) { s = { type: "sub", start: { name: e.parent, parent: e.parent }, lines: [] }; items.push(s); subs.set(e.parent, s); } s.lines.push(e); break; }
        case "subagent_end": { const s = subs.get(e.parent); if (s) s.end = e; else items.push({ type: "plain", e }); break; }
        case "text_delta": break;
        case "text": break;
        default: items.push({ type: "plain", e });
      }
    }
    const html = items.map(renderItem).join("");
    box.innerHTML = html || '<div class="muted">기록 없음</div>';
  }

  const pre = (s) => s ? "<pre>" + esc(String(s).slice(0, 20000)) + "</pre>" : "";
  const inputPretty = (s) => { try { const o = JSON.parse(s); return JSON.stringify(o, null, 1); } catch { return s; } };

  function renderItem(it) {
    if (it.type === "tool") {
      const c = it.call || {}, r = it.result || {};
      const name = c.name || r.name || "도구";
      const err = r.meta && (r.meta.error || (r.meta.exit_code != null && r.meta.exit_code !== 0));
      const state = r.kind ? (err ? "⚠ 실패" : "✓ 완료") : "⏳ 실행 중";
      return '<details class="item"><summary>🔧 <b>' + esc(name) + "</b> " + state + " <span class=\"muted\">" + esc(summarize(c.input || r.input)) + "</span></summary>" +
        (c.input || r.input ? "<div class=\"k\">입력</div>" + pre(inputPretty(c.input || r.input)) : "") +
        (r.output ? "<div class=\"k\">결과</div>" + pre(r.output) : "") + "</details>";
    }
    if (it.type === "sub") {
      const s = it.start, e = it.end;
      const title = "🧩 서브에이전트 <b>" + esc(s.name || s.parent) + "</b> " + (e ? "✓ 종료" : "⏳ 진행 중") + (s.meta && s.meta.description ? ' <span class="muted">' + esc(s.meta.description) + "</span>" : "");
      const lines = it.lines.map(renderSubLine).join("");
      return '<details class="item sub" open><summary>' + title + "</summary>" +
        (s.input ? '<div class="line"><span class="k">과제</span>' + pre(s.input) + "</div>" : "") +
        '<div class="lines">' + lines + "</div>" +
        (e && e.output ? '<div class="line"><span class="k">보고</span>' + pre(e.output) + "</div>" : "") + "</details>";
    }
    const e = it.e;
    switch (e.kind) {
      case "status": return '<div class="item status">· ' + esc(e.text) + "</div>";
      case "error": return '<div class="item error">⚠ ' + esc(e.text) + "</div>";
      case "thinking": return '<details class="item"><summary class="thinking">💭 추론</summary>' + pre(e.text) + "</details>";
      case "session": return '<div class="item status">세션 ' + esc(e.text) + (e.meta && e.meta.model ? " · " + esc(e.meta.model) : "") + "</div>";
      case "usage": return '<div class="item status">사용량 ' + esc(Object.entries(e.meta || {}).map(([k, v]) => k + "=" + (typeof v === "number" ? fmtK(v) : v)).join(" ")) + "</div>";
      case "limits": return '<div class="item status">한도 갱신</div>';
      default: return '<div class="item status">' + esc(e.kind) + " " + esc(e.text || "") + "</div>";
    }
  }

  function renderSubLine(e) {
    switch (e.role) {
      case "assistant": return '<div class="line"><span class="k">에이전트</span><div>' + md(e.text) + "</div></div>";
      case "user": return '<div class="line"><span class="k">입력</span>' + pre(e.text) + "</div>";
      case "system": return '<div class="line"><span class="k">시스템</span>' + pre(e.text) + "</div>";
      case "message": return '<div class="line"><span class="k">메시지</span>' + esc(e.name) + pre(e.text) + "</div>";
      case "tool_call": return '<details class="line"><summary>🔧 ' + esc(e.name) + ' <span class="muted">' + esc(summarize(e.input)) + "</span></summary>" + pre(inputPretty(e.input)) + "</details>";
      case "tool_result": return '<details class="line"><summary>↩ 결과' + (e.name ? " " + esc(e.name) : "") + "</summary>" + pre(e.output) + "</details>";
      case "status": return '<div class="line status">· ' + esc(e.text) + "</div>";
      default: return '<div class="line">' + esc(e.text || e.output || "") + "</div>";
    }
  }

  function summarize(s) { if (!s) return ""; s = String(s).replace(/\s+/g, " "); return s.length > 90 ? s.slice(0, 90) + "…" : s; }

  // ---------- right panel ----------
  function renderParticipants() {
    const box = $("#participants"); box.innerHTML = "";
    for (const p of state.room.participants) {
      const info = state.providers.find((x) => x.id === p.id) || {};
      const card = el("div", "pcard" + (p.enabled ? "" : " off"));
      const top = el("div", "top");
      top.appendChild(el("span", "name " + p.id, LABEL[p.id] + " @" + p.id));
      const sw = el("span", "switch" + (p.enabled ? " on" : "")); sw.title = "참여 켜기/끄기";
      sw.onclick = () => patchParticipant(p.id, { enabled: !p.enabled });
      top.appendChild(sw);
      card.appendChild(top);
      if (!info.available) card.appendChild(el("div", "unavailable", "CLI를 찾지 못함 (" + info.command + ")"));
      const sel = el("div", "sel");
      const ms = el("select"); ms.title = "모델";
      for (const m of info.models || []) { const o = el("option", "", m.label); o.value = m.id; ms.appendChild(o); }
      if (![...ms.options].some((o) => o.value === p.model)) { const o = el("option", "", p.model); o.value = p.model; ms.appendChild(o); }
      ms.value = p.model; ms.onchange = () => patchParticipant(p.id, { model: ms.value });
      sel.appendChild(ms);
      if ((info.efforts || []).length) {
        const es = el("select"); es.title = "추론 노력";
        for (const e of info.efforts) { const o = el("option", "", e); o.value = e; es.appendChild(o); }
        es.value = p.effort || info.default_effort; es.onchange = () => patchParticipant(p.id, { effort: es.value });
        sel.appendChild(es);
      }
      card.appendChild(sel);
      const st = el("div", "status " + p.status);
      if (p.status === "running") { st.appendChild(el("span", "spin")); st.appendChild(el("span", "", p.status_text || "실행 중")); }
      else if (p.status === "queued") st.textContent = "대기 중";
      else st.textContent = "대기 (idle)";
      card.appendChild(st);
      const t = p.totals || {};
      const stats = el("div", "stats");
      const row = (k, v) => { stats.appendChild(el("span", "", k)); const b = el("b", "", v); stats.appendChild(b); };
      if (p.context_window) {
        const pct = p.context_tokens / p.context_window * 100;
        row("컨텍스트", fmtK(p.context_tokens) + " / " + fmtK(p.context_window) + " (" + pct.toFixed(0) + "%)");
        stats.appendChild(el("span", "", "")); stats.appendChild(gauge(pct));
      }
      row("이 방 누적", "입력 " + fmtK(t.input) + " · 출력 " + fmtK(t.output) + " · 캐시 " + fmtK(t.cached));
      if (t.cost_usd) row("비용", "$" + t.cost_usd.toFixed(3));
      row("턴", String(p.turns || 0));
      if (p.session_id) row("세션", p.session_id.slice(0, 8) + "…");
      card.appendChild(stats);
      box.appendChild(card);
    }
  }

  async function patchParticipant(id, patch) {
    try { await api("PATCH", "/api/rooms/" + state.roomId, { participants: [{ id, ...patch }] }); } catch (e) { alert(e.message); }
  }

  function renderQueue() {
    const ul = $("#queue"); ul.innerHTML = "";
    if (!state.queue.length) { ul.appendChild(el("li", "muted", "비어 있음")); return; }
    for (const t of state.queue) {
      const li = el("li", t.status);
      li.appendChild(el("span", "", (t.status === "running" ? "▶ " : "· ") + "@" + t.agent + " · " + ({ mention: "호출", broadcast: "전체", discussion: "토론 " + t.round + "R" }[t.reason] || t.reason)));
      const b = el("button", "", t.status === "running" ? "중단" : "취소");
      b.onclick = () => api("DELETE", "/api/rooms/" + state.roomId + "/turns/" + t.id).catch((e) => alert(e.message));
      li.appendChild(b);
      ul.appendChild(li);
    }
  }

  function renderDiscussion() {
    const bar = $("#discussion-bar");
    const d = state.discussion;
    if (!d) { bar.hidden = true; $("#btn-discuss").disabled = false; return; }
    bar.hidden = false; $("#btn-discuss").disabled = true;
    const sig = Object.entries(d.signals || {}).map(([k, v]) => "@" + k + ":" + v).join("  ");
    bar.textContent = "🗣 토론 진행 중 · " + d.round + "/" + d.total + " 라운드 · " + (sig || "응답 대기");
  }

  // ---------- composer ----------
  function bindUI() {
    $("#btn-new-room").onclick = () => openRoomDialog();
    $("#dlg-room-cancel").onclick = () => $("#dlg-room").close();
    $("#form-room").onsubmit = onRoomSubmit;
    $("#btn-send").onclick = send;
    const input = $("#input");
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(); }
      if (e.key === "Tab" && !$("#mention-pop").hidden) { e.preventDefault(); const b = $("#mention-pop button"); if (b) b.click(); }
      if (e.key === "Escape") $("#mention-pop").hidden = true;
    });
    input.addEventListener("input", mentionPopup);
    $("#target").onchange = updateHint;
    $("#btn-discuss").onclick = () => api("POST", "/api/rooms/" + state.roomId + "/discussion", { rounds: Number($("#rounds").value) || 3 }).catch((e) => alert(e.message));
    $("#btn-stop").onclick = () => api("POST", "/api/rooms/" + state.roomId + "/stop").catch((e) => alert(e.message));
    $("#btn-reset").onclick = () => confirm("각 에이전트의 CLI 세션을 버릴까? 다음 턴에 대화 전체를 다시 읽어.") && api("POST", "/api/rooms/" + state.roomId + "/reset").catch((e) => alert(e.message));
    $("#btn-settings").onclick = () => openRoomDialog(state.room);
    $("#btn-right").onclick = () => document.body.classList.toggle("show-right");
    $("#btn-delete").onclick = async () => {
      if (!confirm("방과 기록을 삭제할까? 되돌릴 수 없어.")) return;
      await api("DELETE", "/api/rooms/" + state.roomId);
      state.roomId = null; state.room = null; localStorage.removeItem("rt.room");
      if (state.es) state.es.close();
      $("#room").hidden = true; $("#empty").hidden = false;
      loadRooms();
    };
  }

  function updateHint() {
    const t = $("#target").value; const hint = $("#composer-hint");
    if (!t) { hint.textContent = ""; return; }
    const p = state.room.participants.find((x) => x.id === t);
    const info = state.providers.find((x) => x.id === t) || {};
    if (p && p.status === "running") hint.textContent = info.can_steer ? "@" + t + " 가 실행 중 — 보내면 지금 하던 작업에 바로 끼어들어(steering)." : "@" + t + " 가 실행 중 — 이 CLI는 즉시 주입을 지원하지 않아 현재 턴이 끝난 직후 전달돼.";
    else hint.textContent = "@" + t + " 에게만 전달돼 (다른 참가자는 이 메시지를 보지 않음).";
  }

  function mentionPopup() {
    const input = $("#input"); const pop = $("#mention-pop");
    const before = input.value.slice(0, input.selectionStart);
    const m = before.match(/(?:^|\s)@([\w-]*)$/);
    if (!m || !state.room) { pop.hidden = true; return; }
    const q = m[1].toLowerCase();
    const cands = ["all", ...state.room.participants.map((p) => p.id)].filter((id) => id.startsWith(q));
    if (!cands.length) { pop.hidden = true; return; }
    pop.innerHTML = "";
    for (const id of cands) {
      const b = el("button", "", "@" + id);
      b.onclick = () => { input.value = before.replace(/@[\w-]*$/, "@" + id + " ") + input.value.slice(input.selectionStart); pop.hidden = true; input.focus(); };
      pop.appendChild(b);
    }
    pop.hidden = false;
  }

  async function send() {
    const input = $("#input"); const text = input.value.trim();
    if (!text || !state.roomId) return;
    const target = $("#target").value;
    input.value = ""; $("#mention-pop").hidden = true;
    try {
      await api("POST", "/api/rooms/" + state.roomId + "/messages", { text, target });
      if (target) { $("#target").value = ""; updateHint(); } // one-shot: never leave a private target armed
    } catch (e) { alert(e.message); input.value = text; }
  }

  // ---------- room dialog ----------
  function openRoomDialog(room) {
    const f = $("#form-room");
    $("#dlg-room-title").textContent = room ? "방 설정" : "새 방";
    $("#dlg-room-ok").textContent = room ? "저장" : "만들기";
    f.title.value = room ? room.title : "";
    f.workspace.value = room ? room.workspace : (state.config.default_workspace || "");
    f.mode.value = room ? room.mode : "read";
    f.note.value = room ? room.note : "";
    f.max_chain.value = room ? room.max_chain : 4;
    f.discussion_rounds.value = room ? room.discussion_rounds : 3;
    f.persona.checked = room ? !!room.persona : false;
    f.querySelectorAll('input[name="p"]').forEach((c) => { c.disabled = !!room; c.checked = room ? room.participants.some((p) => p.id === c.value) : true; });
    f.dataset.editing = room ? room.id : "";
    $("#dlg-room").showModal();
  }

  async function onRoomSubmit(e) {
    e.preventDefault();
    const f = e.target;
    const body = { title: f.title.value.trim(), workspace: f.workspace.value.trim(), mode: f.mode.value, note: f.note.value, max_chain: Number(f.max_chain.value), discussion_rounds: Number(f.discussion_rounds.value), persona: f.persona.checked };
    try {
      if (f.dataset.editing) {
        await api("PATCH", "/api/rooms/" + f.dataset.editing, body);
      } else {
        body.participants = [...f.querySelectorAll('input[name="p"]:checked')].map((c) => c.value);
        const r = await api("POST", "/api/rooms", body);
        await loadRooms(); await openRoom(r.id);
      }
      $("#dlg-room").close();
    } catch (err) { alert(err.message); }
  }

  boot().catch((e) => { document.body.innerHTML = "<pre style='padding:20px'>시작 실패: " + esc(e.message) + "</pre>"; });
})();
