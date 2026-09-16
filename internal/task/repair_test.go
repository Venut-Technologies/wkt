package task

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/perimeter"
	"github.com/Venut-Technologies/wkt/internal/state"
)

// TestRepairFixesGitdirBackPointersAfterAMove — spec §6. Moving a workspace
// breaks every worktree in every task: the gitdir file in the tree points at
// the store by absolute path, and the store's registration points back the
// same way. git worktree repair fixes both, but only when told which path to
// repair — bare "worktree repair" does not rediscover a moved tree.
func TestRepairFixesGitdirBackPointersAfterAMove(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-repair", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := tk.Repos[0].WorktreePath
	if _, err := gitRun(wt, "status", "--porcelain"); err != nil {
		t.Fatalf("precondition: the worktree should work: %v", err)
	}

	// Break the back-pointer the way a move does.
	gitdir := filepath.Join(wt, ".git")
	if err := os.WriteFile(gitdir, []byte("gitdir: /nowhere/that/exists\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitRun(wt, "status", "--porcelain"); err == nil {
		t.Fatal("precondition: a broken gitdir should break the worktree")
	}

	report, err := Repair(c, "feat-repair")
	if err != nil {
		t.Fatal(err)
	}
	var fixed bool
	for _, r := range report {
		if r.Repo == "docs" && r.Repaired {
			fixed = true
		}
	}
	if !fixed {
		t.Fatalf("the worktree should have been repaired: %+v", report)
	}
	if _, err := gitRun(wt, "status", "--porcelain"); err != nil {
		t.Fatalf("the worktree must work again: %v", err)
	}
}

// TestRepairRestoresAMissingBackFillLink — link slots are part of the tree's
// shape: without them a relative path from one repository to another stops
// resolving, which is the entire reason the tree is mirrored.
func TestRepairRestoresAMissingBackFillLink(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-links", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.TreePath("feat-links"), "services", "svc-a")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("precondition: svc-a should be a back-fill link: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}

	if _, err := Repair(c, "feat-links"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("the link must be restored: %v", err)
	}
	if got != target {
		t.Fatalf("restored link points at %q, want %q", got, target)
	}
}

// TestRepairLeavesAReplacedLinkAlone — a link slot that is now a real file or
// directory holds something wkt did not put there. Restoring the link would
// delete it, and repair is not a command that loses work.
func TestRepairLeavesAReplacedLinkAlone(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-replaced", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(c.TreePath("feat-replaced"), "services", "svc-a")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(link, "someones-work.txt")
	if err := os.WriteFile(precious, []byte("not wkt's\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Repair(c, "feat-replaced")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatal("repair deleted content it did not create")
	}
	var said bool
	for _, r := range report {
		if strings.Contains(r.Detail, "svc-a") && !r.Repaired {
			said = true
		}
	}
	if !said {
		t.Fatalf("repair must report what it could not fix: %+v", report)
	}
}

// TestRepairIsQuietOnAHealthyTask — nothing to fix, nothing to say.
func TestRepairIsQuietOnAHealthyTask(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-ok", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	report, err := Repair(c, "feat-ok")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range report {
		if r.Repaired {
			t.Fatalf("nothing was broken: %+v", r)
		}
	}
}

// gitRun is a plain git call that returns its error rather than failing the
// test, for the preconditions above.
func gitRun(dir string, args ...string) (string, error) {
	return gitx.Run(dir, args...)
}

// TestRepairAdoptsAMovedWorkspace — spec §6 names this as repair's reason to
// exist. State records absolute paths, so moving the workspace and its
// container leaves every one of them pointing at where things used to be:
// status reports paths that no longer exist, and the worktrees are detached
// from a store that is no longer where they think it is.
func TestRepairAdoptsAMovedWorkspace(t *testing.T) {
	base := t.TempDir()
	oldWS := filepath.Join(base, "old", "ws")
	seed(t, filepath.Join(oldWS, "docs"))
	seed(t, filepath.Join(oldWS, "services", "svc-a"))

	c, err := container.Locate(oldWS)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	entries, err := discover.Walk(oldWS, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries, "moved", []string{"docs"}); err != nil {
		t.Fatal(err)
	}

	// Move the workspace and its container together, as a person relocating a
	// project would.
	newRoot := filepath.Join(base, "new")
	if err := os.MkdirAll(newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	newWS := filepath.Join(newRoot, "ws")
	if err := os.Rename(oldWS, newWS); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.Root, newWS+".worktrees"); err != nil {
		t.Fatal(err)
	}

	moved, err := container.Locate(newWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Repair(moved, "moved"); err != nil {
		t.Fatalf("repair must handle a moved workspace: %v", err)
	}

	// State now describes where things actually are.
	tk, err := state.Load(moved.StateDir(), "moved")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range tk.Repos {
		if !strings.HasPrefix(r.WorktreePath, moved.Root) {
			t.Fatalf("worktree path still points at the old location: %s", r.WorktreePath)
		}
		if _, statErr := os.Stat(r.WorktreePath); statErr != nil {
			t.Fatalf("the recorded worktree does not exist: %v", statErr)
		}
		// And it is attached to its store again.
		if _, err := gitRun(r.WorktreePath, "status", "--porcelain"); err != nil {
			t.Fatalf("the worktree is still detached after a repair: %v", err)
		}
	}
	for _, l := range tk.Links {
		if strings.Contains(l.Target, filepath.Join("old", "ws")) {
			t.Fatalf("a link slot still points into the old workspace: %+v", l)
		}
		// And the link on disk, not just the record of it: a relative path
		// through a stale link resolves into a workspace that is gone.
		onDisk, readErr := os.Readlink(filepath.Join(moved.TreePath("moved"), filepath.FromSlash(l.RelPath)))
		if readErr != nil {
			t.Fatalf("link slot %s is missing: %v", l.RelPath, readErr)
		}
		if onDisk != l.Target {
			t.Fatalf("link on disk points at %q, state says %q", onDisk, l.Target)
		}
	}

	// And the task is healthy afterwards, which is the only outcome that
	// matters to whoever ran the command.
	div, err := perimeter.Verify(moved, tk)
	if err != nil {
		t.Fatal(err)
	}
	if len(div) != 0 {
		t.Fatalf("the perimeter must be regenerated for the new location: %+v", div)
	}
}
