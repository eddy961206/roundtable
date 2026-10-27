// Update banner: the server compares its build with the published branch on its
// own schedule; this only reads GET /api/version and shows newer commits.
import { $, fmtTime } from "./util.js";
import { api } from "./api.js";

const DISMISS_KEY = "rt.updateDismissed"; // the latest sha the user closed the banner for
const POLL_MS = 30 * 60 * 1000;

function dismissedFor() {
  try { return localStorage.getItem(DISMISS_KEY) || ""; } catch { return ""; }
}

// Shown only while the branch has commits the build lacks; closing hides it until
// the branch moves again. Commit titles come from GitHub, so they go in as text.
export function renderUpdateBanner(v) {
  const box = $("#update-banner");
  const show = !!(v && v.update_available && v.latest && dismissedFor() !== v.latest);
  box.hidden = !show;
  if (!show) return;
  const newest = (v.commits || [])[0];
  $("#update-text").textContent = "새 업데이트 " + v.behind_by + "개" + (newest ? " — 최근: " + newest.title : "");
  const titles = (v.commits || []).map((c) => "• " + c.title).join("\n");
  const built = (v.running.revision || "").slice(0, 7) + (v.running.modified ? " (로컬 수정 포함)" : "");
  box.title = "실행 중 " + built + " → " + v.branch + " " + v.latest.slice(0, 7) +
    (v.checked_at ? " · 확인 " + fmtTime(v.checked_at) : "") + (titles ? "\n" + titles : "");
  const link = $("#update-link");
  link.href = "https://github.com/" + v.repo + "/compare/" + v.running.revision + "..." + v.branch;
  box.dataset.latest = v.latest;
}

async function refresh() {
  try {
    renderUpdateBanner(await api("GET", "/api/version"));
  } catch {
    // An older server without /api/version, or a transient error: no banner.
  }
}

export function startUpdateCheck() {
  $("#btn-update-close").onclick = () => {
    const box = $("#update-banner");
    try { localStorage.setItem(DISMISS_KEY, box.dataset.latest || ""); } catch { /* private mode: closes for this page only */ }
    box.hidden = true;
  };
  refresh();
  setInterval(refresh, POLL_MS);
}
