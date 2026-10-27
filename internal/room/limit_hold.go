package room

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A turn that failed on a provider limit (429, rate/usage/free limit, quota)
// holds only its seat until it may retry, so queued turns do not fail the same
// way one after another (docs/다중에이전트_도구조사_20261027.md item 6). The
// account is not identified: other seats of the same account keep running.
const (
	defaultLimitHold = 10 * time.Minute // when the error names no retry time
	maxLimitHold     = 6 * time.Hour
)

var (
	limitRe      = regexp.MustCompile(`(?i)(\b429\b|rate[ _-]?limit|too many requests|usage limit|free[ _-]?limit|quota|resource[ _-]?exhausted|limit reached)`)
	retryInRe    = regexp.MustCompile(`(?i)(?:try again|retry)(?:\s+(?:after|in))?\s*(\d+)\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h)\b`)
	retryAfterRe = regexp.MustCompile(`(?i)retry[-_ ]after[:=]?\s*(\d+)\b`) // seconds, as the HTTP header
)

// classifyLimit tells whether an error is a provider limit and until when the
// seat should wait; estimated means the error named no time.
func classifyLimit(text string, now time.Time) (hold bool, until time.Time, reason string, estimated bool) {
	if !limitRe.MatchString(text) {
		return false, time.Time{}, "", false
	}
	d, estimated := defaultLimitHold, true
	if m := retryInRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		unit := time.Second
		switch strings.ToLower(m[2])[0] {
		case 'm':
			unit = time.Minute
		case 'h':
			unit = time.Hour
		}
		d, estimated = time.Duration(n)*unit, false
	} else if m := retryAfterRe.FindStringSubmatch(text); m != nil {
		n, _ := strconv.Atoi(m[1])
		d, estimated = time.Duration(n)*time.Second, false
	}
	d = min(d, maxLimitHold)
	return true, now.Add(d), clipRunes(strings.TrimSpace(text), 300), estimated
}

// holdSeat records the hold, announces it and wakes the queue when it ends.
// Caller holds h.mu.
func (h *Hub) holdSeat(rs *roomState, p *Participant, until time.Time, reason string, estimated bool) {
	p.HoldUntil, p.HoldReason = until.UnixMilli(), reason
	when := until.Local().Format("15:04:05 MST")
	if estimated {
		when += "(오류에 재시도 시각이 없어 " + defaultLimitHold.String() + " 뒤로 추정)"
	}
	h.addSystem(rs, fmt.Sprintf("@%s 한도 오류로 %s까지 이 좌석의 새 턴을 보류함 · 원인: %s · 다른 좌석은 계속 실행. 이 좌석을 대상으로 지정해 보내면 바로 다시 시도", p.ID, when, reason))
	time.AfterFunc(time.Until(until)+50*time.Millisecond, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if !h.closing && h.rooms[rs.room.ID] == rs {
			h.pump(rs)
		}
	})
}

// seatHeld clears an ended hold and tells whether the seat must still wait.
func seatHeld(p *Participant) bool {
	if p.HoldUntil == 0 {
		return false
	}
	if nowMs() < p.HoldUntil {
		return true
	}
	p.HoldUntil, p.HoldReason = 0, ""
	return false
}
