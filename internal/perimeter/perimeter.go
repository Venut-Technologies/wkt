// Package perimeter renders the settings document wkt writes into a task
// tree. It is defence in depth against accidents, never a boundary (spec §0):
// what it buys is that an agent working in one task does not casually write
// into the workspace or into another task's tree.
//
// Two facts measured against Claude Code 2.1.238 shape everything here
// (docs/superpowers/specs/2026-08-21-hazard-reverification.md):
//
//   - An "Edit(...)" deny rule constrains the Bash tool too — the rules are
//     merged into the sandbox profile — so the paths are stated once, under
//     permissions.deny, and never restated under sandbox.filesystem.denyWrite.
//   - That profile is compiled into every command the session runs. It works
//     at ~5,000 paths; past roughly 9,000 the profile stops compiling and
//     *every* Bash command fails. So the list is capped, and exceeding the cap
//     is a refusal rather than a file that quietly breaks the tool.
package perimeter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/container"
	"github.com/Venut-Technologies/wkt/internal/gitx"
	"github.com/Venut-Technologies/wkt/internal/paths"
	"github.com/Venut-Technologies/wkt/internal/state"
	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// MaxPaths caps the deny list well below the measured failure point. The
// failure past it is not degradation — the session's Bash stops working
// entirely — so the margin is deliberate.
const MaxPaths = 2000

type Document struct {
	// Marker says who wrote this file and for which task. Claude Code
	// ignores unknown top-level keys — verified on 2.1.238, where a
	// settings file carrying "$wkt" still enforced its deny rules — and it
	// makes ownership a property of the file rather than of wkt's state.
	// Without it, a task whose state lost its recorded hashes could never
	// have its perimeter regenerated: the command that exists to repair
	// that case would refuse, mistaking its own output for the user's file.
	Marker      Marker      `json:"$wkt"`
	Permissions Permissions `json:"permissions"`
	Sandbox     Sandbox     `json:"sandbox"`
}

type Marker struct {
	Version int    `json:"version"`
	Task    string `json:"task"`
	Note    string `json:"note"`
}

// MarkerVersion is the shape of the generated document, not the wkt release.
const MarkerVersion = 1

type Permissions struct {
	Deny []string `json:"deny"`
}

type Sandbox struct {
	Enabled    bool       `json:"enabled"`
	Filesystem Filesystem `json:"filesystem"`
	// A pointer so that "omitempty" does what it says. On a struct field the
	// tag does nothing, and every task in a workspace of local-only
	// repositories carried "network": {} — a present but empty allowlist,
	// which is the shape that made "git ls-remote origin" fail with CONNECT
	// 403 and is the opposite of "opens nothing at all".
	Network *Network `json:"network,omitempty"`
}

// Network is the egress allowlist. With the sandbox on, everything else is
// refused by the proxy — measured inside a covered tree, "git ls-remote
// origin" failed with "CONNECT tunnel failed, response 403". A task that
// cannot reach its own repositories' upstream cannot fetch or push, which is
// most of the point of having the task.
type Network struct {
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

type Filesystem struct {
	AllowWrite []string `json:"allowWrite,omitempty"`
	// DenyWrite stays empty by design: the deny paths already reach this
	// layer through the Edit rules. The field exists so a test can assert
	// that it is empty, and so a future release that stops merging can fill
	// it without a schema change.
	DenyWrite []string `json:"denyWrite,omitempty"`
	DenyRead  []string `json:"denyRead,omitempty"`
}

// spellingsOf returns every spelling of one path. Deny globs are lexical, so
// an alias such as ~/work -> /Volumes/Data/work defeats a single spelling
// entirely (spec §5.6).
func spellingsOf(p string) []string { return paths.Spellings(p) }

// rule renders one deny rule. The "//" prefix is what makes the path
// absolute: verified on 2.1.238 that "Edit(//Users/x/f)" and
// "Edit(///Users/x/f)" both deny, while "Edit(/Users/x/f)" is accepted and
// silently does nothing — the worst possible failure for a guard.
func rule(p string, recursive bool) string {
	if recursive {
		return "Edit(//" + p + "/**)"
	}
	return "Edit(//" + p + ")"
}

// For builds the document for one task, given the names of the other tasks in
// the container.
func For(c container.C, t state.Task, siblings []string) (Document, error) {
	tree := c.TreePath(t.Name)

	deny := map[string]bool{}
	addAll := func(p string, recursive bool) {
		for _, sp := range spellingsOf(p) {
			deny[rule(sp, recursive)] = true
		}
	}

	// The workspace itself: the whole point is that a task never writes there.
	addAll(c.Workspace, true)
	// wkt's own bookkeeping. State is authoritative; staging is where a
	// forced removal parks a tree mid-delete.
	addAll(filepath.Join(c.Root, "state"), true)
	addAll(c.StagingDir(), true)
	// Every other task's tree, named individually: H16 was re-confirmed on
	// 2.1.238, so a wide glob with a narrower allow for this task's own tree
	// does not work — deny wins.
	for _, s := range siblings {
		if s == t.Name {
			continue // never deny the tree this perimeter is for
		}
		addAll(filepath.Join(c.TreesDir(), s), true)
	}
	// The store is writable (see AllowWrite below) because the task's gitdir
	// lives there, so its two dangerous spots are closed explicitly. A
	// narrower deny still beats the broader allow.
	for _, r := range t.Repos {
		storePath := filepath.Join(c.StoreDir(), r.StoreID+".git")
		addAll(filepath.Join(storePath, "hooks"), true)
		addAll(filepath.Join(storePath, "config"), false)
	}
	// The perimeter protects itself (H13), and the task's state mirror.
	addAll(filepath.Join(tree, ".claude"), true)
	addAll(filepath.Join(tree, ".wkt"), true)
	for _, r := range t.Repos {
		addAll(filepath.Join(tree, r.RelPath, ".claude"), true)
	}

	if len(deny) > MaxPaths {
		return Document{}, wkterr.New("WKT_PERIMETER_TOO_LARGE",
			"the perimeter would carry more paths than a sandbox profile can hold").
			WithExpected(strconv.Itoa(MaxPaths)).
			WithFound(strconv.Itoa(len(deny))).
			WithRemedy("remove tasks from this container, or run wkt rm on the ones you have finished")
	}

	return Document{
		Marker: Marker{
			Version: MarkerVersion,
			Task:    t.Name,
			Note:    "generated by wkt; edits are overwritten on the next regeneration",
		},
		Permissions: Permissions{Deny: sorted(deny)},
		Sandbox: Sandbox{
			Enabled: true,
			Filesystem: Filesystem{
				AllowWrite: append(spellingsOf(c.StoreDir()), toolchainCaches()...),
				DenyRead:   credentialDirs(),
			},
			Network: networkFor(t),
		},
	}, nil
}

// networkFor is the egress allowlist, or nothing at all. A task whose
// repositories point nowhere reachable gets no network key, so the document
// says nothing about egress rather than saying "allow no host".
func networkFor(t state.Task) *Network {
	hosts := originHosts(t)
	if len(hosts) == 0 {
		return nil
	}
	return &Network{AllowedDomains: hosts}
}

// credentialDirs are read-denied outright: a task has no business reading the
// developer's keys, cloud credentials, gh token or Claude Code configuration
// (which holds a live OAuth token).
func credentialDirs() []string {
	home, err := homeDir()
	if err != nil {
		return nil
	}
	var out []string
	for _, d := range []string{".ssh", ".aws", ".config/gh", ".claude"} {
		out = append(out, filepath.Join(home, filepath.FromSlash(d)))
	}
	sort.Strings(out)
	return out
}

func sorted(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Render produces the file's bytes. Key order is fixed by the struct and
// slice order by sorted(), so an unchanged task renders identically every
// time — anything else would read as drift to the hash check in status.
func Render(d Document) ([]byte, error) {
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, wkterr.New("WKT_PERIMETER_RENDER", "cannot encode the perimeter").
			WithFound(err.Error())
	}
	return append(b, '\n'), nil
}

func homeDir() (string, error) {
	h, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if runtime.GOOS == "darwin" && strings.HasPrefix(h, "/private/") {
		return strings.TrimPrefix(h, "/private"), nil
	}
	return h, nil
}

// toolchainCaches are the places a build writes that are not inside the tree.
//
// The perimeter switches Claude Code's sandbox on, which confines writes to
// the working directory — and every toolchain keeps its cache outside it.
// Measured: an ordinary "go build" in a task tree failed with "open
// ~/Library/Caches/go-build/…: operation not permitted". A tree that cannot be
// built in is a tree nobody works in, and the remedy people reach for is to
// delete the perimeter, which loses everything it was for. Allowing the cache
// roots costs nothing that matters: they are the developer's own scratch
// space, not anybody's work, and the workspace and sibling trees stay closed.
//
// Roots rather than an inventory of toolchains: enumerating go, npm, cargo,
// pip, gradle and the rest would be a list that is wrong the moment someone
// uses the seventh one.
func toolchainCaches() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p == "" {
			return
		}
		for _, sp := range spellingsOf(p) {
			if !seen[sp] {
				seen[sp] = true
				out = append(out, sp)
			}
		}
	}

	// The XDG cache root, which is where most tools put their cache on Linux
	// and some do everywhere.
	if xdg := os.Getenv("XDG_CACHE_HOME"); xdg != "" {
		add(xdg)
	}
	add(filepath.Join(home, ".cache"))
	if runtime.GOOS == "darwin" {
		add(filepath.Join(home, "Library", "Caches"))
	}
	// Go's module cache sits under neither root.
	add(filepath.Join(home, "go", "pkg", "mod"))
	// And whatever the developer moved with an environment variable, since a
	// moved cache is by definition somewhere the defaults do not name.
	for _, env := range []string{"GOCACHE", "GOMODCACHE", "CARGO_HOME", "npm_config_cache"} {
		add(os.Getenv(env))
	}
	sort.Strings(out)
	return out
}

// originHosts are the forge hosts this task's own repositories point at.
//
// Derived, never broadened: a task gets to reach the upstreams of the
// repositories it contains and nothing else. A repository with no remote, or
// one whose remote is a local path, opens nothing at all.
func originHosts(t state.Task) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range t.Repos {
		url, err := gitx.Run(r.AbsPath, "config", "--get", "remote.origin.url")
		if err != nil {
			continue
		}
		if host := hostFromRemoteURL(strings.TrimSpace(url)); host != "" && !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	sort.Strings(out)
	return out
}

// SSHOrigins names the repositories whose origin is reached over SSH, which
// is the one form a task tree cannot reach at all.
//
// Measured on Claude Code 2.1.239 inside a covered tree: "git ls-remote
// git@github.com:..." fails with "This proxy requires authentication, and
// this client did not offer an authentication method", while the same
// repository over HTTPS succeeds and a host off the allowlist gets CONNECT
// 403. The allowlist is an HTTP proxy; SSH is routed through it and cannot
// authenticate to it, so no entry in allowedDomains helps and nothing wkt
// writes can change it.
//
// This is not fatal to the design — work comes back through "wkt fetch",
// which runs in the developer's own shell, outside any sandbox — but §0's
// "a task tree can push to the repository's real origin" holds only for
// HTTPS remotes, and someone should hear that from wkt rather than from a
// puzzling failure later.
func SSHOrigins(t state.Task) []string {
	var out []string
	for _, r := range t.Repos {
		url, err := gitx.Run(r.AbsPath, "config", "--get", "remote.origin.url")
		if err != nil {
			continue
		}
		if isSSHRemote(strings.TrimSpace(url)) {
			out = append(out, r.RelPath)
		}
	}
	sort.Strings(out)
	return out
}

// isSSHRemote reports whether a git remote is reached over SSH, in the two
// spellings remotes come in: an explicit ssh:// scheme, and the scp-like
// "[user@]host:path" form, which is what a forge hands people by default.
func isSSHRemote(url string) bool {
	url = strings.TrimSpace(url)
	if url == "" {
		return false
	}
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if i := strings.Index(url, "://"); i >= 0 {
		return false // some other scheme, and it names itself
	}
	if strings.HasPrefix(url, "/") || strings.HasPrefix(url, ".") {
		return false // a local path
	}
	// scp-like: the colon separates the path, and it comes before any slash.
	colon := strings.Index(url, ":")
	slash := strings.Index(url, "/")
	return colon > 0 && (slash < 0 || colon < slash)
}

// hostFromRemoteURL pulls the host out of a git remote, in the spellings
// remotes actually come in: https:// and ssh:// URLs, the scp-like
// "git@host:path" form, and local paths — which have no host and must not
// produce one.
func hostFromRemoteURL(url string) string {
	url = strings.TrimSpace(url)
	switch {
	case url == "", strings.HasPrefix(url, "/"), strings.HasPrefix(url, "."),
		strings.HasPrefix(url, "file://"):
		return ""
	}
	if i := strings.Index(url, "://"); i >= 0 {
		rest := url[i+3:]
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			rest = rest[at+1:] // strip any userinfo, credentials included
		}
		if slash := strings.IndexAny(rest, "/"); slash >= 0 {
			rest = rest[:slash]
		}
		return stripPort(rest)
	}
	// scp-like: [user@]host:path — the colon separates the path, not a port.
	colon := strings.Index(url, ":")
	if colon < 0 {
		return ""
	}
	hostPart := url[:colon]
	if at := strings.LastIndex(hostPart, "@"); at >= 0 {
		hostPart = hostPart[at+1:]
	}
	if hostPart == "" || strings.Contains(hostPart, "/") {
		return ""
	}
	return hostPart
}

// stripPort removes a ":port" suffix, since the allowlist takes hosts.
func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 {
		return host[:i]
	}
	return host
}
