package perimeter

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// allowedDomains reads the egress allowlist out of a document. The Network
// field is a pointer so that "omitempty" works, so absent and empty are the
// same answer here and a different one in the rendered file: absent means the
// document says nothing about egress, which is what a task with nothing to
// reach must produce.
func allowedDomains(d Document) []string {
	if d.Sandbox.Network == nil {
		return nil
	}
	return d.Sandbox.Network.AllowedDomains
}

func fixture(t *testing.T, siblings ...string) (container.C, state.Task, []string) {
	t.Helper()
	base := t.TempDir()
	ws := filepath.Join(base, "ws")
	if err := os.MkdirAll(filepath.Join(ws, "services", "svc-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := container.Locate(ws)
	if err != nil {
		t.Fatal(err)
	}
	task := state.Task{
		Name:      "feat-42",
		Container: c.Root,
		Workspace: c.Workspace,
		Repos: []state.Repo{{
			RelPath:      "services/svc-a",
			AbsPath:      filepath.Join(c.Workspace, "services", "svc-a"),
			StoreID:      "services-svc-a-deadbeef",
			WorktreePath: filepath.Join(c.TreePath("feat-42"), "services", "svc-a"),
		}},
	}
	return c, task, siblings
}

// TestEveryRuleUsesTheDoubleSlashPrefix is the most important test in this
// file. Verified against Claude Code 2.1.238: "Edit(//Users/x/f)" and
// "Edit(///Users/x/f)" both deny, while "Edit(/Users/x/f)" — one leading
// slash — is accepted by the settings parser and silently does nothing. A
// perimeter spelled that way looks present and protects nothing.
func TestEveryRuleUsesTheDoubleSlashPrefix(t *testing.T) {
	c, task, sib := fixture(t, "other-task")
	d, err := For(c, task, sib)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Permissions.Deny) == 0 {
		t.Fatal("a perimeter with no deny rules is not a perimeter")
	}
	for _, r := range d.Permissions.Deny {
		if !strings.HasPrefix(r, "Edit(//") {
			t.Fatalf("rule must start with Edit(// — a single slash is a silent no-op: %q", r)
		}
		if strings.HasPrefix(r, "Edit(///") && !strings.HasPrefix(r, "Edit(////") {
			continue // "//" + an absolute path is the documented spelling
		}
	}
}

// TestDenyCoversEveryRequiredRegion pins spec §5.6's list. Asserting the
// section as well as the path matters: naming the workspace under sandbox
// instead of permissions would pass a looser test and protect nothing from
// the file-editing tools.
func TestDenyCoversEveryRequiredRegion(t *testing.T) {
	c, task, sib := fixture(t, "other-task")
	d, err := For(c, task, sib)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(d.Permissions.Deny, "\n")
	for _, want := range []string{
		c.Workspace,
		filepath.Join(c.Root, "state"),
		filepath.Join(c.Root, "staging"),
		filepath.Join(c.TreesDir(), "other-task"),
		filepath.Join(c.TreePath("feat-42"), ".claude"),
		filepath.Join(c.TreePath("feat-42"), ".wkt"),
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("deny list must cover %s:\n%s", want, joined)
		}
	}
	if !strings.Contains(joined, "hooks") || !strings.Contains(joined, "config") {
		t.Errorf("the store's hooks/ and config must be denied:\n%s", joined)
	}
}

// TestOwnTreeIsNotDenied guards the mistake H16 exists to describe: deny wins
// over a narrower allow, so a task that denies its own tree cannot carve an
// exception back out — it simply cannot work in it.
func TestOwnTreeIsNotDenied(t *testing.T) {
	// The container lists every task, this one included — state.List does not
	// filter — so the guard is only exercised when the task's own name is in
	// the slice. A fixture that omits it tests nothing.
	c, task, sib := fixture(t, "other-task", "feat-42")
	d, err := For(c, task, sib)
	if err != nil {
		t.Fatal(err)
	}
	own := c.TreePath("feat-42")
	for _, r := range d.Permissions.Deny {
		inner := strings.TrimSuffix(strings.TrimPrefix(r, "Edit(//"), ")")
		inner = strings.TrimSuffix(inner, "/**")
		if inner == own || inner == "/"+strings.TrimPrefix(own, "/") {
			t.Fatalf("a task must not deny its own tree: %q", r)
		}
	}
}

// TestSpellingsAreAllPresent: deny globs are lexical, so an aliased workspace
// defeats a single spelling entirely (spec §5.6).
func TestSpellingsAreAllPresent(t *testing.T) {
	c, task, sib := fixture(t)
	d, err := For(c, task, sib)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(d.Permissions.Deny, "\n")
	for _, sp := range spellingsOf(c.Workspace) {
		if !strings.Contains(joined, sp) {
			t.Errorf("workspace spelling %q missing from the deny list", sp)
		}
	}
}

// TestGrowthIsProportionalToSiblings — the profile is compiled into every Bash
// command, so the shape of this growth is a cost model, not a detail.
func TestGrowthIsProportionalToSiblings(t *testing.T) {
	c, task, _ := fixture(t)
	one, err := For(c, task, []string{"s1"})
	if err != nil {
		t.Fatal(err)
	}
	forty := make([]string, 40)
	for i := range forty {
		forty[i] = "s" + string(rune('a'+i%26)) + string(rune('0'+i/26))
	}
	many, err := For(c, task, forty)
	if err != nil {
		t.Fatal(err)
	}
	perSibling := len(spellingsOf(c.TreesDir()))
	grew := len(many.Permissions.Deny) - len(one.Permissions.Deny)
	if want := 39 * perSibling; grew != want {
		t.Fatalf("39 more siblings added %d rules, want %d (%d spellings each)", grew, want, perSibling)
	}
}

// TestRefusesAnUnboundedList — measured on 2.1.238: past roughly 9,000 paths
// the sandbox profile stops compiling and *every* Bash command in the session
// fails. Refusing loudly beats generating a file that breaks the tool.
func TestRefusesAnUnboundedList(t *testing.T) {
	c, task, _ := fixture(t)
	huge := make([]string, 5000)
	for i := range huge {
		huge[i] = "task-" + strings.Repeat("x", 3) + string(rune(i))
	}
	_, err := For(c, task, huge)
	if err == nil {
		t.Fatal("a perimeter that would break every Bash command must be refused")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_PERIMETER_TOO_LARGE" {
		t.Fatalf("want WKT_PERIMETER_TOO_LARGE, got %v", err)
	}
}

// TestRenderIsDeterministic — an unstable order makes every regeneration look
// like drift to the hash check that reports coverage.
func TestRenderIsDeterministic(t *testing.T) {
	c, task, _ := fixture(t)
	a, err := For(c, task, []string{"b-task", "a-task", "c-task"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := For(c, task, []string{"c-task", "a-task", "b-task"})
	if err != nil {
		t.Fatal(err)
	}
	ra, err := Render(a)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := Render(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ra) != string(rb) {
		t.Fatal("the same task must render byte-identically whatever the sibling order")
	}
	if !json.Valid(ra) {
		t.Fatal("the rendered perimeter must be valid JSON")
	}
}

// TestStoreStaysWritable — the task's gitdir lives in the store, and denying
// it breaks git add (H5). This is the one mandatory allow.
func TestStoreStaysWritable(t *testing.T) {
	c, task, _ := fixture(t)
	d, err := For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Sandbox.Enabled {
		t.Fatal("the sandbox must be switched on")
	}
	joined := strings.Join(d.Sandbox.Filesystem.AllowWrite, "\n")
	if !strings.Contains(joined, c.StoreDir()) {
		t.Fatalf("the store must be writable, got %v", d.Sandbox.Filesystem.AllowWrite)
	}
	for _, want := range []string{".ssh", ".aws", ".claude"} {
		if !strings.Contains(strings.Join(d.Sandbox.Filesystem.DenyRead, "\n"), want) {
			t.Errorf("credential directory %s must be unreadable", want)
		}
	}
}

// TestDenyPathsAreNotDuplicatedUnderSandbox — Task 1 verified that Edit(...)
// deny rules are merged into the Bash profile already. Restating them doubles
// the profile every command carries, for nothing.
func TestDenyPathsAreNotDuplicatedUnderSandbox(t *testing.T) {
	c, task, _ := fixture(t)
	d, err := For(c, task, []string{"other"})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Sandbox.Filesystem.DenyWrite) != 0 {
		t.Fatalf("deny paths are merged from the Edit rules; restating them is waste: %v",
			d.Sandbox.Filesystem.DenyWrite)
	}
}

// TestToolchainCachesStayWritable — the perimeter turns Claude Code's sandbox
// on, which confines writes to the working directory. Every toolchain keeps
// its cache outside that: measured, an ordinary "go build" inside a task tree
// failed with
//
//	open ~/Library/Caches/go-build/…: operation not permitted
//
// A tree that cannot be built in is a tree nobody will work in, and the answer
// people reach for is to delete the perimeter entirely — which loses the
// protection it exists for. The caches are allowed instead.
func TestToolchainCachesStayWritable(t *testing.T) {
	c, task, _ := fixture(t)
	d, err := For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed := strings.Join(d.Sandbox.Filesystem.AllowWrite, "\n")

	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	want := []string{filepath.Join(home, ".cache")}
	if runtime.GOOS == "darwin" {
		want = append(want, filepath.Join(home, "Library", "Caches"))
	}
	// Go's module cache is not under either cache root.
	want = append(want, filepath.Join(home, "go", "pkg", "mod"))
	for _, p := range want {
		if !strings.Contains(allowed, p) {
			t.Errorf("a build needs %s writable; allowWrite is:\n%s", p, allowed)
		}
	}

	// The store stays writable — the task's gitdir lives there (H5).
	if !strings.Contains(allowed, c.StoreDir()) {
		t.Errorf("the store must stay writable: %v", d.Sandbox.Filesystem.AllowWrite)
	}
	// And none of this opens the workspace, which is the whole point. Compare
	// by path, not by substring: the store lives at "<workspace>.worktrees",
	// which contains the workspace path as a prefix of its own name.
	for _, p := range d.Sandbox.Filesystem.AllowWrite {
		if p == c.Workspace || strings.HasPrefix(p, c.Workspace+string(filepath.Separator)) {
			t.Errorf("the workspace must not become writable: %s", p)
		}
	}
}

// TestCacheOverridesFromTheEnvironmentAreHonoured — a developer who moved
// their cache with GOCACHE or XDG_CACHE_HOME has moved it somewhere the
// defaults do not name.
func TestCacheOverridesFromTheEnvironmentAreHonoured(t *testing.T) {
	moved := t.TempDir()
	t.Setenv("GOCACHE", filepath.Join(moved, "go-build"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(moved, "xdg"))

	c, task, _ := fixture(t)
	d, err := For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	allowed := strings.Join(d.Sandbox.Filesystem.AllowWrite, "\n")
	for _, p := range []string{filepath.Join(moved, "go-build"), filepath.Join(moved, "xdg")} {
		if !strings.Contains(allowed, p) {
			t.Errorf("a cache moved by the environment must still be writable: %s not in\n%s", p, allowed)
		}
	}
}

// TestOriginHostsAreReachable — the perimeter switches the sandbox on, which
// routes egress through a proxy that refuses anything not on the allowlist.
// Measured inside a covered task tree:
//
//	git ls-remote origin → fatal: CONNECT tunnel failed, response 403
//
// So a task could neither fetch nor push: the tool that exists to carry work
// across repositories could not reach the repositories. The hosts the task's
// own repositories point at are allowed — those and nothing else.
func TestOriginHostsAreReachable(t *testing.T) {
	c, task, _ := fixture(t)
	// Two repositories, two different forge hosts, plus one with no remote.
	setOrigin(t, filepath.Join(c.Workspace, "services", "svc-a"), "https://github.com/acme/svc-a.git")
	task.Repos = append(task.Repos, state.Repo{
		RelPath: "docs", AbsPath: filepath.Join(c.Workspace, "docs"), StoreID: "docs-1234abcd",
	})
	seedBareRepo(t, filepath.Join(c.Workspace, "docs"))
	setOrigin(t, filepath.Join(c.Workspace, "docs"), "git@gitlab.example.com:team/docs.git")

	d, err := For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(allowedDomains(d), ",")
	for _, want := range []string{"github.com", "gitlab.example.com"} {
		if !strings.Contains(got, want) {
			t.Errorf("a task must be able to reach %s; allowed: %q", want, got)
		}
	}
	if strings.Contains(got, "acme") || strings.Contains(got, "team") {
		t.Errorf("only the host, never the path: %q", got)
	}
}

// TestNoOriginMeansNoNetwork — a repository with no upstream, or one whose
// remote is a local path, opens nothing. The allowlist is derived, never
// broadened "just in case".
func TestNoOriginMeansNoNetwork(t *testing.T) {
	c, task, _ := fixture(t) // fixture repositories have no origin
	d, err := For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowedDomains(d)) != 0 {
		t.Fatalf("nothing to reach, so nothing should be opened: %v", allowedDomains(d))
	}

	setOrigin(t, filepath.Join(c.Workspace, "services", "svc-a"), filepath.Join(c.Workspace, "mirror.git"))
	d, err = For(c, task, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(allowedDomains(d)) != 0 {
		t.Fatalf("a local-path remote is not a host: %v", allowedDomains(d))
	}
}

// TestHostFromRemoteURL covers the spellings a remote actually comes in.
func TestHostFromRemoteURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/acme/repo.git":            "github.com",
		"https://user:token@github.com/acme/repo.git": "github.com",
		"http://internal.example:8080/repo.git":       "internal.example",
		"ssh://git@ssh.github.com:443/acme/repo.git":  "ssh.github.com",
		"git@github.com:acme/repo.git":                "github.com",
		"gitlab.example.com:team/docs.git":            "gitlab.example.com",
		"/srv/mirrors/repo.git":                       "",
		"file:///srv/mirrors/repo.git":                "",
		// A file:// URL may carry a host and still be local. Opening a
		// network domain for it would be wrong, and it is the case that
		// distinguishes the local-path guard from the parsing below it.
		"file://nas.local/srv/mirrors/repo.git": "",
		"../sibling.git":                        "",
		"":                                      "",
	} {
		if got := hostFromRemoteURL(in); got != want {
			t.Errorf("hostFromRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// setOrigin gives a fixture directory a remote. The fixture only makes
// directories, so it is initialised here — the perimeter reads the remote with
// git, and a directory that is not a repository has nothing to read.
func setOrigin(t *testing.T, dir, url string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); os.IsNotExist(statErr) {
		run(t, dir, "init", "-q")
	}
	run(t, dir, "remote", "add", "origin", url)
}

func seedBareRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "init", "-q")
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %s", args, dir, out)
	}
}

// A repository with no origin, or one whose remote is a local path, opens
// nothing — which has to mean no network block at all, not an empty one.
// "omitempty" does nothing on a struct field, so the document carried
// "network": {} for every such task: a present but empty allowlist, which is
// the shape that made "git ls-remote" fail with CONNECT 403 in the first
// place. The field is a pointer so the tag can do what it says.
func TestNoOriginMeansNoNetworkBlockAtAll(t *testing.T) {
	c, tk, siblings := fixture(t)
	doc, err := For(c, tk, siblings)
	if err != nil {
		t.Fatal(err)
	}
	body, err := Render(doc)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Sandbox map[string]json.RawMessage `json:"sandbox"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatal(err)
	}
	if _, present := probe.Sandbox["network"]; present {
		t.Fatalf("a task that opens nothing must carry no network key; document was:\n%s", body)
	}
}

// TestSSHOriginsNamesWhatCannotBeReachedFromInsideATree — measured on Claude
// Code 2.1.239 inside a covered tree: "git ls-remote git@github.com:..."
// fails with "This proxy requires authentication, and this client did not
// offer an authentication method", while the same repository over HTTPS
// succeeds and a host off the allowlist gets CONNECT 403. The allowlist is an
// HTTP proxy; SSH is routed through it and cannot authenticate to it, so no
// allowlist entry helps. Naming those repositories is all wkt can do.
func TestSSHOriginsNamesWhatCannotBeReachedFromInsideATree(t *testing.T) {
	for _, tc := range []struct {
		url string
		ssh bool
	}{
		{"git@github.com:Venut-Technologies/wkt.git", true},
		{"ssh://git@github.com/Venut-Technologies/wkt.git", true},
		{"https://github.com/Venut-Technologies/wkt.git", false},
		{"http://example.com/x.git", false},
		{"/srv/mirrors/x.git", false},
		{"file:///srv/mirrors/x.git", false},
		{"", false},
	} {
		if got := isSSHRemote(tc.url); got != tc.ssh {
			t.Fatalf("isSSHRemote(%q) = %v, want %v", tc.url, got, tc.ssh)
		}
	}
}
