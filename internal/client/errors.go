package client

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Exit codes of PLAN §4, duplicated here as unexported constants so
// that internal/client does not import internal/cli. Errors in this
// package expose them through an ExitCode method; the CLI layer maps
// any error to an exit code by type-asserting on an anonymous
// interface:
//
//	if ec, ok := err.(interface{ ExitCode() int }); ok { … }
//
// so it never has to import this package's internals.
const (
	exitProblems = 1 // problems found / server-reported error
	exitNoServer = 3 // no server available (here: no such capability)
	exitNotReady = 5 // not ready / indexing timeout
)

// ErrNotReady is the sentinel behind every not-ready condition, for
// callers that want errors.Is rather than errors.As.
var ErrNotReady = errors.New("workspace not ready")

// NotReadyError reports that the workspace could not be shown to be
// ready in time, so any answer the server gave is of unknown
// authority (PLAN §5.2). Returning this instead of a possibly-bogus
// empty result is the whole point of the readiness gate: an empty
// reference list that looks authoritative will make an agent delete
// live code.
type NotReadyError struct {
	// Method is the LSP method that was being gated.
	Method string
	// Reason is a short machine-ish explanation, one of the
	// NotReady* constants below.
	Reason string
	// Elapsed is how long the gate waited before giving up.
	Elapsed time.Duration
	// Attempts is the number of times the request was issued.
	Attempts int
	// Active lists the $/progress tokens still outstanding, if any.
	Active []string
}

// Reasons reported by NotReadyError.
const (
	// NotReadyIndexing: the server still had unfinished progress
	// tokens when the timeout expired.
	NotReadyIndexing = "indexing"
	// NotReadyUnstable: the result kept changing between retries, so
	// it never settled for the stability window.
	NotReadyUnstable = "unstable"
	// NotReadyEmpty: the result was empty and readiness was never
	// established, so the emptiness cannot be trusted.
	NotReadyEmpty = "empty_unverified"
)

func (e *NotReadyError) Error() string {
	var b strings.Builder
	b.WriteString("workspace not ready")
	if e.Method != "" {
		fmt.Fprintf(&b, " for %s", e.Method)
	}
	if e.Reason != "" {
		fmt.Fprintf(&b, " (%s)", e.Reason)
	}
	fmt.Fprintf(&b, ": gave up after %s and %d attempt(s)", e.Elapsed.Round(time.Millisecond), e.Attempts)
	if len(e.Active) > 0 {
		fmt.Fprintf(&b, "; server still reports work in progress: %s", strings.Join(e.Active, ", "))
	}
	return b.String()
}

// Unwrap makes errors.Is(err, ErrNotReady) work.
func (e *NotReadyError) Unwrap() error { return ErrNotReady }

// ExitCode reports PLAN §4's not-ready/indexing-timeout exit code.
func (e *NotReadyError) ExitCode() int { return exitNotReady }

// Code is the machine-readable code for the JSON envelope (PLAN §4).
func (e *NotReadyError) Code() string { return "not_ready" }

// ErrUnsupportedMethod is the sentinel behind UnsupportedMethodError.
var ErrUnsupportedMethod = errors.New("method not advertised by server")

// UnsupportedMethodError reports a refusal to call a method the
// server never advertised (PLAN §5.4: "Never call uncapabilitied
// methods"). It carries the capability that was missing so the
// message can tell the user what the server can actually do.
type UnsupportedMethodError struct {
	// Method is the LSP method that was refused.
	Method string
	// Capability is the InitializeResult capability path that would
	// have had to be present, e.g. "referencesProvider".
	Capability string
	// ServerName is the server's self-reported name, if known.
	ServerName string
}

func (e *UnsupportedMethodError) Error() string {
	server := e.ServerName
	if server == "" {
		server = "server"
	}
	if e.Capability == "" {
		return fmt.Sprintf("%s does not support %s", server, e.Method)
	}
	return fmt.Sprintf("%s does not support %s (no %s capability)", server, e.Method, e.Capability)
}

// Unwrap makes errors.Is(err, ErrUnsupportedMethod) work.
func (e *UnsupportedMethodError) Unwrap() error { return ErrUnsupportedMethod }

// ExitCode reports PLAN §4's "no server" exit code: for this file and
// this request there is effectively no server that can answer.
func (e *UnsupportedMethodError) ExitCode() int { return exitNoServer }

// Code is the machine-readable code for the JSON envelope (PLAN §4).
func (e *UnsupportedMethodError) Code() string { return "no_capability" }

// ExitCode makes a server-reported JSON-RPC error map to "problems
// found" rather than a crash: the server answered, the answer was an
// error. RPCError already has a numeric Code field, so it carries no
// Code method; the envelope code for it is "server_error".
func (e *RPCError) ExitCode() int { return exitProblems }
