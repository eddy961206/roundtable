// Entry point: boot, background timers and UI event wiring.
import { $, $$, announce, esc, fmtElapsed } from "./util.js";
import { api } from "./api.js";
import { hooks } from "./hooks.js";
import { activeParticipants, openSeq, state } from "./state.js";
import { bindPanels, closeDrawers, closeMoreMenu, initTheme, toggleDrawer, toggleTheme } from "./layout.js";
import { refreshLimits, renderProviderStatus, renderUsagePanel, scheduleUsageRefresh, usageTimer } from "./usage.js";
import { addAttachmentFiles, clearComposerError, clearDraftAttachments, closeLightbox, extractClipboardImageFiles, renderRequests } from "./attachments.js";
import { applyMention, closeMentionPop, mentionPopup, renderMentionButtons, send, setTarget, updateHint } from "./composer.js";
import { closeDiffDrawer } from "./work.js";
import { loadRooms, renderRoomHead, renderRoomList } from "./rooms.js";
import { getAgentElapsedMs, renderParticipants } from "./participants.js";
import { ensureAndRenderTrace, expandTrace } from "./trace.js";
import { bindTimeline, focusTimelineMessage, jumpToLatestMessages, onTimelineImageLoad, renderMessage, renderTimeline, scheduleRender, scrollToTimelineBottom, syncPendingScrollRestore } from "./timeline.js";
import { connect, openRoom, schedulePanels, upsertRequest } from "./room.js";
import { addPartRow, addSeatSpec, addVerifyRow, onRoomSubmit, onWorkPlanSubmit, openRoomDialog, openWorkPlanDialog, readSeatSpecs } from "./dialogs.js";

// ---------- boot ----------
async function boot() {
  initTheme();
  const autoChk = $("#chk-auto-trace");
  if (autoChk) autoChk.checked = state.autoTrace;

  const [prov, cfg] = await Promise.all([api("GET", "/api/providers"), api("GET", "/api/config")]);
  state.providers = prov.providers || [];
  state.limits = prov.limits || {};
  state.config = cfg || {};
  renderProviderStatus();
  renderUsagePanel();
  await loadRooms();
  const last = localStorage.getItem("rt.room");
  if (last && state.rooms.some((r) => r.id === last)) {
    await openRoom(last);
  }
  bindUI();
  resumeBackground();
  document.addEventListener("visibilitychange", () => {
    stopBackground();
    if (!document.hidden) {
      resumeBackground();
      refreshLimits();
      if (state.room) { renderTimeline(false); schedulePanels(); }
    }
  });
  window.addEventListener("pagehide", () => { stopBackground(); if (state.es) state.es.close(); });
}

let watchTimer = null;
let elapsedTimer = null;
function stopBackground() {
  clearTimeout(usageTimer); clearInterval(watchTimer); clearInterval(elapsedTimer);
  for (const controller of state.traceControllers.values()) controller.abort();
}
function resumeBackground() {
  if (document.hidden) return;
  watchTimer = setInterval(watchdog, 5000);
  elapsedTimer = setInterval(tickElapsed, 1000);
  scheduleUsageRefresh();
}
function watchdog() {
  if (!state.roomId || document.hidden) return;
  if (state.es && state.es.readyState === 2) {
    connect(state.roomId);
    openRoom(state.roomId, true);
    return;
  }
  const streaming = activeParticipants().some((p) => p.status === "running");
  if (streaming && Date.now() - (state.lastEvent || 0) > 30000) {
    state.lastEvent = Date.now();
    openRoom(state.roomId, true);
  }
}

function tickElapsed() {
  if (!state.roomId || !state.room || document.hidden) return;
  $$("[data-msg-elapsed]").forEach((node) => {
    const id = node.getAttribute("data-msg-elapsed");
    const m = state.messages.get(id);
    if (m && m.status === "streaming") {
      node.textContent = "⏱ " + fmtElapsed(Date.now() - (m.ts || Date.now()));
    }
  });
  $$("[data-agent-elapsed]").forEach((node) => {
    const pid = node.getAttribute("data-agent-elapsed");
    const ms = getAgentElapsedMs(pid);
    if (ms != null) {
      node.textContent = "⏱ " + fmtElapsed(ms);
    }
  });
}

function bindUI() {
  bindPanels();
  let participantDialogSequence = 0;
  $("#btn-refresh-usage").onclick = () => refreshLimits(true);
  $("#btn-latest-history").onclick = jumpToLatestMessages;
  const jumpBottomBtn = $("#btn-jump-bottom");
  if (jumpBottomBtn) {
    jumpBottomBtn.onclick = () => scrollToTimelineBottom();
  }
  bindTimeline();
  $("#btn-theme").onclick = toggleTheme;
  $("#btn-new-room").onclick = () => openRoomDialog();
  const onbNew = $("#btn-onboard-new");
  if (onbNew) onbNew.onclick = () => openRoomDialog();

  $$(".prompt-example").forEach((btn) => {
    btn.onclick = () => {
      const promptText = btn.getAttribute("data-prompt") || "";
      if (state.roomId && $("#input")) {
        $("#input").value = promptText;
        $("#input").focus();
        updateHint();
      } else {
        openRoomDialog(null, promptText);
      }
    };
  });

  const searchInput = $("#room-search");
  if (searchInput) {
    searchInput.addEventListener("input", () => {
      state.roomSearch = searchInput.value;
      renderRoomList();
    });
  }

  $("#dlg-room-cancel").onclick = () => $("#dlg-room").close();
  $("#form-room").onsubmit = onRoomSubmit;
  $("#btn-add-seat-spec").onclick = () => addSeatSpec($("#room-seat-specs"));
  $("#btn-copy-room-seats").onclick = () => {
    const box = $("#room-seat-specs");
    box.replaceChildren();
    for (const p of state.room.participants) addSeatSpec(box, p.provider, true, p);
  };

  const btnWorkPlan = $("#btn-work-plan");
  if (btnWorkPlan) {
    btnWorkPlan.onclick = () => {
      closeMoreMenu();
      openWorkPlanDialog();
    };
  }
  $("#btn-work-plan-head").onclick = openWorkPlanDialog;
  const dlgWorkCancel = $("#dlg-work-cancel");
  if (dlgWorkCancel) dlgWorkCancel.onclick = () => $("#dlg-work").close();
  // Esc must not hide a confirm still in flight: its result or error shows in this dialog.
  $("#dlg-work").addEventListener("cancel", (e) => {
    if ($("#form-work").getAttribute("aria-busy") === "true") e.preventDefault();
  });
  const btnAddVerify = $("#btn-add-verify");
  if (btnAddVerify) btnAddVerify.onclick = () => addVerifyRow($("#work-verify-rows"));
  const btnAddPart = $("#btn-add-part");
  if (btnAddPart) btnAddPart.onclick = () => addPartRow($("#work-part-rows"), true);
  const formWork = $("#form-work");
  if (formWork) formWork.onsubmit = onWorkPlanSubmit;

  $("#btn-add-participant").onclick = () => {
    participantDialogSequence++;
    $("#participant-spec").replaceChildren();
    addSeatSpec($("#participant-spec"), "", false);
    $('#form-participant [type="submit"]').disabled = false;
    $("#dlg-participant").showModal();
  };
  $("#dlg-participant").addEventListener("close", () => { participantDialogSequence++; });
  $("#dlg-participant-cancel").onclick = () => $("#dlg-participant").close();
  $("#form-participant").onsubmit = async (e) => {
    e.preventDefault();
    const button = e.target.querySelector('[type="submit"]');
    const roomId = state.roomId;
    const sequence = openSeq;
    const dialogSequence = participantDialogSequence;
    button.disabled = true;
    try {
      const [spec] = readSeatSpecs($("#participant-spec"));
      if (activeParticipants().length + spec.count > 32) throw new Error("한 방에는 최대 32명까지 참가할 수 있습니다.");
      const room = await api("POST", "/api/rooms/" + roomId + "/participants", spec);
      if (state.roomId !== roomId || openSeq !== sequence || participantDialogSequence !== dialogSequence) return;
      state.room = room;
      renderRoomHead();
      renderParticipants();
      $("#dlg-participant").close();
      announce(spec.count + "명 참가자 추가됨");
    } catch (err) {
      if (state.roomId === roomId && openSeq === sequence && participantDialogSequence === dialogSequence) alert(err.message);
    } finally {
      if (participantDialogSequence === dialogSequence) button.disabled = false;
    }
  };

  const lightboxCloseBtn = $("#lightbox-close");
  if (lightboxCloseBtn) {
    lightboxCloseBtn.onclick = () => closeLightbox();
  }
  const dlgLightbox = $("#dlg-lightbox");
  if (dlgLightbox) {
    dlgLightbox.addEventListener("click", (e) => {
      if (e.target === dlgLightbox) closeLightbox();
    });
    dlgLightbox.addEventListener("close", () => {
      const img = $("#lightbox-img");
      if (img) img.removeAttribute("src");
    });
  }

  const btnAttach = $("#btn-attach");
  const fileInput = $("#file-input");
  if (btnAttach && fileInput) {
    btnAttach.onclick = () => {
      if (!state.roomId) return;
      fileInput.click();
    };
    fileInput.addEventListener("change", () => {
      if (fileInput.files && fileInput.files.length > 0) {
        addAttachmentFiles(fileInput.files);
      }
      fileInput.value = "";
    });
  }

  const composerEl = $("#composer");
  if (composerEl) {
    composerEl.onsubmit = (e) => {
      e.preventDefault();
      send();
    };
    let dragDepth = 0;
    const hasFiles = (dt) => dt && dt.types && Array.from(dt.types).includes("Files");
    composerEl.addEventListener("dragenter", (e) => {
      if (!hasFiles(e.dataTransfer)) return;
      e.preventDefault();
      dragDepth++;
      composerEl.classList.add("drag-over");
    });
    composerEl.addEventListener("dragover", (e) => {
      if (!hasFiles(e.dataTransfer)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
      composerEl.classList.add("drag-over");
    });
    composerEl.addEventListener("dragleave", (e) => {
      if (!hasFiles(e.dataTransfer)) return;
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) composerEl.classList.remove("drag-over");
    });
    composerEl.addEventListener("drop", (e) => {
      if (!hasFiles(e.dataTransfer)) return;
      e.preventDefault();
      dragDepth = 0;
      composerEl.classList.remove("drag-over");
      if (e.dataTransfer.files && e.dataTransfer.files.length > 0) {
        addAttachmentFiles(e.dataTransfer.files);
      }
    });
  }

  const input = $("#input");
  input.addEventListener("paste", (e) => {
    const files = extractClipboardImageFiles(e.clipboardData);
    if (files.length > 0) {
      e.preventDefault();
      addAttachmentFiles(files);
    }
  });
  input.addEventListener("keydown", (e) => {
    const pop = $("#mention-pop");
    if (pop && !pop.hidden && state.mentionCands.length > 0) {
      if (e.key === "ArrowDown" || e.key === "ArrowRight") {
        e.preventDefault();
        state.mentionIdx = (state.mentionIdx + 1) % state.mentionCands.length;
        renderMentionButtons();
        announce(state.mentionCands[state.mentionIdx].label);
        return;
      }
      if (e.key === "ArrowUp" || e.key === "ArrowLeft") {
        e.preventDefault();
        state.mentionIdx = (state.mentionIdx - 1 + state.mentionCands.length) % state.mentionCands.length;
        renderMentionButtons();
        announce(state.mentionCands[state.mentionIdx].label);
        return;
      }
      if (e.key === "Tab" || (e.key === "Enter" && !e.shiftKey && !e.isComposing)) {
        e.preventDefault();
        applyMention(state.mentionCands[state.mentionIdx]);
        return;
      }
      if (e.key === "Escape") {
        e.preventDefault();
        closeMentionPop();
        return;
      }
    }
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send();
    } else if (e.key === "Escape" && state.target) {
      e.preventDefault();
      setTarget("", true);
    }
  });

  input.addEventListener("input", () => {
    clearComposerError();
    mentionPopup();
    updateHint();
  });

  $("#target").onchange = (e) => setTarget(e.target.value, false);

  const autoChk = $("#chk-auto-trace");
  if (autoChk) {
    autoChk.onchange = () => {
      state.autoTrace = autoChk.checked;
      localStorage.setItem("rt.autoTrace", state.autoTrace ? "1" : "0");
      if (state.autoTrace && state.room) {
        state.userCollapsed.clear();
        for (const id of state.order) {
          const m = state.messages.get(id);
          if (m && m.kind === "agent" && (m.status === "streaming" || m.trace > 0)) {
            expandTrace(id);
            if ($("#m-" + id)) ensureAndRenderTrace(id);
          }
        }
      }
    };
  }

  $("#btn-discuss").onclick = () =>
    api("POST", "/api/rooms/" + state.roomId + "/discussion", {
      rounds: Number($("#rounds").value) || 3,
    }).catch((e) => alert(e.message));
  $("#btn-stop").onclick = () =>
    state.roomId && api("POST", "/api/rooms/" + state.roomId + "/stop").catch((e) => alert(e.message));
  $("#btn-reset").onclick = () => {
    closeMoreMenu();
    if (
      state.roomId &&
      confirm("각 에이전트의 CLI 세션을 버릴까요? 다음 턴부터 대화 전체를 새로 읽습니다.")
    ) {
      api("POST", "/api/rooms/" + state.roomId + "/reset").catch((e) => alert(e.message));
    }
  };
  $("#btn-export").onclick = () => closeMoreMenu();
  $("#btn-settings").onclick = () => {
    closeMoreMenu();
    openRoomDialog(state.room);
  };

  // Close more-actions menu when clicking outside
  document.addEventListener("click", (e) => {
    const more = $("#more-actions");
    if (more && more.open && !more.contains(e.target)) {
      more.open = false;
    }
  });

  // Responsive drawers
  $("#btn-right").onclick = () => toggleDrawer("right");
  const btnLeft = $("#btn-left");
  if (btnLeft) btnLeft.onclick = () => toggleDrawer("left");
  const btnEmptyLeft = $("#btn-empty-left");
  if (btnEmptyLeft) btnEmptyLeft.onclick = () => toggleDrawer("left");
  const btnEmptyRight = $("#btn-empty-right");
  if (btnEmptyRight) btnEmptyRight.onclick = () => toggleDrawer("right");
  const btnCloseLeft = $("#btn-close-left");
  if (btnCloseLeft) btnCloseLeft.onclick = closeDrawers;
  const btnCloseRight = $("#btn-close-right");
  if (btnCloseRight) btnCloseRight.onclick = closeDrawers;
  const backdrop = $("#drawer-backdrop");
  if (backdrop) backdrop.onclick = closeDrawers;

  $("#btn-delete").onclick = async () => {
    closeMoreMenu();
    if (!confirm("방과 대화 기록을 영구 삭제할까요? 되돌릴 수 없습니다.")) return;
    await api("DELETE", "/api/rooms/" + state.roomId);
    state.roomId = null;
    state.room = null;
    state.work = null;
    state.requests.clear();
    closeLightbox();
    clearDraftAttachments();
    closeDiffDrawer();
    renderUsagePanel();
    renderRequests();
    $("#btn-add-participant").disabled = true;
    $("#participants").replaceChildren();
    localStorage.removeItem("rt.room");
    if (state.es) state.es.close();
    $("#room").hidden = true;
    $("#empty").hidden = false;
    loadRooms();
  };

  // Global keyboard shortcuts
  document.addEventListener("keydown", (e) => {
    if (document.querySelector("dialog[open]")) return;
    if ((e.ctrlKey || e.metaKey) && e.key === ".") {
      if (state.roomId) {
        e.preventDefault();
        api("POST", "/api/rooms/" + state.roomId + "/stop").catch((err) => alert(err.message));
        announce("모두 중지 요청됨");
      }
      return;
    }
    if (e.key === "Escape") {
      const pop = $("#mention-pop");
      if (pop && !pop.hidden) {
        closeMentionPop();
        return;
      }
      const more = $("#more-actions");
      if (more && more.open) {
        more.open = false;
        return;
      }
      const diffDrawer = $("#diff-drawer");
      if (diffDrawer && !diffDrawer.hidden) {
        closeDiffDrawer();
        return;
      }
      if (document.body.classList.contains("show-left") || document.body.classList.contains("show-right")) {
        closeDrawers();
        return;
      }
      if (state.target) {
        setTarget("", false);
        return;
      }
    }
    if (e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey) {
      const tag = (document.activeElement && document.activeElement.tagName) || "";
      if (tag !== "INPUT" && tag !== "TEXTAREA" && tag !== "SELECT" && !$("#room").hidden) {
        e.preventDefault();
        $("#input").focus();
      }
    }
  });
}

Object.assign(hooks, {
  openRoom,
  renderMessage,
  scheduleRender,
  syncPendingScrollRestore,
  onTimelineImageLoad,
  focusTimelineMessage,
  upsertRequest,
});

if (typeof document !== "undefined") {
  boot().catch((e) => {
    document.body.innerHTML = "<pre style='padding:20px'>시작 실패: " + esc(e.message) + "</pre>";
  });
}
