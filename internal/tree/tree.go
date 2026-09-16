// Package tree materialises a task's tree: repositories mirrored at their
// workspace-relative positions, un-materialised repositories back-filled as
// absolute symlinks so cross-repo references still resolve, non-git
// directories linked, and loose files copied with their content hash
// recorded.
package tree

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/Venut-Technologies/wkt/internal/artifact"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// MaxCopyBytes is where a loose file stops being copied into every task and
// starts being linked. Chosen to sit above source files, notes and scripts —
// the things a task actually edits — and below the media and datasets that
// happen to share a directory with them.
const MaxCopyBytes = 1 << 20 // 1 MiB

type Plan struct {
	Materialise []string // repositories that become real worktrees
	BackFill    []string // repositories present only as symlinks to the workspace
	LinkDirs    []string // non-git directories
	CopyFiles   []string // loose files
}

func PlanFor(workspace string, entries []discover.Entry, selected []string) (Plan, error) {
	sel := map[string]bool{}
	for _, s := range selected {
		sel[s] = true
	}
	var p Plan
	repoPaths := map[string]bool{}
	for _, e := range entries {
		if e.Kind != discover.KindRepo {
			continue
		}
		repoPaths[e.RelPath] = true
		if sel[e.RelPath] {
			p.Materialise = append(p.Materialise, e.RelPath)
		} else {
			p.BackFill = append(p.BackFill, e.RelPath)
		}
	}
	for _, s := range selected {
		if !repoPaths[s] {
			return Plan{}, wkterr.New("WKT_NO_SUCH_REPO", "not a discovered repository").WithRepo(s)
		}
	}
	// Directories on the path to anything the tree places — a materialised
	// worktree, or a back-filled repository's own leaf symlink — must stay
	// real directories. A back-filled repo is a symlink only at its own
	// position; every ancestor above it still has to be real, or the whole
	// ancestor chain would collapse into one whole-directory link and bury
	// the repository (and any of its siblings) inside it.
	ancestors := map[string]bool{}
	addAncestors := func(rel string) {
		for d := filepath.Dir(rel); d != "." && d != string(filepath.Separator); d = filepath.Dir(d) {
			ancestors[d] = true
		}
	}
	for _, m := range p.Materialise {
		addAncestors(m)
	}
	for _, b := range p.BackFill {
		addAncestors(b)
	}
	if err := planDir(workspace, "", repoPaths, ancestors, &p); err != nil {
		return Plan{}, err
	}
	return p, nil
}

// planDir buckets the children of the workspace-relative directory dirRel
// (dirRel == "" for the workspace root). It recurses only into directories
// that lie on the path to something the tree places (ancestors) — anything
// else is a leaf: a repository already bucketed by the caller, a directory
// to link whole, or a file to copy. This is what keeps content that lives
// alongside an ancestor (a sibling directory or loose file inside a
// materialised or back-filled repo's parent) from silently falling out of
// the plan.
func planDir(workspace, dirRel string, repoPaths, ancestors map[string]bool, p *Plan) error {
	abs := filepath.Join(workspace, dirRel)
	top, err := os.ReadDir(abs)
	if err != nil {
		return wkterr.New("WKT_WORKSPACE_UNREADABLE", "cannot read the workspace").WithPath(abs)
	}
	for _, e := range top {
		name := e.Name()
		if dirRel == "" && (name == ".claude" || name == ".wkt") {
			continue
		}
		rel := name
		if dirRel != "" {
			rel = filepath.Join(dirRel, name)
		}
		if repoPaths[rel] {
			continue // already bucketed into Materialise or BackFill
		}
		if ancestors[rel] {
			if err := planDir(workspace, rel, repoPaths, ancestors, p); err != nil {
				return err
			}
			continue
		}
		relSlash := filepath.ToSlash(rel)
		// e.IsDir() is Lstat-based (os.ReadDir never follows symlinks), so a
		// symlink — an ordinary workspace fixture like "current", "bin" or
		// "data" — always reports false here, regardless of what it points
		// at. Bucketing it into CopyFiles on that basis used to route it
		// into copyFile, which os.Stat's (following the link): a symlink to
		// a directory then opens successfully as a directory and the
		// content copy fails, breaking "wkt new" on an ordinary symlink and
		// naming the destination in the error without ever mentioning the
		// symlink. An explicit Lstat here — rather than trusting
		// DirEntry.Type(), which is not guaranteed populated on every
		// platform — routes any symlink to a link slot instead: wkt creates
		// its own symlink pointing at the workspace's, so the chain
		// resolves exactly as it would from the workspace itself, whatever
		// it ultimately points at.
		info, statErr := os.Lstat(filepath.Join(abs, name))
		switch {
		case statErr != nil:
			return wkterr.New("WKT_WORKSPACE_UNREADABLE", "cannot inspect a workspace entry").WithPath(filepath.Join(abs, name))
		case info.Mode()&os.ModeSymlink != 0, info.IsDir():
			p.LinkDirs = append(p.LinkDirs, relSlash)
		case info.Size() > MaxCopyBytes:
			// Copying is right for the files a task edits, and wrong for the
			// ones it only reads. The first real workspace this ran against
			// had 19 slide PNGs in an ancestor directory, and every task
			// copied all of them; a directory of datasets would be copied
			// whole, per task. Above the threshold the file is linked
			// instead — visible from the tree, edited only through a path
			// the perimeter denies, and checked at teardown as a link slot.
			p.LinkDirs = append(p.LinkDirs, relSlash)
		case artifact.IsRegenerable(relSlash):
			// An OS artifact (.DS_Store and friends) carries no work and is
			// rewritten by the file manager on its own. Copying it in makes
			// a copy slot whose hash diverges the first time anyone opens
			// the *tree* in Finder, which then blocked removal on a file
			// nobody created on purpose (live-run finding L2).
		default:
			p.CopyFiles = append(p.CopyFiles, relSlash)
		}
	}
	return nil
}

func Materialise(treeRoot, workspace string, p Plan) ([]state.LinkSlot, error) {
	var slots []state.LinkSlot
	// link creates a whole-directory (or whole-file) link slot at rel.
	// checkNested gates the nested-repository scan (spec §5.3 rule 4): a
	// back-filled repository's own slot is deliberately a live link to a
	// repository wkt already knows about, but an ordinary non-git directory
	// (p.LinkDirs) must never be linked whole without first walking it for
	// a repository sitting below the depth-bounded discovery scan — without
	// that check the SAME real directory becomes writable and shared by
	// every task's tree, and by the workspace itself.
	link := func(rel string, checkNested bool) error {
		dst := filepath.Join(treeRoot, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return wkterr.New("WKT_TREE_BUILD", "cannot create an ancestor directory").WithPath(dst)
		}
		src := filepath.Join(workspace, rel) // absolute target (spec §5.3 rule 3)
		if checkNested {
			resolved, rerr := paths.Canonical(src)
			if rerr != nil {
				resolved = src
			}
			if found := findNestedRepo(resolved); found != "" {
				return wkterr.New("WKT_NESTED_REPO", "a repository lies beneath a directory wkt would otherwise link whole").
					WithPath(dst).WithFound(found)
			}
		}
		switch info, err := os.Lstat(dst); {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			// Already a symlink. Only a match on the exact intended target is
			// idempotent; anything else is a conflict, not a silent overwrite.
			actual, rerr := os.Readlink(dst)
			if rerr != nil {
				return wkterr.New("WKT_TREE_BUILD", "cannot read an existing link slot").WithPath(dst).WithFound(rerr.Error())
			}
			if actual != src {
				return wkterr.New("WKT_TREE_CONFLICT", "tree path already exists and is not the expected link").
					WithPath(dst).WithExpected(src).WithFound(actual)
			}
		case err == nil:
			// Exists and is not a symlink at all: never silently swallowed.
			kind := "a file"
			if info.IsDir() {
				kind = "a directory"
			}
			return wkterr.New("WKT_TREE_CONFLICT", "tree path already exists and is not the expected link").
				WithPath(dst).WithExpected(src).WithFound(kind)
		case os.IsNotExist(err):
			if err := os.Symlink(src, dst); err != nil {
				return wkterr.New("WKT_TREE_BUILD", "cannot create a link slot").WithPath(dst).WithFound(err.Error())
			}
		default:
			return wkterr.New("WKT_TREE_BUILD", "cannot inspect a tree path").WithPath(dst).WithFound(err.Error())
		}
		slots = append(slots, state.LinkSlot{RelPath: rel, Target: src, Type: "symlink"})
		return nil
	}
	for _, rel := range p.BackFill {
		if err := link(rel, false); err != nil {
			return nil, err
		}
	}
	for _, rel := range p.LinkDirs {
		if err := link(rel, true); err != nil {
			return nil, err
		}
	}
	for _, rel := range p.CopyFiles {
		src := filepath.Join(workspace, rel)
		dst := filepath.Join(treeRoot, rel)
		sum, err := copyFile(src, dst)
		if err != nil {
			return nil, err
		}
		slots = append(slots, state.LinkSlot{RelPath: rel, Target: src, Type: "copy", Hash: sum})
	}
	return slots, nil
}

// findNestedRepo walks dir to unbounded depth, never following symlinks,
// looking for a ".git" marker at any depth — a repository sitting below
// the depth-bounded repository *enumeration* scan (spec §5.3 rule 4: "two
// different scans, two different bounds"). It returns the first
// repository directory found, or "" if none. An unreadable subtree is
// skipped, not treated as a scan failure — the same convention
// discover.Walk already uses for repository enumeration.
// HiddenRepos reports repositories that live below dir but were never
// discovered, which is what makes a directory unlinkable (spec §5.3 rule 4).
// init uses it to warn while the workspace is still being adopted, rather
// than letting the first "wkt new" be where the user finds out.
func HiddenRepos(dirs []string) []string {
	var out []string
	for _, d := range dirs {
		if found := findNestedRepo(d); found != "" {
			out = append(out, found)
		}
	}
	return out
}

func findNestedRepo(dir string) string {
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return ""
	}
	var found string
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort the walk
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // never follow symlinks
		}
		if d.Name() != ".git" {
			return nil
		}
		found = filepath.Dir(p)
		return fs.SkipAll
	})
	return found
}

func copyFile(src, dst string) (string, error) {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot stat a workspace file").WithPath(src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot read a workspace file").WithPath(src)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot create a directory").WithPath(dst)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, srcInfo.Mode().Perm())
	if err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot write into the tree").WithPath(dst)
	}
	defer out.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(out, h), in); err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot copy a workspace file").WithPath(dst)
	}
	// OpenFile's mode is subject to umask; chmod explicitly so the copy's
	// permissions (notably the execute bit on scripts and hooks) match the
	// source exactly, not whatever the process umask allowed through.
	if err := out.Chmod(srcInfo.Mode().Perm()); err != nil {
		return "", wkterr.New("WKT_TREE_BUILD", "cannot set permissions on a copied file").WithPath(dst)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Hash re-reads a copied file so teardown can detect divergence (spec §5.3 rule 5).
func Hash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
