package task

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// TestAddGraftsAtTheTasksEpochNotTodaysHead is the whole reason this command
// exists rather than "make a new task with more repositories". A task is a
// coherent slice of time across a set of repositories (spec §6): a repository
// joining late must arrive at the base the task was cut from, or the set stops
// being consistent and the eventual merges do not line up.
func TestAddGraftsAtTheTasksEpochNotTodaysHead(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-add", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	// The workspace moves on after the task was cut.
	late := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(late, "after.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, late, "add", "-A")
	// An explicit committer date an hour ahead: rev-list --before works on
	// committer dates at one-second resolution, and a commit made in the same
	// second as the task's epoch would legitimately count as "at or before"
	// it. The point of the test is a commit that is unambiguously later.
	commitAt(t, late, "committed after the task was created", time.Now().Add(time.Hour))
	head := strings.TrimSpace(g(t, late, "rev-parse", "HEAD"))

	if err := Add(c, entries, "feat-add", "services/svc-a"); err != nil {
		t.Fatal(err)
	}
	got, err := state.Load(c.StateDir(), "feat-add")
	if err != nil {
		t.Fatal(err)
	}
	var added *state.Repo
	for i := range got.Repos {
		if got.Repos[i].RelPath == "services/svc-a" {
			added = &got.Repos[i]
		}
	}
	if added == nil {
		t.Fatalf("the repository must be recorded in the task: %+v", got.Repos)
	}
	if added.BaseSHA == head {
		t.Fatal("the repository was grafted at today's HEAD instead of the task's epoch")
	}
	// And the worktree really is at that older commit.
	at := strings.TrimSpace(g(t, added.WorktreePath, "rev-parse", "HEAD"))
	if at != added.BaseSHA {
		t.Fatalf("worktree is at %s, state says %s", at, added.BaseSHA)
	}
	if _, err := os.Stat(filepath.Join(added.WorktreePath, "after.txt")); !os.IsNotExist(err) {
		t.Fatal("the tree should not contain work committed after the task's epoch")
	}
}

// TestAddPutsTheRepositoryOnTheTaskBranch — one task, one branch, across every
// repository in it.
func TestAddPutsTheRepositoryOnTheTaskBranch(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-branch", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	if err := Add(c, entries, "feat-branch", "services/svc-a"); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(c.TreePath("feat-branch"), "services", "svc-a")
	if got := strings.TrimSpace(g(t, wt, "rev-parse", "--abbrev-ref", "HEAD")); got != "feat-branch" {
		t.Fatalf("branch is %q, want the task's", got)
	}
	// The perimeter must cover the new repository too: a session started there
	// is otherwise uncovered (H6a).
	if _, err := os.Stat(filepath.Join(wt, ".claude", "settings.json")); err != nil {
		t.Fatalf("the new repository needs a perimeter copy: %v", err)
	}
	got, err := state.Load(c.StateDir(), "feat-branch")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.PerimeterCoverage) != 3 {
		t.Fatalf("coverage must include the added repository: %v", got.PerimeterCoverage)
	}
}

// TestAddRefusesTheCasesTheSpecNames — every one of these leaves the task
// exactly as it was.
func TestAddRefusesTheCasesTheSpecNames(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-refuse", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ task, repo, code string }{
		"no such task":          {"nope", "services/svc-a", "WKT_NO_TASK"},
		"not a discovered repo": {"feat-refuse", "services/ghost", "WKT_NO_SUCH_REPO"},
		"already in the task":   {"feat-refuse", "docs", "WKT_REPO_IN_TASK"},
	} {
		err := Add(c, entries, tc.task, tc.repo)
		if err == nil {
			t.Fatalf("%s: must be refused", name)
		}
		var e *wkterr.E
		if !errors.As(err, &e) || e.Code != tc.code {
			t.Fatalf("%s: want %s, got %v", name, tc.code, err)
		}
	}
	// The task is untouched by any of it.
	got, err := state.Load(c.StateDir(), "feat-refuse")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("a refused add must change nothing: %+v", got.Repos)
	}
}

// TestAddRollsBackOnFailure — the same rule as create: nothing half-added.
func TestAddRollsBackOnFailure(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-rb", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	// A branch of the task's name already exists in the repository being
	// added, which validation refuses — after the store may already exist.
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	g(t, repo, "branch", "feat-rb")

	if err := Add(c, entries, "feat-rb", "services/svc-a"); err == nil {
		t.Fatal("a branch collision must refuse")
	}
	// The path goes back to what it was: the back-fill link an unselected
	// repository has in every task tree, not a worktree and not a hole.
	at := filepath.Join(c.TreePath("feat-rb"), "services", "svc-a")
	info, err := os.Lstat(at)
	if err != nil {
		t.Fatalf("the refused add must restore the back-fill link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the path should be a symlink again, as it was before the add")
	}
	got, err := state.Load(c.StateDir(), "feat-rb")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Repos) != 1 {
		t.Fatalf("state must not record a repository that was not added: %+v", got.Repos)
	}
}

// commitAt commits with an explicit committer date, which is the date
// rev-list --before filters on.
func commitAt(t *testing.T, dir, msg string, when time.Time) {
	t.Helper()
	cmd := exec.Command("git", "-c", "user.email=e@x", "-c", "user.name=t", "commit", "-qm", msg)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_COMMITTER_DATE="+when.Format(time.RFC3339),
		"GIT_AUTHOR_DATE="+when.Format(time.RFC3339))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %s", out)
	}
}

// TestAddRestoresTheBackFillLinkWhenALateStepFails — the rollback that
// matters. Everything up to "worktree add" can fail before the link is
// touched; this drives a failure *after* it, which is the only case where the
// restore is load-bearing. Without it a refused add leaves a hole in the tree
// where a link used to be, and every relative path through it stops resolving.
func TestAddRestoresTheBackFillLinkWhenALateStepFails(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-late", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	at := filepath.Join(c.TreePath("feat-late"), "services", "svc-a")
	before, err := os.Readlink(at)
	if err != nil {
		t.Fatalf("precondition: svc-a should be a back-fill link: %v", err)
	}

	// Make the last step — writing state — impossible.
	if err := os.Chmod(c.StateDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(c.StateDir(), 0o700)

	if err := Add(c, entries, "feat-late", "services/svc-a"); err == nil {
		t.Fatal("an add whose state write fails must not report success")
	}

	after, err := os.Readlink(at)
	if err != nil {
		t.Fatalf("the back-fill link must be restored, got %v", err)
	}
	if after != before {
		t.Fatalf("the link points somewhere else now: %q, was %q", after, before)
	}
}

// TestAddRefusesWhenTheEpochPredatesTheRepository — a repository created after
// the task cannot join it: there is no commit as old as the task's epoch, and
// grafting at its first commit would import work the rest of the set has never
// seen.
func TestAddRefusesWhenTheEpochPredatesTheRepository(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-old", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	// Rewrite the epoch to long before any of these repositories existed.
	tk.BaseEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := state.Save(c.StateDir(), tk); err != nil {
		t.Fatal(err)
	}

	err = Add(c, entries, "feat-old", "services/svc-a")
	if err == nil {
		t.Fatal("a repository with no commit that old must be refused")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_BASE_UNREACHABLE" {
		t.Fatalf("want WKT_BASE_UNREACHABLE, got %v", err)
	}
}

// TestAddWorktreeFailureCarriesGitsReason — add builds a worktree the same way
// create does, so it fails the same way on a content filter, and it threw
// git's explanation away just as create did. A defect fixed at one of two
// identical sites is a defect that comes back.
func TestAddWorktreeFailureCarriesGitsReason(t *testing.T) {
	c, entries := fixture(t)

	// The filtered path is committed *before* the task exists. Add grafts a
	// repository at the task's base epoch, and git commit timestamps are whole
	// seconds: committing this afterwards meant the filter was inside the
	// epoch only when both landed in the same second. On a loaded machine they
	// did not, Add grafted the commit before .gitattributes, no filter ran,
	// and the test failed — which is how it failed on CI while passing fifteen
	// times in a row locally.
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.MkdirAll(filepath.Join(repo, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("assets/*.bin filter=leaky\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "assets", "big.bin"), []byte("pretend this is large\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "track a filtered path")

	if _, err := Create(c, entries, "feat-addfilter", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte(
		"[filter \"leaky\"]\n"+
			"\tsmudge = sh -c 'echo fetching objects >&2; exit 3' --token=glpat-SECRET %f\n"+
			"\trequired = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	err := Add(c, entries, "feat-addfilter", "services/svc-a")
	if err == nil {
		t.Fatal("a required filter that cannot run must fail the add")
	}
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("want a typed error, got %v", err)
	}
	if e.Found == "" || !strings.Contains(e.Found, "filter") {
		t.Fatalf("add must carry git's reason too: %+v", e)
	}
	if strings.Contains(e.Found, "glpat-SECRET") {
		t.Fatalf("and must not carry the user's secrets: %+v", e)
	}
	// And the back-fill link is back where it was.
	at := filepath.Join(c.TreePath("feat-addfilter"), "services", "svc-a")
	if info, statErr := os.Lstat(at); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the refused add must restore the back-fill link: %v", statErr)
	}
}
