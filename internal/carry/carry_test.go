package carry

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/state"
)

func g(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=e@x", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// workspace builds a workspace with one repository that has both gitignored
// and tracked files, including a file inside a wholesale-ignored directory.
func workspace(t *testing.T) (ws string, repos []state.Repo) {
	t.Helper()
	ws = t.TempDir()
	repo := filepath.Join(ws, "services", "svc-a")
	write(t, filepath.Join(repo, ".gitignore"), "config/\n.env\n*.pem\n")
	write(t, filepath.Join(repo, "app.js"), "code\n")
	g(t, repo, "init", "-q")
	g(t, repo, "add", "-A")
	g(t, repo, "commit", "-qm", "init")

	write(t, filepath.Join(repo, ".env"), "TOKEN=local\n")
	write(t, filepath.Join(repo, "config", "secrets.json"), "{\"k\":1}\n")
	write(t, filepath.Join(repo, "config", "other.json"), "{}\n")
	write(t, filepath.Join(repo, "key.pem"), "PRIVATE\n")
	write(t, filepath.Join(repo, "notes.txt"), "untracked but not ignored\n")

	return ws, []state.Repo{{RelPath: "services/svc-a", AbsPath: repo}}
}

// TestCarriesOnlyWhatIsBothMatchedAndIgnored — the rule from the report, and
// the one that keeps the mechanism from quietly shadowing versioned content:
// a file must match .wktinclude *and* be gitignored.
func TestCarriesOnlyWhatIsBothMatchedAndIgnored(t *testing.T) {
	ws, repos := workspace(t)
	write(t, filepath.Join(ws, ".wktinclude"), ".env\nconfig/secrets.json\napp.js\nnotes.txt\n")

	plan, err := Plan(ws, repos)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan, "\n")
	for _, want := range []string{"services/svc-a/.env", "services/svc-a/config/secrets.json"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s is gitignored and matched; it must be carried. plan:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{"app.js", "notes.txt", "other.json", "key.pem"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s must not be carried (tracked, or not matched). plan:\n%s", unwanted, got)
		}
	}
}

// TestFindsAFileInsideAWhollyIgnoredDirectory — the trap this project has hit
// before: "git ls-files --others --ignored --directory" collapses config/ to a
// single entry and hides secrets.json inside it. Enumerating that way would
// silently carry nothing.
func TestFindsAFileInsideAWhollyIgnoredDirectory(t *testing.T) {
	ws, repos := workspace(t)
	write(t, filepath.Join(ws, ".wktinclude"), "config/secrets.json\n")

	plan, err := Plan(ws, repos)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.HasSuffix(plan[0], "config/secrets.json") {
		t.Fatalf("want the file inside the ignored directory, got %v", plan)
	}
}

// TestPatternsAreGitignoreSyntax — the patterns are handed to git itself, so
// the syntax is git's rather than an approximation of it.
func TestPatternsAreGitignoreSyntax(t *testing.T) {
	ws, repos := workspace(t)
	write(t, filepath.Join(ws, ".wktinclude"), "*.pem\nservices/*/config/*.json\n!**/other.json\n")

	plan, err := Plan(ws, repos)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(plan, "\n")
	if !strings.Contains(got, "key.pem") {
		t.Errorf("a glob must work: %v", plan)
	}
	if !strings.Contains(got, "secrets.json") {
		t.Errorf("a path glob must work: %v", plan)
	}
	if strings.Contains(got, "other.json") {
		t.Errorf("a negation must work: %v", plan)
	}
}

// TestNoIncludeFileCarriesNothing — the feature is opt-in, and its absence
// must cost nothing.
func TestNoIncludeFileCarriesNothing(t *testing.T) {
	ws, repos := workspace(t)
	plan, err := Plan(ws, repos)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("no .wktinclude means no carry, got %v", plan)
	}
}

// TestApplyCopiesRatherThanLinks — a symlinked secret edited inside a task
// writes back to the developer's checkout, which is the isolation this tool
// exists to provide. It also has to keep the mode: a carried key that loses
// its 0600 is a different kind of problem.
func TestApplyCopiesRatherThanLinks(t *testing.T) {
	ws, repos := workspace(t)
	write(t, filepath.Join(ws, ".wktinclude"), ".env\n")
	if err := os.Chmod(filepath.Join(ws, "services", "svc-a", ".env"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Plan(ws, repos)
	if err != nil {
		t.Fatal(err)
	}

	tree := filepath.Join(t.TempDir(), "tree")
	if err := os.MkdirAll(filepath.Join(tree, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	carried, err := Apply(tree, ws, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(carried) != 1 {
		t.Fatalf("want one carried file, got %v", carried)
	}

	dst := filepath.Join(tree, "services", "svc-a", ".env")
	info, err := os.Lstat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("carried files must be copies: editing a symlinked secret writes into the original checkout")
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the mode must survive the carry, got %v", info.Mode().Perm())
	}
	body, err := os.ReadFile(dst)
	if err != nil || string(body) != "TOKEN=local\n" {
		t.Fatalf("contents: %q %v", body, err)
	}
	if carried[0].Hash == "" {
		t.Fatal("the hash must be recorded, so teardown can tell an edited secret from an untouched copy")
	}
	// Editing the copy must not touch the original.
	if err := os.WriteFile(dst, []byte("TOKEN=changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.ReadFile(filepath.Join(ws, "services", "svc-a", ".env"))
	if string(orig) != "TOKEN=local\n" {
		t.Fatalf("the developer's own file was modified: %q", orig)
	}
}
