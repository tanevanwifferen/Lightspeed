package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"unicode"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// An Instance is a language server that has been started but not yet
// spoken to: the connection to drive it, and the reaping half of its
// lifecycle.
type Instance struct {
	// Conn is the JSON-RPC connection to the server. The pool
	// performs the LSP handshake on it with client.Connect.
	Conn *client.Conn

	// Wait reaps whatever Launch started, and is called after the
	// LSP `shutdown`/`exit` exchange. A launcher that started no
	// process may leave it nil.
	Wait func(ctx context.Context) error

	// Report says how the process ended and what it last wrote to
	// stderr; the pool reads it, after Wait, to explain a server that
	// died during startup. A launcher whose server is not a process may
	// leave it nil.
	Report func() client.ExitReport
}

// A Launcher starts the language server described by def, to serve the
// workspace rooted at root. It is the one seam the daemon needs for
// hermetic tests: the default implementation spawns a subprocess, and
// a test substitutes an in-process scripted server (internal/fakeserver)
// over a pipe, with whatever startup delay it wants to imitate.
type Launcher func(ctx context.Context, def *serverdef.ServerDef, root string) (*Instance, error)

// ExecLauncher runs def.Server.Command as a subprocess speaking LSP
// over stdio, forwarding the server's stderr to the given writer —
// which in a daemon is its log, and must never be the stdout carrying
// the JSON envelope.
func ExecLauncher(stderr io.Writer) Launcher {
	return func(ctx context.Context, def *serverdef.ServerDef, root string) (*Instance, error) {
		srv, err := client.StartCommand(def.Server.Command, stderr)
		if err != nil {
			return nil, err
		}
		return &Instance{Conn: srv.Conn, Wait: srv.Wait, Report: srv.Report}, nil
	}
}

// launcherFor picks the launcher to use, defaulting to [ExecLauncher].
func launcherFor(l Launcher, stderr io.Writer) Launcher {
	if l != nil {
		return l
	}
	return ExecLauncher(stderr)
}

// classifyLaunchError turns a failure to start a server into an error
// that keeps its exit code across the socket: a server that is not
// installed is exit 3 with a code the CLI can turn into "run this mise
// command", anything else is a crash.
func classifyLaunchError(def *serverdef.ServerDef, err error) error {
	if isNotFound(err) {
		return &Error{
			Code:    CodeServerNotInstalled,
			Message: fmt.Sprintf("server %q: command %q not found on PATH", def.Name, def.Server.Command[0]),
			Exit:    exitNoServer,
		}
	}
	return &Error{
		Code:    CodeSpawnFailed,
		Message: fmt.Sprintf("server %q: %v", def.Name, err),
		Exit:    exitCrash,
	}
}

// spawnFailure is the error for a server that started and then could not
// be initialized: the handshake's own failure, and — when the process is
// the reason — how it ended and what it said.
//
// "connection closed" alone is a dead end; the reason is in the process's
// exit status and its stderr, which used to go only to the daemon's log.
// The message carries a one-line summary and error.data the whole tail,
// so the same failure reads the same in a daemon and with --no-daemon.
func spawnFailure(def *serverdef.ServerDef, root string, cause error, rep client.ExitReport) *Error {
	msg := fmt.Sprintf("server %q: initialize failed: %v", def.Name, cause)
	data := map[string]any{"server": def.Name, "command": def.Server.Command}
	if rep.Exited {
		msg += fmt.Sprintf(" (the server exited: %s)", rep.Status)
		data["exit_status"] = rep.Status
		data["exit_code"] = rep.Code
	}
	if rep.Stderr != "" {
		msg += "; its stderr: " + stderrSummary(rep.Stderr)
		data["stderr_tail"] = rep.Stderr
		if rep.StderrTruncated {
			data["stderr_truncated"] = true
		}
	}
	raw, _ := json.Marshal(data) // strings, ints and a string slice
	return &Error{
		Code:    CodeSpawnFailed,
		Message: msg,
		Exit:    exitCrash,
		Server:  def.Name,
		Root:    root,
		Data:    raw,
	}
}

// summaryLimit bounds the stderr excerpt in an error's one-line message.
const summaryLimit = 200

// stderrSummary is the first and the last informative line of s, each on one
// line and bounded: the first is usually the complaint and the last the
// final word, and either alone can be a generic line. One line is shown once.
// A line is informative when it has a letter or a digit in it: a rule of dashes
// or a row of carets under a code excerpt says nothing, and a summary that
// opens with one is a summary of the decoration.
func stderrSummary(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); informativeLine(l) {
			lines = append(lines, clip(l))
		}
	}
	switch len(lines) {
	case 0:
		return ""
	case 1:
		return lines[0]
	}
	return lines[0] + " … " + lines[len(lines)-1]
}

func informativeLine(l string) bool {
	return strings.IndexFunc(l, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0
}

func clip(line string) string {
	if r := []rune(line); len(r) > summaryLimit {
		return string(r[:summaryLimit]) + "…"
	}
	return line
}

func isNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound)
	}
	return errors.Is(err, exec.ErrNotFound)
}
