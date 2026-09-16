// Refuse-only teardown: Remove never deletes anything until Preflight finds
// no blocker (or the caller passes --force for the ordinary ones), and even
// then a foreign repository or a submodule is never removable. Anything that
// deletes enumerates the filesystem, never the state file — state says what
// should be there, the disk says what is there.
package task

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/Venut-Technologies/wkt/internal/artifact"
	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/perimeter"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/tree"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Severity distinguishes a blocker that gates removal from one that is
// merely reported. The zero value ("") means blocking, so every existing
// code that never sets Severity keeps its original meaning unchanged.
type Blocker struct {
	Code     string
	Repo     string
	Path     string
	Detail   string
	Severity string // "" (blocking, default) or "info" (listed, not blocking)
}

// regenerable lists path-component sequences that mark git-ignored content
// as safe to delete without a warning: build output and dependency caches
// any build system regenerates on demand. This is deliberately an
// allowlist, not a denylist. A denylist of "precious" filename substrings
// was tried first and failed the one property that matters here: it cannot
// be made complete. A gitignored "server.key" — an entirely ordinary TLS or
// SSH private key name — matched none of its five entries and was deleted
// with zero blockers and no --force. Now unknown ignored content blocks by
// default; only what's provably regenerable is exempt, and even that is
// still reported, not silently passed over (see the "info" Severity below).

// isRegenerable reports whether relPath — git's own slash-separated,
// repo-relative reporting of an ignored path — contains one of the
// regenerable sequences as whole path components. It never matches a bare
// substring: "target" matches ".../target/..." but not
// ".../my-target-cache/...", and a name like "server.key" never matches
// anything here at all, which is the point.

// Preflight enumerates every reason removing t's tree would lose work. It
// walks the real filesystem under the tree root — never the state file — so
// content a task's state knows nothing about (a foreign repository dropped
// into a worktree, a link slot an agent turned into a real file) is still
// found. It never descends into a symlink: those are workspace back-fill
// slots, not part of the tree wkt owns.
// ownedByWkt returns the perimeter copies wkt wrote into this tree, keyed by
// their path relative to the tree root. They are wkt's own files, so neither
// the per-repository status check nor the tree walk may report them as the
// user's uncommitted work — a tool that refuses to clean up after itself is
// worse than one that never wrote the file.
func ownedByWkt(c container.C, t state.Task) map[string]bool {
	treeRoot := c.TreePath(t.Name)
	owned := map[string]bool{}
	for _, dir := range t.PerimeterCoverage {
		f := filepath.Join(dir, ".claude", "settings.json")
		if rel, err := filepath.Rel(treeRoot, f); err == nil {
			owned[filepath.Clean(rel)] = true
		}
	}
	return owned
}

func Preflight(c container.C, t state.Task) ([]Blocker, error) {
	perimeterOwned := ownedByWkt(c, t)
	// Files wkt carried into the tree, by their tree-relative path.
	carried := map[string]string{}
	// Content the post-create script brought into being, by the same
	// tree-relative path. No hash: the carry records one because it must tell
	// an untouched copy from one the task edited, and produced content is
	// disposable either way, so there is nothing for a hash to decide.
	produced := map[string]bool{}
	for _, l := range t.Links {
		switch l.Type {
		case "carry":
			carried[filepath.Clean(filepath.FromSlash(l.RelPath))] = l.Hash
		case "produced":
			produced[filepath.Clean(filepath.FromSlash(l.RelPath))] = true
		}
	}
	var out []Blocker
	treeRoot := c.TreePath(t.Name)

	for _, r := range t.Repos {
		wt := r.WorktreePath
		if _, err := os.Stat(wt); os.IsNotExist(err) {
			out = append(out, Blocker{Code: "WKT_WORKTREE_MISSING", Repo: r.RelPath, Path: wt})
			continue
		}
		// 1+2: uncommitted and untracked. -uall so an untracked directory is
		// listed file by file rather than collapsing to "?? .claude/": the
		// perimeter copy wkt itself wrote lives in there, and collapsing hides
		// whether anything else does.
		if s, err := gitx.Run(wt, "status", "--porcelain", "-uall"); err != nil {
			out = append(out, Blocker{Code: "WKT_CHECK_FAILED", Repo: r.RelPath, Path: wt})
		} else if s = dropOwned(s, wt, c.TreePath(t.Name), perimeterOwned); s != "" {
			out = append(out, Blocker{Code: "WKT_DIRTY", Repo: r.RelPath, Path: wt, Detail: describePorcelain(s)})
		}
		// 3: ignored content. git's own refusal never fires on any of it (H1).
		// A bulk-ignored directory collapses to one "!! dir/" line — its
		// contents are never listed individually — so an allowlisted
		// directory is trusted whole, and an unrecognised one blocks whole:
		// unknown means precious, not just unknown-named files. A failed
		// check is not "no ignored content" (spec §5.7: "a failed check of
		// any kind ... is treated as 'would lose work'") — the first check
		// above already got this right; this one and the three below it
		// used to fail open, each on its own "err == nil" with no else.
		if s, err := gitx.Run(wt, "status", "--porcelain", "--ignored=matching"); err == nil {
			for _, line := range strings.Split(s, "\n") {
				if !strings.HasPrefix(line, "!! ") {
					continue
				}
				rel := strings.TrimPrefix(line, "!! ")
				if artifact.IsRegenerable(rel) {
					out = append(out, Blocker{Code: "WKT_REGENERABLE_IGNORED", Repo: r.RelPath, Path: rel, Severity: "info"})
					continue
				}
				// A file wkt carried in, still identical to the developer's
				// own copy, loses nothing when the tree goes. Blocking on it
				// would make --force reflexive for every task that carries a
				// .env — which is the habit this whole check exists to avoid.
				// Once the task has edited it, it holds something their file
				// does not, and section 8's copy check blocks on it.
				if carriedUnchanged(carried, filepath.Join(r.RelPath, rel), filepath.Join(wt, rel)) {
					out = append(out, Blocker{Code: "WKT_CARRIED", Repo: r.RelPath, Path: rel, Severity: "info"})
					continue
				}
				// Content the post-create script made. It is generated, not
				// the developer's, and blocking on it would make --force the
				// habit for every task with a setup step — the habit this
				// whole check exists to prevent. Still reported: passing over
				// it in silence would hide the one case where the script
				// wrote something that mattered.
				if producedExempt(produced, filepath.Join(r.RelPath, rel), rel) {
					out = append(out, Blocker{Code: "WKT_PRODUCED", Repo: r.RelPath, Path: rel, Severity: "info"})
					continue
				}
				out = append(out, Blocker{Code: "WKT_PRECIOUS_IGNORED", Repo: r.RelPath, Path: rel})
			}
		} else {
			out = append(out, Blocker{Code: "WKT_CHECK_FAILED", Repo: r.RelPath, Path: wt, Detail: "ignored-content check"})
		}
		// 4: in-progress operations — invisible to status --porcelain (H2):
		// empty during an interactive rebase pause, a bisect, or a detached
		// HEAD, so removal must not be gated on status alone.
		for _, marker := range []string{"rebase-merge", "rebase-apply", "MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "BISECT_LOG"} {
			p, err := gitx.Run(wt, "rev-parse", "--git-path", marker)
			if err != nil {
				out = append(out, Blocker{Code: "WKT_CHECK_FAILED", Repo: r.RelPath, Path: wt, Detail: "in-progress check (" + marker + ")"})
				continue
			}
			// --git-path may answer relatively or absolutely; --path-format needs
			// git 2.31 and our floor is 2.29, so normalise by hand.
			if !filepath.IsAbs(p) {
				p = filepath.Join(wt, p)
			}
			if _, statErr := os.Stat(p); statErr == nil {
				out = append(out, Blocker{Code: "WKT_IN_PROGRESS", Repo: r.RelPath, Detail: marker})
			}
		}
		// 5: unpushed commits, including the no-upstream and detached cases. The
		// recorded base is excluded as well as every remote ref: a bare store has
		// no refs/remotes/* until something fetches, so counting against remotes
		// alone would flag the whole history of a freshly created task.
		args := []string{"rev-list", "--count", "HEAD", "--not", "--remotes"}
		if r.BaseSHA != "" {
			args = append(args, r.BaseSHA)
		}
		if n, err := gitx.Run(wt, args...); err == nil {
			if n != "" && n != "0" {
				out = append(out, Blocker{Code: "WKT_UNPUSHED", Repo: r.RelPath, Detail: plural(n, "commit")})
			}
		} else {
			out = append(out, Blocker{Code: "WKT_CHECK_FAILED", Repo: r.RelPath, Path: wt, Detail: "unpushed-commit check"})
		}
		// 6: submodules — worktree remove refuses unconditionally, and --force
		// destroys their object store (spec §5.7). Run from the worktree, not the
		// store, since submodule state is per-worktree (index-based) and this
		// fires even when the addition is fully committed and status is clean.
		if sm, err := gitx.Run(wt, "submodule", "status", "--recursive"); err == nil {
			if strings.TrimSpace(sm) != "" {
				out = append(out, Blocker{Code: "WKT_SUBMODULE", Repo: r.RelPath, Detail: "submodule " + submodulePath(sm)})
			}
		} else {
			out = append(out, Blocker{Code: "WKT_CHECK_FAILED", Repo: r.RelPath, Path: wt, Detail: "submodule check"})
		}
	}

	// 7: a foreign .git anywhere in the tree, found without following symlinks.
	known := map[string]bool{}
	for _, r := range t.Repos {
		canon, _ := paths.Canonical(r.WorktreePath)
		known[canon] = true
	}

	// 7b: untracked tree-root content (review Critical 1). Preflight's other
	// checks all scope to a repository's WorktreePath, so content living at
	// the tree root itself — a cross-repo plan, a generated report, agent
	// scratch — was invisible to every check and simply vanished into
	// os.RemoveAll(staged). Content *inside* a worktree is already fully
	// covered by "git status --porcelain" above, so this only needs to
	// classify entries that are not a descendant of a recorded worktree:
	// every such entry must be the worktree's own root, a recorded link
	// slot (symlink or copy), or a pure ancestor directory on the path to
	// one of those — tree.Materialise already recorded every sibling of an
	// ancestor as a link slot or copy at create time, so anything left over
	// is exactly what an agent added afterward.
	worktreeRel := map[string]bool{}
	for _, r := range t.Repos {
		worktreeRel[filepath.FromSlash(r.RelPath)] = true
	}
	linkRel := map[string]bool{}
	copyHash := map[string]string{}
	for _, slot := range t.Links {
		// A "produced" entry is a record of what the post-create script made,
		// not a slot the tree is built from. Counting it here would let the
		// link-slot case in the walk swallow it silently, which is the one
		// thing this check must not do: unrecognised content is reported
		// either way, the only question is whether it blocks.
		if slot.Type == "produced" {
			continue
		}
		rel := filepath.FromSlash(slot.RelPath)
		linkRel[rel] = true
		if slot.Type == "copy" {
			copyHash[rel] = slot.Hash
		}
	}
	sep := string(filepath.Separator)
	underWorktree := func(rel string) bool {
		for wt := range worktreeRel {
			if strings.HasPrefix(rel, wt+sep) {
				return true
			}
		}
		return false
	}
	ancestorOfSomething := func(rel string) bool {
		prefix := rel + sep
		for wt := range worktreeRel {
			if strings.HasPrefix(wt, prefix) {
				return true
			}
		}
		for ls := range linkRel {
			if strings.HasPrefix(ls, prefix) {
				return true
			}
		}
		return false
	}

	// Directories already reported as untracked content, so their contents are
	// walked without being reported again.
	var reportedDirs []string

	_ = filepath.WalkDir(treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p != treeRoot {
			if rel, relErr := filepath.Rel(treeRoot, p); relErr == nil {
				rel = filepath.Clean(rel)
				if !underWorktree(rel) {
					switch {
					case worktreeRel[rel]:
						// a worktree's own root: fall through so the walk still
						// descends into it below, for the foreign-.git scan.
					case linkRel[rel]:
						// a recorded link slot. A copy is not silently accepted
						// on the strength of occupying the right path alone: if
						// its hash no longer matches, it still falls through to
						// the untracked-content blocker below, as a defence in
						// depth alongside the dedicated WKT_COPY_DIVERGED check
						// (section 8).
						if wantHash, isCopy := copyHash[rel]; isCopy {
							if sum, hErr := tree.Hash(p); hErr != nil || sum != wantHash {
								b := Blocker{Code: "WKT_UNTRACKED_TREE_CONTENT", Path: p}
								if artifact.IsRegenerable(rel) {
									b = Blocker{Code: "WKT_REGENERABLE_TREE_CONTENT", Path: p, Severity: "info"}
								}
								out = append(out, b)
							}
						}
					case ancestorOfSomething(rel):
						// a real directory on the path to something recorded;
						// fall through and keep descending.
					case perimeterOwned[rel]:
						// wkt's own perimeter copy: its integrity is checked
						// by hash in status, not by refusing to remove the
						// tree that contains it.
					case d.IsDir() && ownsSomethingUnder(perimeterOwned, rel):
						// ".claude" itself, on the way to the copy above.
						// Descend rather than blocking, so a *user* file
						// beside the perimeter is still found.
					case artifact.IsRegenerable(rel):
						// Finder writes .DS_Store into every directory a
						// macOS user opens, the task tree included. Listing
						// it is right; blocking on it taught people to reach
						// for --force without reading the list, which is the
						// one thing this check cannot afford (finding L2).
						out = append(out, Blocker{Code: "WKT_REGENERABLE_TREE_CONTENT", Path: p, Severity: "info"})
					case underReported(reportedDirs, rel):
						// Already accounted for by the directory above it.
						// Descending anyway is the point: a repository created
						// inside the tree lives below untracked content, and
						// stopping here is what made it invisible to the
						// foreign-repo scan — so "wkt rm --force" deleted the
						// only copy of its history (issue #1).
					case producedExempt(produced, rel, rel):
						// The post-create script wrote this, outside every
						// repository — an intermediate directory of the
						// mirrored tree, or the tree root. git cannot answer
						// for it there, so the record is the only thing that
						// can. Reported, never blocking, like its ignored
						// counterpart inside a repository.
						out = append(out, Blocker{Code: "WKT_PRODUCED", Path: p, Severity: "info"})
					default:
						out = append(out, Blocker{Code: "WKT_UNTRACKED_TREE_CONTENT", Path: p})
						if d.IsDir() {
							// Report the directory once, then keep walking
							// into it rather than turning one problem into a
							// wall of them.
							reportedDirs = append(reportedDirs, rel)
						}
					}
				}
			}
		}
		// fs.WalkDir's SkipDir has two different meanings depending on the
		// entry it's returned for: on a directory it means "don't descend
		// into this", but on anything else it means "skip the rest of this
		// directory's *siblings*" — a completely different, much more
		// destructive effect. A symlink is never a directory as far as
		// DirEntry.IsDir() is concerned (Lstat, not Stat), and WalkDir never
		// follows symlinks on its own regardless, so SkipDir here bought
		// nothing and only tripped the sibling-skipping behaviour: whenever a
		// back-fill symlink (e.g. "docs") sorted before other tree content
		// (e.g. "services"), it silently truncated the scan of the entire
		// rest of the tree root, hiding every materialised worktree from the
		// foreign-repo check below. Confirmed against real fs.WalkDir before
		// this fix. plain "return nil" is correct and sufficient: the walk
		// already never recurses into a symlink.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if d.Name() != ".git" {
			return nil
		}
		if !d.IsDir() {
			// A regular-file .git marker is either our own materialised
			// worktree's marker (excluded below via known) or a submodule
			// checkout's marker, whose gitdir always points into the owning
			// repository's own .git/modules/<name> (git's own convention).
			// A submodule is not "a repository wkt did not create" — it is
			// nested inside one wkt did — and it already gets a precise,
			// correctly-worded blocker (WKT_SUBMODULE, with the right
			// remedy) from the per-repo check above. Flagging it here too as
			// WKT_FOREIGN_REPO would mislead a user with the wrong remedy
			// ("move it out of the tree") and, worse, made the submodule
			// hard-force-block in Remove() untestable in isolation: the
			// foreign-repo hard block always fired first regardless of
			// whether the submodule-specific guard was even present.
			// Confirmed by deliberately disabling the submodule guard and
			// finding TestRemoveRefusesOnSubmoduleEvenWithForce still passed
			// before this fix.
			if b, rerr := os.ReadFile(p); rerr == nil &&
				strings.Contains(string(b), string(filepath.Separator)+"modules"+string(filepath.Separator)) {
				return nil
			}
		}
		owner, _ := paths.Canonical(filepath.Dir(p))
		if !known[owner] {
			out = append(out, Blocker{Code: "WKT_FOREIGN_REPO", Path: filepath.Dir(p)})
		}
		// The same asymmetry applies here: a linked worktree's own ".git"
		// marker is a regular *file* (every materialised worktree has
		// exactly this), so SkipDir returned unconditionally on it also hit
		// the sibling-skipping case and stopped scanning the remainder of
		// the worktree right after its own marker was visited — a foreign
		// repository nested anywhere sorting after ".git" (almost anything)
		// was never found. Only an actual .git *directory* (an ordinary
		// nested repository, foreign or otherwise) should stop descent.
		// Confirmed against real fs.WalkDir before this fix; see
		// remove_test.go's
		// TestRemoveDetectsForeignRepoNestedInsideMaterialisedWorktree.
		if d.IsDir() {
			return fs.SkipDir
		}
		return nil
	})

	// 8: link slots whose type changed, and copies that diverged (H12, §5.3 r5).
	for _, slot := range t.Links {
		// Produced content is not a slot: wkt never built it and does not
		// require it to still be there. Someone deleting what a setup script
		// made is housekeeping, not damage.
		if slot.Type == "produced" {
			continue
		}
		p := filepath.Join(treeRoot, slot.RelPath)
		info, err := os.Lstat(p)
		if err != nil {
			out = append(out, Blocker{Code: "WKT_LINK_SLOT_MISSING", Path: slot.RelPath})
			continue
		}
		switch slot.Type {
		case "symlink":
			if info.Mode()&os.ModeSymlink == 0 {
				out = append(out, Blocker{Code: "WKT_LINK_SLOT_CHANGED", Path: slot.RelPath,
					Detail: "the link was replaced by a regular file"})
			}
		case "copy":
			if sum, err := tree.Hash(p); err != nil || sum != slot.Hash {
				// A slot recorded by an older wkt may still name an OS
				// artifact; today's tree never copies one. Either way it is
				// not work at risk.
				if artifact.IsRegenerable(slot.RelPath) {
					out = append(out, Blocker{Code: "WKT_REGENERABLE_TREE_CONTENT", Path: slot.RelPath, Severity: "info"})
					continue
				}
				out = append(out, Blocker{Code: "WKT_COPY_DIVERGED", Path: slot.RelPath})
			}
		}
	}
	return out, nil
}

func firstLine(s string) string { return strings.SplitN(strings.TrimSpace(s), "\n", 2)[0] }

// Remove refuses while any blocker exists. With force it overrides the
// ordinary ones, but never a foreign repository (its history exists nowhere
// else) and never a submodule (its object store lives under the doomed
// worktree; --force would destroy objects reachable from neither the store
// nor the original submodule repository) — spec §5.7.
//
// The fence: one os.Rename moves the whole tree into staging/ first, making
// a still-running agent's cwd vanish atomically, before anything is deleted.
// Deletion goes through os.RemoveAll on a path this function computed —
// never a shell command, never a symlink-following walker (spec H3): "rm -rf
// link/" with a trailing slash destroys the symlink's target, not the link.
// Kept is a branch tip parked in the store because it held work no remote
// has. wkt rm prints these, so "recover it with git log <ref>" is something
// the user is told rather than something they have to know.
type Kept struct {
	Repo  string
	Store string
	Ref   string
	SHA   string
}

func Remove(c container.C, name string, force bool) ([]Kept, error) {
	t, err := state.Load(c.StateDir(), name)
	if err != nil {
		return nil, err
	}

	treeRoot := c.TreePath(name)
	staged := filepath.Join(c.StagingDir(), name)

	// A tree that is simply gone cannot be preflighted — there is nothing
	// left on disk for a blocker to name — and the old behaviour tried
	// anyway: Preflight emitted a blocking WKT_WORKTREE_MISSING per
	// repository, so plain "rm" refused, and "--force" then died at
	// os.Rename(treeRoot, staged) with ENOENT, reported as WKT_STAGING with
	// a remedy about filesystems that had nothing to do with the cause.
	// With no "doctor" in this plan, that left the task permanently
	// unremovable: the state file, the base pin in the workspace repository
	// and the store branch all survived forever, and the name could never
	// be reused. Skip the fence entirely and go straight to store and state
	// cleanup — finishRemove's own os.RemoveAll(staged) is a no-op when
	// staging/<name> does not exist, and resumes an incomplete delete
	// (test/05_staging_fence.sh deliberately produces exactly that state)
	// when it does: that deletion was already authorised by the --force
	// that performed the original move, so it does not need re-authorising.
	if _, statErr := os.Stat(treeRoot); os.IsNotExist(statErr) {
		return finishRemove(c, t, name, staged)
	}

	all, err := Preflight(c, t)
	if err != nil {
		return nil, err
	}
	// Severity "info" entries (regenerable ignored content) are reported but
	// never gate removal or the force decision — only the blocking ones do.
	var blocking []Blocker
	for _, b := range all {
		if b.Severity != "info" {
			blocking = append(blocking, b)
		}
	}
	// Every foreign repository at once, not the first of them. They are all
	// found already; reporting one meant moving it out, retrying, and meeting
	// the next — once per repository (issue #5). WKT_WOULD_LOSE_WORK below
	// answers the same kind of question with problems[], and there was no
	// reason for these two to differ.
	var foreign []Blocker
	for _, b := range blocking {
		if b.Code == "WKT_FOREIGN_REPO" {
			foreign = append(foreign, b)
		}
	}
	if len(foreign) > 0 {
		// Unconditional, --force included: a repository created inside the
		// tree has no store behind it, so its history exists nowhere else
		// and nothing can bring it back (spec §5.7, issue #1). This is the
		// one refusal --force does not cover, which is what keeps --force
		// safe to reach for at all.
		e := wkterr.New("WKT_FOREIGN_REPO", "a repository wkt did not create lives inside the tree")
		if len(foreign) > 1 {
			e = wkterr.New("WKT_FOREIGN_REPO", "repositories wkt did not create live inside the tree")
		}
		for _, b := range foreign {
			e = e.WithProblem(wkterr.Problem{Code: b.Code, Repo: b.Repo, Path: b.Path})
		}
		return nil, e.WithPath(foreign[0].Path).
			WithRemedy("move them out of the tree, or push them somewhere first",
				"then retry the removal")
	}
	for _, b := range blocking {
		if b.Code == "WKT_SUBMODULE" && force {
			return nil, wkterr.New(b.Code, "a submodule is present; --force would destroy its objects").
				WithRepo(b.Repo).WithRemedy("push the submodule's commits", "git submodule deinit", "then retry")
		}
	}
	if len(blocking) > 0 && !force {
		e := wkterr.New("WKT_WOULD_LOSE_WORK", "removal would lose work")
		// List every entry, blocking and informational alike, so --force
		// doesn't become reflexive: the user sees that most of what's in the
		// way is build output, and one line is their .env (spec §5.7). They
		// go in Problems; Remedy is reserved for what to actually do.
		for _, b := range all {
			e = e.WithProblem(wkterr.Problem{
				Code: b.Code, Repo: b.Repo, Path: b.Path,
				Detail: b.Detail, Info: b.Severity == "info",
			})
		}
		return nil, e.WithRemedy(remedyFor(blocking, name)...)
	}

	if err := os.MkdirAll(c.StagingDir(), 0o700); err != nil {
		return nil, wkterr.New("WKT_STAGING", "cannot create the staging directory").WithPath(c.StagingDir())
	}
	if err := os.Rename(treeRoot, staged); err != nil {
		// Degrading the fence to a per-repo, non-atomic sequence when
		// staging/ is on another filesystem would defeat the reason the
		// fence exists (a still-running agent's cwd must vanish atomically),
		// so this refuses rather than falling back. Name the cause when it's
		// the well-known one (EXDEV) instead of only echoing the raw OS
		// error, so the remedy is actionable on first read.
		msg := "cannot move the tree into staging"
		remedy := "relocate the container so its trees/ and staging/ share one filesystem, then retry"
		if errors.Is(err, syscall.EXDEV) {
			msg = "the tree and staging/ are on different filesystems, so the removal fence cannot be atomic"
		}
		return nil, wkterr.New("WKT_STAGING", msg).
			WithPath(treeRoot).WithFound(err.Error()).
			WithRemedy(remedy)
	}

	return finishRemove(c, t, name, staged)
}

// refreshAfterRemoval regenerates every surviving task's perimeter so it stops
// naming a tree that no longer exists. Create already does this when a tree
// appears; without the mirror image, every removal leaves the survivors stale
// and status reporting drift nobody caused.
//
// Failures are swallowed for the same reason as at create time: the removal
// succeeded, and a survivor that could not be refreshed is stale, not broken.
func refreshAfterRemoval(c container.C) {
	names, err := state.List(c.StateDir())
	if err != nil {
		return
	}
	for _, n := range names {
		other, err := state.Load(c.StateDir(), n)
		if err != nil {
			continue
		}
		coverage, hashes, err := perimeter.Write(c, other, names)
		if err != nil {
			continue
		}
		other.PerimeterCoverage, other.PerimeterHashes = coverage, hashes
		_ = state.Save(c.StateDir(), other)
	}
}

// finishRemove does the git-side cleanup (unlock, prune, delete the task
// branch, delete the base pin from the workspace repository) and removes
// the task's state, for every path that reaches it: a normal removal just
// past the staging fence, and Remove's missing-tree shortcut above (where
// staged may or may not exist — os.RemoveAll is a no-op on a path that
// isn't there, so this same call resumes an interrupted delete without a
// separate branch for that case).
func finishRemove(c container.C, t state.Task, name, staged string) ([]Kept, error) {
	var kept []Kept
	for _, r := range t.Repos {
		sp := filepath.Join(c.StoreDir(), r.StoreID+".git")
		_, _ = gitx.Run(sp, "worktree", "unlock", r.WorktreePath)
		_, _ = gitx.Run(sp, "worktree", "prune")
		// The branch a task's worktree was created on (spec §5.4) is never
		// removed by "worktree prune" — confirmed against real git: after
		// unlock+prune the admin entry is gone but "branch --list" still shows
		// it. Left behind, it makes the store un-reusable: a later Create of a
		// task with the same name fails Validate's WKT_BRANCH_EXISTS check
		// against the store even though the task was cleanly removed. See
		// remove_test.go's TestRemoveCleansUpStoreBranchSoTaskNameCanBeReused.
		// Before dropping the branch, keep whatever it held that exists
		// nowhere else. After a forced removal the objects survive in the
		// store but nothing points at them, so recovering means knowing a
		// store exists, finding the right *.git and digging for a dangling
		// commit — which reads as data loss at the moment the tool is trusted
		// least (issue #2). A ref costs nothing and makes it an ordinary
		// "git log".
		if k, ok := keepUnpushed(sp, r, name); ok {
			kept = append(kept, k)
		}
		_, _ = gitx.Run(sp, "branch", "-D", r.Branch)
		_, _ = gitx.Run(r.AbsPath, "update-ref", "-d", r.BasePinRef)
	}
	if err := os.RemoveAll(staged); err != nil {
		return nil, wkterr.New("WKT_REMOVE_FAILED", "cannot remove the staged tree").WithPath(staged)
	}
	if err := os.Remove(filepath.Join(c.StateDir(), name+".json")); err != nil && !os.IsNotExist(err) {
		return nil, wkterr.New("WKT_STATE_WRITE", "cannot remove task state").WithPath(name)
	}
	// The task is gone from state, so this now regenerates exactly the
	// survivors' perimeters, dropping the tree that just disappeared.
	refreshAfterRemoval(c)
	return kept, nil
}

// describePorcelain turns "git status --porcelain" output into one line of
// prose. Reporting only its first line both leaked git's format and hid
// every path after the first.
func describePorcelain(s string) string {
	// Never slice at a fixed column. Porcelain's status is two columns
	// (" M f.txt"), but gitx.Run returns trimmed stdout, so the *first*
	// line arrives with its leading space already gone ("M f.txt") while
	// the rest keep theirs. Splitting on the first space after the status
	// handles both; slicing at [3:] silently ate a character of the first
	// path and reported a file that does not exist.
	var paths []string
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		f := strings.SplitN(strings.TrimLeft(l, " "), " ", 2)
		if len(f) != 2 {
			continue
		}
		p := strings.TrimSpace(f[1])
		if i := strings.Index(p, " -> "); i >= 0 {
			p = p[i+len(" -> "):] // a rename: report where it landed
		}
		if p != "" {
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	if len(paths) <= 3 {
		return strconv.Itoa(len(paths)) + " changed: " + strings.Join(paths, ", ")
	}
	return strconv.Itoa(len(paths)) + " changed, including " + strings.Join(paths[:3], ", ")
}

// remedyFor answers the only question a refusal leaves open: what now. It
// names the action each *blocking* code needs, deduplicated and in a stable
// order, and never repeats the problem list back at the user.
func remedyFor(blocking []Blocker, name string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	forceable := true
	for _, b := range blocking {
		switch b.Code {
		case "WKT_DIRTY", "WKT_UNTRACKED_TREE_CONTENT":
			add("commit or stash the changes, or move them out of the tree")
		case "WKT_UNPUSHED":
			add("push the commits, or keep the task")
		case "WKT_PRECIOUS_IGNORED":
			add("copy the ignored files you need out of the tree")
		case "WKT_IN_PROGRESS":
			add("finish or abort the in-progress git operation")
		case "WKT_SUBMODULE":
			add("push the submodule's commits, then git submodule deinit it")
			forceable = false
		case "WKT_FOREIGN_REPO":
			add("move the repository out of the tree: its history exists nowhere else")
			forceable = false
		case "WKT_COPY_DIVERGED", "WKT_LINK_SLOT_CHANGED", "WKT_LINK_SLOT_MISSING":
			add("reconcile the changed file against the workspace copy")
		case "WKT_CHECK_FAILED":
			add("re-run once the repository is readable: a check that cannot run is treated as work at risk")
			forceable = false
		case "WKT_WORKTREE_MISSING":
			add("the worktree is gone from disk; wkt rm --force finishes the removal")
		}
	}
	if forceable {
		add("or wkt rm " + name + " --force once you are sure nothing above matters")
	}
	return out
}

// plural keeps counted details reading like prose rather than like a log line.
func plural(n, noun string) string {
	if n == "1" {
		return n + " " + noun
	}
	return n + " " + noun + "s"
}

// dropOwned removes wkt's own perimeter copies from a porcelain listing,
// leaving everything else — so a repository whose only untracked file is the
// perimeter reads as clean, while one that also carries the user's own
// .claude/agents/foo.md still blocks.
func dropOwned(status, wt, treeRoot string, owned map[string]bool) string {
	if status == "" || len(owned) == 0 {
		return status
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(status, "\n"), "\n") {
		f := strings.SplitN(strings.TrimLeft(line, " "), " ", 2)
		if len(f) != 2 {
			kept = append(kept, line)
			continue
		}
		rel, err := filepath.Rel(treeRoot, filepath.Join(wt, strings.TrimSpace(f[1])))
		if err != nil || !owned[filepath.Clean(rel)] {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// ownsSomethingUnder reports whether any perimeter copy lives below rel, so
// the walk descends into the directory holding it instead of blocking on the
// directory itself.
func ownsSomethingUnder(owned map[string]bool, rel string) bool {
	prefix := rel + string(filepath.Separator)
	for p := range owned {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// underReported reports whether rel sits below a directory already reported
// as untracked content.
func underReported(reported []string, rel string) bool {
	for _, dir := range reported {
		if strings.HasPrefix(rel, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// keepUnpushed parks a removed branch's tip under refs/wkt/removed/<task>
// when it carries commits no remote has.
//
// Only when there is something to keep: a ref for every removal would turn
// the store into a graveyard and pin objects gc should be free to drop. The
// test for "something to keep" is the same one the refusal used — commits
// reachable from the branch but from no remote and not the recorded base.
func keepUnpushed(storePath string, r state.Repo, task string) (Kept, bool) {
	tip, err := gitx.Run(storePath, "rev-parse", "--verify", "--quiet", "refs/heads/"+r.Branch)
	if err != nil || tip == "" {
		return Kept{}, false
	}
	args := []string{"rev-list", "--count", tip, "--not", "--remotes"}
	if r.BaseSHA != "" {
		args = append(args, r.BaseSHA)
	}
	out, err := gitx.Run(storePath, args...)
	if err != nil || strings.TrimSpace(out) == "0" {
		return Kept{}, false // pushed, or nothing beyond the base: nothing at risk
	}
	ref := "refs/wkt/removed/" + task
	if _, err := gitx.Run(storePath, "update-ref", ref, tip); err != nil {
		return Kept{}, false
	}
	return Kept{Repo: r.RelPath, Store: storePath, Ref: ref, SHA: tip}, true
}

// carriedUnchanged reports whether this ignored path is a file wkt carried in
// that still matches what it copied.
// producedExempt reports whether an ignored path may be passed over because
// the post-create script made it.
//
// A file, yes: it was generated, and a service rewriting its own database is
// expected — hashing it the way the carry does would put --force back into
// everyone's habits, which is what this exemption exists to prevent.
//
// A directory, no. git collapses a wholly ignored directory to one line, so
// exempting ".secrets/" by path exempts every file created inside it
// afterwards, and a key dropped in there would be deleted without a word.
// Only the artifact allowlist exempts a directory, which is a deliberate,
// enumerated list rather than whatever a script happened to create.
func producedExempt(produced map[string]bool, key, reported string) bool {
	if !produced[filepath.Clean(filepath.FromSlash(key))] {
		return false
	}
	if strings.HasSuffix(reported, "/") || strings.HasSuffix(reported, string(filepath.Separator)) {
		return artifact.IsRegenerable(reported)
	}
	return true
}

func carriedUnchanged(carried map[string]string, treeRel, abs string) bool {
	want, ok := carried[filepath.Clean(treeRel)]
	if !ok || want == "" {
		return false
	}
	got, err := tree.Hash(abs)
	return err == nil && got == want
}
