// 3D space view (docs/사내에이전트_커뮤니티_설계.md 9절): the open room drawn as
// an office floor in CSS 3D. Seats stand as avatars around a table (workroom)
// or a coffee table (lounge) and show their state and latest words. The view
// reads only what the page already holds (room, messages, decisions): it sends
// no request and costs no token. Clicking an avatar picks it as the target.
//
// The page that shows it passes a source (bindSpaceView): the host page reads
// its room state, the people page (people.js) its own snapshot. This module
// imports nothing else of either page.
//
// Review 2026-11-03 (research on AI Town, AI Village, Pixel Agents, ChatDev):
// a seat card answers "what is this one doing and why" without hovering, a
// line shows who called whom, and a replay steps through the recent talk
// that a person missed. All of it reads the same page data.
import { $, el } from "./util.js";

// src: { room(), messages(): newest-last array, decisions(): array, target(),
// label(id), pick(id), canSend(), showDecisions(), alwaysOpen }
let src = null;

const W = 900; // floor width in scene pixels
const D = 600; // floor depth
const TILT = 58; // camera tilt in degrees
const BUBBLE_CHARS = 80;
const FULL_AGE = 2; // the three newest messages show their words
const BUBBLE_AGE = 10; // up to here an older message shows a speech icon with its text as a tooltip
const RECENT = 40; // messages scanned for bubbles
const REPLAY = 20; // messages a replay steps through
const REPLAY_STEP = 1600; // ms per replayed message: time to read a bubble
const LINK_AGE = 3 * 60 * 1000; // a call line leaves the floor after this
const HUMAN_SPOT = [90, 525];

let open = false;
const START_YAW = -28;
let yaw = START_YAW;
let zoom = 1;
let fit = 1; // scale that makes the floor fill the stage
let fitSize = ""; // stage size fit was measured for
let frame = 0;
let sceneKey = "";
let narrow = false; // a phone shows one full bubble: enlarged avatars leave no room for more
let cardSeat = ""; // the seat whose card is open
let replay = null; // { all, steps, i, timer } while a replay runs
const spots = new Map(); // seat id → floor position of this render

function store(key, value) {
  try { localStorage.setItem(key, value); } catch { /* private window: the view still works */ }
}
function load(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}

export function bindSpaceView(source) {
  src = source;
  const btn = $("#btn-space-view");
  open = src.alwaysOpen || load("rt.spaceView") === "1";
  if (btn) btn.onclick = () => setOpen(!open);
  $("#sv-close").hidden = !!src.alwaysOpen;
  $("#sv-close").onclick = () => setOpen(false);
  $("#sv-left").onclick = () => turn(-30);
  $("#sv-right").onclick = () => turn(30);
  $("#sv-reset").onclick = () => { yaw = START_YAW; zoom = 1; applyCamera(); };
  $("#sv-zoom-in").onclick = () => zoomBy(1.15);
  $("#sv-zoom-out").onclick = () => zoomBy(1 / 1.15);
  $("#sv-replay").onclick = () => (replay ? stopReplay() : startReplay());
  const stage = $("#sv-stage");
  const card = el("aside", "sv-card");
  card.id = "sv-card";
  card.hidden = true;
  card.setAttribute("aria-label", "좌석 정보");
  stage.appendChild(card);
  let drag = null;
  stage.onpointerdown = (e) => {
    if (e.target.closest(".sv-agent, .sv-card")) return;
    drag = { x: e.clientX, yaw };
    $("#sv-world").style.transition = "none"; // the floor follows the pointer as the avatars do
    stage.setPointerCapture(e.pointerId);
  };
  stage.onpointermove = (e) => {
    if (!drag) return;
    yaw = drag.yaw + (e.clientX - drag.x) * 0.4;
    applyCamera();
  };
  stage.onpointerup = stage.onpointercancel = () => {
    drag = null;
    $("#sv-world").style.transition = "";
  };
  // The wheel scrolls the page (the people page goes on below the stage);
  // Ctrl or ⌘ with the wheel zooms, as on a map.
  stage.onwheel = (e) => {
    if (!e.ctrlKey && !e.metaKey) return;
    e.preventDefault();
    zoomBy(e.deltaY < 0 ? 1.1 : 0.9);
  };
  stage.onkeydown = (e) => {
    if (e.key === "ArrowLeft") { e.preventDefault(); turn(-15); }
    if (e.key === "ArrowRight") { e.preventDefault(); turn(15); }
    if (e.key === "Escape" && cardSeat) { cardSeat = ""; scheduleSpaceView(); }
  };
  // A new width can cross the phone layout: render again, not only re-fit.
  window.addEventListener("resize", scheduleSpaceView);
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
  if (btn) {
    btn.setAttribute("aria-pressed", String(open));
    btn.textContent = open ? "대화 보기" : "3D 보기";
    btn.title = open ? "대화 목록으로 돌아갑니다" : "이 방을 3D 사무실로 봅니다. 좌석의 상태와 마지막 말이 보입니다. 토큰을 쓰지 않습니다.";
  }
  $("#space-view").hidden = !open || !src.room();
}

function turn(deg) {
  yaw += deg;
  applyCamera();
}

function zoomBy(k) {
  zoom = Math.min(1.8, Math.max(0.5, zoom * k));
  applyCamera();
}

// ---------- replay ----------
// A person who looked away sees the last talk again, one message at a time,
// as bubbles over the seats (seat states stay live).
function startReplay() {
  const all = src.messages().slice(-(REPLAY + RECENT) * 3); // enough for the bubbles of the replayed talk, not the whole room
  const steps = [];
  all.forEach((m, i) => { if (m && (m.kind === "agent" || m.kind === "user") && m.status !== "streaming") steps.push(i); });
  if (steps.length < 2) return;
  replay = { all, steps: steps.slice(-REPLAY), i: 0, timer: 0 };
  replay.timer = setInterval(() => {
    replay.i++;
    if (replay.i >= replay.steps.length) stopReplay();
    else scheduleSpaceView();
  }, REPLAY_STEP);
  scheduleSpaceView();
}

function stopReplay() {
  if (replay) clearInterval(replay.timer);
  replay = null;
  scheduleSpaceView();
}

// shown is the talk the bubbles and lines draw: the page's, or the replay's so far.
function shown() {
  return replay ? replay.all.slice(0, replay.steps[replay.i] + 1) : src.messages();
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
  const measure = size !== fitSize;
  if (measure) {
    world.style.transition = "none";
    world.style.transform = `translate(-50%, -50%) rotateX(${TILT}deg) rotateZ(${START_YAW}deg)`;
    const r = floor.getBoundingClientRect();
    // Room above the floor for the avatars of the back row and their words.
    fit = Math.min(1.2, (stage.clientWidth - 32) / r.width, (stage.clientHeight - 80) / r.height);
    fitSize = size;
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
  if (measure) {
    // A new fit lands at once: animating from the measuring pose would show
    // the room zooming in from far too big on every load.
    void world.offsetWidth;
    world.style.transition = "";
  }
}

// A streaming reply sends many small events; the scene redraws at most every
// DRAW_GAP ms, enough for words to grow, so a long answer does not keep the
// person's CPU busy at 60 frames a second. Turning and zooming the camera
// does not wait for this.
const DRAW_GAP = 200;
let lastDraw = 0;
export function scheduleSpaceView() {
  if (!src || !open || frame) return;
  frame = setTimeout(() => requestAnimationFrame(() => {
    frame = 0;
    lastDraw = performance.now();
    renderSpaceView();
  }), Math.max(0, lastDraw + DRAW_GAP - performance.now()));
}

function renderSpaceView() {
  syncButton();
  const r = src.room();
  if (!open || !r) return;
  const kind = r.kind === "lounge" ? "lounge" : "work";
  const seats = r.participants.filter((p) => !p.removed);
  const key = r.id + "|" + kind + "|" + seats.map((p) => p.id).join(",");
  const world = $("#sv-world");
  narrow = $("#sv-stage").clientWidth < 640;
  if (key !== sceneKey) {
    if (!sceneKey.startsWith(r.id + "|")) { // another room: its card and replay end
      cardSeat = "";
      if (replay) stopReplay();
    }
    sceneKey = key;
    world.replaceChildren(buildFloor(kind, r), el("div", "sv-links"), el("div", "sv-agents"));
  }
  $("#sv-replay").textContent = replay ? "■ 멈추기" : "▶ 다시 보기";
  $("#sv-replay").setAttribute("aria-pressed", String(!!replay));
  const talk = shown();
  const words = latestWords(talk);
  renderAttention(renderAgents(world.querySelector(".sv-agents"), kind, seats, words));
  renderLinks(world.querySelector(".sv-links"), talk);
  renderCard(seats, words);
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
// More than RING seats take a second, wider ring with every other seat, so
// names do not pile up on one curve.
const RING = 12;
function seatSpot(i, n, kind) {
  const cx = 450, cy = 300;
  let rx = kind === "lounge" ? 200 : 255, ry = kind === "lounge" ? 150 : 140;
  if (n > RING) {
    const outer = i % 2;
    const count = outer ? Math.floor(n / 2) : Math.ceil(n / 2);
    if (outer) { rx += 75; ry += 65; }
    const a = -Math.PI / 2 + (2 * Math.PI * Math.floor(i / 2)) / count + (outer ? Math.PI / count : 0);
    return [cx + rx * Math.cos(a), cy + ry * Math.sin(a)];
  }
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
// how recent it is among spoken messages (0 = the newest; notices do not count).
function latestWords(talk) {
  const out = new Map();
  const recent = talk.slice(-RECENT);
  let age = 0;
  for (let i = recent.length - 1; i >= 0; i--) {
    const m = recent[i];
    if (!m || (m.kind !== "agent" && m.kind !== "user")) continue;
    const who = m.kind === "user" ? "user" : m.author;
    if (!out.has(who)) out.set(who, { m, age });
    age++;
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

// approvals lists cards waiting for a person (task results and proposed
// decisions), of one seat or all: the same list the approval panel shows.
function approvals(seat) {
  return src.decisions().filter((d) => d.status === "proposed" && (!seat || d.by === seat));
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
  bubble.className = "sv-bubble" + (words.typing ? " typing" : "") + (words.live ? " live" : "") + (words.compact ? " compact" : "");
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
  return { text: text.length > BUBBLE_CHARS ? text.slice(0, BUBBLE_CHARS - 1) + "…" : text, full: text, live: m.status === "streaming" };
}

const STATE_WORDS = { approve: "결과 승인 대기", off: "꺼짐", held: "보류", away: "worker 연결 안 됨", running: "답하는 중", queued: "차례 대기", idle: "쉬는 중" };

function renderAgents(holder, kind, seats, words) {
  const now = Date.now();
  const keep = new Set(["user"]);
  const counts = {};
  spots.clear();
  spots.set("user", HUMAN_SPOT);
  seats.forEach((p, i) => {
    keep.add(p.id);
    const st = seatState(p, now);
    counts[st] = (counts[st] || 0) + 1;
    const [x, y] = seatSpot(i, seats.length, kind);
    spots.set(p.id, [x, y]);
    const cls = ["st-" + st, p.status === "running" ? "running" : "", p.provider === "remote" ? "remote" : "", src.target() === p.id ? "selected" : "", cardSeat === p.id ? "carded" : ""].filter(Boolean).join(" ");
    const a = avatar(holder, p.id);
    const label = src.label(p.id);
    const said = wordsOf(words.get(p.id));
    // The avatar tone (darker than the provider's text color) keeps the white
    // initial readable in both themes.
    paintAvatar(a, { x, y, label, color: `var(--${p.provider}-avatar, var(--accent))`, cls, words: said, chipList: chips(p, now, st), initial: label.slice(0, 1).toUpperCase() });
    const sendable = src.canSend() && p.enabled;
    const last = said.full || (said.text === "•••" ? "" : said.text);
    a.setAttribute("aria-label", label + " · " + STATE_WORDS[st] + (src.target() === p.id ? " · 다음 메시지 대상" : "") + (last ? " · 마지막 말: " + last.slice(0, 60) : "") + (sendable ? " · 누르면 좌석 정보와 보낼 대상" : " · 누르면 좌석 정보"));
    a.setAttribute("aria-pressed", String(src.target() === p.id));
    a.onclick = () => {
      cardSeat = p.id;
      if (sendable) src.pick(p.id); // a seat that cannot take a message only opens its card
      scheduleSpaceView();
    };
  });
  const me = avatar(holder, "user");
  paintAvatar(me, { x: HUMAN_SPOT[0], y: HUMAN_SPOT[1], label: "나", color: "var(--user-avatar)", cls: "human", words: wordsOf(words.get("user")), chipList: [], initial: "나" });
  me.setAttribute("aria-label", src.canSend() ? "나(사람) · 누르면 대상 지정을 풀고 전체에게 보냅니다" : "나(사람) · 누르면 좌석 정보를 닫습니다");
  me.onclick = () => {
    cardSeat = "";
    if (src.canSend()) src.pick("");
    scheduleSpaceView();
  };
  for (const a of [...holder.children]) if (!keep.has(a.dataset.seat)) a.remove();
  return counts;
}

// ---------- who called whom ----------
// The latest call (a message with a target or @mentions) draws a line on the
// floor from its speaker to the seats it called, for a few minutes, so the
// line stays while the called seat answers. Standing avatars can hide a floor
// line, so a called seat's ring turns dashed as well.
function callees(m) {
  return [...new Set([m.target, ...(m.mentions || [])])].filter((id) => id && id !== m.author && spots.has(id));
}

function renderLinks(holder, talk) {
  let m = null;
  for (let i = talk.length - 1; i >= Math.max(0, talk.length - RECENT) && !m; i--) {
    const c = talk[i];
    if (c && (c.kind === "agent" || c.kind === "user") && callees(c).length) m = c;
  }
  const from = m && spots.get(m.kind === "user" ? "user" : m.author);
  const to = from && (replay || Date.now() - m.ts < LINK_AGE) ? callees(m) : [];
  const caller = m && m.kind === "user" ? m.by || "사람" : m && src.label(m.author);
  for (const id of to) {
    const a = document.querySelector(`.sv-agent[data-seat="${CSS.escape(id)}"]`);
    a.classList.add("called");
    const chip = el("span", "sv-chip accent", "📣 " + caller);
    chip.title = caller + "이(가) 이 좌석을 불렀습니다";
    a.querySelector(".sv-chips").appendChild(chip);
  }
  holder.replaceChildren(...to.map((id) => {
    const [x1, y1] = from;
    const [x2, y2] = spots.get(id);
    const gap = 36; // start and end beside the avatars, not under them
    const line = el("div", "sv-link");
    line.style.width = Math.max(0, Math.hypot(x2 - x1, y2 - y1) - 2 * gap) + "px";
    line.style.transform = `translate3d(${x1}px, ${y1}px, 2px) rotateZ(${Math.atan2(y2 - y1, x2 - x1)}rad) translateX(${gap}px)`;
    return line;
  }));
}

// ---------- seat card ----------
// Clicking a seat opens its card: who it is, what it does and why, how much
// it spoke, and its last words in full. A touch screen has no tooltip, and a
// person should not have to hover to learn why a seat is quiet.
function renderCard(seats, words) {
  const card = $("#sv-card");
  const p = seats.find((s) => s.id === cardSeat);
  card.hidden = !p;
  if (!p) return;
  const now = Date.now();
  const r = src.room();
  const st = seatState(p, now);
  const head = el("div", "sv-card-head");
  head.appendChild(el("strong", "", src.label(p.id)));
  head.appendChild(el("span", "muted", p.provider === "remote" ? "다른 PC의 worker" : p.provider + " · " + p.model));
  const close = el("button", "icon-btn sv-card-close", "✕");
  close.type = "button";
  close.setAttribute("aria-label", "좌석 정보 닫기");
  close.onclick = () => { cardSeat = ""; scheduleSpaceView(); };
  head.appendChild(close);
  const why = chips(p, now, st).map(([, title]) => title).join(" · ");
  const rows = [head, el("p", "sv-card-state", STATE_WORDS[st] + (why ? " · " + why : ""))];
  let day = 0; // newest first: stop at a day ago instead of reading a long room's whole history
  const all = src.messages();
  for (let i = all.length - 1; i >= 0 && (!all[i] || now - all[i].ts < 86400000); i--) {
    if (all[i] && all[i].kind === "agent" && all[i].author === p.id) day++;
  }
  rows.push(el("p", "muted", "24시간 답 " + day + (r.daily_speak_limit ? " / " + r.daily_speak_limit : "")));
  const entry = words.get(p.id);
  const last = entry ? plain(entry.m.text) : "";
  rows.push(el("p", "sv-card-words", last ? new Date(entry.m.ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) + " · " + (last.length > 400 ? last.slice(0, 399) + "…" : last) : "최근 기록에 이 좌석의 말이 없습니다"));
  const waiting = approvals(p.id).length;
  if (waiting) {
    const go = el("button", "primary", "❗ 승인 대기 " + waiting + "건 보기");
    go.type = "button";
    go.onclick = () => src.showDecisions();
    rows.push(go);
  }
  const note = src.target() === p.id ? "다음 메시지는 이 좌석에게 갑니다" : !src.canSend() ? "구경 권한이라 보낼 수 없습니다" : !p.enabled ? "꺼진 좌석이라 보낼 수 없습니다" : "";
  if (note) rows.push(el("p", "muted", note));
  card.replaceChildren(...rows);
}

// ---------- attention bar ----------
// A person looking in first asks "do I have to do anything, and is anyone
// working?". The bar answers in order of urgency; only the approval item is
// an action, and it takes the person to the card to approve.
function renderAttention(counts) {
  const bar = $("#sv-attn");
  const pending = approvals().length;
  const items = [];
  if (replay) {
    const m = replay.all[replay.steps[replay.i]];
    items.push(["▶ 다시 보기 " + (replay.i + 1) + "/" + replay.steps.length + " · " + new Date(m.ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) + " · 누르면 멈춤", "accent", "최근 대화를 한 메시지씩 다시 보여 줍니다. 좌석 상태는 지금 상태입니다.", stopReplay]);
  }
  if (pending) items.push(["❗ 승인 대기 " + pending + "건", "accent", "좌석이 올린 결정과 과제 결과를 승인하거나 반려합니다", () => src.showDecisions()]);
  if (counts.held) items.push(["⏸ 보류 " + counts.held, "danger", "예산·발화 상한·멈춤으로 새 턴을 시작하지 않는 좌석"]);
  if (counts.away) items.push(["📡 연결 끊김 " + counts.away, "danger", "worker가 연결돼 있지 않은 원격 좌석"]);
  if (counts.running) items.push(["● 답하는 중 " + counts.running, "ok", "지금 답을 쓰는 좌석"]);
  if (counts.queued) items.push(["⏱ 차례 대기 " + counts.queued, "warn", "앞선 턴이나 발화 간격을 기다리는 좌석"]);
  if (counts.off) items.push(["꺼짐 " + counts.off, "off", "참여가 꺼진 좌석"]);
  if (src.room().agents_paused) items.push(["⏸ 에이전트 발화 멈춤", "danger", "이 공간의 좌석은 새 턴을 시작하지 않습니다"]);
  bar.hidden = !items.length; // nothing to do, nothing to read
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
