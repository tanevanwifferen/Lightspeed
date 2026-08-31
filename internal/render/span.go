package render

import (
	"bytes"
	"slices"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// Point is one end of a Span, in the three coordinate systems that
// matter at the command line.
//
// Line and Column are 1-based and Column is a *byte* column, matching
// gopls's span syntax (`file.go:12:5`) that lightspeed accepts as input
// — so a location printed by lightspeed can be pasted back into
// lightspeed. Offset is the 0-based byte offset, for `file.go:#offset`.
// The UTF-16 coordinates the server actually used are preserved
// verbatim in Span.Range.
type Point struct {
	Line   int `json:"line"`
	Column int `json:"column"`
	Offset int `json:"offset"`
}

// Span is a resolved source range: the LSP range exactly as the server
// sent it, plus the byte-based coordinates a CLI consumer needs, plus
// the matched line.
//
// Construct one with NewSpan so the conversion goes through the
// vendored gopls Mapper; that is the whole reason position encoding is
// not a source of bugs here. A hand-built Span renders fine but has no
// source text, so Options.Context has nothing to show.
type Span struct {
	URI   protocol.DocumentURI `json:"uri"`
	Path  string               `json:"path"`
	Range protocol.Range       `json:"range"`
	Start Point                `json:"start"`
	End   Point                `json:"end"`
	// Text is the matched line — the line containing Start — with its
	// terminator stripped. Just the matched line is the token
	// discipline default of PLAN §4; Options.Context adds neighbours.
	Text string `json:"text"`

	// mapper is the file content the span was resolved against, kept so
	// that Options.Context can read surrounding lines. Unexported: it
	// is a render-time detail, not part of the output contract.
	mapper *protocol.Mapper
}

// NewSpan resolves an LSP range against a file's content.
//
// It fails if the range does not fit the content — a range past the end
// of the file, or a UTF-16 column in the middle of a surrogate pair.
// That is a protocol error on the server's part and worth surfacing:
// rendering a bogus location as `file:0:0` would be worse.
func NewSpan(m *protocol.Mapper, rng protocol.Range) (Span, error) {
	if m == nil {
		return Span{}, Errorf(CodeInternal, "NewSpan: nil mapper")
	}
	start, end, err := m.RangeOffsets(rng)
	if err != nil {
		return Span{}, Errorf(CodeProtocolError, "%s: invalid range %v: %v", m.URI.Path(), rng, err)
	}
	startLine, startCol := m.OffsetLineCol8(start)
	endLine, endCol := m.OffsetLineCol8(end)

	lo, hi := lineBounds(m.Content, start)
	return Span{
		URI:    m.URI,
		Path:   m.URI.Path(),
		Range:  rng,
		Start:  Point{Line: startLine, Column: startCol, Offset: start},
		End:    Point{Line: endLine, Column: endCol, Offset: end},
		Text:   string(m.Content[lo:hi]),
		mapper: m,
	}, nil
}

// NewSpanFromLocation resolves an LSP location. The mapper must hold the
// content of the location's file; a mismatch is a caller bug, and is
// reported rather than silently producing coordinates for the wrong
// file.
func NewSpanFromLocation(m *protocol.Mapper, loc protocol.Location) (Span, error) {
	if m == nil {
		return Span{}, Errorf(CodeInternal, "NewSpanFromLocation: nil mapper")
	}
	if m.URI != loc.URI {
		return Span{}, Errorf(CodeInternal, "NewSpanFromLocation: mapper is for %s, location is in %s",
			m.URI, loc.URI)
	}
	return NewSpan(m, loc.Range)
}

// ContextLine is one line of surrounding source, added by
// Options.Context.
type ContextLine struct {
	Line int    `json:"line"`
	Text string `json:"text"`
}

// context returns up to n lines before and after the span's lines. It
// returns nothing when the span was not built from a mapper.
func (s Span) context(n int) (before, after []ContextLine) {
	if n <= 0 || s.mapper == nil {
		return nil, nil
	}
	content := s.mapper.Content

	// Walk backwards from the start of the matched line.
	lo, _ := lineBounds(content, s.Start.Offset)
	line := s.Start.Line
	for range n {
		if lo == 0 {
			break
		}
		var hi int
		lo, hi = lineBounds(content, lo-1)
		line--
		before = append(before, ContextLine{Line: line, Text: string(content[lo:hi])})
	}
	// Collected nearest-first; report in source order.
	slices.Reverse(before)

	// Walk forwards from the end of the span's last line, so a
	// multi-line match's context begins after the match.
	_, hi := lineBounds(content, s.End.Offset)
	line = s.End.Line
	for range n {
		next := nextLineStart(content, hi)
		if next < 0 {
			break
		}
		lo, hi = lineBounds(content, next)
		line++
		after = append(after, ContextLine{Line: line, Text: string(content[lo:hi])})
	}
	return before, after
}

// lineBounds returns the byte range of the line containing offset,
// excluding the line terminator: content[lo:hi] is the line's text. A
// "\r\n" terminator is excluded whole, so CRLF files do not render a
// stray carriage return into `file:line:col:` output.
func lineBounds(content []byte, offset int) (lo, hi int) {
	offset = min(max(offset, 0), len(content))
	lo = 0
	if i := bytes.LastIndexByte(content[:offset], '\n'); i >= 0 {
		lo = i + 1
	}
	hi = len(content)
	if i := bytes.IndexByte(content[offset:], '\n'); i >= 0 {
		hi = offset + i
	}
	if hi > lo && content[hi-1] == '\r' {
		hi--
	}
	return lo, hi
}

// nextLineStart returns the offset of the line after the one ending at
// hi, or -1 if there is none. hi is a lineBounds high water mark, so it
// may sit on the '\r' of a CRLF pair rather than the '\n'. A file
// ending in a newline has no further line, only a terminator.
func nextLineStart(content []byte, hi int) int {
	i := hi
	if i < len(content) && content[i] == '\r' {
		i++
	}
	if i < len(content) && content[i] == '\n' {
		i++
	} else {
		return -1 // hi was end of file
	}
	if i >= len(content) {
		return -1 // trailing newline: no line follows it
	}
	return i
}
