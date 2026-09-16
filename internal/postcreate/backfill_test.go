package postcreate

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/state"
)

func TestBackFillLinksAreGoneDuringAndBackAfter(t *testing.T) {
	tree, elsewhere := t.TempDir(), t.TempDir()
	link := filepath.Join(tree, "svc-b")
	if err := os.Symlink(elsewhere, link); err != nil {
		t.Fatal(err)
	}
	links := []state.LinkSlot{{RelPath: "svc-b", Target: elsewhere, Type: "symlink"}}

	restore, err := WithdrawBackFill(tree, links)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Lstat(link); statErr == nil {
		t.Fatal("the back-fill link must be gone while the script runs")
	}
	restore()

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("the link must come back: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("it must come back as a symlink, not as a directory")
	}
	if got, readErr := os.Readlink(link); readErr != nil || got != elsewhere {
		t.Fatalf("it must point where it did: %q (%v)", got, readErr)
	}
}

// A script that walks the tree must not be able to reach the workspace. This
// is the measured hazard: writing through a back-filled path lands in the
// developer's own checkout.
func TestAWalkOfTheTreeCannotReachTheWorkspace(t *testing.T) {
	tree, workspaceRepo := t.TempDir(), t.TempDir()
	if err := os.Symlink(workspaceRepo, filepath.Join(tree, "svc-b")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tree, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	links := []state.LinkSlot{{RelPath: "svc-b", Target: workspaceRepo, Type: "symlink"}}

	restore, err := WithdrawBackFill(tree, links)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()

	// The hazard is a shell one and has to be reproduced in a shell: Go's
	// ReadDir reports a symlink with IsDir() false and never follows it, but
	// sh's "*/" glob expands to symlinked directories just the same. This is
	// the loop people write, run the way they run it.
	sh := exec.Command("sh", "-c", `for d in */; do touch "$d/installed"; done`)
	sh.Dir = tree
	_ = sh.Run() // a withdrawn link makes one iteration fail; that is the point
	if _, statErr := os.Stat(filepath.Join(workspaceRepo, "installed")); statErr == nil {
		t.Fatal("a walk of the tree reached the developer's own repository")
	}
	if _, statErr := os.Stat(filepath.Join(tree, "docs", "installed")); statErr != nil {
		t.Fatal("the materialised repository should still have been visited")
	}
}

// Only back-fill slots. A carried .env is a real file the script may
// legitimately read, and withdrawing it would break the thing the carry exists
// to provide.
func TestWithdrawTouchesOnlyBackFillSlots(t *testing.T) {
	tree := t.TempDir()
	carried := filepath.Join(tree, ".env")
	if err := os.WriteFile(carried, []byte("TOKEN=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	links := []state.LinkSlot{{RelPath: ".env", Target: "/somewhere/.env", Type: "carry"}}

	restore, err := WithdrawBackFill(tree, links)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if _, statErr := os.Stat(carried); statErr != nil {
		t.Fatal("a carried file must not be withdrawn")
	}
}

// A slot that no longer holds a symlink is not ours to delete.
func TestASlotThatIsNoLongerALinkIsRefused(t *testing.T) {
	tree := t.TempDir()
	real := filepath.Join(tree, "svc-b")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	links := []state.LinkSlot{{RelPath: "svc-b", Target: "/elsewhere", Type: "symlink"}}

	_, err := WithdrawBackFill(tree, links)
	if err == nil {
		t.Fatal("a slot holding a real directory must be refused, not removed")
	}
	// Refused for the right reason: without this the check could be gone and
	// the failure would merely come from readlink further down.
	if !strings.Contains(err.Error(), "no longer holds a symlink") {
		t.Fatalf("the refusal must name what is wrong; got %v", err)
	}
	if _, statErr := os.Stat(real); statErr != nil {
		t.Fatal("and the directory must still be there")
	}
}
