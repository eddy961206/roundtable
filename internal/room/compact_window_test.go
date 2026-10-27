package room

import (
	"strings"
	"testing"
)

func TestCompactWindowRoomSettingReachesRuns(t *testing.T) {
	h, r, ps := controlledHub(t, "claude")
	for _, bad := range []int{-1, 1000, 2_000_000} {
		if _, err := h.UpdateRoom(r.ID, RoomPatch{CompactWindowTokens: &bad}); err == nil || !strings.Contains(err.Error(), "compact_window_tokens") {
			t.Fatalf("%d: %v", bad, err)
		}
	}
	w := 150000
	if _, err := h.UpdateRoom(r.ID, RoomPatch{CompactWindowTokens: &w}); err != nil {
		t.Fatal(err)
	}
	postQueueMessage(t, h, r.ID, "작업", "claude")
	call := nextCall(t, ps[0])
	if call.opts.CompactWindow != 150000 {
		t.Fatalf("run options: %d", call.opts.CompactWindow)
	}
	finishCall(call, "ok")
	waitIdle(t, h, r.ID)
}
