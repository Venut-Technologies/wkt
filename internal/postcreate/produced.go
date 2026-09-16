package postcreate

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/artifact"
	"github.com/Venut-Technologies/wkt/internal/gitx"
)

// Snapshot lists the ignored paths each materialised repository holds, in
// git's own reporting.
//
// It runs the same command teardown reads — "git status --porcelain
// --ignored=matching" — and that is the point rather than a convenience.
// Teardown classifies the "!! " lines that command produces; a snapshot taken
// any other way would speak a different vocabulary and the two sets would
// never line up. Measured: a wholly ignored directory collapses to a single
// line whatever it holds, so "config/" is reported and "config/local.yaml"
// never is — and so this stays cheap even after an install has put a hundred
// thousand files under node_modules.
//
// A repository that cannot be asked contributes nothing, which is the safe
// direction to be wrong in: an unrecorded path makes teardown refuse and list
// it, which is the behaviour that already exists.
func Snapshot(treeRoot string, repos []string) map[string]bool {
	out := map[string]bool{}
	walkTree(treeRoot, repos, out)
	for _, rel := range repos {
		dir := filepath.Join(treeRoot, filepath.FromSlash(rel))
		s, err := gitx.Run(dir, "status", "--porcelain", "--ignored=matching")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(s, "\n") {
			if !strings.HasPrefix(line, "!! ") {
				continue
			}
			out[rel+"/"+strings.TrimPrefix(line, "!! ")] = true
		}
	}
	return out
}

// walkTree records what lies in the tree outside every repository.
//
// A script can write there — an intermediate directory of the mirrored tree,
// or the tree root itself — and teardown blocks on it as untracked tree
// content rather than as ignored content, so git cannot answer for it. Found
// by the battery: the "for d in */" idiom touches the mirrored tree's own
// directories, not only the repositories inside them.
//
// The paths are tree-relative, which is the same vocabulary the repository
// half produces and the same one teardown's walk uses.
//
// Symlinks are never followed, and never recorded: a back-fill link belongs to
// the workspace, and what lies beyond it is not the tree's to dispose of.
func walkTree(treeRoot string, repos []string, out map[string]bool) {
	inRepo := func(rel string) bool {
		for _, r := range repos {
			if rel == r || strings.HasPrefix(rel, r+"/") {
				return true
			}
		}
		return false
	}
	_ = filepath.WalkDir(treeRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == treeRoot {
			return nil
		}
		rel := filepath.ToSlash(strings.TrimPrefix(p, treeRoot+string(filepath.Separator)))
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if inRepo(rel) {
			return fs.SkipDir // git answers for everything below a repository
		}
		out[rel] = true
		// Collapse a build directory the way git collapses a wholly ignored
		// one. The seam exists to run installers, and an installer at the tree
		// root leaves a node_modules with a hundred thousand files in it;
		// without this each becomes a permanent entry in the task's state, and
		// the walk runs twice per seam. Teardown already treats these as
		// regenerable whole.
		if d.IsDir() && artifact.IsRegenerable(rel) {
			return fs.SkipDir
		}
		return nil
	})
}

// NewSince returns what the script brought into being, sorted so that state
// written twice from the same tree is written the same way.
func NewSince(before, after map[string]bool) []string {
	var out []string
	for p := range after {
		if !before[p] {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}
