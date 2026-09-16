// Package carry implements the gitignored-file carry the design scopes in:
// "there is a post-create seam and a gitignored-file carry, and nothing else"
// (spec §1.1, "No runtime environment").
//
// A worktree is a fresh checkout, so a task tree starts without the local,
// gitignored files a service needs to run — a .env being the usual one. The
// developer had them in their own checkout; the task cannot use them.
//
// Two rules make the mechanism safe:
//
//   - **Copy, never link.** A symlinked secret edited inside a task writes
//     back into the developer's checkout, which is exactly the isolation the
//     rest of the tool provides.
//   - **Matched *and* gitignored.** A file is carried only if a .wktinclude
//     pattern matches it and git already ignores it, so the mechanism can
//     never shadow versioned content with an untracked copy.
package carry

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// IncludeFile is the opt-in, at the workspace root.
const IncludeFile = ".wktinclude"

// Carried is one file that made it into the tree.
type Carried struct {
	// RelPath is workspace-relative, which is also its position in the tree.
	RelPath string
	Hash    string
}

// Plan returns the workspace-relative paths to carry, in a stable order.
//
// Absent a .wktinclude the answer is empty and nothing is read: the feature is
// opt-in and costs nothing when unused.
func Plan(workspace string, repos []state.Repo) ([]string, error) {
	patterns := filepath.Join(workspace, IncludeFile)
	if _, err := os.Stat(patterns); err != nil {
		return nil, nil
	}

	// Pattern matching is git's own, not an approximation of it: the patterns
	// are handed to git as an excludes file and asked about in a scratch
	// repository, so that the repository's own .gitignore cannot contaminate
	// the answer to "does this match .wktinclude".
	scratch, err := os.MkdirTemp("", "wkt-include-")
	if err != nil {
		return nil, wkterr.New("WKT_CARRY", "cannot create a scratch directory for pattern matching").
			WithFound(err.Error())
	}
	defer os.RemoveAll(scratch)
	if _, err := gitx.Run(scratch, "init", "-q"); err != nil {
		return nil, wkterr.New("WKT_CARRY", "cannot prepare pattern matching").WithFound(err.Error())
	}

	var out []string
	for _, r := range repos {
		candidates, err := filesIn(r.AbsPath)
		if err != nil {
			return nil, err
		}
		if len(candidates) == 0 {
			continue
		}

		// Matched by .wktinclude, against BOTH spellings of each file: its
		// path within its repository and its path within the workspace. A
		// gitignore pattern containing a slash is anchored to the root it is
		// read from, so "config/secrets.json" would otherwise mean nothing
		// inside services/svc-a, while "services/*/config/secrets.json" would
		// mean nothing repo-relative. People write both; both work.
		ask := make([]string, 0, len(candidates)*2)
		fromWorkspace := map[string]string{}
		for _, c := range candidates {
			ask = append(ask, c)
			ws := filepath.ToSlash(filepath.Join(r.RelPath, c))
			ask = append(ask, ws)
			fromWorkspace[ws] = c
		}
		hits, err := checkIgnore(scratch, ask, patterns)
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		var matched []string
		for _, h := range hits {
			c := h
			if orig, ok := fromWorkspace[h]; ok {
				c = orig
			}
			if !seen[c] {
				seen[c] = true
				matched = append(matched, c)
			}
		}
		if len(matched) == 0 {
			continue
		}

		// And gitignored by the repository itself, asked with repo-relative
		// paths. Enumerating "git ls-files --others --ignored --directory"
		// instead would have been cheaper and wrong: a wholly ignored
		// directory collapses to one entry there and hides the file inside it.
		ignored, err := checkIgnore(r.AbsPath, matched, "")
		if err != nil {
			return nil, err
		}
		for _, i := range ignored {
			out = append(out, filepath.ToSlash(filepath.Join(r.RelPath, i)))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Apply copies each planned file into the tree, recording its hash so teardown
// can tell an untouched copy from one the task edited.
func Apply(treeRoot, workspace string, plan []string) ([]Carried, error) {
	var out []Carried
	for _, rel := range plan {
		src := filepath.Join(workspace, filepath.FromSlash(rel))
		dst := filepath.Join(treeRoot, filepath.FromSlash(rel))
		sum, err := copyFile(src, dst)
		if err != nil {
			return nil, err
		}
		out = append(out, Carried{RelPath: rel, Hash: sum})
	}
	return out, nil
}

// filesIn lists every regular file in a repository, repo-relative, skipping
// the git directory itself.
func filesIn(repoAbs string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(repoAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner is not a reason to carry nothing
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			return nil
		}
		// Never follow a link out of the repository, and never carry one:
		// what it points at is not this repository's to hand over.
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, relErr := filepath.Rel(repoAbs, p)
		if relErr == nil {
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, wkterr.New("WKT_CARRY", "cannot read the repository").WithPath(repoAbs).WithFound(err.Error())
	}
	return out, nil
}

// checkIgnore asks git which of these paths its ignore rules match. With
// excludesFile set, the rules are that file's; without, the repository's own.
func checkIgnore(dir string, paths []string, excludesFile string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []string{}
	if excludesFile != "" {
		args = append(args, "-c", "core.excludesFile="+excludesFile)
	}
	args = append(args, "check-ignore", "--stdin", "-z")
	if excludesFile != "" {
		// The scratch repository has no index, so paths must be judged as
		// strings rather than as tracked content.
		args = append(args, "--no-index")
	}
	out, err := gitx.RunStdin(dir, strings.Join(paths, "\x00")+"\x00", args...)
	if err != nil {
		// check-ignore exits 1 when nothing matched, which is an answer, not
		// a failure. Anything else is reported.
		if strings.Contains(err.Error(), "exit status 1") || out == "" {
			return nil, nil
		}
		return nil, wkterr.New("WKT_CARRY", "cannot match paths against the ignore rules").
			WithPath(dir).WithFound(err.Error())
	}
	var matched []string
	for _, p := range strings.Split(strings.TrimRight(out, "\x00"), "\x00") {
		if p != "" {
			matched = append(matched, p)
		}
	}
	return matched, nil
}

// copyFile copies contents and mode, and returns the content hash. The mode
// matters: a carried key that loses its 0600 is its own problem.
func copyFile(src, dst string) (string, error) {
	info, err := os.Stat(src)
	if err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot read a file to carry").WithPath(src)
	}
	in, err := os.Open(src)
	if err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot read a file to carry").WithPath(src)
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot create a directory in the tree").WithPath(dst)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot write into the tree").WithPath(dst)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), in); err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot copy a file into the tree").WithPath(dst)
	}
	// OpenFile's mode is subject to umask, so set it explicitly.
	if err := f.Chmod(info.Mode().Perm()); err != nil {
		return "", wkterr.New("WKT_CARRY", "cannot set permissions on a carried file").WithPath(dst)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
