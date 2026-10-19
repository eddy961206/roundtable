// Room list in the sidebar and the open room's header.
import { $, announce, el } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { activeParticipants, state } from "./state.js";
import { closeDrawers } from "./layout.js";
import { renderUsagePanel } from "./usage.js";
import { renderTargetControls } from "./composer.js";
import { renderWorkHeadBar } from "./work.js";

let roomsTimer = null;
let roomsLoading = null;
let roomsRefreshPending = false;
export function scheduleRoomList() {
  roomsRefreshPending = true;
  if (!roomsTimer) roomsTimer = setTimeout(() => { roomsTimer = null; loadRooms().catch((err) => announce("방 목록 갱신 실패: " + err.message)); }, 500);
}

export async function loadRooms() {
  if (roomsLoading) { roomsRefreshPending = true; return roomsLoading; }
  roomsRefreshPending = false;
  roomsLoading = api("GET", "/api/rooms").then((rooms) => { state.rooms = rooms || []; renderRoomList(); }).finally(() => {
    roomsLoading = null;
    if (roomsRefreshPending) scheduleRoomList();
  });
  return roomsLoading;
}

export function renderRoomList() {
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
      hooks.openRoom(r.id);
    };
    li.appendChild(btn);
    ul.appendChild(li);
  }
}

export function renderRoomHead() {
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
