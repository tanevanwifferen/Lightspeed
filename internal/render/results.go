package render

import (
	"bytes"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// Result is one location-shaped answer: a reference, a definition, an
// implementation, a symbol.
type Result struct {
	Span
	// Kind is an optional classification, e.g. a symbol kind
	// ("function", "struct") or "declaration" vs "use".
	Kind string `json:"kind,omitempty"`
	// Detail is optional secondary information: a containing symbol, a
	// type signature.
	Detail string `json:"detail,omitempty"`
	// Label overrides the matched source line in text output, for
	// results whose payload is not a source line — a hover synopsis, a
	// symbol name. JSON output always carries both.
	Label string `json:"label,omitempty"`
}

// NewResult resolves an LSP range in m into a Result.
func NewResult(m *protocol.Mapper, rng protocol.Range) (Result, error) {
	span, err := NewSpan(m, rng)
	if err != nil {
		return Result{}, err
	}
	return Result{Span: span}, nil
}

// ResultSet is a renderable set of results.
type ResultSet struct {
	// Kind names what the results are ("references", "symbols"). It
	// appears in JSON output and in truncation warnings; it defaults to
	// "results".
	Kind string `json:"kind,omitempty"`
	// Results is the answer, in the order it should be printed. Call
	// Sort for deterministic source order.
	Results []Result `json:"results"`
	// Total is how many results existed before any truncation the
	// caller performed upstream. Zero means len(Results).
	Total int `json:"total,omitempty"`
	// Truncated records truncation the caller already performed, for
	// instance because a server capped its own answer. Renderer-side
	// truncation from Options.Limit is ORed into it.
	Truncated bool `json:"truncated,omitempty"`
}

// Sort orders results by file then position, so that repeated runs
// produce byte-identical output.
func (rs *ResultSet) Sort() {
	slices.SortStableFunc(rs.Results, func(a, b Result) int {
		return protocol.CompareLocation(
			protocol.Location{URI: a.URI, Range: a.Range},
			protocol.Location{URI: b.URI, Range: b.Range},
		)
	})
}

// kind is Kind with its default applied.
func (rs ResultSet) kind() string {
	if rs.Kind == "" {
		return "results"
	}
	return rs.Kind
}

// total is Total with its default applied.
func (rs ResultSet) total() int {
	if rs.Total > len(rs.Results) {
		return rs.Total
	}
	return len(rs.Results)
}

// Results renders a result set. json and text are supported; diff is
// for edits and sarif is for diagnostics, and asking for either here is
// a usage error.
func Results(w io.Writer, f Format, rs ResultSet, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	switch f {
	case FormatJSON:
		return resultsJSON(w, rs, opts)
	case FormatText:
		return resultsText(w, rs, opts)
	case FormatDiff, FormatSARIF:
		return unsupported(f, rs.kind(), FormatJSON, FormatText)
	default:
		_, err := ParseFormat(string(f))
		return err
	}
}

// resultView is a Result as rendered: the result itself plus whatever
// Options.Context asked for.
type resultView struct {
	Result
	Before []ContextLine `json:"before,omitempty"`
	After  []ContextLine `json:"after,omitempty"`
}

// resultsData is the payload of a results envelope.
type resultsData struct {
	Kind    string       `json:"kind"`
	Results []resultView `json:"results"`
	// Count is how many results this output contains, Total how many
	// existed. They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	// Limit echoes the --limit that caused the truncation, so a caller
	// can retry with a larger one.
	Limit int `json:"limit,omitempty"`
}

func resultsJSON(w io.Writer, rs ResultSet, opts Options) error {
	kept, cut := truncate(rs.Results, opts.Limit)
	total := rs.total()

	views := make([]resultView, 0, len(kept))
	for _, r := range kept {
		r.Path = opts.displayPath(r.Path)
		before, after := r.context(opts.Context)
		views = append(views, resultView{Result: r, Before: before, After: after})
	}

	data := resultsData{
		Kind:      rs.kind(),
		Results:   views,
		Count:     len(views),
		Total:     total,
		Truncated: cut || rs.Truncated,
	}
	warnings := slices.Clone(opts.Warnings)
	if cut {
		data.Limit = opts.Limit
		warnings = append(warnings, truncationWarning(rs.kind(), len(views), total, opts.Limit))
	}
	return WriteEnvelope(w, Envelope{
		Version:  EnvelopeVersion,
		OK:       true,
		Data:     data,
		Warnings: warnings,
	}, opts)
}

// resultsText writes `file:line:col: text`, one result per line.
//
// With Options.Context it follows grep -C: context lines use
// `file-line-text` so a consumer can tell a match from its
// surroundings, and groups are separated by `--`.
func resultsText(w io.Writer, rs ResultSet, opts Options) error {
	kept, cut := truncate(rs.Results, opts.Limit)

	var buf bytes.Buffer
	for i, r := range kept {
		path := opts.displayPath(r.Path)
		before, after := r.context(opts.Context)
		if opts.Context > 0 && i > 0 {
			buf.WriteString("--\n")
		}
		for _, c := range before {
			fmt.Fprintf(&buf, "%s-%d-%s\n", path, c.Line, c.Text)
		}
		fmt.Fprintf(&buf, "%s:%d:%d: %s\n", path, r.Start.Line, r.Start.Column, oneLine(r.textForDisplay()))
		for _, c := range after {
			fmt.Fprintf(&buf, "%s-%d-%s\n", path, c.Line, c.Text)
		}
	}
	writeNotices(&buf, noticesFor(rs.kind(), len(kept), rs.total(), opts, cut))
	return writeAll(w, buf.Bytes())
}

// textForDisplay is the Label if the caller supplied one, else the
// matched source line.
func (r Result) textForDisplay() string {
	if r.Label != "" {
		return r.Label
	}
	return r.Text
}

// oneLine collapses embedded newlines so that text output keeps its
// one-result-per-line promise even for a multi-line label.
func oneLine(s string) string {
	if !strings.ContainsAny(s, "\n\r") {
		return s
	}
	s = strings.ReplaceAll(s, "\r\n", "\\n")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return strings.ReplaceAll(s, "\r", "\\n")
}

// noticesFor builds the out-of-band notes for a non-JSON format: the
// caller's warnings plus a truncation notice. Truncation is never
// silent, whatever the format.
func noticesFor(kind string, shown, total int, opts Options, cut bool) []string {
	notices := slices.Clone(opts.Warnings)
	if cut {
		notices = append(notices, truncationWarning(kind, shown, total, opts.Limit))
	}
	return notices
}

// writeNotices appends notices as `#` comment lines. `#` cannot be
// confused with a `file:line:col:` result line, and both `git apply`
// and a grep-style consumer skip it.
func writeNotices(buf *bytes.Buffer, notices []string) {
	for _, n := range notices {
		fmt.Fprintf(buf, "# %s\n", oneLine(n))
	}
}

func writeAll(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return Errorf(CodeIOError, "writing output: %v", err)
	}
	return nil
}
