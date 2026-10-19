// Theme, responsive drawers and desktop panel collapse.
import { $, $$, announce } from "./util.js";
import { state } from "./state.js";

// ---------- theme ----------
export function initTheme() {
  const saved = localStorage.getItem("rt.theme");
  if (saved === "light" || saved === "dark") {
    document.documentElement.dataset.theme = saved;
  }
  updateThemeBtn();
}

function currentTheme() {
  const explicit = document.documentElement.dataset.theme;
  if (explicit === "light" || explicit === "dark") return explicit;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}

export function toggleTheme() {
  const next = currentTheme() === "dark" ? "light" : "dark";
  document.documentElement.dataset.theme = next;
  localStorage.setItem("rt.theme", next);
  updateThemeBtn();
  announce(next === "light" ? "라이트 테마로 전환됨" : "다크 테마로 전환됨");
}

function updateThemeBtn() {
  const btn = $("#btn-theme");
  if (!btn) return;
  const isLight = currentTheme() === "light";
  btn.textContent = isLight ? "☀️" : "🌙";
  btn.setAttribute("aria-label", isLight ? "다크 테마로 전환" : "라이트 테마로 전환");
  btn.title = isLight ? "현재 라이트 테마 (클릭하여 다크 테마로 전환)" : "현재 다크 테마 (클릭하여 라이트 테마로 전환)";
}

let drawerReturnFocus = null;
export function closeDrawers() {
  document.body.classList.remove("show-left", "show-right");
  const bd = $("#drawer-backdrop");
  if (bd) bd.hidden = true;
  const target = drawerReturnFocus;
  drawerReturnFocus = null;
  if (target && target.offsetParent) target.focus();
}

export function closeMoreMenu() {
  const more = $("#more-actions");
  if (more && more.open) more.open = false;
}

export function toggleDrawer(side) {
  const cls = side === "left" ? "show-left" : "show-right";
  const other = side === "left" ? "show-right" : "show-left";
  document.body.classList.remove(other);
  document.body.classList.toggle(cls);
  const bd = $("#drawer-backdrop");
  if (bd) {
    bd.hidden = !document.body.classList.contains("show-left") && !document.body.classList.contains("show-right");
  }
}

// Desktop panel collapse. Narrow screens keep the drawer behaviour, so the
// usage panel only moves into the chat area while the sidebar is a grid column.
const sidebarColumn = window.matchMedia("(min-width: 761px)");
function applyPanelLayout() {
  document.body.classList.toggle("left-collapsed", state.leftCollapsed);
  document.body.classList.toggle("right-collapsed", state.rightCollapsed);
  const panel = $("#provider-panel");
  const dock = $("#usage-dock");
  const docked = state.leftCollapsed && sidebarColumn.matches;
  const home = docked ? dock : $("#sidebar");
  if (panel.parentElement !== home) home.appendChild(panel);
  dock.hidden = !docked;
  $$(".panel-collapse-left, .panel-expand-left").forEach((b) => b.setAttribute("aria-expanded", String(!state.leftCollapsed)));
  $$(".panel-collapse-right, .panel-expand-right").forEach((b) => b.setAttribute("aria-expanded", String(!state.rightCollapsed)));
}

function setPanelCollapsed(side, collapsed) {
  const key = side === "left" ? "leftCollapsed" : "rightCollapsed";
  state[key] = collapsed;
  localStorage.setItem("rt." + key, collapsed ? "1" : "0");
  applyPanelLayout();
  // Keep focus on a visible control for the same panel.
  const next = collapsed
    ? $$(".panel-expand-" + side).find((b) => b.offsetParent)
    : $(side === "left" ? "#btn-collapse-left" : "#btn-collapse-right");
  if (next && next.offsetParent) next.focus();
}

export function bindPanels() {
  $("#btn-collapse-left").onclick = () => setPanelCollapsed("left", true);
  $("#btn-collapse-right").onclick = () => setPanelCollapsed("right", true);
  $$(".panel-expand-left").forEach((b) => (b.onclick = () => setPanelCollapsed("left", false)));
  $$(".panel-expand-right").forEach((b) => (b.onclick = () => setPanelCollapsed("right", false)));
  sidebarColumn.addEventListener("change", applyPanelLayout);
  applyPanelLayout();
  $("#btn-usage").onclick = () => {
    drawerReturnFocus = $("#btn-usage");
    document.body.classList.remove("show-right");
    document.body.classList.add("show-left");
    $("#drawer-backdrop").hidden = false;
    $("#provider-panel").scrollTop = 0;
    $("#usage-title").focus();
  };
}
