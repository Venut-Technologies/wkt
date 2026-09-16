package gitx

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Venut-Technologies/wkt/internal/wkterr"
)

func Run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(errb.String())
		if stderr == "" {
			// Fallback to exec error when stderr is empty
			stderr = err.Error()
		}
		return "", wkterr.New("WKT_GIT_FAILED", "git "+args[0]+" failed").
			WithPath(dir).WithFound(firstUsefulLine(stderr))
	}
	return strings.TrimSpace(out.String()), nil
}

// RunStdin is Run with input on stdin, for the git commands that read a list
// of paths that way rather than as arguments — which is also how they avoid
// the argument-length limit on a large repository.
func RunStdin(dir, stdin string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(errb.String())
		if stderr == "" {
			stderr = err.Error()
		}
		return out.String(), wkterr.New("WKT_GIT_FAILED", "git "+args[0]+" failed").
			WithPath(dir).WithFound(firstUsefulLine(stderr))
	}
	return out.String(), nil
}

func RunOK(dir string, args ...string) bool {
	_, err := Run(dir, args...)
	return err == nil
}

func parseVersion(out string) (int, int, error) {
	fields := strings.Fields(out) // "git version 2.50.1"
	if len(fields) < 3 {
		return 0, 0, wkterr.New("WKT_GIT_VERSION", "cannot parse git version").WithFound(out)
	}
	parts := strings.Split(fields[2], ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, wkterr.New("WKT_GIT_VERSION", "cannot parse git version").WithFound(out)
	}
	minor := 0
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}
	return major, minor, nil
}

func Version() (int, int, error) {
	out, err := Run(".", "--version")
	if err != nil {
		return 0, 0, err
	}
	return parseVersion(out)
}

// firstUsefulLine picks the one line of git's stderr worth showing, and takes
// the user's secrets out of it.
//
// Only one line is shown at all, because stderr carries absolute paths
// belonging to other tasks (spec §5.5). Which line matters more than it looks:
// when a content filter fails, git prints the filter's own progress output
// first, its configured command line second, and the reason last. Reporting
// the first line — which wkt did — told the user "fetching objects" and never
// mentioned a filter; reporting the second echoes whatever is in that command,
// which is where people put access tokens.
func firstUsefulLine(stderr string) string {
	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	for _, l := range lines {
		if strings.HasPrefix(l, "fatal:") {
			return redactFilterCommand(l)
		}
	}
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			return redactFilterCommand(l)
		}
	}
	return ""
}

// redactFilterCommand removes the configured command from git's
// "external filter '<command>' failed" message. A filter that is not marked
// required produces that line and no fatal line at all — measured — so this
// is the case where the leaking line is the only one there is.
//
// The command may itself contain quotes, so the closing delimiter is found
// from the right: git always appends "' failed".
func redactFilterCommand(line string) string {
	const open = "external filter '"
	i := strings.Index(line, open)
	if i < 0 {
		return line
	}
	rest := line[i+len(open):]
	j := strings.LastIndex(rest, "' failed")
	if j < 0 {
		return line[:i+len(open)] + "<configured command withheld>'"
	}
	return line[:i+len(open)] + "<configured command withheld>" + rest[j:]
}
