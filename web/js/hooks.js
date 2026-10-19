// Calls that point up the module graph, to timeline or room code. main.js fills
// them before boot so lower modules can reach them without an import cycle.
export const hooks = {
  openRoom: null,
  closeRoom: null,
  renderMessage: null,
  scheduleRender: null,
  syncPendingScrollRestore: null,
  onTimelineImageLoad: null,
  focusTimelineMessage: null,
  upsertRequest: null,
};
