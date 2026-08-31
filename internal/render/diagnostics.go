package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// Severity is an LSP DiagnosticSeverity. The numeric values are the
// wire values, so a server's severity can be converted directly.
type Severity int

const (
	// SeverityUnset is a diagnostic that arrived without a severity.
	// LSP leaves the interpretation to the client; we treat it as a
	// warning, which is the choice that does not silently turn an
	// unlabelled problem into an error (exit 1 for `check`) nor hide it.
	SeverityUnset Severity = 0
	SeverityError Severity = 1
	SeverityWarn  Severity = 2
	SeverityInfo  Severity = 3
	SeverityHint  Severity = 4
)

// String is the label used in text output, chosen to match the
// `file:line:col: error: message` convention that compilers emit and
// editors parse.
func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityWarn, SeverityUnset:
		return "warning"
	case SeverityInfo:
		return "info"
	case SeverityHint:
		return "hint"
	}
	return fmt.Sprintf("severity%d", int(s))
}

// MarshalJSON renders a severity as its label rather than its number,
// because "error" needs no lookup table on the consumer's side. An
// unset severity renders as "warning", so a round trip through JSON
// normalises it to SeverityWarn.
func (s Severity) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.String() + `"`), nil
}

// UnmarshalJSON accepts either the label MarshalJSON produces or the
// raw LSP number, so an envelope this package wrote can be read back.
func (s *Severity) UnmarshalJSON(data []byte) error {
	var n int
	if err := json.Unmarshal(data, &n); err == nil {
		*s = Severity(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return Errorf(CodeProtocolError, "severity is neither a name nor a number: %s", data)
	}
	switch name {
	case "error":
		*s = SeverityError
	case "warning":
		*s = SeverityWarn
	case "info":
		*s = SeverityInfo
	case "hint":
		*s = SeverityHint
	default:
		return Errorf(CodeProtocolError, "unknown severity %q", name)
	}
	return nil
}

// sarifLevel maps a severity to a SARIF 2.1.0 result level. SARIF has
// no "hint", so hints and informational diagnostics both become "note".
func (s Severity) sarifLevel() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityInfo, SeverityHint:
		return "note"
	default:
		return "warning"
	}
}

// Diagnostic is one problem reported by a server.
type Diagnostic struct {
	Span
	Severity Severity `json:"severity"`
	// Code is the server's diagnostic code ("SA1000", "UndeclaredName").
	Code string `json:"code,omitempty"`
	// Source is the producing analyser ("compiler", "staticcheck").
	Source  string `json:"source,omitempty"`
	Message string `json:"message"`
	// Tags are LSP diagnostic tags rendered as names ("unnecessary",
	// "deprecated"). SARIF output carries them in the result's
	// properties bag.
	Tags []string `json:"tags,omitempty"`
}

// NewDiagnostic resolves an LSP range in m into a Diagnostic. Set
// Code, Source and Tags on the result as needed.
func NewDiagnostic(m *protocol.Mapper, rng protocol.Range, sev Severity, message string) (Diagnostic, error) {
	span, err := NewSpan(m, rng)
	if err != nil {
		return Diagnostic{}, err
	}
	return Diagnostic{Span: span, Severity: sev, Message: message}, nil
}

// ruleID is the SARIF rule this diagnostic belongs to. SARIF wants a
// stable identifier per class of problem; a server gives us a source
// and sometimes a code, so we compose whatever it gave us.
func (d Diagnostic) ruleID() string {
	switch {
	case d.Source != "" && d.Code != "":
		return d.Source + "/" + d.Code
	case d.Code != "":
		return d.Code
	case d.Source != "":
		return d.Source
	default:
		return "lightspeed/diagnostic"
	}
}

// DiagnosticSet is a renderable set of diagnostics.
type DiagnosticSet struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
	// Total is how many diagnostics existed before truncation the
	// caller performed. Zero means len(Diagnostics).
	Total int `json:"total,omitempty"`
	// Truncated records truncation performed upstream of the renderer.
	Truncated bool `json:"truncated,omitempty"`
}

// Sort orders diagnostics by file then position.
func (ds *DiagnosticSet) Sort() {
	slices.SortStableFunc(ds.Diagnostics, func(a, b Diagnostic) int {
		return protocol.CompareLocation(
			protocol.Location{URI: a.URI, Range: a.Range},
			protocol.Location{URI: b.URI, Range: b.Range},
		)
	})
}

func (ds DiagnosticSet) total() int {
	if ds.Total > len(ds.Diagnostics) {
		return ds.Total
	}
	return len(ds.Diagnostics)
}

// HasErrors reports whether any diagnostic is error-severity. `check`
// uses it to decide between ExitOK and ExitProblems (PLAN §4: "exit 1
// if errors").
func (ds DiagnosticSet) HasErrors() bool {
	return slices.ContainsFunc(ds.Diagnostics, func(d Diagnostic) bool {
		return d.Severity == SeverityError
	})
}

// Diagnostics renders a diagnostic set. json, text and sarif are
// supported; diff is for edits.
func Diagnostics(w io.Writer, f Format, ds DiagnosticSet, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	switch f {
	case FormatJSON:
		return diagnosticsJSON(w, ds, opts)
	case FormatText:
		return diagnosticsText(w, ds, opts)
	case FormatSARIF:
		return diagnosticsSARIF(w, ds, opts)
	case FormatDiff:
		return unsupported(f, "diagnostics", FormatJSON, FormatText, FormatSARIF)
	default:
		_, err := ParseFormat(string(f))
		return err
	}
}

type diagnosticView struct {
	Diagnostic
	Before []ContextLine `json:"before,omitempty"`
	After  []ContextLine `json:"after,omitempty"`
}

type diagnosticsData struct {
	Diagnostics []diagnosticView `json:"diagnostics"`
	Count       int              `json:"count"`
	Total       int              `json:"total"`
	Truncated   bool             `json:"truncated"`
	Limit       int              `json:"limit,omitempty"`
	// Errors is how many of the rendered diagnostics are
	// error-severity, so a caller need not classify severities itself to
	// explain an exit 1.
	Errors int `json:"errors"`
}

func diagnosticsJSON(w io.Writer, ds DiagnosticSet, opts Options) error {
	kept, cut := truncate(ds.Diagnostics, opts.Limit)
	total := ds.total()

	views := make([]diagnosticView, 0, len(kept))
	errCount := 0
	for _, d := range kept {
		d.Path = opts.displayPath(d.Path)
		if d.Severity == SeverityError {
			errCount++
		}
		before, after := d.context(opts.Context)
		views = append(views, diagnosticView{Diagnostic: d, Before: before, After: after})
	}

	data := diagnosticsData{
		Diagnostics: views,
		Count:       len(views),
		Total:       total,
		Truncated:   cut || ds.Truncated,
		Errors:      errCount,
	}
	warnings := slices.Clone(opts.Warnings)
	if cut {
		data.Limit = opts.Limit
		warnings = append(warnings, truncationWarning("diagnostics", len(views), total, opts.Limit))
	}
	return WriteEnvelope(w, Envelope{
		Version:  EnvelopeVersion,
		OK:       true,
		Data:     data,
		Warnings: warnings,
	}, opts)
}

// diagnosticsText writes `file:line:col: severity: message [source:code]`,
// the convention compilers use and editors already parse.
func diagnosticsText(w io.Writer, ds DiagnosticSet, opts Options) error {
	kept, cut := truncate(ds.Diagnostics, opts.Limit)

	var buf bytes.Buffer
	for i, d := range kept {
		path := opts.displayPath(d.Path)
		before, after := d.context(opts.Context)
		if opts.Context > 0 && i > 0 {
			buf.WriteString("--\n")
		}
		for _, c := range before {
			fmt.Fprintf(&buf, "%s-%d-%s\n", path, c.Line, c.Text)
		}
		fmt.Fprintf(&buf, "%s:%d:%d: %s: %s%s\n",
			path, d.Start.Line, d.Start.Column, d.Severity, oneLine(d.Message), d.originSuffix())
		for _, c := range after {
			fmt.Fprintf(&buf, "%s-%d-%s\n", path, c.Line, c.Text)
		}
	}
	writeNotices(&buf, noticesFor("diagnostics", len(kept), ds.total(), opts, cut))
	return writeAll(w, buf.Bytes())
}

// originSuffix is the ` [source:code]` tail of a text diagnostic, empty
// when the server told us neither.
func (d Diagnostic) originSuffix() string {
	switch {
	case d.Source != "" && d.Code != "":
		return " [" + d.Source + ":" + d.Code + "]"
	case d.Source != "":
		return " [" + d.Source + "]"
	case d.Code != "":
		return " [" + d.Code + "]"
	}
	return ""
}
