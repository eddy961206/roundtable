// Client state and lookups derived from it (participants, seat colours, model labels).
import { LABEL } from "./util.js";

// Each seat gets its own colour (8 slots, by position in the room) so a long
// reply can be attributed at a glance even when two seats share a provider.
const SEAT_COLORS = 8;
export function seatClass(pid) {
  const ps = (state.room && state.room.participants) || [];
  const i = ps.findIndex((p) => p.id === pid);
  return i < 0 ? "" : " seat-" + (i % SEAT_COLORS);
}
// Recent and average response time of one seat, from the messages loaded in this view.
export function seatTimes(pid) {
  const ds = [];
  for (const id of state.order) {
    const m = state.messages.get(id);
    if (m && m.kind === "agent" && m.author === pid && m.status === "done" && m.duration_ms > 0) ds.push(m.duration_ms);
  }
  if (!ds.length) return null;
  return { last: ds[ds.length - 1], avg: ds.reduce((a, b) => a + b, 0) / ds.length, n: ds.length };
}
export const state = {
  providers: [],
  limits: {},
  config: {},
  rooms: [],
  roomSearch: "",
  roomId: null,
  room: null,
  work: null,
  workRevision: 0,
  partNotices: new Map(),
  partReviewOpen: new Map(),
  workBoardCollapsed: false,
  diffPartId: null,
  diffData: null,
  diffLoading: false,
  diffError: "",
  diffController: null,
  messages: new Map(),
  order: [],
  winStart: 0,
  winEnd: 0,
  requests: new Map(),
  decisions: new Map(), // decision cards of the open room, in recorded order
  draftAttachments: [],
  queue: [],
  discussion: null,
  traces: new Map(),
  live: new Map(), // msgId -> live progress of a streaming reply (subagents, tools)
  traceFetching: new Set(),
  traceErrors: new Map(),
  expanded: new Set(),
  userCollapsed: new Set(),
  autoTrace: localStorage.getItem("rt.autoTrace") === "1",
  leftCollapsed: localStorage.getItem("rt.leftCollapsed") === "1",
  rightCollapsed: localStorage.getItem("rt.rightCollapsed") === "1",
  target: "",
  mentionCands: [],
  mentionIdx: 0,
  es: null,
  roomsEs: null,
  renderTimers: new Map(),
  traceTimers: new Map(),
  lastEvent: 0,
  draftMentions: new Map(),
  usageSources: {},
  usageBusy: false,
  usageError: "",
  usageChecked: 0,
  history: { has_more: false, before_seq: 0, total: 0 },
  browsingHistory: false,
  historyLoading: false,
  latestSeq: 0,
  pendingMessages: 0,
  stickToBottom: true,
  unreadBelow: 0,
  traceMeta: new Map(),
  tracePending: new Map(),
  traceControllers: new Map(),
  catalogRevision: 0,
  roomStateRevision: 0,
};

export const activeParticipants = () => state.room ? state.room.participants.filter((p) => !p.removed) : [];
export const participant = (id) => state.room && state.room.participants.find((p) => p.id === id);
let namesRoom = null;
let participantNames = new Map();
export const participantLabel = (id) => {
  const p = participant(id);
  if (!p) return LABEL[id] || id;
  if (namesRoom !== state.room) {
    const reserved = new Set(state.room.participants.map((seat) => seat.label));
    const counts = new Map();
    for (const seat of state.room.participants) counts.set(seat.label, (counts.get(seat.label) || 0) + 1);
    participantNames = new Map();
    for (const seat of state.room.participants) {
      let name = seat.label;
      if (counts.get(name) > 1) {
        let suffix = 1;
        while (reserved.has(name + " · " + suffix)) suffix++;
        name += " · " + suffix;
        reserved.add(name);
      }
      participantNames.set(seat.id, name);
    }
    namesRoom = state.room;
  }
  return participantNames.get(id);
};
export function resetParticipantNames() {
  namesRoom = null;
}
export function modelLabel(provider, model) {
  const info = state.providers.find((p) => p.id === provider);
  const option = info && info.models && info.models.find((m) => m.id === model);
  // A model outside the catalog is shown by its ID so two custom seats stay distinguishable.
  return option ? option.label : model ? model + " (직접 지정)" : "직접 지정 모델";
}
export const authorProvider = (id) => {
  const p = participant(id);
  return p ? p.provider : id;
};

export let openSeq = 0;
// Bumped by openRoom; async handlers compare it to drop results for a room they left.
export function nextOpenSeq() {
  return ++openSeq;
}
