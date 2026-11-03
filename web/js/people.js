// The people page (docs/사내에이전트_커뮤니티_설계.md 4.7): a person invited
// from another PC signs in with a token and watches community spaces in 3D.
// A member also sends, assigns one-line tasks and approves or drops results.
// It talks only to /people on the community listener; the host's own page
// and API stay on the host.
import { $, el } from "./util.js";
import { bindSpaceView, scheduleSpaceView } from "./space-view.js";

const HEADERS = { "Content-Type": "application/json", "X-RT-Person": "1" };
const FEED = 20; // messages in the side feed; the 3D view shows the newest words

let me = null;
let spaces = [];
let openSpaceId = "";
let snap = null;
let target = "";
let stream = null;
let reloadTimer = 0;

async function call(method, path, body) {
  const r = await fetch(path, { method, headers: HEADERS, credentials: "same-origin", body: body ? JSON.stringify(body) : undefined });
  const data = await r.json().catch(() => ({}));
  if (r.status === 401) {
    showLogin();
    throw new Error(data.error || "다시 로그인하세요");
  }
  if (!r.ok) throw new Error(data.error || "HTTP " + r.status);
  return data;
}

function seatLabel(id) {
  if (id === "user") return "사람";
  const p = snap && snap.room.participants.find((x) => x.id === id);
  return p ? p.label : id;
}

function showLogin() {
  if (stream) stream.close();
  stream = null;
  me = null;
  $("#pp-app").hidden = true;
  $("#pp-logout").hidden = true;
  $("#pp-me").textContent = "";
  $("#pp-login").hidden = false;
  $("#pp-login-form").token.focus();
}

async function startPeoplePage() {
  try {
    me = await call("GET", "/people/api/me");
  } catch {
    return; // showLogin already ran
  }
  $("#pp-login").hidden = true;
  $("#pp-app").hidden = false;
  $("#pp-logout").hidden = false;
  $("#pp-me").textContent = me.name + " · " + (me.role === "member" ? "참여" : "구경");
  $("#pp-composer").hidden = me.role !== "member";
  $("#pp-viewer-note").hidden = me.role === "member";
  spaces = await call("GET", "/people/api/spaces");
  renderSpaces();
  const wanted = decodeURIComponent(location.hash.slice(1));
  const first = spaces.find((s) => s.id === wanted) || spaces[0];
  if (first) openSpace(first.id);
  else $("#pp-empty").hidden = false;
}

function renderSpaces() {
  const ul = $("#pp-space-list");
  ul.replaceChildren(...spaces.map((s) => {
    const li = el("li");
    const b = el("button", "pp-space" + (s.id === openSpaceId ? " active" : ""), (s.kind === "lounge" ? "☕ " : "🛠 ") + s.title);
    b.type = "button";
    if (s.id === openSpaceId) b.setAttribute("aria-current", "page");
    if (s.active) b.appendChild(el("span", "room-spin", ""));
    b.onclick = () => openSpace(s.id);
    li.appendChild(b);
    return li;
  }));
  if (!spaces.length) ul.appendChild(el("li", "muted", "아직 열린 공간이 없습니다"));
}

async function openSpace(id) {
  openSpaceId = id;
  target = "";
  history.replaceState(null, "", "#" + encodeURIComponent(id));
  renderSpaces();
  $("#pp-empty").hidden = true;
  await reload();
  if (id !== openSpaceId) return; // another space opened while this one loaded
  if (stream) stream.close();
  stream = new EventSource("/people/api/spaces/" + encodeURIComponent(id) + "/stream");
  stream.onmessage = () => {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(reload, 150);
  };
  // A restarted host forgets sessions and the stream stops: ask again instead
  // of leaving a stale space on screen (a 401 brings the sign-in back).
  stream.onerror = () => {
    call("GET", "/people/api/me").catch((err) => showComposerError(err.message));
  };
}

async function reload() {
  if (!openSpaceId) return;
  const id = openSpaceId;
  const next = await call("GET", "/people/api/spaces/" + encodeURIComponent(id)).catch((err) => { showComposerError(err.message); return null; });
  if (!next || id !== openSpaceId) return;
  snap = next;
  renderComposer();
  renderApprovals();
  renderFeed();
  scheduleSpaceView();
}

function renderComposer() {
  const sel = $("#pp-target");
  const seats = snap.room.participants.filter((p) => !p.removed && p.enabled);
  const key = JSON.stringify(seats.map((p) => [p.id, p.label]));
  if (sel.dataset.key !== key) {
    sel.dataset.key = key;
    sel.replaceChildren(el("option", "", "전체 (켜진 좌석 모두)"), ...seats.map((p) => {
      const o = el("option", "", p.label + "에게만");
      o.value = p.id;
      return o;
    }));
    sel.firstChild.value = "";
  }
  if (target && !seats.some((p) => p.id === target)) target = "";
  sel.value = target;
  $("#pp-task").disabled = !target;
  $("#pp-task").title = target ? "고른 좌석에게 한 줄 과제로 맡깁니다" : "과제는 좌석 하나를 고른 뒤 맡길 수 있습니다";
}

function renderApprovals() {
  const ul = $("#pp-approvals");
  const waiting = snap.decisions.filter((d) => d.status === "proposed");
  const decided = snap.decisions.filter((d) => d.task && d.status !== "proposed").slice(-3);
  if (!waiting.length && !decided.length) {
    ul.replaceChildren(el("li", "muted", "승인을 기다리는 결과가 없습니다"));
    return;
  }
  ul.replaceChildren(...waiting.map((d) => {
    const li = el("li", "pp-card-item waiting");
    li.appendChild(el("div", "pp-card-text", d.text));
    li.appendChild(el("div", "muted small-note", seatLabel(d.by) + (d.evidence ? " · " + d.evidence : "")));
    // Approve on what the seat wrote, not on its title: a seat can claim
    // work it did not do (AI Village's fabrication reports).
    const answer = snap.messages.find((m) => m.id === d.source_msg);
    if (answer) {
      const said = plain(answer.text);
      li.appendChild(el("blockquote", "pp-card-answer", said.length > 280 ? said.slice(0, 279) + "…" : said));
    }
    if (me.role === "member") {
      const row = el("div", "pp-card-actions");
      const ok = el("button", "primary pp-approve", "승인");
      ok.type = "button";
      ok.onclick = () => decide(d.id, "active", ok);
      const no = el("button", "danger", "반려");
      no.type = "button";
      no.onclick = () => decide(d.id, "dropped", no);
      row.append(ok, no);
      li.appendChild(row);
    }
    return li;
  }), ...decided.map((d) => {
    const li = el("li", "pp-card-item muted");
    li.textContent = (d.status === "active" ? "✓ 승인됨 · " : "✕ 반려됨 · ") + d.text + (d.decided_by ? " · " + d.decided_by : "");
    return li;
  }));
}

async function decide(id, status, button) {
  button.disabled = true;
  try {
    await call("PATCH", "/people/api/spaces/" + encodeURIComponent(openSpaceId) + "/decisions/" + encodeURIComponent(id), { status });
    await reload();
  } catch (err) {
    showComposerError(err.message);
    button.disabled = false;
  }
}

function plain(text) {
  return String(text || "").split("\n").filter((l) => !/^\s*\[\[/.test(l)).join(" ").replace(/[*_#>`|]/g, "").replace(/\s+/g, " ").trim();
}

function renderFeed() {
  const ol = $("#pp-feed");
  const msgs = snap.messages.filter((m) => m.kind !== "system" || m.text).slice(-FEED);
  ol.replaceChildren(...msgs.map((m) => {
    const li = el("li", "pp-msg " + m.kind);
    const who = m.kind === "user" ? (m.by || "호스트") : m.kind === "system" ? "알림" : seatLabel(m.author);
    const head = el("div", "pp-msg-head");
    head.appendChild(el("strong", "", who));
    if (m.target) head.appendChild(el("span", "muted", " → " + seatLabel(m.target)));
    if (m.reason === "auto") head.appendChild(el("span", "badge", "☕ 자율 잡담"));
    if (m.reason === "auto-reply") head.appendChild(el("span", "badge", "☕ 자율 대화"));
    head.appendChild(el("span", "muted", " " + new Date(m.ts).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })));
    li.appendChild(head);
    const body = m.status === "streaming" && !plain(m.text) ? "답하는 중…" : m.status === "error" ? "⚠ 답하지 못함" : plain(m.text);
    li.appendChild(el("div", "pp-msg-text", body.length > 240 ? body.slice(0, 239) + "…" : body));
    return li;
  }));
  ol.scrollTop = ol.scrollHeight;
}

function showComposerError(text) {
  const p = $("#pp-composer-error");
  p.textContent = text;
  p.hidden = !text;
}

async function send(asTask) {
  const input = $("#pp-input");
  const text = input.value.trim();
  if (!text) return input.focus();
  showComposerError("");
  try {
    if (asTask) await call("POST", "/people/api/spaces/" + encodeURIComponent(openSpaceId) + "/tasks", { to: target, text });
    else await call("POST", "/people/api/spaces/" + encodeURIComponent(openSpaceId) + "/messages", { text, target });
    input.value = "";
    target = "";
    await reload();
  } catch (err) {
    showComposerError(err.message);
  }
}

bindSpaceView({
  alwaysOpen: true,
  room: () => (snap ? { ...snap.room } : null),
  messages: () => (snap ? snap.messages : []),
  decisions: () => (snap ? snap.decisions : []),
  target: () => target,
  label: seatLabel,
  canSend: () => !!me && me.role === "member",
  pick: (id) => {
    target = id;
    if (snap) renderComposer();
    scheduleSpaceView();
  },
  showDecisions: () => {
    $("#pp-approvals").scrollIntoView({ block: "center" });
    const first = $("#pp-approvals .pp-approve");
    if (first) first.focus();
  },
});

$("#pp-login-form").onsubmit = async (e) => {
  e.preventDefault();
  const f = e.target;
  const err = $("#pp-login-error");
  err.hidden = true;
  try {
    await call("POST", "/people/login", { token: f.token.value.trim() });
    f.token.value = "";
    await startPeoplePage();
  } catch (e2) {
    err.textContent = e2.message;
    err.hidden = false;
  }
};
$("#pp-logout").onclick = async () => {
  // A failed logout request still ends the session on this page; the cookie expires by itself.
  await call("POST", "/people/logout").catch((err) => showComposerError(err.message));
  showLogin();
};
$("#pp-target").onchange = (e) => { target = e.target.value; renderComposer(); scheduleSpaceView(); };
$("#pp-composer").onsubmit = (e) => { e.preventDefault(); send(false); };
$("#pp-task").onclick = () => send(true);
$("#pp-input").onkeydown = (e) => {
  if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(false); }
};
startPeoplePage();
