// 3D space view (docs/사내에이전트_커뮤니티_설계.md 9절): the open room drawn as
// an office floor in CSS 3D. Seats stand as avatars around a table (workroom)
// or a coffee table (lounge) and show their state and latest words. The view
// reads only what the page already holds (room, messages, decisions): it sends
// no request and costs no token. Clicking an avatar picks it as the target.
import { $, $$, el } from "./util.js";
import { toggleDrawer } from "./layout.js";
import { participantLabel, state } from "./state.js";
import { setTarget } from "./composer.js";

const W = 900; // floor width in scene pixels
const D = 600; // floor depth
const TILT = 58; // camera tilt in degrees
const BUBBLE_CHARS = 80;
const FULL_AGE = 2; // the three newest messages show their words
const BUBBLE_AGE = 10; // up to here an older message shows a speech icon with its text as a tooltip
const RECENT = 40; // messages scanned for bubbles

let open = false;
const START_YAW = -28;
let yaw = START_YAW;
let zoom = 1;
let fit = 1; // scale that makes the floor fill the stage
let fitSize = ""; // stage size fit was measured for
let frame = 0;
let sceneKey = "";
let narrow = false; // a phone shows one full bubble: enlarged avatars leave no room for more

function store(key, value) {
  try { localStorage.setItem(key, value); } catch { /* private window: the view still works */ }
}
function load(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

export function bindSpaceView() {
  const btn = $("#btn-space-view");
  open = load("rt.spaceView") === "1";
  btn.onclick = () => setOpen(!open);
  $("#sv-close").onclick = () => setOpen(false);
  $("#sv-left").onclick = () => turn(-30);
  $("#sv-right").onclick = () => turn(30);
  $("#sv-reset").onclick = () => { yaw = START_YAW; zoom = 1; applyCamera(); };
  const stage = $("#sv-stage");
  let drag = null;
  stage.onpointerdown = (e) => {
    if (e.target.closest(".sv-agent")) return;
    drag = { x: e.clientX, yaw };
    stage.setPointerCapture(e.pointerId);
  };
  stage.onpointermove = (e) => {
    if (!drag) return;
    yaw = drag.yaw + (e.clientX - drag.x) * 0.4;
    applyCamera();
  };
  stage.onpointerup = stage.onpointercancel = () => { drag = null; };
  stage.onwheel = (e) => {
    e.preventDefault();
    zoom = Math.min(1.8, Math.max(0.5, zoom * (e.deltaY < 0 ? 1.1 : 0.9)));
    applyCamera();
  };
  stage.onkeydown = (e) => {
    if (e.key === "ArrowLeft") { e.preventDefault(); turn(-15); }
    if (e.key === "ArrowRight") { e.preventDefault(); turn(15); }
  };
  window.addEventListener("resize", () => { if (open) applyCamera(); });
  document.addEventListener("rt-target", scheduleSpaceView); // the selection ring follows the composer target
  syncButton();
  scheduleSpaceView(); // a room may have opened before the saved view state was read
}

function setOpen(next) {
  open = next;
  store("rt.spaceView", open ? "1" : "0");
  syncButton();
  if (open) scheduleSpaceView();
}

function syncButton() {
  const btn = $("#btn-space-view");
  btn.setAttribute("aria-pressed", String(open));
  btn.textContent = open ? "대화 보기" : "3D 보기";
  btn.title = open ? "대화 목록으로 돌아갑니다" : "이 방을 3D 사무실로 봅니다. 좌석의 상태와 마지막 말이 보입니다. 토큰을 쓰지 않습니다.";
  $("#space-view").hidden = !open || !state.room;
}

function turn(deg) {
  yaw += deg;
  applyCamera();
}

// The world is centered in the stage. Its scale comes from the floor's
// projected size at the starting angle, measured once per stage size, so the
// room fills a wide screen and still fits a phone.
function applyCamera() {
  const stage = $("#sv-stage");
  const world = $("#sv-world");
  const floor = world && world.querySelector(".sv-floor");
  if (!floor || !stage.clientWidth) return;
  const size = stage.clientWidth + "x" + stage.clientHeight;
  if (size !== fitSize) {
    world.style.transition = "none";
    world.style.transform = `translate(-50%, -50%) rotateX(${TILT}deg) rotateZ(${START_YAW}deg)`;
    const r = floor.getBoundingClientRect();
    // Room above the floor for the avatars of the back row and their words.
    fit = Math.min(1.2, (stage.clientWidth - 32) / r.width, (stage.clientHeight - 80) / r.height);
    fitSize = size;
    void world.offsetWidth;
    world.style.transition = "";
  }
  const k = (fit * zoom).toFixed(3);
  // scale3d, not scale: after the tilt the floor has depth, and an unscaled depth
  // would bring its near edge up to the camera on a narrow screen.
  world.style.transform = `translate(-50%, -50%) scale3d(${k}, ${k}, ${k}) rotateX(${TILT}deg) rotateZ(${yaw}deg)`;
  // Billboards counter-rotate so avatars and labels keep facing the camera.
  world.style.setProperty("--sv-yaw", -yaw + "deg");
  // Avatars and their words grow back when the room is shrunk to fit, so a
  // name stays readable (about 11px or more) on a phone.
  world.style.setProperty("--sv-ui", Math.min(1.6, Math.max(1, 0.75 / k)).toFixed(2));
}

export function scheduleSpaceView() {
  if (!open || frame) return;
  frame = requestAnimationFrame(() => {
    frame = 0;
    renderSpaceView();
  });
}

function renderSpaceView() {
  syncButton();
  const r = state.room;
  if (!open || !r) return;
  const kind = r.kind === "lounge" ? "lounge" : "work";
  const seats = r.participants.filter((p) => !p.removed);
  const key = r.id + "|" + kind + "|" + seats.map((p) => p.id).join(",");
  const world = $("#sv-world");
  narrow = $("#sv-stage").clientWidth < 640;
  if (key !== sceneKey) {
    sceneKey = key;
    world.replaceChildren(buildFloor(kind, r), el("div", "sv-agents"));
  }
  $("#sv-title").textContent = (kind === "lounge" ? "☕ 휴게실" : "🛠 작업실") + " · " + r.title;
  $("#sv-hint").textContent = r.agents_paused ? "에이전트 발화가 멈춰 있습니다" : "끌어서 돌리기 · 휠로 확대 · 아바타를 누르면 그 좌석에게 보냅니다";
  renderAttention(renderAgents(world.querySelector(".sv-agents"), kind, seats));
  applyCamera();
}

// ---------- scene ----------
function face(cls, css) {
  const f = el("div", "sv-face " + cls);
  Object.assign(f.style, css);
  return f;
}

// box draws a solid w×d×h block standing on the floor at (x, y).
function box(x, y, w, d, h, cls) {
  const b = el("div", "sv-box " + cls);
  b.style.transform = `translate3d(${x}px, ${y}px, 0)`;
  b.append(
    face("top", { width: w + "px", height: d + "px", transform: `translateZ(${h}px)` }),
    face("south", { width: w + "px", height: h + "px", top: d + "px", transformOrigin: "top", transform: "rotateX(90deg)" }),
    face("north", { width: w + "px", height: h + "px", top: -h + "px", transformOrigin: "bottom", transform: "rotateX(-90deg)" }),
    face("west", { width: h + "px", height: d + "px", left: -h + "px", transformOrigin: "right", transform: "rotateY(90deg)" }),
    face("east", { width: h + "px", height: d + "px", left: w + "px", transformOrigin: "left", transform: "rotateY(-90deg)" }),
  );
  return b;
}

// prop is an upright billboard (plant, coffee machine sign) on the floor.
function prop(x, y, text, cls) {
  const p = el("div", "sv-prop " + cls);
  p.style.transform = `translate3d(${x}px, ${y}px, 0)`;
  p.appendChild(el("div", "sv-billboard", text));
  return p;
}

function buildFloor(kind, room) {
  const floor = el("div", "sv-floor " + kind);
  floor.style.width = W + "px";
  floor.style.height = D + "px";
  floor.appendChild(el("div", "sv-floor-label", kind === "lounge" ? "LOUNGE" : "WORKROOM"));
  const parts = [floor, box(0, -14, W, 14, 70, "wall"), box(-14, -14, 14, D + 14, 70, "wall")];
  if (kind === "lounge") {
    parts.push(
      box(390, 250, 120, 120, 26, "table round"),
      box(250, 110, 400, 56, 34, "sofa"),
      box(170, 210, 56, 200, 34, "sofa"),
      box(675, 210, 56, 200, 34, "sofa"),
      box(40, 40, 70, 50, 70, "counter"),
      prop(75, 115, "☕", "coffee"),
      prop(830, 60, "🪴", "plant"),
      prop(840, 540, "🪴", "plant"),
      prop(60, 540, "🛋️", "plant"),
    );
  } else {
    parts.push(
      box(250, 230, 400, 140, 30, "table"),
      box(760, 30, 110, 24, 90, "board"),
      prop(815, 60, room.agents_paused ? "⏸" : "📋", "board-sign"),
      prop(840, 540, "🪴", "plant"),
      prop(50, 60, "🪴", "plant"),
    );
  }
  const door = el("div", "sv-door", "사람");
  door.style.transform = "translate3d(60px, 530px, 1px)";
  parts.push(door);
  const scene = el("div", "sv-scene");
  scene.append(...parts);
  return scene;
}

// ---------- seats ----------
// Seats sit on an ellipse around the table; a person stands by the door.
function seatSpot(i, n, kind) {
  const cx = 450, cy = 300;
  const rx = kind === "lounge" ? 200 : 255, ry = kind === "lounge" ? 150 : 140;
  const a = -Math.PI / 2 + (2 * Math.PI * i) / Math.max(n, 1) + (n % 2 ? 0 : Math.PI / Math.max(n, 1));
  return [cx + rx * Math.cos(a), cy + ry * Math.sin(a)];
}

function plain(text) {
  return String(text || "")
    .replace(/```[\s\S]*?(```|$)/g, " ")
    .split("\n")
    .filter((l) => !/^\s*\[\[/.test(l))
    .join(" ")
    .replace(/[*_#>`|]/g, "")
    .replace(/\s+/g, " ")
    .trim();
}

// latestWords picks each author's newest message among the recent ones and
// how recent it is (0 = the newest message of the room).
function latestWords() {
  const out = new Map();
  const ids = state.order.slice(-RECENT);
  for (let i = ids.length - 1; i >= 0; i--) {
    const m = state.messages.get(ids[i]);
    if (!m || (m.kind !== "agent" && m.kind !== "user")) continue;
    const who = m.kind === "user" ? "user" : m.author;
    if (out.has(who)) continue;
    out.set(who, { m, age: ids.length - 1 - i });
  }
  return out;
}

// seatState is the one state an avatar shows first. One dominant signal per
// person keeps the scene scannable; the order is how urgently a person must act.
function seatState(p, now) {
  if (approvals(p.id).length) return "approve";
  if (!p.enabled) return "off";
  if (p.budget_held || (p.hold_until && p.hold_until > now)) return "held";
  if (p.provider === "remote" && !remoteOnline(p, now)) return "away";
  if (p.status === "running") return "running";
  if (p.status === "queued") return "queued";
  return "idle";
}

function remoteOnline(p, now) {
  return p.status === "running" || (p.remote_seen_at && now - p.remote_seen_at < 120000);
}

// approvals lists task result cards waiting for a person, of one seat or all.
function approvals(seat) {
  return [...state.decisions.values()].filter((d) => d.task && d.status === "proposed" && (!seat || d.by === seat));
}

function chips(p, now, st) {
  const list = [];
  if (p.provider === "remote") {
    const online = remoteOnline(p, now);
    list.push(["📡", online ? "다른 PC의 worker가 실행 · 연결됨" : "다른 PC의 worker가 실행 · 연결 안 됨", online ? "ok" : "off"]);
  }
  if (st === "queued" && p.paced_until > now) {
    list.push(["⏱ " + new Date(p.paced_until).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }), "발화 간격이 지나면 답합니다", "warn"]);
  } else if (st === "queued") list.push(["⏱ 차례 대기", "앞선 턴이 끝나면 답합니다", "warn"]);
  if (st === "held") list.push(["⏸ 보류", p.budget_held || p.hold_reason || "보류", "danger"]);
  if (st === "off") list.push(["꺼짐", "참여가 꺼져 있습니다", "off"]);
  return list;
}

function avatar(holder, id) {
  let a = holder.querySelector(`[data-seat="${CSS.escape(id)}"]`);
  if (a) return a;
  a = el("button", "sv-agent");
  a.type = "button";
  a.dataset.seat = id;
  const fig = el("div", "sv-figure");
  fig.append(el("div", "sv-alert", "!"), el("div", "sv-bubble"), el("div", "sv-chips"), el("div", "sv-head"), el("div", "sv-body"), el("div", "sv-name"));
  a.append(el("div", "sv-ring"), el("div", "sv-shadow"), fig);
  holder.appendChild(a);
  return a;
}

function paintAvatar(a, { x, y, label, color, cls, words, chipList, initial }) {
  a.style.transform = `translate3d(${x.toFixed(1)}px, ${y.toFixed(1)}px, 0)`;
  a.className = "sv-agent " + cls;
  a.style.setProperty("--c", color);
  const fig = a.querySelector(".sv-figure");
  fig.querySelector(".sv-head").textContent = initial;
  fig.querySelector(".sv-name").textContent = label;
  const bubble = fig.querySelector(".sv-bubble");
  bubble.textContent = words.text;
  bubble.title = words.full || "";
  bubble.hidden = !words.text;
  bubble.className = "sv-bubble" + (words.typing ? " typing" : "") + (words.compact ? " compact" : "");
  const chipBox = fig.querySelector(".sv-chips");
  chipBox.replaceChildren(...chipList.map(([text, title, tone]) => {
    const c = el("span", "sv-chip " + tone, text);
    c.title = title;
    return c;
  }));
}

// Bubble budget: only the newest messages speak in full, so the eye has a
// few things to read, not a wall of text. Older words shrink to a speech
// icon and keep their text in the tooltip; an answering seat shows dots.
function wordsOf(entry) {
  if (!entry || entry.age > BUBBLE_AGE) return { text: "" };
  const { m, age } = entry;
  const text = plain(m.text);
  if (m.status === "streaming" && !text) return { text: "•••", typing: true };
  if (m.status === "error") return { text: "⚠ 답하지 못함", full: m.error || "", compact: true };
  if (age > (narrow ? 0 : FULL_AGE)) return { text: "💬", full: text, compact: true };
  return { text: text.length > BUBBLE_CHARS ? text.slice(0, BUBBLE_CHARS - 1) + "…" : text, full: text, typing: m.status === "streaming" };
}

const STATE_WORDS = { approve: "결과 승인 대기", off: "꺼짐", held: "보류", away: "worker 연결 안 됨", running: "답하는 중", queued: "차례 대기", idle: "쉬는 중" };

function renderAgents(holder, kind, seats) {
  const now = Date.now();
  const words = latestWords();
  const keep = new Set(["user"]);
  const counts = {};
  seats.forEach((p, i) => {
    keep.add(p.id);
    const st = seatState(p, now);
    counts[st] = (counts[st] || 0) + 1;
    const [x, y] = seatSpot(i, seats.length, kind);
    const cls = ["st-" + st, p.status === "running" ? "running" : "", p.provider === "remote" ? "remote" : "", state.target === p.id ? "selected" : ""].filter(Boolean).join(" ");
    const a = avatar(holder, p.id);
    const label = participantLabel(p.id);
    paintAvatar(a, { x, y, label, color: `var(--${p.provider}, var(--accent))`, cls, words: wordsOf(words.get(p.id)), chipList: chips(p, now, st), initial: label.slice(0, 1).toUpperCase() });
    a.setAttribute("aria-label", label + " · " + STATE_WORDS[st] + (state.target === p.id ? " · 다음 메시지 대상" : "") + " · 누르면 이 좌석에게 보냅니다");
    a.setAttribute("aria-pressed", String(state.target === p.id));
    a.onclick = () => setTarget(p.id, true);
  });
  const me = avatar(holder, "user");
  paintAvatar(me, { x: 90, y: 525, label: "나", color: "var(--user)", cls: "human", words: wordsOf(words.get("user")), chipList: [], initial: "나" });
  me.setAttribute("aria-label", "나(사람) · 누르면 대상 지정을 풀고 전체에게 보냅니다");
  me.onclick = () => setTarget("", true);
  for (const a of [...holder.children]) if (!keep.has(a.dataset.seat)) a.remove();
  return counts;
}

// ---------- attention bar ----------
// A person looking in first asks "do I have to do anything, and is anyone
// working?". The bar answers in order of urgency; only the approval item is
// an action, and it takes the person to the card to approve.
function renderAttention(counts) {
  const bar = $("#sv-attn");
  const pending = approvals().length;
  const items = [];
  if (pending) items.push(["❗ 승인 대기 " + pending + "건", "accent", "과제 결과를 승인하거나 반려합니다", showDecisions]);
  if (counts.held) items.push(["⏸ 보류 " + counts.held, "danger", "예산·발화 상한·멈춤으로 새 턴을 시작하지 않는 좌석"]);
  if (counts.away) items.push(["📡 연결 끊김 " + counts.away, "danger", "worker가 연결돼 있지 않은 원격 좌석"]);
  if (counts.running) items.push(["● 답하는 중 " + counts.running, "ok", "지금 답을 쓰는 좌석"]);
  if (counts.queued) items.push(["⏱ 차례 대기 " + counts.queued, "warn", "앞선 턴이나 발화 간격을 기다리는 좌석"]);
  if (counts.off) items.push(["꺼짐 " + counts.off, "off", "참여가 꺼진 좌석"]);
  if (!items.length) items.push(["모두 쉬는 중 · 할 일 없음", "off", "실행·대기 중인 턴과 승인할 결과가 없습니다"]);
  const sig = JSON.stringify(items.map((i) => i[0]));
  if (bar.dataset.sig === sig) return;
  bar.dataset.sig = sig;
  bar.replaceChildren(...items.map(([text, tone, title, action]) => {
    const item = el(action ? "button" : "span", "sv-attn-item " + tone, text);
    item.title = title;
    if (action) {
      item.type = "button";
      item.onclick = action;
    }
    return item;
  }));
}

function showDecisions() {
  const expand = $$(".panel-expand-right").find((b) => b.offsetParent);
  if (expand) expand.click();
  else if (!document.body.classList.contains("show-right") && $("#btn-right").offsetParent) toggleDrawer("right");
  const card = $("#decisions .dec-item.status-proposed");
  if (!card) return;
  card.scrollIntoView({ block: "center" });
  const confirm = card.querySelector(".dec-confirm");
  if (confirm) confirm.focus();
}
