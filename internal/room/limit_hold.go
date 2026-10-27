package room

import (
	"fmt"
	"math"
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
	minLimitHold     = time.Second      // floor for "retry-after: 0" and sub-second waits
	maxLimitHold     = 6 * time.Hour
)

var (
	// A bare 429 is too common (file:line, pids); it counts only next to an
	// HTTP/status word. "limit reached" counts only for usage-type limits:
	// context, token or turn limits do not lift with time.
	limitRe = regexp.MustCompile(`(?i)((http|status|code|error|api)\W{0,12}429\b|\b429\W{0,12}(too many|rate|limit|quota)|rate[ _-]?limit|too many requests|(usage|request|free|daily|weekly|monthly|plan|api)[ _-]?limit|quota|resource[ _-]?exhausted|사용 ?한도|요청 ?한도)`)
	// Where a retry time is named: "try again in 2m30s", "retry after 1.5s",
	// "retry-after: 30" (seconds), "3분 후 다시". "retry 1 failed" is a count.
	retryEnRe      = regexp.MustCompile(`(?i)(?:try again|retry)(?:[-_ ]?(?:after|in)\s*[:=]?|\s*[:=])\s*([0-9][0-9a-z.\s]*)`)
	retryKoRe      = regexp.MustCompile(`([0-9][0-9.\s]*(?:시간|분|초)(?:\s*[0-9.]+\s*(?:시간|분|초))*)\s*(?:뒤|후)`)
	durationPartRe = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*(ms|milliseconds?|millis|seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h|시간|분|초)?`)
)

// parseWait reads "2m30s", "1.5 s", "500ms", "3분", "1시간 30분" or a bare number
// (seconds). ok is false when nothing numeric leads the text.
func parseWait(s string) (time.Duration, bool) {
	total, found := 0.0, false
	for {
		m := durationPartRe.FindStringSubmatchIndex(s)
		if m == nil || m[1] == 0 {
			break
		}
		n, _ := strconv.ParseFloat(s[m[2]:m[3]], 64)
		unit, end := "", m[1]
		if m[4] >= 0 {
			unit = strings.ToLower(s[m[4]:m[5]])
			// "30 model" or "30\nmessage": a letter right after an English unit
			// means it was the start of a word, so the number stands alone.
			if next := m[5]; next < len(s) && isASCIILetter(s[next]) && !strings.ContainsAny(unit, "시간분초") {
				unit, end = "", m[3]
			}
		}
		if unit == "" && found {
			break // "2 minutes 429": the bare number is not part of the duration
		}
		mult := 1.0 // a bare number is seconds, as in Retry-After
		switch {
		case unit == "ms" || strings.HasPrefix(unit, "milli"):
			mult = 0.001
		case unit == "m" || strings.HasPrefix(unit, "min") || unit == "분":
			mult = 60
		case unit == "h" || strings.HasPrefix(unit, "h") || unit == "시간":
			mult = 3600
		}
		total += n * mult
		found = true
		s = s[end:]
		if unit == "" {
			break // a bare number ends the duration
		}
	}
	if !found {
		return 0, false
	}
	if total*float64(time.Second) > float64(maxLimitHold) || math.IsInf(total, 0) {
		return maxLimitHold, true
	}
	return time.Duration(total * float64(time.Second)), true
}

func isASCIILetter(c byte) bool { return 'a' <= c|0x20 && c|0x20 <= 'z' }

// classifyLimit tells whether an error is a provider limit and until when the
// seat should wait; estimated means the error named no time.
func classifyLimit(text string, now time.Time) (hold bool, until time.Time, reason string, estimated bool) {
	if !limitRe.MatchString(text) {
		return false, time.Time{}, "", false
	}
	d, estimated := defaultLimitHold, true
	if m := retryEnRe.FindStringSubmatch(text); m != nil {
		if w, ok := parseWait(m[1]); ok {
			d, estimated = w, false
		}
	}
	if m := retryKoRe.FindStringSubmatch(text); estimated && m != nil {
		if w, ok := parseWait(m[1]); ok {
			d, estimated = w, false
		}
	}
	d = min(max(d, minLimitHold), maxLimitHold)
	return true, now.Add(d), clipRunes(strings.TrimSpace(text), 300), estimated
}

// holdSeat records the hold, announces it and wakes the room when it ends.
// Caller holds h.mu.
func (h *Hub) holdSeat(rs *roomState, p *Participant, until time.Time, reason string, estimated bool) {
	p.HoldUntil, p.HoldReason = until.UnixMilli(), reason
	when := until.Local().Format("15:04:05 MST")
	if estimated {
		when += "(오류에 재시도 시각이 없어 " + defaultLimitHold.String() + " 뒤로 추정)"
	}
	h.addSystem(rs, fmt.Sprintf("@%s 한도 오류로 %s까지 이 좌석의 새 턴을 보류함 · 원인: %s · 다른 좌석은 계속 실행. 이 좌석을 대상으로 지정해 보내면 바로 다시 시도", p.ID, when, reason))
	h.armHold(rs, until)
}

// armHold clears ended holds and pumps the room at until. Caller holds h.mu.
func (h *Hub) armHold(rs *roomState, until time.Time) {
	time.AfterFunc(time.Until(until)+50*time.Millisecond, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.closing || h.rooms[rs.room.ID] != rs {
			return
		}
		h.releaseHolds(rs)
		h.pump(rs)
	})
}

// releaseHolds clears every ended hold, waiting turns or not, so the room no
// longer reports it. Caller holds h.mu.
func (h *Hub) releaseHolds(rs *roomState) {
	changed := false
	for _, p := range rs.room.Participants {
		if p.HoldUntil != 0 && nowMs() >= p.HoldUntil {
			p.HoldUntil, p.HoldReason = 0, ""
			rs.broadcast("participant", p)
			changed = true
		}
	}
	if changed {
		h.saveRoom(rs)
	}
}

// restoreHolds runs on start once rs is registered: ended holds are cleared,
// live ones get their timer back (timers do not survive a restart). It reports
// whether the room changed and must be saved. Caller holds h.mu.
func (h *Hub) restoreHolds(rs *roomState) bool {
	changed := false
	for _, p := range rs.room.Participants {
		switch {
		case p.HoldUntil == 0:
		case nowMs() >= p.HoldUntil:
			p.HoldUntil, p.HoldReason = 0, ""
			changed = true
		default:
			h.armHold(rs, time.UnixMilli(p.HoldUntil))
		}
	}
	return changed
}

// seatHeld tells whether the seat must still wait; pump has already released
// ended holds.
func seatHeld(p *Participant) bool {
	return p.HoldUntil != 0 && nowMs() < p.HoldUntil
}
