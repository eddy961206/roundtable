// Community spaces (docs/사내에이전트_커뮤니티_설계.md): the space badge and the
// agent pause switch in the room head, the space fields of the room dialog,
// a remote seat's worker state and connection token, and a person's task.
import { $, announce, askConfirm, el, showAlert } from "./util.js";
import { api } from "./api.js";
import { participant, participantLabel, state } from "./state.js";
import { setTarget } from "./composer.js";

export const SPACE_KIND = Object.freeze({
  work: { name: "작업실", icon: "🛠", note: "에이전트가 함께 일하는 공간입니다. 사람이 과제를 맡기고 결과를 승인합니다." },
  lounge: { name: "휴게실", icon: "☕", note: "에이전트가 짧게 잡담하는 공간입니다. 프로젝트 폴더 없이 대화만 합니다." },
});

const REMOTE_ONLINE_MS = 120000; // the server's remoteOnlineGrace

function minutes(seconds) {
  return seconds % 60 === 0 ? seconds / 60 + "분" : seconds + "초";
}

// speakSummary names a room's speak guards in one line; "" when it has none.
export function speakSummary(r) {
  const parts = [];
  if (r.speak_interval_seconds) parts.push("다른 좌석이 부른 턴은 좌석마다 " + minutes(r.speak_interval_seconds) + "에 1번");
  if (r.daily_speak_limit) parts.push("좌석마다 24시간 " + r.daily_speak_limit + "번");
  if (r.auto_chat_minutes) parts.push("조용하면 " + r.auto_chat_minutes + "분마다 자율 잡담" + (r.auto_chat_hours ? "(" + r.auto_chat_hours + "시)" : ""));
  if (r.agents_paused) parts.push("에이전트 발화 멈춤");
  return parts.join(" · ");
}

export function renderSpaceHead() {
  const r = state.room;
  const badge = $("#room-space");
  const pause = $("#btn-agents-pause");
  if (!r || !badge || !pause) return;
  const kind = SPACE_KIND[r.kind];
  const summary = speakSummary(r);
  badge.hidden = !kind && !summary;
  badge.textContent = (kind ? kind.icon + " " + kind.name : "발화 상한") + (r.agents_paused ? " · 멈춤" : "");
  badge.className = "badge space-badge" + (r.agents_paused ? " paused" : "");
  badge.title = [kind && kind.note, summary].filter(Boolean).join("\n");
  pause.hidden = !kind && !r.agents_paused;
  pause.textContent = r.agents_paused ? "에이전트 발화 재개" : "에이전트 발화 멈춤";
  pause.setAttribute("aria-pressed", String(!!r.agents_paused));
  pause.title = r.agents_paused
    ? "좌석 턴을 다시 시작합니다. 멈춘 동안 온 메시지는 그 좌석의 다음 턴에 함께 전달됩니다."
    : "이 공간의 좌석 턴을 하나도 시작하지 않습니다. 실행 중인 턴은 끝까지 돌고, 메시지는 남겨 둡니다.";
}

export function bindSpaceControls() {
  const pause = $("#btn-agents-pause");
  pause.onclick = async () => {
    const r = state.room;
    if (!r) return;
    pause.disabled = true;
    try {
      state.room = await api("PATCH", "/api/rooms/" + r.id, { agents_paused: !r.agents_paused });
      renderSpaceHead();
      announce(state.room.agents_paused ? "이 공간의 에이전트 발화를 멈췄습니다." : "이 공간의 에이전트 발화를 다시 켰습니다.");
    } catch (err) {
      showAlert("발화 설정을 바꾸지 못했습니다: " + err.message);
    } finally {
      pause.disabled = false;
    }
  };
  const f = $("#form-room");
  f.kind.onchange = () => applyKindDefaults(f);
  $("#btn-assign").onclick = () => sendTask();
  $("#btn-remote-token-copy").onclick = () => copyField("#remote-token-value", "토큰을 복사했습니다.");
  $("#btn-remote-command-copy").onclick = () => copyField("#remote-token-command", "worker 명령을 복사했습니다.");
  $("#btn-people").onclick = () => openPeopleDialog();
  $("#btn-people-invite").onclick = () => invitePerson();
  $("#btn-people-copy").onclick = () => copyField("#people-token", "초대 토큰을 복사했습니다.");
  $("#dlg-people-close").onclick = () => {
    $("#people-token").value = ""; // shown once; do not leave it in the page
    $("#people-issued").hidden = true;
    $("#dlg-people").close();
  };
  $("#dlg-remote-token-close").onclick = () => {
    $("#remote-token-value").value = ""; // the token is shown once; do not leave it in the page
    $("#dlg-remote-token").close();
  };
}

// The room dialog: the kind is chosen only for a new room; the speak guards
// can be changed later in the settings.
export function fillSpaceFields(f, room) {
  $("#room-kind-row").hidden = !!room;
  f.kind.value = room ? room.kind || "" : "";
  f.speak_interval_seconds.value = room ? room.speak_interval_seconds || 0 : 0;
  f.daily_speak_limit.value = room ? room.daily_speak_limit || 0 : 0;
  f.agents_paused.checked = room ? !!room.agents_paused : false;
  f.auto_chat_minutes.value = room ? room.auto_chat_minutes || 0 : 0;
  f.auto_chat_hours.value = room ? room.auto_chat_hours || "" : "";
  // Auto chat and the pause change a running space, so they live in its settings.
  for (const id of ["#room-paused-label", "#room-autochat-label", "#room-autochat-hours-label"]) $(id).hidden = !room;
}

export function readSpaceFields(f, body, editing) {
  body.speak_interval_seconds = Number(f.speak_interval_seconds.value || 0);
  body.daily_speak_limit = Number(f.daily_speak_limit.value || 0);
  if (editing) {
    body.agents_paused = f.agents_paused.checked;
    body.auto_chat_minutes = Number(f.auto_chat_minutes.value || 0);
    body.auto_chat_hours = f.auto_chat_hours.value.trim();
  }
  else if (f.kind.value) body.kind = f.kind.value;
}

function applyKindDefaults(f) {
  const d = (state.config.space_kinds || {})[f.kind.value];
  if (d) {
    f.mode.value = d.mode;
    f.persona.checked = d.persona;
    f.max_chain.value = d.max_chain;
    f.speak_interval_seconds.value = d.speak_interval_seconds;
    f.daily_speak_limit.value = d.daily_speak_limit;
  } else {
    f.speak_interval_seconds.value = 0;
    f.daily_speak_limit.value = 0;
  }
}

// ---------- remote seats ----------
export function renderRemoteSeat(card, p) {
  let line = card.querySelector(".remote-line");
  if (p.provider !== "remote") {
    if (line) line.remove();
    return;
  }
  if (!line) {
    line = el("div", "remote-line");
    line.appendChild(el("span", "remote-state"));
    const issue = el("button", "ghost remote-token-btn");
    issue.type = "button";
    issue.onclick = () => issueRemoteToken(p.id);
    line.appendChild(issue);
    card.querySelector(".status-line").before(line);
  }
  const online = p.status === "running" || (p.remote_seen_at && Date.now() - p.remote_seen_at < REMOTE_ONLINE_MS);
  const status = line.querySelector(".remote-state");
  status.className = "remote-state " + (online ? "online" : "offline");
  status.textContent = online ? "● worker 연결됨" : p.remote_token ? "○ worker 연결 안 됨" : "○ 연결 토큰 없음";
  status.title = p.remote_seen_at
    ? "worker가 마지막으로 확인한 시각: " + new Date(p.remote_seen_at).toLocaleString()
    : "아직 연결한 worker가 없습니다. 연결 토큰을 발급해 그 PC에서 worker를 실행하세요.";
  const issue = line.querySelector(".remote-token-btn");
  issue.textContent = p.remote_token ? "토큰 다시 발급" : "연결 토큰 발급";
  issue.title = "이 좌석의 턴을 다른 PC에서 실행할 worker용 토큰을 발급합니다. 토큰은 한 번만 보입니다.";
}

async function issueRemoteToken(seat) {
  const p = participant(seat);
  if (!p) return;
  if (p.remote_token && !(await askConfirm("새 토큰을 발급하면 지금 토큰으로 연결한 worker는 더 이상 이 좌석을 실행하지 못합니다. 다시 발급할까요?", { okText: "다시 발급", danger: true }))) return;
  try {
    const res = await api("POST", "/api/rooms/" + state.roomId + "/participants/" + seat + "/remote-token", {});
    showRemoteToken(p, res);
  } catch (err) {
    showAlert("토큰을 발급하지 못했습니다: " + err.message);
  }
}

function showRemoteToken(p, res) {
  const url = res.community_url || "http://<이 PC 주소>:<community 포트>";
  $("#remote-token-seat").textContent = participantLabel(p.id) + " (" + p.id + ")";
  $("#remote-token-value").value = res.token;
  $("#remote-token-command").value = "roundtable community worker --url " + url + " --cli claude --token-file <토큰을 저장한 파일> --mode chat";
  $("#remote-token-off").hidden = !!res.community_url;
  $("#dlg-remote-token").showModal();
}

async function copyField(sel, done) {
  try {
    await navigator.clipboard.writeText($(sel).value);
    announce(done);
  } catch (err) {
    showAlert("복사하지 못했습니다: " + err.message);
  }
}

// ---------- people on other PCs (people.go, people.html) ----------
async function openPeopleDialog() {
  $("#people-issued").hidden = true;
  $("#form-people").name.value = "";
  $("#dlg-people").showModal();
  await renderPeople();
}

async function renderPeople() {
  const ul = $("#people-list");
  try {
    const res = await api("GET", "/api/community/people");
    $("#people-off").hidden = !!res.community_url;
    if (!res.people.length) {
      ul.replaceChildren();
      return;
    }
    ul.replaceChildren(...res.people.map((p) => {
      const li = el("li", "people-item");
      li.append(el("strong", "", p.name), el("span", "badge", p.role === "member" ? "참여" : "구경"), el("span", "muted small-note", new Date(p.at).toLocaleDateString()));
      const out = el("button", "ghost danger", "초대 취소");
      out.type = "button";
      out.title = "이 사람의 토큰과 로그인을 바로 끊습니다";
      out.onclick = async () => {
        if (!(await askConfirm(p.name + "의 초대를 취소할까요? 그 사람의 토큰과 로그인이 바로 끊깁니다.", { okText: "초대 취소", danger: true }))) return;
        try {
          await api("DELETE", "/api/community/people/" + encodeURIComponent(p.id));
          announce(p.name + "의 초대를 취소했습니다.");
          await renderPeople();
        } catch (err) {
          showAlert("초대를 취소하지 못했습니다: " + err.message);
        }
      };
      li.appendChild(out);
      return li;
    }));
  } catch (err) {
    ul.replaceChildren(el("li", "warn", "목록을 읽지 못했습니다: " + err.message));
  }
}

async function invitePerson() {
  const f = $("#form-people");
  const name = f.name.value.trim();
  if (!name) return f.name.focus();
  try {
    const res = await api("POST", "/api/community/people", { name, role: f.role.value });
    $("#people-token").value = res.token;
    $("#people-url").textContent = res.community_url || "http://<이 PC 주소>:<community 포트>";
    $("#people-issued").hidden = false;
    f.name.value = "";
    await renderPeople();
  } catch (err) {
    showAlert("초대하지 못했습니다: " + err.message);
  }
}

// ---------- tasks ----------
// sendTask sends the composer text as a person's one-line task for the chosen
// seat. When the seat signals done, a result card waits in the decision panel.
async function sendTask() {
  const input = $("#input");
  const button = $("#btn-assign");
  const to = state.target;
  const text = input.value.trim();
  if (!to || !text) {
    input.focus();
    return;
  }
  button.disabled = true;
  try {
    await api("POST", "/api/rooms/" + state.roomId + "/tasks", { to, text });
    if (input.value.trim() === text) input.value = "";
    setTarget("", false);
    announce(participantLabel(to) + "에게 과제를 맡겼습니다. 끝나면 결정 패널에 승인 대기 카드가 올라옵니다.");
  } catch (err) {
    showAlert("과제를 맡기지 못했습니다: " + err.message);
  } finally {
    button.disabled = false;
  }
}
