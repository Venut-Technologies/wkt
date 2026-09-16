package task

import (
	"errors"
	"path/filepath"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// FetchResult is one repository's outcome.
type FetchResult struct {
	Repo    string
	Updated bool
	Ref     string
	SHA     string
}

// Fetch brings each repository's task branch back into the workspace
// repository the developer actually works in.
//
// Fast-forward only, and never a forcing refspec (spec §6). The branch in
// their repository is theirs: if it has moved somewhere this fetch cannot
// reach, the answer is to say so — naming both commits — and offer another
// name through as, not to overwrite it and mention it afterwards.
func Fetch(c container.C, name, as string) ([]FetchResult, error) {
	t, err := state.Load(c.StateDir(), name)
	if err != nil {
		return nil, err
	}
	target := name
	if as != "" {
		target = as
	}

	// Check the whole set before moving any ref: a fetch that updates two
	// repositories and then refuses on the third leaves the developer holding
	// half of a task.
	type plan struct {
		repo state.Repo
		sha  string
		skip bool
	}
	plans := make([]plan, 0, len(t.Repos))
	for _, r := range t.Repos {
		storePath := filepath.Join(c.StoreDir(), r.StoreID+".git")
		// A branch that cannot coexist with the target is invisible to the
		// lookup below: rev-parse of "feat" reports nothing when "feat/42"
		// holds the path. Without this the plan was made, the first
		// repository's ref moved, and the set failed on a later one — the
		// half-a-task outcome this whole loop exists to prevent.
		if b := dfConflict(r.AbsPath, target); b != "" {
			return nil, wkterr.New("WKT_BRANCH_DF_CONFLICT",
				"the workspace repository holds a branch that cannot coexist with this name").
				WithRepo(r.RelPath).WithPath(r.AbsPath).WithFound(b).
				WithRemedy("wkt fetch "+name+" --as <name> brings it in under another name",
					"or delete "+b+" in that repository")
		}
		sha, err := gitx.Run(storePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+name)
		if err != nil || sha == "" {
			// The task has no branch here yet — nothing was committed in this
			// repository. Not an error.
			plans = append(plans, plan{repo: r, skip: true})
			continue
		}
		existing, err := gitx.Run(r.AbsPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+target)
		if err == nil && existing != "" {
			if existing == sha {
				plans = append(plans, plan{repo: r, sha: sha, skip: true})
				continue
			}
			// Fast-forward means the workspace's commit is an ancestor of the
			// task's. Anything else would need --force, which this command
			// does not have and will not grow.
			//
			// Asked in the store, not in the workspace repository: the task's
			// commit does not exist there, so merge-base failed on an unknown
			// object and every second fetch — an ordinary fast-forward after
			// more work — was refused as a divergence. The store is the one
			// place that can answer, and its answer is complete: it holds
			// everything reachable from the task's branch, so a commit it does
			// not have cannot be an ancestor of one it does.
			if !gitx.RunOK(storePath, "merge-base", "--is-ancestor", existing, sha) {
				return nil, wkterr.New("WKT_NOT_FAST_FORWARD",
					"the branch in the workspace is not an ancestor of the task's").
					WithRepo(r.RelPath).
					WithExpected("workspace "+target+" at "+short(existing)).
					WithFound("task "+name+" at "+short(sha)).
					WithRemedy("wkt fetch "+name+" --as <name> brings it in under another name",
						"or merge or rebase the two yourself; wkt will not force a ref you own")
			}
		}
		plans = append(plans, plan{repo: r, sha: sha})
	}

	var out []FetchResult
	for _, p := range plans {
		if p.skip {
			out = append(out, FetchResult{Repo: p.repo.RelPath})
			continue
		}
		storePath := filepath.Join(c.StoreDir(), p.repo.StoreID+".git")
		// Update the ref directly rather than fetching into a checked-out
		// branch: the developer may be standing on it, and a fetch that moves
		// the branch under their working copy is exactly the surprise this
		// command exists to avoid. update-ref refuses on a checked-out branch,
		// which is the behaviour we want.
		if _, err := gitx.Run(p.repo.AbsPath, "fetch", "--quiet", storePath,
			"refs/heads/"+name+":refs/heads/"+target); err != nil {
			// git's own words, not a guess at them: the remedy below is the
			// common cause, and printing it alone told everyone who hit any
			// other cause to do something irrelevant.
			return out, wkterr.New("WKT_FETCH_FAILED", "cannot bring the branch into the workspace repository").
				WithRepo(p.repo.RelPath).WithPath(p.repo.AbsPath).
				WithFound(gitReason(err)).
				WithRemedy("if the branch is checked out there, switch away from it first")
		}
		out = append(out, FetchResult{
			Repo: p.repo.RelPath, Updated: true,
			Ref: "refs/heads/" + target, SHA: p.sha,
		})
	}
	return out, nil
}

// gitReason recovers git's explanation from a wrapped gitx error, so a
// refusal wkt cannot classify still tells the developer what git said.
func gitReason(err error) string {
	var e *wkterr.E
	if errors.As(err, &e) && e.Found != "" {
		return e.Found
	}
	return err.Error()
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
