// Package update tells whether the running build is behind the published
// branch. It sends one unauthenticated GET to the GitHub compare API per check
// (with If-None-Match) and nothing else; failures only change the reported state.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// MinInterval keeps periodic checks well under GitHub's unauthenticated limit
// (60 requests per hour per address).
const MinInterval = 10 * time.Minute

const maxBody = 16 << 20 // compare replies carry file patches; only a few fields are read

// Build is the source revision baked in by `go build` (runtime/debug VCS stamps).
type Build struct {
	Revision string `json:"revision"`
	Modified bool   `json:"modified"` // uncommitted changes in the build tree
	Time     string `json:"time,omitempty"`
}

func ReadBuild() Build {
	var b Build
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return b
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			b.Revision = s.Value
		case "vcs.modified":
			b.Modified = s.Value == "true"
		case "vcs.time":
			b.Time = s.Value
		}
	}
	return b
}

type Commit struct {
	SHA   string `json:"sha"`
	Title string `json:"title"`
	Date  string `json:"date"`
}

// State values:
//
//	off              checking disabled
//	local_build      no VCS revision in the binary (go run, -buildvcs=false, copied sources)
//	pending          first check not finished
//	up_to_date       the branch has nothing the build lacks
//	behind           the branch has newer commits (update_available)
//	ahead            the build has commits the branch lacks, and nothing is missing
//	diverged         both
//	unknown_revision the build's commit is not in the repository (unpushed local commit)
//	error            the last check failed; earlier results are kept
type Status struct {
	State           string   `json:"state"`
	Repo            string   `json:"repo,omitempty"`
	Branch          string   `json:"branch,omitempty"`
	Running         Build    `json:"running"`
	Latest          string   `json:"latest,omitempty"`
	BehindBy        int      `json:"behind_by"`
	AheadBy         int      `json:"ahead_by"`
	Commits         []Commit `json:"commits"` // newest first, at most 5 of the missing commits
	CheckedAt       int64    `json:"checked_at"`
	UpdateAvailable bool     `json:"update_available"`
	Error           string   `json:"error,omitempty"`
}

type Config struct {
	Enabled  bool
	Repo     string // owner/name
	Branch   string
	Interval time.Duration
	API      string // base URL, https://api.github.com
	Build    Build
	Client   *http.Client
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9_./-]+$`)

// Validate rejects settings that would misdirect or flood the check.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if !repoPattern.MatchString(c.Repo) || strings.Contains(c.Repo, "..") {
		return fmt.Errorf("업데이트 확인 저장소는 owner/name 형식이어야 해: %q", c.Repo)
	}
	if !branchPattern.MatchString(c.Branch) || strings.Contains(c.Branch, "..") {
		return fmt.Errorf("업데이트 확인 브랜치 이름이 올바르지 않아: %q", c.Branch)
	}
	if c.Interval < MinInterval {
		return fmt.Errorf("업데이트 확인 주기는 %s 이상이어야 해: %s", MinInterval, c.Interval)
	}
	if !strings.HasPrefix(c.API, "https://") && !strings.HasPrefix(c.API, "http://127.0.0.1") && !strings.HasPrefix(c.API, "http://localhost") {
		return fmt.Errorf("업데이트 확인 API는 https 주소여야 해(로컬 시험 서버 제외): %q", c.API)
	}
	return nil
}

type Checker struct {
	cfg  Config
	mu   sync.Mutex
	st   Status
	etag string
}

func New(cfg Config) *Checker {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 30 * time.Second}
	}
	st := Status{State: "pending", Repo: cfg.Repo, Branch: cfg.Branch, Running: cfg.Build, Commits: []Commit{}}
	switch {
	case !cfg.Enabled:
		st = Status{State: "off", Running: cfg.Build, Commits: []Commit{}}
	case cfg.Build.Revision == "":
		st.State = "local_build"
	}
	return &Checker{cfg: cfg, st: st}
}

func (c *Checker) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.st
	st.Commits = append([]Commit{}, c.st.Commits...)
	return st
}

// Run checks once now and then every Interval until ctx ends. It returns at
// once when checking is off or the build has no revision to compare.
func (c *Checker) Run(ctx context.Context) {
	if !c.cfg.Enabled || c.cfg.Build.Revision == "" {
		return
	}
	t := time.NewTicker(c.cfg.Interval)
	defer t.Stop()
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type compareReply struct {
	Status   string `json:"status"`
	AheadBy  int    `json:"ahead_by"`  // commits on the branch the build lacks
	BehindBy int    `json:"behind_by"` // commits in the build the branch lacks
	Merge    struct {
		SHA string `json:"sha"`
	} `json:"merge_base_commit"`
	Commits []struct {
		SHA    string `json:"sha"`
		Commit struct {
			Message string `json:"message"`
			Author  struct {
				Date string `json:"date"`
			} `json:"author"`
		} `json:"commit"`
	} `json:"commits"` // oldest first
}

// Check runs one compare request and records the outcome.
func (c *Checker) Check(ctx context.Context) {
	if !c.cfg.Enabled || c.cfg.Build.Revision == "" {
		return
	}
	url := fmt.Sprintf("%s/repos/%s/compare/%s...%s", strings.TrimRight(c.cfg.API, "/"), c.cfg.Repo, c.cfg.Build.Revision, c.cfg.Branch)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		c.fail(err.Error())
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "roundtable-update-check")
	c.mu.Lock()
	if c.etag != "" {
		req.Header.Set("If-None-Match", c.etag)
	}
	c.mu.Unlock()
	resp, err := c.cfg.Client.Do(req)
	if err != nil {
		c.fail("GitHub 연결 실패: " + err.Error())
		return
	}
	defer resp.Body.Close()
	now := time.Now().UnixMilli()
	switch resp.StatusCode {
	case http.StatusNotModified:
		c.mu.Lock()
		c.st.CheckedAt, c.st.Error = now, ""
		if c.st.State == "error" {
			c.st.State = stateOf(c.st.BehindBy, c.st.AheadBy)
		}
		c.mu.Unlock()
		return
	case http.StatusNotFound:
		// GitHub answers 404 when either side is unknown; the branch exists, so
		// it is the build's commit (not pushed, or built from another repository).
		c.mu.Lock()
		c.st = Status{State: "unknown_revision", Repo: c.cfg.Repo, Branch: c.cfg.Branch, Running: c.cfg.Build, Commits: []Commit{}, CheckedAt: now}
		c.etag = ""
		c.mu.Unlock()
		return
	case http.StatusOK:
	default:
		var e struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
		c.fail(fmt.Sprintf("GitHub API HTTP %d: %s", resp.StatusCode, e.Message))
		return
	}
	var r compareReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&r); err != nil {
		c.fail("GitHub 응답 해석 실패: " + err.Error())
		return
	}
	st := Status{Repo: c.cfg.Repo, Branch: c.cfg.Branch, Running: c.cfg.Build, BehindBy: r.AheadBy, AheadBy: r.BehindBy,
		Commits: []Commit{}, CheckedAt: now, State: stateOf(r.AheadBy, r.BehindBy)}
	st.UpdateAvailable = st.BehindBy > 0
	st.Latest = r.Merge.SHA // the branch tip when the branch has nothing new
	if n := len(r.Commits); n > 0 {
		st.Latest = r.Commits[n-1].SHA
		for i := n - 1; i >= 0 && len(st.Commits) < 5; i-- {
			cm := r.Commits[i]
			title, _, _ := strings.Cut(cm.Commit.Message, "\n")
			st.Commits = append(st.Commits, Commit{SHA: cm.SHA, Title: title, Date: cm.Commit.Author.Date})
		}
	}
	c.mu.Lock()
	c.st, c.etag = st, resp.Header.Get("ETag")
	c.mu.Unlock()
}

func stateOf(behind, ahead int) string {
	switch {
	case behind > 0 && ahead > 0:
		return "diverged"
	case behind > 0:
		return "behind"
	case ahead > 0:
		return "ahead"
	}
	return "up_to_date"
}

// fail keeps the last known comparison and records why this check failed.
func (c *Checker) fail(msg string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.st.State, c.st.Error, c.st.CheckedAt = "error", msg, time.Now().UnixMilli()
}
