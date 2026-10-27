// Who an @mention or a rejected marker points at. Pure functions over a participant
// list, mirroring internal/room/mention.go so the screen marks the same seats the
// server calls.

const ALL = new Set(["all", "모두", "전원", "everyone"]);
const PARTICLES = new Set(["", "야", "아", "님", "에게", "한테", "께", "이", "가", "은", "는", "을", "를", "도", "의", "와", "과", "랑", "이랑"]);

// Same token shape as the server: a letter/digit/_/- run after "@", not glued to a word, path or email.
export const MENTION_RE = /(^|[^\p{L}\p{N}_@/.-])@([\p{L}\p{N}_-]+)/gu;

function tokenMatches(token, alias) {
  if (!alias || !token.startsWith(alias)) return false;
  const c = token.charAt(alias.length);
  return c === "" || !/[a-z0-9_-]/.test(c);
}

function labelMatches(token, label) {
  if (!label || /\s/.test(label) || !token.startsWith(label)) return false;
  return PARTICLES.has(token.slice(label.length));
}

// resolveMention tells what "@token" reaches:
//   kind "all"   every seat; "group" every seat of one provider; "seat" named seats;
//   "none" nothing (the server ignores it). enabled lists the seats that would run.
export function resolveMention(token, participants) {
  const t = String(token || "").toLowerCase();
  const seats = (participants || []).filter((p) => !p.removed);
  const enabledOf = (list) => list.filter((p) => p.enabled !== false).map((p) => p.id);
  if (ALL.has(t)) return { kind: "all", ids: seats.map((p) => p.id), enabled: enabledOf(seats), provider: "" };
  const byId = seats.filter((p) => tokenMatches(t, String(p.id).toLowerCase()));
  const byProvider = seats.filter((p) => p.provider && tokenMatches(t, p.provider.toLowerCase()));
  if (byId.length || byProvider.length) {
    const list = [...new Set([...byId, ...byProvider])];
    const group = byId.length === 0;
    return { kind: group ? "group" : "seat", ids: list.map((p) => p.id), enabled: enabledOf(list), provider: group ? byProvider[0].provider : "" };
  }
  const byLabel = seats.filter((p) => labelMatches(t, String(p.label || "").toLowerCase()));
  if (byLabel.length) return { kind: "seat", ids: byLabel.map((p) => p.id), enabled: enabledOf(byLabel), provider: "" };
  return { kind: "none", ids: [], enabled: [], provider: "" };
}

const NOTICE_RE = /^@(\S+) 마커 거절 \(([\s\S]*)\): ([\s\S]*)$/;
const REQUEST_RE = /^\[\[REQUEST:@([^\]\s]+)\]\]\s*([\s\S]*)$/;

// parseMarkerNotice reads the server's "@<seat> 마커 거절 (<marker>): <reason>"
// notice (requests.go markerError). target is the @name a request marker named.
export function parseMarkerNotice(text) {
  const m = NOTICE_RE.exec(String(text || ""));
  if (!m) return null;
  const req = REQUEST_RE.exec(m[2].trim());
  return { sender: m[1], marker: m[2].trim(), reason: m[3].trim(), target: req ? req[1] : "", body: req ? req[2].trim() : "" };
}

// requestMarker splits "[[REQUEST:@x]] text"; null for any other line.
export function requestMarker(line) {
  const m = REQUEST_RE.exec(String(line || "").trim());
  return m ? { target: m[1], body: m[2].trim() } : null;
}
