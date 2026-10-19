/* Roundtable client: one page, SSE-driven, no framework. */
(() => {
  "use strict";

  const DOM_CAP_STEP = 200;
  const STREAM_THROTTLE_MS = 125; // max 8 renders/sec per message/trace
  const TRACE_EVENT_LIMIT = 200;
  const TRACE_CACHE_LIMIT = 8;
  const MAX_ATTACHMENTS = 4;
  const MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;
  const ALLOWED_IMAGE_MIMES = new Set(["image/png", "image/jpeg", "image/webp"]);

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
  const fmtBytes = (bytes) => {
    const n = Number(bytes);
    if (!Number.isFinite(n) || n < 0) return "0 B";
    if (n < 1024) return Math.round(n) + " B";
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
    return (n / (1024 * 1024)).toFixed(2) + " MB";
  };
  function detectImageMimeFromBytes(bytes) {
    if (!bytes || bytes.length < 3) return "";
    if (
      bytes.length >= 8 &&
      bytes[0] === 0x89 &&
      bytes[1] === 0x50 &&
      bytes[2] === 0x4e &&
      bytes[3] === 0x47 &&
      bytes[4] === 0x0d &&
      bytes[5] === 0x0a &&
      bytes[6] === 0x1a &&
      bytes[7] === 0x0a
    ) {
      return "image/png";
    }
    if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) {
      return "image/jpeg";
    }
    if (
      bytes.length >= 12 &&
      bytes[0] === 0x52 &&
      bytes[1] === 0x49 &&
      bytes[2] === 0x46 &&
      bytes[3] === 0x46 &&
      bytes[8] === 0x57 &&
      bytes[9] === 0x45 &&
      bytes[10] === 0x42 &&
      bytes[11] === 0x50
    ) {
      return "image/webp";
    }
    return "";
  }
  function validateAttachmentCandidate({ mime, magicMime, size, activeCount }) {
    if (activeCount >= MAX_ATTACHMENTS) {
      return { ok: false, code: "count", reason: "최대 4장까지 첨부할 수 있습니다 (4장 초과)" };
    }
    if (magicMime !== undefined) {
      if (!ALLOWED_IMAGE_MIMES.has(magicMime)) {
        return { ok: false, code: "format", reason: "PNG·JPEG·WEBP 형식만 첨부할 수 있습니다" };
      }
    } else if (mime) {
      if (!ALLOWED_IMAGE_MIMES.has(mime)) {
        return { ok: false, code: "format", reason: "PNG·JPEG·WEBP 형식만 첨부할 수 있습니다" };
      }
    }
    if (size <= 0) {
      return { ok: false, code: "size", reason: "빈 파일은 첨부할 수 없습니다" };
    }
    if (size > MAX_ATTACHMENT_BYTES) {
      return { ok: false, code: "size", reason: "파일 크기 10MB 초과 (" + fmtBytes(size) + ")" };
    }
    return { ok: true, code: "", reason: "" };
  }
  const REQUEST_STATUS_LABELS = {
    pending: "대기 (pending)",
    in_progress: "진행 중 (in_progress)",
    done: "완료 (done)",
    rejected: "거절됨 (rejected)",
    cancelled: "취소됨 (cancelled)",
  };
  function requestStatusLabel(status) {
    return REQUEST_STATUS_LABELS[status] ? REQUEST_STATUS_LABELS[status] : String(status);
  }
  function isIncompleteRequest(status) {
    return status === "pending" || status === "in_progress";
  }
  const REQUEST_STATUS_PRIORITY = {
    in_progress: 0,
    pending: 1,
    rejected: 2,
    done: 3,
    cancelled: 4,
  };
  function sortRequests(requests) {
    const list = Array.from(requests);
    return list.sort((a, b) => {
      const incA = isIncompleteRequest(a.status);
      const incB = isIncompleteRequest(b.status);
      if (incA !== incB) return incA ? -1 : 1;
      const priA = REQUEST_STATUS_PRIORITY[a.status] !== undefined ? REQUEST_STATUS_PRIORITY[a.status] : 9;
      const priB = REQUEST_STATUS_PRIORITY[b.status] !== undefined ? REQUEST_STATUS_PRIORITY[b.status] : 9;
      if (priA !== priB) return priA - priB;
      if (a.at !== b.at) {
        return incA ? a.at - b.at : b.at - a.at;
      }
      return String(a.id).localeCompare(String(b.id));
    });
  }
  function mergeSnapshotRequests(currentMap, snapRequests, updatedDuringFetchIds) {
    const next = new Map();
    const updated = updatedDuringFetchIds instanceof Set ? updatedDuringFetchIds : new Set();
    if (Array.isArray(snapRequests)) {
      for (const req of snapRequests) {
        if (!req || !req.id) continue;
        const prev = currentMap && typeof currentMap.get === "function" ? currentMap.get(req.id) : undefined;
        const keepPrev =
          prev &&
          (updated.has(req.id) || (!isIncompleteRequest(prev.status) && isIncompleteRequest(req.status)));
        next.set(req.id, keepPrev ? prev : req);
      }
    }
    if (currentMap && typeof currentMap.entries === "function") {
      for (const [id, prev] of currentMap.entries()) {
        if (!next.has(id) && updated.has(id) && prev) {
          next.set(id, prev);
        }
      }
    }
    return next;
  }
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
  const PROVIDER_ICONS = Object.freeze({
    claude: "claude.svg",
    codex: "codex.svg",
    agy: "gemini.svg",
  });

  function avatarContent(provider, label) {
    const icon = Object.hasOwn(PROVIDER_ICONS, provider) ? PROVIDER_ICONS[provider] : null;
    return icon
      ? '<img src="vendor/provider-icons/' + icon + '" alt="" width="22" height="22" draggable="false">'
      : esc(label[0]);
  }

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

  function stripWorkspacePrefix(rawPath) {
    const norm = String(rawPath ?? "")
      .replace(/\\/g, "/")
      .replace(/^\/+([a-zA-Z]:\/)/, "$1");
    const roots = [];
    if (state.work && Array.isArray(state.work.parts)) {
      for (const p of state.work.parts) {
        if (p && p.worktree) roots.push(p.worktree);
      }
    }
    if (state.room && state.room.workspace) {
      roots.push(state.room.workspace);
    }
    for (const root of roots) {
      const ws = String(root)
        .replace(/\\/g, "/")
        .replace(/^\/+([a-zA-Z]:\/)/, "$1")
        .replace(/\/+$/, "");
      if (!ws) continue;
      const normLower = norm.toLowerCase();
      const wsLower = ws.toLowerCase();
      if (normLower.startsWith(wsLower + "/")) {
        return norm.slice(ws.length + 1);
      }
      if (normLower === wsLower) {
        return norm.split("/").pop() || norm;
      }
      // Also strip sibling git worktree prefix (<workspace>-wt/<branch>/...)
      if (normLower.startsWith(wsLower + "-wt/")) {
        const rest = norm.slice((ws + "-wt/").length);
        const slashIdx = rest.indexOf("/");
        if (slashIdx >= 0 && slashIdx + 1 < rest.length) {
          return rest.slice(slashIdx + 1);
        }
      }
      const wtMatch = ws.match(/^(.*)-wt\/[^/]+$/i);
      if (wtMatch && normLower.startsWith(wtMatch[1].toLowerCase() + "/")) {
        return norm.slice(wtMatch[1].length + 1);
      }
    }
    if (!/^[a-zA-Z]:\//.test(norm)) {
      return norm.replace(/^\/+/, "");
    }
    return norm;
  }

  function formatFileHref(rawHref) {
    let raw = String(rawHref ?? "").trim().replace(/^file:(?:\/\/(?:localhost(?=\/))?)?/i, "");
    try {
      raw = decodeURIComponent(raw);
    } catch {
      // keep raw path when percent sequence is not valid URI encoding
    }
    raw = raw.replace(/\\/g, "/").replace(/^\/+([a-zA-Z]:\/)/, "$1");
    const m = raw.match(/^(.*?)(?:#L?(\d+)(?:-L?(\d+))?|:(\d+)(?:-(\d+))?)?$/i);
    const filePath = m ? m[1] : raw;
    const startLine = m ? m[2] || m[4] || "" : "";
    const endLine = m ? m[3] || m[5] || "" : "";
    const lines = startLine ? ":" + startLine + (endLine ? "-" + endLine : "") : "";
    const relPath = stripWorkspacePrefix(filePath);
    const pathDisplay = (relPath || filePath) + lines;
    const fullPath = filePath + (startLine ? "#L" + startLine + (endLine ? "-L" + endLine : "") : "");
    return { fullPath, relPath, lines, pathDisplay };
  }

  function wrapMarkdownCodeSpan(s) {
    let fence = "`";
    while (s.includes(fence)) {
      fence += "`";
    }
    const pad = s.startsWith("`") || s.endsWith("`") || s.startsWith(" ") || s.endsWith(" ") ? " " : "";
    return fence + pad + s + pad + fence;
  }

  function resolveMessageRequests(msg) {
    if (!msg) return [];
    const out = [];
    const seenObj = new Set();
    const seenId = new Set();
    if (Array.isArray(msg.requests)) {
      for (const item of msg.requests) {
        if (!item) continue;
        if (typeof item === "object") {
          if (!seenObj.has(item)) {
            seenObj.add(item);
            out.push(item);
          }
          if (item.id) seenId.add(item.id);
        } else if (typeof item === "string") {
          seenId.add(item);
          const req = state.requests.get(item);
          if (req && !seenObj.has(req)) {
            seenObj.add(req);
            out.push(req);
          }
        }
      }
    }
    if (msg.id) {
      for (const req of state.requests.values()) {
        if (req && req.source_msg === msg.id && !seenObj.has(req) && !seenId.has(req.id)) {
          seenObj.add(req);
          out.push(req);
        }
      }
    }
    return out;
  }

  function classifyAgentMarkerLine(line, msg) {
    const rawLine = String(line == null ? "" : line);
    if (/^(?: {4,}|\t)/.test(rawLine)) return { action: "keep", raw: line };
    const trimmed = rawLine.trim();
    if (!trimmed) return { action: "keep", raw: line };
    if (
      /^\[\[REQUEST_(?:DONE|REJECT):[^\]]+\]\](?:\s+.*)?$/.test(trimmed) ||
      /^(?:WORK:DONE|\[\[WORK:DONE\]\])$/.test(trimmed) ||
      /^\[\[DISCUSSION:[^\]]+\]\]$/.test(trimmed)
    ) {
      return { action: "hide", raw: trimmed };
    }
    const reqMatch = trimmed.match(/^\[\[REQUEST:@([^\]]+)\]\](?:\s+(.*))?$/);
    if (reqMatch) {
      const targetSeat = reqMatch[1].trim();
      const reqText = (reqMatch[2] !== undefined ? reqMatch[2] : "").trim();
      const reqs = resolveMessageRequests(msg);
      const matched =
        reqText !== "" &&
        reqs.some((r) => {
          if (!r || typeof r.text !== "string") return false;
          if (r.to && r.to !== targetSeat) return false;
          return r.text.trim() === reqText;
        });
      return { action: matched ? "hide" : "chip", raw: trimmed };
    }
    const attMatch = trimmed.match(/^\[\[ATTACH:([^\]]+)\]\]$/);
    if (attMatch) {
      // 첨부는 이름으로만 대조, 거절 사유는 system 메시지로 보임
      const rawPath = attMatch[1].trim().replace(/^["']|["']$/g, "");
      const parts = rawPath.split(/[\\/]/).filter(Boolean);
      const fileName = parts.length > 0 ? parts[parts.length - 1] : rawPath;
      const atts = msg && Array.isArray(msg.attachments) ? msg.attachments : [];
      const matched =
        fileName !== "" &&
        atts.some((a) => a && typeof a.name === "string" && a.name === fileName);
      return { action: matched ? "hide" : "chip", raw: trimmed };
    }
    return { action: "keep", raw: line };
  }

  function prepareAgentMessageText(text, msg) {
    const raw = String(text == null ? "" : text);
    if (!raw) return { text: "", rejectedMarkers: [] };
    const lines = raw.split(/\r?\n/);
    const out = [];
    const rejectedMarkers = [];
    let fenceChar = "";
    let fenceLen = 0;

    for (const line of lines) {
      if (fenceChar) {
        out.push(line);
        const closeMatch = line.match(/^ {0,3}(`{3,}|~{3,})\s*$/);
        if (closeMatch && closeMatch[1][0] === fenceChar && closeMatch[1].length >= fenceLen) {
          fenceChar = "";
          fenceLen = 0;
        }
        continue;
      }
      const openMatch = line.match(/^ {0,3}(`{3,}|~{3,})/);
      if (openMatch) {
        fenceChar = openMatch[1][0];
        fenceLen = openMatch[1].length;
        out.push(line);
        continue;
      }
      const res = classifyAgentMarkerLine(line, msg);
      if (res.action === "hide") {
        continue;
      }
      if (res.action === "chip") {
        rejectedMarkers.push(res.raw);
        out.push(wrapMarkdownCodeSpan(res.raw));
        continue;
      }
      out.push(line);
    }
    return { text: out.join("\n"), rejectedMarkers };
  }

  function formatAgentMessageText(text, msg) {
    return prepareAgentMessageText(text, msg).text;
  }

  function applyRejectedMarkerChips(container, rejectedMarkers) {
    if (!container || !Array.isArray(rejectedMarkers) || rejectedMarkers.length === 0) return;
    const markerSet = new Set(rejectedMarkers);
    container.querySelectorAll("code").forEach((codeEl) => {
      if (codeEl.parentElement && codeEl.parentElement.tagName === "PRE") return;
      const txt = codeEl.textContent || "";
      if (markerSet.has(txt)) {
        codeEl.classList.add("marker-chip", "muted");
      }
    });
  }

  const markedLib =
    typeof marked !== "undefined"
      ? marked
      : typeof require === "function"
      ? (() => {
          try {
            const m = require("./vendor/marked.min.js");
            return m && m.marked ? m.marked : m;
          } catch (_) {
            return undefined;
          }
        })()
      : undefined;

  if (markedLib) {
    markedLib.use({
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
            const info = formatFileHref(href);
            const label = info.pathDisplay || text.trim() || href;
            return '<code class="file-path" title="' + esc(info.fullPath) + '">' + esc(label) + "</code>";
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
      if (!markedLib) return "<p>" + esc(text) + "</p>";
      const tokens = markedLib.lexer(text || "");
      friendlyMarkdownTokens(tokens);
      return markedLib.parser(tokens);
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
    if (!res.ok) {
      const err = new Error(data.error || res.statusText);
      err.status = res.status;
      throw err;
    }
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
    work: null,
    workRevision: 0,
    partNotices: new Map(),
    partReviewOpen: new Map(),
    workBoardCollapsed: false,
    diffPartId: null,
    diffData: null,
    diffLoading: false,
    diffError: "",
    diffController: null,
    messages: new Map(),
    order: [],
    winStart: 0,
    winEnd: 0,
    requests: new Map(),
    draftAttachments: [],
    queue: [],
    discussion: null,
    traces: new Map(),
    live: new Map(), // msgId -> live progress of a streaming reply (subagents, tools)
    traceFetching: new Set(),
    traceErrors: new Map(),
    expanded: new Set(),
    userCollapsed: new Set(),
    autoTrace: localStorage.getItem("rt.autoTrace") === "1",
    leftCollapsed: localStorage.getItem("rt.leftCollapsed") === "1",
    rightCollapsed: localStorage.getItem("rt.rightCollapsed") === "1",
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
    stickToBottom: true,
    unreadBelow: 0,
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
      renderParticipants(); renderTargetControls(); renderUsagePanel(); renderRequests(); renderQueue(); renderDiscussion(); renderWork(); updateHint();
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
  let activeSyncUpdatedReqIds = null;
  async function openRoom(id, resync, latest = false) {
    const preserveHistory = !!(resync && !latest && state.roomId === id && state.browsingHistory && state.order.length);
    const before = preserveHistory ? state.messages.get(state.order[state.order.length - 1]).seq + 1 : 0;
    const my = ++openSeq;
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
      pendingRestoreScrollTop = null;
      suppressScrollStick = false;
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
      pendingRestoreScrollTop = null;
      suppressScrollStick = false;
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
        namesRoom = null;
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

    const btnWorkPlan = $("#btn-work-plan");
    if (btnWorkPlan) {
      btnWorkPlan.hidden = r.mode !== "write";
      btnWorkPlan.disabled = !!state.work;
      btnWorkPlan.title = state.work
        ? "이미 작업 계획이 확정된 방입니다"
        : "작업 분배 및 적대적 교차 리뷰 계획을 확정합니다";
    }

    $("#rounds").value = r.discussion_rounds || 3;
    $("#btn-export").href = "/api/rooms/" + r.id + "/export.md";

    renderWorkHeadBar();
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
      const seat = participants.find((p) => p.provider === provider && !p.removed);
      const account = el("section", "usage-account" + (seat ? seatClass(seat.id) : ""));
      account.setAttribute("aria-label", (LABEL[provider] || provider) + " 남은 사용량");
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

  function syncPendingScrollRestore() {
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

  function onTimelineImageLoad(ev) {
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

  async function focusTimelineMessage(msgId) {
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

  function updateJumpBottomBtn() {
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

  async function scrollToTimelineBottom() {
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

  function renderTimeline(scrollToBottom, explicitPriorTop) {
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
    updateJumpBottomBtn();
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

  async function jumpToLatestMessages() {
    if (!state.roomId) return;
    pendingRestoreScrollTop = null;
    suppressScrollStick = false;
    state.stickToBottom = true;
    state.unreadBelow = 0;
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
        meta.push('<span class="resp-time" title="이 답변에 걸린 시간">⏱ ' + esc(fmtElapsed(m.duration_ms)) + "</span>");
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
      const bodyHtml =
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
      const bodyKey =
        live
          ? "live:" + live.ver
          : m.kind === "agent"
          ? visibleMentions(agentDisplayText)
          : visibleMentions(m.text);

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
          "</div>";
        bubble = node.querySelector(".bubble");
      }

      bubble.querySelector(".meta").innerHTML = meta.join("");
      const bodyEl = bubble.querySelector(".body");
      bodyEl.hidden =
        (m.kind === "user" && !m.text) ||
        (m.kind === "agent" && !live && !hasVisibleAgentText && m.status !== "streaming");
      bodyEl.className = "body" + (m.status === "streaming" ? " cursor" : "");
      if (bodyEl.dataset.rawText !== bodyKey || isInitialMount) {
        bodyEl.dataset.rawText = bodyKey;
        bodyEl.innerHTML = bodyHtml;
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

  function formatLightboxMeta(att, width, height) {
    const parts = [];
    if (width > 0 && height > 0) parts.push(width + "×" + height);
    if (att.bytes > 0) parts.push(fmtBytes(att.bytes));
    if (att.by) parts.push(att.by === "user" ? "나" : participantLabel(att.by));
    return parts.join(" · ");
  }

  function openLightbox(att, url) {
    const dlg = $("#dlg-lightbox");
    if (!dlg || !att) return;
    const nameEl = $("#lightbox-name");
    const metaEl = $("#lightbox-meta");
    const dlEl = $("#lightbox-download");
    const imgEl = $("#lightbox-img");
    if (nameEl) nameEl.textContent = att.name;
    if (metaEl) metaEl.textContent = formatLightboxMeta(att, att.width, att.height);
    if (dlEl) {
      dlEl.href = url;
      dlEl.setAttribute("download", att.name);
    }
    if (imgEl) {
      imgEl.onload = () => {
        if (!(att.width > 0 && att.height > 0) && imgEl.naturalWidth > 0 && imgEl.naturalHeight > 0 && metaEl) {
          metaEl.textContent = formatLightboxMeta(att, imgEl.naturalWidth, imgEl.naturalHeight);
        }
      };
      imgEl.src = url;
      imgEl.alt = att.name;
    }
    if (!dlg.open) dlg.showModal();
  }

  function closeLightbox() {
    const dlg = $("#dlg-lightbox");
    if (dlg && dlg.open) dlg.close();
  }

  function renderMessageAttachments(slot, m) {
    if (!slot) return;
    const atts = Array.isArray(m.attachments) ? m.attachments : [];
    if (!atts.length || !state.roomId) {
      slot.hidden = true;
      slot.replaceChildren();
      delete slot.dataset.sig;
      return;
    }
    const sig = JSON.stringify(
      atts.map((a) => [
        a.id,
        a.name,
        a.mime,
        a.bytes,
        a.width,
        a.height,
        a.by,
        a.by && a.by !== "user" ? participantLabel(a.by) : "",
        a.by && a.by !== "user" ? seatClass(a.by) : "",
      ])
    );
    slot.hidden = false;
    if (slot.dataset.sig === sig) return;
    slot.dataset.sig = sig;
    slot.replaceChildren();

    const grid = el("div", "att-grid");
    for (const att of atts) {
      const isAgentAtt = Boolean(att.by && att.by !== "user");
      const sCls = isAgentAtt ? seatClass(att.by) : "";
      const btn = el("button", "att-thumb" + (isAgentAtt ? " agent-att" + sCls : " user-att"));
      btn.type = "button";
      const url = "/api/rooms/" + encodeURIComponent(state.roomId) + "/attachments/" + encodeURIComponent(att.id);

      const img = document.createElement("img");
      img.className = "att-img";
      img.loading = "lazy";
      img.src = url;
      img.alt = att.name;
      if (att.width > 0 && att.height > 0) {
        img.width = att.width;
        img.height = att.height;
      }
      img.addEventListener("load", onTimelineImageLoad);
      btn.appendChild(img);

      const cap = el("span", "att-caption");
      cap.appendChild(el("span", "att-caption-name", att.name));
      if (att.width > 0 && att.height > 0) {
        cap.appendChild(el("span", "mono", att.width + "×" + att.height));
      }
      btn.appendChild(cap);

      const dimTxt = att.width > 0 && att.height > 0 ? " (" + att.width + "×" + att.height + ")" : "";
      const byTxt = isAgentAtt ? " · " + participantLabel(att.by) : "";
      btn.title = att.name + dimTxt + byTxt + " · 클릭하여 원본 보기";
      btn.setAttribute("aria-label", att.name + dimTxt + " 라이트박스 열기");
      btn.onclick = () => openLightbox(att, url);
      grid.appendChild(btn);
    }
    slot.appendChild(grid);
  }

  function collectMessageRequests(m) {
    if (!m) return [];
    const ids = [];
    const seen = new Set();
    if (Array.isArray(m.requests)) {
      for (const rid of m.requests) {
        if (rid && !seen.has(rid)) {
          seen.add(rid);
          ids.push(rid);
        }
      }
    }
    for (const req of state.requests.values()) {
      if (req && req.source_msg === m.id && !seen.has(req.id)) {
        seen.add(req.id);
        ids.push(req.id);
      }
    }
    const list = [];
    for (const rid of ids) {
      const req = state.requests.get(rid);
      if (req) list.push(req);
    }
    return list;
  }

  function buildSeatRouteChip(seatId) {
    return el(
      "span",
      "seat-chip" + seatClass(seatId) + " " + authorProvider(seatId),
      participantLabel(seatId)
    );
  }

  function buildMessageRequestCard(req) {
    const card = el("div", "req-card" + seatClass(req.to) + " status-" + req.status);
    card.dataset.reqId = req.id;

    const head = el("div", "req-card-head");
    const route = el("div", "req-route");
    route.appendChild(buildSeatRouteChip(req.from));
    route.appendChild(el("span", "req-arrow muted", "→"));
    route.appendChild(buildSeatRouteChip(req.to));
    const stBadge = el("span", "badge req-status-badge status-" + req.status, requestStatusLabel(req.status));
    route.appendChild(stBadge);
    head.appendChild(route);

    const actions = el("div", "req-card-actions");
    if (req.result_msg) {
      const jumpBtn = el("button", "ghost req-jump-btn", "결과 메시지로 이동");
      jumpBtn.type = "button";
      jumpBtn.onclick = () => focusTimelineMessage(req.result_msg);
      actions.appendChild(jumpBtn);
    }
    if (isIncompleteRequest(req.status)) {
      const cancelBtn = el("button", "danger ghost req-cancel-btn", "취소");
      cancelBtn.type = "button";
      cancelBtn.onclick = () => cancelRequest(req.id, cancelBtn);
      actions.appendChild(cancelBtn);
    }
    if (actions.childNodes.length > 0) {
      head.appendChild(actions);
    }
    card.appendChild(head);

    if (req.text) {
      card.appendChild(el("div", "req-card-text", rewriteProseMentions(req.text, visibleMentions)));
    }
    if (req.status === "rejected" && req.reason) {
      const reasonEl = el("div", "req-card-reason", "거절 사유: " + req.reason);
      card.appendChild(reasonEl);
    }
    return card;
  }

  function renderMessageRequests(slot, m) {
    if (!slot) return;
    const reqs = collectMessageRequests(m);
    if (!reqs.length) {
      slot.hidden = true;
      slot.replaceChildren();
      delete slot.dataset.sig;
      return;
    }
    const sig = JSON.stringify(
      reqs.map((r) => [
        r.id,
        r.from,
        r.to,
        r.text,
        r.status,
        r.result_msg,
        r.reason,
        participantLabel(r.from),
        participantLabel(r.to),
        seatClass(r.from),
        seatClass(r.to),
      ])
    );
    slot.hidden = false;
    if (slot.dataset.sig === sig) return;
    slot.dataset.sig = sig;
    slot.replaceChildren();
    for (const req of reqs) {
      slot.appendChild(buildMessageRequestCard(req));
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
        renderTrace(id);
        updateTraceCount(id);
      } catch (err) {
        if (controller.signal.aborted) return;
        if (state.roomId !== roomId || openSeq !== sequence) return;
        state.traceErrors.set(id, err.message);
        renderTraceError(id);
        updateTraceCount(id);
      } finally {
        if (state.traceControllers.get(id) === controller) {
          state.traceFetching.delete(id);
          state.tracePending.delete(id);
          state.traceControllers.delete(id);
          syncPendingScrollRestore();
        }
      }
      return;
    }
    renderTrace(id);
    updateTraceCount(id);
    syncPendingScrollRestore();
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
    const shouldPinBottom = !state.browsingHistory && state.stickToBottom;
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
          // The model the caller asked for; a later subagent_meta reports the one that ran.
          if (e.meta && e.meta.model) s.model = e.meta.model;
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
      if (sum) fillSubSummary(sum, it);
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

  // Model and effort come from the CLI and are shown only as far as it reported
  // them: Claude never reports a subagent's effort, Antigravity neither value.
  function fillSubSummary(sum, it) {
    const s = it.start || {};
    const name = s.name || s.parent || "subagent";
    const model = [it.model, it.effort].filter(Boolean).join(" · ");
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

  async function cancelRequest(reqId, btn) {
    if (!state.roomId || !reqId) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    if (btn) btn.disabled = true;
    try {
      const updated = await api("POST", "/api/rooms/" + roomId + "/requests/" + encodeURIComponent(reqId) + "/cancel");
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (updated && updated.id) {
        upsertRequest(updated);
      } else {
        const cur = state.requests.get(reqId);
        if (cur) upsertRequest({ ...cur, status: "cancelled" });
      }
      announce("요청이 취소되었습니다.");
    } catch (e) {
      if (state.roomId === roomId && openSeq === sequence) alert(e.message);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function renderRequests() {
    const ul = $("#requests");
    const cnt = $("#requests-count");
    const sorted = sortRequests(state.requests.values());
    const activeCount = sorted.filter((r) => isIncompleteRequest(r.status)).length;
    if (cnt) {
      cnt.textContent = String(sorted.length);
      cnt.className = "badge" + (activeCount > 0 ? " warn" : "");
      cnt.title = activeCount > 0 ? "미완료 " + activeCount + "건 / 전체 " + sorted.length + "건" : "전체 " + sorted.length + "건";
    }
    if (!ul) return;
    const sig = JSON.stringify(
      sorted.map((r) => [
        r.id,
        r.from,
        r.to,
        r.text,
        r.source_msg,
        r.status,
        r.result_msg,
        r.reason,
        r.at,
        participantLabel(r.from),
        participantLabel(r.to),
        seatClass(r.from),
        seatClass(r.to),
      ])
    );
    if (ul.dataset.sig === sig) return;
    ul.dataset.sig = sig;
    ul.replaceChildren();
    if (!sorted.length) {
      ul.appendChild(el("li", "muted", "진행 중인 요청 없음"));
      return;
    }
    for (const req of sorted) {
      const li = el("li", "req-item status-" + req.status);
      li.dataset.reqId = req.id;

      const top = el("div", "req-item-top");
      const route = el("div", "req-route");
      route.appendChild(buildSeatRouteChip(req.from));
      route.appendChild(el("span", "req-arrow muted", "→"));
      route.appendChild(buildSeatRouteChip(req.to));
      top.appendChild(route);
      top.appendChild(el("span", "badge req-status-badge status-" + req.status, requestStatusLabel(req.status)));
      li.appendChild(top);

      if (req.text) {
        li.appendChild(el("div", "req-item-body", rewriteProseMentions(req.text, visibleMentions)));
      }
      if (req.status === "rejected" && req.reason) {
        li.appendChild(el("div", "req-item-reason", "거절 사유: " + req.reason));
      }

      const actions = el("div", "req-item-actions");
      if (req.source_msg) {
        const srcBtn = el("button", "ghost", "요청 이동");
        srcBtn.type = "button";
        srcBtn.onclick = () => focusTimelineMessage(req.source_msg);
        actions.appendChild(srcBtn);
      }
      if (req.result_msg) {
        const resBtn = el("button", "ghost", "결과 이동");
        resBtn.type = "button";
        resBtn.onclick = () => focusTimelineMessage(req.result_msg);
        actions.appendChild(resBtn);
      }
      if (isIncompleteRequest(req.status)) {
        const cancelBtn = el("button", "danger", "취소");
        cancelBtn.type = "button";
        cancelBtn.setAttribute(
          "aria-label",
          participantLabel(req.from) + " → " + participantLabel(req.to) + " 요청 취소"
        );
        cancelBtn.onclick = () => cancelRequest(req.id, cancelBtn);
        actions.appendChild(cancelBtn);
      }
      if (actions.childNodes.length > 0) {
        li.appendChild(actions);
      }
      ul.appendChild(li);
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
        {
          mention: "@호출",
          request: "요청",
          broadcast: "전체",
          discussion: "토론 " + t.round + "R",
          part: "파트" + (t.part ? " " + t.part : ""),
          review: "리뷰" + (t.part ? " " + t.part : ""),
        }[t.reason] || t.reason;
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

  // ---------- Work division, adversarial review & merge UI ----------
  const shortSHA = (sha) => (sha ? String(sha).slice(0, 8) : "");

  const REVIEW_STEP_LABELS = {
    findings: "지적 (findings)",
    author_reply: "작성자 응답 (author_reply)",
    rebuttal: "반박 검토 (rebuttal)",
    recheck: "수정 확인 (recheck)",
  };
  const REVIEW_STEP_ORDER = {
    findings: 1,
    author_reply: 2,
    rebuttal: 3,
    recheck: 4,
  };
  function reviewStepLabel(step) {
    return REVIEW_STEP_LABELS[step] || step;
  }

  const PART_STATE_LABELS = {
    working: "작업 중 (working)",
    reviewing: "리뷰 중 (reviewing)",
    fixing: "수정 중 (fixing)",
    queued: "병합 대기 (queued)",
    awaiting_approval: "승인 대기 (awaiting_approval)",
    merging: "병합 중 (merging)",
    merged: "병합 완료 (merged)",
    blocked: "차단됨 (blocked)",
    abandoned: "포기됨 (abandoned)",
  };

  function findWorkPart(pid) {
    if (!state.work || !state.work.parts) return null;
    return state.work.parts.find((p) => p.id === pid) || null;
  }

  function isPartErrorResolved(m) {
    if (!m || m.status !== "error" || (!m.part && !m.review_step)) return false;
    const part = m.part ? findWorkPart(m.part) : null;
    if (part && part.state === "merged") {
      return true;
    }
    const baseStepOrder = m.review_step ? REVIEW_STEP_ORDER[m.review_step] || 0 : 0;
    const mIdx = state.order.indexOf(m.id);
    for (let i = 0; i < state.order.length; i++) {
      const cand = state.messages.get(state.order[i]);
      if (!cand || cand.id === m.id) continue;
      const isAfter = m.seq != null && cand.seq != null ? cand.seq > m.seq : mIdx >= 0 && i > mIdx;
      if (!isAfter) continue;
      if (m.part && cand.part !== m.part) continue;
      if (cand.status !== "done") continue;
      if (m.review_step) {
        const candStepOrder = cand.review_step ? REVIEW_STEP_ORDER[cand.review_step] || 0 : 0;
        if (candStepOrder > 0 && candStepOrder >= baseStepOrder) return true;
      } else if (m.part && cand.part === m.part) {
        return true;
      }
    }
    return false;
  }

  function isPartErrorAbandoned(m) {
    if (!m || m.status !== "error" || (!m.part && !m.review_step)) return false;
    if (isPartErrorResolved(m)) return false;
    const part = m.part ? findWorkPart(m.part) : null;
    return !!(part && part.state === "abandoned");
  }

  function refreshPartErrorMessages(partId = "") {
    for (let i = state.winStart; i < state.winEnd; i++) {
      const id = state.order[i];
      const msg = state.messages.get(id);
      if (!msg || msg.status !== "error") continue;
      if (!msg.part && !msg.review_step) continue;
      if (partId && msg.part && msg.part !== partId) continue;
      renderMessage(id, false);
    }
  }

  function focusWorkPartCard(partId) {
    if (!state.work) {
      announce("현재 활성화된 파트 보드가 없습니다.");
      return;
    }
    if (state.workBoardCollapsed) {
      state.workBoardCollapsed = false;
      renderWorkHeadBar();
      renderWork();
    }
    const board = $("#work-board");
    if (!board) return;
    let card = null;
    if (partId) {
      card = Array.from(board.querySelectorAll(".part-card")).find((elNode) => elNode.dataset.partId === partId) || null;
    }
    const target = card || board;
    target.scrollIntoView({ behavior: "smooth", block: "nearest" });
    if (card) {
      card.classList.remove("part-card-flash");
      void card.offsetWidth;
      card.classList.add("part-card-flash");
      setTimeout(() => {
        card.classList.remove("part-card-flash");
      }, 1600);
      const retryBtn = Array.from(card.querySelectorAll(".part-actions button")).find(
        (b) => !b.disabled && (b.textContent === "재시도" || b.textContent.includes("라운드 1회 추가"))
      );
      if (retryBtn) retryBtn.focus({ preventScroll: true });
    }
    announce((partId ? partId + " " : "") + "파트 보드 카드로 이동했습니다.");
  }

  async function patchMergeMode(nextMode, btn) {
    if (!state.roomId || !state.work) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    const workRev = state.workRevision;
    if (btn) btn.disabled = true;
    try {
      const updated = await api("PATCH", "/api/rooms/" + roomId + "/work", { merge_mode: nextMode });
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (state.workRevision === workRev && state.work) {
        state.workRevision++;
        if (updated && updated.merge_mode) state.work = updated;
        else state.work.merge_mode = nextMode;
      }
      renderWorkHeadBar();
      renderWork();
      announce("병합 모드를 " + (nextMode === "auto" ? "자동(auto)" : "확인(confirm)") + "으로 변경했습니다.");
    } catch (err) {
      if (state.roomId === roomId && openSeq === sequence) alert(err.message);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function renderWorkHeadBar() {
    const bar = $("#work-head-bar");
    if (!bar) return;
    const w = state.work;
    if (!w) {
      bar.hidden = true;
      bar.replaceChildren();
      delete bar.dataset.sig;
      return;
    }
    const partsCount = w.parts ? w.parts.length : 0;
    const sig = JSON.stringify([
      w.goal,
      w.base_sha,
      w.integration_branch,
      w.integration_sha,
      w.merge_mode,
      w.review_rounds,
      w.queue,
      w.merging,
      state.workBoardCollapsed,
      partsCount,
    ]);
    bar.hidden = false;
    if (bar.dataset.sig === sig) return;
    bar.dataset.sig = sig;
    bar.replaceChildren();

    const info = el("div", "work-head-info");
    const goalSpan = el("span", "work-goal", "🎯 " + w.goal);
    goalSpan.title = "작업 목표: " + w.goal;
    info.appendChild(goalSpan);

    const branchBadge = el("span", "badge mono work-branch-badge", "🌿 " + w.integration_branch);
    branchBadge.title = "통합 branch: " + w.integration_branch;
    info.appendChild(branchBadge);

    const shaBadge = el("span", "badge mono work-sha-badge", "통합 SHA: " + shortSHA(w.integration_sha));
    shaBadge.title = "통합 branch 현재 커밋 SHA: " + w.integration_sha;
    info.appendChild(shaBadge);

    if (w.base_sha) {
      const baseBadge = el("span", "badge mono", "base: " + shortSHA(w.base_sha));
      baseBadge.title = "기준 커밋 SHA: " + w.base_sha;
      info.appendChild(baseBadge);
    }

    if (w.queue && w.queue.length > 0) {
      const qBadge = el("span", "badge warn", "병합 대기열: " + w.queue.join(" → "));
      qBadge.title = "FIFO 병합 대기열";
      info.appendChild(qBadge);
    }

    if (w.merging) {
      const mBadge = el("span", "badge ok mono", "병합 중: " + w.merging.part_id + " (" + shortSHA(w.merging.merge_sha) + ")");
      mBadge.title = "before: " + w.merging.before_sha + " → merge: " + w.merging.merge_sha;
      info.appendChild(mBadge);
    }
    bar.appendChild(info);

    const controls = el("div", "work-head-controls");
    const modeGroup = el("div", "merge-mode-toggle");
    modeGroup.setAttribute("role", "group");
    modeGroup.setAttribute("aria-label", "병합 모드 전환");
    modeGroup.appendChild(el("span", "muted small-note", "병합:"));

    for (const [mode, labelTxt] of [
      ["confirm", "확인 (confirm)"],
      ["auto", "자동 (auto)"],
    ]) {
      const active = w.merge_mode === mode;
      const b = el("button", "merge-mode-btn" + (active ? " active" : ""), labelTxt);
      b.type = "button";
      b.setAttribute("aria-pressed", String(active));
      b.title =
        mode === "confirm"
          ? "확인 모드: 리뷰 통과 후 사람이 승인 버튼을 눌러야 병합합니다"
          : "자동 모드: 리뷰·검증 통과 시 사람 승인 없이 자동 병합합니다";
      b.onclick = () => {
        if (w.merge_mode !== mode) patchMergeMode(mode, b);
      };
      modeGroup.appendChild(b);
    }
    controls.appendChild(modeGroup);

    const foldBtn = el(
      "button",
      "ghost work-fold-btn",
      state.workBoardCollapsed ? "파트 보드 펼치기 (" + partsCount + ") ▾" : "파트 보드 접기 ▴"
    );
    foldBtn.type = "button";
    foldBtn.setAttribute("aria-expanded", String(!state.workBoardCollapsed));
    foldBtn.onclick = () => {
      state.workBoardCollapsed = !state.workBoardCollapsed;
      renderWorkHeadBar();
      renderWork();
    };
    controls.appendChild(foldBtn);
    bar.appendChild(controls);
  }

  function buildBlockElement(block, isHalted) {
    const box = el("div", isHalted ? "halted-box" : "part-block-box");
    const head = el("div", "block-head");
    head.appendChild(el("strong", "", isHalted ? "⛔ 병합 대기열 정지 (Halted)" : "⛔ 파트 차단 (Blocked)"));
    head.appendChild(el("span", "badge block-kind", block.kind));
    if (block.from) {
      head.appendChild(el("span", "muted small-note", "이전 상태: " + block.from));
    }
    if (block.at) {
      head.appendChild(el("span", "muted small-note", fmtTime(block.at)));
    }
    box.appendChild(head);

    if (block.message) {
      box.appendChild(el("div", "block-msg", block.message));
    }

    if (block.result) {
      const res = block.result;
      if (res.conflicts && res.conflicts.length > 0) {
        const confWrap = el("div", "block-conflicts");
        confWrap.appendChild(el("span", "block-label", "충돌 파일 (" + res.conflicts.length + "): "));
        for (const f of res.conflicts) {
          confWrap.appendChild(el("code", "file-path mono", f));
        }
        box.appendChild(confWrap);
      }
      if (res.name || (res.argv && res.argv.length > 0)) {
        const cmdLine = el("div", "block-cmd mono");
        const argvTxt = res.argv ? res.argv.join(" ") : "";
        cmdLine.textContent =
          "명령: " +
          res.name +
          (argvTxt ? " [" + argvTxt + "]" : "") +
          " · exit code: " +
          res.exit_code +
          (res.timed_out ? " · ⏱ 시간 초과" : "");
        box.appendChild(cmdLine);
      }
      if (res.output) {
        const det = el("details", "block-output-details");
        det.appendChild(el("summary", "", "명령 출력 보기 (Output)"));
        const preEl = el("pre", "mono", res.output);
        det.appendChild(preEl);
        box.appendChild(det);
      }
    }
    return box;
  }

  async function partAction(part, action, btn) {
    if (!state.roomId || !state.work) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    const workRev = state.workRevision;
    const url = "/api/rooms/" + roomId + "/work/parts/" + encodeURIComponent(part.id) + "/" + action;
    const body =
      action === "approve"
        ? {
            submit_sha: part.submit_sha,
            integration_sha: state.work.integration_sha,
          }
        : undefined;
    if (btn) btn.disabled = true;
    state.partNotices.delete(part.id);
    try {
      const res = await api("POST", url, body);
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (state.workRevision === workRev && res && res.parts) {
        state.workRevision++;
        state.work = res;
      }
      renderWorkHeadBar();
      renderWork();
      refreshPartErrorMessages(part.id);
      announce(part.id + " 파트 " + action + " 요청 완료");
    } catch (err) {
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if ((action === "approve" || action === "resolve") && err.status === 409) {
        state.partNotices.set(part.id, "변경됨, 다시 확인");
        announce("변경됨, 다시 확인");
        delete $("#work-board").dataset.sig;
        renderWork();
        openRoom(roomId, true);
        return;
      }
      alert(err.message);
    } finally {
      if (btn) btn.disabled = false;
    }
  }

  function buildReviewSection(part) {
    const rev = part.review;
    const det = el("details", "part-review-section");
    const savedOpen = state.partReviewOpen.get(part.id);
    det.open = savedOpen !== undefined ? savedOpen : true;
    det.ontoggle = () => {
      state.partReviewOpen.set(part.id, det.open);
    };

    const totalItems = rev.rounds.reduce((acc, r) => acc + (r.items ? r.items.length : 0), 0);
    const ovSuffix = rev.resolution
      ? " · 사람 판정으로 통과 · " + rev.resolution.item_ids.length + "건"
      : "";
    const sum = el(
      "summary",
      "",
      "교차 리뷰 상세 (" +
        rev.rounds.length +
        " / " +
        rev.max_rounds +
        " 라운드 · 총 지적 " +
        totalItems +
        "건 · 미해결 " +
        rev.summary.unresolved +
        "건" +
        ovSuffix +
        ")"
    );
    det.appendChild(sum);

    for (const round of rev.rounds) {
      const roundWrap = el("div", "review-round-block");
      const rHead = el(
        "div",
        "review-round-head mono",
        "라운드 " +
          round.ref.round +
          " · base " +
          shortSHA(round.ref.base_sha) +
          " .. head " +
          shortSHA(round.ref.head_sha)
      );
      rHead.title = round.ref.base_sha + ".." + round.ref.head_sha;
      roundWrap.appendChild(rHead);

      const tblWrap = el("div", "review-table-wrap");
      const tbl = el("table", "review-table");
      const thead = el("thead");
      const htr = el("tr");
      for (const col of ["번호", "심각도", "파일:줄", "재현 시나리오", "확신도", "작성자", "리뷰어", "상태"]) {
        htr.appendChild(el("th", "", col));
      }
      thead.appendChild(htr);
      tbl.appendChild(thead);

      const tbody = el("tbody");
      if (!round.items || !round.items.length) {
        const tr = el("tr");
        const td = el("td", "muted", "지적 사항 없음 (0건)");
        td.colSpan = 8;
        tr.appendChild(td);
        tbody.appendChild(tr);
      } else {
        for (const item of round.items) {
          const f = item.finding;
          const tr = el("tr", "review-row status-" + item.status);
          tr.appendChild(el("td", "mono", f.id));

          const sevTd = el("td");
          sevTd.appendChild(el("span", "badge sev-" + f.severity, f.severity));
          tr.appendChild(sevTd);

          const locTxt = (f.side ? f.side + ":" : "") + f.file + ":" + f.line;
          tr.appendChild(el("td", "mono file-loc", locTxt));

          tr.appendChild(el("td", "scenario-cell", f.scenario));

          const confTd = el("td");
          confTd.appendChild(el("span", "badge conf-" + f.confidence, f.confidence));
          tr.appendChild(confTd);

          const authTd = el("td", "reply-cell");
          if (item.author) {
            authTd.appendChild(el("span", "badge reply-action action-" + item.author.action, item.author.action));
            if (item.author.reason) {
              authTd.appendChild(el("span", "reply-reason", " " + item.author.reason));
            }
          } else {
            authTd.textContent = "-";
            authTd.classList.add("muted");
          }
          tr.appendChild(authTd);

          const revTd = el("td", "reply-cell");
          if (item.reviewer) {
            revTd.appendChild(el("span", "badge reply-action action-" + item.reviewer.action, item.reviewer.action));
            if (item.reviewer.reason) {
              revTd.appendChild(el("span", "reply-reason", " " + item.reviewer.reason));
            }
          } else {
            revTd.textContent = "-";
            revTd.classList.add("muted");
          }
          tr.appendChild(revTd);

          const stTd = el("td");
          stTd.appendChild(el("span", "badge item-status status-" + item.status, item.status));
          tr.appendChild(stTd);

          tbody.appendChild(tr);
        }
      }
      tbl.appendChild(tbody);
      tblWrap.appendChild(tbl);
      roundWrap.appendChild(tblWrap);
      det.appendChild(roundWrap);
    }
    return det;
  }

  function renderWork() {
    const haltedBanner = $("#work-halted-banner");
    const board = $("#work-board");
    const w = state.work;
    const shouldPinBottom = !state.browsingHistory && state.stickToBottom;
    const syncBottomPin = () => {
      if (shouldPinBottom) {
        const tl = $("#timeline");
        if (tl) tl.scrollTop = tl.scrollHeight;
      }
    };

    if (haltedBanner) {
      if (!w || !w.halted) {
        haltedBanner.hidden = true;
        haltedBanner.replaceChildren();
        delete haltedBanner.dataset.sig;
      } else {
        const hSig = JSON.stringify(w.halted);
        haltedBanner.hidden = false;
        if (haltedBanner.dataset.sig !== hSig) {
          const wasOpen = !!haltedBanner.querySelector(".block-output-details[open]");
          haltedBanner.dataset.sig = hSig;
          haltedBanner.replaceChildren(buildBlockElement(w.halted, true));
          if (wasOpen) {
            const det = haltedBanner.querySelector(".block-output-details");
            if (det) det.open = true;
          }
        }
      }
    }

    if (!board) {
      syncBottomPin();
      return;
    }
    if (!w) {
      board.hidden = true;
      board.replaceChildren();
      delete board.dataset.sig;
      syncBottomPin();
      return;
    }
    board.hidden = state.workBoardCollapsed;
    if (state.workBoardCollapsed) {
      syncBottomPin();
      return;
    }

    const participantsKey = state.room ? state.room.participants.map((p) => [p.id, p.label, p.provider]) : [];
    const sig = JSON.stringify([w, [...state.partNotices.entries()], participantsKey]);
    if (board.dataset.sig === sig) {
      syncBottomPin();
      return;
    }
    board.dataset.sig = sig;
    board.replaceChildren();

    if (!w.parts) return;
    for (const part of w.parts) {
      const card = el("article", "part-card" + seatClass(part.seat) + " state-" + part.state);
      card.dataset.partId = part.id;

      const top = el("div", "part-top");
      const titleGroup = el("div", "part-title-group");
      titleGroup.appendChild(el("span", "badge mono part-id-badge", part.id));
      titleGroup.appendChild(el("strong", "part-title", part.title));
      const stBadge = el("span", "badge part-state-badge state-" + part.state, PART_STATE_LABELS[part.state] || part.state);
      titleGroup.appendChild(stBadge);

      if (part.branch) {
        const br = el("span", "badge mono", "🌿 " + part.branch);
        br.title = "파트 branch: " + part.branch + (part.worktree ? " (" + part.worktree + ")" : "");
        titleGroup.appendChild(br);
      }
      if (part.submit_sha) {
        const subSha = el("span", "badge mono", "제출 SHA: " + shortSHA(part.submit_sha));
        subSha.title = "SubmitSHA: " + part.submit_sha;
        titleGroup.appendChild(subSha);
      }
      if (part.ready_mode) {
        titleGroup.appendChild(el("span", "badge", "ReadyMode: " + part.ready_mode));
      }
      if (part.merged_sha) {
        const mSha = el("span", "badge mono ok", "병합됨: " + shortSHA(part.merged_sha));
        mSha.title = "MergedSHA: " + part.merged_sha;
        titleGroup.appendChild(mSha);
      }
      top.appendChild(titleGroup);

      const rev = part.review;
      const unresolved = rev && rev.summary ? rev.summary.unresolved : 0;
      const isReviewEscalated =
        part.state === "blocked" &&
        !!part.block &&
        part.block.kind === "review" &&
        !!rev &&
        (rev.phase === "disputed" || rev.phase === "limit");

      const actions = el("div", "part-actions");
      const btnSubmit = el("button", "", part.state === "fixing" ? "재제출" : "제출");
      btnSubmit.type = "button";
      btnSubmit.disabled = part.state !== "working" && part.state !== "fixing";
      btnSubmit.title = "파트 작업 커밋을 교차 리뷰에 제출합니다";
      btnSubmit.onclick = () => partAction(part, "submit", btnSubmit);
      actions.appendChild(btnSubmit);

      const btnApprove = el("button", "primary", "승인");
      btnApprove.type = "button";
      btnApprove.disabled = part.state !== "awaiting_approval";
      btnApprove.title =
        "현재 제출 SHA(" +
        shortSHA(part.submit_sha) +
        ")와 통합 SHA(" +
        shortSHA(w.integration_sha) +
        ")를 확인하여 병합을 승인합니다";
      btnApprove.onclick = () => partAction(part, "approve", btnApprove);
      actions.appendChild(btnApprove);

      const btnRetry = el("button", "", isReviewEscalated ? "라운드 1회 추가 후 수정" : "재시도");
      btnRetry.type = "button";
      btnRetry.disabled = part.state !== "blocked";
      btnRetry.title = isReviewEscalated
        ? "작성자가 고쳐 다시 제출하면 재리뷰합니다"
        : "차단(blocked)을 해제하고 이전 상태에서 다시 시도합니다";
      btnRetry.onclick = () => partAction(part, "retry", btnRetry);
      actions.appendChild(btnRetry);

      if (isReviewEscalated) {
        const btnResolve = el("button", "primary", "사람 판정");
        btnResolve.type = "button";
        btnResolve.title = "미해결 " + unresolved + "건을 넘기고 병합 대기열로 보냅니다";
        btnResolve.onclick = () => {
          if (!confirm("미해결 " + unresolved + "건을 넘기고 병합 대기열로 보냅니다")) return;
          partAction(part, "resolve", btnResolve);
        };
        actions.appendChild(btnResolve);
      }

      const btnAbandon = el("button", "danger ghost", "포기");
      btnAbandon.type = "button";
      btnAbandon.disabled = part.state === "merged" || part.state === "abandoned" || part.state === "merging";
      btnAbandon.title = "이 파트를 포기(abandoned) 상태로 전환합니다";
      btnAbandon.onclick = () => partAction(part, "abandon", btnAbandon);
      actions.appendChild(btnAbandon);

      const btnDiff = el("button", "ghost", "Diff 보기");
      btnDiff.type = "button";
      btnDiff.title = "이 파트의 변경 파일 목록과 패치를 서랍에서 봅니다";
      btnDiff.onclick = () => openDiffDrawer(part.id);
      actions.appendChild(btnDiff);

      top.appendChild(actions);
      card.appendChild(top);

      const meta = el("div", "part-meta");
      const authorChip = el(
        "span",
        "seat-chip" + seatClass(part.seat) + " " + authorProvider(part.seat),
        "담당: " + participantLabel(part.seat)
      );
      meta.appendChild(authorChip);

      const reviewerChip = el(
        "span",
        "seat-chip" + seatClass(part.reviewer) + " " + authorProvider(part.reviewer),
        "리뷰어: " + participantLabel(part.reviewer)
      );
      meta.appendChild(reviewerChip);

      const curRound = rev && rev.rounds && rev.rounds.length ? rev.rounds[rev.rounds.length - 1].ref.round : 0;
      const maxRounds = rev && rev.max_rounds ? rev.max_rounds : w.review_rounds;
      const revBadgeCls =
        "badge review-summary-badge" +
        (unresolved > 0 && !(rev && rev.resolution) ? " warn" : rev && ((rev.summary && rev.summary.passed) || rev.resolution) ? " ok" : "");
      const revSummaryTxt =
        "리뷰 " +
        curRound +
        "/" +
        maxRounds +
        "R · 미해결 " +
        unresolved +
        "건" +
        (rev && rev.resolution
          ? " · 사람 판정으로 통과 · " + rev.resolution.item_ids.length + "건"
          : "");
      meta.appendChild(el("span", revBadgeCls, revSummaryTxt));
      if (rev && rev.phase) {
        meta.appendChild(el("span", "badge", "단계: " + rev.phase));
      }
      if (rev && rev.pending) {
        meta.appendChild(el("span", "badge", "차례: " + rev.pending.who + " · " + rev.pending.kind));
      }

      if (part.scope && part.scope.length > 0) {
        const scopeWrap = el("span", "part-scope");
        scopeWrap.appendChild(el("span", "muted small-note", "범위: "));
        for (const s of part.scope) {
          scopeWrap.appendChild(el("code", "file-path mono", s));
        }
        meta.appendChild(scopeWrap);
      }
      card.appendChild(meta);

      if (part.brief) {
        const briefEl = el("div", "part-brief");
        briefEl.appendChild(el("span", "muted", "완료 조건: "));
        briefEl.appendChild(el("span", "", part.brief));
        card.appendChild(briefEl);
      }

      if (state.partNotices.has(part.id)) {
        const noticeBox = el("div", "part-notice", "⚠ " + state.partNotices.get(part.id));
        noticeBox.setAttribute("role", "alert");
        card.appendChild(noticeBox);
      }

      if (part.block) {
        card.appendChild(buildBlockElement(part.block, false));
      }

      if (rev && rev.rounds && rev.rounds.length > 0) {
        card.appendChild(buildReviewSection(part));
      }

      board.appendChild(card);
    }
    syncBottomPin();
  }

  // ---------- Diff Drawer (lazy loaded via GET /api/rooms/{id}/work/parts/{pid}/diff) ----------
  function closeDiffDrawer() {
    if (state.diffController) {
      state.diffController.abort();
      state.diffController = null;
    }
    state.diffPartId = null;
    state.diffData = null;
    state.diffLoading = false;
    state.diffError = "";
    const drawer = $("#diff-drawer");
    if (drawer) {
      drawer.hidden = true;
      drawer.replaceChildren();
    }
  }

  async function openDiffDrawer(partId) {
    if (!state.roomId) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    if (state.diffController) {
      state.diffController.abort();
    }
    const controller = new AbortController();
    state.diffController = controller;
    state.diffPartId = partId;
    state.diffData = null;
    state.diffLoading = true;
    state.diffError = "";
    renderDiffDrawer();

    try {
      const diff = await api(
        "GET",
        "/api/rooms/" + roomId + "/work/parts/" + encodeURIComponent(partId) + "/diff",
        undefined,
        controller.signal
      );
      if (state.roomId !== roomId || openSeq !== sequence || state.diffPartId !== partId) return;
      state.diffData = diff;
      state.diffLoading = false;
      renderDiffDrawer();
    } catch (err) {
      if (controller.signal.aborted) return;
      if (state.roomId !== roomId || openSeq !== sequence || state.diffPartId !== partId) return;
      state.diffLoading = false;
      state.diffError = err.message;
      renderDiffDrawer();
    } finally {
      if (state.diffController === controller) {
        state.diffController = null;
      }
    }
  }

  function renderPatchPre(patchText) {
    const preEl = el("pre", "diff-patch mono");
    if (!patchText) {
      preEl.textContent = "(패치 내용 없음)";
      return preEl;
    }
    const lines = String(patchText).split("\n");
    for (const line of lines) {
      let cls = "diff-line";
      if (line.startsWith("+++") || line.startsWith("---")) cls += " diff-meta";
      else if (line.startsWith("+")) cls += " diff-add";
      else if (line.startsWith("-")) cls += " diff-del";
      else if (line.startsWith("@@")) cls += " diff-hunk";
      preEl.appendChild(el("div", cls, line));
    }
    return preEl;
  }

  function renderDiffDrawer() {
    const drawer = $("#diff-drawer");
    if (!drawer) return;
    if (!state.diffPartId) {
      drawer.hidden = true;
      drawer.replaceChildren();
      return;
    }
    drawer.hidden = false;
    drawer.replaceChildren();

    const part = findWorkPart(state.diffPartId);
    const head = el("div", "diff-drawer-head");
    const titleWrap = el("div", "diff-drawer-title");
    titleWrap.appendChild(el("strong", "", "Diff · " + state.diffPartId + (part ? " (" + part.title + ")" : "")));
    if (state.diffData) {
      const d = state.diffData;
      const rangeSpan = el("span", "badge mono", shortSHA(d.base_sha) + ".." + shortSHA(d.head_sha));
      rangeSpan.title = d.base_sha + ".." + d.head_sha;
      titleWrap.appendChild(rangeSpan);
      titleWrap.appendChild(el("span", "badge diff-stat-add mono", "+" + d.added));
      titleWrap.appendChild(el("span", "badge diff-stat-del mono", "-" + d.deleted));
      if (d.truncated) {
        titleWrap.appendChild(el("span", "badge warn", "⚠ 일부 생략됨 (truncated)"));
      }
    }
    head.appendChild(titleWrap);

    const headBtns = el("div", "diff-drawer-actions");
    const refreshBtn = el("button", "ghost", "새로고침");
    refreshBtn.type = "button";
    refreshBtn.disabled = state.diffLoading;
    refreshBtn.onclick = () => openDiffDrawer(state.diffPartId);
    headBtns.appendChild(refreshBtn);

    const closeBtn = el("button", "icon-btn", "✕");
    closeBtn.type = "button";
    closeBtn.setAttribute("aria-label", "Diff 서랍 닫기");
    closeBtn.onclick = closeDiffDrawer;
    headBtns.appendChild(closeBtn);
    head.appendChild(headBtns);
    drawer.appendChild(head);

    const body = el("div", "diff-drawer-body");
    if (state.diffLoading) {
      body.appendChild(el("p", "muted", "Diff 불러오는 중…"));
      drawer.appendChild(body);
      return;
    }
    if (state.diffError) {
      const errBox = el("div", "err-box");
      errBox.appendChild(el("span", "", "⚠ Diff 조회 실패: " + state.diffError));
      body.appendChild(errBox);
      drawer.appendChild(body);
      return;
    }
    const d = state.diffData;
    if (!d) {
      drawer.appendChild(body);
      return;
    }

    if (!d.files || !d.files.length) {
      body.appendChild(el("p", "muted", "변경된 파일이 없습니다."));
      drawer.appendChild(body);
      return;
    }
    const files = d.files;

    const fileList = el("ul", "diff-file-list");
    files.forEach((f, idx) => {
      const li = el("li", "diff-file-item");
      const jumpBtn = el("button", "diff-file-jump");
      jumpBtn.type = "button";
      jumpBtn.appendChild(el("span", "badge diff-status status-" + f.status, f.status));
      jumpBtn.appendChild(el("span", "mono diff-path", f.path));
      const statSpan = el("span", "diff-counts mono");
      statSpan.appendChild(el("span", "diff-stat-add", "+" + f.added));
      statSpan.appendChild(el("span", "diff-stat-del", " -" + f.deleted));
      jumpBtn.appendChild(statSpan);
      jumpBtn.onclick = () => {
        const target = $("#diff-patch-" + idx, body);
        if (target) {
          target.open = true;
          target.scrollIntoView({ behavior: "smooth", block: "start" });
        }
      };
      li.appendChild(jumpBtn);
      fileList.appendChild(li);
    });
    body.appendChild(fileList);

    files.forEach((f, idx) => {
      const det = el("details", "diff-file-card");
      det.id = "diff-patch-" + idx;
      det.open = true;
      const sum = el("summary", "diff-file-summary");
      sum.appendChild(el("span", "badge diff-status status-" + f.status, f.status));
      sum.appendChild(el("span", "mono diff-path", f.path));
      const counts = el("span", "diff-counts mono");
      counts.appendChild(el("span", "diff-stat-add", "+" + f.added));
      counts.appendChild(el("span", "diff-stat-del", " -" + f.deleted));
      sum.appendChild(counts);
      det.appendChild(sum);
      det.appendChild(renderPatchPre(f.patch));
      body.appendChild(det);
    });

    drawer.appendChild(body);
  }

  // ---------- Work Plan Dialog (POST /api/rooms/{id}/work) ----------
  function syncReviewerSelect(seatSel, revSel, preferredReviewer = "") {
    const enabledSeats = activeParticipants().filter((p) => p.enabled);
    const author = enabledSeats.find((p) => p.id === seatSel.value);
    const prevValue = preferredReviewer || revSel.value;
    const validReviewers = author ? enabledSeats.filter((p) => p.provider !== author.provider) : [];
    revSel.replaceChildren();
    if (!validReviewers.length) {
      const opt = el("option", "", "다른 공급사 활성 좌석 없음");
      opt.value = "";
      revSel.appendChild(opt);
      revSel.disabled = true;
      return;
    }
    revSel.disabled = false;
    for (const r of validReviewers) {
      const opt = el(
        "option",
        "",
        participantLabel(r.id) + " (" + (LABEL[r.provider] || r.provider) + ")"
      );
      opt.value = r.id;
      revSel.appendChild(opt);
    }
    if (validReviewers.some((r) => r.id === prevValue)) {
      revSel.value = prevValue;
    } else {
      revSel.value = validReviewers[0].id;
    }
  }

  function addVerifyRow(container, initial = null) {
    const row = el("div", "work-verify-row");
    const nameLabel = el("label", "", "이름");
    const nameInput = el("input", "verify-name");
    nameInput.placeholder = "예: test";
    if (initial && initial.name) nameInput.value = initial.name;
    nameLabel.appendChild(nameInput);
    row.appendChild(nameLabel);

    const argvLabel = el("label", "verify-argv-label", "실행 인자 (argv, 한 줄에 인자 1개)");
    const argvInput = el("textarea", "verify-argv mono");
    argvInput.rows = 3;
    argvInput.placeholder = "예:\ngo\ntest\n./...";
    if (initial && Array.isArray(initial.argv)) argvInput.value = initial.argv.join("\n");
    argvLabel.appendChild(argvInput);
    row.appendChild(argvLabel);

    const timeoutLabel = el("label", "", "제한 시간(초)");
    const timeoutInput = el("input", "verify-timeout");
    timeoutInput.type = "number";
    timeoutInput.min = "1";
    timeoutInput.value = initial && initial.timeout_sec ? String(initial.timeout_sec) : "600";
    timeoutLabel.appendChild(timeoutInput);
    row.appendChild(timeoutLabel);

    const removeBtn = el("button", "ghost remove-row-btn", "삭제");
    removeBtn.type = "button";
    removeBtn.onclick = () => row.remove();
    row.appendChild(removeBtn);

    container.appendChild(row);
  }

  function addPartRow(container, removable = true) {
    const row = el("div", "work-part-row");
    const enabledSeats = activeParticipants().filter((p) => p.enabled);

    const titleLabel = el("label", "", "파트 제목");
    const titleInput = el("input", "part-title-input");
    titleInput.required = true;
    titleInput.placeholder = "예: P1 리뷰 프로토콜";
    titleLabel.appendChild(titleInput);
    row.appendChild(titleLabel);

    const scopeLabel = el("label", "", "범위 경로 (쉼표 구분)");
    const scopeInput = el("input", "part-scope-input mono");
    scopeInput.required = true;
    scopeInput.placeholder = "예: internal/room/review.go, internal/room/review_test.go";
    scopeLabel.appendChild(scopeInput);
    row.appendChild(scopeLabel);

    const briefLabel = el("label", "part-brief-label", "완료 조건 (Brief)");
    const briefInput = el("input", "part-brief-input");
    briefInput.required = true;
    briefInput.placeholder = "예: review_types.go 메서드 및 단위 테스트 통과";
    briefLabel.appendChild(briefInput);
    row.appendChild(briefLabel);

    const seatLabel = el("label", "", "담당 좌석");
    const seatSel = el("select", "part-seat-select");
    seatSel.required = true;
    if (!enabledSeats.length) {
      const opt = el("option", "", "활성 좌석 없음");
      opt.value = "";
      seatSel.appendChild(opt);
      seatSel.disabled = true;
    } else {
      for (const s of enabledSeats) {
        const opt = el(
          "option",
          "",
          participantLabel(s.id) + " (" + (LABEL[s.provider] || s.provider) + ")"
        );
        opt.value = s.id;
        seatSel.appendChild(opt);
      }
    }
    seatLabel.appendChild(seatSel);
    row.appendChild(seatLabel);

    const revLabel = el("label", "", "리뷰어 좌석 (다른 공급사)");
    const revSel = el("select", "part-reviewer-select");
    revSel.required = true;
    revLabel.appendChild(revSel);
    row.appendChild(revLabel);

    seatSel.onchange = () => syncReviewerSelect(seatSel, revSel);
    syncReviewerSelect(seatSel, revSel);

    if (removable) {
      const removeBtn = el("button", "ghost remove-row-btn", "파트 삭제");
      removeBtn.type = "button";
      removeBtn.onclick = () => row.remove();
      row.appendChild(removeBtn);
    }

    container.appendChild(row);
  }

  function openWorkPlanDialog() {
    if (!state.room || state.room.mode !== "write") return;
    const dlg = $("#dlg-work");
    const f = $("#form-work");
    const errBox = $("#work-form-error");
    if (!dlg || !f) return;
    errBox.hidden = true;
    errBox.textContent = "";
    f.goal.value = "";
    f.merge_mode.value = "confirm";
    f.review_rounds.value = "3";

    const verifyBox = $("#work-verify-rows");
    verifyBox.replaceChildren();
    addVerifyRow(verifyBox);

    const partBox = $("#work-part-rows");
    partBox.replaceChildren();
    addPartRow(partBox, false);

    dlg.showModal();
  }

  async function onWorkPlanSubmit(e) {
    e.preventDefault();
    if (!state.roomId) return;
    const f = e.target;
    const errBox = $("#work-form-error");
    const submitBtn = $("#dlg-work-ok");
    errBox.hidden = true;
    errBox.textContent = "";

    const showErr = (msg) => {
      errBox.hidden = false;
      errBox.textContent = msg;
    };

    const goal = f.goal.value.trim();
    if (!goal) return showErr("목표(Goal)를 입력하세요.");
    const merge_mode = f.merge_mode.value;
    const review_rounds = Number(f.review_rounds.value);
    if (!Number.isInteger(review_rounds) || review_rounds < 1) {
      return showErr("리뷰 라운드 상한은 1 이상의 정수여야 합니다.");
    }

    const verify = [];
    for (const row of $$(".work-verify-row", $("#work-verify-rows"))) {
      const name = $(".verify-name", row).value.trim();
      const argvRaw = $(".verify-argv", row).value.replace(/\r\n/g, "\n");
      const timeout_sec = Number($(".verify-timeout", row).value);
      if (!name && argvRaw === "") continue;
      if (!name || argvRaw === "") {
        return showErr("검증 명령의 이름과 실행 인자(argv)를 모두 입력하세요.");
      }
      const argv = argvRaw.split("\n");
      if (!argv[0].trim()) {
        return showErr("검증 명령의 첫 번째 인자(실행 파일)는 비워둘 수 없습니다.");
      }
      if (!Number.isFinite(timeout_sec) || timeout_sec < 1) {
        return showErr("검증 명령 제한 시간은 1초 이상이어야 합니다.");
      }
      verify.push({ name, argv, timeout_sec: Math.floor(timeout_sec) });
    }

    const partRows = $$(".work-part-row", $("#work-part-rows"));
    if (!partRows.length) {
      return showErr("최소 1개 이상의 파트를 추가하세요.");
    }

    const parts = [];
    const enabledSeats = activeParticipants().filter((p) => p.enabled);
    for (const row of partRows) {
      const title = $(".part-title-input", row).value.trim();
      const brief = $(".part-brief-input", row).value.trim();
      const scopeRaw = $(".part-scope-input", row).value.trim();
      const seat = $(".part-seat-select", row).value;
      const reviewer = $(".part-reviewer-select", row).value;
      if (!title || !brief) {
        return showErr("모든 파트의 제목과 완료 조건을 입력하세요.");
      }
      const scope = scopeRaw
        .split(/[\n,]+/)
        .map((s) => s.trim())
        .filter(Boolean);
      if (!scope.length) {
        return showErr("파트 '" + title + "'의 범위 경로(scope)를 1개 이상 입력하세요.");
      }
      const authorSeat = enabledSeats.find((p) => p.id === seat);
      const reviewerSeat = enabledSeats.find((p) => p.id === reviewer);
      if (!authorSeat || !reviewerSeat) {
        return showErr("파트 '" + title + "'의 담당 좌석과 리뷰어 좌석을 선택하세요.");
      }
      if (authorSeat.provider === reviewerSeat.provider) {
        return showErr("파트 '" + title + "'의 리뷰어는 담당과 다른 공급사 좌석이어야 합니다.");
      }
      parts.push({ title, brief, scope, seat, reviewer });
    }

    const roomId = state.roomId;
    const sequence = openSeq;
    const workRev = state.workRevision;
    submitBtn.disabled = true;
    try {
      const plan = await api("POST", "/api/rooms/" + roomId + "/work", {
        goal,
        merge_mode,
        review_rounds,
        verify,
        parts,
      });
      if (state.roomId !== roomId || openSeq !== sequence) return;
      if (state.workRevision === workRev) {
        state.workRevision++;
        state.work = plan;
      }
      state.workBoardCollapsed = false;
      $("#dlg-work").close();
      renderRoomHead();
      renderWork();
      announce("작업 계획이 확정되었습니다.");
    } catch (err) {
      if (state.roomId === roomId && openSeq === sequence) {
        showErr(err.message);
      }
    } finally {
      submitBtn.disabled = false;
    }
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

  // Desktop panel collapse. Narrow screens keep the drawer behaviour, so the
  // usage panel only moves into the chat area while the sidebar is a grid column.
  const sidebarColumn = window.matchMedia("(min-width: 761px)");
  function applyPanelLayout() {
    document.body.classList.toggle("left-collapsed", state.leftCollapsed);
    document.body.classList.toggle("right-collapsed", state.rightCollapsed);
    const panel = $("#provider-panel");
    const dock = $("#usage-dock");
    const docked = state.leftCollapsed && sidebarColumn.matches;
    const home = docked ? dock : $("#sidebar");
    if (panel.parentElement !== home) home.appendChild(panel);
    dock.hidden = !docked;
    $$(".panel-collapse-left, .panel-expand-left").forEach((b) => b.setAttribute("aria-expanded", String(!state.leftCollapsed)));
    $$(".panel-collapse-right, .panel-expand-right").forEach((b) => b.setAttribute("aria-expanded", String(!state.rightCollapsed)));
  }

  function setPanelCollapsed(side, collapsed) {
    const key = side === "left" ? "leftCollapsed" : "rightCollapsed";
    state[key] = collapsed;
    localStorage.setItem("rt." + key, collapsed ? "1" : "0");
    applyPanelLayout();
    // Keep focus on a visible control for the same panel.
    const next = collapsed
      ? $$(".panel-expand-" + side).find((b) => b.offsetParent)
      : $(side === "left" ? "#btn-collapse-left" : "#btn-collapse-right");
    if (next && next.offsetParent) next.focus();
  }

  function bindUI() {
    $("#btn-collapse-left").onclick = () => setPanelCollapsed("left", true);
    $("#btn-collapse-right").onclick = () => setPanelCollapsed("right", true);
    $$(".panel-expand-left").forEach((b) => (b.onclick = () => setPanelCollapsed("left", false)));
    $$(".panel-expand-right").forEach((b) => (b.onclick = () => setPanelCollapsed("right", false)));
    sidebarColumn.addEventListener("change", applyPanelLayout);
    applyPanelLayout();
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
    const jumpBottomBtn = $("#btn-jump-bottom");
    if (jumpBottomBtn) {
      jumpBottomBtn.onclick = () => scrollToTimelineBottom();
    }
    const timelineEl = $("#timeline");
    if (timelineEl) {
      const cancelPendingScrollRestore = () => {
        pendingRestoreScrollTop = null;
        suppressScrollStick = false;
      };
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
    $("#btn-copy-room-seats").onclick = () => {
      const box = $("#room-seat-specs");
      box.replaceChildren();
      for (const p of state.room.participants) addSeatSpec(box, p.provider, true, p);
    };

    const btnWorkPlan = $("#btn-work-plan");
    if (btnWorkPlan) {
      btnWorkPlan.onclick = () => {
        closeMoreMenu();
        openWorkPlanDialog();
      };
    }
    const dlgWorkCancel = $("#dlg-work-cancel");
    if (dlgWorkCancel) dlgWorkCancel.onclick = () => $("#dlg-work").close();
    const btnAddVerify = $("#btn-add-verify");
    if (btnAddVerify) btnAddVerify.onclick = () => addVerifyRow($("#work-verify-rows"));
    const btnAddPart = $("#btn-add-part");
    if (btnAddPart) btnAddPart.onclick = () => addPartRow($("#work-part-rows"), true);
    const formWork = $("#form-work");
    if (formWork) formWork.onsubmit = onWorkPlanSubmit;

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

    const lightboxCloseBtn = $("#lightbox-close");
    if (lightboxCloseBtn) {
      lightboxCloseBtn.onclick = () => closeLightbox();
    }
    const dlgLightbox = $("#dlg-lightbox");
    if (dlgLightbox) {
      dlgLightbox.addEventListener("click", (e) => {
        if (e.target === dlgLightbox) closeLightbox();
      });
      dlgLightbox.addEventListener("close", () => {
        const img = $("#lightbox-img");
        if (img) img.removeAttribute("src");
      });
    }

    const btnAttach = $("#btn-attach");
    const fileInput = $("#file-input");
    if (btnAttach && fileInput) {
      btnAttach.onclick = () => {
        if (!state.roomId) return;
        fileInput.click();
      };
      fileInput.addEventListener("change", () => {
        if (fileInput.files && fileInput.files.length > 0) {
          addAttachmentFiles(fileInput.files);
        }
        fileInput.value = "";
      });
    }

    const composerEl = $("#composer");
    if (composerEl) {
      composerEl.onsubmit = (e) => {
        e.preventDefault();
        send();
      };
      let dragDepth = 0;
      const hasFiles = (dt) => dt && dt.types && Array.from(dt.types).includes("Files");
      composerEl.addEventListener("dragenter", (e) => {
        if (!hasFiles(e.dataTransfer)) return;
        e.preventDefault();
        dragDepth++;
        composerEl.classList.add("drag-over");
      });
      composerEl.addEventListener("dragover", (e) => {
        if (!hasFiles(e.dataTransfer)) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = "copy";
        composerEl.classList.add("drag-over");
      });
      composerEl.addEventListener("dragleave", (e) => {
        if (!hasFiles(e.dataTransfer)) return;
        dragDepth = Math.max(0, dragDepth - 1);
        if (dragDepth === 0) composerEl.classList.remove("drag-over");
      });
      composerEl.addEventListener("drop", (e) => {
        if (!hasFiles(e.dataTransfer)) return;
        e.preventDefault();
        dragDepth = 0;
        composerEl.classList.remove("drag-over");
        if (e.dataTransfer.files && e.dataTransfer.files.length > 0) {
          addAttachmentFiles(e.dataTransfer.files);
        }
      });
    }

    const input = $("#input");
    input.addEventListener("paste", (e) => {
      const files = extractClipboardImageFiles(e.clipboardData);
      if (files.length > 0) {
        e.preventDefault();
        addAttachmentFiles(files);
      }
    });
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
      state.work = null;
      state.requests.clear();
      closeLightbox();
      clearDraftAttachments();
      closeDiffDrawer();
      renderUsagePanel();
      renderRequests();
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
        const diffDrawer = $("#diff-drawer");
        if (diffDrawer && !diffDrawer.hidden) {
          closeDiffDrawer();
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

  let draftAttachmentSeq = 0;

  function showComposerError(msg) {
    const error = $("#composer-error");
    if (!error) return;
    error.hidden = false;
    error.textContent = msg;
    const input = $("#input");
    if (input) input.setAttribute("aria-invalid", "true");
  }

  function clearComposerError() {
    const error = $("#composer-error");
    if (!error) return;
    error.hidden = true;
    error.textContent = "";
    const input = $("#input");
    if (input) input.removeAttribute("aria-invalid");
  }

  function hasAttachmentError() {
    return state.draftAttachments.some((item) => item.status === "error");
  }

  function activeDraftAttachmentCount() {
    return state.draftAttachments.filter((item) => item.status === "uploading" || item.status === "ready").length;
  }

  function clearDraftAttachments() {
    for (const item of state.draftAttachments) {
      item.cancelled = true;
      item.sending = false;
      if (item.previewUrl) {
        URL.revokeObjectURL(item.previewUrl);
        item.previewUrl = "";
      }
    }
    state.draftAttachments = [];
    renderDraftAttachments();
  }

  function removeDraftAttachmentsByIds(localIds) {
    if (!localIds || !localIds.size) return;
    const kept = [];
    for (const item of state.draftAttachments) {
      if (localIds.has(item.localId)) {
        item.cancelled = true;
        item.sending = false;
        if (item.previewUrl) {
          URL.revokeObjectURL(item.previewUrl);
          item.previewUrl = "";
        }
      } else {
        kept.push(item);
      }
    }
    state.draftAttachments = kept;
    promoteWaitingCountAttachments();
    if (!hasAttachmentError()) {
      clearComposerError();
    }
    renderDraftAttachments();
  }

  function removeDraftAttachment(localId) {
    if (isSendingMessage) return;
    const idx = state.draftAttachments.findIndex((item) => item.localId === localId);
    if (idx < 0) return;
    if (state.draftAttachments[idx].sending) return;
    const [removed] = state.draftAttachments.splice(idx, 1);
    removed.cancelled = true;
    if (removed.previewUrl) {
      URL.revokeObjectURL(removed.previewUrl);
      removed.previewUrl = "";
    }
    promoteWaitingCountAttachments();
    if (!hasAttachmentError()) {
      clearComposerError();
    }
    renderDraftAttachments();
  }

  function promoteWaitingCountAttachments() {
    if (!state.roomId) return;
    for (const item of state.draftAttachments) {
      if (activeDraftAttachmentCount() >= MAX_ATTACHMENTS) break;
      if (item.status === "error" && item.errorCode === "count" && item.file) {
        const canSlice = typeof item.file.slice === "function";
        const check = validateAttachmentCandidate({
          mime: canSlice ? "" : item.file.type,
          size: item.size,
          activeCount: activeDraftAttachmentCount(),
        });
        if (check.ok) {
          item.status = "uploading";
          item.error = "";
          item.errorCode = "";
          item.uploadPromise = verifyAndUploadAttachment(item);
        } else {
          item.error = check.reason;
          item.errorCode = check.code;
        }
      }
    }
  }

  function renderDraftAttachments() {
    const box = $("#composer-attachments");
    if (!box) return;
    if (!state.draftAttachments.length) {
      box.hidden = true;
      box.replaceChildren();
      return;
    }
    box.hidden = false;
    const nodes = state.draftAttachments.map((item) => {
      let cls = "att-chip";
      if (item.status === "uploading") cls += " uploading";
      else if (item.status === "error") cls += " error";
      const chip = el("div", cls);
      chip.dataset.localId = item.localId;

      if (item.previewUrl) {
        const thumb = document.createElement("img");
        thumb.className = "att-chip-thumb";
        thumb.src = item.previewUrl;
        thumb.alt = item.name;
        chip.appendChild(thumb);
      } else {
        const placeholder = el("div", "att-chip-thumb", "🖼");
        placeholder.style.display = "flex";
        placeholder.style.alignItems = "center";
        placeholder.style.justifyContent = "center";
        chip.appendChild(placeholder);
      }

      const info = el("div", "att-chip-info");
      const nameEl = el("div", "att-chip-name", item.name);
      nameEl.title = item.name;
      info.appendChild(nameEl);

      const sizeText = fmtBytes(item.uploaded ? item.uploaded.bytes : item.size);
      const statusSuffix = item.status === "uploading" ? " · 업로드 중…" : "";
      info.appendChild(el("div", "att-chip-meta", sizeText + statusSuffix));

      if (item.status === "error" && item.error) {
        const errEl = el("div", "att-chip-err", item.error);
        errEl.title = item.error;
        info.appendChild(errEl);
      }
      chip.appendChild(info);

      const rmBtn = el("button", "att-chip-remove", "✕");
      rmBtn.type = "button";
      rmBtn.title = "첨부 제거";
      rmBtn.setAttribute("aria-label", item.name + " 첨부 제거");
      rmBtn.disabled = isSendingMessage || Boolean(item.sending);
      rmBtn.onclick = () => removeDraftAttachment(item.localId);
      chip.appendChild(rmBtn);
      return chip;
    });
    box.replaceChildren(...nodes);
  }

  function extractClipboardImageFiles(clipboardData) {
    if (!clipboardData) return [];
    const out = [];
    if (clipboardData.items && clipboardData.items.length) {
      for (const item of Array.from(clipboardData.items)) {
        if (item.kind === "file") {
          const f = item.getAsFile();
          if (f) out.push(f);
        }
      }
    }
    if (!out.length && clipboardData.files && clipboardData.files.length) {
      for (const f of Array.from(clipboardData.files)) {
        out.push(f);
      }
    }
    return out;
  }

  function addAttachmentFiles(fileList) {
    if (isSendingMessage || !state.roomId || !fileList) return;
    const files = Array.from(fileList);
    if (!files.length) return;

    for (const file of files) {
      const localId = "draft-att-" + ++draftAttachmentSeq;
      const name = file.name ? file.name : "clipboard-image.png";
      const size = typeof file.size === "number" ? file.size : 0;
      const activeCount = activeDraftAttachmentCount();
      const canSlice = Boolean(file && typeof file.slice === "function");
      const check = validateAttachmentCandidate({
        mime: canSlice ? "" : file.type,
        size,
        activeCount,
      });

      let previewUrl = "";
      if (typeof URL !== "undefined" && typeof URL.createObjectURL === "function" && size > 0) {
        try {
          previewUrl = URL.createObjectURL(file);
        } catch (_) {
          previewUrl = "";
        }
      }

      const item = {
        localId,
        roomId: state.roomId,
        file,
        name,
        size,
        previewUrl,
        status: check.ok ? "uploading" : "error",
        error: check.ok ? "" : check.reason,
        errorCode: check.ok ? "" : check.code,
        uploaded: null,
        uploadPromise: null,
        cancelled: false,
        sending: false,
      };
      state.draftAttachments.push(item);
      if (check.ok) {
        item.uploadPromise = verifyAndUploadAttachment(item);
      }
    }
    renderDraftAttachments();
  }

  async function verifyAndUploadAttachment(item) {
    const roomId = item.roomId;
    try {
      if (item.file && typeof item.file.slice === "function") {
        const slice = item.file.slice(0, 12);
        if (typeof slice.arrayBuffer === "function") {
          const buf = await slice.arrayBuffer();
          if (item.cancelled || state.roomId !== roomId) return null;
          const magicMime = detectImageMimeFromBytes(new Uint8Array(buf));
          const magicCheck = validateAttachmentCandidate({
            mime: item.file.type,
            magicMime,
            size: item.size,
            activeCount: 0,
          });
          if (!magicCheck.ok) {
            item.status = "error";
            item.error = magicCheck.reason;
            item.errorCode = magicCheck.code;
            promoteWaitingCountAttachments();
            renderDraftAttachments();
            return null;
          }
        }
      }

      const fd = new FormData();
      fd.append("file", item.file, item.name);
      const res = await fetch("/api/rooms/" + encodeURIComponent(roomId) + "/attachments", {
        method: "POST",
        body: fd,
      });
      if (item.cancelled || state.roomId !== roomId) return null;
      const raw = await res.text();
      let data = null;
      if (raw) {
        try {
          data = JSON.parse(raw);
        } catch (_) {
          data = null;
        }
      }
      if (!res.ok) {
        const reason = data && data.error ? data.error : raw ? raw.trim() : res.statusText;
        throw new Error(reason);
      }
      if (!data || !data.id) {
        throw new Error("서버 응답에 첨부 ID가 없습니다.");
      }
      item.uploaded = data;
      item.status = "ready";
      item.error = "";
      item.errorCode = "";
      renderDraftAttachments();
      return data;
    } catch (err) {
      if (item.cancelled || state.roomId !== roomId) return null;
      item.status = "error";
      item.error = "업로드 실패: " + (err && err.message ? err.message : "알 수 없는 오류");
      item.errorCode = "upload";
      promoteWaitingCountAttachments();
      renderDraftAttachments();
      return null;
    }
  }

  let isSendingMessage = false;

  function setComposerBusy(busy) {
    isSendingMessage = busy;
    $("#input").readOnly = busy;
    $("#btn-send").disabled = busy;
    $("#btn-attach").disabled = busy;
    $("#file-input").disabled = busy;
    renderDraftAttachments();
  }

  async function send() {
    if (isSendingMessage || !state.roomId) return;
    const input = $("#input");
    const rawInput = input.value;
    const draft = rawInput.trim();
    let text;
    try { text = encodedMentions(draft); }
    catch (err) {
      showComposerError(err.message);
      input.focus();
      return;
    }
    if (hasAttachmentError()) {
      showComposerError("오류가 있는 첨부 칩을 제거한 뒤 전송하세요.");
      return;
    }
    clearComposerError();
    const sendingItems = state.draftAttachments.filter(
      (item) => !item.cancelled && (item.status === "uploading" || item.status === "ready")
    );
    if (!text && sendingItems.length === 0) return;

    const sendingIds = new Set(sendingItems.map((item) => item.localId));
    for (const item of sendingItems) item.sending = true;

    const roomId = state.roomId;
    const sequence = openSeq;
    const target = state.target;
    closeMentionPop();
    setComposerBusy(true);

    try {
      const pendingUploads = sendingItems
        .filter((item) => item.status === "uploading" && item.uploadPromise)
        .map((item) => item.uploadPromise);

      if (pendingUploads.length > 0) {
        await Promise.all(pendingUploads);
        if (state.roomId !== roomId || openSeq !== sequence) return;
      }

      if (sendingItems.some((item) => item.status === "error" || !item.uploaded || !item.uploaded.id)) {
        showComposerError("이미지 업로드에 실패했습니다. 오류 칩을 확인하세요.");
        return;
      }

      const attachmentIds = sendingItems.map((item) => item.uploaded.id);
      if (input.value === rawInput) {
        input.value = "";
      }

      const payload = { text, target };
      if (attachmentIds.length > 0) {
        payload.attachments = attachmentIds;
      }
      await api("POST", "/api/rooms/" + roomId + "/messages", payload);
      if (state.roomId !== roomId || openSeq !== sequence) return;
      removeDraftAttachmentsByIds(sendingIds);
      if (!input.value) state.draftMentions.clear();
      if (target) {
        setTarget("", false);
      } else {
        updateHint();
      }
    } catch (e) {
      if (state.roomId !== roomId || openSeq !== sequence) return;
      alert(e.message);
      if (!input.value) {
        input.value = rawInput;
      } else if (input.value !== rawInput) {
        input.value = rawInput + "\n" + input.value;
      }
      updateHint();
    } finally {
      for (const item of sendingItems) item.sending = false;
      setComposerBusy(false);
    }
  }

  // ---------- room dialog ----------
  function addSeatSpec(box, providerId = "", removable = true, seed = null) {
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
    if (seed) {
      const known = [...model.options].some((o) => o.value === seed.model);
      model.value = known ? seed.model : "__custom__";
      custom.value = known ? "" : seed.model;
      syncModel();
      effort.value = seed.effort;
      label.value = seed.label;
    }
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
    $("#btn-copy-room-seats").hidden = !!room || !state.room;
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

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      fmtBytes,
      detectImageMimeFromBytes,
      validateAttachmentCandidate,
      requestStatusLabel,
      isIncompleteRequest,
      sortRequests,
      mergeSnapshotRequests,
      formatLightboxMeta,
      classifyAgentMarkerLine,
      prepareAgentMessageText,
      formatAgentMessageText,
      applyRejectedMarkerChips,
      md,
    };
  }

  if (typeof document !== "undefined") {
    boot().catch((e) => {
      document.body.innerHTML = "<pre style='padding:20px'>시작 실패: " + esc(e.message) + "</pre>";
    });
  }
})();
