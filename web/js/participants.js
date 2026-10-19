// Right panel: participant cards, model catalog, session actions, queue and discussion bar.
import { $, announce, el, esc, fmtElapsed, fmtK } from "./util.js";
import { api } from "./api.js";
import { activeParticipants, modelLabel, openSeq, participant, participantLabel, seatClass, seatTimes, state } from "./state.js";
import { closeDrawers } from "./layout.js";
import { renderProviderStatus, usageGauge } from "./usage.js";
import { setTarget, updateHint } from "./composer.js";
import { renderRoomHead, scheduleRoomList } from "./rooms.js";

export function catalogMessage(info) {
  if (info.catalog_status === "loading") return "모델 목록을 불러오는 중입니다. 잠시 뒤 목록을 다시 불러오세요.";
  if (info.catalog_status === "unavailable") return "모델 목록을 가져오지 못했습니다." + (info.catalog_error ? " " + info.catalog_error : "");
  if (info.catalog_status === "ready" && (!info.models || !info.models.length)) return "사용할 수 있는 모델이 없습니다. OpenCode의 연결 설정을 확인하세요.";
  return "";
}
// ---------- Role presets (GET /api/role-presets) ----------
// Presets and model suggestions are user-edited data on the server; the screen
// only offers them as chips and a default. A load failure is shown, not hidden.
export const rolePresets = { presets: [], suggestions: [], path: "", error: "", loaded: false };
let rolePresetsLoading = null;
export function loadRolePresets() {
  if (rolePresetsLoading) return rolePresetsLoading;
  rolePresetsLoading = api("GET", "/api/role-presets").then((data) => {
    if (!data || !Array.isArray(data.presets)) throw new Error("응답에 presets 배열이 없습니다");
    rolePresets.presets = data.presets;
    rolePresets.suggestions = Array.isArray(data.model_suggestions) ? data.model_suggestions : [];
    rolePresets.path = data.path || "";
    rolePresets.error = "";
    rolePresets.loaded = true;
  }).catch((err) => {
    rolePresets.error = err.message || String(err);
    rolePresets.loaded = true;
  }).finally(() => { rolePresetsLoading = null; });
  return rolePresetsLoading;
}

function globMatches(pattern, value) {
  const re = "^" + String(pattern).split("*").map((part) => part.split("?").map((s) => s.replace(/[.+^${}()|[\]\\]/g, "\\$&")).join(".")).join(".*") + "$";
  return new RegExp(re, "i").test(value);
}

// First suggestion whose model glob matches, as in the contract (top-down, first hit).
export function suggestedPreset(modelId) {
  if (!modelId) return null;
  for (const s of rolePresets.suggestions) {
    if (s && s.match && globMatches(s.match, modelId)) {
      return rolePresets.presets.find((p) => p.id === s.preset) || null;
    }
  }
  return null;
}

// A role editor: preset chips, a free-text directive and the model suggestion.
// `getModel` tells which model the suggestion is for. The caller reads `.value`
// and calls `.syncSuggestion()` whenever the model changes; the suggestion only
// overwrites text the user has not typed.
export function buildRoleField(container, { initial = "", getModel = () => "", onInput = null, autoSuggest = true } = {}) {
  const wrap = el("div", "role-field");
  const chips = el("div", "role-chips");
  chips.setAttribute("role", "group");
  chips.setAttribute("aria-label", "역할 프리셋");
  wrap.appendChild(chips);
  const textarea = el("textarea", "role-input");
  textarea.rows = 2;
  textarea.maxLength = 600;
  textarea.placeholder = "예: 큰 그림과 사용자 경험을 먼저 깊게 본다. 비우면 역할 없음.";
  textarea.setAttribute("aria-label", "역할 지시문");
  textarea.value = initial || "";
  wrap.appendChild(textarea);
  const note = el("p", "role-note small-note muted");
  note.setAttribute("role", "status");
  wrap.appendChild(note);
  container.appendChild(wrap);

  // The suggestion only fills the field while the user has not touched it: typing,
  // clearing or picking a chip is a choice that stays, and it is applied at most once
  // per model so a re-render never overwrites what is there.
  let userEdited = false;
  let suggestedForModel = null;
  let suggestedText = "";
  const field = {
    root: wrap,
    textarea,
    get value() { return textarea.value.trim(); },
    syncSuggestion() { render(); },
  };

  function render() {
    chips.replaceChildren();
    const model = getModel();
    const suggested = suggestedPreset(model);
    const current = textarea.value.trim();
    if (rolePresets.error) {
      note.className = "role-note small-note warn";
      note.textContent = "역할 프리셋을 불러오지 못했습니다: " + rolePresets.error + " 역할은 직접 입력할 수 있습니다.";
    } else if (!rolePresets.loaded) {
      note.className = "role-note small-note muted";
      note.textContent = "역할 프리셋을 불러오는 중…";
    } else if (!rolePresets.presets.length) {
      note.className = "role-note small-note warn";
      note.textContent = "역할 프리셋이 비어 있습니다. " + (rolePresets.path ? rolePresets.path + " 파일에 프리셋을 추가하세요." : "");
    } else if (suggested) {
      const using = current === (suggested.directive || "").trim();
      note.className = "role-note small-note muted";
      note.textContent = "모델 추천: " + suggested.name + (using ? " (적용됨)" : "") + " · 추천은 참고일 뿐이며 자유롭게 고쳐도 됩니다.";
    } else {
      note.className = "role-note small-note muted";
      note.textContent = model ? "이 모델에 대한 추천 역할이 없습니다. 프리셋을 고르거나 직접 적으세요." : "프리셋을 고르거나 직접 적으세요.";
    }
    for (const preset of rolePresets.presets) {
      const chip = el("button", "role-chip", preset.name);
      chip.type = "button";
      const directive = (preset.directive || "").trim();
      chip.setAttribute("aria-pressed", current && current === directive ? "true" : "false");
      chip.title = directive;
      if (suggested && suggested.id === preset.id) {
        chip.classList.add("suggested");
        chip.appendChild(el("span", "role-chip-mark", "추천"));
      }
      chip.onclick = () => {
        textarea.value = directive;
        userEdited = true;
        render();
        if (onInput) onInput();
      };
      chips.appendChild(chip);
    }
    // First sight of this model and the text is empty or the previous model's suggestion.
    if (autoSuggest && !userEdited && suggested && model !== suggestedForModel && (current === "" || current === suggestedText)) {
      suggestedForModel = model;
      suggestedText = (suggested.directive || "").trim();
      if (current !== suggestedText) {
        textarea.value = suggestedText;
        render();
        if (onInput) onInput();
      }
    }
  }
  textarea.addEventListener("input", () => { userEdited = true; render(); if (onInput) onInput(); });
  const ready = loadRolePresets();
  render();
  ready.then(() => { if (wrap.isConnected) render(); });
  return field;
}

let catalogLoading = null;
export async function refreshProviderCatalog(button, afterRefresh) {
  button.disabled = true;
  try {
    if (!catalogLoading) catalogLoading = api("GET", "/api/providers?refresh=true").then((result) => {
      state.providers = result.providers; state.catalogRevision++;
      renderProviderStatus();
    }).finally(() => { catalogLoading = null; });
    await catalogLoading;
    afterRefresh();
    if (state.room) renderParticipants();
  } catch (err) { alert("모델 목록을 불러오지 못했습니다: " + err.message); }
  finally { button.disabled = false; }
}

export function getAgentElapsedMs(pid) {
  for (let i = state.order.length - 1; i >= 0; i--) {
    const m = state.messages.get(state.order[i]);
    if (m && m.kind === "agent" && m.author === pid && m.status === "streaming" && m.ts) {
      return Math.max(0, Date.now() - m.ts);
    }
  }
  const t = state.queue.find((q) => q.agent === pid && q.status === "running");
  if (t && t.created) return Math.max(0, Date.now() - t.created);
  return null;
}

// ---------- Right Panel: In-place Participant Cards (Fix #6 & V5) ----------
export function buildSessionActions(participantId, messageId = "") {
  const actions = el("div", "session-actions");
  const roomId = state.roomId;
  for (const action of ["open", "copy"]) {
    const button = el("button", "ghost", action === "open" ? "CLI에서 열기" : "명령 복사");
    button.type = "button";
    button.dataset.action = action;
    button.onclick = async () => {
      const sequence = openSeq;
      button.disabled = true;
      button.dataset.busy = "true";
      try {
        const url = "/api/rooms/" + roomId + "/participants/" + participantId + "/session";
        const session = await api("GET", url + (messageId ? "?message=" + encodeURIComponent(messageId) : ""));
        if (state.roomId !== roomId || openSeq !== sequence) return;
        if (action === "copy") {
          await navigator.clipboard.writeText(session.command);
          if (state.roomId !== roomId || openSeq !== sequence) return;
          announce("CLI 이어보기 명령을 복사했습니다.");
          button.textContent = "복사됨";
        } else {
          if (!session.can_open) throw new Error("이 환경에서는 CLI 창을 바로 열 수 없습니다. 명령을 복사해서 실행하세요.");
          const result = await api("POST", url + "/open", messageId ? { message_id: messageId } : {});
          if (state.roomId !== roomId || openSeq !== sequence) return;
          announce("기존 대화를 여는 CLI 실행 요청을 전달했습니다." + (result.paused ? " 참가자는 비활성화되었습니다. CLI 창을 닫은 뒤 다시 켜세요." : ""));
          button.textContent = "열기 요청됨";
        }
      } catch (e) {
        if (state.roomId === roomId && openSeq === sequence) alert(e.message);
      }
      finally {
        delete button.dataset.busy;
        button.disabled = actions.dataset.available !== "true";
        setTimeout(() => { button.textContent = action === "open" ? "CLI에서 열기" : "명령 복사"; }, 1800);
      }
    };
    actions.appendChild(button);
  }
  return actions;
}

export function updateSessionActions(actions, sessionId, running) {
  const available = !!sessionId && !running;
  actions.dataset.available = String(available);
  for (const button of actions.children) {
    button.disabled = !available || button.dataset.busy === "true";
    button.title = !sessionId ? "첫 실행에서 CLI 세션이 저장되면 사용할 수 있습니다"
      : running ? "같은 세션의 실행이 끝난 뒤 열 수 있습니다"
      : (button.dataset.action === "open"
        ? "기존 CLI 대화를 엽니다. 이 참가자는 비활성화됩니다. CLI 창을 닫은 뒤 다시 켜세요."
        : "이어보기 명령을 복사합니다. 직접 실행하기 전에 이 참가자를 끄세요.") + " · 세션 " + sessionId;
  }
}

export function getEffortsForModel(info, modelId) {
  if (info && info.model_efforts && Array.isArray(info.model_efforts[modelId])) {
    return info.model_efforts[modelId];
  }
  return info && Array.isArray(info.efforts) ? info.efforts : [];
}

export function fillModelSelect(select, models, selected, query = "", allowCustom = false) {
  const q = query.trim().toLowerCase();
  const filtered = models.filter((m) => !q || (m.label + " " + (m.group || "") + " " + m.id).toLowerCase().includes(q) || m.id === selected);
  select.replaceChildren();
  if (allowCustom && selected === "") {
    const placeholder = el("option", "", "모델을 선택하세요");
    placeholder.value = ""; select.appendChild(placeholder);
  }
  const groups = new Map();
  for (const model of filtered) {
    let parent = select;
    if (model.group) {
      if (!groups.has(model.group)) {
        const group = document.createElement("optgroup"); group.label = model.group;
        groups.set(model.group, group); select.appendChild(group);
      }
      parent = groups.get(model.group);
    }
    const option = el("option", "", model.label);
    option.value = model.id; parent.appendChild(option);
  }
  if (allowCustom) {
    const option = el("option", "", "모델 ID 직접 입력…"); option.value = "__custom__"; select.appendChild(option);
  }
  if ([...select.options].some((o) => o.value === selected)) select.value = selected;
}

export function renderParticipants() {
  const box = $("#participants");
  if (!box || !state.room) return;

  const validIds = new Set(activeParticipants().map((p) => p.id));
  Array.from(box.children).forEach((child) => {
    if (child.dataset && child.dataset.pid && !validIds.has(child.dataset.pid)) {
      child.remove();
    }
  });

  for (const p of activeParticipants()) {
    const info = state.providers.find((x) => x.id === p.provider) || {};
    let card = $("#pcard-" + p.id, box);
    if (!card) {
      card = buildParticipantCardSkeleton(p, info);
      box.appendChild(card);
    }
    updateParticipantCardInPlace(card, p, info);
  }
}

const roleFields = new WeakMap(); // card element -> its role editor
function buildParticipantCardSkeleton(p, info) {
  const card = el("div", "pcard");
  card.id = "pcard-" + p.id;
  card.dataset.pid = p.id;

  const top = el("div", "top");
  top.appendChild(el("span", "name " + p.provider, participantLabel(p.id)));

  const steerBtn = el("button", "ghost steer-btn", "선택");
  steerBtn.type = "button";
  steerBtn.style.padding = "2px 8px";
  steerBtn.style.minHeight = "24px";
  steerBtn.style.fontSize = "11px";
  steerBtn.title = participantLabel(p.id) + "에게만 다음 메시지 보내기";
  steerBtn.onclick = () => {
    closeDrawers();
    setTarget(p.id, true);
  };
  top.appendChild(steerBtn);

  const sw = el("button", "switch");
  sw.type = "button";
  sw.setAttribute("role", "switch");
  sw.setAttribute("aria-label", p.label + " 참여 켜기/끄기");
  sw.onclick = () => {
    const cur = state.room && state.room.participants.find((x) => x.id === p.id);
    if (cur) patchParticipant(p.id, { enabled: !cur.enabled });
  };
  top.appendChild(sw);
  card.appendChild(top);
  card.appendChild(el("div", "participant-model muted"));
  const roleLine = el("div", "participant-role");
  card.appendChild(roleLine);

  const edit = el("details", "participant-edit");
  edit.appendChild(el("summary", "muted", "설정 · 상세"));
  edit.ontoggle = () => {
    const current = participant(p.id);
    if (edit.open && current) { delete card.dataset.signature; updateParticipantCardInPlace(card, current, info); }
  };
  const nameLabel = el("label", "", "참가자 이름");
  const nameInput = el("input", "participant-name");
  nameInput.maxLength = 80;
  nameInput.value = p.label;
  nameLabel.appendChild(nameInput);
  edit.appendChild(nameLabel);
  const rename = el("button", "", "이름 저장");
  rename.type = "button";
  rename.onclick = () => {
    if (!nameInput.value.trim()) return nameInput.focus();
    patchParticipant(p.id, { label: nameInput.value.trim() });
  };
  edit.appendChild(rename);

  // Role: chips + free text; saved with its own button so a half-typed directive is not sent.
  const roleLabel = el("div", "role-label", "역할 (이 관점을 먼저 깊게 본다)");
  edit.appendChild(roleLabel);
  const roleField = buildRoleField(edit, {
    initial: p.role || "",
    getModel: () => { const cur = participant(p.id); return cur ? cur.model : p.model; },
    autoSuggest: false,
  });
  roleField.textarea.setAttribute("aria-label", participantLabel(p.id) + " 역할 지시문");
  roleFields.set(card, roleField);
  const roleSave = el("button", "save-role", "역할 저장");
  roleSave.type = "button";
  roleSave.onclick = () => patchParticipant(p.id, { role: roleField.value });
  edit.appendChild(roleSave);
  edit.appendChild(el("p", "small-note muted", "역할을 바꿔도 세션은 유지됩니다. 다음 턴부터 새 역할이 안내문에 들어갑니다."));
  const customLabel = el("label", "", "모델 ID 직접 입력");
  const custom = el("input", "custom-model");
  custom.placeholder = "CLI가 지원하는 정확한 모델 ID";
  custom.value = p.model;
  customLabel.appendChild(custom);
  edit.appendChild(customLabel);
  edit.appendChild(el("p", "small-note muted", "목록에 없는 모델은 CLI가 지원하는 ID를 입력하세요. 지원 여부는 실행 시 확인됩니다."));
  const apply = el("button", "apply-custom-model", "모델 적용");
  apply.type = "button";
  apply.onclick = () => {
    const model = custom.value.trim();
    if (!model) return custom.focus();
    const efforts = getEffortsForModel(info, model);
    const cur = participant(p.id);
    patchParticipant(p.id, { model, effort: efforts.includes(cur.effort) ? cur.effort : "" });
  };
  edit.appendChild(apply);
  const remove = el("button", "danger ghost remove-participant", "참가자 제거");
  remove.type = "button";
  remove.onclick = async () => {
    if (!confirm(participantLabel(p.id) + " 참가자를 제거할까요? 이전 대화와 사용량은 보존됩니다.")) return;
    const roomId = state.roomId;
    const sequence = openSeq;
    try {
      const room = await api("DELETE", "/api/rooms/" + roomId + "/participants/" + p.id);
      if (state.roomId !== roomId || openSeq !== sequence) return;
      state.room = room;
      renderRoomHead();
      renderParticipants();
      updateHint();
    } catch (e) {
      if (state.roomId === roomId && openSeq === sequence) alert(e.message);
    }
  };
  edit.appendChild(remove);
  card.appendChild(edit);

  const unavailSlot = el("div", "unavail-slot");
  card.appendChild(unavailSlot);

  const selRow = el("div", "sel");
  edit.appendChild(el("p", "catalog-note small-note warn"));
  const catalogRefresh = el("button", "catalog-refresh", p.provider === "opencode" ? "연결·모델 새로고침" : "모델 목록 새로고침");
  catalogRefresh.type = "button";
  catalogRefresh.onclick = () => refreshProviderCatalog(catalogRefresh, () => {});
  edit.appendChild(catalogRefresh);
  const filterLabel = el("label", "model-filter-label", "모델 검색");
  const filter = el("input", "model-filter");
  filter.type = "search"; filter.placeholder = "공급사 또는 모델 이름";
  filterLabel.appendChild(filter); edit.appendChild(filterLabel);
  const ms = el("select", "model-sel");
  ms.title = "모델 선택";
  ms.setAttribute("aria-label", p.label + " 모델");
  selRow.appendChild(ms);

  const es = el("select", "effort-sel");
  es.title = "추론 노력 선택";
  es.setAttribute("aria-label", p.label + " 추론 노력");
  selRow.appendChild(es);
  edit.appendChild(selRow);
  filter.oninput = () => { delete card.dataset.signature; updateParticipantCardInPlace(card, participant(p.id), info); };
  edit.appendChild(el("div", "small-note muted", "모델·추론 수준 변경 시 새 대화 세션으로 시작합니다."));
  edit.appendChild(buildSessionActions(p.id));
  edit.appendChild(el("div", "small-note muted", "CLI 창을 닫은 뒤 참가자를 다시 켜세요. 복사한 명령은 참가자를 끈 뒤 실행하세요."));
  edit.appendChild(el("div", "participant-id muted mono", "참가자 ID: " + p.id));

  es.onchange = () => patchParticipant(p.id, { effort: es.value });
  ms.onchange = () => {
    const curInfo = state.providers.find((x) => x.id === p.provider) || info;
    const curPart = (state.room && state.room.participants.find((x) => x.id === p.id)) || p;
    const newModel = ms.value;
    const curEff = es.hidden ? curPart.effort : es.value;
    const effList = getEffortsForModel(curInfo, newModel);
    syncEffortSelect(es, effList, curEff);
    const selected = effList.length ? es.value : "";
    patchParticipant(p.id, { model: newModel, effort: selected });
  };

  const stLine = el("div", "status-line");
  card.appendChild(stLine);
  card.appendChild(el("div", "resp-times muted"));
  const stop = el("button", "danger stop-participant", "작업 중단");
  stop.type = "button";
  stop.onclick = async () => {
    const roomId = state.roomId;
    const sequence = openSeq;
    const roomStateRevision = state.roomStateRevision;
    stop.dataset.pending = "true";
    stop.disabled = true;
    stop.textContent = "중단 요청 중…";
    try {
      const result = await api("POST", "/api/rooms/" + roomId + "/participants/" + p.id + "/stop", {});
      if (state.roomId !== roomId || openSeq !== sequence) return;
      stop.dataset.requested = "true";
      if (state.roomStateRevision === roomStateRevision) state.room = result.room;
      renderRoomHead(); renderParticipants(); updateHint(); scheduleRoomList();
      announce(participantLabel(p.id) + "의 작업 중단을 요청하고 참여를 껐습니다. 다시 참여하려면 참가자를 켜세요.");
    } catch (err) {
      if (state.roomId === roomId && openSeq === sequence) alert("작업을 중단하지 못했습니다: " + err.message);
    } finally {
      delete stop.dataset.pending;
      if (state.roomId === roomId && openSeq === sequence) {
        delete card.dataset.signature;
        const current = participant(p.id);
        if (current) updateParticipantCardInPlace(card, current, state.providers.find((provider) => provider.id === current.provider) || {});
      }
    }
  };
  card.appendChild(stop);

  const stats = el("div", "stats");
  edit.appendChild(stats);
  card.appendChild(edit);

  return card;
}

export function syncEffortSelect(es, effList, currentEffort) {
  if (!effList.length) {
    es.hidden = true;
    es.dataset.effKey = "";
    es.innerHTML = "";
    return "";
  }
  es.hidden = false;
  const key = effList.join("|");
  if (es.dataset.effKey !== key) {
    es.dataset.effKey = key;
    es.innerHTML = "";
    const automatic = el("option", "", "CLI 기본값");
    automatic.value = "";
    es.appendChild(automatic);
    for (const e of effList) {
      const o = el("option", "", e);
      o.value = e;
      es.appendChild(o);
    }
  }
  const selected = effList.includes(currentEffort) ? currentEffort : "";
  if (document.activeElement !== es && es.value !== selected) {
    es.value = selected;
  } else if (es.value !== "" && !effList.includes(es.value)) {
    es.value = selected;
  }
  return es.value;
}

function updateParticipantCardInPlace(card, p, info) {
  const signature = JSON.stringify([p, participantLabel(p.id), info.available, info.catalog_status, info.catalog_error, state.catalogRevision, seatTimes(p.id), seatClass(p.id)]);
  if (card.dataset.signature === signature) return;
  card.dataset.signature = signature;
  card.className = "pcard" + seatClass(p.id) + (p.enabled ? "" : " off");
  card.querySelector(".name").textContent = participantLabel(p.id);
  card.querySelector(".participant-model").textContent = modelLabel(p.provider, p.model) + (p.effort ? " · " + p.effort : "");
  const catalogNote = card.querySelector(".catalog-note");
  catalogNote.textContent = catalogMessage(info);
  catalogNote.hidden = !catalogNote.textContent;
  card.querySelector(".catalog-refresh").hidden = p.provider !== "opencode" && !catalogNote.textContent;
  const nameInput = card.querySelector(".participant-name");
  const custom = card.querySelector(".custom-model");
  const roleInput = card.querySelector(".role-input");
  if (!card.querySelector(".participant-edit").open) {
    nameInput.value = p.label;
    custom.value = p.model;
    roleInput.value = p.role || "";
  }
  const roleField = roleFields.get(card);
  if (roleField) roleField.syncSuggestion();
  const roleLine = card.querySelector(".participant-role");
  roleLine.textContent = p.role ? "🎯 " + p.role : "역할 없음";
  roleLine.title = p.role ? "역할: " + p.role : "설정 · 상세에서 역할을 정할 수 있습니다";
  roleLine.classList.toggle("muted", !p.role);
  for (const control of card.querySelectorAll(".model-sel, .effort-sel, .custom-model, .apply-custom-model, .remove-participant")) {
    control.disabled = p.status === "running";
    control.title = p.status === "running" ? "현재 턴을 중지하거나 완료한 뒤 변경하세요" : "";
  }
  card.querySelector(".model-sel").setAttribute("aria-label", participantLabel(p.id) + " 모델");
  card.querySelector(".effort-sel").setAttribute("aria-label", participantLabel(p.id) + " 추론 노력");
  updateSessionActions(card.querySelector(".session-actions"), p.session_id, p.status === "running");
  const stop = card.querySelector(".stop-participant");
  const hasWork = p.status === "running" || p.status === "queued";
  if (!hasWork || p.enabled) delete stop.dataset.requested;
  stop.hidden = !hasWork;
  stop.setAttribute("aria-label", participantLabel(p.id) + " 작업 중단");
  stop.title = "이 참가자의 현재 작업과 대기열을 중단하고 참여를 끕니다.";
  stop.disabled = stop.dataset.pending === "true" || stop.dataset.requested === "true";
  stop.textContent = stop.dataset.pending === "true" ? "중단 요청 중…" : stop.dataset.requested === "true" ? "중단 요청됨 · 참여 꺼짐" : "작업 중단";

  const sw = card.querySelector(".switch");
  if (sw) {
    sw.className = "switch" + (p.enabled ? " on" : "");
    sw.setAttribute("aria-checked", p.enabled ? "true" : "false");
    sw.setAttribute("aria-label", participantLabel(p.id) + " 참여 켜기/끄기");
    sw.title = p.enabled ? "참여 중 (클릭하여 끄기)" : "꺼짐 (클릭하여 켜기)";
  }

  const unavailSlot = card.querySelector(".unavail-slot");
  if (unavailSlot) {
    if (info.available === false) {
      unavailSlot.innerHTML = '<div class="unavailable">⚠ CLI를 찾지 못함 (' + esc(info.command || p.id) + ")</div>";
    } else if (unavailSlot.hasChildNodes()) {
      unavailSlot.innerHTML = "";
    }
  }

  const ms = card.querySelector(".model-sel");
  if (ms) {
    const models = [...(info.models || [])];
    if (p.model && !models.some((m) => m.id === p.model)) {
      models.push({ id: p.model, label: "직접 지정 모델" });
    }
    const filter = card.querySelector(".model-filter");
    card.querySelector(".model-filter-label").hidden = models.length < 30;
    const mKey = JSON.stringify([models, filter.value]);
    if (ms.dataset.mKey !== mKey) {
      ms.dataset.mKey = mKey;
      fillModelSelect(ms, models, p.model, filter.value);
    }
    if (document.activeElement !== ms && ms.value !== p.model) {
      ms.value = p.model;
    }
  }

  const es = card.querySelector(".effort-sel");
  if (es) {
    const effList = getEffortsForModel(info, p.model);
    syncEffortSelect(es, effList, p.effort);
  }

  const rt = card.querySelector(".resp-times");
  if (rt) {
    const t = seatTimes(p.id);
    rt.textContent = t ? "응답 시간 · 최근 " + fmtElapsed(t.last) + " · 평균 " + fmtElapsed(t.avg) + " (" + t.n + "회)" : "응답 시간 · 아직 완료한 답변 없음";
  }
  const stLine = card.querySelector(".status-line");
  if (stLine) {
    stLine.className = "status-line " + (p.status || "idle");
    stLine.innerHTML = "";
    const stMain = el("span", "status-main");
    if (p.status === "running") {
      stMain.appendChild(el("span", "spin"));
      stMain.appendChild(el("span", "", "실행 중 · " + (p.status_text || "생각 중")));
      stLine.appendChild(stMain);
      const elapMs = getAgentElapsedMs(p.id);
      const elapSpan = el("span", "elapsed", "⏱ " + fmtElapsed(elapMs || 0));
      elapSpan.setAttribute("data-agent-elapsed", p.id);
      stLine.appendChild(elapSpan);
    } else if (p.status === "queued") {
      stMain.textContent = "⏳ 대기열에서 차례 대기 중";
      stLine.appendChild(stMain);
    } else {
      stMain.textContent = p.enabled ? "대기 중" : "참여 꺼짐";
      stLine.appendChild(stMain);
    }
  }

  const t = p.totals || {};
  const stats = card.querySelector(".stats");
  if (stats && card.querySelector(".participant-edit").open) {
    stats.innerHTML = "";
    const addStatRow = (k, v) => {
      const r = el("div", "stat-row");
      r.appendChild(el("span", "", k));
      r.appendChild(el("b", "", v));
      stats.appendChild(r);
    };
    if (p.context_window > 0) {
      const pct = (p.context_tokens / p.context_window) * 100;
      addStatRow(
        "컨텍스트",
        fmtK(p.context_tokens) + " / " + fmtK(p.context_window) + " (" + pct.toFixed(0) + "%)"
      );
      stats.appendChild(usageGauge(pct, false));
    }
    addStatRow(
      "이 방 누적",
      "입력 " + fmtK(t.input) + " · 출력 " + fmtK(t.output) + " · 캐시 " + fmtK(t.cached)
    );
    if (t.cost_usd) addStatRow("누적 비용", "$" + t.cost_usd.toFixed(3));
    addStatRow(
      "완료 턴",
      (p.turns || 0) + "턴" + (p.session_id ? " · 세션 " + p.session_id.slice(0, 8) + "…" : "")
    );
  }

}

async function patchParticipant(id, patch) {
  if (!state.roomId) return;
  try {
    await api("PATCH", "/api/rooms/" + state.roomId, { participants: [{ id, ...patch }] });
  } catch (e) {
    alert(e.message);
  }
}

export function renderQueue() {
  const ul = $("#queue");
  const cnt = $("#queue-count");
  if (cnt) cnt.textContent = String(state.queue.length);
  if (!ul) return;
  ul.innerHTML = "";
  if (!state.queue.length) {
    ul.appendChild(el("li", "muted", "대기 중인 턴 없음"));
    return;
  }
  for (const t of state.queue) {
    const li = el("li", t.status);
    const reasonTxt =
      {
        mention: "@호출",
        request: "요청",
        broadcast: "전체",
        discussion: "토론 " + t.round + "R",
        part: "파트" + (t.part ? " " + t.part : ""),
        review: "리뷰" + (t.part ? " " + t.part : ""),
      }[t.reason] || t.reason;
    li.appendChild(
      el("span", "", (t.status === "running" ? "실행 · " : "대기 · ") + participantLabel(t.agent) + " · " + reasonTxt)
    );
    const b = el("button", t.status === "running" ? "danger" : "", t.status === "running" ? "중단" : "취소");
    b.type = "button";
    b.setAttribute("aria-label", participantLabel(t.agent) + " 턴 " + (t.status === "running" ? "중단" : "취소"));
    b.onclick = () =>
      api("DELETE", "/api/rooms/" + state.roomId + "/turns/" + t.id).catch((e) => alert(e.message));
    li.appendChild(b);
    ul.appendChild(li);
  }
}

export function renderDiscussion() {
  const bar = $("#discussion-bar");
  const btn = $("#btn-discuss");
  if (!bar || !btn) return;
  const d = state.discussion;
  if (!d) {
    bar.hidden = true;
    btn.disabled = false;
    return;
  }
  bar.hidden = false;
  btn.disabled = true;
  bar.innerHTML = "";
  bar.appendChild(el("b", "", "🗣 토론 진행 중 · " + d.round + " / " + d.total + " 라운드"));
  const sigWrap = el("div", "disc-signals");
  const entries = Object.entries(d.signals || {});
  if (!entries.length) {
    sigWrap.appendChild(el("span", "muted", "발언 및 신호 대기 중…"));
  } else {
    for (const [k, v] of entries) {
      sigWrap.appendChild(el("span", "signal " + v, participantLabel(k) + ": " + v));
    }
  }
  bar.appendChild(sigWrap);
}
