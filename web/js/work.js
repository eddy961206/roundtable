// Work plan header, part board, cross-review tables and the diff drawer.
import { $, announce, el, fmtTime } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { authorProvider, openSeq, participantLabel, seatClass, state } from "./state.js";

// ---------- Work division, adversarial review & merge UI ----------
const shortSHA = (sha) => (sha ? String(sha).slice(0, 8) : "");

const REVIEW_STEP_LABELS = {
  findings: "지적 (findings)",
  author_reply: "작성자 응답 (author_reply)",
  rebuttal: "반박 검토 (rebuttal)",
  recheck: "수정 확인 (recheck)",
};
const REVIEW_STEP_ORDER = {
  findings: 1,
  author_reply: 2,
  rebuttal: 3,
  recheck: 4,
};
export function reviewStepLabel(step) {
  return REVIEW_STEP_LABELS[step] || step;
}

const PART_STATE_LABELS = {
  working: "작업 중 (working)",
  reviewing: "리뷰 중 (reviewing)",
  fixing: "수정 중 (fixing)",
  queued: "병합 대기 (queued)",
  awaiting_approval: "승인 대기 (awaiting_approval)",
  merging: "병합 중 (merging)",
  merged: "병합 완료 (merged)",
  blocked: "차단됨 (blocked)",
  abandoned: "포기됨 (abandoned)",
};

// A folded card is one line. Choices are kept per room in localStorage with the
// state they were made in: a part that newly needs a person (blocked, awaiting
// approval) opens again even if it was folded in an earlier state.
const ATTENTION_STATES = new Set(["blocked", "awaiting_approval"]);
const partOpenKey = () => "rt.partOpen." + state.roomId;
function readPartOpen() {
  try {
    return JSON.parse(localStorage.getItem(partOpenKey())) || {};
  } catch (err) {
    console.warn("part fold state unreadable", err.message);
    return {};
  }
}
function savePartOpen(part, open) {
  const saved = readPartOpen();
  saved[part.id] = { open, state: part.state };
  try {
    localStorage.setItem(partOpenKey(), JSON.stringify(saved));
  } catch (err) {
    console.warn("part fold state not saved", err.message);
  }
}
function isPartOpen(part, saved) {
  const choice = saved[part.id];
  if (ATTENTION_STATES.has(part.state) && (!choice || choice.state !== part.state)) return true;
  return choice ? choice.open : false;
}

export function findWorkPart(pid) {
  if (!state.work || !state.work.parts) return null;
  return state.work.parts.find((p) => p.id === pid) || null;
}

export function isPartErrorResolved(m) {
  if (!m || m.status !== "error" || (!m.part && !m.review_step)) return false;
  const part = m.part ? findWorkPart(m.part) : null;
  if (part && part.state === "merged") {
    return true;
  }
  const baseStepOrder = m.review_step ? REVIEW_STEP_ORDER[m.review_step] || 0 : 0;
  const mIdx = state.order.indexOf(m.id);
  for (let i = 0; i < state.order.length; i++) {
    const cand = state.messages.get(state.order[i]);
    if (!cand || cand.id === m.id) continue;
    const isAfter = m.seq != null && cand.seq != null ? cand.seq > m.seq : mIdx >= 0 && i > mIdx;
    if (!isAfter) continue;
    if (m.part && cand.part !== m.part) continue;
    if (cand.status !== "done") continue;
    if (m.review_step) {
      const candStepOrder = cand.review_step ? REVIEW_STEP_ORDER[cand.review_step] || 0 : 0;
      if (candStepOrder > 0 && candStepOrder >= baseStepOrder) return true;
    } else if (m.part && cand.part === m.part) {
      return true;
    }
  }
  return false;
}

export function isPartErrorAbandoned(m) {
  if (!m || m.status !== "error" || (!m.part && !m.review_step)) return false;
  if (isPartErrorResolved(m)) return false;
  const part = m.part ? findWorkPart(m.part) : null;
  return !!(part && part.state === "abandoned");
}

export function refreshPartErrorMessages(partId = "") {
  for (let i = state.winStart; i < state.winEnd; i++) {
    const id = state.order[i];
    const msg = state.messages.get(id);
    if (!msg || msg.status !== "error") continue;
    if (!msg.part && !msg.review_step) continue;
    if (partId && msg.part && msg.part !== partId) continue;
    hooks.renderMessage(id, false);
  }
}

export function focusWorkPartCard(partId) {
  if (!state.work) {
    announce("현재 활성화된 파트 보드가 없습니다.");
    return;
  }
  if (state.workBoardCollapsed) {
    state.workBoardCollapsed = false;
    renderWorkHeadBar();
    renderWork();
  }
  const board = $("#work-board");
  if (!board) return;
  let card = null;
  if (partId) {
    card = Array.from(board.querySelectorAll(".part-card")).find((elNode) => elNode.dataset.partId === partId) || null;
  }
  const target = card || board;
  target.scrollIntoView({ behavior: "smooth", block: "nearest" });
  if (card) {
    card.classList.remove("part-card-flash");
    void card.offsetWidth;
    card.classList.add("part-card-flash");
    setTimeout(() => {
      card.classList.remove("part-card-flash");
    }, 1600);
    const retryBtn = Array.from(card.querySelectorAll(".part-actions button")).find(
      (b) => !b.disabled && (b.textContent === "재시도" || b.textContent.includes("라운드 1회 추가"))
    );
    if (retryBtn) retryBtn.focus({ preventScroll: true });
  }
  announce((partId ? partId + " " : "") + "파트 보드 카드로 이동했습니다.");
}

async function patchMergeMode(nextMode, btn) {
  if (!state.roomId || !state.work) return;
  const roomId = state.roomId;
  const sequence = openSeq;
  const workRev = state.workRevision;
  if (btn) btn.disabled = true;
  try {
    const updated = await api("PATCH", "/api/rooms/" + roomId + "/work", { merge_mode: nextMode });
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if (state.workRevision === workRev && state.work) {
      state.workRevision++;
      if (updated && updated.merge_mode) state.work = updated;
      else state.work.merge_mode = nextMode;
    }
    renderWorkHeadBar();
    renderWork();
    announce("병합 모드를 " + (nextMode === "auto" ? "자동(auto)" : "확인(confirm)") + "으로 변경했습니다.");
  } catch (err) {
    if (state.roomId === roomId && openSeq === sequence) alert(err.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

export function renderWorkHeadBar() {
  const bar = $("#work-head-bar");
  if (!bar) return;
  const w = state.work;
  if (!w) {
    bar.hidden = true;
    bar.replaceChildren();
    delete bar.dataset.sig;
    return;
  }
  const partsCount = w.parts ? w.parts.length : 0;
  const sig = JSON.stringify([
    w.goal,
    w.base_sha,
    w.integration_branch,
    w.integration_sha,
    w.merge_mode,
    w.review_rounds,
    w.queue,
    w.merging,
    state.workBoardCollapsed,
    partsCount,
  ]);
  bar.hidden = false;
  if (bar.dataset.sig === sig) return;
  bar.dataset.sig = sig;
  bar.replaceChildren();

  const info = el("div", "work-head-info");
  const goalSpan = el("span", "work-goal", "🎯 " + w.goal);
  goalSpan.title = "작업 목표: " + w.goal;
  info.appendChild(goalSpan);

  const branchBadge = el("span", "badge mono work-branch-badge", "🌿 " + w.integration_branch);
  branchBadge.title = "통합 branch: " + w.integration_branch;
  info.appendChild(branchBadge);

  const shaBadge = el("span", "badge mono work-sha-badge", "통합 SHA: " + shortSHA(w.integration_sha));
  shaBadge.title = "통합 branch 현재 커밋 SHA: " + w.integration_sha;
  info.appendChild(shaBadge);

  if (w.base_sha) {
    const baseBadge = el("span", "badge mono", "base: " + shortSHA(w.base_sha));
    baseBadge.title = "기준 커밋 SHA: " + w.base_sha;
    info.appendChild(baseBadge);
  }

  if (w.queue && w.queue.length > 0) {
    const qBadge = el("span", "badge warn", "병합 대기열: " + w.queue.join(" → "));
    qBadge.title = "FIFO 병합 대기열";
    info.appendChild(qBadge);
  }

  if (w.merging) {
    const mBadge = el("span", "badge ok mono", "병합 중: " + w.merging.part_id + " (" + shortSHA(w.merging.merge_sha) + ")");
    mBadge.title = "before: " + w.merging.before_sha + " → merge: " + w.merging.merge_sha;
    info.appendChild(mBadge);
  }
  bar.appendChild(info);

  const controls = el("div", "work-head-controls");
  const modeGroup = el("div", "merge-mode-toggle");
  modeGroup.setAttribute("role", "group");
  modeGroup.setAttribute("aria-label", "병합 모드 전환");
  modeGroup.appendChild(el("span", "muted small-note", "병합:"));

  for (const [mode, labelTxt] of [
    ["confirm", "확인 (confirm)"],
    ["auto", "자동 (auto)"],
  ]) {
    const active = w.merge_mode === mode;
    const b = el("button", "merge-mode-btn" + (active ? " active" : ""), labelTxt);
    b.type = "button";
    b.setAttribute("aria-pressed", String(active));
    b.title =
      mode === "confirm"
        ? "확인 모드: 리뷰 통과 후 사람이 승인 버튼을 눌러야 병합합니다"
        : "자동 모드: 리뷰·검증 통과 시 사람 승인 없이 자동 병합합니다";
    b.onclick = () => {
      if (w.merge_mode !== mode) patchMergeMode(mode, b);
    };
    modeGroup.appendChild(b);
  }
  controls.appendChild(modeGroup);

  const foldBtn = el(
    "button",
    "ghost work-fold-btn",
    state.workBoardCollapsed ? "파트 보드 펼치기 (" + partsCount + ") ▾" : "파트 보드 접기 ▴"
  );
  foldBtn.type = "button";
  foldBtn.setAttribute("aria-expanded", String(!state.workBoardCollapsed));
  foldBtn.onclick = () => {
    state.workBoardCollapsed = !state.workBoardCollapsed;
    renderWorkHeadBar();
    renderWork();
  };
  controls.appendChild(foldBtn);
  bar.appendChild(controls);
}

function buildBlockElement(block, isHalted) {
  const box = el("div", isHalted ? "halted-box" : "part-block-box");
  const head = el("div", "block-head");
  head.appendChild(el("strong", "", isHalted ? "⛔ 병합 대기열 정지 (Halted)" : "⛔ 파트 차단 (Blocked)"));
  head.appendChild(el("span", "badge block-kind", block.kind));
  if (block.from) {
    head.appendChild(el("span", "muted small-note", "이전 상태: " + block.from));
  }
  if (block.at) {
    head.appendChild(el("span", "muted small-note", fmtTime(block.at)));
  }
  box.appendChild(head);

  if (block.message) {
    box.appendChild(el("div", "block-msg", block.message));
  }

  if (block.result) {
    const res = block.result;
    if (res.conflicts && res.conflicts.length > 0) {
      const confWrap = el("div", "block-conflicts");
      confWrap.appendChild(el("span", "block-label", "충돌 파일 (" + res.conflicts.length + "): "));
      for (const f of res.conflicts) {
        confWrap.appendChild(el("code", "file-path mono", f));
      }
      box.appendChild(confWrap);
    }
    if (res.name || (res.argv && res.argv.length > 0)) {
      const cmdLine = el("div", "block-cmd mono");
      const argvTxt = res.argv ? res.argv.join(" ") : "";
      cmdLine.textContent =
        "명령: " +
        res.name +
        (argvTxt ? " [" + argvTxt + "]" : "") +
        " · exit code: " +
        res.exit_code +
        (res.timed_out ? " · ⏱ 시간 초과" : "");
      box.appendChild(cmdLine);
    }
    if (res.output) {
      const det = el("details", "block-output-details");
      det.appendChild(el("summary", "", "명령 출력 보기 (Output)"));
      const preEl = el("pre", "mono", res.output);
      det.appendChild(preEl);
      box.appendChild(det);
    }
  }
  return box;
}

async function partAction(part, action, btn) {
  if (!state.roomId || !state.work) return;
  const roomId = state.roomId;
  const sequence = openSeq;
  const workRev = state.workRevision;
  const url = "/api/rooms/" + roomId + "/work/parts/" + encodeURIComponent(part.id) + "/" + action;
  const body =
    action === "approve"
      ? {
          submit_sha: part.submit_sha,
          integration_sha: state.work.integration_sha,
        }
      : undefined;
  if (btn) btn.disabled = true;
  state.partNotices.delete(part.id);
  try {
    const res = await api("POST", url, body);
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if (state.workRevision === workRev && res && res.parts) {
      state.workRevision++;
      state.work = res;
    }
    renderWorkHeadBar();
    renderWork();
    refreshPartErrorMessages(part.id);
    announce(part.id + " 파트 " + action + " 요청 완료");
  } catch (err) {
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if ((action === "approve" || action === "resolve") && err.status === 409) {
      state.partNotices.set(part.id, "변경됨, 다시 확인");
      announce("변경됨, 다시 확인");
      delete $("#work-board").dataset.sig;
      renderWork();
      hooks.openRoom(roomId, true);
      return;
    }
    alert(err.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

function buildReviewSection(part) {
  const rev = part.review;
  const det = el("details", "part-review-section");
  const savedOpen = state.partReviewOpen.get(part.id);
  det.open = savedOpen !== undefined ? savedOpen : true;
  det.ontoggle = () => {
    state.partReviewOpen.set(part.id, det.open);
  };

  const totalItems = rev.rounds.reduce((acc, r) => acc + (r.items ? r.items.length : 0), 0);
  const ovSuffix = rev.resolution
    ? " · 사람 판정으로 통과 · " + rev.resolution.item_ids.length + "건"
    : "";
  const sum = el(
    "summary",
    "",
    "교차 리뷰 상세 (" +
      rev.rounds.length +
      " / " +
      rev.max_rounds +
      " 라운드 · 총 지적 " +
      totalItems +
      "건 · 미해결 " +
      rev.summary.unresolved +
      "건" +
      ovSuffix +
      ")"
  );
  det.appendChild(sum);

  for (const round of rev.rounds) {
    const roundWrap = el("div", "review-round-block");
    const rHead = el(
      "div",
      "review-round-head mono",
      "라운드 " +
        round.ref.round +
        " · base " +
        shortSHA(round.ref.base_sha) +
        " .. head " +
        shortSHA(round.ref.head_sha)
    );
    rHead.title = round.ref.base_sha + ".." + round.ref.head_sha;
    roundWrap.appendChild(rHead);

    const tblWrap = el("div", "review-table-wrap");
    const tbl = el("table", "review-table");
    const thead = el("thead");
    const htr = el("tr");
    for (const col of ["번호", "심각도", "파일:줄", "재현 시나리오", "확신도", "작성자", "리뷰어", "상태"]) {
      htr.appendChild(el("th", "", col));
    }
    thead.appendChild(htr);
    tbl.appendChild(thead);

    const tbody = el("tbody");
    if (!round.items || !round.items.length) {
      const tr = el("tr");
      const td = el("td", "muted", "지적 사항 없음 (0건)");
      td.colSpan = 8;
      tr.appendChild(td);
      tbody.appendChild(tr);
    } else {
      for (const item of round.items) {
        const f = item.finding;
        const tr = el("tr", "review-row status-" + item.status);
        tr.appendChild(el("td", "mono", f.id));

        const sevTd = el("td");
        sevTd.appendChild(el("span", "badge sev-" + f.severity, f.severity));
        tr.appendChild(sevTd);

        const locTxt = (f.side ? f.side + ":" : "") + f.file + ":" + f.line;
        tr.appendChild(el("td", "mono file-loc", locTxt));

        tr.appendChild(el("td", "scenario-cell", f.scenario));

        const confTd = el("td");
        confTd.appendChild(el("span", "badge conf-" + f.confidence, f.confidence));
        tr.appendChild(confTd);

        const authTd = el("td", "reply-cell");
        if (item.author) {
          authTd.appendChild(el("span", "badge reply-action action-" + item.author.action, item.author.action));
          if (item.author.reason) {
            authTd.appendChild(el("span", "reply-reason", " " + item.author.reason));
          }
        } else {
          authTd.textContent = "-";
          authTd.classList.add("muted");
        }
        tr.appendChild(authTd);

        const revTd = el("td", "reply-cell");
        if (item.reviewer) {
          revTd.appendChild(el("span", "badge reply-action action-" + item.reviewer.action, item.reviewer.action));
          if (item.reviewer.reason) {
            revTd.appendChild(el("span", "reply-reason", " " + item.reviewer.reason));
          }
        } else {
          revTd.textContent = "-";
          revTd.classList.add("muted");
        }
        tr.appendChild(revTd);

        const stTd = el("td");
        stTd.appendChild(el("span", "badge item-status status-" + item.status, item.status));
        tr.appendChild(stTd);

        tbody.appendChild(tr);
      }
    }
    tbl.appendChild(tbody);
    tblWrap.appendChild(tbl);
    roundWrap.appendChild(tblWrap);
    det.appendChild(roundWrap);
  }
  return det;
}

export function renderWork() {
  const haltedBanner = $("#work-halted-banner");
  const board = $("#work-board");
  const w = state.work;
  const shouldPinBottom = !state.browsingHistory && state.stickToBottom;
  const syncBottomPin = () => {
    if (shouldPinBottom) {
      const tl = $("#timeline");
      if (tl) tl.scrollTop = tl.scrollHeight;
    }
  };

  if (haltedBanner) {
    if (!w || !w.halted) {
      haltedBanner.hidden = true;
      haltedBanner.replaceChildren();
      delete haltedBanner.dataset.sig;
    } else {
      const hSig = JSON.stringify(w.halted);
      haltedBanner.hidden = false;
      if (haltedBanner.dataset.sig !== hSig) {
        const wasOpen = !!haltedBanner.querySelector(".block-output-details[open]");
        haltedBanner.dataset.sig = hSig;
        haltedBanner.replaceChildren(buildBlockElement(w.halted, true));
        if (wasOpen) {
          const det = haltedBanner.querySelector(".block-output-details");
          if (det) det.open = true;
        }
      }
    }
  }

  if (!board) {
    syncBottomPin();
    return;
  }
  if (!w) {
    board.hidden = true;
    board.replaceChildren();
    delete board.dataset.sig;
    syncBottomPin();
    return;
  }
  board.hidden = state.workBoardCollapsed;
  if (state.workBoardCollapsed) {
    syncBottomPin();
    return;
  }

  const participantsKey = state.room ? state.room.participants.map((p) => [p.id, p.label, p.provider]) : [];
  const sig = JSON.stringify([w, [...state.partNotices.entries()], participantsKey]);
  if (board.dataset.sig === sig) {
    syncBottomPin();
    return;
  }
  board.dataset.sig = sig;
  board.replaceChildren();

  if (!w.parts) return;
  const savedOpen = readPartOpen();
  for (const part of w.parts) {
    const open = isPartOpen(part, savedOpen);
    const card = el("article", "part-card" + seatClass(part.seat) + " state-" + part.state + (open ? "" : " folded"));
    card.dataset.partId = part.id;
    const detail = el("div", "part-detail");
    detail.id = "part-detail-" + part.id;
    detail.hidden = !open;

    const top = el("div", "part-top");
    const titleGroup = el("div", "part-title-group");
    const foldBtn = el("button", "icon-btn part-fold-btn", open ? "▾" : "▸");
    foldBtn.type = "button";
    foldBtn.setAttribute("aria-expanded", String(open));
    foldBtn.setAttribute("aria-controls", detail.id);
    foldBtn.setAttribute("aria-label", part.id + " 파트 상세 " + (open ? "접기" : "펼치기"));
    foldBtn.onclick = () => {
      const next = detail.hidden;
      detail.hidden = !next;
      card.classList.toggle("folded", !next);
      foldBtn.textContent = next ? "▾" : "▸";
      foldBtn.setAttribute("aria-expanded", String(next));
      foldBtn.setAttribute("aria-label", part.id + " 파트 상세 " + (next ? "접기" : "펼치기"));
      savePartOpen(part, next);
    };
    titleGroup.appendChild(foldBtn);
    titleGroup.appendChild(el("span", "badge mono part-id-badge", part.id));
    const titleEl = el("strong", "part-title", part.title);
    titleEl.title = part.title;
    titleGroup.appendChild(titleEl);
    const stBadge = el("span", "badge part-state-badge state-" + part.state, PART_STATE_LABELS[part.state] || part.state);
    titleGroup.appendChild(stBadge);

    const rev = part.review;
    const unresolved = rev && rev.summary ? rev.summary.unresolved : 0;
    const curRound = rev && rev.rounds && rev.rounds.length ? rev.rounds[rev.rounds.length - 1].ref.round : 0;
    const maxRounds = rev && rev.max_rounds ? rev.max_rounds : w.review_rounds;
    const revBadgeCls =
      "badge review-summary-badge" +
      (unresolved > 0 && !(rev && rev.resolution) ? " warn" : rev && ((rev.summary && rev.summary.passed) || rev.resolution) ? " ok" : "");
    const revSummaryTxt =
      "리뷰 " +
      curRound +
      "/" +
      maxRounds +
      "R · 미해결 " +
      unresolved +
      "건" +
      (rev && rev.resolution
        ? " · 사람 판정으로 통과 · " + rev.resolution.item_ids.length + "건"
        : "");
    titleGroup.appendChild(
      el("span", "seat-chip" + seatClass(part.seat) + " " + authorProvider(part.seat), "담당: " + participantLabel(part.seat))
    );
    titleGroup.appendChild(el("span", revBadgeCls, revSummaryTxt));

    if (part.branch) {
      const br = el("span", "badge mono part-fold-extra", "🌿 " + part.branch);
      br.title = "파트 branch: " + part.branch + (part.worktree ? " (" + part.worktree + ")" : "");
      titleGroup.appendChild(br);
    }
    if (part.worktree_removed) {
      titleGroup.appendChild(el("span", "badge part-fold-extra", "작업 폴더 정리됨"));
    }
    if (part.submit_sha) {
      const subSha = el("span", "badge mono part-fold-extra", "제출 SHA: " + shortSHA(part.submit_sha));
      subSha.title = "SubmitSHA: " + part.submit_sha;
      titleGroup.appendChild(subSha);
    }
    if (part.ready_mode) {
      titleGroup.appendChild(el("span", "badge part-fold-extra", "ReadyMode: " + part.ready_mode));
    }
    if (part.merged_sha) {
      const mSha = el("span", "badge mono ok part-fold-extra", "병합됨: " + shortSHA(part.merged_sha));
      mSha.title = "MergedSHA: " + part.merged_sha;
      titleGroup.appendChild(mSha);
    }
    top.appendChild(titleGroup);

    const isReviewEscalated =
      part.state === "blocked" &&
      !!part.block &&
      part.block.kind === "review" &&
      !!rev &&
      (rev.phase === "disputed" || rev.phase === "limit");

    const actions = el("div", "part-actions");
    const btnSubmit = el("button", "", part.state === "fixing" ? "재제출" : "제출");
    btnSubmit.type = "button";
    btnSubmit.disabled = part.state !== "working" && part.state !== "fixing";
    btnSubmit.title = "파트 작업 커밋을 교차 리뷰에 제출합니다";
    btnSubmit.onclick = () => partAction(part, "submit", btnSubmit);
    actions.appendChild(btnSubmit);

    const btnApprove = el("button", "primary", "승인");
    btnApprove.type = "button";
    btnApprove.disabled = part.state !== "awaiting_approval";
    btnApprove.title =
      "현재 제출 SHA(" +
      shortSHA(part.submit_sha) +
      ")와 통합 SHA(" +
      shortSHA(w.integration_sha) +
      ")를 확인하여 병합을 승인합니다";
    btnApprove.onclick = () => partAction(part, "approve", btnApprove);
    actions.appendChild(btnApprove);

    const btnRetry = el("button", "", isReviewEscalated ? "라운드 1회 추가 후 수정" : "재시도");
    btnRetry.type = "button";
    btnRetry.disabled = part.state !== "blocked";
    btnRetry.title = isReviewEscalated
      ? "작성자가 고쳐 다시 제출하면 재리뷰합니다"
      : "차단(blocked)을 해제하고 이전 상태에서 다시 시도합니다";
    btnRetry.onclick = () => partAction(part, "retry", btnRetry);
    actions.appendChild(btnRetry);

    if (isReviewEscalated) {
      const btnResolve = el("button", "primary", "사람 판정");
      btnResolve.type = "button";
      btnResolve.title = "미해결 " + unresolved + "건을 넘기고 병합 대기열로 보냅니다";
      btnResolve.onclick = () => {
        if (!confirm("미해결 " + unresolved + "건을 넘기고 병합 대기열로 보냅니다")) return;
        partAction(part, "resolve", btnResolve);
      };
      actions.appendChild(btnResolve);
    }

    const btnAbandon = el("button", "danger ghost part-fold-extra", "포기");
    btnAbandon.type = "button";
    btnAbandon.disabled = part.state === "merged" || part.state === "abandoned" || part.state === "merging";
    btnAbandon.title = "이 파트를 포기(abandoned) 상태로 전환합니다";
    btnAbandon.onclick = () => partAction(part, "abandon", btnAbandon);
    actions.appendChild(btnAbandon);

    const btnDiff = el("button", "ghost part-fold-extra", "Diff 보기");
    btnDiff.type = "button";
    // Diff reads the repository by SHA, so it stays available after the worktree is cleaned up.
    btnDiff.title = "이 파트의 변경 파일 목록과 패치를 서랍에서 봅니다";
    btnDiff.onclick = () => openDiffDrawer(part.id);
    actions.appendChild(btnDiff);
    // Marked at build time, not by :disabled, so a button locked during its own request stays in place.
    for (const b of actions.children) if (b.disabled) b.classList.add("part-fold-extra");

    top.appendChild(actions);
    card.appendChild(top);

    const meta = el("div", "part-meta");
    const reviewerChip = el(
      "span",
      "seat-chip" + seatClass(part.reviewer) + " " + authorProvider(part.reviewer),
      "리뷰어: " + participantLabel(part.reviewer)
    );
    meta.appendChild(reviewerChip);

    if (rev && rev.phase) {
      meta.appendChild(el("span", "badge", "단계: " + rev.phase));
    }
    if (rev && rev.pending) {
      meta.appendChild(el("span", "badge", "차례: " + rev.pending.who + " · " + rev.pending.kind));
    }

    if (part.scope && part.scope.length > 0) {
      const scopeWrap = el("span", "part-scope");
      scopeWrap.appendChild(el("span", "muted small-note", "범위: "));
      for (const s of part.scope) {
        scopeWrap.appendChild(el("code", "file-path mono", s));
      }
      meta.appendChild(scopeWrap);
    }
    detail.appendChild(meta);

    if (part.brief) {
      const briefEl = el("div", "part-brief");
      briefEl.appendChild(el("span", "muted", "완료 조건: "));
      briefEl.appendChild(el("span", "", part.brief));
      detail.appendChild(briefEl);
    }

    if (state.partNotices.has(part.id)) {
      const noticeBox = el("div", "part-notice", "⚠ " + state.partNotices.get(part.id));
      noticeBox.setAttribute("role", "alert");
      card.appendChild(noticeBox);
    }

    if (part.block) {
      detail.appendChild(buildBlockElement(part.block, false));
    }

    if (rev && rev.rounds && rev.rounds.length > 0) {
      detail.appendChild(buildReviewSection(part));
    }

    card.appendChild(detail);
    board.appendChild(card);
  }
  syncBottomPin();
}

// ---------- Diff Drawer (lazy loaded via GET /api/rooms/{id}/work/parts/{pid}/diff) ----------
export function closeDiffDrawer() {
  if (state.diffController) {
    state.diffController.abort();
    state.diffController = null;
  }
  state.diffPartId = null;
  state.diffData = null;
  state.diffLoading = false;
  state.diffError = "";
  const drawer = $("#diff-drawer");
  if (drawer) {
    drawer.hidden = true;
    drawer.replaceChildren();
  }
}

async function openDiffDrawer(partId) {
  if (!state.roomId) return;
  const roomId = state.roomId;
  const sequence = openSeq;
  if (state.diffController) {
    state.diffController.abort();
  }
  const controller = new AbortController();
  state.diffController = controller;
  state.diffPartId = partId;
  state.diffData = null;
  state.diffLoading = true;
  state.diffError = "";
  renderDiffDrawer();

  try {
    const diff = await api(
      "GET",
      "/api/rooms/" + roomId + "/work/parts/" + encodeURIComponent(partId) + "/diff",
      undefined,
      controller.signal
    );
    if (state.roomId !== roomId || openSeq !== sequence || state.diffPartId !== partId) return;
    state.diffData = diff;
    state.diffLoading = false;
    renderDiffDrawer();
  } catch (err) {
    if (controller.signal.aborted) return;
    if (state.roomId !== roomId || openSeq !== sequence || state.diffPartId !== partId) return;
    state.diffLoading = false;
    state.diffError = err.message;
    renderDiffDrawer();
  } finally {
    if (state.diffController === controller) {
      state.diffController = null;
    }
  }
}

function renderPatchPre(patchText) {
  const preEl = el("pre", "diff-patch mono");
  if (!patchText) {
    preEl.textContent = "(패치 내용 없음)";
    return preEl;
  }
  const lines = String(patchText).split("\n");
  for (const line of lines) {
    let cls = "diff-line";
    if (line.startsWith("+++") || line.startsWith("---")) cls += " diff-meta";
    else if (line.startsWith("+")) cls += " diff-add";
    else if (line.startsWith("-")) cls += " diff-del";
    else if (line.startsWith("@@")) cls += " diff-hunk";
    preEl.appendChild(el("div", cls, line));
  }
  return preEl;
}

function renderDiffDrawer() {
  const drawer = $("#diff-drawer");
  if (!drawer) return;
  if (!state.diffPartId) {
    drawer.hidden = true;
    drawer.replaceChildren();
    return;
  }
  drawer.hidden = false;
  drawer.replaceChildren();

  const part = findWorkPart(state.diffPartId);
  const head = el("div", "diff-drawer-head");
  const titleWrap = el("div", "diff-drawer-title");
  titleWrap.appendChild(el("strong", "", "Diff · " + state.diffPartId + (part ? " (" + part.title + ")" : "")));
  if (state.diffData) {
    const d = state.diffData;
    const rangeSpan = el("span", "badge mono", shortSHA(d.base_sha) + ".." + shortSHA(d.head_sha));
    rangeSpan.title = d.base_sha + ".." + d.head_sha;
    titleWrap.appendChild(rangeSpan);
    titleWrap.appendChild(el("span", "badge diff-stat-add mono", "+" + d.added));
    titleWrap.appendChild(el("span", "badge diff-stat-del mono", "-" + d.deleted));
    if (d.truncated) {
      titleWrap.appendChild(el("span", "badge warn", "⚠ 일부 생략됨 (truncated)"));
    }
  }
  head.appendChild(titleWrap);

  const headBtns = el("div", "diff-drawer-actions");
  const refreshBtn = el("button", "ghost", "새로고침");
  refreshBtn.type = "button";
  refreshBtn.disabled = state.diffLoading;
  refreshBtn.onclick = () => openDiffDrawer(state.diffPartId);
  headBtns.appendChild(refreshBtn);

  const closeBtn = el("button", "icon-btn", "✕");
  closeBtn.type = "button";
  closeBtn.setAttribute("aria-label", "Diff 서랍 닫기");
  closeBtn.onclick = closeDiffDrawer;
  headBtns.appendChild(closeBtn);
  head.appendChild(headBtns);
  drawer.appendChild(head);

  const body = el("div", "diff-drawer-body");
  if (state.diffLoading) {
    body.appendChild(el("p", "muted", "Diff 불러오는 중…"));
    drawer.appendChild(body);
    return;
  }
  if (state.diffError) {
    const errBox = el("div", "err-box");
    errBox.appendChild(el("span", "", "⚠ Diff 조회 실패: " + state.diffError));
    body.appendChild(errBox);
    drawer.appendChild(body);
    return;
  }
  const d = state.diffData;
  if (!d) {
    drawer.appendChild(body);
    return;
  }

  if (!d.files || !d.files.length) {
    body.appendChild(el("p", "muted", "변경된 파일이 없습니다."));
    drawer.appendChild(body);
    return;
  }
  const files = d.files;

  const fileList = el("ul", "diff-file-list");
  files.forEach((f, idx) => {
    const li = el("li", "diff-file-item");
    const jumpBtn = el("button", "diff-file-jump");
    jumpBtn.type = "button";
    jumpBtn.appendChild(el("span", "badge diff-status status-" + f.status, f.status));
    jumpBtn.appendChild(el("span", "mono diff-path", f.path));
    const statSpan = el("span", "diff-counts mono");
    statSpan.appendChild(el("span", "diff-stat-add", "+" + f.added));
    statSpan.appendChild(el("span", "diff-stat-del", " -" + f.deleted));
    jumpBtn.appendChild(statSpan);
    jumpBtn.onclick = () => {
      const target = $("#diff-patch-" + idx, body);
      if (target) {
        target.open = true;
        target.scrollIntoView({ behavior: "smooth", block: "start" });
      }
    };
    li.appendChild(jumpBtn);
    fileList.appendChild(li);
  });
  body.appendChild(fileList);

  files.forEach((f, idx) => {
    const det = el("details", "diff-file-card");
    det.id = "diff-patch-" + idx;
    det.open = true;
    const sum = el("summary", "diff-file-summary");
    sum.appendChild(el("span", "badge diff-status status-" + f.status, f.status));
    sum.appendChild(el("span", "mono diff-path", f.path));
    const counts = el("span", "diff-counts mono");
    counts.appendChild(el("span", "diff-stat-add", "+" + f.added));
    counts.appendChild(el("span", "diff-stat-del", " -" + f.deleted));
    sum.appendChild(counts);
    det.appendChild(sum);
    det.appendChild(renderPatchPre(f.patch));
    body.appendChild(det);
  });

  drawer.appendChild(body);
}
