package room

import "context"

// Shutdown stops room-owned CLI invocations and waits for their final messages
// to be persisted. Interactive CLI windows opened by the user are independent.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	if !h.closing {
		h.closing = true
		for _, rs := range h.rooms {
			rs.stopLocked("서버 종료")
			for ch := range rs.subs {
				close(ch)
			}
			rs.subs = map[chan []byte]struct{}{}
		}
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.turnWG.Wait(); close(done) }()
	select {
	case <-done:
		return h.waitLimits(ctx)
	case <-ctx.Done():
		_ = h.waitLimits(ctx) // cancel usage RPCs even when a CLI did not drain in time
		return ctx.Err()
	}
}
