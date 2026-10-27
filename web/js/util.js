// Shared constants, DOM helpers, formatters, provider labels and the live announcer.

export const DOM_CAP_STEP = 200;
export const STREAM_THROTTLE_MS = 125; // max 8 renders/sec per message/trace
export const TRACE_EVENT_LIMIT = 200;
export const TRACE_CACHE_LIMIT = 8;
export const MAX_ATTACHMENTS = 4;
export const MAX_ATTACHMENT_BYTES = 10 * 1024 * 1024;
export const ALLOWED_IMAGE_MIMES = new Set(["image/png", "image/jpeg", "image/webp"]);

export const $ = (sel, root = document) => root.querySelector(sel);
export const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
export const el = (tag, cls, text) => {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  return n;
};
export const esc = (s) =>
  String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
export const fmtK = (n) => {
  n = Number(n) || 0;
  return n >= 1e6 ? (n / 1e6).toFixed(2) + "M" : n >= 1000 ? (n / 1000).toFixed(1) + "k" : String(Math.round(n));
};
export const fmtBytes = (bytes) => {
  const n = Number(bytes);
  if (!Number.isFinite(n) || n < 0) return "0 B";
  if (n < 1024) return Math.round(n) + " B";
  if (n < 1024 * 1024) return (n / 1024).toFixed(1) + " KB";
  return (n / (1024 * 1024)).toFixed(2) + " MB";
};
export const fmtTime = (ms) =>
  ms ? new Date(ms).toLocaleTimeString("ko-KR", { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "";
export const fmtDur = (ms) => (ms >= 60000 ? (ms / 60000).toFixed(1) + "분" : (ms / 1000).toFixed(1) + "초");
export const fmtElapsed = (ms) => {
  const sec = Math.max(0, Math.floor((Number(ms) || 0) / 1000));
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  return m > 0 ? m + "분 " + String(s).padStart(2, "0") + "초" : s + "초";
};
export const fmtReset = (sec) => {
  if (!sec) return "";
  const diff = sec * 1000 - Date.now();
  if (diff <= 0) return "곧 리셋";
  const mins = Math.round(diff / 60000);
  if (mins < 60) return mins + "분 후";
  const h = Math.round(mins / 60);
  return h >= 24 ? Math.round(h / 24) + "일 후" : h + "시간 후";
};

export const LABEL = { claude: "Claude", codex: "Codex", agy: "Antigravity", opencode: "OpenCode", user: "나", system: "시스템" };
const PROVIDER_ICONS = Object.freeze({
  claude: "claude.svg",
  codex: "codex.svg",
  agy: "gemini.svg",
});

export function avatarContent(provider, label) {
  const icon = Object.hasOwn(PROVIDER_ICONS, provider) ? PROVIDER_ICONS[provider] : null;
  return icon
    ? '<img src="vendor/provider-icons/' + icon + '" alt="" width="22" height="22" draggable="false">'
    : esc(label[0]);
}

export function announce(msg) {
  const box = $("#live-announcer");
  if (!box || !msg) return;
  box.textContent = "";
  setTimeout(() => {
    box.textContent = msg;
  }, 20);
}

// In-page stand-ins for window.confirm/alert. Embedded browsers such as app panes can suppress
// native dialogs (confirm returns false at once), which silently cancelled deletes and hid errors.
// The answer is taken from the button click or Esc itself: the dialog "close" event waits for the
// next rendered frame, so it never arrives while the page is hidden.
function messageDialog(message, okText, cancelText, danger) {
  return new Promise((resolve) => {
    const dlg = el("dialog", "dlg-message");
    dlg.setAttribute("aria-label", cancelText ? "확인" : "알림");
    const text = el("p", "dlg-message-text", message);
    const menu = el("menu");
    const finish = (answer) => {
      if (!dlg.isConnected) return;
      dlg.close();
      dlg.remove();
      resolve(answer);
    };
    let cancel = null;
    if (cancelText) {
      cancel = el("button", "", cancelText);
      cancel.type = "button";
      cancel.onclick = () => finish(false);
      menu.appendChild(cancel);
    }
    const ok = el("button", danger ? "danger" : "primary", okText);
    ok.type = "button";
    ok.onclick = () => finish(true);
    menu.appendChild(ok);
    dlg.append(text, menu);
    dlg.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      // Esc answers only this dialog, not the page-wide Escape handling behind it.
      e.preventDefault();
      e.stopPropagation();
      finish(false);
    });
    document.body.appendChild(dlg);
    dlg.showModal();
    // Destructive questions start on cancel so a stray Enter does not confirm them.
    (danger && cancel ? cancel : ok).focus();
  });
}

export const askConfirm = (message, { okText = "확인", danger = false } = {}) => messageDialog(message, okText, "취소", danger);
export const showAlert = (message) => messageDialog(String(message ?? ""), "확인", "", false);
