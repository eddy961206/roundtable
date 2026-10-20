// Decision cards: the right-panel "결정" section, the add/edit dialog and the
// compact cards under the message that recorded them.
import { $, announce, el, fmtTime } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { openSeq, participantLabel, state } from "./state.js";

// K2: a card an agent wrote with [[DECISION]] is "proposed" until a person
// confirms it (active) or rejects it (dropped); only active cards reach the brief.
const DECISION_STATUS_LABELS = {
  proposed: "제안",
  active: "유효",
  superseded: "대체됨",
  dropped: "폐기됨",
};
function decisionStatusLabel(status) {
  return DECISION_STATUS_LABELS[status] || String(status);
}
function decisionAuthor(d) {
  return d.by === "user" ? "사용자" : participantLabel(d.by);
}

// Proposed cards (waiting for the person) first, then active cards, then the
// rest; most recently changed first within each group and later records win a
// tie. The standing brief picks active cards in the same order.
const STATUS_RANK = { proposed: 0, active: 1 };
function sortedDecisions() {
  const list = Array.from(state.decisions.values());
  const order = new Map(list.map((d, i) => [d.id, i]));
  return list.sort((a, b) => {
    const rankA = STATUS_RANK[a.status] ?? 2;
    const rankB = STATUS_RANK[b.status] ?? 2;
    if (rankA !== rankB) return rankA - rankB;
    if (a.at !== b.at) return b.at - a.at;
    return order.get(b.id) - order.get(a.id);
  });
}

function buildProposedActions(d) {
  const confirm = el("button", "primary dec-confirm", "확정");
  confirm.type = "button";
  confirm.title = "유효(active)로 바꿉니다. 다음 턴부터 모든 참가자의 안내문에 들어갑니다.";
  confirm.setAttribute("aria-label", "결정 확정: " + d.text);
  confirm.onclick = () => patchDecision(d.id, { status: "active" }, confirm);
  const reject = el("button", "danger dec-reject", "거절");
  reject.type = "button";
  reject.title = "폐기(dropped)로 바꿉니다. 카드는 기록으로 남습니다.";
  reject.setAttribute("aria-label", "결정 거절: " + d.text);
  reject.onclick = () => patchDecision(d.id, { status: "dropped" }, reject);
  return [confirm, reject];
}

// Decisions are never deleted and the server makes "at" strictly increase on
// every change of a card, so a state older than the one already shown (a late
// PATCH reply after a newer SSE event, or a snapshot read before it) is not
// applied. An equal "at" is the same state line (a reply and its SSE echo).
export function isStaleDecision(prev, next) {
  return !!prev && prev.at > next.at;
}

export function mergeSnapshotDecisions(current, snapDecisions) {
  const next = new Map(current);
  for (const d of snapDecisions || []) {
    if (!isStaleDecision(next.get(d.id), d)) next.set(d.id, d);
  }
  return next;
}

export function upsertDecision(d) {
  if (!d || !d.id || isStaleDecision(state.decisions.get(d.id), d)) return;
  state.decisions.set(d.id, d);
  if (document.hidden) return;
  renderDecisions();
  if (d.source_msg && state.messages.has(d.source_msg)) hooks.renderMessage(d.source_msg, false);
}

function detailRow(label, value) {
  const row = el("div", "dec-detail-row");
  row.appendChild(el("span", "dec-detail-label muted", label));
  row.appendChild(el("span", "dec-detail-value", value || "—"));
  return row;
}

export function renderDecisions() {
  const ul = $("#decisions");
  const cnt = $("#decisions-count");
  const addBtn = $("#btn-add-decision");
  if (addBtn) addBtn.disabled = !state.roomId;
  const sorted = sortedDecisions();
  const active = sorted.filter((d) => d.status === "active").length;
  const proposed = sorted.filter((d) => d.status === "proposed").length;
  if (cnt) {
    // Same rule as the 요청 badge: the total, highlighted while something waits for the person.
    cnt.textContent = String(sorted.length);
    cnt.className = "badge" + (proposed > 0 ? " warn" : "");
    cnt.title = (proposed > 0 ? "확정 대기 " + proposed + "건 · " : "") + "유효 " + active + "건 · 전체 " + sorted.length + "건";
  }
  if (!ul) return;
  const sig = JSON.stringify(sorted.map((d) => [d.id, d.text, d.evidence, d.dissent, d.unverified, d.status, d.at, d.source_msg, decisionAuthor(d)]));
  if (ul.dataset.sig === sig) return;
  ul.dataset.sig = sig;
  ul.replaceChildren();
  if (!sorted.length) {
    ul.appendChild(el("li", "muted", "기록된 결정 없음"));
    return;
  }
  for (const d of sorted) {
    const li = el("li", "dec-item status-" + d.status);
    li.dataset.decisionId = d.id;
    const top = el("div", "dec-item-top");
    top.appendChild(el("div", "dec-item-text", d.text));
    top.appendChild(el("span", "badge dec-status-badge status-" + d.status, decisionStatusLabel(d.status)));
    li.appendChild(top);
    li.appendChild(
      el(
        "div",
        "dec-item-meta muted",
        decisionAuthor(d) + (d.status === "proposed" ? " 제안 · 확정해야 안내문에 들어갑니다 · " : " · ") + fmtTime(d.at)
      )
    );

    const details = el("details", "dec-details");
    details.appendChild(el("summary", "", "근거·반대·미확인"));
    details.appendChild(detailRow("근거", d.evidence));
    details.appendChild(detailRow("반대", d.dissent));
    details.appendChild(detailRow("미확인", d.unverified));
    li.appendChild(details);

    const actions = el("div", "dec-item-actions");
    if (d.source_msg) {
      const src = el("button", "ghost", "원 메시지 이동");
      src.type = "button";
      src.onclick = () => hooks.focusTimelineMessage(d.source_msg);
      actions.appendChild(src);
    }
    const edit = el("button", "ghost", "수정");
    edit.type = "button";
    edit.setAttribute("aria-label", "결정 수정: " + d.text);
    edit.onclick = () => openDecisionDialog(d);
    actions.appendChild(edit);
    if (d.status === "proposed") {
      for (const b of buildProposedActions(d)) actions.appendChild(b);
    } else {
      const toggle = el("button", d.status === "active" ? "danger" : "", d.status === "active" ? "폐기" : "다시 유효");
      toggle.type = "button";
      toggle.setAttribute("aria-label", (d.status === "active" ? "결정 폐기: " : "결정 다시 유효: ") + d.text);
      toggle.onclick = () => patchDecision(d.id, { status: d.status === "active" ? "dropped" : "active" }, toggle);
      actions.appendChild(toggle);
    }
    li.appendChild(actions);
    ul.appendChild(li);
  }
}

// Compact cards under the message whose [[DECISION: …]] markers were recorded.
export function renderMessageDecisions(slot, m) {
  if (!slot) return;
  const cards = (m.decisions || []).map((id) => state.decisions.get(id)).filter(Boolean);
  if (!cards.length) {
    slot.hidden = true;
    slot.replaceChildren();
    delete slot.dataset.sig;
    return;
  }
  const sig = JSON.stringify(cards.map((d) => [d.id, d.text, d.status]));
  slot.hidden = false;
  if (slot.dataset.sig === sig) return;
  slot.dataset.sig = sig;
  slot.replaceChildren();
  for (const d of cards) {
    const card = el("div", "msg-decision status-" + d.status);
    card.appendChild(el("span", "msg-decision-label", d.status === "proposed" ? "📌 결정 제안" : "📌 결정"));
    card.appendChild(el("span", "msg-decision-text", d.text));
    card.appendChild(el("span", "badge dec-status-badge status-" + d.status, decisionStatusLabel(d.status)));
    if (d.status === "proposed") {
      const acts = el("span", "msg-decision-actions");
      for (const b of buildProposedActions(d)) acts.appendChild(b);
      card.appendChild(acts);
    }
    slot.appendChild(card);
  }
}

async function patchDecision(id, patch, btn) {
  if (!state.roomId) return;
  const roomId = state.roomId;
  const sequence = openSeq;
  if (btn) btn.disabled = true;
  try {
    const updated = await api("PATCH", "/api/rooms/" + roomId + "/decisions/" + encodeURIComponent(id), patch);
    if (state.roomId !== roomId || openSeq !== sequence) return;
    upsertDecision(updated);
    const status = state.decisions.get(id).status;
    announce(
      status === "active"
        ? "결정을 확정했습니다. 다음 턴부터 안내문에 들어갑니다."
        : "결정이 " + decisionStatusLabel(status) + " 상태로 표시됩니다."
    );
  } catch (e) {
    if (state.roomId === roomId && openSeq === sequence) alert(e.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

let editingDecision = null;
export function openDecisionDialog(d) {
  editingDecision = d || null;
  const f = $("#form-decision");
  $("#dlg-decision-title").textContent = d ? "결정 수정" : "결정 추가";
  f.text.value = d ? d.text : "";
  f.evidence.value = d ? d.evidence : "";
  f.dissent.value = d ? d.dissent : "";
  f.unverified.value = d ? d.unverified : "";
  f.status.value = d ? d.status : "active";
  $("#decision-status-row").hidden = !d;
  const error = $("#decision-form-error");
  error.hidden = true;
  error.textContent = "";
  $("#dlg-decision").showModal();
  f.text.focus();
}

async function onDecisionSubmit(e) {
  e.preventDefault();
  if (!state.roomId) return;
  const f = e.target;
  const submit = $("#dlg-decision-ok");
  const error = $("#decision-form-error");
  const roomId = state.roomId;
  const body = {
    text: f.text.value.trim(),
    evidence: f.evidence.value.trim(),
    dissent: f.dissent.value.trim(),
    unverified: f.unverified.value.trim(),
  };
  // Only a changed status is sent, so editing the text of a card keeps its state line intact.
  if (editingDecision && f.status.value !== editingDecision.status) body.status = f.status.value;
  submit.disabled = true;
  try {
    const saved = editingDecision
      ? await api("PATCH", "/api/rooms/" + roomId + "/decisions/" + encodeURIComponent(editingDecision.id), body)
      : await api("POST", "/api/rooms/" + roomId + "/decisions", body);
    if (state.roomId === roomId) upsertDecision(saved);
    $("#dlg-decision").close();
    announce(editingDecision ? "결정을 수정했습니다." : "결정을 추가했습니다.");
  } catch (err) {
    error.hidden = false;
    error.textContent = err.message;
  } finally {
    submit.disabled = false;
  }
}

export function bindDecisionControls() {
  $("#btn-add-decision").onclick = () => openDecisionDialog(null);
  $("#form-decision").addEventListener("submit", onDecisionSubmit);
  $("#dlg-decision-cancel").onclick = () => $("#dlg-decision").close();
}
