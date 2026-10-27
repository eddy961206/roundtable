package room

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Shutdown stops room-owned CLI invocations and waits for their final messages
// to be persisted. Interactive CLI windows opened by the user are independent.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	stopErr := h.closeLocked()
	h.mu.Unlock()
	return h.drain(ctx, stopErr)
}

// CloseWhenIdle closes the hub at the first moment no room runs or queues a
// turn or a work step. The check and the close share one h.mu hold, so a post
// is either part of the work waited for or refused as during shutdown. When
// ctx ends first the hub keeps serving (closed false). A closed hub may still
// report an error from persisting the stop notes.
func (h *Hub) CloseWhenIdle(ctx context.Context) (closed bool, err error) {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		h.mu.Lock()
		if h.closing {
			h.mu.Unlock()
			return false, fmt.Errorf("서버가 이미 종료 중이야")
		}
		if h.idleLocked() {
			stopErr := h.closeLocked()
			h.mu.Unlock()
			return true, h.drain(context.WithoutCancel(ctx), stopErr)
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("작업이 끝나지 않아 종료하지 않음: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func (h *Hub) idleLocked() bool {
	for _, rs := range h.rooms {
		if len(rs.running) > 0 || len(rs.queue) > 0 {
			return false
		}
		for _, busy := range rs.workBusy {
			if busy {
				return false
			}
		}
	}
	return true
}

// closeLocked refuses new work and cancels running turns. Caller holds h.mu.
func (h *Hub) closeLocked() error {
	if h.closing {
		return nil
	}
	h.closing = true
	var stopErr error
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
	return stopErr
}

func (h *Hub) drain(ctx context.Context, stopErr error) error {
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
