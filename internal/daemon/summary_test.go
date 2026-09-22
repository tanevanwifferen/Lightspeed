package daemon

import (
	"strings"
	"testing"
)

// The one-line summary of a server's stderr quotes the first informative line —
// the complaint — and the last, and skips lines that are only decoration.
func TestStderrSummaryQuotesTheFirstInformativeLine(t *testing.T) {
	stderr := "\n=========\n\nError: cannot load workspace /w: no go.mod found\n  at loader.go:12\n------\n^^^^^\nexiting with status 3\n"
	got := stderrSummary(stderr)
	want := "Error: cannot load workspace /w: no go.mod found … exiting with status 3"
	if got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	if got := stderrSummary("only line\n"); got != "only line" {
		t.Errorf("one line: %q", got)
	}
	if got := stderrSummary("---\n===\n"); got != "" {
		t.Errorf("only decoration: %q", got)
	}
	long := strings.Repeat("x", 500)
	if got := stderrSummary(long + "\nfoo"); !strings.HasPrefix(got, strings.Repeat("x", 200)+"…") {
		t.Errorf("a long first line is clipped: %q", got)
	}
}
