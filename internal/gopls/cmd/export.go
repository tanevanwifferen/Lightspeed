package cmd

// This file is NOT vendored code: it is lightspeed's own, written for
// this project and not copied from the Go tools repository. It lives
// in this package only because Go visibility leaves no alternative —
// gopls keeps span parsing unexported, since nothing outside its own
// cmd package ever needed it. See docs/DECISIONS.md D5, which defers
// the shape of this facade to M1, and ATTRIBUTION, which lists the
// files that *are* vendored.
//
// It adds no parsing of its own: [ParseLocation] is [parseSpan] with
// the result copied into exported fields. PLAN §4 adopts gopls's
// location syntax wholesale — `file.go:line:col`,
// `file.go:line:col-line:col`, `file.go:#offset`, 1-based lines and
// *byte* columns — and PLAN §5.1 is explicit that reimplementing any
// of this is the wrong move.

import "github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"

// A Point is one end of a [Location].
//
// A point may carry a line/column position, a byte offset, or both;
// which of the two the user wrote is preserved, because converting an
// offset to a column needs the file's content and this package never
// reads files. Ask with [Point.HasPosition] and [Point.HasOffset]
// before believing a field.
type Point struct {
	// Line is the 1-based line number, or 0 when the input carried no
	// position.
	Line int
	// Column is the 1-based *byte* column — not a UTF-16 code unit
	// and not a rune index. It is 1 when a line was given without a
	// column, and 0 when there is no position at all.
	Column int
	// Offset is the 0-based byte offset, or -1 when the input carried
	// no offset.
	Offset int
}

// HasPosition reports whether Line and Column are meaningful.
func (p Point) HasPosition() bool { return p.Line > 0 }

// HasOffset reports whether Offset is meaningful.
func (p Point) HasOffset() bool { return p.Offset >= 0 }

// IsValid reports whether the point locates anything at all.
func (p Point) IsValid() bool { return p.HasPosition() || p.HasOffset() }

// A Location is a parsed command-line location: a file plus a point or
// a range within it.
type Location struct {
	// URI is the file, made absolute against the current directory.
	URI protocol.DocumentURI
	// Start and End delimit the range. For a point location they are
	// equal, and [Location.IsPoint] reports so.
	Start Point
	End   Point
}

// IsPoint reports whether the location is a single point rather than a
// range.
func (l Location) IsPoint() bool { return l.Start == l.End }

// IsValid reports whether the location names a file and a position
// within it.
func (l Location) IsValid() bool { return l.URI != "" && l.Start.IsValid() }

// ParseLocation parses gopls's command-line location syntax:
//
//	file.go                 the whole file (offset 0)
//	file.go:12              line 12, column 1
//	file.go:12:5            line 12, byte column 5
//	file.go:12:5-12:9       a range on one line
//	file.go:12:5-14:1       a range across lines
//	file.go:#1234           byte offset 1234
//	file.go:#1234-#1300     a range of byte offsets
//
// The path may be relative; it is resolved against the current
// directory. Parsing never fails and never touches the filesystem —
// an input that names no position yields a location whose Start is
// offset 0 — so callers validate with [Location.IsValid] and by
// resolving the point against the file's content.
func ParseLocation(input string) Location {
	s := parseSpan(input)
	return Location{
		URI:   s.URI(),
		Start: exportPoint(s.v.Start),
		End:   exportPoint(s.v.End),
	}
}

func exportPoint(p _point) Point {
	return Point{Line: p.Line, Column: p.Column, Offset: p.Offset}
}
