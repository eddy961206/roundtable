// Forward one message of the open room to another room as it is (rt forward):
// the receiving seats read the original with its source, so nobody retypes it.
import { announce, el, showAlert } from "./util.js";
import { api } from "./api.js";
import { state } from "./state.js";

export function openForwardDialog(m) {
  const fromRoom = state.roomId;
  const dlg = el("dialog", "dlg-message dlg-forward");
  dlg.setAttribute("aria-label", "다른 방으로 전달");
  const form = el("form");
  form.appendChild(el("h3", "", "#" + m.seq + " 다른 방으로 전달"));
  form.appendChild(el("p", "muted small-note", "원문을 그대로 보냅니다. 받는 좌석은 출처(방·번호·작성자)와 함께 읽고, 원문 속 @멘션과 [[REQUEST]] 같은 마커는 실행하지 않습니다."));
  const roomLabel = el("label", "", "받을 방");
  const roomSel = el("select");
  roomSel.required = true;
  const none = (text) => { const o = el("option", "", text); o.value = ""; return o; };
  roomSel.appendChild(none("방 선택"));
  for (const r of state.rooms.filter((x) => x.id !== fromRoom && !x.archived)) {
    const o = el("option", "", r.title);
    o.value = r.id;
    roomSel.appendChild(o);
  }
  roomLabel.appendChild(roomSel);
  const seatLabel = el("label", "", "받을 좌석");
  const seatSel = el("select");
  seatSel.appendChild(none("활성 좌석 전원"));
  seatLabel.appendChild(seatSel);
  const noteLabel = el("label", "", "메모 (선택 · 한 줄 300자)");
  const note = el("input");
  note.maxLength = 300;
  note.placeholder = "예: 이 지적을 반영해 줘";
  noteLabel.appendChild(note);
  form.append(roomLabel, seatLabel, noteLabel);
  roomSel.onchange = async () => {
    seatSel.replaceChildren(none("활성 좌석 전원"));
    if (!roomSel.value) return;
    const picked = roomSel.value;
    try {
      const snap = await api("GET", "/api/rooms/" + picked + "?limit=1");
      if (roomSel.value !== picked) return;
      for (const p of snap.room.participants.filter((x) => x.enabled && !x.removed)) {
        const o = el("option", "", p.label + " · " + p.model);
        o.value = p.id;
        seatSel.appendChild(o);
      }
    } catch (err) {
      showAlert("좌석을 불러오지 못했습니다: " + err.message);
    }
  };
  const menu = el("menu");
  const cancel = el("button", "", "취소");
  cancel.type = "button";
  cancel.onclick = () => { dlg.close(); dlg.remove(); };
  const ok = el("button", "primary", "전달");
  ok.type = "submit";
  menu.append(cancel, ok);
  form.appendChild(menu);
  form.onsubmit = async (e) => {
    e.preventDefault();
    ok.disabled = true;
    try {
      const sent = await api("POST", "/api/rooms/" + roomSel.value + "/forward", { from_room: fromRoom, seq: m.seq, target: seatSel.value, note: note.value.trim() });
      announce("'" + roomSel.selectedOptions[0].textContent + "' 방에 #" + sent.seq + "로 전달했습니다.");
      dlg.close();
      dlg.remove();
    } catch (err) {
      ok.disabled = false;
      showAlert("전달하지 못했습니다: " + err.message);
    }
  };
  dlg.appendChild(form);
  dlg.addEventListener("keydown", (e) => {
    if (e.key !== "Escape") return;
    e.preventDefault();
    e.stopPropagation();
    dlg.close();
    dlg.remove();
  });
  document.body.appendChild(dlg);
  dlg.showModal();
  roomSel.focus();
}
