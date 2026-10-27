// Room-list marks: a spinner while a room runs, a dot when it has messages added
// since this browser last had it open. The only input is the message count the
// room list already carries; what was seen lives in localStorage, per browser.

const KEY = "rt.seenMessages";

// createUnreadTracker keeps the seen counts in memory and copies them to storage
// when it can; a storage that is missing or throws only loses them on reload.
export function createUnreadTracker(storage) {
  let seen = null;
  const load = () => {
    if (seen) return seen;
    seen = {};
    try {
      const v = JSON.parse(storage.getItem(KEY) || "{}");
      if (v && typeof v === "object" && !Array.isArray(v)) {
        for (const [id, n] of Object.entries(v)) if (Number.isInteger(n) && n >= 0) seen[id] = n;
      }
    } catch { /* no storage, unreadable or corrupt: start over */ }
    return seen;
  };
  const save = () => {
    try { storage.setItem(KEY, JSON.stringify(seen)); } catch { /* no storage, quota or private mode */ }
  };

  // marks returns {roomId: "active" | "unread" | ""} for every listed room. The open
  // room and a room seen for the first time count as read up to their current
  // count; rooms no longer listed are forgotten.
  return {
    marks(rooms, openId) {
      const prev = load();
      const next = {};
      const out = {};
      for (const r of rooms) {
        let n = prev[r.id];
        if (n === undefined || r.id === openId || n > r.messages) n = r.messages;
        next[r.id] = n;
        out[r.id] = r.active ? "active" : r.messages > n ? "unread" : "";
      }
      const changed = Object.keys(next).length !== Object.keys(prev).length || Object.keys(next).some((id) => next[id] !== prev[id]);
      seen = next;
      if (changed) save();
      return out;
    },
  };
}
