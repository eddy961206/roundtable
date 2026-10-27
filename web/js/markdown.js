// Safe Markdown rendering, agent marker lines and @mention text rewriting.
import { esc, LABEL } from "./util.js";
import { activeParticipants, authorProvider, participant, participantLabel, seatClass, state } from "./state.js";
import { MENTION_RE, parseMarkerNotice, requestMarker, resolveMention } from "./mentions.js";

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
        // 서버는 [[REQUEST:@x]]의 x를 좌석 ID 또는 고유한 표시 이름으로 받는다.
        const seat = state.room.participants.find((p) => p.id === r.to);
        if (r.to !== targetSeat && !(seat && seat.label.toLowerCase() === targetSeat.toLowerCase())) return false;
        return r.text.trim() === reqText;
      });
    return { action: matched ? "hide" : "chip", raw: trimmed };
  }
  if (trimmed.startsWith("[[DECISION")) {
    // 서버와 같은 형식 검사를 통과한 줄은 메시지 아래 결정 카드로 보이고, 거절된 줄은 칩으로 남긴다.
    const decMatch = trimmed.match(/^\[\[DECISION:\s*(.*?)\s*\|\s*근거:\s*(.*?)\s*\|\s*반대:\s*(.*?)\s*\|\s*미확인:\s*(.*?)\s*\]\]$/);
    const recorded =
      !!decMatch &&
      decMatch.slice(1).every((v) => v !== "") &&
      Array.from(decMatch[1]).length <= 300 &&
      !!msg &&
      Array.isArray(msg.decisions) &&
      msg.decisions.length > 0;
    return { action: recorded ? "hide" : "chip", raw: trimmed };
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

export function prepareAgentMessageText(text, msg) {
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

export function applyRejectedMarkerChips(container, rejectedMarkers, msg) {
  if (!container || !Array.isArray(rejectedMarkers) || rejectedMarkers.length === 0) return;
  const markerSet = new Set(rejectedMarkers);
  container.querySelectorAll("code").forEach((codeEl) => {
    if (codeEl.parentElement && codeEl.parentElement.tagName === "PRE") return;
    const txt = codeEl.textContent || "";
    if (!markerSet.has(txt)) return;
    const route = requestRouteHtml(txt, msg);
    if (route) {
      codeEl.insertAdjacentHTML("beforebegin", route);
      codeEl.remove();
      return;
    }
    codeEl.classList.add("marker-chip", "muted");
  });
}

// vendor/marked.min.js is a classic script that index.html loads before the modules.
const markedLib = typeof marked !== "undefined" ? marked : undefined;

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
    else if (token.type === "text") { token.text = placeMentions(token.text); token.escaped = false; }
    if (token.items) for (const item of token.items) if (item.tokens) friendlyMarkdownTokens(item.tokens);
    if (token.header) for (const cell of token.header) friendlyMarkdownTokens(cell.tokens);
    if (token.rows) for (const row of token.rows) for (const cell of row) friendlyMarkdownTokens(cell.tokens);
  }
}

export const md = (text) => {
  try {
    if (!markedLib) return "<p>" + esc(text) + "</p>";
    const tokens = markedLib.lexer(text || "");
    friendlyMarkdownTokens(tokens);
    return fillMentions(markedLib.parser(tokens));
  } catch {
    return "<p>" + esc(text) + "</p>";
  }
};

// Mentions become private-use placeholders before escaping and pills after, so
// the label text is escaped once and code spans/blocks are never touched.
function placeMentions(text) {
  return String(text || "").replace(MENTION_RE, (match, prefix, token) => prefix + "" + token + "");
}
function fillMentions(html) {
  return html.replace(/([^]*)/g, (match, token) => mentionPill(token));
}

// mentionPill draws "@token" by what it reaches: a seat in its colour, a provider
// group, everyone, a seat that is off (dashed), or nothing (red, not called).
export function mentionPill(token) {
  const seats = (state.room && state.room.participants) || [];
  const res = resolveMention(token, seats);
  if (res.kind === "none") {
    return '<span class="mention-pill unmatched" title="일치하는 좌석 없음 · 호출되지 않음">@' + esc(token) + "</span>";
  }
  if (res.kind === "all") {
    return '<span class="mention-pill group all" title="참여 중인 좌석 전원">@전원</span>';
  }
  if (res.kind === "group") {
    const names = res.ids.map(participantLabel).join(", ");
    return '<span class="mention-pill group ' + esc(res.provider) + '" title="' + esc(names) + '">@' + esc(LABEL[res.provider] || res.provider) + " 전체</span>";
  }
  const id = res.ids[0];
  const off = res.enabled.length === 0;
  const label = res.ids.map(participantLabel).join(", ");
  const title = off ? label + " · 꺼진 좌석이라 호출되지 않음" : label + " (" + res.ids.join(", ") + ")";
  return '<span class="mention-pill' + seatClass(id) + " " + esc(authorProvider(id)) + (off ? " off" : "") + '" title="' + esc(title) + '">@' + esc(label) + "</span>";
}

// mentionTextHtml renders plain (non-Markdown) text with mention pills.
export function mentionTextHtml(text) {
  return fillMentions(esc(rewriteProseMentions(String(text || ""), placeMentions)));
}

// seatChipHtml is the coloured name chip used in "from → to" routes.
export function seatChipHtml(id) {
  const p = participant(id);
  const off = !!p && p.enabled === false;
  return '<span class="seat-chip' + seatClass(id) + " " + esc(authorProvider(id)) + (off ? " off" : "") + '">' +
    esc(participantLabel(id)) + (off ? " (꺼짐)" : "") + "</span>";
}

// targetChipHtml resolves the @name a marker named into a seat chip, or a red
// "no such seat" chip. It also returns the readable name for aria labels.
function target(name) {
  const res = resolveMention(name, (state.room && state.room.participants) || []);
  if (res.kind === "seat" && res.ids.length === 1) return { html: seatChipHtml(res.ids[0]), name: participantLabel(res.ids[0]) };
  return { html: '<span class="seat-chip unmatched" title="일치하는 좌석 없음">@' + esc(name) + "</span>", name: "@" + name + "(일치하는 좌석 없음)" };
}

function routeHtml(fromId, targetName, kind, outcome, raw) {
  const to = targetName ? target(targetName) : null;
  const aria = "보낸 좌석 " + participantLabel(fromId) + (to ? " → 받는 좌석 " + to.name : "") + ", " + kind + ", " + outcome;
  return '<span class="marker-route rejected" role="group" aria-label="' + esc(aria) + '">' +
    seatChipHtml(fromId) + (to ? '<span class="route-arrow" aria-hidden="true">→</span>' + to.html : "") +
    '<span class="route-kind">' + esc(kind) + "</span>" +
    '<span class="route-fail">' + esc(outcome) + "</span>" +
    '<details class="route-raw"><summary>원문</summary><code>' + esc(raw) + "</code></details></span>";
}

const MARKER_KIND = { request: "요청 마커", request_result: "요청 결과 마커", attach: "첨부 마커", decision: "결정 마커" };

// markerNoticeHtml renders the server's marker rejection notice as
// "sender → target · kind · 전달 안 됨: reason"; null for other system messages.
export function markerNoticeHtml(m) {
  if (!m || !m.notice_for || !MARKER_KIND[m.notice_marker]) return null;
  const n = parseMarkerNotice(m.text);
  if (!n) return null;
  return routeHtml(m.notice_for, n.target, MARKER_KIND[m.notice_marker], "전달 안 됨: " + n.reason, n.marker);
}

// requestRouteHtml replaces a rejected [[REQUEST:@x]] line in a reply with the
// same route, taking the reason from the room's rejection notice when present.
export function requestRouteHtml(raw, msg) {
  const r = requestMarker(raw);
  if (!r || !msg) return null;
  let reason = "";
  for (const other of state.messages.values()) {
    if (other.kind === "system" && other.notice_msg === msg.id && other.notice_marker === "request") {
      const n = parseMarkerNotice(other.text);
      if (n && n.marker === raw.trim()) reason = n.reason;
    }
  }
  return routeHtml(msg.author, r.target, "요청", reason ? "전달 안 됨: " + reason : "처리되지 않음", raw.trim());
}

export function visibleMentions(text) {
  return String(text || "").replace(/(^|[\s([{])@([a-zA-Z0-9_-]+)/g, (match, prefix, id) => {
    if (LABEL[id] && id !== "user" && id !== "system") return prefix + "@" + LABEL[id] + " 전체";
    return participant(id) ? prefix + "@" + participantLabel(id) : match;
  });
}
export function rewriteProseMentions(text, rewrite) {
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
export function encodedMentions(text, strict = true) {
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
