package room

// Work-division plan types. Contract: docs/작업분배_적대적리뷰_계약.md (P2 owns this file).

// WorkPlan is the per-room work-division state stored in room.json ("work").
type WorkPlan struct {
	IntegrationWorktree        string       `json:"integration_worktree"`
	IntegrationWorktreeRemoved bool         `json:"integration_worktree_removed,omitempty"`
	IntegrationCleanupError    string       `json:"integration_cleanup_error,omitempty"`
	Goal                       string       `json:"goal"`
	BaseSHA                    string       `json:"base_sha"`
	Integration                string       `json:"integration_branch"` // rt/<roomID>/integration
	IntegrationSHA             string       `json:"integration_sha"`    // current tip
	MergeMode                  string       `json:"merge_mode"`         // auto | confirm
	ReviewRounds               int          `json:"review_rounds"`
	Verify                     []VerifyCmd  `json:"verify"`
	Parts                      []*Part      `json:"parts"`
	Queue                      []string     `json:"queue"`             // part ids, FIFO; only the head is processed
	Halted                     *Block       `json:"halted,omitempty"`  // D5: why the merge queue stopped
	Merging                    *MergeIntent `json:"merging,omitempty"` // D13: written before the integration ref moves
}

type VerifyCmd struct {
	Name       string   `json:"name"`
	Argv       []string `json:"argv"` // never a shell string
	TimeoutSec int      `json:"timeout_sec"`
}

type Part struct {
	ID                   string       `json:"id"`
	Title                string       `json:"title"`
	Brief                string       `json:"brief"` // completion criteria
	Scope                []string     `json:"scope"` // repository-relative paths
	Seat                 string       `json:"seat"`
	Reviewer             string       `json:"reviewer"` // seat of another provider (D3)
	Branch               string       `json:"branch"`   // rt/<roomID>/<partID>
	Worktree             string       `json:"worktree"`
	WorktreeRemoved      bool         `json:"worktree_removed,omitempty"`
	WorktreeCleanupError string       `json:"worktree_cleanup_error,omitempty"`
	SessionID            string       `json:"session_id"`
	LastSeen             int64        `json:"last_seen"` // delivery boundary independent of the seat's chat session
	Unseen               []int64      `json:"unseen,omitempty"`
	State                string       `json:"state"` // working|reviewing|fixing|queued|awaiting_approval|merging|merged|blocked|abandoned
	SubmitSHA            string       `json:"submit_sha"`
	ReadyMode            string       `json:"ready_mode"` // D6: merge_mode captured when the part became queued
	Approval             *Approval    `json:"approval,omitempty"`
	Block                *Block       `json:"block,omitempty"`
	Review               *ReviewState `json:"review,omitempty"`
	MergedSHA            string       `json:"merged_sha,omitempty"`
}

type Approval struct {
	SubmitSHA      string `json:"submit_sha"`
	IntegrationSHA string `json:"integration_sha"`
	At             int64  `json:"at"`
}

type Block struct {
	Kind    string         `json:"kind"` // worktree|submit|conflict|verify|review|restart
	From    string         `json:"from"` // state that retry returns to
	Message string         `json:"message"`
	Result  *CommandResult `json:"result,omitempty"`
	At      int64          `json:"at"`
}

type CommandResult struct {
	Name      string   `json:"name"`
	Argv      []string `json:"argv"`
	ExitCode  int      `json:"exit_code"`
	TimedOut  bool     `json:"timed_out"`
	Output    string   `json:"output"` // tail only
	Conflicts []string `json:"conflicts,omitempty"`
}

type MergeIntent struct {
	PartID    string `json:"part_id"`
	SubmitSHA string `json:"submit_sha"`
	BeforeSHA string `json:"before_sha"`
	MergeSHA  string `json:"merge_sha"`
}

// WorkSpec is the POST /api/rooms/{id}/work body (input boundary: defaults apply here only).
type WorkSpec struct {
	Goal         string      `json:"goal"`
	MergeMode    string      `json:"merge_mode"`
	ReviewRounds int         `json:"review_rounds"`
	Verify       []VerifyCmd `json:"verify"`
	Parts        []PartSpec  `json:"parts"`
}

type PartSpec struct {
	Title    string   `json:"title"`
	Brief    string   `json:"brief"`
	Scope    []string `json:"scope"`
	Seat     string   `json:"seat"`
	Reviewer string   `json:"reviewer"`
}

// PartDiff is base..submit for one part; reviews and the UI drawer share it.
type PartDiff struct {
	PartID    string     `json:"part_id"`
	BaseSHA   string     `json:"base_sha"`
	HeadSHA   string     `json:"head_sha"`
	Added     int        `json:"added"`
	Deleted   int        `json:"deleted"`
	Truncated bool       `json:"truncated"`
	Files     []DiffFile `json:"files"`
}

type DiffFile struct {
	Path    string `json:"path"`
	Status  string `json:"status"` // A|M|D|R
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
	Patch   string `json:"patch"`
}
