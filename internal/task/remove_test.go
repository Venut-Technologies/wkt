package task

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func TestRemoveRefusesOnIgnoredButPreciousFile(t *testing.T) {
	c, entries := fixture(t)
	// .env is gitignored, so git's own refusal never fires on it (spec H1).
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore env")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-x", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	envPath := filepath.Join(task.Repos[0].WorktreePath, ".env")
	if err := os.WriteFile(envPath, []byte("TOKEN=secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = Remove(c, "feat-x", false)
	if err == nil {
		t.Fatal("removal must refuse while an ignored-but-precious file exists")
	}
	if _, statErr := os.Stat(envPath); statErr != nil {
		t.Fatal("the refused removal must not have deleted anything")
	}
}

func TestPreflightSeesUnpushedCommits(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-y", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, "new.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, wt, "add", "-A")
	g(t, wt, "commit", "-qm", "agent work")

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var sawUnpushed bool
	for _, b := range blockers {
		if b.Code == "WKT_UNPUSHED" {
			sawUnpushed = true
		}
	}
	if !sawUnpushed {
		t.Fatalf("an unpushed commit must block removal, got %+v", blockers)
	}
}

var _ = state.Task{}

// TestRemoveDetectsForeignRepoNestedInsideMaterialisedWorktree is a
// regression guard for a real bug found by mutating the foreign-repo walk:
// fs.WalkDir treats "return SkipDir" differently depending on whether the
// visited entry is a directory or not. A linked worktree's own ".git" is
// always a regular *file*; returning SkipDir unconditionally on it (rather
// than only when it is a directory) skips the rest of that directory's
// siblings, not just the ".git" contents — so a foreign repository nested
// anywhere sorting after ".git" (almost everything) was silently never
// visited. Confirmed empirically against Go's real fs.WalkDir before fixing
// internal/task/remove.go. This is the highest-stakes blocker in the
// package — it must fire even with --force — so it gets its own test rather
// than relying on the other tests to exercise it incidentally.
func TestRemoveDetectsForeignRepoNestedInsideMaterialisedWorktree(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-z", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	// Sorts after ".git" alphabetically, which is what the bug depended on.
	foreign := filepath.Join(wt, "zzz-vendor")
	seed(t, foreign)

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_FOREIGN_REPO" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a foreign repository nested inside a materialised worktree must be detected, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-z", true); err == nil {
		t.Fatal("a foreign repository must block removal even with --force: its history exists nowhere else")
	}
	if _, statErr := os.Stat(filepath.Join(foreign, ".git")); statErr != nil {
		t.Fatal("the refused removal must not have deleted the foreign repository")
	}
}

// TestRemoveCleansUpStoreBranchSoTaskNameCanBeReused is a regression guard
// for a real bug found by mutating the store cleanup: "git worktree prune"
// removes the worktree's admin entry but never the branch the worktree was
// created on. Left behind, the branch makes a later Create of a task with
// the same name fail Validate's WKT_BRANCH_EXISTS check against the store —
// even though the previous task was fully and cleanly removed. Confirmed
// empirically against real git ("branch --list" still showed the branch
// after unlock+prune) before adding the "branch -D" cleanup step.
func TestRemoveCleansUpStoreBranchSoTaskNameCanBeReused(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-reuse", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-reuse", false); err != nil {
		t.Fatalf("a clean tree must remove without --force: %v", err)
	}

	entries2, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries2, "feat-reuse", []string{"docs"}); err != nil {
		t.Fatalf("a task name freed by a clean removal must be reusable, got: %v", err)
	}
}

// TestRemoveRefusesOnUncommittedWorkWithoutForceButForceRemoves exercises
// the whole lifecycle end to end: refusal leaves the tree untouched, --force
// actually deletes it through the staging fence, staging is left clean
// afterward, and the task's state file is gone.
func TestRemoveRefusesOnUncommittedWorkWithoutForceButForceRemoves(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-dirty", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(c, "feat-dirty", false); err == nil {
		t.Fatal("uncommitted work must block removal without --force")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatal("the refused removal must not have deleted anything")
	}

	if _, err := Remove(c, "feat-dirty", true); err != nil {
		t.Fatalf("--force must remove a tree whose only problem is uncommitted work: %v", err)
	}
	if _, statErr := os.Stat(c.TreePath("feat-dirty")); !os.IsNotExist(statErr) {
		t.Fatal("a forced removal must actually delete the tree")
	}
	if _, statErr := os.Stat(filepath.Join(c.StagingDir(), "feat-dirty")); !os.IsNotExist(statErr) {
		t.Fatal("the staging fence must not leave a leftover directory behind")
	}
	if _, err := state.Load(c.StateDir(), "feat-dirty"); err == nil {
		t.Fatal("a successful removal must delete the task's state file")
	}
}

// TestPreflightDetectsInProgressBisect exercises spec H2 directly: a mid-
// bisect worktree has an empty "git status --porcelain" (confirmed
// empirically), so a preflight built on status alone would let it through.
// Bisect is used rather than an interactive rebase pause because it needs no
// editor/sequence-editor trickery to stay portable across macOS and Linux.
func TestPreflightDetectsInProgressBisect(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-bisect", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	good := task.Repos[0].BaseSHA

	g(t, wt, "commit", "-qm", "c2", "--allow-empty")
	g(t, wt, "commit", "-qm", "c3", "--allow-empty")
	bad := strings.TrimSpace(g(t, wt, "rev-parse", "HEAD"))

	g(t, wt, "bisect", "start")
	g(t, wt, "bisect", "bad", bad)
	g(t, wt, "bisect", "good", good)

	if s := statusWithoutPerimeter(t, wt); s != "" {
		t.Fatalf("test setup invariant broken: expected a clean status mid-bisect, got %q", s)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_IN_PROGRESS" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a mid-bisect worktree must block removal even though git status is clean, got %+v", blockers)
	}
}

// TestRemoveRefusesOnSubmoduleEvenWithForce exercises spec §5.7's hardest
// block. Crucially the submodule addition is committed before the check
// runs: an uncommitted "git submodule add" already shows up in plain "git
// status --porcelain" (confirmed empirically) and would be caught by
// WKT_DIRTY regardless, which would prove nothing about the dedicated
// WKT_SUBMODULE check. Committed, status is clean and only the submodule
// check can catch it.
func TestRemoveRefusesOnSubmoduleEvenWithForce(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-sub", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	subSrc := filepath.Join(c.Workspace, "docs")

	cmd := exec.Command("git",
		"-c", "protocol.file.allow=always",
		"-c", "user.email=e@x", "-c", "user.name=t",
		"submodule", "add", "-q", subSrc, "subdir")
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git submodule add: %s", out)
	}
	g(t, wt, "commit", "-qm", "add submodule")

	if s := statusWithoutPerimeter(t, wt); s != "" {
		t.Fatalf("test setup invariant broken: expected a clean status after committing the submodule, got %q", s)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_SUBMODULE" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a committed submodule must still be detected as a blocker, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-sub", false); err == nil {
		t.Fatal("a submodule must block removal without --force")
	}
	if _, err := Remove(c, "feat-sub", true); err == nil {
		t.Fatal("a submodule must block removal even with --force: --force would destroy its object store")
	}
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatal("the refused removal must not have deleted anything")
	}
}

// TestRemoveRefusesOnDivergedCopiedFile exercises the one blocker with no
// git mechanism behind it at all: a loose file living outside every
// repository is copied into the tree by tree.Materialise, and only the
// recorded content hash can tell an agent's edit to the copy apart from an
// untouched one before deletion would silently lose it.
func TestRemoveRefusesOnDivergedCopiedFile(t *testing.T) {
	c, entries := fixture(t)
	readme := filepath.Join(c.Workspace, "README.md")
	if err := os.WriteFile(readme, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Create(c, entries, "feat-copy", []string{"services/svc-a"}); err != nil {
		t.Fatal(err)
	}

	copied := filepath.Join(c.TreePath("feat-copy"), "README.md")
	if _, statErr := os.Stat(copied); statErr != nil {
		t.Fatalf("test setup invariant broken: README.md must have been copied into the tree, got %v", statErr)
	}
	if err := os.WriteFile(copied, []byte("agent edited this\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(c, "feat-copy", false); err == nil {
		t.Fatal("a diverged copied file must block removal")
	}
	if _, statErr := os.Stat(copied); statErr != nil {
		t.Fatal("the refused removal must not have deleted the diverged copy")
	}
}

// --- Round 2: the precious-file classifier was inverted from a denylist of
// five substrings (proven to miss ordinary secrets like "server.key" with
// zero blockers) to an allowlist of provably regenerable path components.
// Unknown ignored content now blocks by default.

func TestRemoveRefusesOnIgnoredKeyFileNotOnAnyDenylist(t *testing.T) {
	c, entries := fixture(t)
	// "server.key" is an entirely ordinary TLS/SSH private key name. It
	// matched none of the old classifier's five substrings
	// (".env", ".env.", "credentials", "id_rsa", ".pem") and was deleted
	// with zero blockers and no --force — the exact condition this check
	// exists for.
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("server.key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore server.key")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-key", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(task.Repos[0].WorktreePath, "server.key")
	if err := os.WriteFile(keyPath, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Remove(c, "feat-key", false); err == nil {
		t.Fatal("an ignored private key must block removal even though its name isn't on any hardcoded denylist")
	}
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Fatal("the refused removal must not have deleted the key")
	}
}

func TestRemoveRefusesOnBulkIgnoredDirectoryNotOnAllowlist(t *testing.T) {
	c, entries := fixture(t)
	// git status collapses a wholly-ignored directory to a single
	// "!! secrets/" line — its contents are never listed individually — so
	// the classifier must treat an unrecognised directory name as precious
	// as a whole, not inspect (and miss) files inside it one by one.
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("secrets/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore secrets")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-secrets", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	secretsDir := filepath.Join(task.Repos[0].WorktreePath, "secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretsDir, "api_token"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a bulk-ignored, unrecognised directory must block as a whole, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-secrets", false); err == nil {
		t.Fatal("a bulk-ignored unrecognised directory must block removal")
	}
}

func TestRemoveListsRegenerableIgnoredContentButDoesNotBlockOnIt(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("node_modules/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore node_modules")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-nm", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	nm := filepath.Join(task.Repos[0].WorktreePath, "node_modules", "leftpad")
	if err := os.MkdirAll(nm, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nm, "index.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var sawInfo bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" {
			t.Fatalf("node_modules must not be flagged as precious, got %+v", b)
		}
		if b.Code == "WKT_REGENERABLE_IGNORED" && b.Severity == "info" {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Fatalf("a regenerable ignored directory must still be listed (as info), got %+v", blockers)
	}

	// This is the whole point of listing it: --force must not be needed
	// when the only ignored content is provably regenerable.
	if _, err := Remove(c, "feat-nm", false); err != nil {
		t.Fatalf("a tree whose only ignored content is regenerable must remove cleanly without --force: %v", err)
	}
}

// TestRemoveRefusesOnLinkSlotReplacedByRegularFile guards WKT_LINK_SLOT_CHANGED,
// which had no test at all — proven by mutation: disabling that branch left
// every other test in the package passing. An atomic save (write a temp
// file, then rename it over the target) replaces the symlink itself with a
// regular file, unlike a naive write through the symlink, which would leave
// the link intact.
func TestRemoveRefusesOnLinkSlotReplacedByRegularFile(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-linkchange", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}

	docsLink := filepath.Join(c.TreePath("feat-linkchange"), "docs")
	if info, statErr := os.Lstat(docsLink); statErr != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("test setup invariant broken: docs must be a symlink slot, got info=%v err=%v", info, statErr)
	}

	tmp := docsLink + ".atomic-tmp"
	if err := os.WriteFile(tmp, []byte("replaced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, docsLink); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_LINK_SLOT_CHANGED" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a link slot replaced by a regular file must block removal, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-linkchange", false); err == nil {
		t.Fatal("a changed link slot must block removal without --force")
	}
}

// TestPreflightDetectsEachInProgressMarker table-drives all six markers
// individually. Previously only BISECT_LOG was exercised (indirectly, via a
// real bisect); a regression dropping e.g. MERGE_HEAD from the Go slice
// would have gone unnoticed. Each marker's real on-disk location is
// resolved via "git rev-parse --git-path" exactly as Preflight itself
// resolves it, then created directly — rebase-merge/rebase-apply are
// directories, the rest are files — so the test targets precisely the
// regression class at risk (the marker's name being dropped from the list),
// without depending on git's internal machinery to reach six different
// real operational states, several of which (conflict-driven MERGE_HEAD,
// CHERRY_PICK_HEAD, REVERT_HEAD) would also show up in plain "git status"
// and so would not by themselves prove this check is pulling its weight.
func TestPreflightDetectsEachInProgressMarker(t *testing.T) {
	markers := []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			c, entries := fixture(t)
			task, err := Create(c, entries, "feat-marker", []string{"docs"})
			if err != nil {
				t.Fatal(err)
			}
			wt := task.Repos[0].WorktreePath

			p := strings.TrimSpace(g(t, wt, "rev-parse", "--git-path", marker))
			if !filepath.IsAbs(p) {
				p = filepath.Join(wt, p)
			}
			if marker == "rebase-merge" || marker == "rebase-apply" {
				if err := os.MkdirAll(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			blockers, err := Preflight(c, task)
			if err != nil {
				t.Fatal(err)
			}
			var saw bool
			for _, b := range blockers {
				if b.Code == "WKT_IN_PROGRESS" && b.Detail == marker {
					saw = true
				}
			}
			if !saw {
				t.Fatalf("marker %q must be individually detected, got %+v", marker, blockers)
			}
		})
	}
}

// --- Round 3: .DS_Store and other OS artifacts added to the regenerable
// allowlist. Finder writes ".DS_Store" into essentially every directory a
// macOS user opens, and nearly every macOS repository gitignores it; left
// blocking, "wkt rm" would refuse on almost every real tree on the primary
// development platform, teaching people to reach for --force without
// reading the list — the exact reflex the classifier inversion exists to
// prevent.

func TestRemoveListsDSStoreButDoesNotBlockOnIt(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".DS_Store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore .DS_Store")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-dsstore", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	dsStore := filepath.Join(task.Repos[0].WorktreePath, ".DS_Store")
	if err := os.WriteFile(dsStore, []byte("finder metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var sawInfo bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" {
			t.Fatalf(".DS_Store must not be flagged as precious, got %+v", b)
		}
		if b.Code == "WKT_REGENERABLE_IGNORED" && b.Severity == "info" {
			sawInfo = true
		}
	}
	if !sawInfo {
		t.Fatalf(".DS_Store must still be listed (as info), got %+v", blockers)
	}

	// The whole point: --force must not be needed just because Finder wrote
	// a metadata file into the tree.
	if _, err := Remove(c, "feat-dsstore", false); err != nil {
		t.Fatalf("a tree whose only ignored content is .DS_Store must remove cleanly without --force: %v", err)
	}
}

func TestRemoveRefusesOnServerKeyBesideDSStore(t *testing.T) {
	c, entries := fixture(t)
	// The regenerable addition must not mask a real secret sitting right
	// next to an OS artifact in the same ignored listing.
	repo := filepath.Join(c.Workspace, "services", "svc-a")
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".DS_Store\nserver.key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "ignore .DS_Store and server.key")
	entries, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}

	task, err := Create(c, entries, "feat-dsstore-key", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, ".DS_Store"), []byte("finder metadata\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "server.key"), []byte("-----BEGIN PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var sawPrecious, sawInfo bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" {
			sawPrecious = true
		}
		if b.Code == "WKT_REGENERABLE_IGNORED" {
			sawInfo = true
		}
	}
	if !sawPrecious {
		t.Fatalf("server.key must still block even beside a regenerable .DS_Store, got %+v", blockers)
	}
	if !sawInfo {
		t.Fatalf(".DS_Store beside it should still be listed as regenerable, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-dsstore-key", false); err == nil {
		t.Fatal("a real secret beside a regenerable OS artifact must still block removal without --force")
	}
}

// --- Review finding Critical 1: Preflight scoped every content check to a
// repository's WorktreePath, so content living at the tree root itself —
// exactly where a session's working directory is, what "wkt path" prints —
// was invisible to every check and silently deleted by os.RemoveAll(staged).

func TestRemoveRefusesOnFileAtTreeRoot(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-root-file", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(c.TreePath(task.Name), "PLAN.md")
	if err := os.WriteFile(plan, []byte("cross-repo plan\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_UNTRACKED_TREE_CONTENT" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a file at the tree root must be reported as untracked tree content, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-root-file", false); err == nil {
		t.Fatal("a file at the tree root must block removal without --force")
	}
	if _, statErr := os.Stat(plan); statErr != nil {
		t.Fatal("the refused removal must not have deleted the tree-root file")
	}
}

func TestRemoveRefusesOnFileInNewSubdirectoryOfTreeRoot(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-root-subdir", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	scratchDir := filepath.Join(c.TreePath(task.Name), "scratch")
	if err := os.MkdirAll(scratchDir, 0o755); err != nil {
		t.Fatal(err)
	}
	scratchFile := filepath.Join(scratchDir, "out.json")
	if err := os.WriteFile(scratchFile, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_UNTRACKED_TREE_CONTENT" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a file in a new subdirectory of the tree root must be reported as untracked tree content, got %+v", blockers)
	}

	if _, err := Remove(c, "feat-root-subdir", false); err == nil {
		t.Fatal("a new subdirectory at the tree root must block removal without --force")
	}
	if _, statErr := os.Stat(scratchFile); statErr != nil {
		t.Fatal("the refused removal must not have deleted the new subdirectory's content")
	}

	// Once the unexpected content is cleared away, removal must succeed —
	// proving the new check does not fail closed permanently on an
	// otherwise perfectly ordinary tree.
	if err := os.RemoveAll(scratchDir); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-root-subdir", false); err != nil {
		t.Fatalf("a clean tree must remove once the untracked content is gone: %v", err)
	}
}

// --- Review finding Important 2: a task whose tree is missing could never
// be removed. Preflight blocked with WKT_WORKTREE_MISSING, so plain "rm"
// refused; "--force" then died at os.Rename(treeRoot, staged) with ENOENT,
// reported as WKT_STAGING with a remedy about filesystems that had nothing
// to do with the cause. With no "doctor" in this plan, that left the state
// file, the base pin and the store branch behind forever, and the name
// permanently unusable.

func TestRemoveSucceedsWhenTheTreeWasDeletedByHand(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-hand-deleted", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.TreePath(task.Name)); err != nil {
		t.Fatal(err)
	}

	// Plain "rm", no --force: there is nothing left on disk to fence or to
	// force through, so this must succeed on the first, unforced call.
	if _, err := Remove(c, "feat-hand-deleted", false); err != nil {
		t.Fatalf("removing a task whose tree was deleted by hand must succeed: %v", err)
	}
	if _, err := state.Load(c.StateDir(), "feat-hand-deleted"); err == nil {
		t.Fatal("the task's state file must be gone")
	}

	docsAbs := filepath.Join(c.Workspace, "docs")
	if out := g(t, docsAbs, "for-each-ref", task.Repos[0].BasePinRef); len(out) != 0 {
		t.Fatalf("the base pin must be removed from the workspace repository, found %q", out)
	}

	entries2, err := discover.Walk(c.Workspace, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Create(c, entries2, "feat-hand-deleted", []string{"docs"}); err != nil {
		t.Fatalf("a name freed by removing a hand-deleted tree must be reusable, got: %v", err)
	}
}

// TestRemoveResumesFromStagingWhenTheTreeWasAlreadyMovedButNotFullyDeleted
// reproduces the exact state test/05_staging_fence.sh deliberately
// produces: a previous "--force" run moved the tree into staging/ (the
// fence) but could not finish deleting it (there, a locked subtree; here,
// simulated directly by moving the tree by hand). The tree root is gone,
// staging/<name> is not — Remove must resume the delete from there rather
// than trying, and failing, to fence a tree that is no longer at its
// original path.
func TestRemoveResumesFromStagingWhenTheTreeWasAlreadyMovedButNotFullyDeleted(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-resume", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, "scratch.txt"), []byte("uncommitted\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(c.StagingDir(), "feat-resume")
	if err := os.MkdirAll(c.StagingDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.TreePath("feat-resume"), staged); err != nil {
		t.Fatal(err)
	}

	// No --force: the deletion this resumes was already authorised by
	// whatever produced the staged-but-undeleted state in the first place.
	if _, err := Remove(c, "feat-resume", false); err != nil {
		t.Fatalf("removal must resume from staging rather than dying on a missing tree root: %v", err)
	}
	if _, statErr := os.Stat(staged); !os.IsNotExist(statErr) {
		t.Fatal("staging must be fully cleared once removal resumes and completes")
	}
	if _, err := state.Load(c.StateDir(), "feat-resume"); err == nil {
		t.Fatal("the task's state file must be gone")
	}
}

// --- Review finding Important 3: four Preflight checks failed open. Each
// was guarded by "err == nil" with no else, so a git failure silently
// produced zero blockers from that check instead of one — unlike the very
// first check (plain "status --porcelain"), which already correctly
// emitted WKT_CHECK_FAILED in its own else branch. Spec §5.7 is explicit:
// "a failed check of any kind ... is treated as 'would lose work'."

// withFailingGit installs a "git" shim ahead of PATH for the rest of the
// test that fails any invocation whose argument list starts with prefix
// and delegates everything else, byte for byte, to the real git — used to
// make exactly one of Preflight's git calls fail while the others (in
// particular the first, already-correct "status --porcelain" check) keep
// succeeding, so each else branch can be proven in isolation.
func withFailingGit(t *testing.T, prefix string) {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  \"" + prefix + "\"*)\n" +
		"    echo 'injected failure for testing' >&2\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exec \"" + real + "\" \"$@\"\n"
	shim := filepath.Join(dir, "git")
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestPreflightFailsClosedWhenTheBaseSHACannotBeResolved is the review
// finding's own reproduction: a base_sha the store can no longer resolve
// makes "git rev-list ... <bad-sha>" error, and the old code silently
// dropped the unpushed-commit blocker instead of reporting that the check
// itself failed.
func TestPreflightFailsClosedWhenTheBaseSHACannotBeResolved(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-badbase", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	task.Repos[0].BaseSHA = strings.Repeat("f", 40)
	if err := state.Save(c.StateDir(), task); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_CHECK_FAILED" && strings.Contains(b.Detail, "unpushed") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("an unresolvable base SHA must fail the unpushed-commit check closed, got %+v", blockers)
	}

	if _, err := Remove(c, task.Name, false); err == nil {
		t.Fatal("a failed unpushed-commit check must block removal: 'cannot tell' is 'would lose work'")
	}
	if _, statErr := os.Stat(task.Repos[0].WorktreePath); statErr != nil {
		t.Fatal("the refused removal must not have deleted anything")
	}
}

func TestPreflightFailsClosedWhenTheIgnoredContentCheckFails(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-ignoredfail", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	withFailingGit(t, "status --porcelain --ignored=matching")

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_CHECK_FAILED" && strings.Contains(b.Detail, "ignored-content") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a failed ignored-content check must fail closed, got %+v", blockers)
	}

	if _, err := Remove(c, task.Name, false); err == nil {
		t.Fatal("a failed ignored-content check must block removal")
	}
}

func TestPreflightFailsClosedWhenTheInProgressCheckFails(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-inprogressfail", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	withFailingGit(t, "rev-parse --git-path")

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_CHECK_FAILED" && strings.Contains(b.Detail, "in-progress check") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a failed in-progress check must fail closed, got %+v", blockers)
	}

	if _, err := Remove(c, task.Name, false); err == nil {
		t.Fatal("a failed in-progress check must block removal")
	}
}

func TestPreflightFailsClosedWhenTheSubmoduleCheckFails(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-submodulefail", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	withFailingGit(t, "submodule status --recursive")

	blockers, err := Preflight(c, task)
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, b := range blockers {
		if b.Code == "WKT_CHECK_FAILED" && strings.Contains(b.Detail, "submodule check") {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("a failed submodule check must fail closed, got %+v", blockers)
	}

	if _, err := Remove(c, task.Name, false); err == nil {
		t.Fatal("a failed submodule check must block removal")
	}
}

// TestRefusalSeparatesProblemsFromRemedy covers adversarial finding F5. The
// refusal used to pack every blocker into the "remedy" field as
// "CODE repo path detail", so the field meant to say what to do listed what
// was wrong, with an empty path and raw git output inside it.
func TestRefusalSeparatesProblemsFromRemedy(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-shape", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("dist/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, wt, "add", ".gitignore")
	g(t, wt, "commit", "-qm", "ignore dist")
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(wt, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "dist", "out.js"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err = Remove(c, "feat-shape", false)
	if err == nil {
		t.Fatal("a dirty tree must refuse")
	}
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("want a typed error, got %v", err)
	}
	if len(e.Problems) == 0 {
		t.Fatal("the refusal must carry its blockers as problems")
	}
	var sawDirty, sawIgnored bool
	for _, p := range e.Problems {
		switch p.Code {
		case "WKT_DIRTY":
			sawDirty = true
			if p.Repo == "" || p.Path == "" {
				t.Fatalf("a blocker must name its repository and path: %+v", p)
			}
			if p.Info {
				t.Fatal("uncommitted work blocks")
			}
			// The detail used to be one raw line of git status --porcelain,
			// which both leaks git's format and hides every path after the
			// first. Prose, yes — but prose that still names the paths: an
			// empty detail passes a "not porcelain" check while saying
			// nothing at all.
			if p.Detail == "" {
				t.Fatal("the dirty blocker must say what changed")
			}
			if strings.HasPrefix(p.Detail, " M") || strings.HasPrefix(p.Detail, "??") {
				t.Fatalf("detail must be prose, not porcelain: %q", p.Detail)
			}
			if !strings.Contains(p.Detail, "f.txt") {
				t.Fatalf("the modified path must appear in the detail: %q", p.Detail)
			}
		case "WKT_REGENERABLE_IGNORED":
			sawIgnored = true
			if !p.Info {
				t.Fatal("regenerable ignored content is listed, never blocking")
			}
		}
	}
	if !sawDirty || !sawIgnored {
		t.Fatalf("want both the dirty blocker and the informational one, got %+v", e.Problems)
	}
	if len(e.Remedy) == 0 {
		t.Fatal("a refusal must say what to do")
	}
	for _, r := range e.Remedy {
		if strings.Contains(r, "WKT_") {
			t.Fatalf("remedy must hold actions, not problem codes: %q", r)
		}
	}
}

// TestDescribePorcelainKeepsThePaths pins the helper directly. Its first
// version ran TrimSpace over the whole blob, which shifted " M f" left by one
// column and silently produced "1 changed: .txt" — a detail that reads fine
// and names a file that does not exist.
func TestDescribePorcelainKeepsThePaths(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{" M f.txt\n", "1 changed: f.txt"},
		{"M f.txt\n", "1 changed: f.txt"}, // gitx.Run trims, so the first line loses its leading space
		{"?? new.txt\n", "1 changed: new.txt"},
		{"M  staged.txt\n", "1 changed: staged.txt"},
		{"R  old.txt -> new.txt\n", "1 changed: new.txt"},
		{" M a\n?? b\n", "2 changed: a, b"},
		{" M a\n M b\n M c\n M d\n", "4 changed, including a, b, c"},
		{"", ""},
	} {
		if got := describePorcelain(tc.in); got != tc.want {
			t.Errorf("describePorcelain(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestOSArtifactAtTheTreeRootDoesNotBlockRemoval covers live-run finding L2
// at the teardown end: opening a task tree in Finder writes .DS_Store into
// the tree root, which the untracked-tree-content check treated as work at
// risk — so on macOS an ordinary look at the folder made the task
// undeletable without --force.
func TestOSArtifactAtTheTreeRootDoesNotBlockRemoval(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-finder", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	root := c.TreePath("feat-finder")
	if err := os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("finder junk\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t2, err := state.Load(c.StateDir(), "feat-finder")
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := Preflight(c, t2)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range blockers {
		if b.Path != "" && strings.HasSuffix(b.Path, ".DS_Store") && b.Severity != "info" {
			t.Fatalf(".DS_Store must be listed, never blocking: %+v", b)
		}
	}
	if _, err := Remove(c, "feat-finder", false); err != nil {
		t.Fatalf("a tree whose only extra content is an OS artifact must remove cleanly: %v", err)
	}
}

// TestRealUntrackedContentAtTheTreeRootStillBlocks is the other half: the
// exemption is for artifacts, not for anything at the tree root.
func TestRealUntrackedContentAtTheTreeRootStillBlocks(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-notes", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.TreePath("feat-notes"), "notes.md"), []byte("real work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-notes", false); err == nil {
		t.Fatal("untracked content at the tree root must still block removal")
	}
}

// statusWithoutPerimeter is "is this worktree clean apart from the perimeter
// wkt itself wrote". Since task 4, every materialised repository carries an
// untracked .claude/settings.json, so a bare porcelain check no longer means
// what these fixtures need it to mean.
func statusWithoutPerimeter(t *testing.T, wt string) string {
	t.Helper()
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(g(t, wt, "status", "--porcelain", "-uall"), "\n"), "\n") {
		if line == "" || strings.HasSuffix(line, ".claude/settings.json") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestUserContentBesideThePerimeterStillBlocks — the exemption added in task 4
// is for the one file wkt wrote, not for the .claude directory. A mutation
// that exempted the whole directory survived the suite until this test
// existed, which is exactly the shape of "the guard is gone and everything is
// still green".
func TestUserContentBesideThePerimeterStillBlocks(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-beside", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	root := c.TreePath("feat-beside")
	// A file the user put next to the perimeter, in the same directory.
	if err := os.MkdirAll(filepath.Join(root, ".claude", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "agents", "reviewer.md"),
		[]byte("my custom agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-beside", false); err == nil {
		t.Fatal("content the user put beside the perimeter must still block removal")
	}

	// And the same one directory deeper, inside a materialised repository.
	c2, entries2 := fixture(t)
	if _, err := Create(c2, entries2, "feat-beside2", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	repoClaude := filepath.Join(c2.TreePath("feat-beside2"), "docs", ".claude")
	if err := os.WriteFile(filepath.Join(repoClaude, "notes.md"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c2, "feat-beside2", false); err == nil {
		t.Fatal("content beside the perimeter inside a repository must block too")
	}
}

// TestClaudeCodeBookkeepingDoesNotBlockRemoval — Claude Code writes
// .claude/.cc-writes into a tree the first time the agent edits a file.
// Verified end to end against 2.1.238 through the worktree hooks: without
// this exemption, every task a session actually worked in needed --force to
// remove, which is the reflexive --force this whole check exists to avoid.
func TestClaudeCodeBookkeepingDoesNotBlockRemoval(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-cc", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	root := c.TreePath("feat-cc")
	if err := os.MkdirAll(filepath.Join(root, ".claude", ".cc-writes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", ".cc-writes", "log.jsonl"),
		[]byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-cc", false); err != nil {
		t.Fatalf("Claude Code's own bookkeeping must not block removal: %v", err)
	}
}

// TestUserFilesInsideDotClaudeStillBlock — the exemption is that one
// directory, not all of .claude, which holds the user's agents and
// instructions.
func TestUserFilesInsideDotClaudeStillBlock(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-cc2", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	root := c.TreePath("feat-cc2")
	if err := os.MkdirAll(filepath.Join(root, ".claude", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".claude", "agents", "reviewer.md"),
		[]byte("my agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-cc2", false); err == nil {
		t.Fatal("a file the user wrote inside .claude must still block removal")
	}
}

// TestForceNeverDestroysARepositoryCreatedInsideTheTree — reported as issue #1
// and reproduced exactly: someone starts a new service while working on a
// task ("git init" inside the tree, a few commits, nothing pushed), and
// "wkt rm --force" deletes the only copy of that history.
//
// --force means "discard the worktree changes I know about". It cannot mean
// "delete a repository", because that one is not recoverable from anywhere:
// there is no store behind it. Spec §5.7 already says so; the walk simply
// never got far enough to notice, because it reported the directory as
// untracked content and stopped descending.
func TestForceNeverDestroysARepositoryCreatedInsideTheTree(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-newsvc", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(c.TreePath("feat-newsvc"), "services", "svc-new")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fresh, "a.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, fresh, "init", "-q")
	g(t, fresh, "add", "-A")
	g(t, fresh, "commit", "-qm", "new service, never pushed anywhere")
	sha := strings.TrimSpace(g(t, fresh, "rev-parse", "HEAD"))

	// Without --force it already refuses; the point is that --force must not
	// override this particular refusal.
	_, err := Remove(c, "feat-newsvc", true)
	if err == nil {
		t.Fatal("--force must not delete a repository whose history exists nowhere else")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_FOREIGN_REPO" {
		t.Fatalf("want WKT_FOREIGN_REPO, got %v", err)
	}
	if !strings.Contains(strings.Join(e.Remedy, " "), "move") &&
		!strings.Contains(strings.Join(e.Remedy, " "), "push") {
		t.Fatalf("the refusal must say what to do with it: %+v", e)
	}

	// And it is all still there.
	if _, statErr := os.Stat(filepath.Join(fresh, "a.txt")); statErr != nil {
		t.Fatal("the repository was deleted anyway")
	}
	if !gitx.RunOK(fresh, "cat-file", "-e", sha) {
		t.Fatal("its history is gone")
	}
}

// TestForeignRepoIsFoundBelowUntrackedContent pins the mechanism rather than
// the symptom: the walk used to report an untracked directory and stop, so a
// repository one level inside it was invisible to every later check.
func TestForeignRepoIsFoundBelowUntrackedContent(t *testing.T) {
	c, entries := fixture(t)
	if _, err := Create(c, entries, "feat-deep", []string{"docs"}); err != nil {
		t.Fatal(err)
	}
	// Two levels of ordinary untracked directories, then a repository.
	deep := filepath.Join(c.TreePath("feat-deep"), "scratch", "experiments", "prototype")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, deep, "init", "-q")
	g(t, deep, "add", "-A")
	g(t, deep, "commit", "-qm", "prototype")

	tk, err := state.Load(c.StateDir(), "feat-deep")
	if err != nil {
		t.Fatal(err)
	}
	blockers, err := Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var sawForeign bool
	for _, b := range blockers {
		if b.Code == "WKT_FOREIGN_REPO" {
			sawForeign = true
		}
	}
	if !sawForeign {
		t.Fatalf("a repository below untracked content must still be found: %+v", blockers)
	}

	// The untracked directory itself is reported once, not once per file
	// inside it: descending must not turn one problem into a wall of them.
	var untracked int
	for _, b := range blockers {
		if b.Code == "WKT_UNTRACKED_TREE_CONTENT" {
			untracked++
		}
	}
	if untracked > 1 {
		t.Fatalf("untracked content should be reported once per directory, got %d entries", untracked)
	}
}

// TestForcedRemovalKeepsUnpushedWorkReachable — reported as issue #2. After
// "wkt rm --force" the objects survive in the store, but nothing points at
// them: the branch went with the tree. Recovery is possible and requires
// knowing a store exists, finding the right *.git inside it, and digging for
// a dangling commit — which in practice reads as data loss, at the moment the
// user trusts the tool least.
//
// A ref costs nothing and turns that into a line the tool can print.
func TestForcedRemovalKeepsUnpushedWorkReachable(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-keep", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := task.Repos[0].WorktreePath
	g(t, wt, "commit", "-qm", "never pushed", "--allow-empty")
	sha := strings.TrimSpace(g(t, wt, "rev-parse", "HEAD"))
	storePath := filepath.Join(c.StoreDir(), task.Repos[0].StoreID+".git")

	if _, err := Remove(c, "feat-keep", true); err != nil {
		t.Fatal(err)
	}

	// The commit is still there — that part already worked.
	if !gitx.RunOK(storePath, "cat-file", "-e", sha) {
		t.Fatal("the objects should survive in the store")
	}
	// And now something points at it.
	kept := strings.TrimSpace(g(t, storePath, "rev-parse", "--verify", "--quiet", "refs/wkt/removed/feat-keep"))
	if kept != sha {
		t.Fatalf("refs/wkt/removed/feat-keep is %q, want the removed branch tip %q", kept, sha)
	}
	// Reachable, so an ordinary "git log" finds it rather than a fsck for
	// dangling objects.
	out := g(t, storePath, "rev-list", "--all")
	if !strings.Contains(out, sha) {
		t.Fatal("the kept commit must be reachable from a ref, not merely present")
	}
}

// TestForcedRemovalKeepsNothingWhenThereWasNothingToKeep — a task whose work
// was pushed, or which never committed anything, should leave no refs behind.
// Keeping one for every removal would turn the store into a graveyard and
// pin objects gc should be free to drop.
func TestForcedRemovalKeepsNothingWhenThereWasNothingToKeep(t *testing.T) {
	c, entries := fixture(t)
	task, err := Create(c, entries, "feat-nothing", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(c.StoreDir(), task.Repos[0].StoreID+".git")
	// Dirty, but nothing committed: --force is discarding worktree changes,
	// which is exactly what it is for.
	if err := os.WriteFile(filepath.Join(task.Repos[0].WorktreePath, "scratch.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(c, "feat-nothing", true); err != nil {
		t.Fatal(err)
	}
	if out := strings.TrimSpace(g(t, storePath, "for-each-ref", "--format=%(refname)", "refs/wkt/removed/")); out != "" {
		t.Fatalf("nothing was at risk, so nothing should be kept: %q", out)
	}
}

// TestRemoveTreatsProducedContentAsInformation — a local database the
// post-create script made is disposable: it is not the developer's, it was
// generated, and blocking on it would make --force the habit for every task
// that runs a setup step. Which is the habit this check exists to prevent.
func TestRemoveTreatsProducedContentAsInformation(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-prod", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := tk.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("*.sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, wt, "add", "-A")
	g(t, wt, "commit", "-qm", "ignore rules")
	if err := os.WriteFile(filepath.Join(wt, "local.sqlite"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without the record it is precious ignored content and blocks.
	blockers, err := Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" && strings.Contains(b.Path, "local.sqlite") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("unrecorded ignored content must still block; the fixture is not exercising the case")
	}

	tk.Links = append(tk.Links, state.LinkSlot{RelPath: "docs/local.sqlite", Type: "produced"})
	blockers, err = Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, b := range blockers {
		if b.Code == "WKT_PRECIOUS_IGNORED" && strings.Contains(b.Path, "local.sqlite") {
			t.Fatal("produced content must not block removal")
		}
		if b.Code == "WKT_PRODUCED" && b.Severity == "info" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("produced content must still be reported, not silently passed over")
	}
}

// The script can also write outside every repository — an intermediate
// directory of the mirrored tree, or the tree root. Teardown meets that as
// untracked tree content rather than as ignored content, so it needs the same
// record. Found by the battery, not by design.
func TestRemoveTreatsProducedTreeContentAsInformation(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-prod-tree", []string{"services/svc-a"})
	if err != nil {
		t.Fatal(err)
	}
	tree := c.TreePath(tk.Name)
	beside := filepath.Join(tree, "services", "installed")
	if err := os.WriteFile(beside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	blockers, err := Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var blocked bool
	for _, b := range blockers {
		if b.Code == "WKT_UNTRACKED_TREE_CONTENT" && strings.Contains(b.Path, "installed") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatal("unrecorded tree content must still block; the fixture is not exercising the case")
	}

	tk.Links = append(tk.Links, state.LinkSlot{RelPath: "services/installed", Type: "produced"})
	blockers, err = Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, b := range blockers {
		if b.Code == "WKT_UNTRACKED_TREE_CONTENT" && strings.Contains(b.Path, "installed") {
			t.Fatal("produced tree content must not block removal")
		}
		if b.Code == "WKT_PRODUCED" && strings.Contains(b.Path, "installed") && b.Severity == "info" {
			reported = true
		}
	}
	if !reported {
		t.Fatal("produced tree content must still be reported")
	}
}

// A produced directory collapses to one line in git's reporting, so exempting
// it by path exempts everything created inside it afterwards. The script made
// an ignored directory; a developer then drops a key in there, and a plain
// wkt rm would delete it without a word. A produced *file* is different: it
// was generated, and a service rewriting its own database is expected.
func TestAProducedDirectoryDoesNotExemptWhatIsPutInItLater(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-secrets", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	wt := tk.Repos[0].WorktreePath
	if err := os.WriteFile(filepath.Join(wt, ".gitignore"), []byte(".secrets/\n*.sqlite\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, wt, "add", "-A")
	g(t, wt, "commit", "-qm", "ignore rules")
	if err := os.MkdirAll(filepath.Join(wt, ".secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".secrets", "prod.pem"), []byte("KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "local.sqlite"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk.Links = append(tk.Links,
		state.LinkSlot{RelPath: "docs/.secrets/", Type: "produced"},
		state.LinkSlot{RelPath: "docs/local.sqlite", Type: "produced"})

	blockers, err := Preflight(c, tk)
	if err != nil {
		t.Fatal(err)
	}
	var dirBlocks, fileExempt bool
	for _, b := range blockers {
		if strings.Contains(b.Path, ".secrets") && b.Severity != "info" {
			dirBlocks = true
		}
		if strings.Contains(b.Path, "local.sqlite") && b.Code == "WKT_PRODUCED" && b.Severity == "info" {
			fileExempt = true
		}
	}
	if !dirBlocks {
		t.Fatal("a produced directory must not exempt what was put in it later")
	}
	if !fileExempt {
		t.Fatal("a produced file is still disposable; blocking on it brings --force back as a habit")
	}
}

// TestForeignRepoNamesEveryOneAtOnce — the refusal found every foreign
// repository and reported one, so clearing a tree meant moving one out,
// retrying, and meeting the next. WKT_WOULD_LOSE_WORK already lists all its
// blockers in problems[]; this is the same question with a different answer
// shape for no reason (issue #5).
func TestForeignRepoNamesEveryOneAtOnce(t *testing.T) {
	c, entries := fixture(t)
	tk, err := Create(c, entries, "feat-foreign", []string{"docs"})
	if err != nil {
		t.Fatal(err)
	}
	tree := c.TreePath(tk.Name)
	for _, name := range []string{"svc-new", "svc-other"} {
		dir := filepath.Join(tree, "services", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		g(t, dir, "init", "-q")
		g(t, dir, "add", "-A")
		g(t, dir, "commit", "-qm", "own history")
	}

	_, err = Remove(c, "feat-foreign", true)
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_FOREIGN_REPO" {
		t.Fatalf("want WKT_FOREIGN_REPO, got %v", err)
	}
	if len(e.Problems) != 2 {
		t.Fatalf("both repositories must be listed at once, got %d: %+v", len(e.Problems), e.Problems)
	}
	var seen []string
	for _, p := range e.Problems {
		if p.Info {
			t.Fatalf("every one of them blocks: %+v", p)
		}
		seen = append(seen, filepath.Base(p.Path))
	}
	sort.Strings(seen)
	if seen[0] != "svc-new" || seen[1] != "svc-other" {
		t.Fatalf("want both named, got %v", seen)
	}
}
