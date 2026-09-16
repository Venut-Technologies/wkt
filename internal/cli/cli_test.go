package cli

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func seedRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "init"}} {
		full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
}

func TestInitNewPathRmRoundTrip(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "services", "svc-a"))
	seedRepo(t, filepath.Join(ws, "docs"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	if code := Run([]string{"new", "feat-42", "--workspace", ws, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	out.Reset()
	if code := Run([]string{"path", "feat-42", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("path exited %d: %s", code, errb.String())
	}
	treePath := strings.TrimSpace(out.String())
	if _, err := os.Stat(filepath.Join(treePath, "services", "svc-a")); err != nil {
		t.Fatalf("path must point at a materialised mirrored tree: %v", err)
	}
	out.Reset()
	if code := Run([]string{"rm", "feat-42", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("rm on a clean task exited %d: %s", code, errb.String())
	}
	if _, err := os.Stat(treePath); !os.IsNotExist(err) {
		t.Fatal("rm must remove a clean tree")
	}
}

func TestNewOnExistingTaskExitsTwo(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))
	var out, errb bytes.Buffer
	Run([]string{"init", "--workspace", ws}, &out, &errb)
	Run([]string{"new", "t1", "--workspace", ws, "--all"}, &out, &errb)
	if code := Run([]string{"new", "t1", "--workspace", ws, "--all"}, &out, &errb); code != 2 {
		t.Fatalf("a duplicate task must exit 2, got %d", code)
	}
}

// --- exit-code contract, exercised beyond "not zero" ---

// TestFailMapsErrorCodesToContractExitCodes locks the fail() mapping table
// down directly: each wkterr code must land on its documented exit code, not
// merely "something non-zero". A regression that mapped every error to 1
// (or every error to 2) would still pass a test that only checked "!= 0".
func TestFailMapsErrorCodesToContractExitCodes(t *testing.T) {
	cases := []struct {
		code string
		want int
	}{
		{"WKT_TASK_EXISTS", 2},
		{"WKT_NO_CONTAINER", 4},
		{"WKT_TREE_MISSING", 3},
		{"WKT_NO_TASK", 1},
		{"WKT_DIRTY", 1},
		{"WKT_GIT_FAILED", 1},
	}
	for _, c := range cases {
		var errb bytes.Buffer
		got := fail(&errb, wkterr.New(c.code, "x"))
		if got != c.want {
			t.Errorf("fail(%s) = %d, want %d", c.code, got, c.want)
		}
		if errb.Len() == 0 {
			t.Errorf("fail(%s) wrote nothing to stderr", c.code)
		}
		if strings.Contains(errb.String(), "\t") {
			t.Errorf("fail(%s) leaked a raw-looking payload: %s", c.code, errb.String())
		}
	}
}

// TestStatusInfoSeverityDoesNotTriggerDrift is correction 2: an ordinary
// regenerable directory like node_modules must be reported (so --force
// doesn't become reflexive) but must NOT flip status's exit code to 3. A
// status loop that set drift on every Preflight entry — info or not — would
// fail this test; one that filtered by severity passes it.
func TestStatusInfoSeverityDoesNotTriggerDrift(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	wsRepo := filepath.Join(ws, "svc-a")
	seedRepo(t, wsRepo)

	// node_modules must be gitignored for git to report it as "!!" ignored,
	// which is what Preflight's regenerable classifier keys on. The
	// .gitignore is committed into the *workspace* repository, before "new"
	// resolves the task's base — so it becomes part of the base commit
	// itself rather than an unpushed commit made inside the tree (which
	// would introduce its own, unrelated WKT_UNPUSHED blocker and confound
	// this test's claim that ONLY the info-severity entry is present).
	if err := os.WriteFile(filepath.Join(wsRepo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "ignore node_modules"}} {
		full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = wsRepo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	if code := Run([]string{"new", "t-info", "--workspace", ws, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	out.Reset()
	Run([]string{"path", "t-info", "--workspace", ws}, &out, &errb)
	treePath := strings.TrimSpace(out.String())

	repoDir := filepath.Join(treePath, "svc-a")
	if err := os.MkdirAll(filepath.Join(repoDir, "node_modules", "leftpad"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "node_modules", "leftpad", "index.js"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{"status", "t-info", "--workspace", ws}, &out, &errb)
	if code != 0 {
		t.Fatalf("status with only regenerable ignored content exited %d, want 0: stdout=%s stderr=%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "WKT_REGENERABLE_IGNORED") {
		t.Fatalf("status must still report the regenerable entry, got: %s", out.String())
	}
}

// TestStatusRealBlockerTriggersDriftExitThree is the other half of
// correction 2: a genuine blocker (an uncommitted change) must still flip
// status to exit 3. Without this paired test, a status loop that never sets
// drift at all would also pass the info-severity test above.
func TestStatusRealBlockerTriggersDriftExitThree(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))

	var out, errb bytes.Buffer
	Run([]string{"init", "--workspace", ws}, &out, &errb)
	Run([]string{"new", "t-dirty", "--workspace", ws, "--all"}, &out, &errb)
	out.Reset()
	Run([]string{"path", "t-dirty", "--workspace", ws}, &out, &errb)
	treePath := strings.TrimSpace(out.String())

	if err := os.WriteFile(filepath.Join(treePath, "svc-a", "f.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{"status", "t-dirty", "--workspace", ws}, &out, &errb)
	if code != 3 {
		t.Fatalf("status with an uncommitted change exited %d, want 3: stdout=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "WKT_DIRTY") {
		t.Fatalf("status must report the dirty blocker, got: %s", out.String())
	}
}

// TestPathFailsWhenTreeMissingFromDisk is correction 3: state can still have
// a record of the task after its tree directory is gone from disk (e.g.
// deleted outside wkt). path must refuse rather than print a path to
// nothing — and, crucially, must not merely print the path anyway.
func TestPathFailsWhenTreeMissingFromDisk(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))

	var out, errb bytes.Buffer
	Run([]string{"init", "--workspace", ws}, &out, &errb)
	Run([]string{"new", "t-gone", "--workspace", ws, "--all"}, &out, &errb)
	out.Reset()
	Run([]string{"path", "t-gone", "--workspace", ws}, &out, &errb)
	treePath := strings.TrimSpace(out.String())
	if treePath == "" {
		t.Fatal("setup: expected a tree path")
	}

	if err := os.RemoveAll(treePath); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{"path", "t-gone", "--workspace", ws}, &out, &errb)
	if code == 0 {
		t.Fatalf("path must not exit 0 once the tree is gone from disk, got stdout=%q", out.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("path must not print a path to nothing, got stdout=%q", out.String())
	}
	if code != 3 {
		t.Fatalf("path on a state/disk mismatch must exit 3 (drift), got %d", code)
	}
}

// TestRmRefusesOnDirtyTreeExitsOne mirrors the acceptance battery's
// destructive-cleanup test: a plain rm on a tree with uncommitted work must
// refuse (exit 1, not 0, not silently succeed) and must leave the tree
// untouched.
func TestRmRefusesOnDirtyTreeExitsOne(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "docs"))

	var out, errb bytes.Buffer
	Run([]string{"init", "--workspace", ws}, &out, &errb)
	Run([]string{"new", "t-refuse", "--workspace", ws, "--all"}, &out, &errb)
	out.Reset()
	Run([]string{"path", "t-refuse", "--workspace", ws}, &out, &errb)
	treePath := strings.TrimSpace(out.String())

	if err := os.WriteFile(filepath.Join(treePath, "docs", "untracked.md"), []byte("draft\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	code := Run([]string{"rm", "t-refuse", "--workspace", ws}, &out, &errb)
	if code != 1 {
		t.Fatalf("rm on a dirty tree exited %d, want 1", code)
	}
	if _, err := os.Stat(treePath); err != nil {
		t.Fatalf("rm must not remove a dirty tree: %v", err)
	}
}

// TestCreateAndCleanupAreDocumentedAliases checks the acceptance battery's
// two required verbs actually reach new/rm, not merely that some command
// named "create" exists and returns 2 for anything.
func TestCreateAndCleanupAreDocumentedAliases(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))

	var out, errb bytes.Buffer
	Run([]string{"init", "--workspace", ws}, &out, &errb)
	out.Reset()
	if code := Run([]string{"create", "t-alias", "--workspace", ws, "--all"}, &out, &errb); code != 0 {
		t.Fatalf("create exited %d: %s", code, errb.String())
	}
	out.Reset()
	if code := Run([]string{"path", "t-alias", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("path after create exited %d: %s", code, errb.String())
	}
	treePath := strings.TrimSpace(out.String())
	if _, err := os.Stat(treePath); err != nil {
		t.Fatalf("create must materialise a tree just like new: %v", err)
	}
	out.Reset()
	if code := Run([]string{"cleanup", "t-alias", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("cleanup exited %d: %s", code, errb.String())
	}
	if _, err := os.Stat(treePath); !os.IsNotExist(err) {
		t.Fatal("cleanup must remove the tree just like rm")
	}
}

// TestUsageErrorsExitTwo checks the "2 = usage error" half of the contract
// with real malformed invocations rather than the happy path.
func TestUsageErrorsExitTwo(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{}, &out, &errb); code != 2 {
		t.Fatalf("no args exited %d, want 2", code)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"bogus-command"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command exited %d, want 2", code)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new"}, &out, &errb); code != 2 {
		t.Fatalf("new with no task name exited %d, want 2", code)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "t", "--not-a-real-flag"}, &out, &errb); code != 2 {
		t.Fatalf("an unparsable flag set exited %d, want 2", code)
	}
}

// --- review round 2 ---

// TestPathAndRmRequireATaskNameExitTwo is minor fix 4: an empty task name on
// path or rm is a usage error, exactly like new, not whatever incidental
// error state.Load or task.Remove happens to produce for an empty name.
func TestPathAndRmRequireATaskNameExitTwo(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"path", "--workspace", ws}, &out, &errb); code != 2 {
		t.Fatalf("path with no task name exited %d, want 2", code)
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"rm", "--workspace", ws}, &out, &errb); code != 2 {
		t.Fatalf("rm with no task name exited %d, want 2", code)
	}
}

// TestUninitialisedContainerExitsFour is Important fix 1: new, path, status
// and rm against a workspace that was never `wkt init`-ed must all exit 4,
// not whatever incidental error each command happens to hit first. This is
// an integration test against real commands, not just a check on fail()'s
// mapping table — it would have caught the original bug (status silently
// exiting 0, new/path/rm exiting 1) that the mapping-only test could not.
func TestUninitialisedContainerExitsFour(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))
	// Deliberately no "init" call.

	cases := [][]string{
		{"new", "t1", "--workspace", ws, "--all"},
		{"path", "t1", "--workspace", ws},
		{"status", "--workspace", ws},
		{"rm", "t1", "--workspace", ws},
	}
	for _, args := range cases {
		var out, errb bytes.Buffer
		code := Run(args, &out, &errb)
		if code != 4 {
			t.Errorf("%v against an uninitialised container exited %d, want 4: stdout=%q stderr=%q",
				args, code, out.String(), errb.String())
		}
		if !strings.Contains(errb.String(), "WKT_NO_CONTAINER") {
			t.Errorf("%v: stderr must report WKT_NO_CONTAINER, got %q", args, errb.String())
		}
	}
}

// TestInitRefusesNonexistentWorkspace and TestInitRefusesWorkspaceWithNoRepos
// are Important fix 2: init must not silently succeed and create an empty
// container for a workspace that plainly isn't one — a typo in --workspace
// looks exactly like success otherwise, since discover.Walk swallows a
// root-level walk error and simply returns zero entries.
func TestInitRefusesNonexistentWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "does-not-exist")

	var out, errb bytes.Buffer
	code := Run([]string{"init", "--workspace", ws}, &out, &errb)
	if code == 0 {
		t.Fatalf("init on a nonexistent workspace exited 0, want a typed failure; stdout=%q", out.String())
	}
	if _, err := os.Stat(ws + ".worktrees"); !os.IsNotExist(err) {
		t.Fatal("init must not create a container for a workspace that does not exist")
	}
}

func TestInitRefusesWorkspaceWithNoRepos(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errb bytes.Buffer
	code := Run([]string{"init", "--workspace", ws}, &out, &errb)
	if code == 0 {
		t.Fatalf("init on a workspace with zero repositories exited 0, want a typed failure; stdout=%q", out.String())
	}
	if _, err := os.Stat(ws + ".worktrees"); !os.IsNotExist(err) {
		t.Fatal("init must not create a container for a workspace with nothing to materialise")
	}
}

// TestNewAcceptsFlagsBeforeOrAfterTheTaskName is Important fix 3: the task
// name may be typed before its flags or after them. Each subtest checks the
// flags actually took effect (a materialised tree under the given
// --workspace, a repo selected by --all), not merely that the exit code was
// 0 — a version that silently ignored --workspace and operated on "." could
// still exit 0 while doing the wrong thing entirely.
func TestNewAcceptsFlagsBeforeOrAfterTheTaskName(t *testing.T) {
	positionalFirst := func(t *testing.T, ws, task string) {
		var out, errb bytes.Buffer
		if code := Run([]string{"new", task, "--workspace", ws, "--all"}, &out, &errb); code != 0 {
			t.Fatalf("positional-first order exited %d: %s", code, errb.String())
		}
	}
	flagsFirst := func(t *testing.T, ws, task string) {
		var out, errb bytes.Buffer
		if code := Run([]string{"new", "--all", "--workspace", ws, task}, &out, &errb); code != 0 {
			t.Fatalf("flags-first order exited %d: %s", code, errb.String())
		}
	}

	for name, create := range map[string]func(t *testing.T, ws, task string){
		"positional-first": positionalFirst,
		"flags-first":      flagsFirst,
	} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			ws := filepath.Join(base, "ws")
			seedRepo(t, filepath.Join(ws, "svc-a"))
			var out, errb bytes.Buffer
			if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
				t.Fatalf("init exited %d: %s", code, errb.String())
			}

			create(t, ws, "t-order")

			out.Reset()
			errb.Reset()
			if code := Run([]string{"path", "t-order", "--workspace", ws}, &out, &errb); code != 0 {
				t.Fatalf("path exited %d: %s", code, errb.String())
			}
			// Proves both flags actually took effect, not merely that the
			// exit code was 0: if --workspace were silently ignored (falling
			// back to "."), svc-a — created only under this test's isolated
			// tempdir — would not exist under whatever tree got built, and
			// if --all were ignored, no repository would be selected at all.
			treePath := strings.TrimSpace(out.String())
			if _, err := os.Stat(filepath.Join(treePath, "svc-a")); err != nil {
				t.Fatalf("--workspace and/or --all was not honoured (svc-a missing from %q): %v", treePath, err)
			}
		})
	}
}

// TestUsageStringDoesNotAdvertiseJSON is minor fix 5: --json was advertised
// but never implemented as a flag on any command, so passing it fails with
// "flag provided but not defined". The usage text must not promise it.
func TestUsageStringDoesNotAdvertiseJSON(t *testing.T) {
	if strings.Contains(usage, "json") {
		t.Fatalf("usage text still advertises --json: %q", usage)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"status", "--json"}, &out, &errb); code != 2 {
		t.Fatalf("--json exited %d, want 2 (an unrecognised flag is a usage error)", code)
	}
}

// --- review round 3 ---

// newSplitFlagsWorkspace seeds a two-repository workspace and runs `wkt
// init` against it, returning the workspace path. Two repositories are
// essential here, not one: with only one repository, --repos and --all
// select the same tree, and a version that silently ignored --repos would
// pass undetected.
func newSplitFlagsWorkspace(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "svc-a"))
	seedRepo(t, filepath.Join(ws, "svc-b"))
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	return ws
}

// assertOnlySvcA runs `wkt path` for the task and asserts the materialised
// tree contains svc-a and does NOT contain svc-b — proving --repos svc-a
// was actually honoured, not merely that the command exited 0. Exit 0 alone
// is not enough: the round 3 regression exited 0 while silently
// materialising both repositories instead of the one requested.
func assertOnlySvcA(t *testing.T, ws, task string) {
	t.Helper()
	var out, errb bytes.Buffer
	if code := Run([]string{"path", task, "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("path exited %d: %s", code, errb.String())
	}
	treePath := strings.TrimSpace(out.String())

	// An unselected repository is still present in the tree — as a
	// back-filled symlink to the workspace, per tree.PlanFor's design, so
	// cross-repo references still resolve — so "svc-b must not exist" is
	// the wrong assertion (and was, in an earlier draft of this test,
	// incorrectly red against CORRECT code). The real distinction --repos
	// makes is real materialised worktree (a directory, on the task
	// branch) vs. back-fill symlink (unmodified, still on main).
	aInfo, err := os.Lstat(filepath.Join(treePath, "svc-a"))
	if err != nil {
		t.Fatalf("--repos svc-a must materialise svc-a: %v", err)
	}
	if aInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("svc-a must be a real materialised worktree, not a back-fill symlink")
	}
	if branch, err := exec.Command("git", "-C", filepath.Join(treePath, "svc-a"), "rev-parse", "--abbrev-ref", "HEAD").Output(); err != nil || strings.TrimSpace(string(branch)) != task {
		t.Fatalf("svc-a must be checked out on the task branch %q, got %q (err=%v)", task, strings.TrimSpace(string(branch)), err)
	}

	bInfo, err := os.Lstat(filepath.Join(treePath, "svc-b"))
	if err != nil {
		t.Fatalf("svc-b should still be present as a back-fill symlink: %v", err)
	}
	if bInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("--repos svc-a must NOT materialise svc-b as a real worktree; it must remain a back-fill symlink")
	}
}

// TestNewHonoursReposFlagRegardlessOfPositionalPlacement is the round 3 fix:
// --repos must take effect no matter which side of the task name it's typed
// on, including split across both sides of it — the shape that regressed
// ("new --workspace WS task --repos svc-a" silently selected every
// repository instead of refusing or honouring --repos).
func TestNewHonoursReposFlagRegardlessOfPositionalPlacement(t *testing.T) {
	t.Run("positional-first", func(t *testing.T) {
		ws := newSplitFlagsWorkspace(t)
		var out, errb bytes.Buffer
		if code := Run([]string{"new", "task", "--workspace", ws, "--repos", "svc-a"}, &out, &errb); code != 0 {
			t.Fatalf("new exited %d: %s", code, errb.String())
		}
		assertOnlySvcA(t, ws, "task")
	})

	t.Run("flags-first", func(t *testing.T) {
		ws := newSplitFlagsWorkspace(t)
		var out, errb bytes.Buffer
		if code := Run([]string{"new", "--workspace", ws, "--repos", "svc-a", "task"}, &out, &errb); code != 0 {
			t.Fatalf("new exited %d: %s", code, errb.String())
		}
		assertOnlySvcA(t, ws, "task")
	})

	// This is the shape that regressed in round 2: flags on both sides of
	// the positional. Before this fix it exited 0 and silently materialised
	// every repository in the workspace (svc-a AND svc-b) instead of
	// honouring --repos svc-a — the worst-direction failure (a safe refusal
	// traded for a silent wrong result). Verified live against the round 2
	// binary before writing this fix; see the task report for the
	// transcript.
	t.Run("split-across-the-positional", func(t *testing.T) {
		ws := newSplitFlagsWorkspace(t)
		var out, errb bytes.Buffer
		if code := Run([]string{"new", "--workspace", ws, "task", "--repos", "svc-a"}, &out, &errb); code != 0 {
			t.Fatalf("new exited %d: %s", code, errb.String())
		}
		assertOnlySvcA(t, ws, "task")
	})
}

// TestNewRefusesTwoPositionalsExitsTwo is the belt-and-braces half of the
// round 3 fix: a shape splitPositional cannot safely resolve to one
// positional (here, two bare tokens) must be refused, not silently resolved
// by picking the first and discarding the second.
func TestNewRefusesTwoPositionalsExitsTwo(t *testing.T) {
	ws := newSplitFlagsWorkspace(t)
	var out, errb bytes.Buffer
	code := Run([]string{"new", "task1", "task2", "--workspace", ws, "--all"}, &out, &errb)
	if code != 2 {
		t.Fatalf("two positionals exited %d, want 2: stdout=%q stderr=%q", code, out.String(), errb.String())
	}
	// And nothing must have been created under either name.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"path", "task1", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("a refused two-positional new must not have created task1")
	}
}

// TestSplitPositionalDerivesValueFlagsFromTheFlagSet is review finding
// Minor 9: which flags consume a separately-typed value used to be a
// hand-maintained map, independent of the FlagSet actually being parsed. A
// future flag added to the FlagSet but forgotten in that map silently
// reintroduces the exact round-2 bug (a value flag's argument mistaken for
// the positional, or vice versa) — which already happened once on this
// branch. Registers a brand-new string flag, "--extra", that has never
// appeared in any hardcoded list anywhere, and checks that its
// separately-typed value is still skipped correctly purely because
// splitPositional now derives the classification from fs.VisitAll.
func TestSplitPositionalDerivesValueFlagsFromTheFlagSet(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.String("workspace", ".", "")
	fs.Bool("all", false, "")
	fs.String("extra", "", "a brand-new value flag, not in any hand-maintained list")

	positional, remaining := splitPositional(fs, []string{"--extra", "not-the-positional", "task", "--all"})
	if positional != "task" {
		t.Fatalf("got positional %q, want %q — the new value flag's argument must be skipped, not mistaken for the positional", positional, "task")
	}
	wantRemaining := []string{"--extra", "not-the-positional", "--all"}
	if strings.Join(remaining, ",") != strings.Join(wantRemaining, ",") {
		t.Fatalf("got remaining %v, want %v", remaining, wantRemaining)
	}

	// A boolean flag's own argument, by contrast, must never be skipped: it
	// takes no separately-typed value, so the very next token is fair game
	// as the positional.
	positional2, _ := splitPositional(fs, []string{"--all", "task2"})
	if positional2 != "task2" {
		t.Fatalf("got positional %q, want %q — a boolean flag must not swallow the following token", positional2, "task2")
	}
}

// TestNewWithASeparatorInTheTaskNameCreatesNothing covers adversarial
// finding F1 end to end: the refusal must happen before anything is built,
// so no debris is left in trees/ to block a later, legitimate task name.
func TestNewWithASeparatorInTheTaskNameCreatesNothing(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "feature/x", "--all", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatalf("new must refuse a task name carrying a path separator; stderr=%s", errb.String())
	}
	if !strings.Contains(errb.String(), "WKT_BAD_TASK_NAME") {
		t.Fatalf("want WKT_BAD_TASK_NAME, got %s", errb.String())
	}
	trees := filepath.Join(ws+".worktrees", "trees")
	ents, err := os.ReadDir(trees)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("the refusal must leave trees/ empty, found %d entries", len(ents))
	}
	// The name whose slot the debris used to occupy must still be usable.
	out.Reset()
	if code := Run([]string{"new", "feature", "--all", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("the plain name must remain available, exited %d: %s", code, errb.String())
	}
}

// TestTreeExistsRemedyIsActionable covers the second half of F1: the old
// remedy suggested "wkt rm <task>", which answers WKT_NO_TASK when only the
// directory exists — a dead end that left the user with no documented way out.
func TestTreeExistsRemedyIsActionable(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	orphan := filepath.Join(ws+".worktrees", "trees", "orphan")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := Run([]string{"new", "orphan", "--all", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("new must refuse when the tree directory is already there")
	}
	msg := errb.String()
	if !strings.Contains(msg, "WKT_TREE_EXISTS") {
		t.Fatalf("want WKT_TREE_EXISTS, got %s", msg)
	}
	if strings.Contains(msg, "wkt rm orphan") {
		t.Fatalf("the remedy must not recommend a command that answers WKT_NO_TASK: %s", msg)
	}
	if !strings.Contains(msg, orphan) {
		t.Fatalf("the remedy must name the directory to deal with: %s", msg)
	}
}

// TestNewWarnsOnStderrWhenASelectedRepositoryHasASubmodule covers F3 at the
// CLI seam: the warning goes to stderr and the command still succeeds.
func TestNewWarnsOnStderrWhenASelectedRepositoryHasASubmodule(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(base, "lib"))
	seedRepo(t, filepath.Join(ws, "super"))
	sub := filepath.Join(ws, "super")
	for _, args := range [][]string{
		{"-c", "protocol.file.allow=always", "submodule", "add", "-q", filepath.Join(base, "lib"), "vendor"},
		{"commit", "-qm", "add submodule"},
	} {
		cmd := exec.Command("git", append([]string{"-c", "user.email=e@x", "-c", "user.name=t"}, args...)...)
		cmd.Dir = sub
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, o)
		}
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "t1", "--all", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new must still succeed, exited %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "WKT_SUBMODULE") || !strings.Contains(errb.String(), "super") {
		t.Fatalf("new must warn on stderr that super carries a submodule, got %q", errb.String())
	}
}

// TestInitExcludeAdoptsAWorkspaceWithANestedRepository covers adversarial
// finding F4. Spec §5.3 rule 6 and the §7.1 command table both promise
// --exclude as the escape hatch for a genuine nested repository; without it,
// init refuses and a workspace containing one cannot be adopted at all.
func TestInitExcludeAdoptsAWorkspaceWithANestedRepository(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "a", "inner"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("a genuine nested repository must be refused without --exclude")
	}
	if !strings.Contains(errb.String(), "WKT_NESTED_REPO") {
		t.Fatalf("want WKT_NESTED_REPO, got %s", errb.String())
	}
	// The refusal has to name the way out, or the user is stuck.
	if !strings.Contains(errb.String(), "--exclude") {
		t.Fatalf("the refusal must point at --exclude: %s", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"init", "--exclude", "a/inner", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("--exclude must adopt the workspace, exited %d: %s", code, errb.String())
	}

	// Recorded in container state: a later run must not need the flag again.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("the exclusion must be remembered, exited %d: %s", code, errb.String())
	}

	// And the workspace is usable: a task over the outer repository works.
	out.Reset()
	if code := Run([]string{"new", "t1", "--all", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
}

// TestInitExcludeRefusesAPathThatIsNotANestedRepository keeps the flag from
// becoming a silent no-op for a typo, the same failure shape as defect 24.
func TestInitExcludeRefusesAPathThatIsNotANestedRepository(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "a", "inner"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--exclude", "a/typo", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("excluding something that is not a nested repository must fail")
	}
	if !strings.Contains(errb.String(), "WKT_NO_SUCH_NESTED_REPO") {
		t.Fatalf("want WKT_NO_SUCH_NESTED_REPO, got %s", errb.String())
	}
	// Excluding the *outer* repository is not what the flag is for either:
	// it is not nested, and dropping it would leave its directory to be
	// linked whole, hiding a repository inside a shared writable link.
	errb.Reset()
	if code := Run([]string{"init", "--exclude", "a", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("excluding a non-nested repository must fail")
	}
}

// TestUsageDocumentsExclude keeps the escape hatch discoverable: a flag the
// refusal recommends but the usage never mentions is a flag nobody finds.
func TestUsageDocumentsExclude(t *testing.T) {
	if !strings.Contains(usage, "--exclude") {
		t.Fatalf("usage must document --exclude:\n%s", usage)
	}
}

// TestStatusColumnsAlignWhateverThePathLength covers live-run finding L3: the
// branch column was padded to a fixed 28 characters, so a real path like
// "DVS/Research/excalidraw-diagram-skill" pushed it out of line.
func TestStatusColumnsAlignWhateverThePathLength(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "group", "research", "excalidraw-diagram-skill"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	if code := Run([]string{"new", "t1", "--all", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	out.Reset()
	if code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("status exited %d: %s", code, errb.String())
	}
	col := -1
	for _, line := range strings.Split(out.String(), "\n") {
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "  !") || strings.HasPrefix(line, "  i") {
			continue
		}
		at := strings.LastIndex(line, "t1")
		if at < 0 {
			continue
		}
		if col == -1 {
			col = at
		} else if at != col {
			t.Fatalf("branch column must line up for every repository:\n%s", out.String())
		}
	}
	if col == -1 {
		t.Fatalf("status printed no repository lines:\n%s", out.String())
	}
}

// TestPerimeterRegeneratesEveryTask — with no task name the verb refreshes
// them all, which is how a workspace recovers after tasks were created by a
// version that did not write perimeters, or after someone deleted one.
func TestPerimeterRegeneratesEveryTask(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t2", "--all", "--workspace", ws)

	// Delete one copy and corrupt another.
	c := containerOf(t, ws)
	gone := filepath.Join(c, "trees", "t1", ".claude", "settings.json")
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	edited := filepath.Join(c, "trees", "t2", ".claude", "settings.json")
	if err := os.WriteFile(edited, []byte(`{"permissions":{"deny":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"perimeter", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("perimeter exited %d: %s", code, errb.String())
	}
	for _, f := range []string{gone, edited} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s was not regenerated: %v", f, err)
		}
		if !strings.Contains(string(b), "sandbox") {
			t.Fatalf("%s does not look like a regenerated perimeter:\n%s", f, b)
		}
	}

	// Regenerating has to record the new hashes, or the very next check
	// reports drift against the file it just wrote — and a report that is
	// wrong immediately after the repair teaches people to ignore it.
	out.Reset()
	errb.Reset()
	if code := Run([]string{"perimeter", "--check", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("a check straight after a regeneration must be clean, exited %d: %s", code, out.String())
	}
}

// TestPerimeterCheckReportsDriftAndWritesNothing — --check is a report, and a
// report that repairs what it is reporting on cannot be trusted twice.
func TestPerimeterCheckReportsDriftAndWritesNothing(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	out.Reset()
	errb.Reset()
	if code := Run([]string{"perimeter", "--check", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("a clean perimeter must check clean, exited %d: %s", code, out.String()+errb.String())
	}

	f := filepath.Join(containerOf(t, ws), "trees", "t1", ".claude", "settings.json")
	tampered := []byte(`{"permissions":{"deny":[]}}`)
	if err := os.WriteFile(f, tampered, 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"perimeter", "--check", "--workspace", ws}, &out, &errb); code != 3 {
		t.Fatalf("drift must exit 3, got %d: %s", code, out.String())
	}
	back, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(tampered) {
		t.Fatal("--check must not write: it repaired the file it was asked to report on")
	}
}

// TestEachVerbAcceptsOnlyItsOwnFlags covers finding F6: every verb shared one
// flag set, so "wkt path t --force --all" was accepted in silence. Input that
// means nothing must not read as success — the same rule as defect 24.
func TestEachVerbAcceptsOnlyItsOwnFlags(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	for _, args := range [][]string{
		{"path", "t1", "--force", "--workspace", ws},
		{"path", "t1", "--all", "--workspace", ws},
		{"init", "--repos", "a", "--workspace", ws},
		{"init", "--force", "--workspace", ws},
		{"new", "t2", "--check", "--workspace", ws},
		{"status", "--force", "--workspace", ws},
		{"rm", "t1", "--all", "--workspace", ws},
		{"perimeter", "--repos", "a", "--workspace", ws},
	} {
		out.Reset()
		errb.Reset()
		if code := Run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exited %d, want 2 — a flag the verb does not take is a usage error", args, code)
		}
	}
}

// TestEachVerbStillAcceptsItsOwnFlags is the other half: the tightening must
// not remove a flag a verb is documented to take.
func TestEachVerbStillAcceptsItsOwnFlags(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "b"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--dry-run", "--workspace", ws)
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--repos", "a", "--workspace", ws)
	mustRun(t, &out, &errb, "status", "t1", "--workspace", ws)
	mustRun(t, &out, &errb, "perimeter", "t1", "--workspace", ws)
	mustRun(t, &out, &errb, "rm", "t1", "--force", "--workspace", ws)
}

func TestUsageDocumentsPerimeter(t *testing.T) {
	if !strings.Contains(usage, "wkt perimeter") {
		t.Fatalf("usage must document the perimeter verb:\n%s", usage)
	}
}

func mustRun(t *testing.T, out, errb *bytes.Buffer, args ...string) {
	t.Helper()
	out.Reset()
	errb.Reset()
	if code := Run(args, out, errb); code != 0 {
		t.Fatalf("%v exited %d: %s", args, code, out.String()+errb.String())
	}
}

func containerOf(t *testing.T, ws string) string {
	t.Helper()
	return ws + ".worktrees"
}

// TestPerimeterCheckCatchesATaskWithNoPerimeter — a task created before this
// feature existed has no recorded coverage, and that is precisely the case
// the command exists to repair. Reporting it clean would hide it.
func TestPerimeterCheckCatchesATaskWithNoPerimeter(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	// Rewrite the state as an older wkt would have left it: no coverage, no
	// hashes, and no files on disk either.
	stateFile := filepath.Join(containerOf(t, ws), "state", "tasks", "t1.json")
	b, err := os.ReadFile(stateFile)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "perimeter_coverage")
	delete(raw, "perimeter_hashes")
	nb, _ := json.Marshal(raw)
	if err := os.WriteFile(stateFile, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(containerOf(t, ws), "trees", "t1", ".claude")); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"perimeter", "--check", "--workspace", ws}, &out, &errb); code != 3 {
		t.Fatalf("a task with no perimeter must report drift, exited %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "uncovered") {
		t.Fatalf("the report must say what is wrong: %s", out.String())
	}

	// And the command must be able to repair exactly that.
	mustRun(t, &out, &errb, "perimeter", "--workspace", ws)
	out.Reset()
	if code := Run([]string{"perimeter", "--check", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("after repair the check must be clean, exited %d: %s", code, out.String())
	}
}

// TestStatusReportsPerimeterCoverage — status is where a user finds out what
// the perimeter actually covers, which is never "everything" (H6a).
func TestStatusReportsPerimeterCoverage(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	out.Reset()
	if code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("status exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "perimeter") {
		t.Fatalf("status must report perimeter coverage:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "2") {
		t.Fatalf("coverage should name how many directories are covered:\n%s", out.String())
	}
}

// TestStatusReportsPerimeterDrift — an edited copy is drift, exit 3, like any
// other drift.
func TestStatusReportsPerimeterDrift(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	f := filepath.Join(containerOf(t, ws), "trees", "t1", ".claude", "settings.json")
	if err := os.WriteFile(f, []byte(`{"$wkt":{"version":1},"permissions":{"deny":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb); code != 3 {
		t.Fatalf("a diverged perimeter copy is drift, exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "PERIMETER") {
		t.Fatalf("status must name the perimeter problem:\n%s", out.String())
	}
}

// TestStatusReportsAStalePerimeter — a perimeter that predates a sibling does
// not name that sibling's tree, and H16 means a wide glob cannot cover it. The
// user has to be told, because nothing else will.
func TestStatusReportsAStalePerimeter(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	// Freeze t1's perimeter directory so creating t2 cannot refresh it.
	dir := filepath.Join(containerOf(t, ws), "trees", "t1", ".claude")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	mustRun(t, &out, &errb, "new", "t2", "--all", "--workspace", ws)

	out.Reset()
	code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb)
	if code != 3 {
		t.Fatalf("a stale perimeter is drift, exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "STALE") {
		t.Fatalf("status must say the perimeter is out of date:\n%s", out.String())
	}
}

// TestStatusNeverClaimsIsolation pins a promise the project has already broken
// once (finding F2): v0 makes no isolation claim at all, and a promise is
// cheaper to keep with a test than with memory.
func TestStatusNeverClaimsIsolation(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)
	out.Reset()
	Run([]string{"status", "--workspace", ws}, &out, &errb)
	blob := strings.ToLower(out.String() + errb.String() + usage)
	for _, word := range []string{"isolated", "isolation", "sandboxed from", "secure"} {
		if strings.Contains(blob, word) {
			t.Fatalf("wkt claims %q; v0.1 makes no isolation claim (spec §0, §9):\n%s", word, blob)
		}
	}
}

// TestRemovingATaskRefreshesTheSurvivorsPerimeter — new refreshes siblings, so
// rm must too, or every removal leaves the others naming a tree that is gone.
func TestRemovingATaskRefreshesTheSurvivorsPerimeter(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "keeper", "--all", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "goner", "--all", "--workspace", ws)

	f := filepath.Join(containerOf(t, ws), "trees", "keeper", ".claude", "settings.json")
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "goner") {
		t.Fatal("precondition: keeper's perimeter should name goner")
	}
	mustRun(t, &out, &errb, "rm", "goner", "--workspace", ws)

	after, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "goner") {
		t.Fatal("after removal the survivor's perimeter must stop naming the tree that is gone")
	}
	out.Reset()
	if code := Run([]string{"status", "keeper", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("the survivor must not be left in drift, exited %d:\n%s", code, out.String())
	}
}

// TestVersionVerb — the release build stamps a version in, and a binary that
// cannot say which one it is makes every bug report guesswork.
func TestVersionVerb(t *testing.T) {
	for _, arg := range []string{"version", "--version", "-v"} {
		var out, errb bytes.Buffer
		if code := Run([]string{arg}, &out, &errb); code != 0 {
			t.Fatalf("%s exited %d: %s", arg, code, errb.String())
		}
		if !strings.HasPrefix(out.String(), "wkt ") {
			t.Fatalf("%s printed %q", arg, out.String())
		}
	}
	if !strings.Contains(usage, "wkt version") {
		t.Fatal("usage must document the version verb")
	}
}

// TestDoctorReportsAndFixes — the verb's contract at the CLI seam: quiet on a
// healthy container, exit 3 on a problem, and --all lists what wkt wrote on
// purpose, which is the uninstall inventory.
func TestDoctorReportsAndFixes(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	out.Reset()
	if code := Run([]string{"doctor", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("a healthy container must exit 0, got %d:\n%s", code, out.String())
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("a healthy container must be quiet, got:\n%s", out.String())
	}

	// --all is the uninstall answer: it names the ref wkt put in the repo.
	out.Reset()
	if code := Run([]string{"doctor", "--all", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("--all must not turn information into failure, got %d", code)
	}
	if !strings.Contains(out.String(), "refs/wkt/base/t1") {
		t.Fatalf("--all must list what wkt wrote into the workspace:\n%s", out.String())
	}

	// A problem: debris in trees/ that no task claims.
	orphan := filepath.Join(containerOf(t, ws), "trees", "leftover")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"doctor", "--workspace", ws}, &out, &errb); code != 3 {
		t.Fatalf("a problem must exit 3, got %d:\n%s", code, out.String())
	}

	out.Reset()
	if code := Run([]string{"doctor", "--fix", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("--fix must clear it, got %d:\n%s", code, out.String())
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("--fix must remove the empty leftover")
	}
}

// TestHookWorktreeCreateEmitsOneLineAndIsIdempotent — the whole contract is
// that single line, and "--resume --worktree" re-fires the event (H14), so
// firing twice for the same name must hand back the same tree rather than
// failing with WKT_TASK_EXISTS.
func TestHookWorktreeCreateEmitsOneLineAndIsIdempotent(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "b"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)

	run := func(payload string) (string, int) {
		out.Reset()
		errb.Reset()
		stdin = strings.NewReader(payload)
		defer func() { stdin = os.Stdin }()
		code := Run([]string{"hook", "worktree-create", "--workspace", ws}, &out, &errb)
		return out.String(), code
	}

	got, code := run(`{"session_id":"s","cwd":"` + ws + `","name":"feat-42"}`)
	if code != 0 {
		t.Fatalf("hook exited %d: %s", code, errb.String())
	}
	if n := strings.Count(strings.TrimRight(got, "\n"), "\n"); n != 0 {
		t.Fatalf("stdout must be exactly one line, got %d extra:\n%q", n, got)
	}
	tree := strings.TrimSpace(got)
	if !filepath.IsAbs(tree) {
		t.Fatalf("the hook must emit an absolute path, got %q", tree)
	}
	if strings.Contains(tree, "/../") || strings.Contains(tree, "/./") {
		t.Fatalf("the binary rejects a path with dot segments: %q", tree)
	}
	if st, err := os.Stat(tree); err != nil || !st.IsDir() {
		t.Fatalf("the emitted path must be a directory: %v", err)
	}

	again, code := run(`{"session_id":"s","cwd":"` + ws + `","name":"feat-42"}`)
	if code != 0 {
		t.Fatalf("re-firing must succeed, exited %d: %s", code, errb.String())
	}
	if strings.TrimSpace(again) != tree {
		t.Fatalf("re-firing must return the same tree:\n%q\n%q", again, tree)
	}
}

// TestHookWorktreeCreateSanitisesTheSuggestedName — the slug is a suggestion,
// not a validated task name, and the contract has nowhere to report a rename.
func TestHookWorktreeCreateSanitisesTheSuggestedName(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)

	out.Reset()
	errb.Reset()
	stdin = strings.NewReader(`{"name":"feature/x","cwd":"` + ws + `"}`)
	defer func() { stdin = os.Stdin }()
	if code := Run([]string{"hook", "worktree-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("a slug with a separator must still produce a worktree, exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if filepath.Base(tree) != "feature-x" {
		t.Fatalf("want the sanitised name, got %q", tree)
	}
}

// TestHookWorktreeRemoveAcceptsAnySpellingOfThePath — the payload's path may
// arrive as typed or resolved, and on macOS a temp path resolves through
// /private, so the two differ. Comparing by string equality would miss one of
// them, which is why the lookup goes through paths.Spellings.
func TestHookWorktreeRemoveAcceptsAnySpellingOfThePath(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)

	// The un-resolved spelling is the interesting one: state records the
	// canonical path, so this is the case a naive comparison gets wrong.
	raw := filepath.Join(containerOf(t, ws), "trees", "t-raw")
	resolved := raw
	if r, err := filepath.EvalSymlinks(filepath.Dir(filepath.Dir(raw))); err == nil {
		resolved = filepath.Join(r, "trees", "t-raw")
	}
	if raw == resolved {
		t.Skip("this platform has no second spelling for the temp path")
	}

	for name, spelling := range map[string]string{"t-raw": raw, "t-res": resolved} {
		mustRun(t, &out, &errb, "new", name, "--all", "--workspace", ws)
		p := strings.Replace(spelling, "t-raw", name, 1)
		out.Reset()
		errb.Reset()
		stdin = strings.NewReader(`{"worktree_path":"` + p + `"}`)
		if code := Run([]string{"hook", "worktree-remove", "--workspace", ws}, &out, &errb); code != 0 {
			stdin = os.Stdin
			t.Fatalf("removing by the %s spelling exited %d: %s", name, code, errb.String())
		}
		stdin = os.Stdin
		if _, err := os.Stat(filepath.Join(containerOf(t, ws), "trees", name)); !os.IsNotExist(err) {
			t.Fatalf("%s should be gone", name)
		}
	}
}

// TestHookWorktreeRemoveKeepsTheRefusals — the hook is a different entry
// point, not a way around teardown. Its stderr is what the user sees.
func TestHookWorktreeRemoveKeepsTheRefusals(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	tree := filepath.Join(containerOf(t, ws), "trees", "t1")
	if err := os.WriteFile(filepath.Join(tree, "a", "uncommitted.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	stdin = strings.NewReader(`{"worktree_path":"` + tree + `"}`)
	defer func() { stdin = os.Stdin }()
	if code := Run([]string{"hook", "worktree-remove", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("uncommitted work must still block a hook-driven removal")
	}
	if !strings.Contains(errb.String(), "WKT_WOULD_LOSE_WORK") {
		t.Fatalf("the refusal must reach stderr, where the user sees it: %q", errb.String())
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("the refused removal must not have deleted anything")
	}
}

// TestHookInstallPrintsSomethingUsable — the settings block is what a user
// pastes; it has to name the real binary and both events.
func TestHookInstallPrintsSomethingUsable(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"hook", "install"}, &out, &errb); code != 0 {
		t.Fatalf("exited %d: %s", code, errb.String())
	}
	blob := out.String()
	for _, want := range []string{"WorktreeCreate", "WorktreeRemove", "hook worktree-create", "hook worktree-remove"} {
		if !strings.Contains(blob, want) {
			t.Fatalf("the printed settings must mention %q:\n%s", want, blob)
		}
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(blob[strings.Index(blob, "{"):strings.LastIndex(blob, "}")+1]), &probe); err != nil {
		t.Fatalf("the printed block must be valid JSON: %v\n%s", err, blob)
	}
}

// TestInitWarnsAboutARepositoryBelowTheDiscoveryBound covers live-run finding
// L4. A repository deeper than the discovery depth makes its containing
// directory unlinkable (spec §5.3 rule 4 — it must not be shared writable by
// every task), so init succeeds and new then refuses. The user learns one
// command too late, and init is the command that already walks the workspace.
func TestInitWarnsAboutARepositoryBelowTheDiscoveryBound(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	// Depth 5: past the default bound of 4.
	deep := filepath.Join(ws, "notes", "one", "two", "three", "hidden-repo")
	seedRepo(t, deep)

	var out, errb bytes.Buffer
	code := Run([]string{"init", "--workspace", ws}, &out, &errb)
	if code != 0 {
		t.Fatalf("init must still adopt the workspace, exited %d: %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "WKT_REPO_BELOW_BOUND") {
		t.Fatalf("init must warn about the repository new will refuse over:\n%s", errb.String())
	}
	if !strings.Contains(errb.String(), "hidden-repo") {
		t.Fatalf("the warning must name it:\n%s", errb.String())
	}

	// And the warning is honest about what happens next.
	out.Reset()
	if code := Run([]string{"new", "t1", "--all", "--workspace", ws}, &out, &errb); code == 0 {
		t.Fatal("precondition: new should refuse to link a directory hiding a repository")
	}
}

// TestInitIsQuietWhenThereIsNothingToWarnAbout — a warning that always fires
// is noise, and this one has to stay rare enough to read.
func TestInitIsQuietWhenThereIsNothingToWarnAbout(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	if err := os.MkdirAll(filepath.Join(ws, "notes", "deep", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "notes", "deep", "deeper", "x.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "WKT_REPO_BELOW_BOUND") {
		t.Fatalf("nothing here is below the bound:\n%s", errb.String())
	}
}

// TestAddVerb — the CLI seam for grafting a repository onto a live task.
func TestAddVerb(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	seedRepo(t, filepath.Join(ws, "b"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--repos", "a", "--workspace", ws)

	// Before: b is a back-fill link, not a worktree.
	at := filepath.Join(containerOf(t, ws), "trees", "t1", "b")
	if info, err := os.Lstat(at); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("precondition: b should start as a back-fill link, got %v %v", info, err)
	}

	mustRun(t, &out, &errb, "add", "t1", "--repos", "b", "--workspace", ws)

	if info, err := os.Lstat(at); err != nil || info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("b must become a real worktree: %v %v", info, err)
	}
	out.Reset()
	if code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("status exited %d after the add:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "b ") {
		t.Fatalf("status must list the added repository:\n%s", out.String())
	}

	// Usage: the verb needs both a task and something to add.
	for _, args := range [][]string{
		{"add", "--workspace", ws},
		{"add", "t1", "--workspace", ws},
	} {
		out.Reset()
		if code := Run(args, &out, &errb); code != 2 {
			t.Errorf("%v: exited %d, want 2", args, code)
		}
	}
}

// TestSyncAndFetchVerbs — the CLI seam for the two commands that move work
// between a task and the repositories the developer works in.
func TestSyncAndFetchVerbs(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	// Nothing has moved yet.
	out.Reset()
	if code := Run([]string{"sync", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("sync on an untouched task exited %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "up to date") {
		t.Fatalf("sync should say so:\n%s", out.String())
	}

	// The workspace moves on: sync reports drift and exits 3, without moving
	// the task's base.
	repo := filepath.Join(ws, "a")
	gitCmd(t, repo, "commit", "-qm", "moved on", "--allow-empty")
	out.Reset()
	if code := Run([]string{"sync", "t1", "--workspace", ws}, &out, &errb); code != 3 {
		t.Fatalf("drift must exit 3, got %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "behind") {
		t.Fatalf("sync must say how far behind:\n%s", out.String())
	}

	// Work in the task comes back with fetch.
	tree := filepath.Join(containerOf(t, ws), "trees", "t1", "a")
	gitCmd(t, tree, "commit", "-qm", "task work", "--allow-empty")
	out.Reset()
	if code := Run([]string{"fetch", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("fetch exited %d:\n%s", code, out.String()+errb.String())
	}
	if !strings.Contains(out.String(), "refs/heads/t1") {
		t.Fatalf("fetch must say where it put the branch:\n%s", out.String())
	}
	if code := Run([]string{"fetch", "--workspace", ws}, &out, &errb); code != 2 {
		t.Fatal("fetch needs a task name")
	}
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

// TestRepairVerb — the CLI seam. A moved workspace is the case, and the
// report has to say what it could not fix as clearly as what it did.
func TestRepairVerb(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	wt := filepath.Join(containerOf(t, ws), "trees", "t1", "a")
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /nowhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run([]string{"repair", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("repair exited %d:\n%s", code, out.String()+errb.String())
	}
	if !strings.Contains(out.String(), "fixed") {
		t.Fatalf("repair must say what it fixed:\n%s", out.String())
	}
	// And the tree works again.
	out.Reset()
	if code := Run([]string{"status", "t1", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("the task should be healthy after a repair, exited %d:\n%s", code, out.String())
	}
	if code := Run([]string{"repair", "--workspace", ws}, &out, &errb); code != 2 {
		t.Fatal("repair needs a task name")
	}
}

// TestSyncReportsAnUnreachableUpstream — "up to date" from a command that
// could not reach the upstream is worse than an error, because it is
// believed.
func TestSyncReportsAnUnreachableUpstream(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	repo := filepath.Join(ws, "a")
	seedRepo(t, repo)
	bare := filepath.Join(base, "origin.git")
	gitCmd(t, base, "init", "-q", "--bare", bare)
	gitCmd(t, repo, "remote", "add", "origin", bare)
	gitCmd(t, repo, "push", "-q", "-u", "origin", "HEAD:refs/heads/main")

	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	if err := os.RemoveAll(bare); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	code := Run([]string{"sync", "t1", "--workspace", ws}, &out, &errb)
	if strings.Contains(out.String(), "up to date") {
		t.Fatalf("sync claimed up to date without reaching the upstream:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "cannot reach origin") {
		t.Fatalf("sync must say it could not look:\n%s", out.String())
	}
	if code != 3 {
		t.Fatalf("a check that could not run is not a clean result, got exit %d", code)
	}
}

// TestInitDoesNotWarnAboutRepositoriesItFound — the warning added for finding
// L4 fired on any directory that merely *contains* a discovered repository,
// which is the product's whole shape: services/svc-a, DVS/Research/skill.
// A warning that fires on the ordinary case teaches people to ignore
// warnings, which is worse than not having it.
func TestInitDoesNotWarnAboutRepositoriesItFound(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "services", "svc-a"))
	seedRepo(t, filepath.Join(ws, "docs"))

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	if strings.Contains(errb.String(), "WKT_REPO_BELOW_BOUND") {
		t.Fatalf("services/svc-a is at depth 2 and was discovered; nothing to warn about:\n%s", errb.String())
	}
	if !strings.Contains(out.String(), "services/svc-a") {
		t.Fatalf("precondition: it should have been discovered:\n%s", out.String())
	}
}

// TestRmSaysWhereItKeptTheWork — the half of issue #2 that matters. Keeping a
// ref is only useful if the person is told it exists; otherwise recovery
// still means knowing the store's layout by heart.
func TestRmSaysWhereItKeptTheWork(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "a"))
	var out, errb bytes.Buffer
	mustRun(t, &out, &errb, "init", "--workspace", ws)
	mustRun(t, &out, &errb, "new", "t1", "--all", "--workspace", ws)

	tree := filepath.Join(containerOf(t, ws), "trees", "t1", "a")
	gitCmd(t, tree, "commit", "-qm", "never pushed", "--allow-empty")

	out.Reset()
	if code := Run([]string{"rm", "t1", "--force", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("rm --force exited %d: %s", code, errb.String())
	}
	if !strings.Contains(out.String(), "refs/wkt/removed/t1") {
		t.Fatalf("rm must say where it kept the unpushed work:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "recover with") {
		t.Fatalf("and how to get at it:\n%s", out.String())
	}
}

// seamWorkspace is a workspace with two repositories and a post-create script.
func seamWorkspace(t *testing.T, body string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "ws")
	seedRepo(t, filepath.Join(ws, "docs"))
	seedRepo(t, filepath.Join(ws, "services", "svc-a"))
	if err := os.MkdirAll(filepath.Join(ws, ".wkt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".wkt", "post-create"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	return ws
}

// wkt new prints exactly the tree path to stdout, and the worktree-create
// hook does the same because Claude Code reads that as the worktree path. A
// script that echoes must not be able to break either.
func TestNewRunsTheSeamAndKeepsStdoutClean(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\necho noise-on-stdout\ntouch \"$WKT_TREE/ran\"\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-seam", "--repos", "docs", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if strings.Contains(out.String(), "noise-on-stdout") {
		t.Fatalf("the script's output must never reach wkt's stdout; stdout was %q", out.String())
	}
	if !strings.Contains(errb.String(), "noise-on-stdout") {
		t.Fatalf("the script's output must still reach the user, on stderr; stderr was %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(tree, "ran")); err != nil {
		t.Fatal("the script did not run in the tree")
	}
}

// Deleting a correctly built tree over a failed install would destroy
// branches, a store and base pins for a transient error.
func TestNewLeavesTheTaskStandingWhenTheSeamFails(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\necho 'registry unreachable' >&2\nexit 3\n")
	var out, errb bytes.Buffer
	code := Run([]string{"new", "feat-fail", "--repos", "docs", "--workspace", ws}, &out, &errb)
	if code == 0 {
		t.Fatal("a failing seam must make wkt exit non-zero")
	}
	tree := strings.TrimSpace(out.String())
	if tree == "" {
		t.Fatal("the tree path must still be printed, so the developer can go in and fix it")
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("the task must stand: the tree, its branches and its store are all fine")
	}
	if !strings.Contains(errb.String(), "WKT_POST_CREATE_FAILED") {
		t.Fatalf("the failure must be named; stderr was %s", errb.String())
	}
	if !strings.Contains(errb.String(), "registry unreachable") {
		t.Fatalf("the script's own words must survive into the error; stderr was %s", errb.String())
	}
}

func TestNoPostCreateSkipsTheSeam(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\ntouch \"$WKT_TREE/ran\"\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-skip", "--repos", "docs", "--no-post-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(strings.TrimSpace(out.String()), "ran")); err == nil {
		t.Fatal("--no-post-create must skip the script")
	}
}

// The back-fill link is a symlink into the workspace; a script that walks the
// tree must not reach the developer's own repository through it, and the link
// must be back when the seam is done.
// services/svc-a is selected, so docs is the back-filled one — and docs sits
// at the tree root, which is where a flat "*/" glob can actually reach it.
// With the repositories the other way round the link is one level down, the
// naive loop never touches it, and the test proves nothing.
func TestNewProtectsTheWorkspaceFromAWalkingScript(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\n"+
		// The dangerous idiom, written the way people write it.
		"for d in */; do touch \"$d/installed\"; done\n"+
		// The sanctioned one.
		"echo \"$WKT_REPOS\" | while read -r r; do [ -n \"$r\" ] && touch \"$r/set-up\"; done\n"+
		"true\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-walk", "--repos", "services/svc-a", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if _, err := os.Stat(filepath.Join(ws, "docs", "installed")); err == nil {
		t.Fatal("the script reached the developer's own repository through a back-fill link")
	}
	if _, err := os.Stat(filepath.Join(tree, "services", "svc-a", "set-up")); err != nil {
		t.Fatal("the materialised repository should have been set up through WKT_REPOS")
	}
	info, err := os.Lstat(filepath.Join(tree, "docs"))
	if err != nil {
		t.Fatal("the back-fill link must be back after the seam")
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("it must come back as a symlink")
	}
}

// A repository grafted in later would otherwise stay unconfigured, which is
// the gap the seam exists to close. It runs once for the whole command, with
// the newcomers named, because running a script twice for one invocation
// would be a surprise.
func TestAddRunsTheSeamAndNamesTheNewRepository(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\nprintf '%s' \"$WKT_ADDED_REPO\" > \"$WKT_TREE/added\"\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-add", "--repos", "docs", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	// The create run wrote an empty file; the add run must overwrite it.
	if got, err := os.ReadFile(filepath.Join(tree, "added")); err != nil || strings.TrimSpace(string(got)) != "" {
		t.Fatalf("on create the variable must be empty; got %q (%v)", got, err)
	}
	out.Reset()
	errb.Reset()

	if code := Run([]string{"add", "feat-add", "--repos", "services/svc-a", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("add exited %d: %s", code, errb.String())
	}
	got, err := os.ReadFile(filepath.Join(tree, "added"))
	if err != nil {
		t.Fatal("the seam did not run on add")
	}
	if strings.TrimSpace(string(got)) != "services/svc-a" {
		t.Fatalf("WKT_ADDED_REPO must name the newcomer; got %q", got)
	}
}

func TestAddNoPostCreateSkipsTheSeam(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\ntouch \"$WKT_TREE/ran-$WKT_ADDED_REPO\"\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-add-skip", "--repos", "docs", "--no-post-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	out.Reset()
	if code := Run([]string{"add", "feat-add-skip", "--repos", "services/svc-a", "--no-post-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("add exited %d: %s", code, errb.String())
	}
	entries, err := os.ReadDir(tree)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ran-") {
			t.Fatalf("--no-post-create must skip the script on add too; found %s", e.Name())
		}
	}
}

// The hook is the case the seam most needs to serve: a tree a Claude Code
// session opens should be ready to work in. Measured on 2.1.239 that a
// WorktreeCreate hook is not cut short — one ran 399 seconds and its output
// was still read — so a dependency install has room there.
func TestHookWorktreeCreateRunsTheSeam(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\ntouch \"$WKT_TREE/ran\"\n")
	var out, errb bytes.Buffer
	stdin = strings.NewReader(`{"session_id":"s","cwd":"` + ws + `","name":"feat-hook"}`)
	defer func() { stdin = os.Stdin }()
	if code := Run([]string{"hook", "worktree-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("hook exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if n := strings.Count(strings.TrimRight(out.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("stdout must hold exactly the tree path; got %q", out.String())
	}
	if _, err := os.Stat(filepath.Join(tree, "ran")); err != nil {
		t.Fatal("the seam did not run on the hook path")
	}
}

// A non-zero hook makes Claude Code refuse a tree that is built and usable,
// so on this path a failing script is a warning and the exit stays 0. That is
// the one place the hook differs from the command line.
func TestHookWorktreeCreateSurvivesAFailingSeam(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\necho 'registry unreachable' >&2\nexit 3\n")
	var out, errb bytes.Buffer
	stdin = strings.NewReader(`{"session_id":"s","cwd":"` + ws + `","name":"feat-hook-fail"}`)
	defer func() { stdin = os.Stdin }()
	code := Run([]string{"hook", "worktree-create", "--workspace", ws}, &out, &errb)
	if code != 0 {
		t.Fatalf("a failing seam must not fail the hook: exit %d, stderr %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("the tree must exist and be handed to the session")
	}
	if n := strings.Count(strings.TrimRight(out.String(), "\n"), "\n"); n != 0 {
		t.Fatalf("stdout must still hold exactly the tree path; got %q", out.String())
	}
	if !strings.Contains(errb.String(), "WKT_POST_CREATE_FAILED") {
		t.Fatalf("the failure must be reported on stderr; got %q", errb.String())
	}
	if !strings.Contains(errb.String(), "registry unreachable") {
		t.Fatalf("the script's own words must reach the session; got %q", errb.String())
	}
}

// Without this verb the only way to finish a setup is to run the script by
// hand — which is unsafe, because a hand-run script does not get the
// back-fill links withdrawn, so the loop everyone writes installs into the
// developer's own repositories. The protection and its only repair path have
// to be the same code.
func TestPostCreateVerbRunsTheSeamOnAnExistingTask(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\n"+
		"for d in */; do touch \"$d/installed\"; done\n"+
		"echo \"$WKT_REPOS\" | while read -r r; do [ -n \"$r\" ] && touch \"$r/set-up\"; done\n"+
		"true\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-later", "--repos", "services/svc-a", "--no-post-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if _, err := os.Stat(filepath.Join(tree, "services", "svc-a", "set-up")); err == nil {
		t.Fatal("--no-post-create should have skipped the script")
	}

	out.Reset()
	errb.Reset()
	if code := Run([]string{"post-create", "feat-later", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("post-create exited %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(tree, "services", "svc-a", "set-up")); err != nil {
		t.Fatal("the verb did not run the script")
	}
	// The same protection as every other path, which is the whole point.
	if _, err := os.Stat(filepath.Join(ws, "docs", "installed")); err == nil {
		t.Fatal("the verb let the script reach the developer's own repository")
	}
	info, err := os.Lstat(filepath.Join(tree, "docs"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the back-fill link must be back after the verb")
	}
}

func TestPostCreateVerbNeedsATaskName(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\ntrue\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"post-create", "--workspace", ws}, &out, &errb); code != 2 {
		t.Fatalf("a missing task name is a usage error; got %d", code)
	}
}

// On the hook path wkt must finish before Claude Code cancels it: measured on
// 2.1.239 that a hook cancelled at 591 seconds left the session with no
// worktree at all, though wkt had built one. A script wkt stops leaves a
// usable tree and a warning instead.
func TestHookWorktreeCreateStopsALongScriptAndStillHandsOverTheTree(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\nsleep 60\ntouch \"$WKT_TREE/finished\"\n")
	old := hookSeamBudget
	hookSeamBudget = 300 * time.Millisecond
	defer func() { hookSeamBudget = old }()

	var out, errb bytes.Buffer
	stdin = strings.NewReader(`{"session_id":"s","cwd":"` + ws + `","name":"feat-slow"}`)
	defer func() { stdin = os.Stdin }()
	start := time.Now()
	code := Run([]string{"hook", "worktree-create", "--workspace", ws}, &out, &errb)
	if code != 0 {
		t.Fatalf("the session must still get its tree: exit %d, stderr %s", code, errb.String())
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("the budget was not enforced: %s", elapsed)
	}
	tree := strings.TrimSpace(out.String())
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("the tree must exist and be handed to the session")
	}
	if _, err := os.Stat(filepath.Join(tree, "finished")); err == nil {
		t.Fatal("the script should have been stopped")
	}
	if !strings.Contains(errb.String(), "WKT_POST_CREATE_TIMEOUT") {
		t.Fatalf("the session must be told the setup did not finish; got %q", errb.String())
	}
}

// Skipping a directory is only acceptable if wkt says so. Silence would make
// partial coverage exactly what the old refusal was written to avoid:
// coverage that is only sometimes true, with nobody the wiser.
func TestNewNamesTheDirectoriesItCouldNotCover(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "docs"))
	own := filepath.Join(ws, "docs", ".claude")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte(`{"permissions":{"allow":["Bash(make *)"]}}`)
	if err := os.WriteFile(filepath.Join(own, "settings.json"), mine, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "own settings"}} {
		full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = filepath.Join(ws, "docs")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "feat-own", "--repos", "docs", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("a repository with its own settings must not fail the task: %d %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())
	if _, err := os.Stat(filepath.Join(tree, "docs")); err != nil {
		t.Fatal("the tree must still be built")
	}
	if !strings.Contains(errb.String(), "WKT_PERIMETER_SKIPPED") {
		t.Fatalf("the uncovered directory must be named on stderr; got %q", errb.String())
	}
	if !strings.Contains(errb.String(), "docs") {
		t.Fatalf("and named specifically; got %q", errb.String())
	}
	back, _ := os.ReadFile(filepath.Join(tree, "docs", ".claude", "settings.json"))
	if string(back) != string(mine) {
		t.Fatal("the repository's own settings were overwritten in the tree")
	}
}

// An SSH origin cannot be reached from inside a task tree at all — measured,
// and not something wkt can fix, since the proxy is Claude Code's. Saying so
// at creation is the difference between a puzzling failure later and a known
// limit now.
func TestNewWarnsAboutAnSSHOrigin(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "docs"))
	cmd := exec.Command("git", "remote", "add", "origin", "git@github.com:example/docs.git")
	cmd.Dir = filepath.Join(ws, "docs")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %s", out)
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "feat-ssh", "--repos", "docs", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("an SSH origin must not fail the task: %d %s", code, errb.String())
	}
	if !strings.Contains(errb.String(), "WKT_SSH_ORIGIN") {
		t.Fatalf("the limit must be named; stderr was %q", errb.String())
	}
	if !strings.Contains(errb.String(), "docs") {
		t.Fatalf("and the repository named; stderr was %q", errb.String())
	}
	if !strings.Contains(errb.String(), "wkt fetch") {
		t.Fatalf("and the way work comes back; stderr was %q", errb.String())
	}
}

// sessionStartIn runs the hook with the tree as the working directory, which
// is how Claude Code invokes it.
func sessionStartIn(t *testing.T, tree, ws string) (string, string, int) {
	t.Helper()
	t.Chdir(tree)
	var out, errb bytes.Buffer
	stdin = strings.NewReader(`{"session_id":"s","cwd":"` + tree + `"}`)
	defer func() { stdin = os.Stdin }()
	code := Run([]string{"hook", "session-start", "--workspace", ws}, &out, &errb)
	return out.String(), errb.String(), code
}

// The hook exists to close H16: a sibling tree created since WorktreeCreate
// fired is not in this tree's deny list until something regenerates it. When
// the regeneration fails, the session carries on with a stale perimeter —
// missing exactly the sibling the hook was added to cover — and nothing said
// so. The guard whose purpose is closing H16 failed open in silence.
func TestSessionStartReportsAFailedPerimeterRefresh(t *testing.T) {
	ws := seamWorkspace(t, "#!/bin/sh\ntrue\n")
	var out, errb bytes.Buffer
	if code := Run([]string{"new", "feat-ss", "--repos", "docs", "--no-post-create", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())

	// Make the regeneration fail: the directory it must write into is
	// read-only, so the temporary file cannot be created.
	claudeDir := filepath.Join(tree, ".claude")
	if err := os.Chmod(claudeDir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(claudeDir, 0o755)

	_, stderr, code := sessionStartIn(t, tree, ws)
	if code != 0 {
		t.Fatalf("aborting a session over this would be a poor trade; got exit %d", code)
	}
	if !strings.Contains(stderr, "WKT_PERIMETER_STALE") {
		t.Fatalf("a failed refresh must be said out loud; stderr was %q", stderr)
	}
	if !strings.Contains(stderr, "feat-ss") {
		t.Fatalf("and must name the task; stderr was %q", stderr)
	}
}

// Since v0.6.1 a directory wkt does not own is skipped rather than failing the
// write, so the hook has a second thing it can lose quietly — and the likelier
// of the two. The session is exactly who needs to hear it: the repository it
// is about to work in has no wkt perimeter.
//
// The gap has to pre-date the task. A directory wkt covered at creation stays
// wkt's — checkOwned treats a recorded hash as ownership, and the generated
// file says so itself: "edits are overwritten on the next regeneration".
func TestSessionStartReportsASkippedDirectory(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seedRepo(t, filepath.Join(ws, "docs"))
	own := filepath.Join(ws, "docs", ".claude")
	if err := os.MkdirAll(own, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "settings.json"),
		[]byte(`{"permissions":{"allow":["Bash(make *)"]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", "own settings"}} {
		full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t"}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = filepath.Join(ws, "docs")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}

	var out, errb bytes.Buffer
	if code := Run([]string{"init", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("init exited %d: %s", code, errb.String())
	}
	out.Reset()
	errb.Reset()
	if code := Run([]string{"new", "feat-ss2", "--repos", "docs", "--workspace", ws}, &out, &errb); code != 0 {
		t.Fatalf("new exited %d: %s", code, errb.String())
	}
	tree := strings.TrimSpace(out.String())

	_, stderr, code := sessionStartIn(t, tree, ws)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr, "WKT_PERIMETER_SKIPPED") {
		t.Fatalf("a skipped directory must be said out loud here too; stderr was %q", stderr)
	}
	if !strings.Contains(stderr, "docs") {
		t.Fatalf("and named; stderr was %q", stderr)
	}
}
