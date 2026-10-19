// Composer: steering target chips, hints, @mention popup and send.
import { $, announce, el } from "./util.js";
import { api } from "./api.js";
import { activeParticipants, modelLabel, openSeq, participant, participantLabel, state } from "./state.js";
import { encodedMentions } from "./markdown.js";
import { clearComposerError, hasAttachmentError, isSendingMessage, removeDraftAttachmentsByIds, setComposerBusy, showComposerError } from "./attachments.js";

// ---------- steering target controls ----------
export function renderTargetControls() {
  const r = state.room;
  if (!r) return;
  const controlsKey = JSON.stringify([state.target, activeParticipants().map((p) => [p.id, p.label, p.enabled, p.status, p.provider])]);
  if (state.targetControlsKey === controlsKey) return;
  state.targetControlsKey = controlsKey;
  const sel = $("#target");
  if (sel) {
    sel.innerHTML = '<option value="">전체 (@멘션 규칙)</option>';
    for (const p of activeParticipants()) {
      const o = el("option", "", participantLabel(p.id) + "에게만");
      o.value = p.id;
      sel.appendChild(o);
    }
    if (state.target && !activeParticipants().some((p) => p.id === state.target)) {
      state.target = "";
    }
    sel.value = state.target;
  }

  const chipsBox = $("#target-chips");
  if (chipsBox) {
    const focusedId = chipsBox.contains(document.activeElement) ? document.activeElement.dataset.target : null;
    chipsBox.innerHTML = "";
    const allBtn = el("button", "target-chip-btn" + (state.target === "" ? " active" : ""), "전체");
    allBtn.type = "button";
    allBtn.dataset.target = "";
    allBtn.setAttribute("aria-pressed", state.target === "" ? "true" : "false");
    allBtn.title = "특정 대상 지정 없이 @멘션 규칙(또는 전원)으로 전송";
    allBtn.onclick = () => setTarget("");
    chipsBox.appendChild(allBtn);

    for (const p of activeParticipants()) {
      const isAct = state.target === p.id;
      const b = el("button", "target-chip-btn" + (isAct ? " active" : ""), participantLabel(p.id));
      b.type = "button";
      b.dataset.target = p.id;
      b.setAttribute("aria-pressed", isAct ? "true" : "false");
      b.title = participantLabel(p.id) + "에게만 보내기 · " + modelLabel(p.provider, p.model);
      b.onclick = () => setTarget(isAct ? "" : p.id, true);
      chipsBox.appendChild(b);
    }
    if (focusedId !== null) {
      const nextFocus = [...chipsBox.children].find((b) => b.dataset.target === focusedId);
      if (nextFocus) nextFocus.focus();
    }
  }

  const actChip = $("#target-active-chip");
  if (actChip) {
    if (!state.target) {
      actChip.hidden = true;
      actChip.innerHTML = "";
    } else {
      const hadFocus = actChip.contains(document.activeElement);
      const p = r.participants.find((x) => x.id === state.target);
      const info = state.providers.find((x) => x.id === p.provider) || {};
      const modeTxt =
        p && p.status === "running"
          ? info.can_steer
            ? "⚡ 실행 중 즉시 주입"
            : "⏳ 현재 턴 직후 전달"
          : "🎯 1회성 단독 전달";
      actChip.hidden = false;
      actChip.innerHTML = "";
      actChip.appendChild(el("span", "", participantLabel(p.id) + " · " + modeTxt));
      const clr = el("button", "", "× Esc");
      clr.type = "button";
      clr.setAttribute("aria-label", participantLabel(state.target) + " 대상 지정 해제 (Esc)");
      clr.onclick = () => setTarget("", true);
      actChip.appendChild(clr);
      if (hadFocus) clr.focus();
    }
  }
}

export function setTarget(targetId, focusInput) {
  state.target = targetId || "";
  const sel = $("#target");
  if (sel) sel.value = state.target;
  renderTargetControls();
  updateHint();
  if (state.target) {
    announce(participantLabel(state.target) + " 대상으로 지정됨. Esc 키로 해제할 수 있습니다.");
  } else if (focusInput) {
    announce("대상 지정이 해제되어 전체 대상으로 돌아왔습니다.");
  }
  if (focusInput && $("#input")) $("#input").focus();
}

// ---------- composer, mentions, warnings & shortcuts ----------
export function closeMentionPop() {
  const pop = $("#mention-pop");
  const input = $("#input");
  if (pop) pop.hidden = true;
  if (input) input.removeAttribute("aria-activedescendant");
  state.mentionCands = [];
}

function findDisabledMentions(text) {
  if (!state.room || !text) return [];
  const disabled = new Set(activeParticipants().filter((p) => !p.enabled).map((p) => p.id));
  const found = new Set();
  const re = /(?:^|\s)@([a-zA-Z0-9_-]+)/g;
  let m;
  while ((m = re.exec(text)) !== null) {
    const id = m[1].toLowerCase();
    const group = activeParticipants().filter((p) => p.provider === id);
    if (group.length ? group.every((p) => !p.enabled) : disabled.has(id)) found.add(id);
  }
  return Array.from(found);
}

export function updateHint() {
  const hint = $("#composer-hint");
  if (!hint || !state.room) return;
  const text = ($("#input") && $("#input").value) || "";
  const disabledMentions = findDisabledMentions(encodedMentions(text, false));
  const t = state.target;
  const targetParticipant = t ? state.room.participants.find((x) => x.id === t) : null;

  if (targetParticipant && !targetParticipant.enabled) {
    hint.className = "composer-hint warn";
    hint.textContent =
      participantLabel(t) + "의 참여가 꺼져 있습니다. 참가자를 켜거나 대상을 해제하세요.";
    return;
  }
  if (disabledMentions.length > 0) {
    hint.className = "composer-hint warn";
    hint.textContent =
      "⚠ 꺼진 참가자(" +
      disabledMentions.map((id) => participantLabel(id)).join(", ") +
      ")가 멘션되었습니다. 켜지 않으면 해당 에이전트는 답하지 않습니다.";
    return;
  }

  hint.className = "composer-hint muted";
  if (!t) {
    hint.textContent = "이름을 선택하면 한 명에게 · @로 여러 명 선택 · Enter 전송";
    return;
  }
  const info = state.providers.find((x) => x.id === targetParticipant.provider) || {};
  if (targetParticipant && targetParticipant.status === "running") {
    hint.textContent = info.can_steer
      ? participantLabel(t) + " 작업 중 · 다음 메시지를 즉시 전달합니다."
      : participantLabel(t) + " 작업 중 · 현재 답변 뒤에 다음 메시지를 전달합니다.";
  } else {
    hint.textContent =
      participantLabel(t) + "에게만 다음 메시지를 전달합니다. Esc로 해제";
  }
}

// Fix #7: Connect mention candidates to accessibility tree via option IDs & aria-activedescendant
export function mentionPopup() {
  const input = $("#input");
  const pop = $("#mention-pop");
  if (!input || !pop || !state.room) return;
  const before = input.value.slice(0, input.selectionStart);
  const m = before.match(/(?:^|\s)@([^\s@\[\]]*)$/u);
  if (!m) {
    closeMentionPop();
    return;
  }
  const q = m[1].toLowerCase();
  const cands = [
    { id: "all", label: "전체 참가자", search: "all 전체", enabled: true },
    ...state.providers.filter((provider) => activeParticipants().some((p) => p.provider === provider.id)).map((provider) => ({
      id: provider.id,
      label: provider.label + " 전체",
      search: provider.id + " " + provider.label,
      enabled: true,
    })),
    ...activeParticipants().filter((p) => !state.providers.some((provider) => provider.id === p.id)).map((p) => ({
      id: p.id,
      label: participantLabel(p.id) + " · " + modelLabel(p.provider, p.model) + (p.enabled ? "" : " · 꺼짐"),
      search: participantLabel(p.id) + " " + modelLabel(p.provider, p.model),
      enabled: p.enabled,
    })),
  ].filter((item) => item.search.toLowerCase().includes(q));

  if (!cands.length) {
    closeMentionPop();
    return;
  }
  state.mentionCands = cands;
  if (state.mentionIdx >= cands.length) state.mentionIdx = 0;
  pop.hidden = false;
  renderMentionButtons();
}

export function renderMentionButtons() {
  const pop = $("#mention-pop");
  const input = $("#input");
  if (!pop) return;
  pop.innerHTML = "";
  state.mentionCands.forEach((cand, idx) => {
    const optId = "mention-opt-" + idx;
    const isSelected = idx === state.mentionIdx;
    const b = el("button", isSelected ? "active" : "", cand.label);
    b.type = "button";
    b.id = optId;
    b.setAttribute("role", "option");
    b.setAttribute("aria-selected", isSelected ? "true" : "false");
    b.onclick = () => applyMention(cand);
    pop.appendChild(b);
    if (isSelected && input && !pop.hidden) {
      input.setAttribute("aria-activedescendant", optId);
    }
  });
  const selected = pop.querySelector('[aria-selected="true"]');
  if (selected) selected.scrollIntoView({ block: "nearest" });
}

export function applyMention(cand) {
  if (!cand) return;
  clearComposerError();
  const input = $("#input");
  const before = input.value.slice(0, input.selectionStart);
  const after = input.value.slice(input.selectionStart);
  let token = "@" + cand.id;
  if (participant(cand.id) && !state.providers.some((p) => p.id === cand.id)) {
    const name = participantLabel(cand.id).replace(/[\[\]\n]/g, " ");
    token = "@[" + name + "]";
    let suffix = 2;
    while (state.draftMentions.has(token) && state.draftMentions.get(token) !== cand.id) token = "@[" + name + " · " + suffix++ + "]";
    state.draftMentions.set(token, cand.id);
  }
  const replaced = before.replace(/@[^\s@\[\]]*$/u, token + " ");
  input.value = replaced + after;
  input.selectionStart = input.selectionEnd = replaced.length;
  closeMentionPop();
  input.focus();
  updateHint();
}

export async function send() {
  if (isSendingMessage || !state.roomId) return;
  const input = $("#input");
  const rawInput = input.value;
  const draft = rawInput.trim();
  let text;
  try { text = encodedMentions(draft); }
  catch (err) {
    showComposerError(err.message);
    input.focus();
    return;
  }
  if (hasAttachmentError()) {
    showComposerError("오류가 있는 첨부 칩을 제거한 뒤 전송하세요.");
    return;
  }
  clearComposerError();
  const sendingItems = state.draftAttachments.filter(
    (item) => !item.cancelled && (item.status === "uploading" || item.status === "ready")
  );
  if (!text && sendingItems.length === 0) return;

  const sendingIds = new Set(sendingItems.map((item) => item.localId));
  for (const item of sendingItems) item.sending = true;

  const roomId = state.roomId;
  const sequence = openSeq;
  const target = state.target;
  closeMentionPop();
  setComposerBusy(true);

  try {
    const pendingUploads = sendingItems
      .filter((item) => item.status === "uploading" && item.uploadPromise)
      .map((item) => item.uploadPromise);

    if (pendingUploads.length > 0) {
      await Promise.all(pendingUploads);
      if (state.roomId !== roomId || openSeq !== sequence) return;
    }

    if (sendingItems.some((item) => item.status === "error" || !item.uploaded || !item.uploaded.id)) {
      showComposerError("이미지 업로드에 실패했습니다. 오류 칩을 확인하세요.");
      return;
    }

    const attachmentIds = sendingItems.map((item) => item.uploaded.id);
    if (input.value === rawInput) {
      input.value = "";
    }

    const payload = { text, target };
    if (attachmentIds.length > 0) {
      payload.attachments = attachmentIds;
    }
    await api("POST", "/api/rooms/" + roomId + "/messages", payload);
    if (state.roomId !== roomId || openSeq !== sequence) return;
    removeDraftAttachmentsByIds(sendingIds);
    if (!input.value) state.draftMentions.clear();
    if (target) {
      setTarget("", false);
    } else {
      updateHint();
    }
  } catch (e) {
    if (state.roomId !== roomId || openSeq !== sequence) return;
    alert(e.message);
    if (!input.value) {
      input.value = rawInput;
    } else if (input.value !== rawInput) {
      input.value = rawInput + "\n" + input.value;
    }
    updateHint();
  } finally {
    for (const item of sendingItems) item.sending = false;
    setComposerBusy(false);
  }
}
