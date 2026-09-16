package store

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// ID is a collision-free function of the workspace-relative path — never the
// basename, so services/api and tools/api cannot collide (spec §5.2).
func ID(relPath, absPath string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, relPath)
	canon, _ := paths.Canonical(absPath)
	sum := sha256.Sum256([]byte(canon))
	return strings.Trim(slug, "-") + "-" + hex.EncodeToString(sum[:])[:8]
}

// Ensure performs the six steps of spec §5.2, in order. The base pin is written
// FIRST so no gc window exists before the store references the base (H11).
func Ensure(storeDir, repoAbs, relPath, taskName, baseSHA string) (string, error) {
	pin := "refs/wkt/base/" + taskName
	if _, err := gitx.Run(repoAbs, "update-ref", pin, baseSHA); err != nil {
		return "", wkterr.New("WKT_PIN_FAILED", "cannot pin the base commit in the workspace repository").
			WithRepo(relPath).WithPath(repoAbs).WithFound(err.Error())
	}

	sp := filepath.Join(storeDir, ID(relPath, repoAbs)+".git")
	if _, err := os.Stat(sp); err == nil {
		// A directory here is not evidence that the store inside it is
		// finished. A build interrupted after the clone — Ctrl-C is enough —
		// leaves one that looks complete and is not: it still borrows objects
		// from the developer's own repository, so a later gc or re-clone makes
		// every commit in the task unreadable, and its hooks are live.
		// Verify before adopting.
		if err := adoptable(sp, repoAbs); err != nil {
			return "", err
		}
		// Re-run on every Ensure: the developer may have changed their
		// identity, or dropped a repository-specific one, since the store was
		// built.
		if err := bridgeConfig(sp, repoAbs); err != nil {
			return "", err
		}
		return sp, nil
	}

	// --template= is not tidiness. Measured: a reference-transaction hook in
	// the user's init.templateDir fires four times *during* the clone —
	// before any config can be written — and is then copied into the store,
	// where it would run on every later operation. Setting core.hooksPath
	// afterwards cannot undo a run that already happened; an empty template
	// is the only thing that prevents it.
	if _, err := gitx.Run(storeDir, "clone", "--template=", "--shared", "--bare", "-q", repoAbs, sp); err != nil {
		return "", wkterr.New("WKT_STORE_CREATE", "cannot mirror the repository").
			WithRepo(relPath).WithPath(sp).WithFound(err.Error())
	}
	// De-borrow: copy the objects in, then drop the alternates pointer, so the
	// store survives deletion or re-clone of the workspace repository (spec §5.2).
	if _, err := gitx.Run(sp, "repack", "-a", "-d", "-q"); err != nil {
		return "", wkterr.New("WKT_STORE_CREATE", "cannot repack the store").WithRepo(relPath).WithPath(sp).WithFound(err.Error())
	}
	if err := os.Remove(filepath.Join(sp, "objects", "info", "alternates")); err != nil && !os.IsNotExist(err) {
		return "", wkterr.New("WKT_STORE_CREATE", "cannot de-borrow the store").WithRepo(relPath).WithPath(sp).WithFound(err.Error())
	}

	origin, err := gitx.Run(repoAbs, "remote", "get-url", "origin")
	if err == nil && origin != "" {
		if _, err := gitx.Run(sp, "remote", "set-url", "origin", origin); err != nil {
			return "", wkterr.New("WKT_STORE_CREATE", "cannot point the store at the real origin").WithRepo(relPath).WithFound(err.Error())
		}
		// Bare clones set NO fetch refspec; without this refs/remotes/* never exist,
		// which silently breaks sync and the unpushed-commit guard (spec H15).
		if _, err := gitx.Run(sp, "config", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
			return "", wkterr.New("WKT_STORE_CREATE", "cannot configure the origin refspec").WithRepo(relPath).WithFound(err.Error())
		}
	} else {
		// No origin on the workspace repository: drop the borrowed "origin" the
		// clone created rather than leave a URL-less remote with a refspec.
		_, _ = gitx.Run(sp, "remote", "remove", "origin")
	}
	// Second remote: the workspace repository, so a task can branch from work the
	// developer has committed locally and not pushed (spec §5.2).
	if _, err := gitx.Run(sp, "remote", "add", "workspace", repoAbs); err != nil {
		return "", wkterr.New("WKT_STORE_CREATE", "cannot add the workspace remote").WithRepo(relPath).WithFound(err.Error())
	}
	if _, err := gitx.Run(sp, "config", "remote.workspace.fetch", "+refs/heads/*:refs/remotes/ws/*"); err != nil {
		return "", wkterr.New("WKT_STORE_CREATE", "cannot configure the workspace refspec").WithRepo(relPath).WithFound(err.Error())
	}
	for _, kv := range [][2]string{{"gc.auto", "0"}, {"core.hooksPath", "/dev/null"}} {
		if _, err := gitx.Run(sp, "config", kv[0], kv[1]); err != nil {
			return "", wkterr.New("WKT_STORE_CREATE", "cannot harden the store").WithRepo(relPath).WithFound(err.Error())
		}
	}
	if err := bridgeConfig(sp, repoAbs); err != nil {
		return "", err
	}
	// The marker is written by the code that verifies the invariants, never
	// by hand. Ordering by convention is how "stamp it last" quietly becomes
	// "stamp it first" in a later edit — and a marker that can be written
	// before the store is hardened is a marker that lies, permanently, since
	// nothing re-checks a marked store.
	if err := stamp(sp, repoAbs); err != nil {
		return "", err
	}
	return sp, nil
}

// stamp records that a store is finished, after checking that it is.
func stamp(sp, repoAbs string) error {
	if problems := invariants(sp, repoAbs); len(problems) > 0 {
		e := wkterr.New("WKT_STORE_CREATE", "the store is not in the state wkt requires; refusing to mark it complete").
			WithPath(sp)
		for _, p := range problems {
			e = e.WithProblem(wkterr.Problem{Code: "WKT_STORE_CREATE", Path: sp, Detail: p})
		}
		return e
	}
	if _, err := gitx.Run(sp, "config", markerKey, "1"); err != nil {
		return wkterr.New("WKT_STORE_CREATE", "cannot mark the store complete").
			WithPath(sp).WithFound(err.Error())
	}
	return nil
}

// invariants returns everything about a store that is not as spec §5.2
// requires, in language a person can act on.
// Invariants is exported because doctor has to answer the same question. A
// WKT_STORE_INCOMPLETE refusal tells the user to run wkt doctor, so the two
// must not have separate opinions about what "incomplete" means — they did,
// and the store whose origin still pointed at the developer's clone was
// refused by one and called healthy by the other.
func Invariants(sp, repoAbs string) []string { return invariants(sp, repoAbs) }

func invariants(sp, repoAbs string) []string {
	var problems []string
	if _, err := os.Stat(filepath.Join(sp, "objects", "info", "alternates")); err == nil {
		problems = append(problems, "it still borrows objects from the workspace repository (objects/info/alternates present), so the task's commits would become unreadable if that repository is re-cloned or garbage-collected")
	}
	if v, err := gitx.Run(sp, "config", "--get", "gc.auto"); err != nil || strings.TrimSpace(v) != "0" {
		problems = append(problems, "gc.auto is not disabled, so git may collect objects a task still needs")
	}
	if v, err := gitx.Run(sp, "config", "--get", "core.hooksPath"); err != nil || strings.TrimSpace(v) == "" {
		problems = append(problems, "its hooks are live: anything in the store's hooks directory runs when a task commits")
	}
	if v, err := gitx.Run(sp, "config", "--get", "remote.workspace.url"); err != nil || strings.TrimSpace(v) == "" {
		problems = append(problems, "it has no workspace remote, so a task cannot branch from work committed locally and not pushed")
	}
	if v, err := gitx.Run(sp, "config", "--get", "remote.origin.url"); err == nil {
		if canon, cErr := paths.Canonical(strings.TrimSpace(v)); cErr == nil {
			if repoCanon, rErr := paths.Canonical(repoAbs); rErr == nil && canon == repoCanon {
				problems = append(problems, "its origin still points at the workspace repository instead of the real upstream")
			}
		}
	}
	return problems
}

// markerKey records that a store passed through every step of Ensure. git
// lowercases config key names, so it is written and read in the form git
// stores it.
const markerKey = "wkt.storecomplete"

// MarkerKey is markerKey for the same reason Invariants is exported: doctor
// reads the marker, and a second spelling of it in another package is a
// divergence waiting to happen.
const MarkerKey = markerKey

// adoptable decides whether an existing store may be reused.
//
// It verifies spec §5.2's invariants rather than demanding the marker,
// because every store built before the marker existed is complete and has
// none — a rule of "unmarked means unusable" would condemn the whole
// installed base. A store that verifies is stamped, so the check happens once.
//
// It never deletes and never rebuilds. The store is the only copy of every
// task's unpushed commits, and it owns the worktree admin directories: a
// rebuild re-issues the registration names and aliases an existing task's tree
// onto a new one. Refusing loudly is the worst thing that may happen here.
func adoptable(sp, repoAbs string) error {
	if v, err := gitx.Run(sp, "config", "--get", markerKey); err == nil && strings.TrimSpace(v) != "" {
		return nil
	}

	problems := invariants(sp, repoAbs)
	if len(problems) == 0 {
		// Complete, merely unmarked: an earlier version built it. Verify once,
		// stamp, and never look again.
		_, _ = gitx.Run(sp, "config", markerKey, "1")
		return nil
	}

	e := wkterr.New("WKT_STORE_INCOMPLETE",
		"an unfinished store is already at that path; wkt will not adopt it and will not delete it").
		WithPath(sp)
	for _, p := range problems {
		e = e.WithProblem(wkterr.Problem{Code: "WKT_STORE_INCOMPLETE", Path: sp, Detail: p})
	}
	if out, err := gitx.Run(sp, "worktree", "list", "--porcelain"); err == nil && strings.Count(out, "worktree ") > 1 {
		e = e.WithRemedy("task trees are still attached to this store — inspect them before doing anything",
			"wkt status lists what this container holds")
	} else {
		e = e.WithRemedy("this store holds no attached trees; if it also holds no work you need, remove the directory yourself and retry",
			"wkt doctor --all reports what the container is holding")
	}
	return e
}

func FetchWorkspace(storePath string) error {
	if _, err := gitx.Run(storePath, "fetch", "-q", "workspace"); err != nil {
		return wkterr.New("WKT_FETCH_FAILED", "cannot fetch from the workspace repository").WithPath(storePath).WithFound(err.Error())
	}
	return nil
}

func HasObject(storePath, sha string) bool {
	return gitx.RunOK(storePath, "cat-file", "-e", sha)
}

// bridged are the settings a task's commits should share with the repository
// the work belongs to. A bare clone copies no config, and the store lives
// outside the workspace where neither .git/config nor an "includeIf gitdir:"
// can reach it — so without this, commits made in a task are authored with
// whatever the global identity happens to be.
//
// Nothing here can execute. Every excluded key is excluded for that reason:
// filter.*.clean/smudge/process (which git runs during "worktree add", inside
// wkt new itself), core.sshCommand, core.fsmonitor, gpg.program,
// gpg.ssh.program, gpg.ssh.defaultKeyCommand, trailer.*.command,
// diff.*.textconv, merge.*.driver, credential.helper, init.templateDir,
// core.pager, core.editor and alias.*. core.hooksPath=/dev/null stops none of
// them, because none of them are hooks. url.*.insteadOf is excluded too: it
// silently redirects where the store fetches from.
//
// Also excluded: remote.* (wkt owns those, and a narrower refspec would lose
// refs/remotes/origin/* — spec H15) and the filesystem probes
// core.ignorecase/precomposeunicode/filemode, which git computes for the
// directory it is in; forcing another repository's answer onto the store makes
// it misreport tracked files.
var bridged = []string{
	"user.name",
	"user.email",
	"user.signingkey",
	"commit.gpgsign",
	"tag.gpgsign",
	"gpg.format",
	"gpg.ssh.allowedsignersfile",
	"core.autocrlf",
	"core.eol",
}

// bridgeConfig makes the store resolve the same values the workspace
// repository resolves, writing a local override only where the ambient config
// does not already agree — and removing one it wrote earlier when the
// repository no longer needs it.
//
// Two reads, not two per key: the whole list comes back in one call per side.
func bridgeConfig(sp, repoAbs string) error {
	want, err := effectiveConfig(repoAbs)
	if err != nil {
		return nil // a repository whose config cannot be read is not fatal here
	}
	local, ambient, err := storeConfig(sp)
	if err != nil {
		return nil
	}

	for _, key := range bridged {
		desired := want[key]
		switch {
		case desired == "" && local[key] != "":
			// The repository dropped it; stop pinning it.
			_, _ = gitx.Run(sp, "config", "--local", "--unset-all", key)
		case desired == "":
			// Nothing to say about this key.
		case desired == ambient[key] && local[key] != "":
			// The ambient config already agrees; drop the redundant override
			// so a later change to it is picked up.
			_, _ = gitx.Run(sp, "config", "--local", "--unset-all", key)
		case desired == ambient[key]:
			// Already right without a local override.
		case desired != local[key]:
			if _, err := gitx.Run(sp, "config", "--local", key, desired); err != nil {
				return wkterr.New("WKT_STORE_CREATE", "cannot carry the repository's git configuration into the store").
					WithPath(sp).WithFound(err.Error())
			}
		}
	}
	return nil
}

// effectiveConfig is what the repository actually resolves, including values
// an "includeIf gitdir:" brought in — which is the whole point, since that is
// how corporate identities are usually configured and exactly what the store
// cannot see for itself.
func effectiveConfig(dir string) (map[string]string, error) {
	out, err := gitx.Run(dir, "config", "--list", "-z")
	if err != nil {
		return nil, err
	}
	return parseConfigZ(out, false), nil
}

// storeConfig splits the store's own settings from the ones it inherits, so
// the bridge can tell "I wrote this" from "the machine already says this".
func storeConfig(dir string) (local, ambient map[string]string, err error) {
	out, err := gitx.Run(dir, "config", "--list", "--show-scope", "-z")
	if err != nil {
		return nil, nil, err
	}
	local, ambient = map[string]string{}, map[string]string{}
	// Records are NUL-separated as: scope NUL key NEWLINE value.
	fields := strings.Split(out, "\x00")
	for i := 0; i+1 < len(fields); i += 2 {
		scope := fields[i]
		kv := strings.SplitN(fields[i+1], "\n", 2)
		if len(kv) != 2 {
			continue
		}
		key := strings.ToLower(kv[0])
		if scope == "local" || scope == "worktree" {
			local[key] = kv[1]
		} else {
			ambient[key] = kv[1]
		}
	}
	return local, ambient, nil
}

// parseConfigZ reads "git config --list -z": key NEWLINE value, NUL-separated.
func parseConfigZ(out string, _ bool) map[string]string {
	m := map[string]string{}
	for _, rec := range strings.Split(out, "\x00") {
		kv := strings.SplitN(rec, "\n", 2)
		if len(kv) != 2 {
			continue
		}
		m[strings.ToLower(kv[0])] = kv[1]
	}
	return m
}
