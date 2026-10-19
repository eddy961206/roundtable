// Room list in the sidebar (with the per-room ⋯ menu) and the open room's header (with inline rename).
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

// The room-list feed only signals; the list itself always comes from GET /api/rooms.
let roomsFeedSeen = false;
export function connectRoomsFeed() {
  const es = new EventSource("/api/stream");
  state.roomsEs = es;
  es.onmessage = () => scheduleRoomList();
  // A later hello is a reconnection: read the changes made while the feed was down.
  es.addEventListener("hello", () => {
    if (roomsFeedSeen) scheduleRoomList();
    roomsFeedSeen = true;
  });
}

export async function loadRooms() {
  if (roomsLoading) { roomsRefreshPending = true; return roomsLoading; }
  roomsRefreshPending = false;
  roomsLoading = api("GET", "/api/rooms").then((rooms) => {
    state.rooms = rooms || [];
    if (state.roomId && !state.rooms.some((r) => r.id === state.roomId)) {
      announce("이 방은 다른 곳에서 삭제됨");
      hooks.closeRoom("이 방은 다른 곳에서 삭제됨");
    }
    renderRoomList();
  }).finally(() => {
    roomsLoading = null;
    if (roomsRefreshPending) scheduleRoomList();
  });
  return roomsLoading;
}

// ---------- rename / delete / export shared by the header, the list menu and the more menu ----------
export async function renameRoom(id, title) {
  const next = String(title || "").trim();
  const current = state.rooms.find((r) => r.id === id);
  if (!next) throw new Error("방 이름은 비울 수 없습니다.");
  if (current && current.title === next) return;
  await api("PATCH", "/api/rooms/" + id, { title: next });
  if (current) current.title = next;
  if (state.room && state.room.id === id) {
    state.room.title = next;
    renderRoomHead();
  }
  renderRoomList();
  announce("방 이름을 '" + next + "'(으)로 바꿨습니다.");
}

export async function deleteRoom(id) {
  const target = state.rooms.find((r) => r.id === id) || (state.room && state.room.id === id ? state.room : null);
  const name = target ? target.title : id;
  if (!confirm("'" + name + "' 방과 대화 기록을 영구 삭제할까요? 되돌릴 수 없습니다.")) return false;
  await api("DELETE", "/api/rooms/" + id);
  if (state.roomId === id) hooks.closeRoom("");
  announce("'" + name + "' 방을 삭제했습니다.");
  loadRooms();
  return true;
}

export function exportRoom(id) {
  const a = document.createElement("a");
  a.href = "/api/rooms/" + id + "/export.md";
  a.download = "";
  document.body.appendChild(a);
  a.click();
  a.remove();
}

// ---------- room list ----------
// Which room's ⋯ menu is open and which is being renamed inline; both survive re-renders.
let menuOpenFor = "";
let renamingId = "";
let renameDraft = "";
let rebuildingList = false;

function closeRoomMenu(restoreFocus = true) {
  if (!menuOpenFor) return;
  const id = menuOpenFor;
  menuOpenFor = "";
  renderRoomList();
  if (restoreFocus) {
    const trigger = $('#room-list .room-menu-btn[data-room="' + CSS.escape(id) + '"]');
    if (trigger) trigger.focus();
  }
}

function beginRename(id) {
  const r = state.rooms.find((x) => x.id === id);
  if (!r) return;
  menuOpenFor = "";
  renamingId = id;
  renameDraft = r.title;
  renderRoomList();
  const input = $("#room-list .room-rename-input");
  if (input) { input.focus(); input.select(); }
}

function endRename() {
  renamingId = "";
  renameDraft = "";
  renderRoomList();
  const btn = $("#room-list li.active .room-item") || $("#room-list .room-item");
  if (btn) btn.focus();
}

function buildRoomMenu(r) {
  const pop = el("div", "room-menu-pop");
  pop.setAttribute("role", "menu");
  pop.setAttribute("aria-label", r.title + " 방 동작");
  const items = [
    ["이름 바꾸기", () => beginRename(r.id)],
    ["내보내기 (Markdown)", () => { closeRoomMenu(); exportRoom(r.id); }],
    ["삭제…", async () => {
      closeRoomMenu();
      try { await deleteRoom(r.id); } catch (err) { alert("방을 삭제하지 못했습니다: " + err.message); }
    }],
  ];
  for (const [text, action] of items) {
    const b = el("button", "room-menu-item" + (text.startsWith("삭제") ? " danger" : ""), text);
    b.type = "button";
    b.setAttribute("role", "menuitem");
    b.onclick = (e) => { e.stopPropagation(); action(); };
    pop.appendChild(b);
  }
  pop.onkeydown = (e) => {
    const buttons = Array.from(pop.querySelectorAll("button"));
    const idx = buttons.indexOf(document.activeElement);
    if (e.key === "ArrowDown") { e.preventDefault(); buttons[(idx + 1) % buttons.length].focus(); }
    else if (e.key === "ArrowUp") { e.preventDefault(); buttons[(idx - 1 + buttons.length) % buttons.length].focus(); }
    else if (e.key === "Home") { e.preventDefault(); buttons[0].focus(); }
    else if (e.key === "End") { e.preventDefault(); buttons[buttons.length - 1].focus(); }
    else if (e.key === "Escape" || e.key === "Tab") { e.preventDefault(); closeRoomMenu(); }
  };
  return pop;
}

function buildRenameRow(r) {
  const form = el("form", "room-rename");
  const input = el("input", "room-rename-input");
  input.type = "text";
  input.maxLength = 120;
  input.value = renameDraft;
  input.setAttribute("aria-label", "새 방 이름 (Enter 저장, Esc 취소)");
  input.oninput = () => { renameDraft = input.value; };
  input.onkeydown = (e) => {
    if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); renamingId = ""; endRename(); }
  };
  form.appendChild(input);
  let busy = false;
  const save = async () => {
    if (busy) return;
    if (!input.value.trim() || input.value.trim() === r.title) { endRename(); return; }
    busy = true;
    input.disabled = true; // this also blurs the field; `busy` keeps the blur from saving twice
    try {
      await renameRoom(r.id, input.value);
      endRename();
    } catch (err) {
      input.disabled = false;
      input.focus();
      alert("방 이름을 바꾸지 못했습니다: " + err.message);
    } finally {
      busy = false;
    }
  };
  form.onsubmit = (e) => { e.preventDefault(); save(); };
  // Leaving the field keeps what was typed, like the header editor.
  // A blur caused by the list being rebuilt is not the user leaving the field.
  input.onblur = () => { if (!busy && !rebuildingList && renamingId === r.id) save(); };
  return form;
}

export function renderRoomList() {
  const ul = $("#room-list");
  if (!ul) return;
  // A feed-driven re-render must not break an inline rename in progress: remember the
  // input's focus and caret, then put them back on the rebuilt input.
  const prevInput = renamingId ? $(".room-rename-input", ul) : null;
  const caret = prevInput && document.activeElement === prevInput
    ? { start: prevInput.selectionStart, end: prevInput.selectionEnd, dir: prevInput.selectionDirection }
    : null;
  rebuildingList = true;
  try {
    ul.innerHTML = "";
    buildRoomListItems(ul);
  } finally {
    rebuildingList = false;
  }
  if (caret && renamingId) {
    const input = $(".room-rename-input", ul);
    if (input) {
      input.focus();
      input.setSelectionRange(caret.start, caret.end, caret.dir || "none");
    }
  }
}

function buildRoomListItems(ul) {
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
  if (menuOpenFor && !filtered.some((r) => r.id === menuOpenFor)) menuOpenFor = "";
  if (renamingId && !filtered.some((r) => r.id === renamingId)) { renamingId = ""; renameDraft = ""; }
  for (const r of filtered) {
    const isAct = r.id === state.roomId;
    const li = el("li", isAct ? "active" : "");
    if (renamingId === r.id) {
      li.appendChild(buildRenameRow(r));
      ul.appendChild(li);
      continue;
    }
    const rowBox = el("div", "room-row");
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
    rowBox.appendChild(btn);

    const menuBtn = el("button", "icon-btn room-menu-btn", "⋯");
    menuBtn.type = "button";
    menuBtn.dataset.room = r.id;
    menuBtn.setAttribute("aria-label", r.title + " 방 메뉴 (이름 바꾸기·삭제·내보내기)");
    menuBtn.setAttribute("aria-haspopup", "menu");
    menuBtn.setAttribute("aria-expanded", menuOpenFor === r.id ? "true" : "false");
    menuBtn.title = "이름 바꾸기 · 삭제 · 내보내기";
    menuBtn.onclick = (e) => {
      e.stopPropagation();
      if (menuOpenFor === r.id) { closeRoomMenu(); return; }
      menuOpenFor = r.id;
      renderRoomList();
      const first = $('#room-list li[data-menu-open] .room-menu-item');
      if (first) first.focus();
    };
    rowBox.appendChild(menuBtn);
    li.appendChild(rowBox);
    if (menuOpenFor === r.id) {
      li.dataset.menuOpen = "true";
      li.appendChild(buildRoomMenu(r));
    }
    ul.appendChild(li);
  }
}

// Clicking anywhere outside closes the open ⋯ menu; the global Escape handler calls closeRoomMenu too.
export function bindRoomList() {
  document.addEventListener("click", (e) => {
    if (!menuOpenFor) return;
    const open = $("#room-list li[data-menu-open]");
    if (open && open.contains(e.target)) return;
    closeRoomMenu(false);
  });
}
export function roomMenuIsOpen() { return !!menuOpenFor; }
export { closeRoomMenu };

// ---------- room header: inline title edit ----------
let titleEditing = false;
export function bindRoomTitleEdit() {
  const title = $("#room-title");
  const btn = $("#btn-rename-room");
  const input = $("#room-title-input");
  if (!title || !btn || !input) return;
  const start = () => {
    if (!state.room || titleEditing) return;
    titleEditing = true;
    input.value = state.room.title;
    title.hidden = true;
    btn.hidden = true;
    input.hidden = false;
    input.focus();
    input.select();
  };
  const stop = () => {
    titleEditing = false;
    input.hidden = true;
    title.hidden = false;
    btn.hidden = false;
  };
  let saving = false;
  const save = async () => {
    if (saving || !state.room) return;
    const id = state.room.id;
    const next = input.value.trim();
    if (!next || next === state.room.title) { stop(); btn.focus(); return; }
    saving = true;
    input.disabled = true;
    try {
      await renameRoom(id, next);
      stop();
      renderRoomHead(); // the heading is skipped while editing, so paint the new title now
      btn.focus();
    } catch (err) {
      alert("방 이름을 바꾸지 못했습니다: " + err.message);
      input.focus();
    } finally {
      saving = false;
      input.disabled = false;
    }
  };
  btn.onclick = start;
  // Enter on the title itself also opens the editor (the heading is focusable for keyboard users).
  title.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); start(); } };
  title.ondblclick = start;
  input.onkeydown = (e) => {
    if (e.key === "Enter") { e.preventDefault(); save(); }
    else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); stop(); btn.focus(); }
  };
  // Leaving the field keeps what was typed: a changed name is saved, an unchanged one just closes.
  input.onblur = () => { if (titleEditing && !saving) save(); };
}

export function renderRoomHead() {
  const r = state.room;
  if (!r) return;
  $("#btn-add-participant").disabled = activeParticipants().length >= 32;
  if (!titleEditing) $("#room-title").textContent = r.title;
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
  // Once a plan exists the work head bar takes this place.
  $("#btn-work-plan-head").hidden = r.mode !== "write" || !!state.work;

  $("#rounds").value = r.discussion_rounds || 3;
  $("#btn-export").href = "/api/rooms/" + r.id + "/export.md";

  renderWorkHeadBar();
  renderTargetControls();
  renderUsagePanel();
}
