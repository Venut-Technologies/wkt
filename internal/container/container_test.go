package container

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func TestLocateIsSiblingAndNeverInsideWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "work")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root != c.Workspace+".worktrees" {
		t.Fatalf("container %q is not the documented sibling of %q", c.Root, c.Workspace)
	}
	if paths.IsUnder(c.Root, c.Workspace) {
		t.Fatalf("container %q must never live inside the workspace", c.Root)
	}
	canonWs, err := paths.Canonical(ws)
	if err != nil {
		t.Fatal(err)
	}
	if c.Workspace != canonWs {
		t.Fatalf("container workspace %q is not canonical (expected %q)", c.Workspace, canonWs)
	}
}

func TestLocateFallsBackWhenParentUnwritable(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "work")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })

	c, err := Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if c.Root == ws+".worktrees" {
		t.Fatal("an unwritable parent must force the state-directory fallback")
	}
}

func TestLocateRejectsWhenFallbackWouldBeInsideWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "work")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(base, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(base, 0o755) })

	// Set HOME to the workspace so the fallback would be inside it
	oldHome := os.Getenv("HOME")
	t.Setenv("HOME", ws)
	t.Cleanup(func() { os.Setenv("HOME", oldHome) })

	_, err := Locate(ws)
	if err == nil {
		t.Fatal("Locate must refuse when the fallback container would be inside the workspace")
	}
	e, ok := err.(*wkterr.E)
	if !ok {
		t.Fatalf("error is not *wkterr.E: %T", err)
	}
	if e.Code != "WKT_NO_CONTAINER" {
		t.Fatalf("error code is %q, expected WKT_NO_CONTAINER", e.Code)
	}
}

func TestLockIsExclusive(t *testing.T) {
	base := t.TempDir()
	c := C{Root: filepath.Join(base, "c"), Workspace: base}
	if err := Create(c); err != nil {
		t.Fatal(err)
	}

	lockPath := filepath.Join(c.Root, ".wkt.lock")
	release, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}

	// Capture inode after first lock
	info1, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	inode1 := info1.Sys().(*syscall.Stat_t).Ino

	if _, err := LockFor(c, 0); err == nil {
		release()
		t.Fatal("a second lock must fail while the first is held")
	}
	release()

	release2, err := Lock(c)
	if err != nil {
		t.Fatalf("lock after release must succeed: %v", err)
	}

	// Verify inode is the same after second lock
	info2, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	inode2 := info2.Sys().(*syscall.Stat_t).Ino

	if inode1 != inode2 {
		t.Fatalf("lock file inode changed: %d != %d (proves lock file was unlinked and recreated)", inode1, inode2)
	}

	release2()
}

// TestLockWaitsForTheHolder covers finding F7. The tool's premise is two
// agents working at once, and LOCK_NB with no wait made the second one fail
// outright whenever their commands happened to overlap — correct, but
// abrasive, and a failure the caller cannot do anything useful about.
func TestLockWaitsForTheHolder(t *testing.T) {
	base := t.TempDir()
	c := C{Root: filepath.Join(base, "c"), Workspace: base}
	if err := Create(c); err != nil {
		t.Fatal(err)
	}
	release, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}

	// Hold it briefly, then let go, as a short wkt command would.
	go func() {
		time.Sleep(150 * time.Millisecond)
		release()
	}()

	// Lock, not LockFor: the point is that the entry point every command
	// uses waits. A test that calls LockFor here passes against a Lock that
	// still fails immediately.
	start := time.Now()
	second, err := Lock(c)
	if err != nil {
		t.Fatalf("the second caller must wait rather than fail: %v", err)
	}
	defer second()
	if waited := time.Since(start); waited < 100*time.Millisecond {
		t.Fatalf("it returned in %v, so it cannot have waited for the holder", waited)
	}
}

// TestLockGivesUpEventually — waiting forever would be its own bug: a stale
// holder would hang every later command with no way to tell what is wrong.
func TestLockGivesUpEventually(t *testing.T) {
	base := t.TempDir()
	c := C{Root: filepath.Join(base, "c"), Workspace: base}
	if err := Create(c); err != nil {
		t.Fatal(err)
	}
	release, err := Lock(c)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	start := time.Now()
	if _, err := LockFor(c, 200*time.Millisecond); err == nil {
		t.Fatal("a lock that is never released must eventually be reported")
	} else if e, ok := err.(*wkterr.E); !ok || e.Code != "WKT_LOCKED" {
		t.Fatalf("want WKT_LOCKED, got %v", err)
	}
	if waited := time.Since(start); waited > 3*time.Second {
		t.Fatalf("it waited %v, far past the deadline it was given", waited)
	}
}

// TestTaskLockExcludesTheSameTaskAndNotAnother — the container lock is too
// coarse for work that takes minutes. A post-create script may run for the
// length of a dependency install, and holding the container across it would
// stop every other command in the workspace, in a tool whose premise is many
// tasks at once. It is also not optional: while that script runs the tree's
// back-fill links are withdrawn, and a command arriving to find them missing
// would report damage that is not there.
func TestTaskLockExcludesTheSameTaskAndNotAnother(t *testing.T) {
	base := t.TempDir()
	c := C{Root: filepath.Join(base, "c"), Workspace: base}
	if err := Create(c); err != nil {
		t.Fatal(err)
	}

	release, err := LockTask(c, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockTaskFor(c, "alpha", 0); err == nil {
		release()
		t.Fatal("a second lock on the same task must not be granted")
	}
	// A different task is unaffected: that other tasks proceed is the whole
	// reason the container lock is released.
	other, err := LockTaskFor(c, "beta", 0)
	if err != nil {
		release()
		t.Fatalf("another task must still be lockable: %v", err)
	}
	other()
	release()

	again, err := LockTaskFor(c, "alpha", 0)
	if err != nil {
		t.Fatalf("the lock must be reusable after release: %v", err)
	}
	again()
}

// The container lock and a task lock are different locks: a task's own long
// step must not exclude commands that touch nothing of its.
func TestTaskLockDoesNotHoldTheContainer(t *testing.T) {
	base := t.TempDir()
	c := C{Root: filepath.Join(base, "c"), Workspace: base}
	if err := Create(c); err != nil {
		t.Fatal(err)
	}
	release, err := LockTask(c, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	container, err := LockFor(c, 0)
	if err != nil {
		t.Fatalf("the container must stay free while a task lock is held: %v", err)
	}
	container()
}
