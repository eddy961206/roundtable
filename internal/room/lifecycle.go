package room

import (
	"context"
	"errors"
)

// Shutdown stops room-owned CLI invocations and waits for their final messages
// to be persisted. Interactive CLI windows opened by the user are independent.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	var stopErr error
	if !h.closing {
		h.closing = true
		for _, rs := range h.rooms {
			stopErr = errors.Join(stopErr, h.stopLocked(rs, "서버 종료"))
			for ch := range rs.subs {
				close(ch)
			}
			rs.subs = map[chan []byte]struct{}{}
		}
		for ch := range h.listSubs {
			delete(h.listSubs, ch)
			close(ch)
		}
	}
	h.mu.Unlock()
	done := make(chan struct{})
	go func() { h.turnWG.Wait(); close(done) }()
	select {
	case <-done:
		return errors.Join(stopErr, h.waitLimits(ctx))
	case <-ctx.Done():
		_ = h.waitLimits(ctx) // cancel usage RPCs even when a CLI did not drain in time
		return errors.Join(stopErr, ctx.Err())
	}
}
