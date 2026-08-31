// Package render produces lightspeed's output formats and owns the
// agent-facing output contract of PLAN §4: the JSON envelope, the
// machine-readable error code taxonomy, the exit-code taxonomy, and
// token discipline.
//
// The four formats are json, text, diff and sarif; not every command
// supports every format (sarif is diagnostics-only, diff is edits-only),
// and asking for an unsupported combination is a usage error rather than
// a surprise. Only json wraps its payload in the Envelope: a text, diff
// or sarif consumer expects that format and nothing else.
//
// This package deliberately depends on nothing but the vendored
// internal/gopls packages. It takes plain data plus LSP types; it never
// talks to a server, a router or a document store, so every format is
// testable from a byte slice.
package render

import (
	"encoding/json"
	"errors"
	"io"
)

// EnvelopeVersion is the version stamped on every JSON envelope.
const EnvelopeVersion = 1

// Envelope is the machine-readable output contract:
//
//	{"version":1,"ok":true,"data":…,"warnings":[…]}
//
// Errors reuse the envelope with ok:false and a machine-readable code;
// a bare stack trace is never valid output.
type Envelope struct {
	Version  int      `json:"version"`
	OK       bool     `json:"ok"`
	Data     any      `json:"data,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Error    *Error   `json:"error,omitempty"`
}

// Error is the machine-readable error payload of a failed envelope.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// Data carries optional structured context for the failure: the
	// install command that would fix a missing server, the conflicting
	// ranges of a rejected edit set. Consumers may ignore it; its
	// shape is per-code.
	Data any `json:"data,omitempty"`
}

// OK writes a successful envelope wrapping data to w.
func OK(w io.Writer, data any, warnings ...string) error {
	return WriteEnvelope(w, Envelope{
		Version:  EnvelopeVersion,
		OK:       true,
		Data:     data,
		Warnings: warnings,
	}, Options{})
}

// Fail writes a failed envelope with a machine code and message to w.
func Fail(w io.Writer, code Code, message string) error {
	return WriteEnvelope(w, Envelope{
		Version: EnvelopeVersion,
		OK:      false,
		Error:   &Error{Code: code, Message: message},
	}, Options{})
}

// FailError writes a failed envelope derived from err: the code comes
// from CodeForError, the message from err.Error(), and error.data from
// a *CodedError's Details.
//
// It reports only the failure to write; the caller decides the process
// exit code by passing the same err to ExitCode.
func FailError(w io.Writer, err error, warnings ...string) error {
	e := Envelope{
		Version:  EnvelopeVersion,
		OK:       false,
		Warnings: warnings,
		Error:    &Error{Code: CodeForError(err), Message: message(err)},
	}
	var coded *CodedError
	if errors.As(err, &coded) {
		e.Error.Data = coded.Details
	}
	return WriteEnvelope(w, e, Options{})
}

// WriteEnvelope writes an envelope to w, honouring opts.Indent. The
// output is always newline-terminated so that a stream of envelopes is
// JSON-lines parseable.
func WriteEnvelope(w io.Writer, e Envelope, opts Options) error {
	enc := json.NewEncoder(w)
	if opts.Indent {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(e); err != nil {
		return Errorf(CodeIOError, "writing output: %v", err)
	}
	return nil
}

// message renders an error for the envelope's message field. A
// *CodedError contributes only its message: the code is already a
// separate field, and repeating it reads as a stack trace.
func message(err error) string {
	if err == nil {
		return ""
	}
	var coded *CodedError
	if errors.As(err, &coded) && coded.Message != "" {
		return coded.Message
	}
	return err.Error()
}
