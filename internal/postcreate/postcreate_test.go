package postcreate

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

// script writes an executable post-create into a workspace.
func script(t *testing.T, ws, body string) string {
	t.Helper()
	dir := filepath.Join(ws, ".wkt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "post-create")
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var e *wkterr.E
	if !errors.As(err, &e) {
		t.Fatalf("not a wkt error: %v", err)
	}
	return e.Code
}

// The seam is opt-in, and the common case is not having one.
func TestAbsentScriptIsANoOp(t *testing.T) {
	res, err := Run(Request{Workspace: t.TempDir(), TreeRoot: t.TempDir(), Task: "t", Out: &bytes.Buffer{}})
	if err != nil {
		t.Fatalf("an absent script is not an error: %v", err)
	}
	if res.Ran {
		t.Fatal("nothing should have run")
	}
}

// A guard that quietly does nothing is the worst kind a guard can be — this
// project has already been bitten by one, in a deny rule that was accepted
// and enforced nothing. A script someone wrote and forgot to chmod says so.
func TestNonExecutableScriptIsRefused(t *testing.T) {
	ws := t.TempDir()
	p := script(t, ws, "#!/bin/sh\ntrue\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Request{Workspace: ws, TreeRoot: t.TempDir(), Task: "t", Out: &bytes.Buffer{}})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_NOT_EXECUTABLE" {
		t.Fatalf("want WKT_POST_CREATE_NOT_EXECUTABLE, got %q (err %v)", code, err)
	}
}

// wkt does not execute through a link whose target it has not established,
// the same rule perimeter.checkOwned follows for the settings file.
func TestSymlinkedScriptIsRefused(t *testing.T) {
	ws := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ws, ".wkt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(ws, ".wkt", "post-create")); err != nil {
		t.Fatal(err)
	}
	_, err := Run(Request{Workspace: ws, TreeRoot: t.TempDir(), Task: "t", Out: &bytes.Buffer{}})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_FOREIGN" {
		t.Fatalf("want WKT_POST_CREATE_FOREIGN, got %q (err %v)", code, err)
	}
}

// Measured: wkt new accepts a;b, a$b, a&b and a`b`, all legal branch names.
// wkt itself is safe — the script is executed directly, never through a
// shell — but handing a script a WKT_TREE with metacharacters in it arms
// every unquoted expansion the script contains, and "rm -rf $WKT_TREE" in a
// cleanup path is the obvious way to be hurt by that.
func TestUnsafeTaskNameIsRefusedBeforeAnythingRuns(t *testing.T) {
	ws := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	script(t, ws, "#!/bin/sh\ntouch "+marker+"\n")
	_, err := Run(Request{Workspace: ws, TreeRoot: t.TempDir(), Task: "a;b", Out: &bytes.Buffer{}})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_UNSAFE_NAME" {
		t.Fatalf("want WKT_POST_CREATE_UNSAFE_NAME, got %q (err %v)", code, err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the script ran despite the refusal")
	}
}

func TestSafeNameAcceptsWhatTasksActuallyUse(t *testing.T) {
	for _, ok := range []string{"feat-42", "feat_42", "feat.42", "Feat42"} {
		if !SafeName(ok) {
			t.Fatalf("%q must be accepted", ok)
		}
	}
	for _, bad := range []string{"a;b", "a$b", "a&b", "a`b`", "a b", "", "a/b", "a\nb"} {
		if SafeName(bad) {
			t.Fatalf("%q must be refused", bad)
		}
	}
}

// The script is told where it is and what it is setting up. The working
// directory is checked by creating a file rather than by comparing pwd to the
// temporary path: on macOS pwd resolves /var to /private/var, and a test that
// compares those strings fails for a reason that has nothing to do with wkt.
func TestScriptGetsTheTreeAndTheTaskDescribed(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\ntouch here-is-cwd\n"+
		"{ echo \"$WKT_TASK\"; echo \"$WKT_WORKSPACE\"; echo \"$WKT_REPOS\"; echo \"added=$WKT_ADDED_REPO\"; echo \"tree=$WKT_TREE\"; } > seen\n")

	var out bytes.Buffer
	res, err := Run(Request{
		Workspace: ws, TreeRoot: tree, Task: "feat-42",
		Repos: []string{"docs", "services/svc-a"}, AddedRepo: "services/svc-a", Out: &out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ran {
		t.Fatal("the script should have run")
	}
	if _, statErr := os.Stat(filepath.Join(tree, "here-is-cwd")); statErr != nil {
		t.Fatal("the script did not run with the tree root as its working directory")
	}
	seen, err := os.ReadFile(filepath.Join(tree, "seen"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(seen)
	for _, want := range []string{"feat-42", ws, "docs\nservices/svc-a", "added=services/svc-a", "tree=" + tree} {
		if !strings.Contains(got, want) {
			t.Fatalf("the script did not see %q; it saw:\n%s", want, got)
		}
	}
}

// wkt new prints exactly the tree path to stdout, and so does the
// worktree-create hook, which Claude Code reads as the worktree path. A
// script echoing into wkt's stdout would break both.
func TestScriptOutputGoesToTheWriterNotStdout(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\necho to-stdout\necho to-stderr >&2\n")
	var out bytes.Buffer
	if _, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out}); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "to-stdout") || !strings.Contains(got, "to-stderr") {
		t.Fatalf("both of the script's streams must reach the writer; got %q", got)
	}
}

// An agent reads this failure. A bare exit status tells it nothing it can act
// on, so the script's own words have to survive.
func TestNonZeroExitCarriesTheStatusAndTheOutput(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\necho 'could not reach the registry' >&2\nexit 7\n")
	var out bytes.Buffer
	res, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_FAILED" {
		t.Fatalf("want WKT_POST_CREATE_FAILED, got %q (err %v)", code, err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("want exit 7, got %d", res.ExitCode)
	}
	var e *wkterr.E
	errors.As(err, &e)
	if !strings.Contains(e.Detail, "could not reach the registry") {
		t.Fatalf("the script's own words must survive; detail was %q", e.Detail)
	}
	if !strings.Contains(e.Found, "7") {
		t.Fatalf("the exit status must be named; found was %q", e.Found)
	}
}

// Measured on Claude Code 2.1.239: a WorktreeCreate hook was cancelled at 591
// seconds — "Hook cancelled", and the session got no worktree at all, though
// wkt had built one. So on that path wkt must finish first: a script stopped
// by wkt leaves a usable tree and a warning, where one stopped by Claude Code
// leaves the session with nothing.
func TestARunCanBeGivenADeadline(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\nsleep 30\ntouch \"$WKT_TREE/finished\"\n")
	var out bytes.Buffer
	start := time.Now()
	res, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out, Timeout: 300 * time.Millisecond})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_TIMEOUT" {
		t.Fatalf("want WKT_POST_CREATE_TIMEOUT, got %q (err %v)", code, err)
	}
	if !res.Ran {
		t.Fatal("it did run; it was stopped")
	}
	// Tight on purpose. Killing only the shell leaves "sleep" holding the
	// output pipe, and Wait then blocks until the WaitDelay backstop fires
	// seconds later — which looks like enforcement and is not. Only killing
	// the process group returns promptly, and a real script's npm and git are
	// exactly the children this is about.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("the deadline was not enforced promptly: %s", elapsed)
	}
	if _, statErr := os.Stat(filepath.Join(tree, "finished")); statErr == nil {
		t.Fatal("the script was not actually stopped")
	}
}

// No deadline is the command-line default: there is no external ceiling
// there, and killing a legitimate twenty-minute install is worse than waiting.
func TestZeroTimeoutMeansNoDeadline(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\nsleep 1\ntouch \"$WKT_TREE/finished\"\n")
	var out bytes.Buffer
	if _, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out}); err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(tree, "finished")); statErr != nil {
		t.Fatal("a run with no deadline must be allowed to finish")
	}
}

// A setup script's ordinary last act is to leave something running — a dev
// server, "docker compose up -d", a watcher. That child inherits the output
// pipe, so Wait cannot finish and the WaitDelay backstop fires. The backstop
// is right (otherwise wkt hangs on the dev server); reporting it as a failure
// is not: the script exited 0.
func TestABackgroundChildDoesNotTurnSuccessIntoFailure(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\nsleep 8 &\nexit 0\n")
	var out bytes.Buffer
	res, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out})
	if err != nil {
		t.Fatalf("the script exited 0; wkt must not report a failure: %v", err)
	}
	if !res.Ran || res.ExitCode != 0 {
		t.Fatalf("want a clean run, got %+v", res)
	}
}

// And the backstop still has to do its job: wkt must not wait on a child the
// script left running.
func TestWktDoesNotWaitForWhatTheScriptLeftRunning(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\nsleep 8 &\nexit 0\n")
	var out bytes.Buffer
	start := time.Now()
	if _, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("wkt waited for the background child: %s", elapsed)
	}
}

// A script that fails is still a failure, background child or not.
func TestABackgroundChildDoesNotHideAFailure(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\nsleep 8 &\nexit 4\n")
	var out bytes.Buffer
	res, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out})
	if code := codeOf(t, err); code != "WKT_POST_CREATE_FAILED" {
		t.Fatalf("want WKT_POST_CREATE_FAILED, got %q", code)
	}
	if res.ExitCode != 4 {
		t.Fatalf("want exit 4, got %d", res.ExitCode)
	}
}

// The script runs in its own process group, so the terminal's Ctrl-C never
// reaches it — and wkt, dying instantly on the same signal, would never run
// the deferred restore that puts the tree's back-fill links back. Both halves
// matter: forward the signal, then end the run normally so the caller can
// clean up on the way out.
func TestAnInterruptReachesTheScriptAndEndsTheRun(t *testing.T) {
	ws, tree := t.TempDir(), t.TempDir()
	script(t, ws, "#!/bin/sh\ntrap 'echo caught > \"$WKT_TREE/caught\"; exit 130' INT\nsleep 30\n")
	var out bytes.Buffer
	go func() {
		time.Sleep(400 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
	}()
	start := time.Now()
	res, err := Run(Request{Workspace: ws, TreeRoot: tree, Task: "t", Out: &out})
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the interrupt did not reach the script: %s", elapsed)
	}
	if code := codeOf(t, err); code != "WKT_POST_CREATE_INTERRUPTED" {
		t.Fatalf("want WKT_POST_CREATE_INTERRUPTED, got %q (err %v)", code, err)
	}
	if !res.Ran {
		t.Fatal("it did run; it was interrupted")
	}
	if _, statErr := os.Stat(filepath.Join(tree, "caught")); statErr != nil {
		t.Fatal("the script never saw the signal")
	}
}
