# Changelog

## v0.7.0 — 2026-09-16

**The module path is now `github.com/Venut-Technologies/wkt`.** The GitHub
organisation was renamed from Venut Labs to Venut Technologies, and a Go
module's path has to match where it is fetched from: every earlier tag
declares `github.com/Venut-Labs/wkt`, so installing any of them from the new
path fails with "module declares its path as github.com/Venut-Labs/wkt".

- **Migrating:** `go install github.com/Venut-Technologies/wkt/cmd/wkt@latest`.
  The binary keeps its name, so hooks already written into
  `~/.claude/settings.json` keep working when it lands in the same `GOBIN`.
  Task state does not record the module path, so existing containers and
  tasks are unaffected.
- The old path is frozen at v0.6.3, which marks it deprecated — `go install`
  says so when it fetches it. Versions v0.1.0 through v0.6.3 stay installable
  there by explicit version from the Go module proxy.
- Nothing imports wkt — everything but `cmd/wkt` is under `internal/` — so no
  code outside this repository needs changing.

## v0.6.3 — 2026-09-16

The last release under `github.com/Venut-Labs/wkt`. The GitHub organisation
was renamed to Venut Technologies, and from v0.7.0 the module path is
`github.com/Venut-Technologies/wkt`; this release marks the old path
deprecated in `go.mod` so `go list -m -u` and pkg.go.dev say so.

- Copyright and branding now name Venut Technologies.

- **`wkt hook session-start` no longer fails open in silence** (issue #6). The
  hook exists to close H16 — a sibling tree created since `WorktreeCreate`
  fired is not in this tree's deny list until something regenerates it — and
  when the regeneration failed it returned 0 with nothing on stderr, so the
  session carried on with a stale perimeter missing exactly the sibling the
  hook was added to cover. It now says `WKT_PERIMETER_STALE` and names the
  task. It also reports `WKT_PERIMETER_SKIPPED` for a directory wkt does not
  own, which since v0.6.1 is the likelier of the two things it could lose. The
  exit stays 0: aborting a session over a stale deny list would be the worse
  trade.
- **A forced removal names every foreign repository at once** (issue #5). All
  of them were found and one was reported, so clearing a tree meant moving one
  out, retrying, and meeting the next. They now arrive together in
  `problems[]`, the shape `WKT_WOULD_LOSE_WORK` already used.
- **The README no longer promises more than wkt delivers** (issue #4). "The
  workspace itself is never written to" is true of wkt's own writes and was
  read as a guarantee about everything running in a tree — three lines after
  the sentence introducing the back-fill symlinks. It now says which half is
  guaranteed and by whom.

## v0.6.2 — 2026-08-24

- **`wkt new` and `wkt add` warn about an SSH origin.** Measured inside a
  covered tree on Claude Code 2.1.239: `git ls-remote git@github.com:…` fails
  with "This proxy requires authentication, and this client did not offer an
  authentication method", while the same repository over HTTPS succeeds and a
  host off the allowlist gets `CONNECT 403`. The sandbox routes SSH through
  its HTTP proxy and SSH cannot authenticate to it, so no `allowedDomains`
  entry helps and nothing wkt writes can change it — the proxy is Claude
  Code's. Work still returns through `wkt fetch`, which runs outside the
  sandbox, so this limits an agent pushing from inside the tree, not the
  design. `WKT_SSH_ORIGIN` names the repository and the route back.

## v0.6.1 — 2026-08-24

- **A repository with its own `.claude/settings.json` no longer blocks the
  task.** Measured: `wkt new` failed outright with `WKT_PERIMETER_FOREIGN` and
  rolled the tree back, so a workspace holding one such repository — a common
  shape — could not be used at all, while the refusal's own remedy offered to
  "leave it and accept that this directory has no wkt perimeter", an option
  the tool did not provide. That directory is now skipped and named on stderr
  as `WKT_PERIMETER_SKIPPED`; the file is untouched and the rest of the tree
  is covered as before.

## v0.6.0 — 2026-08-23

The `post-create` seam: the last thing the design promised and had never
built. §1.1 says "there is a `post-create` seam and a gitignored-file carry,
and nothing else" — the carry shipped in v0.5.0, and this is the other half.

- **A tree can set itself up.** An executable `.wkt/post-create` at the
  workspace root runs once the tree is built, on `wkt new` and again on `wkt
  add`, with the tree root as its working directory and `WKT_TASK`,
  `WKT_TREE`, `WKT_WORKSPACE`, `WKT_REPOS` and `WKT_ADDED_REPO` describing
  what it is setting up. Its output is streamed to stderr, so `wkt new` still
  prints exactly the tree path to stdout and the Claude Code worktree hook
  still works.
- **A failing script leaves the task standing.** wkt exits non-zero and
  carries the script's own words in the error rather than a guess at them —
  the reader is usually an agent, and an exit status alone is nothing it can
  act on. `--no-post-create` skips the script.
- **Back-fill links are withdrawn while it runs.** Measured: a repository the
  task did not select is a symlink into the workspace, and writing through it
  lands in the developer's own checkout — so the `for d in */` loop everyone
  writes would install into their working repositories. Closed by construction
  rather than by a warning, because the dangerous script is the one written
  without thinking.
- **What the script produces does not make removal demand `--force`.**
  Measured: a gitignored `local.sqlite` blocked `wkt rm` and pointed at
  `--force`. The seam exists to produce exactly that kind of content, and
  reflexive `--force` is the habit teardown's refusals exist to prevent.
- **A task name is checked before it reaches the script.** Measured: `wkt new`
  accepts `a;b`, `a$b` and backticked names, all legal branches — harmless to
  wkt, which never uses a shell, and not harmless to a script expanding
  `$WKT_TREE` unquoted.
- **It runs on the Claude Code worktree hook too**, which is the case it most
  needs to serve: a tree a session opens should be ready to work in. On that
  path a failing script is a warning and the exit stays 0 — a non-zero hook
  makes Claude Code refuse a tree that is built and usable, which is the one
  outcome worse than a failed install.
- **On the hook, wkt stops the script at eight minutes.** Measured on 2.1.239:
  a `WorktreeCreate` hook running 399 seconds was untouched, and a longer one
  was cancelled at 591 with `Hook cancelled` — leaving the session with no
  worktree at all, though wkt had built one. A script wkt stops leaves a usable
  tree and a warning instead. The command line keeps no deadline.
- **`wkt post-create TASK`** runs the seam again on a task that already exists:
  after a failure, after `--no-post-create`, or after the hook stopped it
  short. Not a convenience — running the script by hand skips the back-fill
  withdrawal, so the loop everyone writes would install into your own
  repositories. The protection and its repair path are the same code.
- **A produced directory no longer exempts what is put in it later.** git
  collapses a wholly ignored directory to one line, so recording `.secrets/`
  as produced would have exempted a key dropped in there afterwards, and a
  plain `wkt rm` would have deleted it. A produced *file* stays disposable —
  a service rewriting its own database is expected, and blocking on that
  brings `--force` back as a habit.
- **A build directory outside a repository is recorded whole.** An installer
  run at the tree root leaves a `node_modules` with a hundred thousand files;
  each would have become a permanent entry in the task's state. The tree half
  now collapses it the way git already collapses the repository half.
- **A task with nothing to reach no longer carries an empty egress
  allowlist.** `omitempty` does nothing on a struct field, so every task in a
  workspace of local-only repositories rendered `"network": {}` — a present
  but empty allowlist, which is the shape that made `git ls-remote origin`
  fail with `CONNECT tunnel failed, response 403` and the opposite of what the
  code claimed. Shipped in v0.4.2, found by review.
- Internally: a per-task lock, so an install that runs for minutes no longer
  holds the container lock and stops every other command in the workspace.

## v0.5.1 — 2026-08-22

`wkt fetch` worked exactly once per task, and three collision checks let git
fail from the middle of an operation that had already changed things.

- **`wkt fetch` can be run more than once.** The ordinary rhythm — fetch what
  is done, keep working, fetch again — was refused on the second call with
  `WKT_NOT_FAST_FORWARD`, and the developer was told to bring the branch in
  under another name or merge the two by hand, for a fast-forward git would
  have taken without comment. The ancestry question was asked in the workspace
  repository, which has never seen the task's newer commit, so `merge-base`
  failed on an unknown object and the failure was read as divergence. It is
  asked in the store now: the store holds everything reachable from the task's
  branch, so a commit it does not have cannot be an ancestor of one it does.
- **A hierarchical collision is refused before any ref moves.** `refs/heads/feat`
  and `refs/heads/feat/42` cannot coexist, and the second is invisible to a
  lookup of the first — `rev-parse feat` reports nothing when `feat/42` holds
  the path. `fetch` checks the whole set before moving anything precisely so a
  developer is never left holding half a task; this slipped past that check, so
  the first repository's ref moved and a later one failed.
- **`wkt new` and `wkt add` ask the store, not only the workspace.** A store
  outlives the task that built it: state can be lost while the store keeps
  every branch it ever held, so the developer's repository can look clear while
  the place the branch is actually created is not. `add` had no hierarchical or
  case check at all. Both now refuse with `WKT_BRANCH_DF_CONFLICT` or
  `WKT_BRANCH_CASE_COLLISION`, naming the branch in the way, before a store is
  built or a base pin written.
- **A refusal wkt cannot classify carries git's own words.** `WKT_FETCH_FAILED`
  discarded git's explanation and printed one guess in its place — "if the
  branch is checked out there, switch away from it first" — which was simply
  wrong for every other cause.

## v0.5.0 — 2026-08-22

A task tree can now run the services in it: the gitignored-file carry the
design scoped in exists.

- **The gitignored-file carry exists** (issue #3). The design scoped it in —
  "there is a `post-create` seam and a gitignored-file carry, and nothing
  else" — and it was never built, so a fresh tree had no `.env` and no way to
  get one. Name the files in a `.wktinclude` at the workspace root, in
  gitignore syntax; a file is carried only if a pattern matches it **and** git
  already ignores it. Copies, never links, and the mode is preserved. The
  matching is git's own — the patterns are handed to git as an excludes file —
  so the syntax is not an approximation of gitignore, it is gitignore.
- Teardown tells a carried file apart from work: one the task never changed
  loses nothing when the tree goes and does not block removal; one it edited
  does.

## v0.4.2 — 2026-08-21

The perimeter made task trees unusable: nothing could be built in one, and
nothing could be fetched or pushed from one. Both are fixed, and both were
found by measuring rather than by reading the design.

- **A task tree can be built in again.** The perimeter switches Claude Code's
  sandbox on, which confines writes to the working directory — and every
  toolchain keeps its cache outside it. Measured: an ordinary `go build` inside
  a task tree failed with `open ~/Library/Caches/go-build/…: operation not
  permitted`, and npm, cargo and pip fail the same way. A tree nobody can build
  in is a tree nobody works in, and the remedy people reach for is deleting the
  perimeter, which loses everything it is for. The cache roots are writable
  now — `~/.cache`, `~/Library/Caches`, Go's module cache, and whatever
  `GOCACHE`, `GOMODCACHE`, `CARGO_HOME` or `npm_config_cache` point at.
  Verified that the workspace, sibling trees and wkt's own state stay closed.
- **A task can reach its own repositories' upstream again.** The same sandbox
  routes egress through a proxy that refuses anything not on an allowlist, so
  inside a covered tree `git ls-remote origin` failed with `CONNECT tunnel
  failed, response 403` — no fetch, no push, from the tool whose job is
  carrying work across repositories. The perimeter now allows exactly the hosts
  the task's own repositories point at, derived from their origins: a
  repository with no remote, or one whose remote is a local path, opens
  nothing, and a host no repository in the task uses stays refused.

## v0.4.1 — 2026-08-21

Two reported defects, both about `--force` losing work, and one warning of my
own that fired on the ordinary case.

- **`--force` no longer deletes a repository created inside the tree**
  (issue #1). Someone starting a new service inside a task — `git init`, a few
  commits, nothing pushed — could lose that history entirely: the walk reported
  the directory as untracked content and stopped descending, so the repository
  below it was invisible to every later check. The walk now reports each
  untracked directory once and keeps going, and a repository whose history
  exists nowhere else is the one refusal `--force` does not cover.
- **A forced removal says where it kept your unpushed work** (issue #2). The
  objects always survived in the store, but nothing pointed at them, so
  recovery meant knowing the store's layout and digging for a dangling commit.
  `wkt rm --force` now parks the branch tip at `refs/wkt/removed/<task>` — only
  when there is something no remote has — and prints the command that reads it.
- `wkt init` no longer warns about repositories it just found. The
  below-the-bound warning fired on any directory *containing* a discovered
  repository, which is the product's ordinary shape (`services/svc-a`).

## v0.4.0 — 2026-08-21

Four defects, all found by measuring the design's own open questions rather
than by reading them. Two of the four were shipped in v0.3.0 and failed
silently.

Four fixes, all found by measuring the design's own open questions rather than
by reading them.

- **Commits made in a task now carry the repository's identity.** A bare clone
  copies no config and the store lives outside the workspace, so neither
  `.git/config` nor an `includeIf gitdir:` reached it: work done in a task was
  authored with whatever the *global* identity happened to be, silently, until
  a CI identity check or a DCO bot rejected the push. wkt now bridges
  `user.*`, the signing settings and `core.autocrlf`/`core.eol` into the store,
  reading what the repository *resolves* rather than what it stores — which is
  how corporate identities are usually configured. Nothing that can execute is
  carried: not `filter.*`, `core.sshCommand`, `gpg.program`, `credential.helper`,
  `trailer.*.command` or `url.*.insteadOf`.
- **Building a store no longer runs your template hooks.** Measured: a
  `reference-transaction` hook in `init.templateDir` fires four times during
  `git clone` — before any config can be written — and is copied into the
  store. The clone is now made with an empty template.
- **An unfinished store is never adopted.** A build interrupted after the clone
  left a directory that looked finished; wkt reused it, and the tree kept
  borrowing objects from your own repository, so a later `gc` or re-clone made
  every commit in the task unreadable. wkt now verifies the store's invariants
  before reuse and refuses with each broken one named — and never deletes or
  rebuilds it, because the store may hold the only copy of a task's unpushed
  commits.
- **`wkt sync` no longer says "up to date" when it could not look.** The origin
  fetch error was discarded, so a store that had never once reached its
  upstream reported success. It now says it could not reach origin, and exits 3.
- **A failed checkout says why.** `wkt new` and `wkt add` threw git's
  explanation away, so a repository whose checkout runs a content filter
  (git-lfs is the common one) failed with nothing but "cannot create the
  worktree". The reason is carried now — with the configured command redacted,
  because that is where people keep access tokens.

## v0.3.0 — 2026-08-21

The rest of the command table: a task can gain a repository, learn that its
base has moved, hand its work back, and survive the workspace being moved.

- `wkt repair TASK` **adopts a moved workspace.** State records absolute paths,
  so moving a project used to break every task in it beyond fixing: the
  worktrees detach, the back-fill links point at a workspace that is no longer
  there, and the perimeter denies directories that no longer exist. repair now
  rewrites the recorded paths, reattaches each worktree, re-aims the links and
  regenerates the perimeter. It never clears the way first — where a link slot
  has become a real directory, it says so and leaves the contents alone.

- `wkt add TASK --repos b` grafts another repository onto a live task **at the
  task's recorded base epoch**, not today's tip: a task is a coherent slice of
  time across a set of repositories, and a latecomer arriving at HEAD would be
  based on work the rest of the set has never seen. The repository's back-fill
  link becomes a real worktree, and a failed add puts the link back.
- `wkt sync TASK` fetches in every store and reports how far each repository's
  base has drifted — across **both** remotes, so a colleague's push and your own
  unpushed commits both count. It never advances the base itself.
- `wkt fetch TASK [--as NAME]` brings the task's branches back into the
  repositories you work in. Fast-forward only: if the branch there is not an
  ancestor, it refuses, names both commits, and offers `--as`. It never forces
  a ref you own, and it checks the whole set before moving any of them.

## v0.2.0 — 2026-08-21

Claude Code integration, and a command that reconciles what wkt thinks with
what is on disk.

### Claude Code worktree hooks

`claude --worktree` normally cuts a worktree from the one repository the
session was launched in. Point its hooks at wkt — `wkt hook install` prints the
block — and a session started in a multi-repo workspace lands in a task tree
covering every repository, all on one branch. Verified end to end against
Claude Code 2.1.238.

- Re-firing the event returns the same tree, since `--resume --worktree` fires
  it again.
- A suggested slug carrying a separator is sanitised rather than refused: the
  contract has no channel for "I renamed your slug".
- The removal path keeps every teardown refusal and puts the reason on stderr,
  where Claude Code shows it to you.
- Claude Code's own `.claude/.cc-writes` no longer blocks removal. It appears
  the first time an agent edits a file, so without this every task a session
  had actually worked in needed `--force`. Found by running the real thing.

### `wkt doctor`

Reconciles state against the disk: tasks whose tree is gone, directories in
`trees/` that no task claims, and base pins left in your repositories by tasks
that no longer exist. `--fix` repairs only what is unambiguous and never
removes anything that could hold work.

`wkt doctor --all` is the uninstall answer: it lists every `refs/wkt/*` wkt has
written into your own repositories, whether or not it is a problem. A tool that
writes into someone else's repository should be able to say exactly what it
put there.

### Fixes

- A command **waits** for another wkt process (up to 10s) instead of failing
  the moment two runs overlap. The premise is two agents at once, and a
  set-level operation takes well under a second — but it gives up rather than
  hanging forever on a stale holder.
- **Loose files over 1 MiB are linked into the tree, not copied.** Copying is
  right for what a task edits and wrong for what it only reads: the first real
  workspace this ran against had 19 slide images in an ancestor directory and
  copied all of them into every task. Measured there: 3.3M and 41 files down to
  1.7M and 35.
- `wkt init` **warns about a repository deeper than the discovery bound.** Such
  a repository makes its containing directory unlinkable — wkt will not share it
  writably with every task — and that refusal used to arrive at the first
  `wkt new` instead.

## v0.1.1 — 2026-08-21

- `wkt version` now reports the tag a `go install github.com/Venut-Labs/wkt/cmd/wkt@v0.1.1`
  build came from. v0.1.0 said `dev` there, because that install path passes no
  ldflags; the version now falls back to the toolchain's own build info.

## v0.1.0 — 2026-08-21

First release. `wkt` adopts a multi-repo workspace, gives each task its own
tree of real git worktrees laid out in the workspace's shape, and refuses to
remove anything that would lose work.

### Commands

- `wkt init` — discover repositories, refuse genuine nested ones
  (`--exclude` adopts the workspace without them, recorded in container state),
  create the container. `--dry-run` writes nothing.
- `wkt new` — two-phase create with full rollback: nothing is left behind on
  failure. Warns when a selected repository carries a submodule, because `rm`
  refuses on one even with `--force`.
- `wkt path`, `wkt status` — where a task lives, and what has drifted.
- `wkt rm` — refuse-only teardown. Enumerates the filesystem rather than
  trusting its own state, and `--force` moves the tree into staging in one
  rename before deleting anything.
- `wkt perimeter` — generate or check the per-task settings that keep an agent
  from casually writing into the workspace or another task's tree.

### What it deliberately does not claim

Not a security boundary, and no read-only workspace: an agent inside a task
tree can still write into the workspace through a back-fill link. A perimeter
file covers only the directory it sits in, and `status` reports which
directories those are rather than implying protection.

### Verified rather than asserted

The behaviour of the Claude Code surfaces this depends on was measured against
release 2.1.238, not taken from documentation: deny rules reach the Bash
sandbox, a perimeter does not cover subdirectories, a narrower allow does not
escape a broader deny, and the deny list works to roughly 5,000 paths before
the profile stops compiling.

Tested by 12 packages of unit tests and a 12-scenario acceptance battery that
drives the real binary through real git repositories.
