// Package cli wires lightspeed's command surface: flag parsing, the
// location syntax of PLAN §4, and the path every read-only command
// takes from an argument to an answer.
//
// One command is one pass through the same four collaborators, in this
// order and no other:
//
//  1. internal/router resolves the path to a server and a workspace
//     root, because a polyglot repo has no single server.
//  2. internal/daemon finds or starts that server — in the workspace's
//     shared daemon, or in this process with --no-daemon — and
//     internal/client has performed the handshake and recorded what it
//     advertised: a method the server never claimed is never called
//     (PLAN §5.4).
//  3. internal/docstore announces the document with didOpen and owns
//     the vendored gopls Mapper that converts the CLI's 1-based *byte*
//     columns to the LSP's UTF-16 positions (PLAN §5.1).
//  4. internal/client's readiness gate decides whether the answer may
//     be believed, and returns exit code 5 rather than an empty result
//     of unknown authority (PLAN §5.2).
//
// Only then does internal/render turn the answer into json, text or
// diff. This package therefore contains no position arithmetic, no
// output formatting and no protocol framing; when something here looks
// like one of those, it is a bug.
package cli

import (
	"io"
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// Exit codes, PLAN §4. They are aliases of internal/render's
// definitions rather than a second copy: render owns the taxonomy and
// the error-to-exit-code mapping, and two tables would eventually
// disagree.
const (
	ExitOK       = render.ExitOK       // success
	ExitProblems = render.ExitProblems // problems found, or an authoritative empty answer
	ExitUsage    = render.ExitUsage    // bad invocation
	ExitNoServer = render.ExitNoServer // no server available
	ExitCrash    = render.ExitCrash    // server crash or timeout
	ExitNotReady = render.ExitNotReady // workspace still indexing (PLAN §5.2)
)

// Main runs the CLI and returns the process exit code. Machine output
// (the JSON envelope, or the requested format) goes to stdout, human
// diagnostics and server logs to stderr, so that stdout can be parsed
// without filtering.
func Main(args []string, stdout, stderr io.Writer) int {
	return MainWithStdin(args, os.Stdin, stdout, stderr)
}

// MainWithStdin is Main with the input stream named explicitly. Only
// `batch` reads it — it is the whole point of that command — and every
// other command ignores it, so the ordinary entry point can keep the
// two-stream signature it has had since M0.
func MainWithStdin(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &env{stdin: stdin, stdout: stdout, stderr: stderr, noDaemon: envNoDaemon()}
	// --no-daemon is global, so it is accepted before the subcommand as
	// well as after it (every command registers it too, for the agent
	// that appends it to a line it already has).
	// --offline is global for the same reason.
	for len(args) > 0 && e.globalFlag(args[0]) {
		args = args[1:]
	}
	if len(args) == 0 {
		writeUsage(stderr)
		return e.usagef("missing subcommand (try: lightspeed help)")
	}
	switch args[0] {
	case "-h", "--help":
		writeUsage(stderr)
		return ExitOK
	case "-v", "--version":
		return versionCommand(e, lookupCommand("version"), args[1:])
	}
	c := lookupCommand(args[0])
	if c == nil {
		writeUsage(stderr)
		return e.usagef("unknown subcommand %q (known: %v)", args[0], commandNames())
	}
	return c.Run(e, c, args[1:])
}

// globalFlag consumes one of the flags that is accepted before the
// subcommand, reporting whether arg was one.
func (e *env) globalFlag(arg string) bool {
	switch arg {
	case noDaemonFlag, "-no-daemon":
		e.noDaemon = true
	case offlineFlag, "-offline":
		e.offline = true
	default:
		return false
	}
	return true
}

// usage emits a usage-error envelope and returns the usage exit code.
// Kept for the M0 `raw` command, which predates the env type.
func usage(stdout io.Writer, msg string) int {
	_ = render.Fail(stdout, render.CodeUsage, msg)
	return ExitUsage
}
