// Requests to the person have a durable lifetime independent of seat turns.
import { $, announce, el, showAlert } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { openSeq, participantLabel, state } from "./state.js";

const STATUS = { pending: "답변 대기", answered: "답변 보냄", acknowledged: "확인 완료" };

// A resolved request never reopens. A snapshot read before an SSE resolution
// must not restore its old pending state (including while browsing history).
export function mergeUserRequests(current, requests) {
  const next = new Map(current);
  for (const request of requests) {
    const previous = next.get(request.id);
    if (!previous || previous.status === "pending") next.set(request.id, request);
  }
  return next;
}

export function upsertUserRequest(request) {
  state.userRequests = mergeUserRequests(state.userRequests, [request]);
  if (!document.hidden) renderUserRequests();
}

async function resolveUserRequest(request, body, card) {
  const roomId = state.roomId;
  const sequence = openSeq;
  const controls = card.querySelectorAll("button, textarea");
  for (const control of controls) control.disabled = true;
  try {
    const result = await api("POST", "/api/rooms/" + roomId + "/user-requests/" + request.id + "/resolve", body);
    if (state.roomId !== roomId || openSeq !== sequence) return;
    upsertUserRequest(result.user_request);
    announce(body.acknowledge ? "사용자 요청을 확인 처리했습니다." : "요청한 좌석에 답변을 보냈습니다.");
  } catch (error) {
    if (state.roomId === roomId && openSeq === sequence) showAlert(error.message);
  } finally {
    for (const control of controls) control.disabled = false;
  }
}

function requestCard(request) {
  const card = el("li", "user-request-card status-" + request.status);
  card.dataset.requestId = request.id;
  const head = el("div", "user-request-head");
  head.append(el("strong", "", participantLabel(request.seat)), el("span", "badge", STATUS[request.status]));
  card.append(head, el("div", "muted user-request-seat", "@" + request.seat), el("p", "user-request-text", request.text));
  if (request.part) card.append(el("div", "muted", "파트 " + request.part));
  if (request.answer) card.append(el("p", "user-request-answer", "답변: " + request.answer));
  const actions = el("div", "user-request-actions");
  const source = el("button", "ghost", "원문 보기");
  source.type = "button";
  source.onclick = () => hooks.focusTimelineMessage(request.source_msg);
  actions.append(source);
  if (request.status === "pending") {
    const ack = el("button", "", "확인 완료");
    ack.type = "button";
    ack.title = "요청을 처리한 것으로 표시합니다. 좌석은 호출하지 않습니다.";
    ack.onclick = () => resolveUserRequest(request, { acknowledge: true }, card);
    actions.append(ack);
    const details = el("details", "user-request-reply");
    details.append(el("summary", "", "답하기"));
    const form = el("form");
    const input = el("textarea");
    input.rows = 3;
    input.required = true;
    input.setAttribute("aria-label", participantLabel(request.seat) + "에게 보낼 답변");
    const submit = el("button", "primary", "답변 보내기");
    submit.type = "submit";
    form.append(input, submit);
    form.onsubmit = (event) => {
      event.preventDefault();
      resolveUserRequest(request, { text: input.value }, card);
    };
    details.append(form);
    card.append(actions, details);
  } else {
    card.append(actions);
  }
  return card;
}

export function renderUserRequests() {
  const section = $("#user-requests-section");
  const pendingList = $("#user-requests");
  const resolvedList = $("#resolved-user-requests");
  const requests = Array.from(state.userRequests.values()).sort((a, b) => b.seq - a.seq);
  const pending = requests.filter((request) => request.status === "pending").length;
  section.hidden = requests.length === 0;
  $("#user-requests-count").textContent = String(pending);
  $("#btn-user-requests").hidden = pending === 0;
  $("#btn-user-requests").textContent = "사용자 확인 " + pending;
  $("#resolved-user-requests-wrap").hidden = requests.length === pending;
  $("#resolved-user-requests-title").textContent = "처리한 요청 " + (requests.length - pending);
  // Keep an unchanged card's open form, draft and focus while another seat
  // emits events or another request is added/resolved.
  const previous = new Map(Array.from(section.querySelectorAll("li[data-request-id]")).map((card) => [card.dataset.requestId, card]));
  let pendingIndex = 0;
  let resolvedIndex = 0;
  for (const request of requests) {
    let card = previous.get(request.id);
    const signature = JSON.stringify([request, participantLabel(request.seat)]);
    if (!card || card.dataset.signature !== signature) {
      if (card) card.remove();
      card = requestCard(request);
      card.dataset.signature = signature;
    }
    const list = request.status === "pending" ? pendingList : resolvedList;
    const index = request.status === "pending" ? pendingIndex++ : resolvedIndex++;
    if (list.children.item(index) !== card) list.insertBefore(card, list.children.item(index));
    previous.delete(request.id);
  }
  for (const card of previous.values()) card.remove();
}
