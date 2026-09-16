package store

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
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

func seedRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src", "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, dir, "init", "-q")
	g(t, dir, "add", "-A")
	g(t, dir, "commit", "-qm", "init")
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func TestEnsureProducesUsableStoreThatSurvivesSourceDeletion(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws", "services", "svc-a")
	sha := seedRepo(t, ws)
	storeDir := filepath.Join(base, "container", "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sp, err := Ensure(storeDir, ws, "services/svc-a", "feat-42", sha)
	if err != nil {
		t.Fatal(err)
	}
	if !HasObject(sp, sha) {
		t.Fatal("the store must contain the base commit")
	}
	// The pin must exist in the WORKSPACE repository, written before the clone.
	if out := g(t, ws, "rev-parse", "--verify", "refs/wkt/base/feat-42"); len(out) < 40 {
		t.Fatalf("base pin missing in the workspace repo: %q", out)
	}
	// De-borrowed: no alternates file, and the object survives losing the source.
	if _, err := os.Stat(filepath.Join(sp, "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("a de-borrowed store must have no alternates file")
	}
	if err := os.RemoveAll(filepath.Join(base, "ws")); err != nil {
		t.Fatal(err)
	}
	if !HasObject(sp, sha) {
		t.Fatal("the store must survive deletion of the workspace repository")
	}
}

func TestEnsureConfiguresFetchRefspecAndWorkspaceRemote(t *testing.T) {
	base := t.TempDir()
	ws := filepath.Join(base, "ws", "svc-a")
	sha := seedRepo(t, ws)
	// Give the workspace a real origin: with no origin, Ensure legitimately
	// drops the "origin" remote entirely (untidy otherwise), so the refspec
	// assertion below needs an origin present to mean anything.
	origin := filepath.Join(base, "origin.git")
	g(t, base, "init", "--bare", "-q", origin)
	g(t, ws, "remote", "add", "origin", origin)
	g(t, ws, "push", "-q", "origin", "main")
	storeDir := filepath.Join(base, "c", "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, ws, "svc-a", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	refspec := g(t, sp, "config", "--get", "remote.origin.fetch")
	if len(refspec) == 0 {
		t.Fatal("bare clones set no refspec; Ensure must add one (spec H15)")
	}
	if out := g(t, sp, "remote"); !containsLine(out, "workspace") {
		t.Fatalf("the workspace remote is missing: %q", out)
	}
	// A commit made locally in the workspace and never pushed must become reachable.
	if err := os.WriteFile(filepath.Join(ws, "src", "a.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, ws, "add", "-A")
	g(t, ws, "commit", "-qm", "local only")
	local := g(t, ws, "rev-parse", "HEAD")[:40]
	if HasObject(sp, local) {
		t.Fatal("precondition: the store should not have the new commit yet")
	}
	if err := FetchWorkspace(sp); err != nil {
		t.Fatal(err)
	}
	if !HasObject(sp, local) {
		t.Fatal("after FetchWorkspace the local-only commit must be reachable (spec §5.2)")
	}
}

func TestEnsureRepointsOriginAndFetchesFromRealUpstream(t *testing.T) {
	base := t.TempDir()
	upstream := filepath.Join(base, "upstream.git")
	g(t, base, "init", "--bare", "-q", upstream)

	ws := filepath.Join(base, "ws", "svc-a")
	sha := seedRepo(t, ws)
	g(t, ws, "remote", "add", "origin", upstream)
	wantURL := strings.TrimSpace(g(t, ws, "config", "--get", "remote.origin.url"))
	g(t, ws, "push", "-q", "origin", "main")

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sp, err := Ensure(storeDir, ws, "svc-a", "t-origin", sha)
	if err != nil {
		t.Fatal(err)
	}

	// The store must point origin at the repository's real upstream, not at
	// the workspace it was cloned from (the whole reason Ensure touches
	// remotes at all).
	gotURL := strings.TrimSpace(g(t, sp, "config", "--get", "remote.origin.url"))
	if gotURL != wantURL {
		t.Fatalf("store remote.origin.url = %q, want %q (the real upstream)", gotURL, wantURL)
	}

	// Push a commit to the upstream from a THIRD clone -- never through the
	// workspace repo -- to prove the URL and the refspec work together.
	elsewhere := filepath.Join(base, "elsewhere")
	g(t, base, "clone", "-q", upstream, elsewhere)
	if err := os.WriteFile(filepath.Join(elsewhere, "src", "a.txt"), []byte("v3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g(t, elsewhere, "-c", "user.email=e@x", "-c", "user.name=t", "add", "-A")
	g(t, elsewhere, "commit", "-qm", "pushed upstream")
	g(t, elsewhere, "push", "-q", "origin", "main")
	upstreamOnly := strings.TrimSpace(g(t, elsewhere, "rev-parse", "HEAD"))

	if HasObject(sp, upstreamOnly) {
		t.Fatal("precondition: the store should not have the upstream-only commit yet")
	}
	g(t, sp, "fetch", "-q", "origin")
	if !HasObject(sp, upstreamOnly) {
		t.Fatal("after fetching origin the upstream-only commit must be reachable")
	}
	if out := g(t, sp, "rev-parse", "--verify", "refs/remotes/origin/main"); len(strings.TrimSpace(out)) < 40 {
		t.Fatalf("origin fetch must land in refs/remotes/origin/*: %q", out)
	}
}

func containsLine(haystack, want string) bool {
	for _, line := range splitLines(haystack) {
		if line == want {
			return true
		}
	}
	return false
}

func splitLines(s string) []string {
	var out, cur = []string{}, ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// TestEnsureRefusesAHalfBuiltStore covers a defect that was silent for days
// and then destroyed work.
//
// A store build interrupted after "git clone" but before hardening leaves a
// directory that looks finished. Ensure tested only that the directory
// existed, so the next run adopted it: the tree kept borrowing objects from
// the developer's own repository (so a later gc or re-clone made every commit
// in the task unreadable), origin still pointed at their clone, there was no
// workspace remote, and the store's hooks were live — a planted pre-commit
// hook was observed executing inside the task tree.
//
// Ctrl-C during "wkt new" is all it takes.
func TestEnsureRefusesAHalfBuiltStore(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	// Exactly what an interrupted Ensure leaves behind: the clone, nothing else.
	sp := filepath.Join(storeDir, ID("repo", repo)+".git")
	g(t, storeDir, "clone", "--shared", "--bare", "-q", repo, sp)
	planted := filepath.Join(sp, "hooks", "pre-commit")
	if err := os.WriteFile(planted, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err == nil {
		t.Fatal("a half-built store must never be adopted as finished")
	}
	var e *wkterr.E
	if !errors.As(err, &e) || e.Code != "WKT_STORE_INCOMPLETE" {
		t.Fatalf("want WKT_STORE_INCOMPLETE, got %v", err)
	}
	// The refusal has to say what is wrong, or the person cannot judge whether
	// the store holds work they still need.
	var saidBorrow, saidHooks bool
	for _, p := range e.Problems {
		if strings.Contains(p.Detail, "borrow") {
			saidBorrow = true
		}
		if strings.Contains(p.Detail, "hooks") {
			saidHooks = true
		}
	}
	if !saidBorrow || !saidHooks {
		t.Fatalf("the refusal must name each broken invariant: %+v", e.Problems)
	}
	if len(e.Remedy) == 0 {
		t.Fatalf("and say what to do about it: %+v", e)
	}

	// And it is left exactly as found: the store may be the only copy of a
	// previous task's unpushed commits, so wkt never deletes or rebuilds it.
	if _, statErr := os.Stat(sp); statErr != nil {
		t.Fatal("the refused store must not be deleted")
	}
	if _, statErr := os.Stat(planted); statErr != nil {
		t.Fatal("nothing inside the store may be touched either")
	}
}

// TestEnsureAdoptsAStoreFromAnEarlierVersion is the upgrade path, and the
// reason the check verifies invariants instead of demanding a marker: every
// store built before this change is complete and carries no marker, so a rule
// of "no marker means rebuild" would condemn the entire installed base — and
// rebuilding is itself destructive.
func TestEnsureAdoptsAStoreFromAnEarlierVersion(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	// Make it look like a store an older wkt built: complete, unmarked.
	g(t, sp, "config", "--unset-all", "wkt.storecomplete")

	again, err := Ensure(storeDir, repo, "repo", "t2", sha)
	if err != nil {
		t.Fatalf("a complete store from an earlier version must be adopted: %v", err)
	}
	if again != sp {
		t.Fatalf("adopted %s, want %s", again, sp)
	}
	// Having verified it, wkt stamps it, so the next run is a single lookup.
	if out := strings.TrimSpace(g(t, sp, "config", "--get", "wkt.storecomplete")); out == "" {
		t.Fatal("a verified store must be stamped")
	}
}

// TestEnsureStampsOnlyAfterHardening — the marker means "every invariant in
// spec §5.2 holds", so writing it before they do would make the check a lie.
func TestEnsureStampsOnlyAfterHardening(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []struct{ what, got string }{
		{"gc.auto", strings.TrimSpace(g(t, sp, "config", "--get", "gc.auto"))},
		{"core.hooksPath", strings.TrimSpace(g(t, sp, "config", "--get", "core.hooksPath"))},
		{"workspace remote", strings.TrimSpace(g(t, sp, "config", "--get", "remote.workspace.url"))},
		{"marker", strings.TrimSpace(g(t, sp, "config", "--get", "wkt.storecomplete"))},
	} {
		if check.got == "" {
			t.Errorf("%s must be set on a finished store", check.what)
		}
	}
	if _, statErr := os.Stat(filepath.Join(sp, "objects", "info", "alternates")); !os.IsNotExist(statErr) {
		t.Error("a finished store must not borrow objects")
	}
}

// TestStoreCarriesTheRepositorysIdentity — the defect this fixes: commits made
// in a task tree were authored with the *global* identity even when the
// workspace repository resolved a different one, because a bare clone copies
// no config and the store sits outside the workspace, where neither
// .git/config nor an includeIf can reach it. Silent until a CI identity check
// or a DCO bot rejects the push.
func TestStoreCarriesTheRepositorysIdentity(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	g(t, repo, "config", "user.email", "work@company.com")
	g(t, repo, "config", "user.name", "Work Identity")

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"user.email": "work@company.com",
		"user.name":  "Work Identity",
	} {
		if got := cfg(t, sp, key); got != want {
			t.Errorf("store resolves %s = %q, the repository resolves %q", key, got, want)
		}
	}
}

// TestStoreWritesNoOverrideItDoesNotNeed — a developer with only a global
// identity should end up with a store that has no local identity at all.
// Copying it anyway would freeze today's value into every store and quietly
// outlive the developer changing it.
func TestStoreWritesNoOverrideItDoesNotNeed(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	globalCfg := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[user]\n\temail = only-global@example.com\n\tname = Only Global\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	local := localList(t, sp)
	if strings.Contains(local, "user.email") {
		t.Fatalf("the store pinned an identity it did not need:\n%s", local)
	}
	// And it still resolves the right one, through the ambient config.
	if got := cfg(t, sp, "user.email"); got != "only-global@example.com" {
		t.Fatalf("store resolves %q", got)
	}
}

// TestStoreReleasesAnOverrideTheRepositoryDropped — the bridge has to work in
// both directions, or a repository-specific identity set once becomes
// permanent in the store long after the developer removed it.
func TestStoreReleasesAnOverrideTheRepositoryDropped(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	globalCfg := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(globalCfg, []byte("[user]\n\temail = global@example.com\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	g(t, repo, "config", "user.email", "temporary@work.com")

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg(t, sp, "user.email"); got != "temporary@work.com" {
		t.Fatalf("precondition: store should carry the override, got %q", got)
	}

	g(t, repo, "config", "--unset", "user.email")
	if _, err := Ensure(storeDir, repo, "repo", "t2", sha); err != nil {
		t.Fatal(err)
	}
	if got := cfg(t, sp, "user.email"); got != "global@example.com" {
		t.Fatalf("the store kept an override the repository no longer has: %q", got)
	}
}

// TestStoreNeverCarriesExecutableConfig is the line the bridge must not cross.
// Every one of these makes git run a command the repository's own config
// chose, in a context the developer did not — and core.hooksPath=/dev/null
// stops none of them, because they are not hooks.
func TestStoreNeverCarriesExecutableConfig(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	dangerous := map[string]string{
		"filter.evil.smudge":          "sh -c 'touch /tmp/pwned'",
		"filter.evil.clean":           "sh -c 'touch /tmp/pwned'",
		"filter.evil.process":         "sh -c 'touch /tmp/pwned'",
		"core.sshCommand":             "sh -c 'touch /tmp/pwned'",
		"core.fsmonitor":              "sh -c 'touch /tmp/pwned'",
		"gpg.program":                 "sh -c 'touch /tmp/pwned'",
		"gpg.ssh.program":             "sh -c 'touch /tmp/pwned'",
		"gpg.ssh.defaultKeyCommand":   "sh -c 'touch /tmp/pwned'",
		"trailer.sign.command":        "sh -c 'touch /tmp/pwned'",
		"diff.evil.textconv":          "sh -c 'touch /tmp/pwned'",
		"merge.evil.driver":           "sh -c 'touch /tmp/pwned'",
		"credential.helper":           "!sh -c 'touch /tmp/pwned'",
		"init.templateDir":            "/tmp/evil-template",
		"url.https://evil/.insteadOf": "https://github.com/",
		"core.pager":                  "sh -c 'touch /tmp/pwned'",
		"alias.st":                    "!sh -c 'touch /tmp/pwned'",
	}
	for k, v := range dangerous {
		g(t, repo, "config", k, v)
	}

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	local := localList(t, sp)
	for k := range dangerous {
		if strings.Contains(strings.ToLower(local), strings.ToLower(k)) {
			t.Errorf("the store carried %s, which lets the repository's config run a command:\n%s", k, local)
		}
	}
}

// TestCloneRunsNoHookFromTheUsersTemplate — measured: a reference-transaction
// hook in the user's init.templateDir fires four times *during* git clone,
// before wkt can write any config, and is then copied into the store. Setting
// core.hooksPath afterwards is too late for the run that already happened.
// --template= is the only thing that closes it.
func TestCloneRunsNoHookFromTheUsersTemplate(t *testing.T) {
	base := t.TempDir()
	tmpl := filepath.Join(base, "template", "hooks")
	if err := os.MkdirAll(tmpl, 0o755); err != nil {
		t.Fatal(err)
	}
	fired := filepath.Join(base, "fired.log")
	hook := "#!/bin/sh\necho fired >> " + fired + "\n"
	if err := os.WriteFile(filepath.Join(tmpl, "reference-transaction"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(globalCfg, []byte(
		"[init]\n\ttemplateDir = "+filepath.Join(base, "template")+"\n[user]\n\temail = e@x\n\tname = t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(fired); !os.IsNotExist(statErr) {
		body, _ := os.ReadFile(fired)
		t.Fatalf("building the store ran a hook out of the user's template:\n%s", body)
	}
	if _, statErr := os.Stat(filepath.Join(sp, "hooks", "reference-transaction")); !os.IsNotExist(statErr) {
		t.Fatal("and it was copied into the store, where it would run on every later operation")
	}
}

// cfg reads a config value with plain git. The g() helper passes
// "-c user.email=..." on every call, which would override exactly the value
// these tests are trying to observe.
func cfg(t *testing.T, dir, key string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--get", key)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// localList reads only the store's own settings, again with plain git.
func localList(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "config", "--local", "--list")
	cmd.Dir = dir
	out, _ := cmd.Output()
	return string(out)
}

// TestStoreDropsAnOverrideWhenTheRepositoryHasNoValueAtAll — the other release
// path. When the repository still resolves *something* (through the global
// config) the bridge drops the redundant override; this is the case where it
// resolves nothing, and the override has to go all the same or the store keeps
// a signing key the developer deleted.
func TestStoreDropsAnOverrideWhenTheRepositoryHasNoValueAtAll(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)
	globalCfg := filepath.Join(base, "gitconfig")
	// Deliberately no user.signingkey anywhere but the repository.
	if err := os.WriteFile(globalCfg, []byte("[user]\n\temail = e@x\n\tname = t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	g(t, repo, "config", "user.signingkey", "ABCD1234")

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg(t, sp, "user.signingkey"); got != "ABCD1234" {
		t.Fatalf("precondition: the store should carry it, got %q", got)
	}

	g(t, repo, "config", "--unset", "user.signingkey")
	if _, err := Ensure(storeDir, repo, "repo", "t2", sha); err != nil {
		t.Fatal(err)
	}
	if got := cfg(t, sp, "user.signingkey"); got != "" {
		t.Fatalf("the store kept a signing key the repository no longer has: %q", got)
	}
}

// TestStoreCarriesAnIdentityThatCameFromIncludeIf is why the bridge reads the
// repository's *effective* configuration rather than its local one. A
// corporate identity is usually configured as an includeIf on a directory
// prefix, which the store — living outside that prefix — cannot resolve for
// itself, and which "config --local" in the repository does not report either.
func TestStoreCarriesAnIdentityThatCameFromIncludeIf(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	sha := seedRepo(t, repo)

	workCfg := filepath.Join(base, "work-gitconfig")
	if err := os.WriteFile(workCfg, []byte("[user]\n\temail = via-include@company.com\n\tname = Via Include\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	globalCfg := filepath.Join(base, "gitconfig")
	// The prefix is the repository itself, so the container beside it is not
	// covered — which is the case that actually breaks.
	if err := os.WriteFile(globalCfg, []byte(
		"[user]\n\temail = personal@example.com\n\tname = Personal\n"+
			"[includeIf \"gitdir:"+repo+"/\"]\n\tpath = "+workCfg+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)

	if got := cfg(t, repo, "user.email"); got != "via-include@company.com" {
		t.Fatalf("precondition: the repository should resolve the included identity, got %q", got)
	}

	storeDir := filepath.Join(base, "store")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sp, err := Ensure(storeDir, repo, "repo", "t1", sha)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg(t, sp, "user.email"); got != "via-include@company.com" {
		t.Fatalf("the store resolves %q; a task's commits would be attributed to the wrong person", got)
	}
}
