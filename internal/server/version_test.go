package server

import (
	"testing"

	"roundtable/internal/update"
)

type fixedUpdates update.Status

func (f fixedUpdates) Status() update.Status { return update.Status(f) }

func TestVersionEndpoint(t *testing.T) {
	ts := newTestServer(t)
	code, got := call(t, ts, "GET", "/api/version", nil)
	if code != 200 || got["state"] != "off" || got["update_available"] != false {
		t.Fatalf("without a checker: %d %v", code, got)
	}

	srv := ts.Config.Handler.(*Server)
	srv.SetUpdates(fixedUpdates{State: "behind", Repo: "o/r", Branch: "main", Running: update.Build{Revision: "abc", Modified: true},
		Latest: "def", BehindBy: 3, Commits: []update.Commit{{SHA: "def", Title: "새 기능"}}, CheckedAt: 42, UpdateAvailable: true})
	code, got = call(t, ts, "GET", "/api/version", nil)
	running, _ := got["running"].(map[string]any)
	commits, _ := got["commits"].([]any)
	if code != 200 || got["state"] != "behind" || got["behind_by"] != float64(3) || got["latest"] != "def" || got["update_available"] != true ||
		got["checked_at"] != float64(42) || running["revision"] != "abc" || running["modified"] != true || len(commits) != 1 {
		t.Fatalf("%d %v", code, got)
	}
	for _, key := range []string{"running", "latest", "behind_by", "commits", "checked_at", "update_available"} {
		if _, ok := got[key]; !ok {
			t.Errorf("missing %s", key)
		}
	}
}
