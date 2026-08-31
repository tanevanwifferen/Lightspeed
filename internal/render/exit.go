package render

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os/exec"
)

// The exit-code taxonomy of PLAN §4. These are the only values
// lightspeed ever exits with.
const (
	// ExitOK: the command did what was asked.
	ExitOK = 0
	// ExitProblems: the command ran and the answer is bad news —
	// diagnostics found, no references at that position, an edit set
	// rejected. The tool worked; the code (or the question) did not.
	ExitProblems = 1
	// ExitUsage: the invocation was wrong. No server was consulted.
	ExitUsage = 2
	// ExitNoServer: no language server could answer, and installing or
	// configuring one would fix it.
	ExitNoServer = 3
	// ExitCrash: a server crashed, a deadline expired, or lightspeed
	// itself failed. The result is unknown, not empty.
	ExitCrash = 4
	// ExitNotReady: the server is still indexing and any answer would
	// be of unknown authority (PLAN §5.2). Never conflated with
	// ExitProblems: an empty result and an unready server are the two
	// things an agent must not confuse.
	ExitNotReady = 5
)

// exitCodes maps every Code to its exit code. Codes absent from this
// table are treated as ExitCrash by ExitCodeForCode; codes_test.go
// asserts that no declared constant is missing.
var exitCodes = map[Code]int{
	CodeUsage:              ExitUsage,
	CodeInvalidPosition:    ExitUsage,
	CodeInvalidFormat:      ExitUsage,
	CodeUnsupportedFormat:  ExitUsage,
	CodeNoSuchFile:         ExitUsage,
	CodeNoServer:           ExitNoServer,
	CodeServerNotInstalled: ExitNoServer,
	CodeUnsupportedMethod:  ExitNoServer,
	CodeOffline:            ExitNoServer,
	CodeNotReady:           ExitNotReady,
	CodeTimeout:            ExitCrash,
	CodeCancelled:          ExitCrash,
	CodeSpawnFailed:        ExitCrash,
	CodeServerCrash:        ExitCrash,
	CodeProtocolError:      ExitCrash,
	CodeIOError:            ExitCrash,
	CodeInternal:           ExitCrash,
	CodeProblemsFound:      ExitProblems,
	CodeDiagnosticsFound:   ExitProblems,
	CodeServerError:        ExitProblems,
	CodeNotFound:           ExitProblems,
	CodeEditConflict:       ExitProblems,
	CodeDirtyWorktree:      ExitProblems,
}

// ExitCodeForCode maps a machine-readable code to a process exit code.
//
// An unrecognised code yields ExitCrash rather than ExitProblems: an
// unclassified failure is indistinguishable from a malfunction, and
// reporting a malfunction as "problems found" would tell a caller the
// tool worked when we do not know that.
func ExitCodeForCode(c Code) int {
	if exit, ok := exitCodes[c]; ok {
		return exit
	}
	return ExitCrash
}

// ExitCode maps an error to its process exit code. It is the single
// mapping function of PLAN §4: every command's return value goes
// through here, so the taxonomy cannot drift between subcommands.
//
// Resolution order:
//
//  1. nil is ExitOK.
//  2. Any error in the chain implementing interface{ ExitCode() int }
//     decides, verbatim. This is the cross-package contract: a
//     readiness error from internal/client returns 5 without that
//     package importing render.
//  3. Any error in the chain implementing Coder is mapped through
//     ExitCodeForCode.
//  4. Well-known sentinels are classified by kind: expired and
//     cancelled contexts are ExitCrash, a missing executable is
//     ExitNoServer, a missing file is ExitUsage, a truncated stream is
//     ExitCrash.
//  5. Anything else is ExitCrash — an unclassified error is a bug in
//     lightspeed, which is exactly what exit 4 means.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}

	// (2) The error names its own exit code. Declared inline as an
	// anonymous interface so no other package needs render to
	// participate in the contract.
	var exiter interface{ ExitCode() int }
	if errors.As(err, &exiter) {
		return exiter.ExitCode()
	}

	// (3) The error names a code and we map it.
	var coder Coder
	if errors.As(err, &coder) {
		return ExitCodeForCode(coder.ErrorCode())
	}

	// (4) Classify by kind.
	switch {
	case errors.Is(err, context.DeadlineExceeded),
		errors.Is(err, context.Canceled),
		errors.Is(err, io.ErrUnexpectedEOF):
		return ExitCrash
	case errors.Is(err, exec.ErrNotFound):
		return ExitNoServer
	case errors.Is(err, fs.ErrNotExist):
		return ExitUsage
	}

	// (5) Unknown.
	return ExitCrash
}

// CodeForError maps an error to the machine-readable code that belongs
// in a failed envelope, using the same classification as ExitCode.
// Errors that name neither a code nor an exit code become CodeInternal,
// except for the well-known sentinels.
func CodeForError(err error) Code {
	if err == nil {
		return ""
	}

	var coder Coder
	if errors.As(err, &coder) {
		return coder.ErrorCode()
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return CodeTimeout
	case errors.Is(err, context.Canceled):
		return CodeCancelled
	case errors.Is(err, exec.ErrNotFound):
		return CodeServerNotInstalled
	case errors.Is(err, fs.ErrNotExist):
		return CodeNoSuchFile
	case errors.Is(err, io.ErrUnexpectedEOF):
		return CodeProtocolError
	}

	// An error that named an exit code but no error code: keep the
	// exit code authoritative and describe the code from it.
	var exiter interface{ ExitCode() int }
	if errors.As(err, &exiter) {
		if c, ok := codeForExit(exiter.ExitCode()); ok {
			return c
		}
	}

	return CodeInternal
}

// codeForExit picks a representative code for a bare exit code, for
// errors that implement ExitCoder but not Coder.
func codeForExit(exit int) (Code, bool) {
	switch exit {
	case ExitOK:
		return "", false
	case ExitProblems:
		return CodeProblemsFound, true
	case ExitUsage:
		return CodeUsage, true
	case ExitNoServer:
		return CodeNoServer, true
	case ExitNotReady:
		return CodeNotReady, true
	case ExitCrash:
		return CodeInternal, true
	}
	return "", false
}
