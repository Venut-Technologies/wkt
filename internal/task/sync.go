package task

import (
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/store"
)

// SyncReport is what one repository of the set looks like against its upstream
// after a fetch.
type SyncReport struct {
	Repo    string
	Drifted bool
	Behind  int    // commits the task's base is behind the upstream tip
	Tip     string // the upstream tip, for the message
	// Unreachable is set when the repository has an origin that could not be
	// consulted. It is not the same as having no origin: "I looked and found
	// nothing new" and "I could not look" are different answers, and only one
	// of them justifies saying "up to date".
	Unreachable string
}

// Sync fetches in every store of the set and reports how far each repository's
// base has drifted. It deliberately does **not** advance the base (spec §6).
//
// The base is what every branch in the task was cut from, and what makes the
// set coherent: moving it under a task that is half-finished would change what
// the work is based on without anybody deciding to. Reporting is the useful
// half; deciding belongs to the person.
func Sync(c container.C, name string) ([]SyncReport, error) {
	t, err := state.Load(c.StateDir(), name)
	if err != nil {
		return nil, err
	}

	var out []SyncReport
	for _, r := range t.Repos {
		storePath := filepath.Join(c.StoreDir(), r.StoreID+".git")
		// Both remotes: origin is the real upstream, workspace is the
		// developer's own repository, which may hold commits they have not
		// pushed. A task can be behind either.
		unreachable := ""
		if url, err := gitx.Run(storePath, "config", "--get", "remote.origin.url"); err == nil && strings.TrimSpace(url) != "" {
			if _, err := gitx.Run(storePath, "fetch", "--quiet", "origin"); err != nil {
				// Discarding this is how a store that has never once reached
				// its upstream reported "up to date": the answer sync must not
				// give when it did not manage to look.
				// git's own words, not wkt's wrapper: err.Error() renders as
				// "WKT_GIT_FAILED: git fetch failed (fatal: ...)", leading with
				// a code the reader already knows and burying the reason.
				unreachable = gitReason(err)
			}
		}
		if err := store.FetchWorkspace(storePath); err != nil {
			return nil, err
		}

		tip, behind := furthestAhead(storePath, r)
		if tip == "" {
			// Nothing to compare against yet — a repository with no origin
			// and no local movement. Not drift, and not an error.
			out = append(out, SyncReport{Repo: r.RelPath, Unreachable: unreachable})
			continue
		}
		out = append(out, SyncReport{
			Repo:        r.RelPath,
			Drifted:     behind > 0,
			Behind:      behind,
			Tip:         tip,
			Unreachable: unreachable,
		})
	}
	return out, nil
}

// furthestAhead answers "how far has this repository moved since the task was
// cut", across both remotes the store carries.
//
// Checking only origin misses the case that matters most day to day: the
// developer commits locally and has not pushed. Those commits reach the store
// through the workspace remote (spec §5.2), and a sync that called that "up to
// date" would be wrong exactly when someone is working. The answer is whichever
// remote is further ahead.
func furthestAhead(storePath string, r state.Repo) (tip string, behind int) {
	var candidates []string
	if ref := strings.TrimPrefix(r.BaseRef, "refs/remotes/"); ref != r.BaseRef {
		candidates = append(candidates, "refs/remotes/"+ref)
	}
	if ref := strings.TrimPrefix(r.BaseRef, "refs/heads/"); ref != r.BaseRef {
		candidates = append(candidates, "refs/remotes/origin/"+ref)
	}
	candidates = append(candidates, "refs/remotes/origin/HEAD")
	// Every branch the workspace remote carries. Enumerating beats naming one:
	// the base may have come from origin/HEAD, from a differently named local
	// branch, or from a detached HEAD, and in each case the developer's
	// unpushed work is somewhere under refs/remotes/ws/.
	if out, err := gitx.Run(storePath, "for-each-ref", "--format=%(refname)", "refs/remotes/ws/"); err == nil {
		for _, ref := range strings.Split(strings.TrimSpace(out), "\n") {
			if ref != "" {
				candidates = append(candidates, ref)
			}
		}
	}

	for _, ref := range candidates {
		sha, err := gitx.Run(storePath, "rev-parse", "--verify", "--quiet", ref)
		if err != nil || sha == "" {
			continue
		}
		if n := countBetween(storePath, r.BaseSHA, sha); n > behind || tip == "" {
			tip, behind = sha, n
		}
	}
	return tip, behind
}

// countBetween is how many commits tip has that base does not.
func countBetween(storePath, base, tip string) int {
	if base == "" || tip == "" || base == tip {
		return 0
	}
	out, err := gitx.Run(storePath, "rev-list", "--count", base+".."+tip)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0
	}
	return n
}
