// Image attachments (draft chips, upload, message thumbnails, lightbox) and agent requests.
import { $, ALLOWED_IMAGE_MIMES, announce, el, fmtBytes, MAX_ATTACHMENT_BYTES, MAX_ATTACHMENTS, showAlert } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { authorProvider, openSeq, participantLabel, seatClass, state } from "./state.js";
import { rewriteProseMentions, visibleMentions } from "./markdown.js";

function detectImageMimeFromBytes(bytes) {
  if (!bytes || bytes.length < 3) return "";
  if (
    bytes.length >= 8 &&
    bytes[0] === 0x89 &&
    bytes[1] === 0x50 &&
    bytes[2] === 0x4e &&
    bytes[3] === 0x47 &&
    bytes[4] === 0x0d &&
    bytes[5] === 0x0a &&
    bytes[6] === 0x1a &&
    bytes[7] === 0x0a
  ) {
    return "image/png";
  }
  if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) {
    return "image/jpeg";
  }
  if (
    bytes.length >= 12 &&
    bytes[0] === 0x52 &&
    bytes[1] === 0x49 &&
    bytes[2] === 0x46 &&
    bytes[3] === 0x46 &&
    bytes[8] === 0x57 &&
    bytes[9] === 0x45 &&
    bytes[10] === 0x42 &&
    bytes[11] === 0x50
  ) {
    return "image/webp";
  }
  return "";
}
function validateAttachmentCandidate({ mime, magicMime, size, activeCount }) {
  if (activeCount >= MAX_ATTACHMENTS) {
    return { ok: false, code: "count", reason: "최대 4장까지 첨부할 수 있습니다 (4장 초과)" };
  }
  if (magicMime !== undefined) {
    if (!ALLOWED_IMAGE_MIMES.has(magicMime)) {
      return { ok: false, code: "format", reason: "PNG·JPEG·WEBP 형식만 첨부할 수 있습니다" };
    }
  } else if (mime) {
    if (!ALLOWED_IMAGE_MIMES.has(mime)) {
      return { ok: false, code: "format", reason: "PNG·JPEG·WEBP 형식만 첨부할 수 있습니다" };
    }
  }
  if (size <= 0) {
    return { ok: false, code: "size", reason: "빈 파일은 첨부할 수 없습니다" };
  }
  if (size > MAX_ATTACHMENT_BYTES) {
    return { ok: false, code: "size", reason: "파일 크기 10MB 초과 (" + fmtBytes(size) + ")" };
  }
  return { ok: true, code: "", reason: "" };
}
const REQUEST_STATUS_LABELS = {
  pending: "대기 (pending)",
  in_progress: "진행 중 (in_progress)",
  done: "완료 (done)",
  rejected: "거절됨 (rejected)",
  cancelled: "취소됨 (cancelled)",
};
function requestStatusLabel(status) {
  return REQUEST_STATUS_LABELS[status] ? REQUEST_STATUS_LABELS[status] : String(status);
}
function isIncompleteRequest(status) {
  return status === "pending" || status === "in_progress";
}
const REQUEST_STATUS_PRIORITY = {
  in_progress: 0,
  pending: 1,
  rejected: 2,
  done: 3,
  cancelled: 4,
};
function sortRequests(requests) {
  const list = Array.from(requests);
  return list.sort((a, b) => {
    const incA = isIncompleteRequest(a.status);
    const incB = isIncompleteRequest(b.status);
    if (incA !== incB) return incA ? -1 : 1;
    const priA = REQUEST_STATUS_PRIORITY[a.status] !== undefined ? REQUEST_STATUS_PRIORITY[a.status] : 9;
    const priB = REQUEST_STATUS_PRIORITY[b.status] !== undefined ? REQUEST_STATUS_PRIORITY[b.status] : 9;
    if (priA !== priB) return priA - priB;
    if (a.at !== b.at) {
      return incA ? a.at - b.at : b.at - a.at;
    }
    return String(a.id).localeCompare(String(b.id));
  });
}
export function mergeSnapshotRequests(currentMap, snapRequests, updatedDuringFetchIds) {
  const next = new Map();
  const updated = updatedDuringFetchIds instanceof Set ? updatedDuringFetchIds : new Set();
  if (Array.isArray(snapRequests)) {
    for (const req of snapRequests) {
      if (!req || !req.id) continue;
      const prev = currentMap && typeof currentMap.get === "function" ? currentMap.get(req.id) : undefined;
      const keepPrev =
        prev &&
        (updated.has(req.id) || (!isIncompleteRequest(prev.status) && isIncompleteRequest(req.status)));
      next.set(req.id, keepPrev ? prev : req);
    }
  }
  if (currentMap && typeof currentMap.entries === "function") {
    for (const [id, prev] of currentMap.entries()) {
      if (!next.has(id) && updated.has(id) && prev) {
        next.set(id, prev);
      }
    }
  }
  return next;
}

function formatLightboxMeta(att, width, height) {
  const parts = [];
  if (width > 0 && height > 0) parts.push(width + "×" + height);
  if (att.bytes > 0) parts.push(fmtBytes(att.bytes));
  if (att.by) parts.push(att.by === "user" ? "나" : participantLabel(att.by));
  return parts.join(" · ");
}

function openLightbox(att, url) {
  const dlg = $("#dlg-lightbox");
  if (!dlg || !att) return;
  const nameEl = $("#lightbox-name");
  const metaEl = $("#lightbox-meta");
  const dlEl = $("#lightbox-download");
  const imgEl = $("#lightbox-img");
  if (nameEl) nameEl.textContent = att.name;
  if (metaEl) metaEl.textContent = formatLightboxMeta(att, att.width, att.height);
  if (dlEl) {
    dlEl.href = url;
    dlEl.setAttribute("download", att.name);
  }
  if (imgEl) {
    imgEl.onload = () => {
      if (!(att.width > 0 && att.height > 0) && imgEl.naturalWidth > 0 && imgEl.naturalHeight > 0 && metaEl) {
        metaEl.textContent = formatLightboxMeta(att, imgEl.naturalWidth, imgEl.naturalHeight);
      }
    };
    imgEl.src = url;
    imgEl.alt = att.name;
  }
  if (!dlg.open) dlg.showModal();
}

export function closeLightbox() {
  const dlg = $("#dlg-lightbox");
  if (dlg && dlg.open) dlg.close();
}

export function renderMessageAttachments(slot, m) {
  if (!slot) return;
  const atts = Array.isArray(m.attachments) ? m.attachments : [];
  if (!atts.length || !state.roomId) {
    slot.hidden = true;
    slot.replaceChildren();
    delete slot.dataset.sig;
    return;
  }
  const sig = JSON.stringify(
    atts.map((a) => [
      a.id,
      a.name,
      a.mime,
      a.bytes,
      a.width,
      a.height,
      a.by,
      a.by && a.by !== "user" ? participantLabel(a.by) : "",
      a.by && a.by !== "user" ? seatClass(a.by) : "",
    ])
  );
  slot.hidden = false;
  if (slot.dataset.sig === sig) return;
  slot.dataset.sig = sig;
  slot.replaceChildren();

  const grid = el("div", "att-grid");
  for (const att of atts) {
    const isAgentAtt = Boolean(att.by && att.by !== "user");
    const sCls = isAgentAtt ? seatClass(att.by) : "";
    const btn = el("button", "att-thumb" + (isAgentAtt ? " agent-att" + sCls : " user-att"));
    btn.type = "button";
    const url = "/api/rooms/" + encodeURIComponent(state.roomId) + "/attachments/" + encodeURIComponent(att.id);

    const img = document.createElement("img");
    img.className = "att-img";
    img.loading = "lazy";
    img.src = url;
    img.alt = att.name;
    if (att.width > 0 && att.height > 0) {
      img.width = att.width;
      img.height = att.height;
    }
    img.addEventListener("load", hooks.onTimelineImageLoad);
    btn.appendChild(img);

    const cap = el("span", "att-caption");
    cap.appendChild(el("span", "att-caption-name", att.name));
    if (att.width > 0 && att.height > 0) {
      cap.appendChild(el("span", "mono", att.width + "×" + att.height));
    }
    btn.appendChild(cap);

    const dimTxt = att.width > 0 && att.height > 0 ? " (" + att.width + "×" + att.height + ")" : "";
    const byTxt = isAgentAtt ? " · " + participantLabel(att.by) : "";
    btn.title = att.name + dimTxt + byTxt + " · 클릭하여 원본 보기";
    btn.setAttribute("aria-label", att.name + dimTxt + " 라이트박스 열기");
    btn.onclick = () => openLightbox(att, url);
    grid.appendChild(btn);
  }
  slot.appendChild(grid);
}

export function collectMessageRequests(m) {
  if (!m) return [];
  const ids = [];
  const seen = new Set();
  if (Array.isArray(m.requests)) {
    for (const rid of m.requests) {
      if (rid && !seen.has(rid)) {
        seen.add(rid);
        ids.push(rid);
      }
    }
  }
  for (const req of state.requests.values()) {
    if (req && req.source_msg === m.id && !seen.has(req.id)) {
      seen.add(req.id);
      ids.push(req.id);
    }
  }
  const list = [];
  for (const rid of ids) {
    const req = state.requests.get(rid);
    if (req) list.push(req);
  }
  return list;
}

function buildSeatRouteChip(seatId) {
  return el(
    "span",
    "seat-chip" + seatClass(seatId) + " " + authorProvider(seatId),
    participantLabel(seatId)
  );
}

function buildMessageRequestCard(req) {
  const card = el("div", "req-card" + seatClass(req.to) + " status-" + req.status);
  card.dataset.reqId = req.id;

  const head = el("div", "req-card-head");
  const route = el("div", "req-route");
  route.appendChild(buildSeatRouteChip(req.from));
  route.appendChild(el("span", "req-arrow muted", "→"));
  route.appendChild(buildSeatRouteChip(req.to));
  const stBadge = el("span", "badge req-status-badge status-" + req.status, requestStatusLabel(req.status));
  route.appendChild(stBadge);
  head.appendChild(route);

  const actions = el("div", "req-card-actions");
  if (req.result_msg) {
    const jumpBtn = el("button", "ghost req-jump-btn", "결과 메시지로 이동");
    jumpBtn.type = "button";
    jumpBtn.onclick = () => hooks.focusTimelineMessage(req.result_msg);
    actions.appendChild(jumpBtn);
  }
  if (isIncompleteRequest(req.status)) {
    const cancelBtn = el("button", "danger ghost req-cancel-btn", "취소");
    cancelBtn.type = "button";
    cancelBtn.onclick = () => cancelRequest(req.id, cancelBtn);
    actions.appendChild(cancelBtn);
  }
  if (actions.childNodes.length > 0) {
    head.appendChild(actions);
  }
  card.appendChild(head);

  if (req.text) {
    card.appendChild(el("div", "req-card-text", rewriteProseMentions(req.text, visibleMentions)));
  }
  if (req.status === "rejected" && req.reason) {
    const reasonEl = el("div", "req-card-reason", "거절 사유: " + req.reason);
    card.appendChild(reasonEl);
  }
  return card;
}

export function renderMessageRequests(slot, m) {
  if (!slot) return;
  const reqs = collectMessageRequests(m);
  if (!reqs.length) {
    slot.hidden = true;
    slot.replaceChildren();
    delete slot.dataset.sig;
    return;
  }
  const sig = JSON.stringify(
    reqs.map((r) => [
      r.id,
      r.from,
      r.to,
      r.text,
      r.status,
      r.result_msg,
      r.reason,
      participantLabel(r.from),
      participantLabel(r.to),
      seatClass(r.from),
      seatClass(r.to),
    ])
  );
  slot.hidden = false;
  if (slot.dataset.sig === sig) return;
  slot.dataset.sig = sig;
  slot.replaceChildren();
  for (const req of reqs) {
    slot.appendChild(buildMessageRequestCard(req));
  }
}

async function cancelRequest(reqId, btn) {
  if (!state.roomId || !reqId) return;
  const roomId = state.roomId;
  const sequence = openSeq;
  if (btn) btn.disabled = true;
  try {
    const updated = await api("POST", "/api/rooms/" + roomId + "/requests/" + encodeURIComponent(reqId) + "/cancel");
    if (state.roomId !== roomId || openSeq !== sequence) return;
    if (updated && updated.id) {
      hooks.upsertRequest(updated);
    } else {
      const cur = state.requests.get(reqId);
      if (cur) hooks.upsertRequest({ ...cur, status: "cancelled" });
    }
    announce("요청이 취소되었습니다.");
  } catch (e) {
    if (state.roomId === roomId && openSeq === sequence) showAlert(e.message);
  } finally {
    if (btn) btn.disabled = false;
  }
}

export function renderRequests() {
  const ul = $("#requests");
  const cnt = $("#requests-count");
  const sorted = sortRequests(state.requests.values());
  const activeCount = sorted.filter((r) => isIncompleteRequest(r.status)).length;
  if (cnt) {
    cnt.textContent = String(sorted.length);
    cnt.className = "badge" + (activeCount > 0 ? " warn" : "");
    cnt.title = activeCount > 0 ? "미완료 " + activeCount + "건 / 전체 " + sorted.length + "건" : "전체 " + sorted.length + "건";
  }
  if (!ul) return;
  const sig = JSON.stringify(
    sorted.map((r) => [
      r.id,
      r.from,
      r.to,
      r.text,
      r.source_msg,
      r.status,
      r.result_msg,
      r.reason,
      r.at,
      participantLabel(r.from),
      participantLabel(r.to),
      seatClass(r.from),
      seatClass(r.to),
    ])
  );
  if (ul.dataset.sig === sig) return;
  ul.dataset.sig = sig;
  ul.replaceChildren();
  if (!sorted.length) {
    ul.appendChild(el("li", "muted", "진행 중인 요청 없음"));
    return;
  }
  for (const req of sorted) {
    const li = el("li", "req-item status-" + req.status);
    li.dataset.reqId = req.id;

    const top = el("div", "req-item-top");
    const route = el("div", "req-route");
    route.appendChild(buildSeatRouteChip(req.from));
    route.appendChild(el("span", "req-arrow muted", "→"));
    route.appendChild(buildSeatRouteChip(req.to));
    top.appendChild(route);
    top.appendChild(el("span", "badge req-status-badge status-" + req.status, requestStatusLabel(req.status)));
    li.appendChild(top);

    if (req.text) {
      li.appendChild(el("div", "req-item-body", rewriteProseMentions(req.text, visibleMentions)));
    }
    if (req.status === "rejected" && req.reason) {
      li.appendChild(el("div", "req-item-reason", "거절 사유: " + req.reason));
    }

    const actions = el("div", "req-item-actions");
    if (req.source_msg) {
      const srcBtn = el("button", "ghost", "요청 이동");
      srcBtn.type = "button";
      srcBtn.onclick = () => hooks.focusTimelineMessage(req.source_msg);
      actions.appendChild(srcBtn);
    }
    if (req.result_msg) {
      const resBtn = el("button", "ghost", "결과 이동");
      resBtn.type = "button";
      resBtn.onclick = () => hooks.focusTimelineMessage(req.result_msg);
      actions.appendChild(resBtn);
    }
    if (isIncompleteRequest(req.status)) {
      const cancelBtn = el("button", "danger", "취소");
      cancelBtn.type = "button";
      cancelBtn.setAttribute(
        "aria-label",
        participantLabel(req.from) + " → " + participantLabel(req.to) + " 요청 취소"
      );
      cancelBtn.onclick = () => cancelRequest(req.id, cancelBtn);
      actions.appendChild(cancelBtn);
    }
    if (actions.childNodes.length > 0) {
      li.appendChild(actions);
    }
    ul.appendChild(li);
  }
}

let draftAttachmentSeq = 0;

export function showComposerError(msg) {
  const error = $("#composer-error");
  if (!error) return;
  error.hidden = false;
  error.textContent = msg;
  const input = $("#input");
  if (input) input.setAttribute("aria-invalid", "true");
}

export function clearComposerError() {
  const error = $("#composer-error");
  if (!error) return;
  error.hidden = true;
  error.textContent = "";
  const input = $("#input");
  if (input) input.removeAttribute("aria-invalid");
}

export function hasAttachmentError() {
  return state.draftAttachments.some((item) => item.status === "error");
}

function activeDraftAttachmentCount() {
  return state.draftAttachments.filter((item) => item.status === "uploading" || item.status === "ready").length;
}

export function clearDraftAttachments() {
  for (const item of state.draftAttachments) {
    item.cancelled = true;
    item.sending = false;
    if (item.previewUrl) {
      URL.revokeObjectURL(item.previewUrl);
      item.previewUrl = "";
    }
  }
  state.draftAttachments = [];
  renderDraftAttachments();
}

export function removeDraftAttachmentsByIds(localIds) {
  if (!localIds || !localIds.size) return;
  const kept = [];
  for (const item of state.draftAttachments) {
    if (localIds.has(item.localId)) {
      item.cancelled = true;
      item.sending = false;
      if (item.previewUrl) {
        URL.revokeObjectURL(item.previewUrl);
        item.previewUrl = "";
      }
    } else {
      kept.push(item);
    }
  }
  state.draftAttachments = kept;
  promoteWaitingCountAttachments();
  if (!hasAttachmentError()) {
    clearComposerError();
  }
  renderDraftAttachments();
}

function removeDraftAttachment(localId) {
  if (isSendingMessage) return;
  const idx = state.draftAttachments.findIndex((item) => item.localId === localId);
  if (idx < 0) return;
  if (state.draftAttachments[idx].sending) return;
  const [removed] = state.draftAttachments.splice(idx, 1);
  removed.cancelled = true;
  if (removed.previewUrl) {
    URL.revokeObjectURL(removed.previewUrl);
    removed.previewUrl = "";
  }
  promoteWaitingCountAttachments();
  if (!hasAttachmentError()) {
    clearComposerError();
  }
  renderDraftAttachments();
}

function promoteWaitingCountAttachments() {
  if (!state.roomId) return;
  for (const item of state.draftAttachments) {
    if (activeDraftAttachmentCount() >= MAX_ATTACHMENTS) break;
    if (item.status === "error" && item.errorCode === "count" && item.file) {
      const canSlice = typeof item.file.slice === "function";
      const check = validateAttachmentCandidate({
        mime: canSlice ? "" : item.file.type,
        size: item.size,
        activeCount: activeDraftAttachmentCount(),
      });
      if (check.ok) {
        item.status = "uploading";
        item.error = "";
        item.errorCode = "";
        item.uploadPromise = verifyAndUploadAttachment(item);
      } else {
        item.error = check.reason;
        item.errorCode = check.code;
      }
    }
  }
}

function renderDraftAttachments() {
  const box = $("#composer-attachments");
  if (!box) return;
  if (!state.draftAttachments.length) {
    box.hidden = true;
    box.replaceChildren();
    return;
  }
  box.hidden = false;
  const nodes = state.draftAttachments.map((item) => {
    let cls = "att-chip";
    if (item.status === "uploading") cls += " uploading";
    else if (item.status === "error") cls += " error";
    const chip = el("div", cls);
    chip.dataset.localId = item.localId;

    if (item.previewUrl) {
      const thumb = document.createElement("img");
      thumb.className = "att-chip-thumb";
      thumb.src = item.previewUrl;
      thumb.alt = item.name;
      chip.appendChild(thumb);
    } else {
      const placeholder = el("div", "att-chip-thumb", "🖼");
      placeholder.style.display = "flex";
      placeholder.style.alignItems = "center";
      placeholder.style.justifyContent = "center";
      chip.appendChild(placeholder);
    }

    const info = el("div", "att-chip-info");
    const nameEl = el("div", "att-chip-name", item.name);
    nameEl.title = item.name;
    info.appendChild(nameEl);

    const sizeText = fmtBytes(item.uploaded ? item.uploaded.bytes : item.size);
    const statusSuffix = item.status === "uploading" ? " · 업로드 중…" : "";
    info.appendChild(el("div", "att-chip-meta", sizeText + statusSuffix));

    if (item.status === "error" && item.error) {
      const errEl = el("div", "att-chip-err", item.error);
      errEl.title = item.error;
      info.appendChild(errEl);
    }
    chip.appendChild(info);

    const rmBtn = el("button", "att-chip-remove", "✕");
    rmBtn.type = "button";
    rmBtn.title = "첨부 제거";
    rmBtn.setAttribute("aria-label", item.name + " 첨부 제거");
    rmBtn.disabled = isSendingMessage || Boolean(item.sending);
    rmBtn.onclick = () => removeDraftAttachment(item.localId);
    chip.appendChild(rmBtn);
    return chip;
  });
  box.replaceChildren(...nodes);
}

export function extractClipboardImageFiles(clipboardData) {
  if (!clipboardData) return [];
  const out = [];
  if (clipboardData.items && clipboardData.items.length) {
    for (const item of Array.from(clipboardData.items)) {
      if (item.kind === "file") {
        const f = item.getAsFile();
        if (f) out.push(f);
      }
    }
  }
  if (!out.length && clipboardData.files && clipboardData.files.length) {
    for (const f of Array.from(clipboardData.files)) {
      out.push(f);
    }
  }
  return out;
}

export function addAttachmentFiles(fileList) {
  if (isSendingMessage || !state.roomId || !fileList) return;
  const files = Array.from(fileList);
  if (!files.length) return;

  for (const file of files) {
    const localId = "draft-att-" + ++draftAttachmentSeq;
    const name = file.name ? file.name : "clipboard-image.png";
    const size = typeof file.size === "number" ? file.size : 0;
    const activeCount = activeDraftAttachmentCount();
    const canSlice = Boolean(file && typeof file.slice === "function");
    const check = validateAttachmentCandidate({
      mime: canSlice ? "" : file.type,
      size,
      activeCount,
    });

    let previewUrl = "";
    if (typeof URL !== "undefined" && typeof URL.createObjectURL === "function" && size > 0) {
      try {
        previewUrl = URL.createObjectURL(file);
      } catch (_) {
        previewUrl = "";
      }
    }

    const item = {
      localId,
      roomId: state.roomId,
      file,
      name,
      size,
      previewUrl,
      status: check.ok ? "uploading" : "error",
      error: check.ok ? "" : check.reason,
      errorCode: check.ok ? "" : check.code,
      uploaded: null,
      uploadPromise: null,
      cancelled: false,
      sending: false,
    };
    state.draftAttachments.push(item);
    if (check.ok) {
      item.uploadPromise = verifyAndUploadAttachment(item);
    }
  }
  renderDraftAttachments();
}

async function verifyAndUploadAttachment(item) {
  const roomId = item.roomId;
  try {
    if (item.file && typeof item.file.slice === "function") {
      const slice = item.file.slice(0, 12);
      if (typeof slice.arrayBuffer === "function") {
        const buf = await slice.arrayBuffer();
        if (item.cancelled || state.roomId !== roomId) return null;
        const magicMime = detectImageMimeFromBytes(new Uint8Array(buf));
        const magicCheck = validateAttachmentCandidate({
          mime: item.file.type,
          magicMime,
          size: item.size,
          activeCount: 0,
        });
        if (!magicCheck.ok) {
          item.status = "error";
          item.error = magicCheck.reason;
          item.errorCode = magicCheck.code;
          promoteWaitingCountAttachments();
          renderDraftAttachments();
          return null;
        }
      }
    }

    const fd = new FormData();
    fd.append("file", item.file, item.name);
    const res = await fetch("/api/rooms/" + encodeURIComponent(roomId) + "/attachments", {
      method: "POST",
      body: fd,
    });
    if (item.cancelled || state.roomId !== roomId) return null;
    const raw = await res.text();
    let data = null;
    if (raw) {
      try {
        data = JSON.parse(raw);
      } catch (_) {
        data = null;
      }
    }
    if (!res.ok) {
      const reason = data && data.error ? data.error : raw ? raw.trim() : res.statusText;
      throw new Error(reason);
    }
    if (!data || !data.id) {
      throw new Error("서버 응답에 첨부 ID가 없습니다.");
    }
    item.uploaded = data;
    item.status = "ready";
    item.error = "";
    item.errorCode = "";
    renderDraftAttachments();
    return data;
  } catch (err) {
    if (item.cancelled || state.roomId !== roomId) return null;
    item.status = "error";
    item.error = "업로드 실패: " + (err && err.message ? err.message : "알 수 없는 오류");
    item.errorCode = "upload";
    promoteWaitingCountAttachments();
    renderDraftAttachments();
    return null;
  }
}

export let isSendingMessage = false;

export function setComposerBusy(busy) {
  isSendingMessage = busy;
  $("#input").readOnly = busy;
  $("#btn-send").disabled = busy;
  $("#btn-attach").disabled = busy;
  $("#file-input").disabled = busy;
  renderDraftAttachments();
}
