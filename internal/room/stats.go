package room

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
)

type ModelStatsRow struct {
	Provider              string         `json:"provider"`
	Model                 string         `json:"model"`
	Effort                string         `json:"effort"`
	Role                  string         `json:"role"`
	Turns                 int            `json:"turns"`
	Errors                int            `json:"errors"`
	MedianTotalMs         int64          `json:"median_total_ms"`
	MedianFirstOutputMs   int64          `json:"median_first_output_ms"`
	InputTokens           float64        `json:"input_tokens"`
	OutputTokens          float64        `json:"output_tokens"`
	CachedTokens          float64        `json:"cached_tokens"`
	PartsAuthored         int            `json:"parts_authored"`
	PartsPassedFirstRound int            `json:"parts_passed_first_round"`
	FindingsRaised        int            `json:"findings_raised"`
	FindingsFixed         int            `json:"findings_fixed"`
	FindingsWithdrawn     int            `json:"findings_withdrawn"`
	MarkerRejects         map[string]int `json:"marker_rejects"` // rejected marker lines by kind
	ReviewFormatErrors    int            `json:"review_format_errors"`
	LastSeenAt            int64          `json:"last_seen_at"`
}

type ModelStats struct {
	Rows []ModelStatsRow `json:"rows"`
	// Rejection notices recorded before notice_msg existed name only a seat,
	// whose model may have changed since; they are counted, not attributed.
	UnattributedMarkerRejects int   `json:"unattributed_marker_rejects"`
	GeneratedAt               int64 `json:"generated_at"`
}

type modelKey struct{ provider, model, effort, role string }
type modelSamples struct {
	ModelStatsRow
	total, first []int64
}

// ModelStats uses persisted room/message records. Each room captures its own
// append boundary; history reads and decoding hold neither the store nor hub lock.
func (h *Hub) ModelStats() (*ModelStats, error) {
	rooms, messages, _, err := h.store.loadRooms(false, false)
	if err != nil {
		return nil, err
	}
	groups := map[modelKey]*modelSamples{}
	unattributed := 0
	rowFor := func(r *Room, m *Message) *modelSamples {
		provider := m.Provider
		// Legacy messages omitted provider; a seat's provider never changes.
		if provider == "" {
			if p := r.participant(m.Author); p != nil {
				provider = p.Provider
			}
		}
		key := modelKey{provider, m.Model, m.Effort, m.Role}
		if groups[key] == nil {
			groups[key] = &modelSamples{ModelStatsRow: ModelStatsRow{Provider: provider, Model: m.Model, Effort: m.Effort, Role: m.Role, MarkerRejects: map[string]int{}}}
		}
		return groups[key]
	}
	for _, r := range rooms {
		msgs := messages[r.ID]
		byID := map[string]*Message{}
		for _, m := range msgs {
			byID[m.ID] = m
		}
		for _, m := range msgs {
			if m.Kind == "system" && m.NoticeFor != "" {
				if m.NoticeMsg == "" {
					unattributed++
					continue
				}
				source := byID[m.NoticeMsg]
				if source == nil {
					return nil, fmt.Errorf("방 %s 마커 거절 %s: 원인 응답 %s 없음", r.ID, m.ID, m.NoticeMsg)
				}
				rowFor(r, source).MarkerRejects[m.NoticeMarker]++
				continue
			}
			if m.Kind != "agent" || m.Status == "streaming" {
				continue
			}
			row := rowFor(r, m)
			row.Turns++
			if m.Status == "error" {
				row.Errors++
			}
			if m.ErrorKind == "review_format" {
				row.ReviewFormatErrors++
			}
			row.LastSeenAt = max(row.LastSeenAt, m.TS)
			if m.Timing != nil {
				// Interrupted turns can have no completed total-time measurement.
				if m.Timing.TotalMs > 0 {
					row.total = append(row.total, m.Timing.TotalMs)
				}
				// No output is a missing observation, not an instantaneous reply.
				if m.Trace > 0 || m.Timing.FirstOutputMs > 0 {
					row.first = append(row.first, m.Timing.FirstOutputMs)
				}
			}
			row.InputTokens += usageNumber(m.Usage, "input_tokens")
			row.OutputTokens += usageNumber(m.Usage, "output_tokens")
			row.CachedTokens += usageNumber(m.Usage, "cache_read_input_tokens") + usageNumber(m.Usage, "cached_input_tokens") + usageNumber(m.Usage, "cache_read_tokens")
		}
		if r.Work == nil {
			continue
		}
		for _, part := range r.Work.Parts {
			var author *Message
			var reviewMessages []*Message
			for _, m := range msgs {
				if m.Kind != "agent" || m.Part != part.ID || m.Status != "done" {
					continue
				}
				if m.Author == part.Seat && m.Reason != "review" && author == nil {
					author = m
				}
				if m.Author == part.Reviewer && (m.ReviewStep == "findings" || m.ReviewStep == "recheck") {
					reviewMessages = append(reviewMessages, m)
				}
			}
			// A part is attributed once to its first successful author turn.
			if author != nil {
				row := rowFor(r, author)
				row.PartsAuthored++
				if part.Review != nil && len(part.Review.Rounds) == 1 && part.Review.Summary.Complete && part.Review.Summary.Passed && part.Review.Resolution == nil {
					row.PartsPassedFirstRound++
				}
			}
			if part.Review == nil {
				continue
			}
			for i, round := range part.Review.Rounds {
				var reviewer *Message
				// Each successful findings/recheck applies exactly one round.
				// This ordered relationship also covers records predating Round.
				// Explicit round metadata must agree when it is present.
				if i < len(reviewMessages) {
					m := reviewMessages[i]
					if m.Round == 0 || m.Round == round.Ref.Round {
						reviewer = m
					}
				}
				for _, item := range round.Items {
					if reviewer != nil {
						row := rowFor(r, reviewer)
						row.FindingsRaised++
						if item.Status == "withdrawn" {
							row.FindingsWithdrawn++
						}
						if item.Status == "fixed" {
							row.FindingsFixed++
						}
					}
				}
			}
		}
	}
	result := &ModelStats{Rows: []ModelStatsRow{}, UnattributedMarkerRejects: unattributed, GeneratedAt: nowMs()}
	for _, row := range groups {
		row.MedianTotalMs, row.MedianFirstOutputMs = medianMs(row.total), medianMs(row.first)
		result.Rows = append(result.Rows, row.ModelStatsRow)
	}
	slices.SortFunc(result.Rows, func(a, b ModelStatsRow) int {
		if n := cmp.Compare(a.Provider, b.Provider); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Model, b.Model); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Effort, b.Effort); n != 0 {
			return n
		}
		return cmp.Compare(a.Role, b.Role)
	})
	return result, nil
}

func usageNumber(usage map[string]any, key string) float64 {
	value, _ := usage[key].(float64) // persisted JSON numbers
	return value
}

func medianMs(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	mid := len(values) / 2
	if len(values)%2 != 0 {
		return values[mid]
	}
	return values[mid-1] + (values[mid]-values[mid-1])/2
}

// UsageTotals normalizes token usage across providers. Input is every input
// token the model processed; Cached is the part read from the prompt cache and
// Uncached the rest (cache writes count as uncached).
type UsageTotals struct {
	Turns      int     `json:"turns"`
	Errors     int     `json:"errors"`
	Input      float64 `json:"input_tokens"`
	Cached     float64 `json:"cached_tokens"`
	Uncached   float64 `json:"uncached_tokens"`
	Output     float64 `json:"output_tokens"`
	CostUSD    float64 `json:"cost_usd"`
	MaxContext int     `json:"max_context_tokens"` // largest single model call of a turn
	DurationMs int64   `json:"duration_ms"`
}

type SeatUsage struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Effort   string `json:"effort"`
	Removed  bool   `json:"removed,omitempty"`
	UsageTotals
	ContextTokens int `json:"context_tokens"` // the seat's latest call
	ContextWindow int `json:"context_window"`
}

type RoomUsage struct {
	Room   string      `json:"room"`
	Title  string      `json:"title"`
	Totals UsageTotals `json:"totals"`
	Seats  []SeatUsage `json:"seats"`
}

// add counts one finished agent reply. Codex input_tokens already includes its
// cached part; Claude, agy and OpenCode report input without the cache.
func (t *UsageTotals) add(provider string, m *Message) {
	t.Turns++
	if m.Status == "error" {
		t.Errors++
	}
	t.DurationMs += m.Duration
	cached := usageNumber(m.Usage, "cached_input_tokens") + usageNumber(m.Usage, "cache_read_input_tokens") + usageNumber(m.Usage, "cache_read_tokens")
	input := usageNumber(m.Usage, "input_tokens")
	if provider != "codex" {
		input += usageNumber(m.Usage, "cache_creation_input_tokens") + cached
	}
	t.Input += input
	t.Cached += cached
	t.Uncached += input - cached
	t.Output += usageNumber(m.Usage, "output_tokens")
	t.CostUSD += usageNumber(m.Usage, "total_cost_usd")
	t.MaxContext = max(t.MaxContext, int(usageNumber(m.Usage, "context_tokens")))
}

func (t *UsageTotals) merge(o UsageTotals) {
	t.Turns += o.Turns
	t.Errors += o.Errors
	t.Input += o.Input
	t.Cached += o.Cached
	t.Uncached += o.Uncached
	t.Output += o.Output
	t.CostUSD += o.CostUSD
	t.MaxContext = max(t.MaxContext, o.MaxContext)
	t.DurationMs += o.DurationMs
}

// RoomUsage sums the room's finished agent replies per seat, removed seats included.
func (h *Hub) RoomUsage(id string) (*RoomUsage, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	rs, ok := h.rooms[id]
	if !ok {
		return nil, fmt.Errorf("방 없음: %s", id)
	}
	out := &RoomUsage{Room: id, Title: rs.room.Title, Seats: []SeatUsage{}}
	index := map[string]int{}
	for _, p := range rs.room.Participants {
		index[p.ID] = len(out.Seats)
		out.Seats = append(out.Seats, SeatUsage{ID: p.ID, Label: p.Label, Provider: p.Provider, Model: p.Model, Effort: p.Effort, Removed: p.Removed, ContextTokens: p.ContextTokens, ContextWindow: p.ContextWindow})
	}
	for _, m := range rs.msgs {
		if m.Kind != "agent" || m.Status == "streaming" {
			continue
		}
		i, seat := index[m.Author]
		if !seat {
			return nil, fmt.Errorf("방 %s 답변 %s의 좌석 %s가 참가자 기록에 없음", id, m.ID, m.Author)
		}
		out.Seats[i].add(out.Seats[i].Provider, m)
	}
	for _, s := range out.Seats {
		out.Totals.merge(s.UsageTotals)
	}
	return out, nil
}
