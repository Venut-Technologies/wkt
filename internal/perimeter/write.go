package perimeter

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// Divergence is one copy that no longer matches what wkt wrote.
type Divergence struct {
	Dir    string // the covered directory, not the file
	Reason string // "missing" or "diverged"
}

// settingsPath is where Claude Code reads project settings from, relative to
// a directory a session might start in.
func settingsPath(dir string) string {
	return filepath.Join(dir, ".claude", "settings.json")
}

// coveredDirs are the directories a session can start in and still be under a
// perimeter: the tree root and each materialised repository root. Nothing
// else is listed, because nothing else is covered — a session started deeper
// has no perimeter at all (H6a), and claiming otherwise would be the one lie
// this feature cannot afford.
//
// Back-filled repositories are deliberately absent: their tree entry is a
// symlink into the user's workspace, so "writing into the tree" there would
// write into the user's repository.
func coveredDirs(c container.C, t state.Task) []string {
	tree := c.TreePath(t.Name)
	dirs := []string{tree}
	for _, r := range t.Repos {
		dirs = append(dirs, filepath.Join(tree, filepath.FromSlash(r.RelPath)))
	}
	return dirs
}

// Write renders the perimeter and installs a copy in every covered directory,
// returning the coverage and each copy's hash for the caller to record in
// state. It refuses rather than overwriting a settings file wkt does not own.
func Write(c container.C, t state.Task, siblings []string) ([]string, map[string]string, error) {
	doc, err := For(c, t, siblings)
	if err != nil {
		return nil, nil, err
	}
	body, err := Render(doc)
	if err != nil {
		return nil, nil, err
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])

	dirs := coveredDirs(c, t)

	// Decide about every destination before writing any of them, so a
	// half-written tree is not a thing that can happen. A directory wkt does
	// not own is skipped rather than refused: repositories carry their own
	// .claude/settings.json in git often enough that refusing made wkt
	// unusable on them, and the refusal's own advice — leave it and accept
	// that this directory has no perimeter — named an option the tool did not
	// offer. Partial coverage is not new here: a back-filled repository is
	// deliberately uncovered, and a session started deeper has none at all
	// (H6a). What the caller must do is say which directories were left out;
	// see Skipped.
	writable := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		err := checkOwned(dir, t)
		if err == nil {
			writable = append(writable, dir)
			continue
		}
		var e *wkterr.E
		if errors.As(err, &e) && e.Code == "WKT_PERIMETER_FOREIGN" {
			continue // not wkt's to manage; the rest of the tree still is
		}
		return nil, nil, err
	}

	coverage := make([]string, 0, len(writable))
	hashes := make(map[string]string, len(writable))
	for _, dir := range writable {
		if err := writeAtomic(settingsPath(dir), body); err != nil {
			return nil, nil, err
		}
		coverage = append(coverage, dir)
		hashes[dir] = hash
	}
	return coverage, hashes, nil
}

// Skipped names the directories a perimeter write left out — the ones holding
// a settings file wkt did not write. Coverage that is only sometimes true is
// worse than none *when nobody says so*, so this is what makes the omission
// visible: callers warn with it, and status reports it from the recorded
// coverage.
func Skipped(c container.C, t state.Task, coverage []string) []string {
	covered := make(map[string]bool, len(coverage))
	for _, d := range coverage {
		covered[d] = true
	}
	var out []string
	for _, dir := range coveredDirs(c, t) {
		if !covered[dir] {
			out = append(out, dir)
		}
	}
	return out
}

// checkOwned refuses when a settings file exists that wkt did not write. A
// repository may carry its own .claude/settings.json in git; silently
// replacing it would destroy the user's configuration, and a task is not
// worth that.
func checkOwned(dir string, t state.Task) error {
	path := settingsPath(dir)
	// Lstat, not Stat: a symlink here is not a file wkt may follow.
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot inspect a settings file").
			WithPath(path).WithFound(err.Error())
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return wkterr.New("WKT_PERIMETER_FOREIGN", "the settings path is a symlink; wkt will not write through it").
			WithPath(path).
			WithRemedy("replace the symlink with a real file, or move it aside")
	}
	if _, ours := t.PerimeterHashes[dir]; ours {
		return nil // wkt wrote this one; regenerating it is the whole point
	}
	// State can be lost, hand-edited, or written by a wkt that predates the
	// perimeter. The file itself carries the answer, so ask it before
	// refusing to overwrite what is plainly wkt's own output.
	if b, readErr := os.ReadFile(path); readErr == nil && marked(b) {
		return nil
	}
	return wkterr.New("WKT_PERIMETER_FOREIGN", "a settings file already exists that wkt did not write").
		WithPath(path).
		WithRemedy("move the existing file aside if you want wkt to manage this directory",
			"or leave it and accept that this directory has no wkt perimeter")
}

// writeAtomic writes through a temporary file in the same directory and then
// renames, so a reader never sees a partial perimeter.
func writeAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot create the settings directory").
			WithPath(dir).WithFound(err.Error())
	}
	tmp, err := os.CreateTemp(dir, "settings.*.tmp")
	if err != nil {
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot create a temporary settings file").
			WithPath(dir).WithFound(err.Error())
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot write the perimeter").
			WithPath(tmp.Name()).WithFound(err.Error())
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot close the temporary settings file").
			WithPath(tmp.Name()).WithFound(err.Error())
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
		return wkterr.New("WKT_PERIMETER_WRITE", "cannot commit the perimeter").
			WithPath(path).WithFound(err.Error())
	}
	return nil
}

// Verify reports which recorded copies are gone and which no longer match the
// hash wkt recorded. It enumerates the filesystem rather than trusting state,
// like everything else in wkt that has to be believed.
func Verify(c container.C, t state.Task) ([]Divergence, error) {
	var out []Divergence
	for _, dir := range t.PerimeterCoverage {
		want, recorded := t.PerimeterHashes[dir]
		body, err := os.ReadFile(settingsPath(dir))
		if os.IsNotExist(err) {
			out = append(out, Divergence{Dir: dir, Reason: "missing"})
			continue
		}
		if err != nil {
			// Unreadable is not "fine": a check that cannot run counts as
			// drift, the same rule the teardown checks follow.
			out = append(out, Divergence{Dir: dir, Reason: "diverged"})
			continue
		}
		sum := sha256.Sum256(body)
		if !recorded || hex.EncodeToString(sum[:]) != want {
			out = append(out, Divergence{Dir: dir, Reason: "diverged"})
		}
	}
	return out, nil
}

// marked reports whether a settings file carries wkt's marker.
func marked(b []byte) bool {
	var probe struct {
		Marker Marker `json:"$wkt"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	return probe.Marker.Version > 0
}

// Stale reports whether the perimeter on disk is what today's task list would
// produce. It is a different question from Verify: a copy can match its
// recorded hash exactly and still be out of date, because a task created since
// is not named in it — and sibling trees have to be named individually (H16),
// so nothing else covers them.
func Stale(c container.C, t state.Task, names []string) (bool, error) {
	doc, err := For(c, t, names)
	if err != nil {
		return false, err
	}
	body, err := Render(doc)
	if err != nil {
		return false, err
	}
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])
	for _, dir := range t.PerimeterCoverage {
		if t.PerimeterHashes[dir] != want {
			return true, nil
		}
	}
	return len(t.PerimeterCoverage) == 0, nil
}
