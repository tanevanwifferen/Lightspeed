package render

import "fmt"

// Code is a machine-readable error code. Every failed envelope carries
// exactly one; agents branch on the code, humans read the message, and
// neither ever sees a stack trace (PLAN §4).
//
// Codes are stable identifiers: add new ones rather than repurposing
// existing ones, and keep them in exitCodes below so that
// ExitCodeForCode never has to guess.
type Code string

// The error code taxonomy. The comment on each constant gives the exit
// code it maps to; exitCodes is the authoritative table.
const (
	// -- exit 2, usage: the invocation is wrong and no server was consulted.

	// CodeUsage is a malformed command line: unknown subcommand,
	// bad flag, missing argument.
	CodeUsage Code = "usage"
	// CodeInvalidPosition is a location argument that does not parse
	// as a span, or that points outside the file.
	CodeInvalidPosition Code = "invalid_position"
	// CodeInvalidFormat is an unknown --format value.
	CodeInvalidFormat Code = "invalid_format"
	// CodeUnsupportedFormat is a --format value that is valid but
	// meaningless for this command (sarif for references, say).
	CodeUnsupportedFormat Code = "unsupported_format"
	// CodeNoSuchFile is a path argument that does not exist.
	CodeNoSuchFile Code = "no_such_file"

	// -- exit 3, no server: nothing is able to answer, and installing
	// something would fix it.

	// CodeNoServer is "no server definition matches this file".
	CodeNoServer Code = "no_server"
	// CodeServerNotInstalled is "a server matches, but its command is
	// not on PATH". The message must carry the exact install command.
	CodeServerNotInstalled Code = "server_not_installed"
	// CodeUnsupportedMethod is "the server does not advertise the
	// capability this command needs" (PLAN §5.4: never call
	// uncapabilitied methods).
	CodeUnsupportedMethod Code = "unsupported_method"
	// CodeOffline is "this would need the network and --offline is set".
	CodeOffline Code = "offline"

	// -- exit 5, not ready: the answer would be of unknown authority.

	// CodeNotReady is the readiness gate of PLAN §5.2 refusing to
	// return a result while the server is still indexing.
	CodeNotReady Code = "not_ready"

	// -- exit 4, crash or timeout: lightspeed or the server failed.

	// CodeTimeout is a deadline expiring on a request.
	CodeTimeout Code = "timeout"
	// CodeCancelled is a context cancelled from outside (signal).
	CodeCancelled Code = "cancelled"
	// CodeSpawnFailed is a server process that would not start.
	CodeSpawnFailed Code = "spawn_failed"
	// CodeServerCrash is a server process that died or stopped
	// answering mid-request.
	CodeServerCrash Code = "server_crash"
	// CodeProtocolError is a syntactically or semantically invalid
	// message from the server.
	CodeProtocolError Code = "protocol_error"
	// CodeIOError is a filesystem or stream failure on our side.
	CodeIOError Code = "io_error"
	// CodeInternal is a bug in lightspeed. The fallback for any error
	// that carries no code of its own.
	CodeInternal Code = "internal"

	// -- exit 1, problems found: we asked, we got an answer, and the
	// answer is bad news rather than a malfunction.

	// CodeProblemsFound is the generic "the command succeeded and
	// found problems" code.
	CodeProblemsFound Code = "problems_found"
	// CodeDiagnosticsFound is `check` finding error-severity
	// diagnostics.
	CodeDiagnosticsFound Code = "diagnostics_found"
	// CodeServerError is a JSON-RPC error response. The server
	// answered, so this is a result and not a crash.
	CodeServerError Code = "server_error"
	// CodeNotFound is an empty but authoritative answer: no definition
	// at that position, no references, no such symbol.
	CodeNotFound Code = "not_found"
	// CodeEditConflict is a WorkspaceEdit rejected by the applier:
	// overlapping edits, stale versions, or a path outside the
	// workspace (PLAN §5.3). Nothing was written.
	CodeEditConflict Code = "edit_conflict"
	// CodeDirtyWorktree is a write refused because the git worktree
	// has uncommitted changes and --allow-dirty was not passed.
	CodeDirtyWorktree Code = "dirty_worktree"
)

// CodedError is an error carrying a Code, and therefore an exit code.
// It is the error type renderers return and the recommended type for
// the rest of lightspeed to return across package boundaries.
type CodedError struct {
	Code    Code
	Message string
	// Details is an optional structured payload copied into the failed
	// envelope's error.data — for instance the install command that
	// would fix a CodeServerNotInstalled.
	Details any
	// Err is the wrapped cause, for errors.Is/errors.As. It is never
	// rendered: an envelope shows Message, not a chain.
	Err error
}

// Errorf builds a CodedError with a formatted message. Any error
// operand is wrapped, so errors.Is still works through the code.
func Errorf(code Code, format string, args ...any) *CodedError {
	e := &CodedError{Code: code, Message: fmt.Sprintf(format, args...)}
	for _, arg := range args {
		if err, ok := arg.(error); ok {
			e.Err = err
			break
		}
	}
	return e
}

// WithDetails returns a copy of e carrying a structured payload for
// the envelope's error.data field.
func (e *CodedError) WithDetails(details any) *CodedError {
	clone := *e
	clone.Details = details
	return &clone
}

func (e *CodedError) Error() string { return string(e.Code) + ": " + e.Message }

// ErrorCode reports the machine-readable code.
func (e *CodedError) ErrorCode() Code { return e.Code }

// ExitCode reports the process exit code for this error, satisfying
// the interface{ ExitCode() int } that ExitCode(error) looks for.
func (e *CodedError) ExitCode() int { return ExitCodeForCode(e.Code) }

// Unwrap returns the wrapped cause, if any.
func (e *CodedError) Unwrap() error { return e.Err }

// Coder is implemented by errors that carry a machine-readable code.
// Satisfying it requires naming render.Code, so it is for packages that
// already depend on render; packages that must not (internal/client and
// friends) use ExitCoder instead, which is import-free.
type Coder interface {
	ErrorCode() Code
}

// ExitCoder is implemented by errors that dictate their own process
// exit code. ExitCode(error) checks this first, so a package such as
// internal/client can return exit 5 for a readiness timeout without
// importing render at all: the interface is structural.
type ExitCoder interface {
	ExitCode() int
}
