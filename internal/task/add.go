package task

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/perimeter"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/store"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Add grafts one more repository onto an existing task.
//
// The grafting point is the task's recorded **base epoch**, not today's
// origin/HEAD (spec §6). A task is a coherent slice of time across a set of
// repositories: a repository joining late at today's tip would be based on
// work the rest of the set has never seen, and the eventual merges would not
// line up. Everything committed to that repository after the task was cut is
// simply not in the tree.
func Add(c container.C, entries []discover.Entry, name, relPath string) error {
	t, err := state.Load(c.StateDir(), name)
	if err != nil {
		return err
	}
	for _, r := range t.Repos {
		if r.RelPath == relPath {
			return wkterr.New("WKT_REPO_IN_TASK", "the task already includes that repository").
				WithRepo(relPath).WithRemedy("wkt status " + name + " lists what it holds")
		}
	}
	var entry *discover.Entry
	for i := range entries {
		if entries[i].RelPath == relPath && entries[i].Kind == discover.KindRepo {
			entry = &entries[i]
		}
	}
	if entry == nil {
		return wkterr.New("WKT_NO_SUCH_REPO", "not a discovered repository").
			WithRepo(relPath).WithRemedy("wkt init lists what this workspace holds")
	}

	sha, ref, err := baseAtEpoch(entry.AbsPath, t.BaseEpoch)
	if err != nil {
		return err
	}
	repo := state.Repo{
		RelPath: relPath, AbsPath: entry.AbsPath,
		StoreID: store.ID(relPath, entry.AbsPath),
		Branch:  name, BaseSHA: sha, BaseRef: ref,
		WorktreePath: filepath.Join(c.TreePath(name), filepath.FromSlash(relPath)),
		BasePinRef:   "refs/wkt/base/" + name,
	}

	// The repository is already in the tree — as a back-fill symlink into the
	// workspace, which is what an unselected repository looks like (spec §5.3
	// rule 3). Adding it means replacing that link with a real worktree, so
	// the link is not an obstacle; anything else at that path is.
	var backFill *state.LinkSlot
	for i := range t.Links {
		if t.Links[i].RelPath == relPath && t.Links[i].Type == "symlink" {
			backFill = &t.Links[i]
		}
	}
	if err := validateOne(c, repo, name, backFill != nil); err != nil {
		return err
	}

	var undos []undo
	rollback := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
	}

	// The pin is written by store.Ensure as its first action, so its undo is
	// registered before the call rather than after it returns — the same
	// ordering defect the foundation plan shipped twice.
	repoAbs, pin := repo.AbsPath, repo.BasePinRef
	undos = append(undos, func() { _, _ = gitx.Run(repoAbs, "update-ref", "-d", pin) })

	storePath, err := store.Ensure(c.StoreDir(), repo.AbsPath, repo.RelPath, name, repo.BaseSHA)
	if err != nil {
		rollback()
		return err
	}
	if err := store.FetchWorkspace(storePath); err != nil {
		rollback()
		return err
	}
	// Defence in depth, and deliberately without a test: the fetch above
	// should always bring the epoch's commit, so reaching this means the
	// repository's history was rewritten or pruned between resolving the base
	// and mirroring it. Cheap to check, and the alternative is a worktree at a
	// commit the store cannot produce.
	if !store.HasObject(storePath, repo.BaseSHA) {
		rollback()
		return wkterr.New("WKT_BASE_UNREACHABLE", "the task's base epoch is no longer reachable in this repository").
			WithRepo(relPath).WithFound(repo.BaseSHA).
			WithRemedy("the history was rewritten or pruned since the task was created",
				"create a new task instead of grafting onto this one")
	}

	undos = append(undos, func() { _, _ = gitx.Run(storePath, "branch", "-D", name) })
	wtPath := repo.WorktreePath

	// Remove the back-fill link so the worktree can take its place, and put it
	// back if anything below fails: a rolled-back add must leave the task
	// exactly as it found it, link included.
	if backFill != nil {
		target := backFill.Target
		if err := os.Remove(wtPath); err != nil && !os.IsNotExist(err) {
			rollback()
			return wkterr.New("WKT_TREE_BUILD", "cannot replace the back-fill link").WithPath(wtPath)
		}
		undos = append(undos, func() { _ = os.Symlink(target, wtPath) })
	}
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		rollback()
		return wkterr.New("WKT_TREE_BUILD", "cannot create the repository's parent directory").WithPath(wtPath)
	}
	if _, err := gitx.Run(storePath, "worktree", "add", "--quiet", "-b", name, wtPath, repo.BaseSHA); err != nil {
		rollback()
		// Same reason as create's site: git's explanation is the only thing
		// that distinguishes a filter failure from a path collision.
		return wkterr.New("WKT_WORKTREE_ADD", "cannot add the worktree").
			WithRepo(relPath).WithPath(wtPath).WithFound(err.Error())
	}
	undos = append(undos, func() {
		_, _ = gitx.Run(storePath, "worktree", "remove", "--force", "--force", wtPath)
	})
	if _, err := gitx.Run(storePath, "worktree", "lock", "--reason", "held by wkt task "+name, wtPath); err != nil {
		rollback()
		return wkterr.New("WKT_WORKTREE_LOCK", "cannot lock the worktree").WithRepo(relPath)
	}
	wtName, err := worktreeName(wtPath)
	if err != nil {
		rollback()
		return err
	}
	repo.StoreWorktreeName = wtName

	t.Repos = append(t.Repos, repo)
	if backFill != nil {
		kept := t.Links[:0]
		for _, l := range t.Links {
			if l.RelPath != relPath {
				kept = append(kept, l)
			}
		}
		t.Links = kept
	}
	names, _ := state.List(c.StateDir())
	coverage, hashes, err := perimeter.Write(c, t, names)
	if err != nil {
		rollback()
		return err
	}
	t.PerimeterCoverage, t.PerimeterHashes = coverage, hashes
	if err := state.Save(c.StateDir(), t); err != nil {
		rollback()
		return err
	}
	return nil
}

// baseAtEpoch finds the commit the repository was at when the task was cut.
// git's --before takes the newest commit at or before that instant on the same
// ref resolution order create uses, so a repository that has moved on since is
// grafted where the rest of the set is, not where it is today.
func baseAtEpoch(repoAbs string, epoch time.Time) (sha, ref string, err error) {
	_, ref, err = resolveBase(repoAbs)
	if err != nil {
		return "", "", err
	}
	if epoch.IsZero() {
		s, e := gitx.Run(repoAbs, "rev-parse", ref)
		return s, ref, e
	}
	out, err := gitx.Run(repoAbs, "rev-list", "-1",
		"--before="+epoch.UTC().Format(time.RFC3339), ref)
	if err != nil || strings.TrimSpace(out) == "" {
		// Every commit on that ref is newer than the task. Grafting at the
		// tip would silently import work the rest of the set has not seen.
		return "", "", wkterr.New("WKT_BASE_UNREACHABLE",
			"this repository has no commit as old as the task's base epoch").
			WithPath(repoAbs).WithExpected(epoch.UTC().Format(time.RFC3339)).
			WithRemedy("create a new task, whose epoch is now")
	}
	return strings.TrimSpace(out), ref, nil
}

// validateOne is Validate's per-repository half, for the case where the rest
// of the set is already built and only the newcomer is in question.
func validateOne(c container.C, r state.Repo, name string, pathIsBackFill bool) error {
	if gitx.RunOK(r.AbsPath, "rev-parse", "--verify", "refs/heads/"+name) {
		return wkterr.New("WKT_BRANCH_EXISTS", "a branch of that name already exists").
			WithRepo(r.RelPath).WithRemedy("delete the branch, or use a different task")
	}
	if bad := collisionIn(r.AbsPath, name); bad != nil {
		return bad.WithRepo(r.RelPath).
			WithRemedy("delete the branch that is in the way, or use a different task")
	}
	storePath := filepath.Join(c.StoreDir(), r.StoreID+".git")
	if _, err := os.Stat(storePath); err == nil {
		if gitx.RunOK(storePath, "rev-parse", "--verify", "refs/heads/"+name) {
			return wkterr.New("WKT_BRANCH_EXISTS", "a branch of that name already exists in the store").
				WithRepo(r.RelPath).WithRemedy("wkt doctor lists what the container is holding")
		}
		if bad := collisionIn(storePath, name); bad != nil {
			return bad.WithRepo(r.RelPath).WithPath(storePath).
				WithRemedy("a store left by an earlier task still holds a branch in the way",
					"wkt doctor lists what the container is holding")
		}
	}
	if _, err := os.Lstat(r.WorktreePath); err == nil && !pathIsBackFill {
		return wkterr.New("WKT_TREE_EXISTS", "something already occupies that path in the tree").
			WithPath(r.WorktreePath)
	}
	return nil
}
