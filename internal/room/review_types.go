package room

// Adversarial review types. Contract: docs/작업분배_적대적리뷰_계약.md §4 (P1 owns this file).

// ReviewRef pins a review round to exact commits. The server assigns it; model output never does.
type ReviewRef struct {
	PartID  string `json:"part_id"`
	Round   int    `json:"round"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
}

type Finding struct {
	ID         string `json:"id"`       // server-issued: r1-1
	Severity   string `json:"severity"` // critical|high|medium|low
	File       string `json:"file"`     // repository-relative
	Side       string `json:"side"`     // base|head
	Line       int    `json:"line"`     // 1-based line at that side's SHA
	Scenario   string `json:"scenario"`
	Confidence string `json:"confidence"` // high|medium|low
}

type ItemReply struct {
	ID     string `json:"id"`
	Action string `json:"action"` // ACCEPT|REJECT|MAINTAIN|WITHDRAW|FIXED|UNFIXED
	Reason string `json:"reason"`
}

type ReviewItem struct {
	Finding  Finding    `json:"finding"`
	Author   *ItemReply `json:"author,omitempty"`
	Reviewer *ItemReply `json:"reviewer,omitempty"`
	Status   string     `json:"status"` // open|accepted_pending|disputed|withdrawn|fixed
}

type ReviewRound struct {
	Ref   ReviewRef    `json:"ref"`
	Items []ReviewItem `json:"items"`
}

type ReviewSummary struct {
	Unresolved int  `json:"unresolved"`
	Complete   bool `json:"complete"`
	Passed     bool `json:"passed"` // review passed only; merge eligibility is decided by work.go
}

// ReviewInput is everything a reviewer may see (D10): no room history.
type ReviewInput struct {
	Ref          ReviewRef `json:"ref"`
	Goal         string    `json:"goal"`
	Brief        string    `json:"brief"`
	Scope        []string  `json:"scope"`
	AuthorRole   string    `json:"author_role,omitempty"`
	ReviewerRole string    `json:"reviewer_role,omitempty"`
	Diff         *PartDiff `json:"diff"`
}

// ReviewStep is the next turn the hub must run.
type ReviewStep struct {
	Who           string `json:"who"`  // reviewer (fresh session) | author (part session)
	Kind          string `json:"kind"` // findings|author_reply|rebuttal|recheck
	Prompt        string `json:"prompt"`
	FormatRetries int    `json:"format_retries"` // one automatic correction per step, persisted before dispatch
}

// HumanResolution records an explicit waiver without changing review findings.
type HumanResolution struct {
	By      string   `json:"by"`
	At      int64    `json:"at"` // Unix milliseconds
	ItemIDs []string `json:"item_ids"`
}

// ReviewState is stored on Part.Review and drives the review loop.
type ReviewState struct {
	MaxRounds  int              `json:"max_rounds"`
	Rounds     []ReviewRound    `json:"rounds"`
	Phase      string           `json:"phase"` // findings|author_reply|rebuttal|fix|recheck|passed|disputed|limit
	Pending    *ReviewStep      `json:"pending,omitempty"`
	Summary    ReviewSummary    `json:"summary"`
	Input      ReviewInput      `json:"input"` // retained to build subsequent steps after restart
	Resolution *HumanResolution `json:"resolution,omitempty"`
}
