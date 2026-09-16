package tree

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func TestBackFilledRepoKeepsCrossRepoRelativePathResolving(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "shared", "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "shared", "src", "index.js"), []byte("export const X=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []discover.Entry{
		{RelPath: "services/svc-a", AbsPath: filepath.Join(ws, "services", "svc-a"), Kind: discover.KindRepo},
		{RelPath: "shared", AbsPath: filepath.Join(ws, "shared"), Kind: discover.KindRepo},
	}
	p, err := PlanFor(ws, entries, []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.BackFill) != 1 || p.BackFill[0] != "shared" {
		t.Fatalf("shared must be back-filled, got %+v", p)
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(treeRoot, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatal(err)
	}
	// The reference the source code actually contains: ../../shared from services/svc-a
	target := filepath.Join(treeRoot, "services", "svc-a", "..", "..", "shared", "src", "index.js")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("../../shared must resolve through the back-filled symlink: %v", err)
	}
}

func TestAncestorDirectoriesAreRealNotSymlinked(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []discover.Entry{
		{RelPath: "services/svc-a", AbsPath: filepath.Join(ws, "services", "svc-a"), Kind: discover.KindRepo},
	}
	p, err := PlanFor(ws, entries, []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(treeRoot, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(treeRoot, "services"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("a directory on the path to a selected repo must be real, never a symlink (spec §5.3 rule 2)")
	}
	// Pin the exclusion in the plan itself (review finding, Important 4):
	// this must not pass only because Materialise's old EEXIST-swallow
	// happened to leave a pre-existing directory alone.
	for _, d := range p.LinkDirs {
		if d == "services" {
			t.Fatalf("PlanFor must not put an ancestor directory into LinkDirs, got %+v", p.LinkDirs)
		}
	}
}

// TestBackFilledRepoAncestorsStayReal reproduces review finding Critical 1:
// an un-materialised (back-filled) repository still needs every directory on
// its own path to be real, because the symlink only replaces the leaf. Before
// the fix, ancestors were computed from Plan.Materialise alone, so "platform"
// (on the path to the back-filled "platform/team/svc2") fell through to
// LinkDirs as an ordinary whole-directory link, and Materialise's back-fill
// pass then raced the LinkDirs pass for the same tree path.
func TestBackFilledRepoAncestorsStayReal(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "platform", "team", "svc2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "platform", "team", "svc2", "marker.txt"), []byte("m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []discover.Entry{
		{RelPath: "platform/team/svc2", AbsPath: filepath.Join(ws, "platform", "team", "svc2"), Kind: discover.KindRepo},
	}
	// Nothing selected: svc2 is entirely back-filled.
	p, err := PlanFor(ws, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.BackFill) != 1 || p.BackFill[0] != "platform/team/svc2" {
		t.Fatalf("platform/team/svc2 must be back-filled, got %+v", p)
	}
	for _, d := range p.LinkDirs {
		if d == "platform" || d == "platform/team" {
			t.Fatalf("an ancestor of a back-filled repo must not be a whole-directory link, got LinkDirs=%+v", p.LinkDirs)
		}
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"platform", filepath.Join("platform", "team")} {
		info, err := os.Lstat(filepath.Join(treeRoot, d))
		if err != nil {
			t.Fatalf("%s must exist as a real directory: %v", d, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s must be a real directory, not a symlink", d)
		}
	}
	marker := filepath.Join(treeRoot, "platform", "team", "svc2", "marker.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("content of the back-filled repo must be reachable through its symlink: %v", err)
	}
}

// TestMaterialiseRefusesConflictingTreeContent reproduces review finding
// Critical 2: Materialise used to treat os.Symlink's EEXIST as success
// unconditionally, so stale real content already sitting at a link slot's
// path was silently left in place while the returned state claimed a fresh,
// correct symlink was there.
func TestMaterialiseRefusesConflictingTreeContent(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries := []discover.Entry{
		{RelPath: "shared", AbsPath: filepath.Join(ws, "shared"), Kind: discover.KindRepo},
	}
	p, err := PlanFor(ws, entries, nil) // shared is back-filled
	if err != nil {
		t.Fatal(err)
	}

	treeRoot := filepath.Join(base, "tree")
	// Stale real content already occupies the slot: not the expected symlink.
	if err := os.MkdirAll(filepath.Join(treeRoot, "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(treeRoot, "shared", "stale.txt")
	if err := os.WriteFile(stale, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Materialise(treeRoot, ws, p)
	if err == nil {
		t.Fatal("Materialise must refuse when a tree path already exists and is not the expected symlink")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_TREE_CONFLICT" {
		t.Fatalf("expected WKT_TREE_CONFLICT, got %v", err)
	}
	// Materialise never deletes anything: the stale content must survive
	// untouched pending manual resolution.
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stale content must be left untouched on refusal: %v", err)
	}
}

// TestAncestorSiblingContentIsMaterialised reproduces review finding
// Critical 3: PlanFor only scanned the workspace's immediate children, so
// content living alongside a materialised repo's ancestor chain — a sibling
// directory or file inside "platform" when only "platform/team/svc" is
// selected — never entered any bucket and silently vanished from the tree.
func TestAncestorSiblingContentIsMaterialised(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "platform", "team", "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, "platform", "design"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "platform", "design", "notes.md"), []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "platform", "README.md"), []byte("readme\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries := []discover.Entry{
		{RelPath: "platform/team/svc", AbsPath: filepath.Join(ws, "platform", "team", "svc"), Kind: discover.KindRepo},
	}
	p, err := PlanFor(ws, entries, []string{"platform/team/svc"})
	if err != nil {
		t.Fatal(err)
	}

	wantLinkDir := filepath.ToSlash(filepath.Join("platform", "design"))
	if !contains(p.LinkDirs, wantLinkDir) {
		t.Fatalf("platform/design must be linked at its full relative path, got LinkDirs=%+v", p.LinkDirs)
	}
	wantCopy := filepath.ToSlash(filepath.Join("platform", "README.md"))
	if !contains(p.CopyFiles, wantCopy) {
		t.Fatalf("platform/README.md must be copied at its full relative path, got CopyFiles=%+v", p.CopyFiles)
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(filepath.Join(treeRoot, "platform", "team", "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatal(err)
	}

	for _, d := range []string{"platform", filepath.Join("platform", "team")} {
		info, err := os.Lstat(filepath.Join(treeRoot, d))
		if err != nil {
			t.Fatalf("%s must exist as a real directory: %v", d, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("%s must be a real directory, not a symlink", d)
		}
	}
	info, err := os.Lstat(filepath.Join(treeRoot, "platform", "design"))
	if err != nil {
		t.Fatalf("platform/design must exist: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("platform/design must be a symlink")
	}
	if _, err := os.Stat(filepath.Join(treeRoot, "platform", "design", "notes.md")); err != nil {
		t.Fatalf("platform/design/notes.md must be reachable: %v", err)
	}
	if _, err := os.Stat(filepath.Join(treeRoot, "platform", "README.md")); err != nil {
		t.Fatalf("platform/README.md must be reachable: %v", err)
	}
}

// TestCopiedLooseFileKeepsExecuteBit covers review finding Minor 5:
// copyFile used to hardcode 0o644, stripping the execute bit off any
// executable loose file (a helper script, a hook).
func TestCopiedLooseFileKeepsExecuteBit(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hook.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := PlanFor(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(treeRoot, "hook.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("copied loose file must keep its execute bit, got mode %v", info.Mode().Perm())
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

// TestLinkDirRefusesWhenItHidesANestedRepoBelowTheDiscoveryBound reproduces
// review finding Important 7: PlanFor's repository *enumeration* stops at
// a configurable depth (default 4) and Materialise used to link any
// non-git directory whole regardless — so a repository sitting deeper than
// that bound was invisible to discovery yet still made its containing
// directory a single real directory shared, writable, by every task's tree
// and by the workspace itself. Spec §5.3 rule 4 requires every symlink
// target to be separately resolved and walked, unbounded depth, before the
// link is created.
func TestLinkDirRefusesWhenItHidesANestedRepoBelowTheDiscoveryBound(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	deep := filepath.Join(ws, "notes", "a", "b", "c", "d", "hidden")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	initCmd := exec.Command("git", "-c", "init.defaultBranch=main", "init", "-q", deep)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %s", out)
	}

	// Nothing discovered at all: "notes" plans as an ordinary whole-directory
	// link, exactly as it would for a workspace where "hidden" sits beyond
	// the discovery depth and so was never classified as a repository.
	p, err := PlanFor(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(p.LinkDirs, "notes") {
		t.Fatalf("notes must be planned as a whole-directory link, got %+v", p)
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err = Materialise(treeRoot, ws, p)
	if err == nil {
		t.Fatal("Materialise must refuse to link a directory that hides a nested repository below the discovery bound")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_NESTED_REPO" {
		t.Fatalf("expected WKT_NESTED_REPO, got %v", err)
	}
	if _, statErr := os.Lstat(filepath.Join(treeRoot, "notes")); !os.IsNotExist(statErr) {
		t.Fatal("the refused link must never have been created — a shared writable slot must never exist")
	}
}

// TestSymlinkedWorkspaceEntryRoutesToALinkSlotNotACopy reproduces review
// finding Important 8: DirEntry.IsDir() is Lstat-based, so an ordinary
// symlink at the workspace root — "current", "bin", "data": normal
// workspace furniture — always reported false, bucketing it into
// CopyFiles. copyFile then os.Stat's it (following the link), opens a
// directory, and the content copy fails, breaking "wkt new" on the
// symlink without ever naming it in the error.
func TestSymlinkedWorkspaceEntryRoutesToALinkSlotNotACopy(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "releases", "v3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "releases", "v3", "marker.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "releases", "v3"), filepath.Join(ws, "current")); err != nil {
		t.Fatal(err)
	}

	p, err := PlanFor(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(p.LinkDirs, "current") {
		t.Fatalf("a symlinked workspace entry must be planned as a link, got %+v", p)
	}
	for _, f := range p.CopyFiles {
		if f == "current" {
			t.Fatal("a symlink must never be routed to CopyFiles")
		}
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialise(treeRoot, ws, p); err != nil {
		t.Fatalf("materialising a symlinked workspace entry must not fail: %v", err)
	}
	info, err := os.Lstat(filepath.Join(treeRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the tree's own slot for a symlinked entry must itself be a symlink, not a copy")
	}
	if _, err := os.Stat(filepath.Join(treeRoot, "current", "marker.txt")); err != nil {
		t.Fatalf("the symlink chain must resolve to the real content: %v", err)
	}
}

// TestOSArtifactsAreNotCopiedIntoTheTree covers live-run finding L2. On a
// real macOS workspace every directory Finder has opened holds a .DS_Store,
// and copying it into the tree creates a copy slot whose hash diverges the
// moment Finder opens the *tree* — which blocked removal on a file nobody
// created on purpose.
func TestOSArtifactsAreNotCopiedIntoTheTree(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		".DS_Store":      "finder junk",
		"Thumbs.db":      "explorer junk",
		"CONVENTIONS.md": "real content",
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := PlanFor(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(p.CopyFiles, "CONVENTIONS.md") {
		t.Fatalf("a real loose file must still be copied: %+v", p.CopyFiles)
	}
	for _, junk := range []string{".DS_Store", "Thumbs.db"} {
		if contains(p.CopyFiles, junk) {
			t.Fatalf("%s must not become a copy slot: %+v", junk, p.CopyFiles)
		}
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	slots, err := Materialise(treeRoot, ws, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range slots {
		if s.RelPath == ".DS_Store" {
			t.Fatal("no slot may be recorded for an OS artifact")
		}
	}
	if _, err := os.Lstat(filepath.Join(treeRoot, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatal("the tree must not carry a copied .DS_Store")
	}
}

// TestLargeLooseFilesAreLinkedNotCopied covers live-run finding L1. Copying
// loose files is right for the things a task edits — a CONVENTIONS.md, a
// script — but the first real workspace this ran against had 19 slide PNGs in
// an ancestor directory, and every task copied all of them. A directory of
// datasets or video would be copied whole, per task.
func TestLargeLooseFilesAreLinkedNotCopied(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	small := []byte("# conventions\n")
	if err := os.WriteFile(filepath.Join(ws, "CONVENTIONS.md"), small, 0o644); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, MaxCopyBytes+1)
	if err := os.WriteFile(filepath.Join(ws, "slides.pdf"), big, 0o644); err != nil {
		t.Fatal(err)
	}

	p, err := PlanFor(ws, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(p.CopyFiles, "CONVENTIONS.md") {
		t.Fatalf("a small loose file must still be copied: %+v", p)
	}
	if contains(p.CopyFiles, "slides.pdf") {
		t.Fatalf("a large loose file must not be copied into every task: %+v", p)
	}
	if !contains(p.LinkDirs, "slides.pdf") {
		t.Fatalf("it must still appear in the tree, as a link: %+v", p)
	}

	treeRoot := filepath.Join(base, "tree")
	if err := os.MkdirAll(treeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	slots, err := Materialise(treeRoot, ws, p)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(treeRoot, "slides.pdf"))
	if err != nil {
		t.Fatalf("the large file must be reachable from the tree: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the large file should be a link, not a copy")
	}
	var slot state.LinkSlot
	for _, s := range slots {
		if s.RelPath == "slides.pdf" {
			slot = s
		}
	}
	if slot.Type != "symlink" {
		t.Fatalf("state must record it as a link so teardown checks the right thing: %+v", slot)
	}
	// And the tree must be cheap: the copy is what cost megabytes per task.
	if st, err := os.Stat(treeRoot); err == nil {
		_ = st
	}
}
