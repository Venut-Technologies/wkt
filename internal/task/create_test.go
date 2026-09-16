package task

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/store"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func g(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
	return string(out)
}

func seed(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, dir, "init", "-q")
	g(t, dir, "add", "-A")
	g(t, dir, "commit", "-qm", "init")
}

func fixture(t *testing.T) (container.C, []discover.Entry) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seed(t, filepath.Join(ws, "services", "svc-a"))
	seed(t, filepath.Join(ws, "docs"))
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
	return c, entries
}

func TestCreateBuildsMirroredTreeOnOneBranch(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-42", []string{"services/svc-a", "docs"})
	if err != nil {
		t.Fatal(err)
	}
	if len(task.Repos) != 2 {
		t.Fatalf("want 2 repos in the task, got %d", len(task.Repos))
	}
	for _, r := range task.Repos {
		br := g(t, r.WorktreePath, "rev-parse", "--abbrev-ref", "HEAD")
		if br != "feat-42\n" {
			t.Fatalf("%s is on %q, want feat-42", r.RelPath, br)
		}
		if r.StoreWorktreeName == "" {
			t.Fatalf("%s: the store worktree registration name must be recorded", r.RelPath)
		}
	}
	if _, err := os.Stat(filepath.Join(c.TreePath("feat-42"), "services", "svc-a")); err != nil {
		t.Fatalf("the tree must mirror the workspace shape: %v", err)
	}
}

// TestCreateRecordsDistinctStoreWorktreeNamesOnCollision reproduces review
// finding Important 6: two tasks selecting the same repository collide by
// construction — every task tree mirrors the workspace shape, so both
// worktrees sit at a path whose basename is the repository's own leaf name
// (".../feat-a/svc-a" and ".../feat-b/svc-a"), even though git registers
// the second one under the shared store disambiguated, as "svc-a1". The old
// code took filepath.Base of the *worktree path* — always "svc-a" for
// both — instead of reading back git's actual admin directory name, which
// repair cannot work without (spec §5.4).
func TestCreateRecordsDistinctStoreWorktreeNamesOnCollision(t *testing.T) {
	c, entries := fixture(t)
	taskA, err := Create(c, entries, "feat-a", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	entries2, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	taskB, err := Create(c, entries2, "feat-b", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}

	nameA := taskA.Repos[0].StoreWorktreeName
	nameB := taskB.Repos[0].StoreWorktreeName
	if nameA != "svc-a" {
		t.Fatalf("the first task must register under the plain leaf name, got %q", nameA)
	}
	if nameA == nameB {
		t.Fatalf("two tasks colliding on one repository must record different store worktree registration names, both got %q", nameA)
	}

	sp := filepath.Join(c.StoreDir(), taskA.Repos[0].StoreID+".git")
	if _, err := os.Stat(filepath.Join(sp, "worktrees", nameB)); err != nil {
		t.Fatalf("the recorded name %q must be git's actual admin directory under the store: %v", nameB, err)
	}
}

func TestCreateAbortsWholeSetWhenOneRepoHasADivergentBranch(t *testing.T) {
	c, entries := fixture(t)
	// docs already carries a branch of that name, pointing somewhere else.
	docs := filepath.Join(c.Workspace, "docs")
	g(t, docs, "branch", "feat-42")

	_, err := Create(c, entries, "feat-42", []string{"services/svc-a", "docs"})
	if err == nil {
		t.Fatal("create must refuse when one repository of the set has a conflicting branch")
	}
	if _, statErr := os.Stat(c.TreePath("feat-42")); !os.IsNotExist(statErr) {
		t.Fatal("a refused create must leave no tree behind")
	}
	out := g(t, filepath.Join(c.Workspace, "services", "svc-a"), "branch", "--list", "feat-42")
	if len(out) != 0 {
		t.Fatalf("rollback must remove branches created during the attempt, found %q", out)
	}
}

// TestCreateRollsBackAlreadyCreatedRepositoriesOnMidPhaseTwoFailure closes a
// gap the other two tests leave open: a divergent branch is caught by
// Validate before phase two ever starts, so it never exercises the rollback
// undo stack — a no-op rollback passes it just as well as a working one.
// Here the conflict is invisible to Validate and can only surface once
// phase two actually runs, after the first repository's store worktree,
// branch and base pin already exist. Only a real rollback removes them.
//
// The conflict is planted at the *second repository's store path*, not at
// its worktree destination under the tree root: review finding Important 5
// (fixed in the same pass as this test's adjustment) makes Create refuse up
// front whenever trees/<name> already exists, which is exactly what
// pre-populating a worktree destination under treeRoot would trip before
// Create ever reached phase two. A plain file sitting where the bare clone
// needs to go is just as invisible to Validate and just as unreachable
// until phase two runs.
func TestCreateRollsBackAlreadyCreatedRepositoriesOnMidPhaseTwoFailure(t *testing.T) {
	c, entries := fixture(t)

	treeRoot := c.TreePath("feat-42")
	docsAbs := filepath.Join(c.Workspace, "docs")
	docsStore := filepath.Join(c.StoreDir(), store.ID("docs", docsAbs)+".git")
	if err := os.WriteFile(docsStore, []byte("blocking"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Create(c, entries, "feat-42", []string{"services/svc-a", "docs"})
	if err == nil {
		t.Fatal("create must fail when a repository's store cannot be created")
	}

	if _, statErr := os.Stat(treeRoot); !os.IsNotExist(statErr) {
		t.Fatal("a refused create must leave no tree behind")
	}

	svcAbs := filepath.Join(c.Workspace, "services", "svc-a")
	svcStore := filepath.Join(c.StoreDir(), store.ID("services/svc-a", svcAbs)+".git")
	if out := g(t, svcStore, "branch", "--list", "feat-42"); len(out) != 0 {
		t.Fatalf("rollback must remove the branch already created for the first repository, found %q", out)
	}
	if out := g(t, svcStore, "worktree", "list", "--porcelain"); strings.Contains(out, "branch refs/heads/feat-42") {
		t.Fatalf("rollback must deregister the worktree already created for the first repository, found:\n%s", out)
	}
	if out := g(t, svcAbs, "for-each-ref", "refs/wkt/base/feat-42"); len(out) != 0 {
		t.Fatalf("rollback must remove the base pin already written for the first repository, found %q", out)
	}
}

// TestCreateRemovesBasePinWhenStoreEnsureFailsAfterWritingIt guards the base
// pin specifically: store.Ensure writes refs/wkt/base/<task> into the
// *workspace* repository as its unconditional first action, even when it
// then fails on a later step (here: cloning into an unwritable store
// directory). The pin undo must be registered before Ensure runs, not after
// it returns, or a failed create leaves a stray ref behind in the
// developer's own repository forever.
func TestCreateRemovesBasePinWhenStoreEnsureFailsAfterWritingIt(t *testing.T) {
	c, entries := fixture(t)
	if err := os.Chmod(c.StoreDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.StoreDir(), 0o700) })

	_, err := Create(c, entries, "feat-42", []string{"services/svc-a"})
	if err == nil {
		t.Fatal("create must fail when the store directory cannot be written to")
	}

	svcAbs := filepath.Join(c.Workspace, "services", "svc-a")
	if out := g(t, svcAbs, "for-each-ref", "refs/wkt/base/feat-42"); len(out) != 0 {
		t.Fatalf("rollback must remove the base pin even when store.Ensure fails after writing it, found %q", out)
	}
}

// TestCreateRollsBackBranchWhenWorktreeAddFails reproduces review finding
// Important 4: "worktree add -b" creates the branch as a side effect before
// it checks anything out, and can still fail on the checkout itself. The
// undo that deletes that branch used to be registered only after "worktree
// add" returned successfully, so a checkout failure left the branch behind
// forever and the task name permanently unusable (WKT_BRANCH_EXISTS on
// every later Create attempt).
//
// The failure is fabricated via plumbing rather than a pre-existing,
// non-empty destination directory: review finding Important 5 (fixed
// separately, in the same pass) makes Create refuse up front whenever
// trees/<name> already exists, which would trip before ever reaching
// "worktree add" if the destination were pre-populated. Instead, the base
// commit's own tree is given an entry literally named ".git" — git refuses
// to check that out ("invalid path '.git'") — built with hash-object /
// mktree / commit-tree so no working tree, anywhere, ever holds it; the
// local filesystem's own refusal to create a directory named ".git" is
// never in the path. Confirmed empirically against real git before writing
// this test: the branch is created and "worktree add" still exits non-zero.
func TestCreateRollsBackBranchWhenWorktreeAddFails(t *testing.T) {
	c, _ := fixture(t)
	repo := filepath.Join(c.Workspace, "docs")

	hashObj := exec.Command("git", "hash-object", "-w", "--stdin")
	hashObj.Dir = repo
	hashObj.Stdin = strings.NewReader("malicious\n")
	blobOut, err := hashObj.Output()
	if err != nil {
		t.Fatalf("git hash-object: %v", err)
	}
	blob := strings.TrimSpace(string(blobOut))

	mktree := exec.Command("git", "mktree")
	mktree.Dir = repo
	mktree.Stdin = strings.NewReader("100644 blob " + blob + "\t.git\n")
	treeOut, err := mktree.Output()
	if err != nil {
		t.Fatalf("git mktree: %v", err)
	}
	tree := strings.TrimSpace(string(treeOut))

	commit := strings.TrimSpace(g(t, repo, "commit-tree", tree, "-m", "evil tree with a .git entry"))
	g(t, repo, "update-ref", "refs/heads/main", commit)

	entries2, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries2, "feat-leak", []string{"docs"}); err == nil {
		t.Fatal("create must fail when the base commit cannot be checked out")
	}

	docsStore := filepath.Join(c.StoreDir(), store.ID("docs", repo)+".git")
	if out := g(t, docsStore, "branch", "--list", "feat-leak"); len(out) != 0 {
		t.Fatalf("a failed worktree add must not leak the branch git created before the checkout failed, found %q", out)
	}

	entries3, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries3, "feat-leak", []string{"services/svc-a"}); err != nil {
		t.Fatalf("a name freed by a failed create's rollback must be reusable, got: %v", err)
	}
}

// TestCreateRefusesAndDoesNotTouchAPreExistingTreeDirectory reproduces
// review finding Important 5's exact scenario: os.MkdirAll succeeds
// silently on a directory that already exists, and the old code then
// registered an unconditional os.RemoveAll(treeRoot) rollback undo
// regardless — so a pre-existing trees/<name>/ with unrelated content (a
// stale leftover, or simply a name collision with something that isn't a
// wkt tree at all) was destroyed once create failed for any reason.
//
// The second repository's own worktree destination is pre-populated too
// (the same conflict TestCreateRollsBackAlreadyCreatedRepositoriesOnMidPhaseTwoFailure
// used before it was adjusted for this same finding), so that against the
// pre-fix code this doesn't merely fail to refuse: phase two runs, the
// first repository's store worktree, branch and base pin all get created,
// the second repository's "worktree add" then fails on the pre-existing
// conflict, and the unconditional rollback os.RemoveAll(treeRoot) destroys
// "unrelated.txt" along with everything else. The fix must refuse before
// any of that happens at all.
func TestCreateRefusesAndDoesNotTouchAPreExistingTreeDirectory(t *testing.T) {
	c, entries := fixture(t)

	treeRoot := c.TreePath("feat-preexisting")
	blocked := filepath.Join(treeRoot, "docs")
	if err := os.MkdirAll(blocked, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "stray.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(treeRoot, "unrelated.txt")
	if err := os.WriteFile(unrelated, []byte("not wkt's to touch\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Create(c, entries, "feat-preexisting", []string{"services/svc-a", "docs"})
	if err == nil {
		t.Fatal("create must refuse when the task tree directory already exists")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_TREE_EXISTS" {
		t.Fatalf("expected WKT_TREE_EXISTS, got %v", err)
	}
	if _, statErr := os.Stat(unrelated); statErr != nil {
		t.Fatal("a refused create must never delete content it did not create")
	}
	svcAbs := filepath.Join(c.Workspace, "services", "svc-a")
	if out := g(t, svcAbs, "for-each-ref", "refs/wkt/base/feat-preexisting"); len(out) != 0 {
		t.Fatalf("refusing up front must mean nothing was ever created for either repository, found pin %q", out)
	}
}

// TestValidateRejectsATaskNameThatIsNotOneSafePathSegment covers adversarial
// finding F1. "feature/x" is a perfectly valid *branch* name, so
// check-ref-format waves it through — but the task name is also a path
// segment (trees/<name>, state/tasks/<name>.json), and a name carrying a
// separator made Create build the tree, fail at the state write, roll back,
// and leave an empty trees/feature behind that then blocked the plain name
// "feature" forever.
func TestValidateRejectsATaskNameThatIsNotOneSafePathSegment(t *testing.T) {
	c, entries := fixture(t)
	for _, name := range []string{"feature/x", "a/b/c", "sub/dir/task", "with\\backslash"} {
		_, err := Validate(c, entries, name, []string{"docs"})
		if err == nil {
			t.Fatalf("%q must be refused: the task name is a path segment", name)
		}
		var e *wkterr.E
		if !errors.As(err, &e) || e.Code != "WKT_BAD_TASK_NAME" {
			t.Fatalf("%q: got %v, want WKT_BAD_TASK_NAME", name, err)
		}
	}
}

// TestValidateStillAcceptsOrdinaryTaskNames pins the other side of F1's fix:
// the guard must reject separators, not tighten the name rules generally.
func TestValidateStillAcceptsOrdinaryTaskNames(t *testing.T) {
	c, entries := fixture(t)
	for _, name := range []string{"feat-42", "feat_42", "FEAT.42", "задача"} {
		if _, err := Validate(c, entries, name, []string{"docs"}); err != nil {
			t.Fatalf("%q must be accepted, got %v", name, err)
		}
	}
}

// TestSubmoduleWarningsNamesEverySelectedRepositoryWithASubmodule covers
// adversarial finding F3. Spec §5.7 requires wkt new to warn while the
// submodule route is unimplemented, because rm refuses on a submodule even
// with --force: creating such a task silently produces one that cannot be
// removed by any wkt command at all.
func TestSubmoduleWarningsNamesEverySelectedRepositoryWithASubmodule(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seed(t, filepath.Join(ws, "plain"))
	seed(t, filepath.Join(base, "lib"))
	seed(t, filepath.Join(ws, "super"))
	g(t, filepath.Join(ws, "super"), "-c", "protocol.file.allow=always",
		"submodule", "add", "-q", filepath.Join(base, "lib"), "vendor")
	g(t, filepath.Join(ws, "super"), "commit", "-qm", "add submodule")

	entries, err := discover.Walk(ws, 4)
	if err != nil {
		t.Fatal(err)
	}
	warned := SubmoduleWarnings(entries, []string{"plain", "super"})
	if len(warned) != 1 || warned[0].Repo != "super" {
		t.Fatalf("want exactly one warning naming super, got %+v", warned)
	}
	if warned[0].Code != "WKT_SUBMODULE" {
		t.Fatalf("warning must carry WKT_SUBMODULE, got %q", warned[0].Code)
	}
	if SubmoduleWarnings(entries, []string{"plain"}) != nil {
		t.Fatal("a selection without submodules must warn about nothing")
	}
}

// TestCreateWritesThePerimeter — spec §9 puts the generator before new in the
// build order precisely because "new writes the file, so it cannot come
// later". A task without a perimeter is a task that silently has none.
func TestCreateWritesThePerimeter(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-p", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	tree := c.TreePath("feat-p")
	for _, dir := range []string{tree, filepath.Join(tree, "docs")} {
		f := filepath.Join(dir, ".claude", "settings.json")
		if _, statErr := os.Stat(f); statErr != nil {
			t.Fatalf("no perimeter at %s: %v", f, statErr)
		}
	}
	if len(task.PerimeterCoverage) != 2 {
		t.Fatalf("state must record what is covered, got %v", task.PerimeterCoverage)
	}
	if len(task.PerimeterHashes) != 2 {
		t.Fatalf("state must record a hash per copy, got %v", task.PerimeterHashes)
	}
	// The recorded state has to survive the round trip, or status reads back
	// an empty coverage and reports every task as uncovered.
	reloaded, err := state.Load(c.StateDir(), "feat-p")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.PerimeterCoverage) != 2 || len(reloaded.PerimeterHashes) != 2 {
		t.Fatalf("coverage must round-trip through state: %+v", reloaded)
	}
}

// TestCreatingASecondTaskRefreshesTheFirstsPerimeter — the deny list names
// sibling trees individually (H16: a wide glob with a narrower allow does not
// work), so a task created later is invisible to an older perimeter until it
// is regenerated.
func TestCreatingASecondTaskRefreshesTheFirstsPerimeter(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-first", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(c.TreePath("feat-first"), ".claude", "settings.json")
	before, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(before), "feat-second") {
		t.Fatal("the first task cannot already know about a task that does not exist")
	}

	if _, err := Create(c, entries, "feat-second", []string{"services/svc-a"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(first)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(after), "feat-second") {
		t.Fatalf("the first task's perimeter must be refreshed to name the new sibling:\n%s", after)
	}
	// And its recorded hash must match what is now on disk, or status will
	// report drift the moment a second task exists.
	reloaded, err := state.Load(c.StateDir(), "feat-first")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(after)
	if reloaded.PerimeterHashes[c.TreePath("feat-first")] != hex.EncodeToString(sum[:]) {
		t.Fatal("refreshing a sibling's perimeter must update its recorded hash too")
	}
}

// TestASiblingRefreshFailureDoesNotFailTheNewTask — the new task is correct;
// the older one is merely stale, which is what status is for. Failing the
// creation would make one unwritable directory block every future task.
func TestASiblingRefreshFailureDoesNotFailTheNewTask(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-stale", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	// Make the older task's perimeter directory unwritable.
	dir := filepath.Join(c.TreePath("feat-stale"), ".claude")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)

	if _, err := Create(c, entries, "feat-new", []string{"services/svc-a"}); err != nil {
		t.Fatalf("a stale sibling must not block a new task: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.TreePath("feat-new"), ".claude", "settings.json")); err != nil {
		t.Fatalf("the new task's own perimeter must still be written: %v", err)
	}
}

// TestRollbackTakesThePerimeterWithIt — the copies live inside the tree, so
// the tree's own undo removes them; this pins that the undo really does run
// before anything claims the task failed cleanly.
func TestRollbackTakesThePerimeterWithIt(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-doomed", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-doomed", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.TreePath("feat-doomed")); !os.IsNotExist(err) {
		t.Fatal("the tree, and with it every perimeter copy, must be gone")
	}
}

// TestWorktreeAddFailureCarriesGitsReason — a checkout that runs a content
// filter fails inside "worktree add", and wkt used to report only "cannot
// create the worktree", with git's explanation thrown away. That is the
// git-lfs shape: "git lfs install" writes filter.lfs.* into the *global*
// config, which the store does see, while the object cache it needs does not
// come with the store.
func TestWorktreeAddFailureCarriesGitsReason(t *testing.T) {
	c, entries := fixture(t)

	// A required filter in global config, failing the way git-lfs does when it
	// cannot produce the object. Local config would not reproduce this: wkt
	// deliberately never copies filter.* into the store.
	// Commit the filtered path first, with no filter configured: the file is
	// already in history by the time the developer installs the tool that
	// filters it. (Configuring it first fails the *clean* step instead, which
	// is a different failure and not the one under test.)
	repo := filepath.Join(c.Workspace, "docs")
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

	// Now the filter exists, in global config — which is where "git lfs
	// install" puts it, and which the store does see. Only a smudge driver,
	// so this fails on checkout, exactly as git-lfs does when it cannot fetch
	// the objects.
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(globalCfg, []byte(
		"[filter \"leaky\"]\n"+
			"\tsmudge = sh -c 'echo fetching objects >&2; exit 3' --token=glpat-SECRET %f\n"+
			"\trequired = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	_, err := Create(c, entries, "feat-filter", []string{"docs"})
	if err == nil {
		t.Fatal("a required filter that cannot run must fail the checkout, not produce a silent tree")
	}
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("want a typed error, got %v", err)
	}
	if e.Found == "" {
		t.Fatalf("the failure must carry git's reason, or nobody can tell a filter from a path collision: %+v", e)
	}
	if !strings.Contains(e.Found, "filter") {
		t.Fatalf("git named a filter; the error must say so: %+v", e)
	}
	if strings.Contains(e.Found, "glpat-SECRET") {
		t.Fatalf("and must not carry the user's secrets with it: %+v", e)
	}
	// The failed create still leaves nothing behind.
	if _, statErr := os.Stat(c.TreePath("feat-filter")); !os.IsNotExist(statErr) {
		t.Fatal("a failed create must roll back its tree")
	}
}

// TestCreateCarriesGitignoredFiles — reported as issue #3. The design scopes
// in a gitignored-file carry and it did not exist, so a fresh tree had no
// .env and no way to get one: every new task started unable to run anything
// that needs local configuration.
func TestCreateCarriesGitignoredFiles(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "docs")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore .env")
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace, ".wktinclude"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-carry", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	carried := filepath.Join(task.Repos[0].WorktreePath, ".env")
	body, err := os.ReadFile(carried)
	if err != nil {
		t.Fatalf("the tree must have the file the service needs: %v", err)
	}
	if string(body) != "TOKEN=local\n" {
		t.Fatalf("contents: %q", body)
	}
	// Recorded, so teardown can tell it apart from work.
	var slot *state.LinkSlot
	for i := range task.Links {
		if strings.HasSuffix(task.Links[i].RelPath, ".env") {
			slot = &task.Links[i]
		}
	}
	if slot == nil || slot.Type != "carry" || slot.Hash == "" {
		t.Fatalf("a carried file must be recorded with its hash: %+v", task.Links)
	}
}

// TestAnUntouchedCarriedFileDoesNotBlockRemoval — the copy is identical to the
// developer's own, so removing the tree loses nothing and refusing would make
// --force reflexive for every task that carries a secret.
func TestAnUntouchedCarriedFileDoesNotBlockRemoval(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "docs")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore .env")
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace, ".wktinclude"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries, "feat-untouched", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-untouched", false); err != nil {
		t.Fatalf("an untouched carried file must not block removal: %v", err)
	}
}

// TestAnEditedCarriedFileBlocksRemoval — the other half. Once the task has
// changed it, the copy holds something the developer's own file does not.
func TestAnEditedCarriedFileBlocksRemoval(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "docs")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore .env")
	if err := os.WriteFile(filepath.Join(repo, ".env"), []byte("TOKEN=local\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Workspace, ".wktinclude"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	task, err := Create(c, entries, "feat-edited", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(task.Repos[0].WorktreePath, ".env"),
		[]byte("TOKEN=changed-in-the-task\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-edited", false); err == nil {
		t.Fatal("a carried file the task edited must block removal")
	}
}
