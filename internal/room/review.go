package room

// Contract signatures for P1. The P1 implementation replaces these bodies.

// NewReviewState starts an empty review with a round limit.
func NewReviewState(maxRounds int) *ReviewState {
	return &ReviewState{MaxRounds: maxRounds}
}

// Start begins round 1 for a freshly submitted part.
func (rs *ReviewState) Start(in ReviewInput) (ReviewStep, error) {
	panic("review: P1 미구현")
}

// Apply parses the reply to the pending step. outcome is "" (run next),
// "fix", "passed", "disputed" or "limit". A format violation returns an error
// and changes nothing.
func (rs *ReviewState) Apply(reply string) (next *ReviewStep, outcome string, err error) {
	panic("review: P1 미구현")
}

// Resubmit starts the next round after the author fixed accepted items.
func (rs *ReviewState) Resubmit(in ReviewInput) (ReviewStep, error) {
	panic("review: P1 미구현")
}
