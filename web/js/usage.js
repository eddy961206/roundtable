// Sidebar account usage panel, provider CLI status (with versions) and the usage refresh timer.
import { $, el, fmtK, fmtReset, fmtTime, LABEL } from "./util.js";
import { api } from "./api.js";
import { activeParticipants, seatClass, state } from "./state.js";

export let usageTimer = null;
export function scheduleUsageRefresh(delay = 60000) {
  clearTimeout(usageTimer);
  if (!document.hidden && usageProviders().length) usageTimer = setTimeout(() => refreshLimits(), delay);
}

export async function refreshLimits(manual = false, cached = false) {
  const providers = usageProviders();
  if (!providers.length || state.usageBusy || document.hidden) return;
  const selection = providers.join(",");
  state.usageBusy = true;
  state.usageError = "";
  renderUsagePanel();
  try {
    const usage = cached
      ? await api("GET", "/api/usage?providers=" + encodeURIComponent(selection))
      : await api("POST", "/api/usage/refresh", { providers, force: manual });
    if (usageProviders().join(",") !== selection) return;
    state.limits = usage.limits;
    state.usageSources = usage.sources;
    state.usageChecked = Date.now();
  } catch (err) {
    if (usageProviders().join(",") === selection) state.usageError = "갱신 실패 · 다시 시도하세요";
  } finally {
    state.usageBusy = false;
    renderUsagePanel();
    scheduleUsageRefresh(usageProviders().join(",") === selection ? 60000 : 0);
  }
}

// One line per provider: found/not found, then the CLI version with the time it was read
// (R6: version, version_checked_at in ms, version_error come with GET /api/providers).
function versionText(p) {
  if (!p.available) return { text: "", cls: "muted" };
  if (p.version_error) return { text: "버전 확인 실패 · " + p.version_error, cls: "warn" };
  if (p.version) return { text: "v" + p.version + (p.version_checked_at ? " · 확인 " + fmtTime(p.version_checked_at) : ""), cls: "muted" };
  if (p.version_checked_at) return { text: "버전 없음 · 확인 " + fmtTime(p.version_checked_at), cls: "muted" };
  return { text: "버전 확인 중…", cls: "muted" };
}

let versionsError = "";
export function renderProviderStatus() {
  const box = $("#limits");
  if (!box) return;
  box.innerHTML = "";
  for (const p of state.providers) {
    const row = el("div", "prov-mini");
    const head = el("div", "prov-mini-head");
    head.appendChild(el("span", "prov-name " + p.id, LABEL[p.id] || p.label || p.id));
    const statusText = !p.available ? "CLI 없음" : p.can_steer ? "CLI 있음 · 즉시 조정" : "CLI 있음";
    head.appendChild(el("span", p.available ? "muted" : "unavailable", statusText));
    row.appendChild(head);
    const v = versionText(p);
    if (v.text) {
      const line = el("div", "prov-version small-note " + v.cls, v.text);
      line.title = p.version_error ? p.version_error : p.version_checked_at ? new Date(p.version_checked_at).toLocaleString("ko-KR") + "에 확인" : "";
      row.appendChild(line);
    }
    box.appendChild(row);
  }
  const err = $("#versions-error");
  if (err) {
    err.textContent = versionsError;
    err.hidden = !versionsError;
  }
}

// POST /api/providers/versions re-runs the version commands; the result is read back with GET /api/providers.
let versionsBusy = false;
export async function refreshProviderVersions() {
  const btn = $("#btn-refresh-versions");
  if (versionsBusy) return;
  versionsBusy = true;
  versionsError = "";
  if (btn) { btn.disabled = true; btn.textContent = "확인 중…"; }
  try {
    await api("POST", "/api/providers/versions");
    const prov = await api("GET", "/api/providers");
    if (!prov || !Array.isArray(prov.providers)) throw new Error("응답에 providers 배열이 없습니다");
    state.providers = prov.providers;
    state.catalogRevision++;
  } catch (err) {
    versionsError = "버전을 다시 확인하지 못했습니다: " + (err.message || String(err));
  } finally {
    versionsBusy = false;
    if (btn) { btn.disabled = false; btn.textContent = "다시 확인"; }
    renderProviderStatus();
  }
}

export function usageGauge(usedPct, mini) {
  const pct = Math.min(100, Math.max(0, Number(usedPct) || 0));
  const cls = "gauge" + (pct >= 90 ? " bad" : pct >= 80 ? " warn" : "") + (mini ? " mini" : "");
  const g = el("div", cls);
  const i = el("i");
  i.style.width = pct.toFixed(0) + "%";
  g.appendChild(i);
  return g;
}

function remainGauge(remainPct, mini) {
  const rem = Math.min(100, Math.max(0, Number(remainPct) || 0));
  const cls = "gauge" + (rem <= 15 ? " bad" : rem <= 30 ? " warn" : "") + (mini ? " mini" : "");
  const g = el("div", cls);
  const i = el("i");
  i.style.width = rem.toFixed(0) + "%";
  g.appendChild(i);
  return g;
}

// ---------- Account usage in the persistent sidebar ----------
export function messageInputTokens(u) {
  if (!u || u.input_tokens == null) return 0;
  return (
    Number(u.input_tokens || 0) +
    Number(u.cache_read_input_tokens || u.cached_input_tokens || u.cache_read_tokens || 0)
  );
}

function usageProviders() {
  return [...new Set(activeParticipants().map((p) => p.provider))];
}

export function renderUsagePanel() {
  const strip = $("#token-strip");
  const detail = $("#usage-detail-content");
  if (!strip || !detail) return;
  const selected = usageProviders();
  const selection = selected.join(",");
  if (state.usageSelection !== selection) {
    state.usageSelection = selection; state.usageChecked = 0; state.usageError = "";
    scheduleUsageRefresh(0);
  }
  const participants = state.room ? state.room.participants : [];
  const key = JSON.stringify([!!state.room, selection, state.limits, state.usageSources, state.usageBusy, state.usageError, state.usageChecked, participants.map((p) => p.totals)]);
  if (strip.dataset.key === key) return;
  strip.dataset.key = key;
  strip.replaceChildren();
  detail.replaceChildren();
  for (const provider of selected) {
    const limit = state.limits[provider];
    const windows = limit && limit.windows ? limit.windows.filter((w) => Number.isFinite(w.used_percent)) : [];
    const source = state.usageSources[provider];
    const unsupported = source && source.refresh_kind === "unsupported";
    const remaining = windows.map((w) => Math.max(0, Math.min(100, 100 - Number(w.used_percent))));
    const seat = participants.find((p) => p.provider === provider && !p.removed);
    const account = el("section", "usage-account" + (seat ? seatClass(seat.id) : ""));
    account.setAttribute("aria-label", (LABEL[provider] || provider) + " 남은 사용량");
    const heading = el("div", "usage-account-head");
    heading.appendChild(el("span", "prov-name " + provider, LABEL[provider] || provider));
    if (!unsupported) {
      const stale = !source || source.stale;
      const observed = source && source.observed_at || limit && limit.at;
      const quality = el("span", "usage-quality" + (stale ? " stale" : ""), !observed ? "미확인" : stale ? "이전 관측" : "최근 관측");
      quality.title = source && source.observed_at ? new Date(source.observed_at).toLocaleString("ko-KR") + "에 관측" : "새 관측을 아직 확인하지 못했어.";
      heading.appendChild(quality);
    }
    account.appendChild(heading);
    for (let i = 0; i < windows.length; i++) {
      const item = el("div", "usage-window" + (remaining[i] <= 15 ? " warn" : ""));
      const title = el("div", "usage-window-title");
      title.appendChild(el("span", "", windows[i].label));
      title.appendChild(el("b", "", Math.round(remaining[i]) + "%"));
      item.appendChild(title);
      const gauge = remainGauge(remaining[i], true);
      gauge.setAttribute("aria-hidden", "true");
      item.appendChild(gauge);
      account.appendChild(item);
    }
    if (!windows.length) account.appendChild(el("p", "usage-empty muted", unsupported ? "한도·잔액 조회 미지원" : "아직 관측값이 없어"));
    strip.appendChild(account);
    const group = el("section", "usage-provider");
    group.appendChild(el("b", "", LABEL[provider] || provider));
    for (let i = 0; i < windows.length; i++) {
      const w = windows[i];
      const row = el("div", "stat-row");
      row.appendChild(el("span", "", w.label));
      const resetText = !w.resets_at ? "" : w.resets_at * 1000 <= Date.now()
        ? " · 초기화 시각 지남 · 새 관측 필요"
        : " · " + fmtReset(w.resets_at) + " 초기화";
      row.appendChild(el("b", "", "남음 " + Math.round(remaining[i]) + "%" + resetText));
      group.appendChild(row);
    }
    if (unsupported) {
      group.appendChild(el("p", "small-note muted", "한도·잔액 조회는 지원하지 않습니다. 연결한 공급사의 관리 화면에서 확인하세요."));
    } else if (source) {
      const observed = source.observed_at ? "관측 " + new Date(source.observed_at).toLocaleString("ko-KR") : "관측 시각 없음";
      group.appendChild(el("p", "small-note muted", observed + (source.refresh_kind === "vendor" ? " · 공급사 조회" : " · 저장된 기록")));
      if (source.note) group.appendChild(el("p", "small-note muted", source.note));
      if (source.error) group.appendChild(el("p", "small-note warn", "갱신하지 못했습니다: " + source.error));
    }
    detail.appendChild(group);
  }
  if (!selected.length) strip.appendChild(el("p", "usage-empty muted", state.room ? "이 방에 공급사를 추가하면 사용량을 보여줘." : "방을 선택하면 해당 공급사의 사용량을 보여줘."));
  const totals = participants.reduce((sum, p) => {
    const t = p.totals || {};
    sum.input += Number(t.input || 0); sum.output += Number(t.output || 0); sum.cost += Number(t.cost_usd || 0);
    return sum;
  }, { input: 0, output: 0, cost: 0 });
  if (state.room) detail.appendChild(el("p", "small-note muted", "이 방 누적 · 입력 " + fmtK(totals.input) + " · 출력 " + fmtK(totals.output) + (totals.cost ? " · 보고된 비용 $" + totals.cost.toFixed(3) : "")));
  detail.appendChild(el("p", "small-note muted", "이 방에 추가한 공급사만 화면을 보는 동안 1분마다 갱신해. 참가자를 꺼도 사용량은 남아 있어. 공급사마다 조회 방식과 관측 시각은 달라."));
  $("#btn-refresh-usage").disabled = state.usageBusy || !selected.length;
  $("#usage-status").textContent = !selected.length ? "" : state.usageBusy ? "갱신 중…" : state.usageError ? state.usageError : state.usageChecked ? "확인 " + fmtTime(state.usageChecked) : "";
}
