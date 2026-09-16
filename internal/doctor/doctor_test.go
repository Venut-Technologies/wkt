package doctor

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/discover"
	"github.com/Venut-Technologies/wkt/internal/task"
)

func g(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
	return string(out)
}

func seed(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, dir, "init", "-q")
	g(t, dir, "add", "-A")
	g(t, dir, "commit", "-qm", "init")
}

// fixture gives a container with one healthy task.
func fixture(t *testing.T) (container.C, []discover.Entry) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	seed(t, filepath.Join(ws, "svc-a"))
	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := container.Create(c); err != nil {
		t.Fatal(err)
	}
	entries, err := discover.Walk(ws, 4)
	if err != nil {
		t.Fatal(err)
	}
	return c, entries
}

func codes(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Code)
	}
	return out
}

func has(fs []Finding, code string) bool {
	for _, f := range fs {
		if f.Code == code {
			return true
		}
	}
	return false
}

// TestHealthyContainerReportsNoProblems — a doctor that always finds
// something teaches people to ignore it. Informational entries (the pins wkt
// is holding on purpose) are still returned: they are the uninstall
// inventory, and the CLI decides when to show them.
func TestHealthyContainerReportsNoProblems(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "t1", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		if !f.Info {
			t.Fatalf("a healthy container must report no problems, got %v", codes(findings))
		}
	}
	if len(findings) == 0 {
		t.Fatal("the inventory of what wkt wrote must still be available")
	}
}

// TestFindsATreeWithNoTask — the debris finding F1 produced: a directory in
// trees/ that no state file claims. It blocks the task name forever and
// nothing else reports it.
func TestFindsATreeWithNoTask(t *testing.T) {
	c, _ := fixture(t)
	orphan := filepath.Join(c.TreesDir(), "leftover")
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if !has(findings, "WKT_ORPHAN_TREE") {
		t.Fatalf("want WKT_ORPHAN_TREE, got %v", codes(findings))
	}
}

// TestFindsATaskWithNoTree — the mirror case: state says a task exists and
// the disk disagrees.
func TestFindsATaskWithNoTree(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "t1", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(c.TreePath("t1")); err != nil {
		t.Fatal(err)
	}
	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if !has(findings, "WKT_MISSING_TREE") {
		t.Fatalf("want WKT_MISSING_TREE, got %v", codes(findings))
	}
}

// TestReportsEveryRefWktInTheWorkspace — this is the uninstall path. A user
// deciding whether to keep the tool needs a complete answer to "what has it
// put in my repositories", and base pins are the only thing wkt writes there.
func TestReportsEveryRefWktInTheWorkspace(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "t1", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	// A pin belonging to a live task is reported for information, never as a
	// problem: it is doing its job.
	var pin *Finding
	for i := range findings {
		if findings[i].Code == "WKT_WORKSPACE_REF" {
			pin = &findings[i]
		}
	}
	if pin == nil {
		t.Fatalf("doctor must list the refs wkt wrote into the workspace, got %v", codes(findings))
	}
	if !pin.Info {
		t.Fatal("a pin held by a live task is information, not a problem")
	}
	if !strings.Contains(pin.Detail, "refs/wkt/base/t1") {
		t.Fatalf("the ref must be named exactly: %+v", pin)
	}
}

// TestFindsAStrayBasePin — a pin left in the user's repository by a task that
// no longer exists. This one is a problem: it pins objects forever.
func TestFindsAStrayBasePin(t *testing.T) {
	c, entries := fixture(t)
	repo := filepath.Join(c.Workspace, "svc-a")
	sha := strings.TrimSpace(g(t, repo, "rev-parse", "HEAD"))
	g(t, repo, "update-ref", "refs/wkt/base/ghost", sha)

	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if !has(findings, "WKT_STRAY_PIN") {
		t.Fatalf("want WKT_STRAY_PIN, got %v", codes(findings))
	}
	_ = entries
}

// TestFixRemovesOnlyWhatIsUnambiguous — an empty leftover directory and a
// stray pin, and nothing else.
func TestFixRemovesOnlyWhatIsUnambiguous(t *testing.T) {
	c, _ := fixture(t)
	repo := filepath.Join(c.Workspace, "svc-a")
	sha := strings.TrimSpace(g(t, repo, "rev-parse", "HEAD"))
	g(t, repo, "update-ref", "refs/wkt/base/ghost", sha)
	empty := filepath.Join(c.TreesDir(), "empty-leftover")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := Run(c, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(empty); !os.IsNotExist(err) {
		t.Fatal("--fix must remove an empty leftover tree directory")
	}
	if out := g(t, repo, "for-each-ref", "--format=%(refname)", "refs/wkt/"); strings.TrimSpace(out) != "" {
		t.Fatalf("--fix must delete a stray pin, still there: %q", out)
	}
}

// TestFixNeverRemovesANonEmptyLeftover is the line --fix must not cross. A
// half-created tree looks exactly like debris and may hold the only copy of
// someone's work; removing it is rm's job, with rm's refusals.
func TestFixNeverRemovesANonEmptyLeftover(t *testing.T) {
	c, _ := fixture(t)
	leftover := filepath.Join(c.TreesDir(), "not-empty")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	precious := filepath.Join(leftover, "notes.md")
	if err := os.WriteFile(precious, []byte("the only copy\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(c, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(precious); err != nil {
		t.Fatal("--fix removed a leftover that had content in it")
	}
}

// TestFixNeverFollowsALinkOutOfTheContainer — the same H3 rule the teardown
// follows: --fix is the second thing in this codebase that deletes.
func TestFixNeverFollowsALinkOutOfTheContainer(t *testing.T) {
	c, _ := fixture(t)
	outside := filepath.Join(c.Workspace, "svc-a")
	link := filepath.Join(c.TreesDir(), "a-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(c, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "f.txt")); err != nil {
		t.Fatal("--fix followed a symlink out of the container and deleted through it")
	}
	// And the link itself stays: wkt did not put it there, so removing it
	// silently is not doctor's call either. It is reported and left alone.
	if _, err := os.Lstat(link); err != nil {
		t.Fatal("--fix removed a symlink it did not create; it should report it instead")
	}
	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, f := range findings {
		if f.Code == "WKT_ORPHAN_TREE" && strings.Contains(f.Path, "a-link") {
			reported = true
			if !strings.Contains(f.Detail, "symlink") {
				t.Fatalf("the finding must say it is a symlink: %+v", f)
			}
		}
	}
	if !reported {
		t.Fatalf("a symlink in trees/ must be reported, got %v", codes(findings))
	}
}

// TestFindsAnUnfinishedStore — doctor returned rc=0 on a store that still
// borrowed objects from the developer's own repository and had live hooks.
// That is the state where the task looks healthy right up until the
// repository is re-cloned and every commit in the tree becomes unreadable, so
// it is exactly what doctor is for.
func TestFindsAnUnfinishedStore(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "t1", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	stores, err := os.ReadDir(c.StoreDir())
	if err != nil || len(stores) == 0 {
		t.Fatalf("precondition: a store should exist: %v", err)
	}
	sp := filepath.Join(c.StoreDir(), stores[0].Name())

	// Undo the hardening, the way an interrupted build leaves it.
	g(t, sp, "config", "--unset-all", "wkt.storecomplete")
	g(t, sp, "config", "--unset-all", "core.hooksPath")
	if err := os.MkdirAll(filepath.Join(sp, "objects", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sp, "objects", "info", "alternates"),
		[]byte(filepath.Join(c.Workspace, "svc-a", ".git", "objects")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	findings, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if !has(findings, "WKT_STORE_INCOMPLETE") {
		t.Fatalf("doctor must report a store that is not finished, got %v", codes(findings))
	}
	for _, f := range findings {
		if f.Code == "WKT_STORE_INCOMPLETE" && f.Info {
			t.Fatal("an unfinished store is a problem, not an informational note")
		}
	}
}

// TestFixNeverTouchesAnUnfinishedStore — doctor reports it and stops. The
// store may be the only copy of a task's unpushed commits, and deleting or
// rebuilding it turns a loud, recoverable problem into a silent, lossy one.
func TestFixNeverTouchesAnUnfinishedStore(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "t1", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	stores, _ := os.ReadDir(c.StoreDir())
	sp := filepath.Join(c.StoreDir(), stores[0].Name())
	g(t, sp, "config", "--unset-all", "wkt.storecomplete")
	g(t, sp, "config", "--unset-all", "core.hooksPath")

	before, err := os.ReadDir(sp)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(c, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(sp)
	if err != nil {
		t.Fatal("--fix removed a store it was only meant to report")
	}
	if len(after) != len(before) {
		t.Fatalf("--fix changed the store's contents: %d entries, was %d", len(after), len(before))
	}
}

// doctor is the command a WKT_STORE_INCOMPLETE refusal tells you to run, so
// the two must agree on what an unfinished store is. They did not: store's
// own check also flags an origin still pointing at the developer's clone,
// doctor's copy did not, so the one store wkt new refuses was reported
// healthy by the one command that exists to explain it.
func TestDoctorAgreesWithTheStoreOnWhatIsIncomplete(t *testing.T) {
	c, entries := fixture(t)
	if _, err := task.Create(c, entries, "feat-store", []string{"svc-a"}); err != nil {
		t.Fatal(err)
	}
	stores, err := os.ReadDir(c.StoreDir())
	if err != nil || len(stores) == 0 {
		t.Fatalf("expected a store: %v", err)
	}
	sp := filepath.Join(c.StoreDir(), stores[0].Name())

	// Complete in every other way, and unmarked, so only this can be the
	// reason: origin points back at the workspace repository.
	g(t, sp, "config", "--unset", "wkt.storecomplete")
	g(t, sp, "config", "remote.origin.url", filepath.Join(c.Workspace, "svc-a"))

	found, err := Run(c, false)
	if err != nil {
		t.Fatal(err)
	}
	if !has(found, "WKT_STORE_INCOMPLETE") {
		t.Fatalf("doctor must report the store wkt refuses; it reported %v", codes(found))
	}
}
