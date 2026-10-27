// Dialogs: new room / room settings with seat specs, and the work plan form.
import { $, $$, announce, el, LABEL } from "./util.js";
import { api } from "./api.js";
import { activeParticipants, openSeq, participantLabel, state } from "./state.js";
import { updateHint } from "./composer.js";
import { renderWork } from "./work.js";
import { loadRooms, renderRoomHead } from "./rooms.js";
import { buildRoleField, catalogMessage, fillModelSelect, getEffortsForModel, refreshProviderCatalog, syncEffortSelect } from "./participants.js";
import { openRoom } from "./room.js";

// ---------- Work Plan Dialog (POST /api/rooms/{id}/work) ----------
function syncReviewerSelect(seatSel, revSel, preferredReviewer = "") {
  const enabledSeats = activeParticipants().filter((p) => p.enabled);
  const author = enabledSeats.find((p) => p.id === seatSel.value);
  const prevValue = preferredReviewer || revSel.value;
  const validReviewers = author ? enabledSeats.filter((p) => p.provider !== author.provider) : [];
  revSel.replaceChildren();
  if (!validReviewers.length) {
    const opt = el("option", "", "다른 공급사 활성 좌석 없음");
    opt.value = "";
    revSel.appendChild(opt);
    revSel.disabled = true;
    return;
  }
  revSel.disabled = false;
  for (const r of validReviewers) {
    const opt = el(
      "option",
      "",
      participantLabel(r.id) + " (" + (LABEL[r.provider] || r.provider) + ")"
    );
    opt.value = r.id;
    revSel.appendChild(opt);
  }
  if (validReviewers.some((r) => r.id === prevValue)) {
    revSel.value = prevValue;
  } else {
    revSel.value = validReviewers[0].id;
  }
}

export function addVerifyRow(container, initial = null) {
  const row = el("div", "work-verify-row");
  const nameLabel = el("label", "", "이름");
  const nameInput = el("input", "verify-name");
  nameInput.placeholder = "예: test";
  if (initial && initial.name) nameInput.value = initial.name;
  nameLabel.appendChild(nameInput);
  row.appendChild(nameLabel);

  const argvLabel = el("label", "verify-argv-label", "실행 인자 (argv, 한 줄에 인자 1개)");
  const argvInput = el("textarea", "verify-argv mono");
  argvInput.rows = 3;
  argvInput.placeholder = "예:\ngo\ntest\n./...";
  if (initial && Array.isArray(initial.argv)) argvInput.value = initial.argv.join("\n");
  argvLabel.appendChild(argvInput);
  row.appendChild(argvLabel);

  const timeoutLabel = el("label", "", "제한 시간(초)");
  const timeoutInput = el("input", "verify-timeout");
  timeoutInput.type = "number";
  timeoutInput.min = "1";
  timeoutInput.value = initial && initial.timeout_sec ? String(initial.timeout_sec) : "600";
  timeoutLabel.appendChild(timeoutInput);
  row.appendChild(timeoutLabel);

  const removeBtn = el("button", "ghost remove-row-btn", "삭제");
  removeBtn.type = "button";
  removeBtn.onclick = () => row.remove();
  row.appendChild(removeBtn);

  container.appendChild(row);
}

export function addPartRow(container, removable = true) {
  const row = el("div", "work-part-row");
  const enabledSeats = activeParticipants().filter((p) => p.enabled);

  const titleLabel = el("label", "", "파트 제목");
  const titleInput = el("input", "part-title-input");
  titleInput.required = true;
  titleInput.placeholder = "예: P1 리뷰 프로토콜";
  titleLabel.appendChild(titleInput);
  row.appendChild(titleLabel);

  const scopeLabel = el("label", "", "범위 경로 (쉼표 구분)");
  const scopeInput = el("input", "part-scope-input mono");
  scopeInput.required = true;
  scopeInput.placeholder = "예: internal/room/review.go, internal/room/review_test.go";
  scopeLabel.appendChild(scopeInput);
  row.appendChild(scopeLabel);

  const briefLabel = el("label", "part-brief-label", "완료 조건 (Brief)");
  const briefInput = el("input", "part-brief-input");
  briefInput.required = true;
  briefInput.placeholder = "예: review_types.go 메서드 및 단위 테스트 통과";
  briefLabel.appendChild(briefInput);
  row.appendChild(briefLabel);

  const seatLabel = el("label", "", "담당 좌석");
  const seatSel = el("select", "part-seat-select");
  seatSel.required = true;
  if (!enabledSeats.length) {
    const opt = el("option", "", "활성 좌석 없음");
    opt.value = "";
    seatSel.appendChild(opt);
    seatSel.disabled = true;
  } else {
    for (const s of enabledSeats) {
      const opt = el(
        "option",
        "",
        participantLabel(s.id) + " (" + (LABEL[s.provider] || s.provider) + ")"
      );
      opt.value = s.id;
      seatSel.appendChild(opt);
    }
  }
  seatLabel.appendChild(seatSel);
  row.appendChild(seatLabel);

  const revLabel = el("label", "", "리뷰어 좌석 (다른 공급사)");
  const revSel = el("select", "part-reviewer-select");
  revSel.required = true;
  revLabel.appendChild(revSel);
  row.appendChild(revLabel);

  seatSel.onchange = () => syncReviewerSelect(seatSel, revSel);
  syncReviewerSelect(seatSel, revSel);

  if (removable) {
    const removeBtn = el("button", "ghost remove-row-btn", "파트 삭제");
    removeBtn.type = "button";
    removeBtn.onclick = () => row.remove();
    row.appendChild(removeBtn);
  }

  container.appendChild(row);
}

export function openWorkPlanDialog() {
  if (!state.room || state.room.mode !== "write") return;
  const dlg = $("#dlg-work");
  const f = $("#form-work");
  const errBox = $("#work-form-error");
  if (!dlg || !f) return;
  errBox.hidden = true;
  errBox.textContent = "";
  f.goal.value = "";
  f.merge_mode.value = "confirm";
  f.review_rounds.value = "3";

  const verifyBox = $("#work-verify-rows");
  verifyBox.replaceChildren();
  addVerifyRow(verifyBox);

  const partBox = $("#work-part-rows");
  partBox.replaceChildren();
  addPartRow(partBox, false);

  dlg.showModal();
}

export async function onWorkPlanSubmit(e) {
  e.preventDefault();
  if (!state.roomId) return;
  const f = e.target;
  const errBox = $("#work-form-error");
  const submitBtn = $("#dlg-work-ok");
  errBox.hidden = true;
  errBox.textContent = "";

  const showErr = (msg) => {
    errBox.hidden = false;
    errBox.textContent = msg;
  };

  const goal = f.goal.value.trim();
  if (!goal) return showErr("목표(Goal)를 입력하세요.");
  const merge_mode = f.merge_mode.value;
  const review_rounds = Number(f.review_rounds.value);
  if (!Number.isInteger(review_rounds) || review_rounds < 1) {
    return showErr("리뷰 라운드 상한은 1 이상의 정수여야 합니다.");
  }

  const verify = [];
  for (const row of $$(".work-verify-row", $("#work-verify-rows"))) {
    const name = $(".verify-name", row).value.trim();
    const argvRaw = $(".verify-argv", row).value.replace(/\r\n/g, "\n");
    const timeout_sec = Number($(".verify-timeout", row).value);
    if (!name && argvRaw === "") continue;
    if (!name || argvRaw === "") {
      return showErr("검증 명령의 이름과 실행 인자(argv)를 모두 입력하세요.");
    }
    const argv = argvRaw.split("\n");
    if (!argv[0].trim()) {
      return showErr("검증 명령의 첫 번째 인자(실행 파일)는 비워둘 수 없습니다.");
    }
    if (!Number.isFinite(timeout_sec) || timeout_sec < 1) {
      return showErr("검증 명령 제한 시간은 1초 이상이어야 합니다.");
    }
    verify.push({ name, argv, timeout_sec: Math.floor(timeout_sec) });
  }

  const partRows = $$(".work-part-row", $("#work-part-rows"));
  if (!partRows.length) {
    return showErr("최소 1개 이상의 파트를 추가하세요.");
  }

  const parts = [];
  const enabledSeats = activeParticipants().filter((p) => p.enabled);
  for (const row of partRows) {
    const title = $(".part-title-input", row).value.trim();
    const brief = $(".part-brief-input", row).value.trim();
    const scopeRaw = $(".part-scope-input", row).value.trim();
    const seat = $(".part-seat-select", row).value;
    const reviewer = $(".part-reviewer-select", row).value;
    if (!title || !brief) {
      return showErr("모든 파트의 제목과 완료 조건을 입력하세요.");
    }
    const scope = scopeRaw
      .split(/[\n,]+/)
      .map((s) => s.trim())
      .filter(Boolean);
    if (!scope.length) {
      return showErr("파트 '" + title + "'의 범위 경로(scope)를 1개 이상 입력하세요.");
    }
    const authorSeat = enabledSeats.find((p) => p.id === seat);
    const reviewerSeat = enabledSeats.find((p) => p.id === reviewer);
    if (!authorSeat || !reviewerSeat) {
      return showErr("파트 '" + title + "'의 담당 좌석과 리뷰어 좌석을 선택하세요.");
    }
    if (authorSeat.provider === reviewerSeat.provider) {
      return showErr("파트 '" + title + "'의 리뷰어는 담당과 다른 공급사 좌석이어야 합니다.");
    }
    parts.push({ title, brief, scope, seat, reviewer });
  }

  const roomId = state.roomId;
  const sequence = openSeq;
  const workRev = state.workRevision;
  // Creating the worktrees takes seconds; lock the dialog so the plan is not sent twice.
  const cancelBtn = $("#dlg-work-cancel");
  f.setAttribute("aria-busy", "true");
  submitBtn.disabled = true;
  cancelBtn.disabled = true;
  submitBtn.textContent = "worktree 만드는 중…";
  try {
    const plan = await api("POST", "/api/rooms/" + roomId + "/work", {
      goal,
      merge_mode,
      review_rounds,
      verify,
      parts,
    });
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if (state.workRevision === workRev) {
      state.workRevision++;
      state.work = plan;
    }
    state.workBoardCollapsed = false;
    $("#dlg-work").close();
    renderRoomHead();
    renderWork();
    announce("작업 계획이 확정되었습니다.");
  } catch (err) {
    if (state.roomId === roomId && openSeq === sequence) {
      showErr(err.message);
    }
  } finally {
    f.removeAttribute("aria-busy");
    submitBtn.disabled = false;
    cancelBtn.disabled = false;
    submitBtn.textContent = "계획 확정";
  }
}

// ---------- room dialog ----------
export function addSeatSpec(box, providerId = "", removable = true, seed = null) {
  const row = el("div", "seat-spec");
  function field(text, control) {
    const label = el("label", "", text);
    label.appendChild(control);
    row.appendChild(label);
    return control;
  }
  const provider = field("공급사", el("select", "seat-provider"));
  provider.required = true;
  const providerPlaceholder = el("option", "", "이용 중인 공급사 선택");
  providerPlaceholder.value = "";
  provider.appendChild(providerPlaceholder);
  for (const info of state.providers) {
    const option = el("option", "", info.label + (info.available === false ? " (CLI 없음)" : ""));
    option.value = info.id;
    option.disabled = info.available === false;
    provider.appendChild(option);
  }
  provider.value = providerId;
  const catalogNote = el("p", "catalog-note small-note warn");
  catalogNote.setAttribute("role", "status");
  row.appendChild(catalogNote);
  const catalogRefresh = el("button", "catalog-refresh", "모델 목록 새로고침");
  catalogRefresh.type = "button";
  catalogRefresh.onclick = () => refreshProviderCatalog(catalogRefresh, () => provider.onchange());
  row.appendChild(catalogRefresh);
  const filter = field("모델 검색", el("input", "seat-model-filter"));
  filter.type = "search"; filter.placeholder = "공급사 또는 모델 이름";
  const model = field("모델", el("select", "seat-model"));
  model.required = true;
  const custom = field("직접 입력할 모델 ID", el("input", "seat-custom-model"));
  custom.placeholder = "CLI가 지원하는 정확한 모델 ID";
  const effort = field("추론 수준", el("select", "seat-effort"));
  const count = field("인원", el("input", "seat-count"));
  count.type = "number";
  count.min = "1";
  count.max = "32";
  count.value = "1";
  count.required = true;
  const label = field("이름 (선택)", el("input", "seat-label"));
  label.maxLength = 80;
  label.placeholder = "예: 설계 검토자";
  const roleBox = el("div", "seat-role");
  roleBox.appendChild(el("div", "role-label", "역할 (선택 · 이 관점을 먼저 깊게 본다)"));
  row.appendChild(roleBox);
  const selectedModel = () => (model.value === "__custom__" ? custom.value.trim() : model.value);
  // A copied seat keeps its own (possibly empty) role; only a fresh seat gets the model suggestion.
  const role = buildRoleField(roleBox, { initial: seed ? seed.role || "" : "", getModel: selectedModel, autoSuggest: !seed });
  const syncEffort = () => {
    const info = state.providers.find((p) => p.id === provider.value);
    const selected = selectedModel();
    const efforts = selected ? getEffortsForModel(info, selected) : [];
    syncEffortSelect(effort, efforts, info.default_effort);
    effort.parentElement.hidden = !efforts.length;
    role.syncSuggestion();
  };
  const syncModel = () => {
    custom.parentElement.hidden = model.value !== "__custom__";
    custom.required = model.value === "__custom__";
    syncEffort();
  };
  provider.onchange = () => {
    model.disabled = !provider.value;
    effort.disabled = !provider.value;
    custom.disabled = !provider.value;
    if (!provider.value) {
      const modelPlaceholder = el("option", "", "공급사를 먼저 선택하세요");
      modelPlaceholder.value = ""; model.replaceChildren(modelPlaceholder);
      const effortPlaceholder = el("option", "", "공급사 선택 후 설정");
      effortPlaceholder.value = ""; effort.replaceChildren(effortPlaceholder);
      effort.hidden = false; effort.parentElement.hidden = false;
      delete effort.dataset.effKey;
      custom.required = false; custom.parentElement.hidden = true;
      filter.parentElement.hidden = true;
      catalogNote.hidden = true; catalogRefresh.hidden = true;
      return;
    }
    const info = state.providers.find((p) => p.id === provider.value);
    catalogNote.textContent = catalogMessage(info);
    catalogNote.hidden = !catalogNote.textContent;
    catalogRefresh.textContent = provider.value === "opencode" ? "연결·모델 새로고침" : "모델 목록 새로고침";
    catalogRefresh.hidden = provider.value !== "opencode" && !catalogNote.textContent;
    filter.value = "";
    filter.parentElement.hidden = !info.models || info.models.length < 30;
    fillModelSelect(model, info.models || [], info.default_model, "", true);
    syncModel();
  };
  filter.oninput = () => {
    const info = state.providers.find((p) => p.id === provider.value);
    fillModelSelect(model, info.models || [], model.value, filter.value, true);
  };
  model.onchange = syncModel;
  custom.oninput = syncEffort;
  provider.onchange();
  if (seed) {
    const known = [...model.options].some((o) => o.value === seed.model);
    model.value = known ? seed.model : "__custom__";
    custom.value = known ? "" : seed.model;
    syncModel();
    effort.value = seed.effort;
    label.value = seed.label;
  }
  if (removable) {
    const remove = el("button", "ghost", "구성 제거");
    remove.type = "button";
    remove.onclick = () => row.remove();
    row.appendChild(remove);
  }
  box.appendChild(row);
}

export function readSeatSpecs(box) {
  return $$(".seat-spec", box).map((row) => ({
    provider: $(".seat-provider", row).value,
    model: $(".seat-model", row).value === "__custom__" ? $(".seat-custom-model", row).value.trim() : $(".seat-model", row).value,
    effort: $(".seat-effort", row).hidden ? "" : $(".seat-effort", row).value,
    label: $(".seat-label", row).value.trim(),
    role: $(".role-input", row).value.trim(),
    count: Number($(".seat-count", row).value),
  }));
}

let pendingInitialPrompt = "";
export function openRoomDialog(room, initialPrompt = "") {
  pendingInitialPrompt = room ? "" : initialPrompt;
  const f = $("#form-room");
  $("#dlg-room-title").textContent = room ? "방 설정" : "새 방";
  $("#dlg-room-ok").textContent = room ? "저장" : "만들기";
  f.title.value = room ? room.title : "";
  f.workspace.value = room ? room.workspace : state.config.default_workspace || "";
  f.mode.value = room ? room.mode : "read";
  f.note.value = room ? room.note : "";
  f.max_chain.value = room ? room.max_chain : 4;
  f.discussion_rounds.value = room ? room.discussion_rounds : 3;
  f.turn_timeout_minutes.value = room ? room.turn_timeout_minutes || 0 : 0;
  f.persona.checked = room ? !!room.persona : false;
  f.blind_first_default.checked = room ? room.blind_first_default === true : true;
  // Usage guards apply to an existing room (PATCH); a new room starts with the defaults.
  $("#room-guards").hidden = !room;
  for (const k of ["input_warn_tokens", "context_warn_percent", "seat_input_budget", "room_input_budget"]) f[k].value = room ? room[k] || 0 : 0;
  $("#room-seats-fieldset").hidden = !!room;
  $("#room-seat-specs").replaceChildren();
  $("#btn-copy-room-seats").hidden = !!room || !state.room;
  if (!room) {
    addSeatSpec($("#room-seat-specs"));
  }
  f.dataset.editing = room ? room.id : "";
  $("#dlg-room").showModal();
}

export async function onRoomSubmit(e) {
  e.preventDefault();
  const f = e.target;
  const submit = $("#dlg-room-ok");
  submit.disabled = true;
  const body = {
    title: f.title.value.trim(),
    workspace: f.workspace.value.trim(),
    mode: f.mode.value,
    note: f.note.value,
    max_chain: Number(f.max_chain.value),
    discussion_rounds: Number(f.discussion_rounds.value),
    turn_timeout_minutes: Number(f.turn_timeout_minutes.value || 0),
    persona: f.persona.checked,
    blind_first_default: f.blind_first_default.checked,
  };
  try {
    if (f.dataset.editing) {
      for (const k of ["input_warn_tokens", "context_warn_percent", "seat_input_budget", "room_input_budget"]) body[k] = Number(f[k].value || 0);
      await api("PATCH", "/api/rooms/" + f.dataset.editing, body);
    } else {
      body.participant_specs = readSeatSpecs($("#room-seat-specs"));
      const total = body.participant_specs.reduce((sum, spec) => sum + spec.count, 0);
      if (total < 1 || total > 32) throw new Error("참가자를 1명 이상, 총 32명 이하로 구성하세요.");
      const r = await api("POST", "/api/rooms", body);
      await loadRooms();
      await openRoom(r.id);
      if (pendingInitialPrompt && $("#input")) {
        $("#input").value = pendingInitialPrompt;
        $("#input").focus();
        updateHint();
        pendingInitialPrompt = "";
      }
    }
    $("#dlg-room").close();
  } catch (err) {
    alert(err.message);
  } finally {
    submit.disabled = false;
  }
}
