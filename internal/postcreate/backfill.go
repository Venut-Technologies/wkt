package postcreate

import (
	"os"
	"path/filepath"

	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// WithdrawBackFill removes the tree's back-fill symlinks and returns the
// function that puts them back.
//
// A repository the task did not select appears in the tree as a symlink into
// the workspace (spec §5.3 rule 3). Measured: writing through that path lands
// in the developer's own checkout — both a file and a node_modules directory
// arrived there.
//
// The setup script people actually write is
//
//	for d in */; do (cd "$d" && npm install); done
//
// and written that way it installs into their working repositories. That is
// the isolation this whole tool exists to provide, undone by the one feature
// that runs their code. The carry copies rather than links for exactly this
// reason; the seam would open the door again.
//
// Documentation does not reach this, because the dangerous script is the one
// written without thinking about it — and increasingly the one an agent
// writes, which will reach for the idiom every time. So the links are simply
// not there to be walked into.
//
// The cost is real and worth stating: a script that legitimately wants to read
// a back-filled repository will not see it while the seam runs.
func WithdrawBackFill(treeRoot string, links []state.LinkSlot) (func(), error) {
	type held struct{ path, target string }
	var withdrawn []held
	restore := func() {
		for _, h := range withdrawn {
			// Best effort by design. A failure here shows up as a missing
			// link in wkt status and is repaired by wkt repair; failing the
			// command instead would be worse, because the task and its work
			// are fine.
			_ = os.Symlink(h.target, h.path)
		}
	}

	for _, l := range links {
		if l.Type != "symlink" {
			continue
		}
		p := filepath.Join(treeRoot, filepath.FromSlash(l.RelPath))
		info, err := os.Lstat(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink == 0 {
			// Not the link that was recorded. Say so rather than deleting
			// something that is not wkt's to delete.
			restore()
			return nil, wkterr.New("WKT_POST_CREATE",
				"a back-fill slot no longer holds a symlink; wkt will not run the seam over it").
				WithPath(p).
				WithRemedy("wkt repair " + filepath.Base(treeRoot) + " rebuilds the tree's links")
		}
		target, err := os.Readlink(p)
		if err != nil {
			restore()
			return nil, wkterr.New("WKT_POST_CREATE", "cannot read a back-fill link").WithPath(p)
		}
		if err := os.Remove(p); err != nil {
			restore()
			return nil, wkterr.New("WKT_POST_CREATE", "cannot withdraw a back-fill link").WithPath(p)
		}
		withdrawn = append(withdrawn, held{path: p, target: target})
	}
	return restore, nil
}
