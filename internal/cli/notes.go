package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Notes: things that happened on the way to an answer which the caller
// should know and which no command's own code is placed to say — a daemon
// from another build that was replaced (D29). Each command assembles its own
// warnings, and there are dozens of them; what they all do is write to
// env.stdout, so that is where a note joins the answer.

// addNote makes msg part of the next thing the command writes: a warning in
// its JSON envelope, or a line on stderr for a format that has no envelope
// (text, diff, SARIF must stay exactly their format).
func (e *env) addNote(msg string) {
	if nw, ok := e.stdout.(*noteWriter); ok {
		nw.notes = append(nw.notes, msg)
		return
	}
	e.stdout = &noteWriter{w: e.stdout, stderr: e.stderr, notes: []string{msg}}
}

// noteWriter delivers pending notes with the first write it sees, and is a
// plain pass-through after that.
type noteWriter struct {
	w      io.Writer
	stderr io.Writer
	notes  []string
}

func (n *noteWriter) Write(p []byte) (int, error) {
	notes := n.notes
	n.notes = nil
	if len(notes) == 0 {
		return n.w.Write(p)
	}
	if out, ok := withWarnings(p, notes); ok {
		if _, err := n.w.Write(out); err != nil {
			return 0, err
		}
		return len(p), nil
	}
	for _, note := range notes {
		fmt.Fprintf(n.stderr, "lightspeed: warning: %s\n", note)
	}
	return n.w.Write(p)
}

// envelopeBytes is a JSON envelope with its parts left as they were written,
// so that adding a warning changes nothing else in the output: not the
// order of keys, not the numbers, not the shape of data.
type envelopeBytes struct {
	Version  int             `json:"version"`
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
	Error    json.RawMessage `json:"error,omitempty"`
}

// withWarnings puts notes first among an envelope's warnings. It reports
// false for output that is not one envelope — anything but a single JSON
// object with a version — and leaves the decision to the caller.
func withWarnings(p []byte, notes []string) ([]byte, bool) {
	var env envelopeBytes
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil || env.Version == 0 || dec.More() {
		return nil, false
	}
	env.Warnings = append(append([]string(nil), notes...), env.Warnings...)
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	if bytes.HasPrefix(p, []byte("{\n")) {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(env); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}
