# wkt

[![CI](https://github.com/Venut-Technologies/wkt/actions/workflows/ci.yml/badge.svg)](https://github.com/Venut-Technologies/wkt/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/Venut-Technologies/wkt.svg)](https://pkg.go.dev/github.com/Venut-Technologies/wkt)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

One task, one branch, many repositories.

A change rarely fits in one repository. `wkt` gives each task its own tree
where every participating repository is a real git worktree on the task
branch, **laid out in the same shape as the workspace**, so relative paths
between repositories keep resolving. Repositories left out of the task are
symlinked at their mirrored positions, so a partial selection still gives a
complete tree.

`wkt` never writes into the workspace itself: task worktrees are cut from
per-repository bare stores kept in a container beside it. That is a promise
about `wkt`, not about everything running in a tree — the repositories a task
selected are isolated by git, and the ones it left out are reachable through
those symlinks. A Claude Code session is kept out of them by the perimeter
`wkt` writes; an editor, a `make` target or a plain shell is not. See
[what it does not promise](#what-it-does-not-promise).

```
~/work/                        the workspace, a plain directory
  services/svc-a/              a repository
  services/svc-b/              another
  shared/                      another
  CONVENTIONS.md               a loose file

~/work.worktrees/trees/feat-42/
  services/svc-a/              worktree from the store, branch feat-42
  services/svc-b -> ~/work/services/svc-b     not in this task
  shared         -> ~/work/shared             not in this task
  CONVENTIONS.md               copy, hash-recorded
```

## Status

v0: `init`, `new`, `path`, `status`, `rm`. Removal is **refuse-only** — it
enumerates the filesystem rather than trusting its own state file, and
refuses while anything in the tree would lose work.

macOS and Linux. Windows is out of scope (symlinks plus deletion semantics).

## Install

```sh
go install github.com/Venut-Technologies/wkt/cmd/wkt@latest
```

> **Installed from `github.com/Venut-Labs/wkt` before v0.7.0?** That path is
> frozen at v0.6.3 and gets no further releases. Install again with
> the command above; the binary keeps its name, so hooks already written into
> `~/.claude/settings.json` keep working if it lands in the same `GOBIN`.

Or build from a clone — standard library only, no third-party dependencies:

```sh
go build -o wkt ./cmd/wkt
go test ./...                                  # unit tests
WT_CMD=$PWD/wkt bash test/run.sh               # acceptance battery
```

Requires `git` 2.29 or newer (`git worktree repair`), macOS or Linux.

## Use

```sh
wkt init --workspace ~/work            # adopt: discover repositories, build the container
cd ~/work                              # every verb defaults to --workspace .
wkt new feat-42 --all                  # a task over every repository
wkt new feat-42 --repos services/svc-a,shared
cd "$(wkt path feat-42)"               # work here
wkt status                             # what exists, and what has drifted
wkt rm feat-42                         # refuses while anything would be lost
wkt rm feat-42 --force                 # override, behind a staging fence

wkt add feat-42 --repos shared         # graft another repository onto a live task
wkt sync feat-42                       # has the base moved on? (never moves it for you)
wkt fetch feat-42                      # bring the task's branches back, fast-forward only

wkt repair feat-42                     # after moving the workspace somewhere else
wkt perimeter --check                  # is each task's perimeter current?
wkt doctor                             # does state still match the disk?
wkt doctor --all                       # everything wkt has written into your repositories
```

`wkt doctor --all` is also the uninstall answer. wkt writes exactly one thing
into a repository of yours — a base pin under `refs/wkt/` — and `doctor` lists
every one of them, whether or not it is a problem, so trying the tool is not a
one-way door.

`init` refuses a genuine nested repository; `--exclude services/svc-a/vendored`
adopts the workspace without it and records the decision.

Exit codes: `0` consistent, `2` usage error or task already exists, `3` drift
detected, `4` no container (run `init`), `1` any other typed failure. Errors
are one line of JSON on stderr; a refusal that has several causes lists them
under `problems` (what is in the way), with `remedy` reserved for what to do.

## Carrying local files into a task

A worktree is a fresh checkout, so a task tree starts without the gitignored
files a service needs to run. Name them in a `.wktinclude` at the workspace
root, in gitignore syntax:

```
.env
.env.local
services/*/config/secrets.json
```

A file is carried only if a pattern matches it **and** git already ignores it,
so the mechanism can never shadow versioned content. Files are copied, never
linked: editing a symlinked secret inside a task would write back into your own
checkout. A carried file that the task has not changed does not block removal;
one it has changed does.

### Setting a tree up

A tree arrives with its files and nothing run: no dependencies installed, no
local database, no generated config. Put an executable `.wkt/post-create` at
the workspace root and wkt runs it once the tree is built, on `wkt new` and
again on `wkt add`:

```sh
#!/bin/sh
echo "$WKT_REPOS" | while read -r repo; do
  [ -f "$repo/package.json" ] && (cd "$repo" && npm install)
done
```

It runs with the tree root as its working directory and your own environment,
and it is told `WKT_TASK`, `WKT_TREE`, `WKT_WORKSPACE`, `WKT_REPOS`, and on
`wkt add` also `WKT_ADDED_REPO`. Everything it prints goes to stderr, because
`wkt new` prints exactly the tree path to stdout and nothing else.

Three things worth knowing:

- **Iterate `WKT_REPOS`, not the tree.** Repositories the task did not select
  are symlinks into your workspace, so `for d in */` would install into your
  own checkouts. wkt withdraws those links while the script runs and puts them
  back afterwards, so the loop cannot reach them — which also means the script
  cannot read an unselected repository.
- **It must be safe to run twice**, because `wkt add` runs it again for the
  whole tree. `WKT_ADDED_REPO` names what just arrived, if that helps.
- **A failure leaves the task standing.** wkt prints what the script said and
  exits non-zero; the tree, its branches and its store are all fine. Pass
  `--no-post-create` to skip the script entirely. On the Claude Code worktree
  hook, where there is no flag to pass, a failure is a warning and the session
  still gets its tree — and the script is stopped after eight minutes, because
  Claude Code cancels a hook at about ten and the session would then get no
  tree at all.

`wkt post-create TASK` runs the script again on a task that already exists.
Use it rather than running the script yourself: by hand it does not get the
back-fill links withdrawn, so the loop above would install into your own
checkouts.

What the script creates is remembered, so `wkt rm` treats it as disposable
rather than as work at risk. A task name that is not letters, digits, dot,
dash or underscore is refused before the script runs, because the name reaches
it as `$WKT_TREE`.

## With Claude Code

`claude --worktree` normally cuts a worktree from the one repository the
session was launched in. Point its worktree hooks at wkt and it asks wkt
instead, so a session started in a multi-repo workspace lands in a task tree
covering every repository, all on one branch:

```sh
wkt hook install        # prints the block to paste into ~/.claude/settings.json
cd ~/work && claude --worktree
```

Verified end to end against Claude Code 2.1.238. The teardown keeps its
refusals on that path too: `WorktreeRemove` will not delete a tree holding
uncommitted work or unpushed commits, and says why on stderr, where Claude
Code shows it to you.

If a repository carries its own `.claude/settings.json`, wkt leaves it alone
and does not cover that directory — it says so on stderr rather than
overwriting your configuration or refusing to build the task.

## What it does not promise

- **Not a security boundary**, and **no read-only workspace in v0**: an agent
  inside a task tree can write into the workspace through a back-fill link.
  `wkt` never writes there itself, and teardown never follows a link.
- **No conflict prevention.** Two tasks editing one file still conflict; the
  conflict moves to the eventual rebase.
- **No cross-repo atomic merge**, and **not a remote-side boundary** — a task
  tree can push to the real origin over HTTPS. An **SSH origin cannot be
  reached from inside a tree at all**: Claude Code's sandbox routes SSH
  through an HTTP proxy it cannot authenticate to, which no allowlist entry
  changes. Work still comes back with `wkt fetch`, which runs outside the
  sandbox, and pushes from your workspace as usual; `wkt new` warns when a
  repository has one.
- **No runtime environment**: ports, compose projects, databases and
  dependency installation are out of scope.
- **A task over a repository with submodules cannot be removed** until the
  submodule is deinitialised — `new` warns when it sees one.
- **Loose files over 1 MiB are linked into the tree rather than copied**, so a
  directory of images or datasets is not duplicated per task. Small files are
  still copied, and a diverged copy blocks teardown.

## License

Apache License 2.0 — see [LICENSE](LICENSE). Copyright 2026 Venut Technologies.

Apache-2.0 over MIT for one reason that matters to a tool people run at work:
it grants patent rights explicitly and terminates them for anyone who sues over
the software. Everything MIT permits — use, modify, redistribute, ship inside a
closed product — this permits too.

