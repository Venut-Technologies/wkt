package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/state"
)

// TestSyncFetchesAndReportsDriftWithoutMovingAnything — spec §6: sync fetches
// in every store of the set and reports base drift, and "does not advance the
// base by itself". A task's base is the thing every branch in it was cut from;
// moving it silently would rewrite what the task means.
func TestSyncFetchesAndReportsDrift(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-sync", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	before := tk.Repos[0].BaseSHA

	// The workspace moves on after the task was cut.
	repo := filepath.Join(c.Workspace, "docs")
	if err := os.WriteFile(filepath.Join(repo, "new.md"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "moved on")

	report, err := Sync(c, "feat-sync")
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("want one repository reported, got %+v", report)
	}
	if !report[0].Drifted {
		t.Fatalf("the base has moved on and sync must say so: %+v", report[0])
	}
	if report[0].Behind < 1 {
		t.Fatalf("it should say how far behind the task is: %+v", report[0])
	}

	// And nothing moved: the recorded base is still what the task was cut from.
	after, err := loadRepo(t, c, "feat-sync", "docs")
	if err != nil {
		t.Fatal(err)
	}
	if after.BaseSHA != before {
		t.Fatal("sync must not advance the base by itself")
	}
	head := strings.TrimSpace(g(t, after.WorktreePath, "rev-parse", "HEAD"))
	if head != before {
		t.Fatal("sync must not move the worktree either")
	}
}

// TestSyncIsQuietWhenNothingMoved — a report that always says "drifted"
// carries no information.
func TestSyncIsQuietWhenNothingMoved(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-quiet", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(c, "feat-quiet")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report {
		if r.Drifted {
			t.Fatalf("nothing has moved: %+v", r)
		}
	}
}

func loadRepo(t *testing.T, c container.C, task, rel string) (state.Repo, error) {
	t.Helper()
	tk, err := state.Load(c.StateDir(), task)
	if err != nil {
		return state.Repo{}, err
	}
	for _, r := range tk.Repos {
		if r.RelPath == rel {
			return r, nil
		}
	}
	return state.Repo{}, os.ErrNotExist
}

// TestSyncSeesLocalCommitsThatWereNeverPushed — the case the battery caught
// that the first unit tests could not: with a real origin present, looking
// only at origin calls a repository "up to date" while the developer's own
// unpushed commits sit in it. Those reach the store through the workspace
// remote (spec §5.2), which exists for exactly this.
func TestSyncSeesLocalCommitsThatWereNeverPushed(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-local", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(c.Workspace, "docs")

	// Give the repository an origin that is *behind* the local branch, which
	// is what an unpushed commit looks like.
	bare := filepath.Join(t.TempDir(), "origin.git")
	g(t, c.Workspace, "init", "-q", "--bare", bare)
	g(t, repo, "remote", "add", "origin", bare)
	g(t, repo, "push", "-q", "origin", "HEAD")
	g(t, repo, "commit", "-qm", "local, unpushed", "--allow-empty")

	report, err := Sync(c, "feat-local")
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !report[0].Drifted {
		t.Fatalf("an unpushed local commit is still drift: %+v", report)
	}
}

// TestSyncReportsTheFurthestRemote — origin and the workspace can both have
// moved, by different amounts. Reporting whichever was looked at first would
// understate the drift exactly when it matters. The origin has to exist
// *before* the task is created, or the store never learns about it and the
// test quietly checks nothing.
func TestSyncReportsTheFurthestRemote(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	repo := filepath.Join(ws, "docs")
	seed(t, repo)
	bare := filepath.Join(base, "origin.git")
	g(t, base, "init", "-q", "--bare", bare)
	g(t, repo, "remote", "add", "origin", bare)
	g(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/main")

	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	entries, err := discover.Walk(ws, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries, "feat-furthest", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	// origin moves one commit ahead; the local branch moves three.
	g(t, repo, "commit", "-qm", "pushed 1", "--allow-empty")
	g(t, repo, "push", "-q", "origin", "HEAD:refs/heads/main")
	g(t, repo, "commit", "-qm", "local 1", "--allow-empty")
	g(t, repo, "commit", "-qm", "local 2", "--allow-empty")

	report, err := Sync(c, "feat-furthest")
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("want one repository: %+v", report)
	}
	if report[0].Behind != 3 {
		t.Fatalf("want the furthest remote's distance (3), got %d: %+v", report[0].Behind, report[0])
	}
}

// TestSyncFetchesFromOrigin — the origin fetch is not decoration: work someone
// else pushed is drift too, and it reaches the store only through that fetch.
func TestSyncFetchesFromOrigin(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	repo := filepath.Join(ws, "docs")
	seed(t, repo)
	bare := filepath.Join(base, "origin.git")
	g(t, base, "init", "-q", "--bare", bare)
	g(t, repo, "remote", "add", "origin", bare)
	g(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/main")

	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	entries, err := discover.Walk(ws, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries, "feat-origin", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	// Someone else pushes, through a clone the workspace never sees.
	other := filepath.Join(base, "other")
	g(t, base, "clone", "-q", bare, other)
	g(t, other, "commit", "-qm", "someone else", "--allow-empty")
	g(t, other, "push", "-q", "origin", "HEAD:refs/heads/main")

	report, err := Sync(c, "feat-origin")
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 || !report[0].Drifted {
		t.Fatalf("work pushed to origin is drift the task should hear about: %+v", report)
	}
}

// TestSyncSaysWhenItCouldNotReachTheUpstream — sync discarded the origin
// fetch error, so a store that has never once reached its upstream reported
// "up to date", exit 0. That is the one answer sync must not give when it did
// not manage to look: wkt's rule everywhere else is that a check which cannot
// run counts against you, not for you.
func TestSyncSaysWhenItCouldNotReachTheUpstream(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	repo := filepath.Join(ws, "docs")
	seed(t, repo)
	bare := filepath.Join(base, "origin.git")
	g(t, base, "init", "-q", "--bare", bare)
	g(t, repo, "remote", "add", "origin", bare)
	g(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/main")

	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	entries, err := discover.Walk(ws, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries, "feat-unreachable", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	// The upstream goes away — moved, renamed, VPN down, credentials expired.
	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}

	report, err := Sync(c, "feat-unreachable")
	if err != nil {
		t.Fatal(err)
	}
	if len(report) != 1 {
		t.Fatalf("want one repository: %+v", report)
	}
	if report[0].Unreachable == "" {
		t.Fatalf("sync must say it could not consult the upstream: %+v", report[0])
	}
	if strings.Contains(report[0].Unreachable, "\n") {
		t.Fatalf("the reason must stay one line: %q", report[0].Unreachable)
	}
}

// TestSyncIsSilentAboutAnUpstreamThatDoesNotExist — a repository with no
// origin at all is not "unreachable", it simply has nowhere to look, and
// saying otherwise would fire on every local-only repository.
func TestSyncIsSilentAboutAnUpstreamThatDoesNotExist(t *testing.T) {
	c, entries := fixture(t) // fixture repositories have no origin
	if _, err := Create(c, entries, "feat-noorigin", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	report, err := Sync(c, "feat-noorigin")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report {
		if r.Unreachable != "" {
			t.Fatalf("a repository without an origin is not unreachable: %+v", r)
		}
	}
}

// The reason a store could not be reached is git's, not wkt's wrapper around
// it. firstLineOf was applied to err.Error(), which renders as
// "WKT_GIT_FAILED: git fetch failed (fatal: ...)" — so the report led with
// wkt's own code and buried what git said inside parentheses.
func TestSyncReportsGitsReasonNotWktsWrapper(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-unreach", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	tk, err := state.Load(c.StateDir(), "feat-unreach")
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(c.StoreDir(), tk.Repos[0].StoreID+".git")
	g(t, storePath, "config", "remote.origin.url", filepath.Join(t.TempDir(), "gone.git"))

	reports, err := Sync(c, "feat-unreach")
	if err != nil {
		t.Fatal(err)
	}
	if len(reports) != 1 || reports[0].Unreachable == "" {
		t.Fatalf("an origin that cannot be consulted must be reported: %+v", reports)
	}
	if strings.Contains(reports[0].Unreachable, "WKT_GIT_FAILED") {
		t.Fatalf("the reason must be git's own words, not wkt's wrapper: %q", reports[0].Unreachable)
	}
}
