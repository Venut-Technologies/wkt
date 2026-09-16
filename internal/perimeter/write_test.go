package perimeter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/state"
)

// treeFixture builds a container with a real task tree on disk: one
// materialised repository directory and one back-filled repository that is a
// symlink into the workspace, which is the case that loses data if Write gets
// it wrong.
func treeFixture(t *testing.T) (container.C, state.Task) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	for _, d := range []string{"services/svc-a", "services/svc-b"} {
		if err := os.MkdirAll(filepath.Join(ws, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	tree := c.TreePath("feat-42")
	if err := os.MkdirAll(filepath.Join(tree, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	// svc-b is back-filled: the tree entry is a symlink at the mirrored
	// position, pointing into the user's workspace.
	if err := os.Symlink(filepath.Join(c.Workspace, "services", "svc-b"),
		filepath.Join(tree, "services", "svc-b")); err != nil {
		t.Fatal(err)
	}
	task := state.Task{
		Name:      "feat-42",
		Container: c.Root,
		Workspace: c.Workspace,
		Repos: []state.Repo{{
			RelPath:      "services/svc-a",
			AbsPath:      filepath.Join(c.Workspace, "services", "svc-a"),
			StoreID:      "services-svc-a-deadbeef",
			WorktreePath: filepath.Join(tree, "services", "svc-a"),
		}},
		Links: []state.LinkSlot{{
			RelPath: "services/svc-b",
			Target:  filepath.Join(c.Workspace, "services", "svc-b"),
			Type:    "symlink",
		}},
	}
	return c, task
}

// TestWriteCoversTheTreeRootAndEachMaterialisedRepository — H6a: a perimeter
// file covers only the directory it sits in, so one copy at the root is not
// enough for a session started in a repository.
func TestWriteCoversTheTreeRootAndEachMaterialisedRepository(t *testing.T) {
	c, task := treeFixture(t)
	coverage, hashes, err := Write(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	tree := c.TreePath("feat-42")
	want := []string{tree, filepath.Join(tree, "services", "svc-a")}
	if len(coverage) != len(want) {
		t.Fatalf("coverage %v, want %v", coverage, want)
	}
	for _, dir := range want {
		if !contains(coverage, dir) {
			t.Errorf("coverage must list %s: %v", dir, coverage)
		}
		f := filepath.Join(dir, ".claude", "settings.json")
		b, readErr := os.ReadFile(f)
		if readErr != nil {
			t.Fatalf("missing perimeter copy at %s: %v", f, readErr)
		}
		if !json.Valid(b) {
			t.Fatalf("perimeter copy at %s is not valid JSON", f)
		}
		if _, ok := hashes[dir]; !ok {
			t.Errorf("no hash recorded for %s: %v", dir, hashes)
		}
	}
}

// TestWriteNeverFollowsABackFillLink is the one that loses data if it fails:
// the tree entry for an unselected repository is a symlink into the user's
// workspace, and writing "into the tree" there writes into their repository.
func TestWriteNeverFollowsABackFillLink(t *testing.T) {
	c, task := treeFixture(t)
	if _, _, err := Write(c, task, nil); err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(c.Workspace, "services", "svc-b", ".claude")
	if _, err := os.Lstat(leaked); !os.IsNotExist(err) {
		t.Fatalf("Write followed a back-fill link into the workspace: %s exists", leaked)
	}
	viaLink := filepath.Join(c.TreePath("feat-42"), "services", "svc-b", ".claude")
	if _, err := os.Lstat(viaLink); !os.IsNotExist(err) {
		t.Fatalf("nothing may be written through a link slot: %s exists", viaLink)
	}
}

// TestWriteSkipsSettingsItDoesNotOwnAndCoversTheRest — a repository may carry
// its own .claude/settings.json, checked into git, and plenty do. Overwriting
// it would destroy the user's configuration; refusing the whole task over it
// made wkt unusable on such a repository, and the refusal's own advice —
// "leave it and accept that this directory has no wkt perimeter" — named an
// option the tool did not offer. Partial coverage is not new: a back-filled
// repository is deliberately uncovered, and a session started deeper has none
// at all (H6a).
func TestWriteSkipsSettingsItDoesNotOwnAndCoversTheRest(t *testing.T) {
	c, task := treeFixture(t)
	tree := c.TreePath("feat-42")
	repo := filepath.Join(tree, "services", "svc-a")
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := []byte(`{"permissions":{"allow":["Bash(make *)"]}}`)
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.json"), mine, 0o644); err != nil {
		t.Fatal(err)
	}

	coverage, hashes, err := Write(c, task, nil)
	if err != nil {
		t.Fatalf("a repository with its own settings must not fail the task: %v", err)
	}
	for _, dir := range coverage {
		if dir == repo {
			t.Fatal("the directory wkt does not own must not be reported as covered")
		}
	}
	if _, ours := hashes[repo]; ours {
		t.Fatal("and no hash may be recorded for it")
	}
	// The tree root is still covered, which is the point of not refusing.
	var rootCovered bool
	for _, dir := range coverage {
		if dir == tree {
			rootCovered = true
		}
	}
	if !rootCovered {
		t.Fatalf("the rest of the tree must still be covered; coverage was %v", coverage)
	}
	back, _ := os.ReadFile(filepath.Join(repo, ".claude", "settings.json"))
	if string(back) != string(mine) {
		t.Fatal("the user's settings file was modified")
	}
	// And the caller has to be able to say so.
	skipped := Skipped(c, task, coverage)
	if len(skipped) != 1 || skipped[0] != repo {
		t.Fatalf("the skipped directory must be reportable; got %v", skipped)
	}
}

// TestWriteReplacesItsOwnCopy: a recorded hash means wkt owns the file, so
// regeneration must proceed — otherwise no perimeter could ever be refreshed.
func TestWriteReplacesItsOwnCopy(t *testing.T) {
	c, task := treeFixture(t)
	_, hashes, err := Write(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	task.PerimeterHashes = hashes
	if _, _, err := Write(c, task, []string{"another-task"}); err != nil {
		t.Fatalf("wkt must be able to rewrite the copy it owns: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(c.TreePath("feat-42"), ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "another-task") {
		t.Fatal("the regenerated perimeter must name the new sibling")
	}
}

// TestWriteLeavesNoTemporaryFiles — the write is atomic, so a reader never
// sees a partial perimeter and no debris survives.
func TestWriteLeavesNoTemporaryFiles(t *testing.T) {
	c, task := treeFixture(t)
	if _, _, err := Write(c, task, nil); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(c.TreePath("feat-42"), ".claude")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "settings.json" {
			t.Fatalf("unexpected file left in %s: %s", dir, e.Name())
		}
	}
}

// TestVerifyReportsMissingAndDiverged — what status and doctor read back.
func TestVerifyReportsMissingAndDiverged(t *testing.T) {
	c, task := treeFixture(t)
	coverage, hashes, err := Write(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	task.PerimeterCoverage, task.PerimeterHashes = coverage, hashes

	if d, vErr := Verify(c, task); vErr != nil || len(d) != 0 {
		t.Fatalf("a freshly written perimeter must verify clean: %v %v", d, vErr)
	}

	// One copy edited by hand, one deleted.
	root := filepath.Join(c.TreePath("feat-42"), ".claude", "settings.json")
	if err := os.WriteFile(root, []byte(`{"permissions":{"deny":[]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	repoCopy := filepath.Join(c.TreePath("feat-42"), "services", "svc-a", ".claude", "settings.json")
	if err := os.Remove(repoCopy); err != nil {
		t.Fatal(err)
	}

	div, err := Verify(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var sawDiverged, sawMissing bool
	for _, d := range div {
		switch d.Reason {
		case "diverged":
			sawDiverged = d.Dir == c.TreePath("feat-42")
		case "missing":
			sawMissing = strings.HasSuffix(d.Dir, "svc-a")
		}
	}
	if !sawDiverged || !sawMissing {
		t.Fatalf("want one diverged and one missing copy, got %+v", div)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestWriteAdoptsItsOwnMarkedFileWhenStateForgot — state can be lost,
// hand-edited, or written by a wkt that predates the perimeter. The file
// carries a marker saying who wrote it, so the command that exists to repair
// that case is not blocked by its own guard.
func TestWriteAdoptsItsOwnMarkedFileWhenStateForgot(t *testing.T) {
	c, task := treeFixture(t)
	if _, _, err := Write(c, task, nil); err != nil {
		t.Fatal(err)
	}
	// The file stays on disk; state forgets all about it.
	task.PerimeterCoverage, task.PerimeterHashes = nil, nil

	if _, _, err := Write(c, task, []string{"later-task"}); err != nil {
		t.Fatalf("wkt must adopt a file carrying its own marker: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(c.TreePath("feat-42"), ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "later-task") {
		t.Fatal("the adopted file must be regenerated, not merely tolerated")
	}
}

// TestWriteStillLeavesAnUnmarkedFileAlone is the other half: adoption keys on
// the marker, not on the filename, so the user's own settings are still safe.
// Safe now means untouched and uncovered rather than refused — the file is
// what has to survive, not the whole task.
func TestWriteStillLeavesAnUnmarkedFileAlone(t *testing.T) {
	c, task := treeFixture(t)
	tree := c.TreePath("feat-42")
	dir := filepath.Join(tree, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Valid JSON, plausibly a settings file, but not wkt's.
	mine := []byte(`{"permissions":{"deny":["Edit(//tmp/**)"]}}`)
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), mine, 0o644); err != nil {
		t.Fatal(err)
	}
	coverage, _, err := Write(c, task, nil)
	if err != nil {
		t.Fatalf("the task must still be buildable: %v", err)
	}
	back, _ := os.ReadFile(filepath.Join(dir, "settings.json"))
	if string(back) != string(mine) {
		t.Fatal("the user's own settings file was overwritten")
	}
	for _, d := range coverage {
		if d == tree {
			t.Fatal("a directory wkt did not write must not be claimed as covered")
		}
	}
	if skipped := Skipped(c, task, coverage); len(skipped) == 0 {
		t.Fatal("and the omission must be reportable")
	}
}
