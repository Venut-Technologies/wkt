package task

import (
	"strings"

	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Branch names live in a namespace shaped like a filesystem, and git only says
// so at the moment it creates something. By then wkt has built a store,
// written a base pin into the developer's repository and begun adding a
// worktree, so the refusal arrives as a raw "fatal:" from the middle of an
// operation that has already changed things. These questions are asked of
// every repository wkt is about to touch, before it touches any of them.

// branchesIn lists a repository's local branches by short name. A repository
// that cannot be asked yields nothing rather than an error: the callers are
// pre-flight checks, and the operations they guard will fail loudly on their
// own if the repository is truly unusable.
func branchesIn(dir string) []string {
	out, err := gitx.Run(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	if err != nil {
		return nil
	}
	var names []string
	for _, b := range strings.Split(out, "\n") {
		if b != "" {
			names = append(names, b)
		}
	}
	return names
}

// caseCollision returns a branch differing from name only in case, or "".
//
// The question goes to git's ref list rather than to the filesystem, so macOS
// and Linux give the same answer. On a case-insensitive filesystem the two
// branches cannot coexist at all; on a case-sensitive one they can, and a task
// that relied on the difference would stop working the moment a colleague on a
// Mac cloned the repository.
func caseCollision(dir, name string) string {
	for _, b := range branchesIn(dir) {
		// An exact match is a different answer with a different remedy, and
		// the callers ask that one first.
		if b != name && strings.EqualFold(b, name) {
			return b
		}
	}
	return ""
}

// dfConflict returns a branch that cannot coexist with name, or "".
//
// refs/heads/feat and refs/heads/feat/42 need one path to be both a file and a
// directory. Both directions are checked: a task name is a single segment and
// can only collide downwards, but fetch --as takes a name with slashes in it
// and collides upwards just as well.
func dfConflict(dir, name string) string {
	for _, b := range branchesIn(dir) {
		if strings.HasPrefix(b, name+"/") || strings.HasPrefix(name, b+"/") {
			return b
		}
	}
	return ""
}

// collisionIn asks both questions of one repository and returns wkt's refusal,
// naming the branch in the way. The caller adds the repository and the remedy,
// because what to do about it differs between the developer's own repository
// and a store they have never seen.
func collisionIn(dir, name string) *wkterr.E {
	if b := caseCollision(dir, name); b != "" {
		return wkterr.New("WKT_BRANCH_CASE_COLLISION", "a branch differing only in case exists").
			WithFound(b)
	}
	if b := dfConflict(dir, name); b != "" {
		return wkterr.New("WKT_BRANCH_DF_CONFLICT", "a branch name conflicts hierarchically").
			WithFound(b)
	}
	return nil
}
