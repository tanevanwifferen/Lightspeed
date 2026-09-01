package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os/exec"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// Exit codes of PLAN §4, as unexported constants. This package follows
// internal/client's contract (docs/DECISIONS.md D7): an error carries
// its own exit code through an ExitCode method, and the CLI maps it by
// asserting on an anonymous interface, so neither package has to
// import internal/render.
const (
	exitProblems = 1
	exitUsage    = 2
	exitNoServer = 3
	exitCrash    = 4
	exitNotReady = 5
)

// Machine-readable codes this package mints itself, spelled as in
// internal/render's Code constants (PLAN §4). Codes that come from an
// error further down — internal/client's "not_ready" — are passed
// through verbatim rather than translated, so that the daemon and
// --no-daemon paths report identical codes.
const (
	CodeUsage              = "usage"
	CodeNoSuchFile         = "no_such_file"
	CodeNoServer           = "no_server"
	CodeServerNotInstalled = "server_not_installed"
	CodeTimeout            = "timeout"
	CodeCancelled          = "cancelled"
	CodeSpawnFailed        = "spawn_failed"
	CodeServerCrash        = "server_crash"
	CodeServerError        = "server_error"
	CodeInternal           = "internal"
	CodeDaemonClosed       = "daemon_closed"
)

// An Error is a daemon-reported failure that survives the socket with
// its classification intact: the machine code for the JSON envelope
// and the process exit code of PLAN §4.
//
// This matters more than it looks. The whole point of the readiness
// gate (PLAN §5.2) is that "still indexing" exits 5 instead of
// pretending to be an empty result; if that distinction were lost in
// transit, running with a daemon would be quietly less safe than
// running without one. Every error the service produces is therefore
// encoded into an Error on the way out and decoded back on the way in.
type Error struct {
	// Code is the machine-readable code (PLAN §4).
	Code string `json:"code"`
	// Message is the human-readable explanation.
	Message string `json:"message"`
	// Exit is the process exit code the CLI should use.
	Exit int `json:"exit"`
	// Server, when known, is the server definition that failed.
	Server string `json:"server,omitempty"`
	// Root, when known, is the workspace root it was serving.
	Root string `json:"root,omitempty"`

	// wrapped is the error this was built from, if any. It keeps
	// errors.Is working in the in-process mode — a caller may still
	// ask errors.Is(err, client.ErrNotReady) — while over the socket
	// only Code and Exit survive, which is why those two are what
	// the CLI must key on.
	wrapped error
}

func (e *Error) Error() string { return e.Message }

// Unwrap exposes the error this was built from.
func (e *Error) Unwrap() error { return e.wrapped }

// ExitCode reports the process exit code of PLAN §4.
func (e *Error) ExitCode() int {
	if e.Exit == 0 {
		return exitCrash
	}
	return e.Exit
}

// ErrorCode reports the machine-readable code for the JSON envelope.
// It is spelled like internal/render's Coder accessor but returns a
// plain string, because satisfying that interface would mean importing
// render into the daemon; the CLI converts the string.
func (e *Error) ErrorCode() string { return e.Code }

// ErrDaemonClosed is returned by a handle whose daemon has shut down
// or whose pool has been closed.
var ErrDaemonClosed = errors.New("daemon: closed")

// asError converts any error into a wire-ready *Error, preserving the
// exit code and machine code the error already knew about.
//
// Resolution order mirrors internal/render.ExitCode, deliberately: an
// error that names its own exit code decides, then one that names a
// code, then the well-known sentinels, then "internal".
func asError(err error) *Error {
	if err == nil {
		return nil
	}
	var already *Error
	if errors.As(err, &already) {
		return already
	}

	out := &Error{Message: err.Error(), wrapped: err}

	var exiter interface{ ExitCode() int }
	if errors.As(err, &exiter) {
		out.Exit = exiter.ExitCode()
	}
	// internal/client's errors report their envelope code through a
	// Code() string method (docs/DECISIONS.md D7).
	var coder interface{ Code() string }
	if errors.As(err, &coder) {
		out.Code = coder.Code()
	}

	switch {
	case errors.Is(err, router.ErrNoServer):
		fill(out, router.CodeNoServer, exitNoServer)
	case errors.Is(err, client.ErrNotReady):
		fill(out, "not_ready", exitNotReady)
	case errors.Is(err, client.ErrUnsupportedMethod):
		fill(out, "unsupported_method", exitNoServer)
	case errors.Is(err, client.ErrConnClosed):
		fill(out, CodeServerCrash, exitCrash)
	case errors.Is(err, ErrDaemonClosed):
		fill(out, CodeDaemonClosed, exitCrash)
	case errors.Is(err, context.DeadlineExceeded):
		fill(out, CodeTimeout, exitCrash)
	case errors.Is(err, context.Canceled):
		fill(out, CodeCancelled, exitCrash)
	case errors.Is(err, exec.ErrNotFound):
		fill(out, CodeServerNotInstalled, exitNoServer)
	case errors.Is(err, fs.ErrNotExist):
		fill(out, CodeNoSuchFile, exitUsage)
	}

	var rpcErr *client.RPCError
	if errors.As(err, &rpcErr) {
		fill(out, CodeServerError, exitProblems)
	}

	fill(out, CodeInternal, exitCrash)
	return out
}

// fill sets the code and exit code only if they are not already known,
// so the first (most specific) classification wins.
func fill(e *Error, code string, exit int) {
	if e.Code == "" {
		e.Code = code
	}
	if e.Exit == 0 {
		e.Exit = exit
	}
}

// wireData is the payload an *Error travels in: the JSON-RPC error
// object's `data` field, alongside the message.
type wireData struct {
	Code   string `json:"code,omitempty"`
	Exit   int    `json:"exit,omitempty"`
	Server string `json:"server,omitempty"`
	Root   string `json:"root,omitempty"`
}

// toRPC encodes an error as the JSON-RPC error response the daemon
// sends. The numeric JSON-RPC code is always "internal error": the
// classification that matters to us rides in `data`, and inventing
// JSON-RPC codes for it would be a second taxonomy to keep in sync.
func toRPC(err error) *client.RPCError {
	e := asError(err)
	data, merr := json.Marshal(wireData{Code: e.Code, Exit: e.Exit, Server: e.Server, Root: e.Root})
	if merr != nil {
		data = nil
	}
	return &client.RPCError{Code: -32603, Message: e.Message, Data: data}
}

// fromRPC decodes a daemon error response back into an *Error, so that
// a client of the daemon sees the same error, the same code and the
// same exit code as it would have seen running in process. A response
// without our `data` field — a JSON-RPC level failure, say — becomes a
// crash, which is what an unclassifiable daemon failure is.
func fromRPC(err error) error {
	var rpcErr *client.RPCError
	if !errors.As(err, &rpcErr) {
		return err
	}
	out := &Error{Message: rpcErr.Message}
	if len(rpcErr.Data) > 0 {
		var d wireData
		if json.Unmarshal(rpcErr.Data, &d) == nil {
			out.Code, out.Exit, out.Server, out.Root = d.Code, d.Exit, d.Server, d.Root
		}
	}
	fill(out, CodeInternal, exitCrash)
	return out
}
