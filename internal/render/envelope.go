// Package render produces lightspeed's output formats. For M0 only
// the JSON envelope of PLAN §4 exists; text, diff and sarif renderers
// arrive with the commands that need them.
package render

import (
	"encoding/json"
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
	Code    string `json:"code"`
	Message string `json:"message"`
}

// OK writes a successful envelope wrapping data to w.
func OK(w io.Writer, data any, warnings ...string) error {
	return write(w, Envelope{Version: EnvelopeVersion, OK: true, Data: data, Warnings: warnings})
}

// Fail writes a failed envelope with a machine code and message to w.
func Fail(w io.Writer, code, message string) error {
	return write(w, Envelope{Version: EnvelopeVersion, OK: false, Error: &Error{Code: code, Message: message}})
}

func write(w io.Writer, e Envelope) error {
	enc := json.NewEncoder(w)
	return enc.Encode(e)
}
