package task

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// codeOf pulls wkt's own code out of an error, so a test asserts on the
// contract a caller sees rather than on a message.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("not a wkt error: %v", err)
	}
	return e.Code
}

// TestCaseCollisionFindsABranchDifferingOnlyInCase — asked of git's ref list
// rather than of the filesystem, so macOS and Linux give the same answer. On a
// case-insensitive filesystem the two branches cannot coexist at all; on a
// case-sensitive one they can, and a task that relied on the difference would
// stop working the moment a colleague on a Mac cloned it.
func TestCaseCollisionFindsABranchDifferingOnlyInCase(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	seed(t, dir)
	g(t, dir, "branch", "Feat")

	if got := caseCollision(dir, "feat"); got != "Feat" {
		t.Fatalf("want the colliding branch Feat, got %q", got)
	}
	if got := caseCollision(dir, "unrelated"); got != "" {
		t.Fatalf("want no collision, got %q", got)
	}
}

// TestDFConflictFindsBothDirections — refs/heads/feat and refs/heads/feat/42
// cannot coexist, because one is a file where the other needs a directory.
// Both directions matter: a task name can only collide downwards, but fetch
// --as takes a name with slashes in it and can collide upwards too.
func TestDFConflictFindsBothDirections(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "r")
	seed(t, dir)
	g(t, dir, "branch", "feat/42")
	g(t, dir, "branch", "release")

	if got := dfConflict(dir, "feat"); got != "feat/42" {
		t.Fatalf("a branch below the wanted name must be found, got %q", got)
	}
	if got := dfConflict(dir, "release/x"); got != "release" {
		t.Fatalf("a branch above the wanted name must be found, got %q", got)
	}
	if got := dfConflict(dir, "unrelated"); got != "" {
		t.Fatalf("want no conflict, got %q", got)
	}
}

// TestValidateRefusesADFConflictAlreadyInTheStore — a store outlives the task
// that built it. State can be lost while the store keeps every branch it ever
// held, so the workspace repository can look clear while the place the branch
// is actually created is not. Measured: git refuses at "worktree add -b feat"
// with "cannot lock ref 'refs/heads/feat': 'refs/heads/feat/42' exists" —
// after the tree has begun to be built.
func TestValidateRefusesADFConflictAlreadyInTheStore(t *testing.T) {
	c, entries := fixture(t)
	prev, err := Create(c, entries, "prev", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(c.StoreDir(), prev.Repos[0].StoreID+".git")
	g(t, storePath, "branch", "feat/42")

	_, err = Validate(c, entries, "feat", []string{"docs"})
	if code := codeOf(t, err); code != "WKT_BRANCH_DF_CONFLICT" {
		t.Fatalf("want WKT_BRANCH_DF_CONFLICT, got %q (err %v)", code, err)
	}
}

// TestValidateRefusesADFConflictInTheWorkspace covers the guard create already
// had and no test exercised.
func TestValidateRefusesADFConflictInTheWorkspace(t *testing.T) {
	c, entries := fixture(t)
	g(t, filepath.Join(c.Workspace, "docs"), "branch", "feat/42")

	_, err := Validate(c, entries, "feat", []string{"docs"})
	if code := codeOf(t, err); code != "WKT_BRANCH_DF_CONFLICT" {
		t.Fatalf("want WKT_BRANCH_DF_CONFLICT, got %q (err %v)", code, err)
	}
}

// TestAddRefusesADFConflict — add's pre-flight asked only whether the exact
// name was taken, so a hierarchical collision reached git, which failed after
// the store had been built and the base pin written.
func TestAddRefusesADFConflict(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	g(t, filepath.Join(c.Workspace, "services", "svc-a"), "branch", "feat/42")

	err := Add(c, entries, "feat", "services/svc-a")
	if code := codeOf(t, err); code != "WKT_BRANCH_DF_CONFLICT" {
		t.Fatalf("want WKT_BRANCH_DF_CONFLICT, got %q (err %v)", code, err)
	}
}

// TestAddRefusesADFConflictAlreadyInTheStore — the same question asked of the
// place the branch is actually created.
func TestAddRefusesADFConflictAlreadyInTheStore(t *testing.T) {
	c, entries := fixture(t)
	prev, err := Create(c, entries, "prev", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(c.StoreDir(), prev.Repos[0].StoreID+".git")
	g(t, storePath, "branch", "feat/42")
	if _, err := Create(c, entries, "feat", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	err = Add(c, entries, "feat", "services/svc-a")
	if code := codeOf(t, err); code != "WKT_BRANCH_DF_CONFLICT" {
		t.Fatalf("want WKT_BRANCH_DF_CONFLICT, got %q (err %v)", code, err)
	}
}

// TestFetchRefusesADFConflictBeforeMovingAnyRef — fetch checks the whole set
// before moving anything, precisely so a developer is never left holding half
// a task. A hierarchical collision escaped that check, because rev-parse of
// the wanted name reports nothing when the name is blocked by a branch
// beneath it: the plan was made, the first repository's ref moved, and the
// second failed.
func TestFetchRefusesADFConflictBeforeMovingAnyRef(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat", []string{"docs", "services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tk.Repos {
		if err := os.WriteFile(filepath.Join(r.WorktreePath, "work.md"), []byte("done\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g(t, r.WorktreePath, "add", "-A")
		g(t, r.WorktreePath, "commit", "-qm", "task work")
	}
	// The blocker sits in the second repository of the set.
	g(t, filepath.Join(c.Workspace, "services", "svc-a"), "branch", "feat/42")

	_, err = Fetch(c, "feat", "")
	if code := codeOf(t, err); code != "WKT_BRANCH_DF_CONFLICT" {
		t.Fatalf("want WKT_BRANCH_DF_CONFLICT, got %q (err %v)", code, err)
	}
	// And nothing moved in the repository that would have gone first.
	if out, _ := gitRun(filepath.Join(c.Workspace, "docs"), "rev-parse", "--verify", "--quiet", "refs/heads/feat"); strings.TrimSpace(out) != "" {
		t.Fatal("the first repository's ref moved even though the set could not complete")
	}
}

// TestFetchCarriesGitsReasonWhenTheRefCannotBeUpdated — the failure path threw
// git's explanation away and printed one guess in its place, so a developer
// hitting any other cause was told to do something irrelevant.
func TestFetchCarriesGitsReasonWhenTheRefCannotBeUpdated(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-co", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := tk.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, "work.md"), []byte("done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, wt, "add", "-A")
	g(t, wt, "commit", "-qm", "task work")
	// The developer is standing on the branch fetch wants to move.
	repo := filepath.Join(c.Workspace, "docs")
	g(t, repo, "checkout", "-q", "-b", "feat-co")

	_, err = Fetch(c, "feat-co", "")
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("want a wkt error, got %v", err)
	}
	if !strings.Contains(e.Found, "refusing to fetch") {
		t.Fatalf("git's own reason must survive; found %q", e.Found)
	}
}
