// Package doctor reconciles what wkt's state says against what is actually on
// disk and in the user's repositories.
//
// It is also the uninstall path. A tool that writes into someone else's
// repositories — wkt writes exactly one thing, a base pin under refs/wkt/ —
// has to be able to answer "what have you put in mine" completely, or trying
// it is a one-way door. Everything wkt has written is listed, whether or not
// it is a problem.
package doctor

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/store"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Finding is one thing doctor noticed. Info entries are reported and never
// "fixed": they describe wkt working as intended.
type Finding struct {
	Code   string
	Path   string
	Detail string
	Info   bool
	Fixed  bool
}

// Run reconciles the container. With fix, it repairs only what is
// unambiguous — an empty leftover directory, a pin for a task that does not
// exist — and never anything that could hold work. Removing a tree with
// content in it is rm's job, with rm's refusals.
func Run(c container.C, fix bool) ([]Finding, error) {
	names, err := state.List(c.StateDir())
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, n := range names {
		known[n] = true
	}

	var out []Finding
	out = append(out, checkTasks(c, names)...)
	orphans, err := checkTrees(c, known, fix)
	if err != nil {
		return nil, err
	}
	out = append(out, orphans...)
	out = append(out, checkStores(c)...)
	refs, err := checkWorkspaceRefs(c, names, known, fix)
	if err != nil {
		return nil, err
	}
	out = append(out, refs...)

	sort.SliceStable(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

// checkTasks: state claims a task; does its tree exist?
func checkTasks(c container.C, names []string) []Finding {
	var out []Finding
	for _, n := range names {
		tree := c.TreePath(n)
		if _, err := os.Stat(tree); os.IsNotExist(err) {
			out = append(out, Finding{
				Code: "WKT_MISSING_TREE", Path: tree,
				Detail: "task " + n + " has state but no tree; wkt rm " + n + " --force finishes the removal",
			})
		}
	}
	return out
}

// checkTrees: the disk holds a tree; does any task claim it?
//
// This is the debris an interrupted create leaves behind, and it is not
// harmless: the directory blocks that task name forever, and the error a user
// gets is WKT_TREE_EXISTS with no obvious way out.
func checkTrees(c container.C, known map[string]bool, fix bool) ([]Finding, error) {
	entries, err := os.ReadDir(c.TreesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wkterr.New("WKT_CHECK_FAILED", "cannot read the trees directory").
			WithPath(c.TreesDir()).WithFound(err.Error())
	}
	var out []Finding
	for _, e := range entries {
		if known[e.Name()] {
			continue
		}
		p := filepath.Join(c.TreesDir(), e.Name())
		// Never follow a link out of the container (spec H3). A symlink in
		// trees/ is reported and left alone: deleting it is safe, deleting
		// *through* it is not, and the distinction is not worth risking here.
		if e.Type()&os.ModeSymlink != 0 {
			out = append(out, Finding{
				Code: "WKT_ORPHAN_TREE", Path: p,
				Detail: "a symlink in trees/ that no task claims; remove it by hand once you know what it points at",
			})
			continue
		}
		f := Finding{
			Code: "WKT_ORPHAN_TREE", Path: p,
			Detail: "no task claims this directory; it blocks a task of the same name",
		}
		if fix {
			// os.Remove, not RemoveAll: it succeeds on an empty directory and
			// fails on one with anything in it, which is exactly the line
			// this must not cross.
			if err := os.Remove(p); err == nil {
				f.Fixed = true
				f.Detail = "removed an empty leftover directory"
			} else {
				f.Detail = "not empty, so wkt did not touch it; inspect it, then remove it yourself"
			}
		}
		out = append(out, f)
	}
	return out, nil
}

// checkWorkspaceRefs lists every refs/wkt/* wkt has written into the user's
// own repositories, and flags the ones whose task is gone. A pin held by a
// live task is doing its job: it keeps the base commit from being collected
// while a worktree still needs it.
func checkWorkspaceRefs(c container.C, names []string, known map[string]bool, fix bool) ([]Finding, error) {
	repos := map[string]bool{}
	for _, n := range names {
		t, err := state.Load(c.StateDir(), n)
		if err != nil {
			continue
		}
		for _, r := range t.Repos {
			repos[r.AbsPath] = true
		}
	}
	// A task whose state is gone leaves no record of which repositories it
	// touched, so also sweep the workspace itself.
	if entries, err := os.ReadDir(c.Workspace); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				p := filepath.Join(c.Workspace, e.Name())
				if _, statErr := os.Stat(filepath.Join(p, ".git")); statErr == nil {
					repos[p] = true
				}
			}
		}
	}

	var out []Finding
	paths := make([]string, 0, len(repos))
	for p := range repos {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	for _, repo := range paths {
		listed, err := gitx.Run(repo, "for-each-ref", "--format=%(refname)", "refs/wkt/")
		if err != nil {
			out = append(out, Finding{
				Code: "WKT_CHECK_FAILED", Path: repo,
				Detail: "cannot list refs/wkt/ in this repository",
			})
			continue
		}
		for _, ref := range strings.Split(strings.TrimSpace(listed), "\n") {
			if ref == "" {
				continue
			}
			taskName := strings.TrimPrefix(ref, "refs/wkt/base/")
			if known[taskName] {
				out = append(out, Finding{
					Code: "WKT_WORKSPACE_REF", Path: repo, Info: true,
					Detail: ref + " — held by task " + taskName,
				})
				continue
			}
			f := Finding{
				Code: "WKT_STRAY_PIN", Path: repo,
				Detail: ref + " — no task owns it; it pins objects that git would otherwise collect",
			}
			if fix {
				if _, err := gitx.Run(repo, "update-ref", "-d", ref); err == nil {
					f.Fixed = true
					f.Detail = "deleted " + ref + ", which no task owned"
				}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

// checkStores reports a store that did not finish being built.
//
// This is the state where everything looks healthy and is not: the store still
// borrows objects from the developer's own repository, so the task's commits
// become unreadable the moment that repository is re-cloned or collected, and
// its hooks are live. doctor reports it and stops — the store may be the only
// copy of a task's unpushed commits, so repairing it by rebuilding would turn
// a recoverable problem into a lost one.
func checkStores(c container.C) []Finding {
	entries, err := os.ReadDir(c.StoreDir())
	if err != nil {
		return nil
	}
	var out []Finding
	for _, e := range entries {
		if !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") {
			continue
		}
		sp := filepath.Join(c.StoreDir(), e.Name())
		if v, err := gitx.Run(sp, "config", "--get", store.MarkerKey); err == nil && strings.TrimSpace(v) != "" {
			continue
		}
		// The same question the store asks itself, asked the same way. This
		// used to be a second copy of the checks, and the copies had already
		// diverged: a store whose origin still pointed at the developer's
		// clone was refused by wkt new and called healthy here — by the one
		// command the refusal tells people to run.
		//
		// The workspace repository comes from the store's own workspace
		// remote, which is the only record of where it was built from.
		repoAbs := ""
		if v, err := gitx.Run(sp, "config", "--get", "remote.workspace.url"); err == nil {
			repoAbs = strings.TrimSpace(v)
		}
		missing := store.Invariants(sp, repoAbs)
		if len(missing) == 0 {
			continue // complete, merely unmarked: an earlier version built it
		}
		out = append(out, Finding{
			Code: "WKT_STORE_INCOMPLETE", Path: sp,
			Detail: "unfinished store: " + strings.Join(missing, "; ") +
				" — wkt will not rebuild it, because it may hold the only copy of a task's unpushed commits",
		})
	}
	return out
}
