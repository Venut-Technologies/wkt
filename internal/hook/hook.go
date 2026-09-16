// Package hook implements Claude Code's worktree hook contracts.
//
// Claude Code's --worktree mode normally calls git. Configure these hooks and
// it calls wkt instead, so a session started in a multi-repo workspace lands
// in a wkt task tree — every participating repository on one branch, laid out
// in the workspace's shape — rather than in a single repository's worktree.
//
// The contract, as documented by the 2.1.238 binary:
//
//	WorktreeCreate  stdin: JSON with "name" (a suggested slug)
//	                stdout: the absolute path of the created worktree
//	                exit 0 on success
//	WorktreeRemove  stdin: JSON with "worktree_path"
//	                exit 0 on success; other codes show stderr to the user
//
// Two properties follow from that and are easy to lose:
//
//   - stdout carries the path and nothing else. Any other line — a warning, a
//     progress note — is read as part of the path.
//   - the payload is unstable (H14): its shape differs by entry path and has
//     already gained a field between releases. Take what is needed, ignore the
//     rest, and never read transcript_path, which on one path names a file
//     that does not exist yet.
package hook

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Create is the WorktreeCreate payload, reduced to the fields wkt uses.
type Create struct {
	Name string `json:"name"`
	Cwd  string `json:"cwd"`
}

// Remove is the WorktreeRemove payload.
type Remove struct {
	WorktreePath string `json:"worktree_path"`
}

func ParseCreate(r io.Reader) (Create, error) {
	var c Create
	if err := json.NewDecoder(r).Decode(&c); err != nil {
		return Create{}, wkterr.New("WKT_HOOK_PAYLOAD", "cannot read the WorktreeCreate payload").
			WithFound(err.Error())
	}
	if strings.TrimSpace(c.Name) == "" {
		return Create{}, wkterr.New("WKT_HOOK_PAYLOAD", "the WorktreeCreate payload carries no name").
			WithRemedy("this is the one field the hook contract promises; check the Claude Code version")
	}
	return c, nil
}

func ParseRemove(r io.Reader) (Remove, error) {
	var rm Remove
	if err := json.NewDecoder(r).Decode(&rm); err != nil {
		return Remove{}, wkterr.New("WKT_HOOK_PAYLOAD", "cannot read the WorktreeRemove payload").
			WithFound(err.Error())
	}
	if strings.TrimSpace(rm.WorktreePath) == "" {
		return Remove{}, wkterr.New("WKT_HOOK_PAYLOAD", "the WorktreeRemove payload carries no worktree_path").
			WithRemedy("wkt will not guess which tree to remove")
	}
	return rm, nil
}

// Slug turns a suggested name into something that works as both a directory
// name and a branch name. The hook contract has no channel for "I renamed your
// slug", so this never fails: an unusable suggestion becomes "task".
func Slug(name string) string {
	out := make([]rune, 0, len(name))
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		case r == '-' || r == '_':
			out = append(out, r)
		case r == '.':
			// A dot is legal inside a branch name but leads to ".." and to
			// hidden directories, so it is dropped rather than reasoned about.
		default:
			// Everything else — separators, spaces, colons, control
			// characters — collapses to a single dash.
			out = append(out, '-')
		}
	}
	s := collapseDashes(string(out))
	s = strings.Trim(s, "-_")
	if s == "" || s == "." || s == ".." {
		return "task"
	}
	return s
}

func collapseDashes(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
		} else {
			prevDash = false
		}
		b.WriteRune(r)
	}
	return b.String()
}
