package discover

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/paths"
)

type Kind int

const (
	KindRepo Kind = iota
	KindLinkedWorktree
	KindSubmodule
	KindNested
)

type Entry struct {
	RelPath     string
	AbsPath     string
	Kind        Kind
	ContainedBy string
}

// Walk enumerates .git markers under workspace, never following symlinks.
func Walk(workspace string, maxDepth int) ([]Entry, error) {
	root, err := paths.Canonical(workspace)
	if err != nil {
		return nil, err
	}
	var out []Entry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable subtree: skip, do not abort the walk
		}
		if p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if strings.Count(rel, string(filepath.Separator)) > maxDepth {
			return fs.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil // never follow symlinks (spec §5.3 rule 4), skip only this entry
		}
		if d.Name() != ".git" {
			return nil
		}
		repoDir := filepath.Dir(p)
		repoRel, _ := filepath.Rel(root, repoDir)
		e := Entry{RelPath: filepath.ToSlash(repoRel), AbsPath: repoDir, Kind: classify(p, repoDir)}
		out = append(out, e)
		// fs.WalkDir's SkipDir means "don't descend into this" only when the
		// visited entry is itself a directory; on a non-directory entry it
		// instead means "skip the rest of this directory's siblings" — a
		// linked worktree or submodule checkout's ".git" is always a regular
		// *file*, so returning SkipDir unconditionally here silently
		// truncated the scan of the rest of that repository's own subtree
		// right after visiting its own marker, hiding any nested repository
		// sorting after ".git" (almost anything). Only an actual ".git"
		// *directory* should stop descent — the same fix already applied in
		// internal/task/remove.go's foreign-repo walk (round 2 review),
		// confirmed there against real fs.WalkDir before being ported here.
		if d.IsDir() {
			return fs.SkipDir // do not descend into a repository's own .git directory
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	markNested(out)
	return out, nil
}

func classify(gitMarker, repoDir string) Kind {
	info, err := os.Lstat(gitMarker)
	if err != nil {
		return KindRepo
	}
	if info.IsDir() {
		return KindRepo
	}
	// .git is a file: either a linked worktree or a submodule checkout.
	b, err := os.ReadFile(gitMarker)
	if err != nil {
		return KindRepo
	}
	target := strings.TrimSpace(strings.TrimPrefix(string(b), "gitdir:"))

	// Resolve target relative to repoDir
	var gitdir string
	if filepath.IsAbs(target) {
		gitdir = target
	} else {
		gitdir = filepath.Join(repoDir, target)
	}

	// Check if gitdir contains a gitdir file (linked worktree marker)
	if _, err := os.Stat(filepath.Join(gitdir, "gitdir")); err == nil {
		return KindLinkedWorktree
	}

	// Fall back to substring matching for submodules
	if strings.Contains(target, string(filepath.Separator)+"modules"+string(filepath.Separator)) {
		return KindSubmodule
	}

	return KindRepo
}

func markNested(entries []Entry) {
	// First pass: collect all repositories to analyze
	var repos []*Entry
	for i := range entries {
		if entries[i].Kind == KindRepo {
			repos = append(repos, &entries[i])
		}
	}

	// Second pass: for each repo, find all containing repos and pick the deepest one
	for _, e := range repos {
		var containers []*Entry
		for _, c := range repos {
			if e == c {
				continue
			}
			if paths.IsUnder(e.AbsPath, c.AbsPath) {
				containers = append(containers, c)
			}
		}

		if len(containers) > 0 {
			// Find the deepest container (longest AbsPath)
			deepest := containers[0]
			for _, c := range containers[1:] {
				if len(c.AbsPath) > len(deepest.AbsPath) {
					deepest = c
				}
			}
			e.Kind = KindNested
			e.ContainedBy = deepest.RelPath
		}
	}
}

func NestedPairs(entries []Entry) [][2]string {
	var out [][2]string
	for _, e := range entries {
		if e.Kind == KindNested {
			out = append(out, [2]string{e.RelPath, e.ContainedBy})
		}
	}
	return out
}
