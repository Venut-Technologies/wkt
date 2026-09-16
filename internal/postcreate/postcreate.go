// Package postcreate runs the one script the design scopes in: "there is a
// post-create seam and a gitignored-file carry, and nothing else" (spec §1.1).
//
// A task tree is a set of fresh worktrees. The carry brings in the local files
// a service needs; nothing has been run. Every fresh tree then needs the same
// handful of commands — install dependencies, create a local database,
// generate a config — and a tool whose premise is that trees are cheap to make
// cannot leave that to the person each time.
//
// The script belongs to the developer and runs with their privileges. It has
// to: installing dependencies needs the network and the toolchain, and the
// sandbox a task tree carries refuses both. What this package owes them in
// return is that it never executes something they did not put there, and never
// hands the script a tree through which it can write into their own
// repositories.
package postcreate

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// ScriptRel is the seam, relative to the workspace root.
//
// A directory rather than a bare dotfile beside .wktinclude, so the script may
// keep helpers next to it. Verified that a .wkt/ in the workspace is not
// mirrored into task trees, so it cannot collide with a tree's own .wkt state
// mirror.
const ScriptRel = ".wkt/post-create"

// Request is everything one run needs.
type Request struct {
	Workspace string
	TreeRoot  string
	Task      string
	Repos     []string // materialised repositories, workspace-relative
	// AddedRepo names what wkt add just grafted on, newline-separated the way
	// Repos is, and is empty on create. It is what lets a script that must be
	// safe to run twice do only the new work the second time.
	AddedRepo string
	Out       io.Writer
	// Timeout stops the script after this long. Zero means no deadline, which
	// is the command line's default: nothing there imposes a ceiling, and
	// killing a legitimate twenty-minute install is worse than waiting.
	//
	// The Claude Code worktree hook is the exception. Measured on 2.1.239: a
	// hook was cancelled at 591 seconds — "Hook cancelled" — and the session
	// got no worktree at all, though wkt had already built one. wkt has to
	// finish first, because a script wkt stops leaves a usable tree and a
	// warning, while one Claude Code stops leaves the session with nothing.
	Timeout time.Duration
}

// Result reports what happened. ExitCode and Tail are meaningful only when the
// script ran and returned non-zero.
type Result struct {
	Ran      bool
	ExitCode int
	Tail     string
}

// SafeName reports whether a task name may be handed to a script.
//
// Deliberately narrower than a branch name. Measured: wkt new accepts "a;b",
// "a$b", "a&b" and "a`b`" — all legal branch names, all of which reach the
// script as WKT_TASK and as the last segment of WKT_TREE. wkt is not the one
// at risk, since the script is executed directly rather than through a shell;
// the script is, the moment it expands either without quotes, and "rm -rf
// $WKT_TREE" in a cleanup path is the obvious way to be hurt by that.
//
// The restriction lives here rather than in task.Validate: the seam is what
// introduces the hazard, and narrowing every task name for everyone because of
// an optional feature would be the wrong trade.
func SafeName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-':
		default:
			return false
		}
	}
	return true
}

// Run executes the seam, if the workspace has one.
func Run(req Request) (Result, error) {
	path := filepath.Join(req.Workspace, filepath.FromSlash(ScriptRel))
	// Lstat, not Stat: a symlink here is not a file wkt may execute through.
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return Result{}, nil // opt-in, and costs nothing unused
	}
	if err != nil {
		return Result{}, wkterr.New("WKT_POST_CREATE", "cannot inspect the post-create script").
			WithPath(path).WithFound(err.Error())
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return Result{}, wkterr.New("WKT_POST_CREATE_FOREIGN",
			"the post-create script is a symlink; wkt will not execute through it").
			WithPath(path).
			WithRemedy("replace the symlink with a real file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return Result{}, wkterr.New("WKT_POST_CREATE_NOT_EXECUTABLE",
			"the post-create script is not executable").
			WithPath(path).
			WithRemedy("chmod +x " + path)
	}
	if !SafeName(req.Task) {
		return Result{}, wkterr.New("WKT_POST_CREATE_UNSAFE_NAME",
			"this task's name cannot be handed to a script safely").
			WithFound(req.Task).
			WithRemedy("use a task name of letters, digits, dot, dash and underscore",
				"or remove "+path+" if this workspace does not want a post-create step")
	}
	return run(path, req)
}

// run executes the script.
//
// The output is streamed rather than collected: an install runs for minutes,
// and output shown only at the end is indistinguishable from a hang. A copy of
// the tail is kept anyway, because the failure is read by an agent that cannot
// see the terminal, and an exit status on its own is nothing it can act on.
//
// No timeout. Any number would be arbitrary, and killing a legitimate
// twenty-minute install is worse than waiting: the output streams, so the run
// is visibly alive, and Ctrl-C works.
func run(path string, req Request) (Result, error) {
	tail := &tailBuffer{limit: 4096}
	ctx := context.Background()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, path)
	// Its own process group, killed as a group. A setup script spawns npm and
	// git; killing only the shell leaves those running, and they hold the
	// output pipe open, so Wait would block until they finished anyway —
	// which is the deadline not being enforced at all.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	// And a backstop: if something still holds the output pipe after the script
	// itself is done, stop waiting for it. A setup script's ordinary last act
	// is to leave something running — a dev server, "docker compose up -d", a
	// watcher — and that child inherits the pipe, so without this wkt would
	// wait for the dev server to exit.
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = req.TreeRoot
	cmd.Stdout = io.MultiWriter(req.Out, tail)
	cmd.Stderr = cmd.Stdout
	// The developer's own environment, whole. The script is theirs; scrubbing
	// PATH, or the token a private registry needs, breaks exactly what it is
	// for.
	cmd.Env = append(os.Environ(),
		"WKT_TASK="+req.Task,
		"WKT_TREE="+req.TreeRoot,
		"WKT_WORKSPACE="+req.Workspace,
		"WKT_REPOS="+strings.Join(req.Repos, "\n"),
		"WKT_ADDED_REPO="+req.AddedRepo,
	)

	// Signals reach wkt, not the script. The script has its own process group,
	// so the terminal's Ctrl-C never gets there — and wkt dying on the spot
	// would skip the caller's restore of the tree's back-fill links, leaving
	// the tree missing them with nothing to say so. Take the signal, pass it
	// on, and let the run end normally so the cleanup happens on the way out.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	var interrupted atomic.Bool
	watched := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			interrupted.Store(true)
			// Hand wkt's own disposition back immediately: a second Ctrl-C
			// from someone who has lost patience should kill wkt outright
			// rather than be swallowed here.
			signal.Stop(sigs)
			if cmd.Process != nil {
				if s, ok := sig.(syscall.Signal); ok {
					_ = syscall.Kill(-cmd.Process.Pid, s)
				}
			}
		case <-watched:
		}
	}()

	err := cmd.Run()
	close(watched)

	if interrupted.Load() {
		return Result{Ran: true, Tail: tail.String()},
			wkterr.New("WKT_POST_CREATE_INTERRUPTED", "the post-create script was interrupted").
				WithPath(path).
				WithDetail(tail.String()).
				WithRemedy("the task was created and its tree is usable, but its setup did not finish",
					"wkt post-create "+req.Task+" runs it again")
	}
	if err == nil {
		return Result{Ran: true}, nil
	}
	if ctx.Err() == context.DeadlineExceeded {
		return Result{Ran: true, Tail: tail.String()},
			wkterr.New("WKT_POST_CREATE_TIMEOUT", "the post-create script ran out of time and was stopped").
				WithPath(path).
				WithFound(req.Timeout.String()).
				WithDetail(tail.String()).
				WithRemedy("the task was created and its tree is usable, but its setup did not finish",
					"wkt post-create "+req.Task+" runs it again, without a deadline")
	}

	// Work out what the script itself did, which is not always what cmd.Run
	// returns. A child the script left running — a dev server, a watcher —
	// inherits the output pipe, so Wait cannot finish and the WaitDelay
	// backstop fires with ErrWaitDelay even though the script exited cleanly.
	// Reporting that as a failure breaks every setup that starts a service.
	var exit *exec.ExitError
	switch {
	case errors.As(err, &exit):
	case errors.Is(err, exec.ErrWaitDelay) && cmd.ProcessState != nil:
	default:
		// Never started: a missing interpreter, or a file that is executable
		// but not runnable on this machine.
		return Result{Ran: true}, wkterr.New("WKT_POST_CREATE_FAILED",
			"the post-create script could not be run").
			WithPath(path).WithFound(err.Error()).
			WithRemedy("check that its interpreter exists and that the file is runnable here")
	}

	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		return Result{Ran: true}, nil
	}
	return Result{Ran: true, ExitCode: code, Tail: tail.String()},
		wkterr.New("WKT_POST_CREATE_FAILED", "the post-create script failed").
			WithPath(path).
			WithFound("exit status "+strconv.Itoa(code)).
			WithDetail(tail.String()).
			WithRemedy("the task was created and its tree is usable; fix the script, then wkt post-create "+req.Task,
				// Never "run it yourself": a hand-run script does not get the
				// back-fill links withdrawn, so the loop everyone writes would
				// install into the developer's own repositories — the hazard
				// this package exists to close.
				"or pass --no-post-create to skip it next time")
}

// tailBuffer keeps the last limit bytes written through it, so a failure can
// quote the end of an install log without holding all of it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }
