package render

import (
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// TestNewSpanIsByteExact is the PLAN §8 M1 acceptance property, at the
// render layer: a span resolved from a server's UTF-16 range must name
// the exact bytes of the identifier, in a file where UTF-16 columns,
// byte columns and rune columns all disagree.
func TestNewSpanIsByteExact(t *testing.T) {
	m := mapper("fixture.go", cjkSource)

	for _, needle := range []string{"ユーザー名", "名前", "🎉", "使う", "package"} {
		count := strings.Count(cjkSource, needle)
		for n := range count {
			rng := rangeOf(t, m, needle, n)
			span, err := NewSpan(m, rng)
			if err != nil {
				t.Fatalf("NewSpan(%q #%d): %v", needle, n, err)
			}
			if got := cjkSource[span.Start.Offset:span.End.Offset]; got != needle {
				t.Errorf("%q #%d: span covers %q, want %q", needle, n, got, needle)
			}
			// The byte column must index the same byte the offset does.
			line := lineOf(cjkSource, span.Start.Line)
			if got := line[span.Start.Column-1:]; !strings.HasPrefix(got, needle) {
				t.Errorf("%q #%d: byte column %d points at %q", needle, n, span.Start.Column, truncateStr(got, 12))
			}
			if span.Text != line {
				t.Errorf("%q #%d: Text = %q, want the matched line %q", needle, n, span.Text, line)
			}
		}
	}
}

// TestByteAndUTF16ColumnsDiffer guards the reason the fixture exists: if
// the two encodings ever agreed here, the test above would be proving
// nothing.
func TestByteAndUTF16ColumnsDiffer(t *testing.T) {
	m := mapper("fixture.go", cjkSource)

	// "ユーザー名" on the var line is preceded by "var 名前, ": 12 bytes
	// but 8 UTF-16 code units.
	span, err := NewSpan(m, rangeOf(t, m, "ユーザー名", 1))
	if err != nil {
		t.Fatal(err)
	}
	if span.Start.Column != 13 {
		t.Errorf("byte column = %d, want 13", span.Start.Column)
	}
	if span.Range.Start.Character != 8 {
		t.Errorf("UTF-16 column = %d, want 8", span.Range.Start.Character)
	}

	// The emoji is a surrogate pair: two UTF-16 units for four bytes,
	// so everything after it on the line is offset differently again.
	emoji, err := NewSpan(m, rangeOf(t, m, "🎉", 1))
	if err != nil {
		t.Fatal(err)
	}
	if got := emoji.Range.End.Character - emoji.Range.Start.Character; got != 2 {
		t.Errorf("emoji spans %d UTF-16 units, want 2 (a surrogate pair)", got)
	}
	if got := emoji.End.Offset - emoji.Start.Offset; got != 4 {
		t.Errorf("emoji spans %d bytes, want 4", got)
	}
}

func TestNewSpanRejectsImpossibleRange(t *testing.T) {
	m := mapper("fixture.go", asciiSource)
	_, err := NewSpan(m, protocol.Range{
		Start: protocol.Position{Line: 999, Character: 0},
		End:   protocol.Position{Line: 999, Character: 1},
	})
	if err == nil {
		t.Fatal("expected an error for a range past end of file")
	}
	if got := CodeForError(err); got != CodeProtocolError {
		t.Errorf("code = %q, want %q", got, CodeProtocolError)
	}
	if got := ExitCode(err); got != ExitCrash {
		t.Errorf("exit = %d, want %d", got, ExitCrash)
	}
}

func TestNewSpanFromLocationChecksFile(t *testing.T) {
	m := mapper("a.go", asciiSource)
	other := protocol.URIFromPath("/w/b.go")
	_, err := NewSpanFromLocation(m, protocol.Location{URI: other})
	if err == nil {
		t.Fatal("expected an error for a location in another file")
	}
	if got := CodeForError(err); got != CodeInternal {
		t.Errorf("code = %q, want %q", got, CodeInternal)
	}
}

func TestCRLFLinesDropTheCarriageReturn(t *testing.T) {
	m := mapper("win.go", crlfSource)
	span, err := NewSpan(m, rangeOf(t, m, "Path", 0))
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(span.Text, '\r') {
		t.Errorf("Text = %q, want no carriage return", span.Text)
	}
	if span.Text != `var Path = "C:\\tmp"` {
		t.Errorf("Text = %q", span.Text)
	}
	before, after := span.context(2)
	for _, c := range append(before, after...) {
		if strings.ContainsRune(c.Text, '\r') {
			t.Errorf("context line %d = %q, want no carriage return", c.Line, c.Text)
		}
	}
	if len(after) != 0 {
		t.Errorf("after = %v, want none: the file ends with a terminator, not another line", after)
	}
	if len(before) != 2 {
		t.Errorf("before = %v, want 2 lines", before)
	}
}

func TestContextStopsAtFileEdges(t *testing.T) {
	m := mapper("a.go", asciiSource)

	first, err := NewSpan(m, rangeOf(t, m, "package", 0))
	if err != nil {
		t.Fatal(err)
	}
	before, after := first.context(3)
	if len(before) != 0 {
		t.Errorf("before the first line = %v, want none", before)
	}
	if len(after) != 3 {
		t.Errorf("after = %v, want 3 lines", after)
	}

	last, err := NewSpan(m, rangeOf(t, m, "r.db.user(id)", 0))
	if err != nil {
		t.Fatal(err)
	}
	before, after = last.context(3)
	if len(before) != 3 {
		t.Errorf("before = %v, want 3 lines", before)
	}
	// asciiSource ends "}\n": one real line after the match, then the
	// terminator. A phantom empty line here would be a lie.
	if len(after) != 1 || after[0].Text != "}" {
		t.Errorf("after = %v, want exactly the closing brace", after)
	}
}

func TestContextLineNumbersMatchSource(t *testing.T) {
	m := mapper("a.go", asciiSource)
	span, err := NewSpan(m, rangeOf(t, m, "db *DB", 0))
	if err != nil {
		t.Fatal(err)
	}
	before, after := span.context(2)
	for _, c := range append(before, after...) {
		if got := lineOf(asciiSource, c.Line); got != c.Text {
			t.Errorf("context line %d = %q, but source line %d is %q", c.Line, c.Text, c.Line, got)
		}
	}
}

func TestContextWithoutMapperIsEmpty(t *testing.T) {
	// A hand-built Span has no source, and must render rather than panic.
	span := Span{Path: "/w/a.go", Start: Point{Line: 1, Column: 1}, End: Point{Line: 1, Column: 1}}
	before, after := span.context(3)
	if before != nil || after != nil {
		t.Errorf("got %v / %v, want nothing", before, after)
	}
}

// lineOf returns the 1-based line of s, without its terminator.
func lineOf(s string, line int) string {
	lines := strings.Split(s, "\n")
	if line < 1 || line > len(lines) {
		return ""
	}
	return strings.TrimSuffix(lines[line-1], "\r")
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
