package task

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/perimeter"
	"github.com/Venut-Technologies/wkt/internal/state"
)

// RepairResult is one thing repair looked at.
type RepairResult struct {
	Repo     string
	Repaired bool
	Detail   string
}

// Repair fixes the pointers a moved workspace or a re-cloned repository
// breaks: the gitdir back-pointers between a worktree and its store, and the
// link slots that give the tree its shape (spec §6).
//
// It fixes; it does not clear the way first. A link slot that has become a
// real directory holds something wkt did not create, and restoring the link
// would delete it — so that case is reported and left alone. Every outcome
// here is meant to be predictable enough to describe in one line, because a
// repair command that sometimes deletes is a repair command nobody runs.
func Repair(c container.C, name string) ([]RepairResult, error) {
	t, err := state.Load(c.StateDir(), name)
	if err != nil {
		return nil, err
	}

	// A moved workspace is the case this command is named for (spec §6), and
	// state is full of absolute paths: rewrite them before touching git, or
	// every repair below runs against where things used to be.
	var out []RepairResult
	if rewritten := relocate(&t, c); rewritten {
		if err := state.Save(c.StateDir(), t); err != nil {
			return nil, err
		}
		out = append(out, RepairResult{
			Repo: t.Name, Repaired: true,
			Detail: "state pointed at a previous location; rewritten to " + c.Root,
		})
		// The perimeter's deny rules name the old container and workspace by
		// absolute path, so after a move it protects directories that no
		// longer exist. Regenerating is the only thing that makes it true
		// again.
		names, _ := state.List(c.StateDir())
		if coverage, hashes, pErr := perimeter.Write(c, t, names); pErr == nil {
			t.PerimeterCoverage, t.PerimeterHashes = coverage, hashes
			_ = state.Save(c.StateDir(), t)
			out = append(out, RepairResult{Repo: t.Name, Repaired: true, Detail: "regenerated the perimeter for the new location"})
		}
	}

	for _, r := range t.Repos {
		storePath := filepath.Join(c.StoreDir(), r.StoreID+".git")
		if _, err := gitx.Run(r.WorktreePath, "rev-parse", "--git-dir"); err == nil {
			out = append(out, RepairResult{Repo: r.RelPath, Detail: "worktree is attached"})
			continue
		}
		// The path argument is mandatory: bare "worktree repair" does not
		// rediscover a tree that moved (spec §5.7).
		//
		// Its exit status is deliberately ignored. Measured against real git:
		// repairing a worktree whose .git file points nowhere prints
		// "repair: .git file broken" and exits 1 — *and rewrites the file
		// correctly anyway*. Believing the status here reported a failure on
		// the exact case this command exists for.
		_, _ = gitx.Run(storePath, "worktree", "repair", r.WorktreePath)
		if _, err := gitx.Run(r.WorktreePath, "rev-parse", "--git-dir"); err != nil {
			out = append(out, RepairResult{Repo: r.RelPath, Detail: "still detached after a repair attempt"})
			continue
		}
		out = append(out, RepairResult{Repo: r.RelPath, Repaired: true, Detail: "reattached the worktree to its store"})
	}

	for _, slot := range t.Links {
		if slot.Type != "symlink" {
			continue
		}
		p := filepath.Join(c.TreePath(name), filepath.FromSlash(slot.RelPath))
		info, err := os.Lstat(p)
		switch {
		case err == nil && info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(p)
			if readErr == nil && target == slot.Target {
				out = append(out, RepairResult{Repo: slot.RelPath, Detail: "link slot is intact"})
				continue
			}
			// The recorded target moved with the workspace, so the link on
			// disk still points where the workspace used to be. Replacing a
			// symlink deletes nothing but the symlink.
			if rmErr := os.Remove(p); rmErr != nil {
				out = append(out, RepairResult{Repo: slot.RelPath, Detail: "cannot replace the stale link slot"})
				continue
			}
			if linkErr := os.Symlink(slot.Target, p); linkErr != nil {
				out = append(out, RepairResult{Repo: slot.RelPath, Detail: "cannot re-aim the link slot"})
				continue
			}
			out = append(out, RepairResult{Repo: slot.RelPath, Repaired: true, Detail: "re-aimed the link slot at " + slot.Target})
		case err == nil:
			out = append(out, RepairResult{
				Repo:   slot.RelPath,
				Detail: "a real file or directory stands where the link slot was: wkt will not delete it to restore the link (" + slot.RelPath + ")",
			})
		default:
			if mkErr := os.MkdirAll(filepath.Dir(p), 0o755); mkErr != nil {
				out = append(out, RepairResult{Repo: slot.RelPath, Detail: "cannot create the parent directory"})
				continue
			}
			if linkErr := os.Symlink(slot.Target, p); linkErr != nil {
				out = append(out, RepairResult{Repo: slot.RelPath, Detail: "cannot restore the link slot"})
				continue
			}
			out = append(out, RepairResult{Repo: slot.RelPath, Repaired: true, Detail: "restored the back-fill link"})
		}
	}
	return out, nil
}

// relocate rewrites a task's recorded paths when the container or workspace
// has moved, reporting whether anything changed.
//
// Only the two prefixes are rewritten. Anything that does not sit under the
// old container or the old workspace is left exactly as it is: a path wkt did
// not place is not wkt's to reinterpret.
func relocate(t *state.Task, c container.C) bool {
	oldRoot, oldWS := t.Container, t.Workspace
	if oldRoot == c.Root && oldWS == c.Workspace {
		return false
	}
	swap := func(p string) string {
		if oldRoot != "" && strings.HasPrefix(p, oldRoot) {
			return c.Root + strings.TrimPrefix(p, oldRoot)
		}
		if oldWS != "" && strings.HasPrefix(p, oldWS) {
			return c.Workspace + strings.TrimPrefix(p, oldWS)
		}
		return p
	}

	t.Container, t.Workspace = c.Root, c.Workspace
	t.WorkspaceSpellings = paths.Spellings(c.Workspace)
	for i := range t.Repos {
		t.Repos[i].AbsPath = swap(t.Repos[i].AbsPath)
		t.Repos[i].WorktreePath = swap(t.Repos[i].WorktreePath)
	}
	for i := range t.Links {
		t.Links[i].Target = swap(t.Links[i].Target)
	}
	moved := make([]string, 0, len(t.PerimeterCoverage))
	for _, dir := range t.PerimeterCoverage {
		moved = append(moved, swap(dir))
	}
	t.PerimeterCoverage = moved
	// The hashes are keyed by directory, so they move with it.
	rehashed := make(map[string]string, len(t.PerimeterHashes))
	for dir, h := range t.PerimeterHashes {
		rehashed[swap(dir)] = h
	}
	t.PerimeterHashes = rehashed
	return true
}
