package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/diff"
)

// Format is an output format of PLAN §4.
type Format string

const (
	// FormatJSON is the envelope of PLAN §4, one line of JSON unless
	// Options.Indent is set. The default when stdout is not a TTY.
	FormatJSON Format = "json"
	// FormatText is `file:line:col: text`, one result per line,
	// grep-compatible. Columns are 1-based *byte* columns, matching the
	// location syntax lightspeed accepts as input.
	FormatText Format = "text"
	// FormatDiff is a unified diff feedable to `git apply`. Edits only.
	FormatDiff Format = "diff"
	// FormatSARIF is SARIF 2.1.0. Diagnostics only.
	FormatSARIF Format = "sarif"
)

// formats is every valid --format value, in help order.
var formats = []Format{FormatJSON, FormatText, FormatDiff, FormatSARIF}

// Formats returns every valid --format value, for help text and shell
// completion.
func Formats() []Format { return slices.Clone(formats) }

// ParseFormat validates a --format value. The empty string is not a
// format: use ResolveFormat to apply the TTY default.
func ParseFormat(s string) (Format, error) {
	f := Format(s)
	if slices.Contains(formats, f) {
		return f, nil
	}
	names := make([]string, len(formats))
	for i, f := range formats {
		names[i] = string(f)
	}
	return "", Errorf(CodeInvalidFormat, "unknown format %q (want one of %s)",
		s, strings.Join(names, ", "))
}

// DefaultFormat is the format to use when --format was not given:
// FormatJSON when w is not a terminal (the machine case, and the
// documented default of PLAN §4), FormatText when it is.
func DefaultFormat(w io.Writer) Format {
	if isTerminal(w) {
		return FormatText
	}
	return FormatJSON
}

// ResolveFormat turns a --format flag value into a Format, applying the
// DefaultFormat of w when the flag was not given.
func ResolveFormat(flagValue string, w io.Writer) (Format, error) {
	if flagValue == "" {
		return DefaultFormat(w), nil
	}
	return ParseFormat(flagValue)
}

// isTerminal reports whether w is a character device. Deliberately
// dependency-free: the only thing we need to know is whether a human is
// probably reading.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// Options are the render-time knobs of PLAN §4, chiefly the token
// discipline: a matched line only by default, more context and more
// results strictly opt-in, and truncation always announced.
//
// The zero Options is valid and means: matched line only, no limit,
// absolute paths, compact JSON.
type Options struct {
	// Context is --context N: N lines of surrounding source on each
	// side of every match, in addition to the matched line. Zero means
	// the matched line only.
	Context int

	// Limit is --limit N: at most N results. Zero means no limit.
	// Truncation is never silent — the JSON payload carries
	// truncated:true and a warning, and the text and diff formats
	// carry a `#` notice.
	Limit int

	// Root is the directory that paths are reported relative to,
	// typically the workspace root. Empty means report absolute paths.
	// Files outside Root keep their absolute path.
	Root string

	// DiffContext is the number of unchanged context lines in a
	// unified diff. nil means diff.DefaultContextLines (3), the value
	// that makes a patch robust enough for `git apply` to verify what
	// it is patching; use DiffContextLines(0) to ask for none.
	DiffContext *int

	// Indent pretty-prints JSON and SARIF. Off by default so that JSON
	// output is one line per envelope.
	Indent bool

	// Warnings are non-fatal notes from the caller, merged with the
	// renderer's own (truncation) warnings.
	Warnings []string

	// ToolVersion is reported as the SARIF driver version. Empty omits
	// the field.
	ToolVersion string
}

// validate rejects nonsensical options as a usage error rather than
// silently normalising them: a caller passing --context -1 has a bug
// the user should hear about.
func (o Options) validate() error {
	if o.Context < 0 {
		return Errorf(CodeUsage, "--context must not be negative (got %d)", o.Context)
	}
	if o.Limit < 0 {
		return Errorf(CodeUsage, "--limit must not be negative (got %d)", o.Limit)
	}
	if o.DiffContext != nil && *o.DiffContext < 0 {
		return Errorf(CodeUsage, "--diff-context must not be negative (got %d)", *o.DiffContext)
	}
	return nil
}

// DiffContextLines returns a value for Options.DiffContext, so that a
// caller can ask for exactly zero context lines and be distinguished
// from a caller who left the field alone.
func DiffContextLines(n int) *int { return &n }

// diffContext is DiffContext with its default applied.
func (o Options) diffContext() int {
	if o.DiffContext == nil {
		return diff.DefaultContextLines
	}
	return *o.DiffContext
}

// displayPath renders an absolute path for output: relative to
// Options.Root when it is inside it, absolute otherwise. Paths that
// would need to climb out of Root stay absolute, because "../../x" is
// less useful than the real path.
func (o Options) displayPath(path string) string {
	if o.Root == "" || path == "" || !filepath.IsAbs(path) {
		return path
	}
	rel, err := filepath.Rel(o.Root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// unsupported reports that format is valid but meaningless for this
// payload. It is a usage error: the command line asked for something
// that cannot exist, and silently substituting another format would
// hide that.
func unsupported(f Format, payload string, supported ...Format) error {
	names := make([]string, len(supported))
	for i, s := range supported {
		names[i] = string(s)
	}
	return Errorf(CodeUnsupportedFormat, "format %q is not available for %s (want one of %s)",
		f, payload, strings.Join(names, ", "))
}

// truncate applies Options.Limit to a slice, reporting whether anything
// was dropped. It never reorders: callers sort before rendering.
func truncate[T any](items []T, limit int) (kept []T, truncated bool) {
	if limit <= 0 || len(items) <= limit {
		return items, false
	}
	return items[:limit], true
}

// truncationWarning is the human-readable half of the truncated flag.
func truncationWarning(kind string, shown, total, limit int) string {
	return fmt.Sprintf("%s truncated: showing %d of %d %s (--limit %d)", kind, shown, total, kind, limit)
}
