// Room list in the sidebar (with the per-room context menu) and the open room's header (with inline rename).
import { $, $$, announce, el } from "./util.js";
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
const svgIcon = (paths) =>
  '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' + paths + "</svg>";
const ROOM_MODE = Object.freeze({
  read: { name: "read 모드 (읽기·검색)", icon: svgIcon('<path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/>') },
  chat: { name: "chat 모드 (대화)", icon: svgIcon('<path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/>') },
  write: { name: "write 모드 (파일 수정)", icon: svgIcon('<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4z"/>') },
});

// Which room's context menu (right-click, Shift+F10, long-press) is open and which is
// being renamed inline; both survive re-renders.
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
    const trigger = $('#room-list .room-item[data-room="' + CSS.escape(id) + '"]');
    if (trigger) trigger.focus();
  }
}

function openRoomMenu(id) {
  menuOpenFor = id;
  renderRoomList();
  const first = $("#room-list li[data-menu-open] .room-menu-item");
  if (first) first.focus();
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
    if (e.isComposing) return; // Enter/Esc that only settle an IME composition are not commands
    if (e.key === "Enter") { e.preventDefault(); save(); }
    else if (e.key === "Escape") { e.preventDefault(); e.stopPropagation(); renamingId = ""; endRename(); }
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
  // Same for an open room menu driven by the keyboard: remember which item had focus.
  const menuItems = menuOpenFor ? $$(".room-menu-pop .room-menu-item", ul) : [];
  const menuIdx = menuItems.indexOf(document.activeElement);
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
  if (menuIdx >= 0 && menuOpenFor) {
    const items = $$(".room-menu-pop .room-menu-item", ul);
    const next = items[menuIdx] || $('.room-item[data-room="' + CSS.escape(menuOpenFor) + '"]', ul);
    if (next) next.focus();
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
    // The whole row is the title: the mode is a 14px icon and the room actions open
    // from the context menu (right-click, Shift+F10 or the menu key, long-press).
    const btn = el("button", "room-item");
    btn.type = "button";
    btn.dataset.room = r.id;
    if (isAct) btn.setAttribute("aria-current", "page");
    btn.title = r.title + "\n" + ROOM_MODE[r.mode].name + " · 우클릭: 이름 바꾸기·삭제·내보내기";
    btn.setAttribute("aria-description", ROOM_MODE[r.mode].name + ". Shift+F10으로 이름 바꾸기·삭제·내보내기 메뉴");
    const icon = el("span", "mode-icon " + r.mode);
    icon.innerHTML = ROOM_MODE[r.mode].icon;
    icon.setAttribute("aria-hidden", "true");
    btn.appendChild(icon);
    btn.appendChild(el("span", "name", r.title));
    if (r.active) {
      const dot = el("span", "dot");
      dot.title = "실행 중";
      dot.setAttribute("aria-label", "실행 중");
      btn.appendChild(dot);
    }
    btn.onclick = () => {
      closeDrawers();
      hooks.openRoom(r.id);
    };
    btn.oncontextmenu = (e) => {
      e.preventDefault();
      openRoomMenu(r.id);
    };
    // Not every browser turns Shift+F10 or the menu key into a contextmenu event.
    btn.onkeydown = (e) => {
      if (e.key === "ContextMenu" || (e.shiftKey && e.key === "F10")) {
        e.preventDefault();
        openRoomMenu(r.id);
      }
    };
    li.appendChild(btn);
    if (menuOpenFor === r.id) {
      li.dataset.menuOpen = "true";
      li.appendChild(buildRoomMenu(r));
    }
    ul.appendChild(li);
  }
}

// Clicking anywhere outside closes the open room menu; the global Escape handler calls closeRoomMenu too.
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

// The blind first round runs one independent call per enabled seat, so a question
// that already has answers costs that many calls more. Shown next to the toggle.
export function renderBlindHint() {
  const hint = $("#blind-hint");
  if (!hint) return;
  if (!state.room) {
    hint.hidden = true;
    return;
  }
  const n = activeParticipants().filter((p) => p.enabled).length;
  hint.hidden = false;
  hint.textContent = "시작 전에 이미 답이 있으면 블라인드는 호출 " + (n > 0 ? n + "회" : "참가자 수만큼") + "를 더 씁니다";
}

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
      input.disabled = false; // a disabled input cannot take focus back
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
    if (e.isComposing) return; // Enter/Esc that only settle an IME composition are not commands
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
  $("#chk-blind-first").checked = r.blind_first_default === true;
  renderBlindHint();
  $("#btn-export").href = "/api/rooms/" + r.id + "/export.md";

  renderWorkHeadBar();
  renderTargetControls();
  renderUsagePanel();
}
