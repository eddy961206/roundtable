// Safe Markdown rendering, agent marker lines and @mention text rewriting.
import { esc, LABEL } from "./util.js";
import { activeParticipants, participant, participantLabel, state } from "./state.js";

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

export function applyRejectedMarkerChips(container, rejectedMarkers) {
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
    else if (token.type === "text") { token.text = visibleMentions(token.text); token.escaped = false; }
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
    return markedLib.parser(tokens);
  } catch {
    return "<p>" + esc(text) + "</p>";
  }
};

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
