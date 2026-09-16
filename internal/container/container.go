package container

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

type C struct {
	Root      string
	Workspace string
}

func (c C) StoreDir() string   { return filepath.Join(c.Root, "store") }
func (c C) TreesDir() string   { return filepath.Join(c.Root, "trees") }
func (c C) StateDir() string   { return filepath.Join(c.Root, "state", "tasks") }
func (c C) StagingDir() string { return filepath.Join(c.Root, "staging") }

// ConfigDir holds the container's own state, as opposed to StateDir, which
// holds one file per task.
func (c C) ConfigDir() string { return filepath.Join(c.Root, "state") }

func (c C) TreePath(task string) string { return filepath.Join(c.TreesDir(), task) }

func Locate(workspace string) (C, error) {
	ws, err := paths.Canonical(workspace)
	if err != nil {
		return C{}, err
	}
	sibling := ws + ".worktrees"
	if writable(filepath.Dir(ws)) {
		return C{Root: sibling, Workspace: ws}, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return C{}, wkterr.New("WKT_NO_CONTAINER", "cannot place the container").WithFound(err.Error())
	}
	sum := sha256.Sum256([]byte(ws))
	id := hex.EncodeToString(sum[:])[:12]
	root := filepath.Join(home, ".local", "state", "wkt", id)
	if paths.IsUnder(root, ws) {
		return C{}, wkterr.New("WKT_NO_CONTAINER", "the fallback container would live inside the workspace").
			WithPath(root).
			WithFound("workspace: " + ws).
			WithRemedy("configure the container location explicitly")
	}
	return C{Root: root, Workspace: ws}, nil
}

func writable(dir string) bool {
	probe := filepath.Join(dir, ".wkt-write-probe-"+strconv.Itoa(os.Getpid()))
	f, err := os.OpenFile(probe, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	_ = f.Close()
	_ = os.Remove(probe)
	return true
}

func Create(c C) error {
	for _, d := range []string{c.StoreDir(), c.TreesDir(), c.StateDir(), c.StagingDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return wkterr.New("WKT_NO_CONTAINER", "cannot create the container").
				WithPath(d).WithFound(err.Error())
		}
	}
	return nil
}

// DefaultLockWait is how long a command waits for another wkt process to
// finish before giving up. The whole premise is two agents working at once,
// and set-level operations here take well under a second, so a caller that
// arrives mid-command should queue rather than fail — but never forever: a
// stale holder must be reported, not hung on.
const DefaultLockWait = 10 * time.Second

// Lock takes the container lock, waiting up to DefaultLockWait.
func Lock(c C) (func(), error) { return LockFor(c, DefaultLockWait) }

// LockFor is Lock with an explicit deadline. A zero wait is the old
// fail-immediately behaviour, which is what a test wants when it is checking
// that the lock excludes at all.
func LockFor(c C, wait time.Duration) (func(), error) {
	return flockPath(filepath.Join(c.Root, ".wkt.lock"), "container lock", wait)
}

// LockTask takes a lock scoped to one task, waiting up to DefaultLockWait.
//
// The container lock is too coarse for work that runs for minutes. A
// post-create script may take as long as a dependency install, and holding the
// container across it would stop every other command in the workspace — in a
// tool whose premise is many tasks at once. Nor is a task lock merely an
// optimisation: while that script runs the tree's back-fill links are
// withdrawn, and a command arriving to find them missing would report damage
// that is not there.
//
// The name is a single path segment: task.Validate refuses a separator, and
// the hook path narrows it further through hook.Slug.
func LockTask(c C, name string) (func(), error) { return LockTaskFor(c, name, DefaultLockWait) }

// LockTaskFor is LockTask with an explicit deadline. A zero wait fails
// immediately, which is what a test wants when it is checking that the lock
// excludes at all.
func LockTaskFor(c C, name string, wait time.Duration) (func(), error) {
	// Beside the state files rather than among them: state.List reads that
	// directory, and a lock file sitting there would read as a task.
	dir := filepath.Join(c.Root, "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, wkterr.New("WKT_CONTAINER_UNUSABLE", "cannot create the lock directory").
			WithPath(dir).WithFound(err.Error())
	}
	return flockPath(filepath.Join(dir, name+".lock"), "lock on this task", wait)
}

// flockPath is the locking both locks share: open, poll for the flock until
// the deadline, stamp the holder's pid, and hand back a release.
func flockPath(path, subject string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, wkterr.New("WKT_CONTAINER_UNUSABLE", "cannot open the "+subject).
			WithPath(path).WithFound(err.Error())
	}

	deadline := time.Now().Add(wait)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			holder, _ := os.ReadFile(path)
			_ = f.Close()
			return nil, wkterr.New("WKT_LOCKED", "another wkt process holds the "+subject).
				WithPath(path).WithFound(string(holder)).
				WithRemedy("wait for it to finish",
					"or remove the lock file if no wkt process is running")
		}
		// Poll rather than blocking in flock: a blocking flock cannot be
		// given a deadline without signals, and 50ms of latency is nothing
		// against commands that take a second.
		time.Sleep(50 * time.Millisecond)
	}

	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return func() {
		// Unlock and close, but never unlink: a party that opened the path
		// before an unlink would hold a lock on an orphaned inode while the
		// next Lock locked a freshly created one.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
