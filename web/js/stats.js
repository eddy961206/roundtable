// "모델 기록" dialog: the per-model performance table computed by GET /api/stats/models,
// plus the turn-timing tooltip text shared with message meta (R4, R5).
import { $, el, fmtElapsed, fmtK, LABEL } from "./util.js";
import { api } from "./api.js";

// Tooltip for a reply's response time: wait in the queue, time to the first output, total.
export function timingTitle(m) {
  const t = m && m.timing;
  if (!t) return "이 답변에 걸린 시간";
  const parts = [];
  if (t.queued_ms > 0) parts.push("대기 " + fmtElapsed(t.queued_ms));
  if (t.first_output_ms > 0) parts.push("첫 출력까지 " + fmtElapsed(t.first_output_ms));
  if (t.total_ms > 0) parts.push("전체 " + fmtElapsed(t.total_ms));
  return parts.length ? "이 답변에 걸린 시간 · " + parts.join(" · ") : "이 답변에 걸린 시간";
}

const COLUMNS = [
  { key: "provider", label: "공급사", text: (r) => LABEL[r.provider] || r.provider },
  { key: "model", label: "모델", text: (r) => r.model || "" },
  { key: "effort", label: "추론", text: (r) => r.effort || "기본" },
  { key: "role", label: "역할", text: (r) => r.role || "(없음)", title: (r) => r.role || "" },
  { key: "turns", label: "턴", num: true, text: (r) => String(r.turns || 0) },
  { key: "errors", label: "오류", num: true, text: (r) => String(r.errors || 0) },
  { key: "median_total_ms", label: "전체 중앙값", num: true, text: (r) => (r.median_total_ms > 0 ? fmtElapsed(r.median_total_ms) : "–") },
  { key: "median_first_output_ms", label: "첫 출력 중앙값", num: true, text: (r) => (r.median_first_output_ms > 0 ? fmtElapsed(r.median_first_output_ms) : "–") },
  { key: "input_tokens", label: "입력", num: true, text: (r) => fmtK(r.input_tokens) },
  { key: "output_tokens", label: "출력", num: true, text: (r) => fmtK(r.output_tokens) },
  { key: "cached_tokens", label: "캐시", num: true, text: (r) => fmtK(r.cached_tokens) },
  { key: "parts_authored", label: "파트 담당", num: true, text: (r) => String(r.parts_authored || 0) },
  { key: "parts_passed_first_round", label: "첫 라운드 통과", num: true, text: (r) => String(r.parts_passed_first_round || 0) },
  { key: "findings_raised", label: "지적 제기", num: true, text: (r) => String(r.findings_raised || 0) },
  { key: "findings_fixed", label: "지적 수정됨", num: true, text: (r) => String(r.findings_fixed || 0) },
  { key: "findings_withdrawn", label: "지적 철회", num: true, text: (r) => String(r.findings_withdrawn || 0) },
  { key: "last_seen_at", label: "최근 관측", num: true, text: (r) => (r.last_seen_at ? new Date(r.last_seen_at).toLocaleDateString("ko-KR") : "–"), title: (r) => (r.last_seen_at ? new Date(r.last_seen_at).toLocaleString("ko-KR") : "") },
];

let rows = [];
let generatedAt = 0;
let sortKey = "turns";
let sortDesc = true;

function compare(a, b, col) {
  const av = a[col.key];
  const bv = b[col.key];
  if (col.num) return (Number(av) || 0) - (Number(bv) || 0);
  return String(av || "").localeCompare(String(bv || ""), "ko");
}

function renderTable() {
  const box = $("#stats-table");
  if (!box) return;
  // Sorting rebuilds the table; a keyboard user who pressed Enter on a header keeps that header.
  const keepFocus = box.contains(document.activeElement) && document.activeElement.classList.contains("sort-btn");
  box.replaceChildren();
  const col = COLUMNS.find((c) => c.key === sortKey) || COLUMNS[0];
  const sorted = [...rows].sort((a, b) => (sortDesc ? -1 : 1) * compare(a, b, col));
  const table = el("table", "stats-table");
  const thead = el("thead");
  const tr = el("tr");
  for (const c of COLUMNS) {
    const th = el("th", c.num ? "num" : "");
    th.setAttribute("scope", "col");
    th.setAttribute("aria-sort", c.key === sortKey ? (sortDesc ? "descending" : "ascending") : "none");
    const b = el("button", "sort-btn", c.label + (c.key === sortKey ? (sortDesc ? " ▾" : " ▴") : ""));
    b.type = "button";
    b.dataset.key = c.key;
    b.title = c.label + " 기준으로 정렬";
    b.onclick = () => {
      if (sortKey === c.key) sortDesc = !sortDesc;
      else { sortKey = c.key; sortDesc = !!c.num; }
      renderTable();
    };
    th.appendChild(b);
    tr.appendChild(th);
  }
  thead.appendChild(tr);
  table.appendChild(thead);
  const tbody = el("tbody");
  for (const r of sorted) {
    const row = el("tr");
    for (const c of COLUMNS) {
      const td = el("td", c.num ? "num" : c.key === "role" ? "role" : "", c.text(r));
      if (c.title) { const t = c.title(r); if (t) td.title = t; }
      row.appendChild(td);
    }
    tbody.appendChild(row);
  }
  table.appendChild(tbody);
  box.appendChild(table);
  if (keepFocus) {
    const btn = box.querySelector('.sort-btn[data-key="' + CSS.escape(sortKey) + '"]');
    if (btn) btn.focus();
  }
}

export async function openModelStatsDialog() {
  const dlg = $("#dlg-stats");
  const status = $("#stats-status");
  const box = $("#stats-table");
  if (!dlg || !status || !box) return;
  box.replaceChildren();
  status.className = "muted";
  status.textContent = "모델 기록을 불러오는 중…";
  dlg.showModal();
  try {
    const data = await api("GET", "/api/stats/models");
    if (!data || !Array.isArray(data.rows)) throw new Error("응답에 rows 배열이 없습니다");
    rows = data.rows;
    generatedAt = Number(data.generated_at) || 0;
    if (!rows.length) {
      status.textContent = "아직 기록된 에이전트 턴이 없습니다. 방에서 대화하면 여기에 쌓입니다.";
      return;
    }
    status.textContent = rows.length + "행 · 계산 시각 " + (generatedAt ? new Date(generatedAt).toLocaleString("ko-KR") : "알 수 없음") + " · 저장된 방 데이터에서 계산한 값이며, 표본(턴 수)이 적은 행은 참고만 하세요.";
    renderTable();
  } catch (err) {
    status.className = "warn";
    status.textContent = "모델 기록을 불러오지 못했습니다: " + (err.message || String(err));
  }
}
