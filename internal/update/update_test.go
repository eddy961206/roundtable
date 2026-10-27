package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const rev = "1111111111111111111111111111111111111111"

// fakeGitHub answers the compare endpoint with handler and records requests.
type fakeGitHub struct {
	mu   sync.Mutex
	reqs []*http.Request
	srv  *httptest.Server
}

func newFake(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *fakeGitHub {
	f := &fakeGitHub{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Clone(context.Background()))
		f.mu.Unlock()
		handler(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func checker(api string, b Build) *Checker {
	return New(Config{Enabled: true, Repo: "o/r", Branch: "main", Interval: time.Hour, API: api, Build: b})
}

func compareJSON(status string, ahead, behind int, shas ...string) string {
	var cs []string
	for i, s := range shas {
		cs = append(cs, fmt.Sprintf(`{"sha":%q,"commit":{"message":"title %d\n\nbody","author":{"date":"2026-10-27T0%d:00:00Z"}}}`, s, i, i))
	}
	return fmt.Sprintf(`{"status":%q,"ahead_by":%d,"behind_by":%d,"merge_base_commit":{"sha":"basesha"},"commits":[%s],"files":[{"patch":"x"}]}`,
		status, ahead, behind, strings.Join(cs, ","))
}

func TestBehind(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, compareJSON("ahead", 7, 0, "c1", "c2", "c3", "c4", "c5", "c6", "c7"))
	})
	c := checker(f.srv.URL, Build{Revision: rev})
	c.Check(context.Background())
	st := c.Status()
	if st.State != "behind" || !st.UpdateAvailable || st.BehindBy != 7 || st.AheadBy != 0 || st.Latest != "c7" || st.Error != "" {
		t.Fatalf("%+v", st)
	}
	if len(st.Commits) != 5 || st.Commits[0].SHA != "c7" || st.Commits[0].Title != "title 6" || st.Commits[4].SHA != "c3" {
		t.Fatalf("commits newest first, at most 5: %+v", st.Commits)
	}
	r := f.reqs[0]
	if r.URL.Path != "/repos/o/r/compare/"+rev+"...main" || r.Method != "GET" {
		t.Fatalf("request %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("User-Agent") != "roundtable-update-check" || r.URL.RawQuery != "" {
		t.Fatalf("only an anonymous GET may leave: %v %q", r.Header, r.URL.RawQuery)
	}
}

func TestUpToDateAndAhead(t *testing.T) {
	for _, tc := range []struct {
		body, state string
		ahead       int
	}{{compareJSON("identical", 0, 0), "up_to_date", 0}, {compareJSON("behind", 0, 2), "ahead", 2}} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, tc.body) })
		c := checker(f.srv.URL, Build{Revision: rev})
		c.Check(context.Background())
		st := c.Status()
		if st.State != tc.state || st.UpdateAvailable || st.BehindBy != 0 || st.AheadBy != tc.ahead || st.Latest != "basesha" || len(st.Commits) != 0 {
			t.Fatalf("%s: %+v", tc.state, st)
		}
	}
}

func TestLocalCommitNotOnRemote(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	})
	c := checker(f.srv.URL, Build{Revision: rev, Modified: true})
	c.Check(context.Background())
	st := c.Status()
	if st.State != "unknown_revision" || st.UpdateAvailable || st.Error != "" || st.CheckedAt == 0 || !st.Running.Modified {
		t.Fatalf("%+v", st)
	}
}

func TestNotModifiedReusesResult(t *testing.T) {
	calls := 0
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("ETag", `W/"v1"`)
			fmt.Fprint(w, compareJSON("ahead", 2, 0, "c1", "c2"))
			return
		}
		if r.Header.Get("If-None-Match") != `W/"v1"` {
			t.Errorf("If-None-Match = %q", r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(304)
	})
	c := checker(f.srv.URL, Build{Revision: rev})
	c.Check(context.Background())
	first := c.Status()
	time.Sleep(2 * time.Millisecond)
	c.Check(context.Background())
	st := c.Status()
	if st.State != "behind" || st.BehindBy != 2 || len(st.Commits) != 2 || st.CheckedAt <= first.CheckedAt || st.Error != "" {
		t.Fatalf("%+v", st)
	}
}

func TestNetworkErrorKeepsLastResult(t *testing.T) {
	ok := true
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		if ok {
			fmt.Fprint(w, compareJSON("ahead", 1, 0, "c1"))
			return
		}
		w.WriteHeader(403)
		fmt.Fprint(w, `{"message":"API rate limit exceeded"}`)
	})
	c := checker(f.srv.URL, Build{Revision: rev})
	c.Check(context.Background())
	ok = false
	c.Check(context.Background())
	st := c.Status()
	if st.State != "error" || !strings.Contains(st.Error, "403") || !strings.Contains(st.Error, "rate limit") || !st.UpdateAvailable || st.BehindBy != 1 {
		t.Fatalf("API failure must keep the last comparison: %+v", st)
	}
	f.srv.Close() // nothing listens now
	c.Check(context.Background())
	if st := c.Status(); st.State != "error" || !strings.Contains(st.Error, "GitHub 연결 실패") {
		t.Fatalf("%+v", st)
	}
	fresh := checker(f.srv.URL, Build{Revision: rev})
	fresh.Check(context.Background())
	if st := fresh.Status(); st.State != "error" || st.UpdateAvailable {
		t.Fatalf("%+v", st)
	}
}

func TestBadBody(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"status":`) })
	c := checker(f.srv.URL, Build{Revision: rev})
	c.Check(context.Background())
	if st := c.Status(); st.State != "error" || !strings.Contains(st.Error, "해석") {
		t.Fatalf("%+v", st)
	}
}

func TestOffAndLocalBuildSendNothing(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, compareJSON("ahead", 1, 0, "c1")) })
	off := New(Config{Enabled: false, Repo: "o/r", Branch: "main", Interval: time.Hour, API: f.srv.URL, Build: Build{Revision: rev}})
	local := checker(f.srv.URL, Build{})
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for _, c := range []*Checker{off, local} {
		done := make(chan struct{})
		go func() { c.Run(ctx); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("Run kept going for a check it must not make")
		}
		c.Check(ctx)
	}
	if f.count() != 0 {
		t.Fatalf("%d requests sent", f.count())
	}
	if off.Status().State != "off" || local.Status().State != "local_build" {
		t.Fatalf("%+v / %+v", off.Status(), local.Status())
	}
}

func TestRunChecksAtStartAndPeriodically(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, compareJSON("identical", 0, 0)) })
	c := New(Config{Enabled: true, Repo: "o/r", Branch: "main", Interval: 30 * time.Millisecond, API: f.srv.URL, Build: Build{Revision: rev}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for f.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if f.count() < 3 || c.Status().State != "up_to_date" {
		t.Fatalf("requests %d, %+v", f.count(), c.Status())
	}
}

func TestValidate(t *testing.T) {
	good := Config{Enabled: true, Repo: "eddy961206/roundtable", Branch: "main", Interval: 6 * time.Hour, API: "https://api.github.com"}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"short interval": func(c *Config) { c.Interval = time.Minute },
		"bad repo":       func(c *Config) { c.Repo = "../x" },
		"bad branch":     func(c *Config) { c.Branch = "a..b" },
		"plain http":     func(c *Config) { c.API = "http://example.com" },
	} {
		c := good
		mutate(&c)
		if c.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if (Config{Enabled: false}).Validate() != nil {
		t.Error("disabled config needs no other setting")
	}
}
