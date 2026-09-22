package symbols

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// A LineIndex is a file's lines, for slicing by line without copying.
//
// Lines end at "\n", which is what LSP's line numbers and the vendored Mapper
// count, and a "\r" before it is part of the terminator. A file that ends in a
// newline does not have an empty last line.
type LineIndex struct {
	content []byte
	starts  []int
}

func NewLineIndex(content []byte) *LineIndex {
	x := &LineIndex{content: content}
	if len(content) == 0 {
		return x
	}
	x.starts = append(x.starts, 0)
	for i, b := range content {
		if b == '\n' && i+1 < len(content) {
			x.starts = append(x.starts, i+1)
		}
	}
	return x
}

// Count is the number of lines.
func (x *LineIndex) Count() int { return len(x.starts) }

// Slice is lines lo through hi (0-based, inclusive) exactly as they are in the
// file, without the terminator of the last one.
func (x *LineIndex) Slice(lo, hi int) []byte {
	if x.Count() == 0 || lo > hi {
		return nil
	}
	start := x.starts[lo]
	end := len(x.content)
	if hi+1 < len(x.starts) {
		end = x.starts[hi+1]
	}
	out := x.content[start:end]
	out = bytes.TrimSuffix(out, []byte("\n"))
	return bytes.TrimSuffix(out, []byte("\r"))
}

// Text is line i, trimmed of its terminator and surrounding blanks.
func (x *LineIndex) Text(i int) string {
	return strings.TrimSpace(string(x.Slice(i, i)))
}

// EndLine is the last line (0-based) a symbol's declaration occupies. A range
// that ends at column 0 of a line ends *before* it, which is how most servers
// write a declaration that includes its trailing newline.
func EndLine(sym Symbol) int {
	end := sym.Full.End
	if end.Character == 0 && end.Line > sym.Full.Start.Line {
		return int(end.Line) - 1
	}
	return int(end.Line)
}

// commentLead are the openings of a line that belongs to the declaration below
// it: a comment, or a decorator or attribute. It is a heuristic across
// languages and deliberately a short one.
var commentLead = []string{"//", "/*", "*/", "--", "#", "@", ";;"}

// preprocessor are the `#` lines that are code, not comments.
var preprocessor = []string{"#include", "#define", "#undef", "#if", "#else", "#elif", "#endif", "#pragma", "#import", "#error", "#line"}

// isLeadingLine reports whether a line, directly above a declaration, is its
// doc comment or its decorator.
func isLeadingLine(line string) bool {
	for _, p := range preprocessor {
		if strings.HasPrefix(line, p) {
			return false
		}
	}
	for _, p := range commentLead {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	// The body lines of a /* */ block. `*p = 1` is not one.
	return line == "*" || strings.HasPrefix(line, "* ")
}

// LeadingStart moves a declaration's first line up over the comment and
// decorator lines that sit directly above it, stopping at a blank line: a
// comment separated from the code is not its documentation.
func (x *LineIndex) LeadingStart(first int) int {
	for first > 0 {
		if start, ok := x.blockCommentStart(first - 1); ok {
			first = start
			continue
		}
		if !isLeadingLine(x.Text(first - 1)) {
			break
		}
		first--
	}
	return first
}

// maxBlockCommentLines bounds the search for the opener of a block comment: a
// licence header is long, and a stray `*/` must not walk to the top of the
// file.
const maxBlockCommentLines = 400

// blockCommentStart reports whether line i closes a /* ... */ block that
// starts on a line of its own, and where. It is what finds a comment whose last
// line is text and not `*/` or ` * `, and one whose body lines have no leading
// star at all, which isLeadingLine's per-line rule cannot: a line has to be
// read as the end of the block to know the lines above it belong to it. A
// blank line inside the block is part of it (a licence has them); a line that
// closes an earlier comment, or code that merely ends in `*/`, is not a block.
func (x *LineIndex) blockCommentStart(i int) (int, bool) {
	if !strings.HasSuffix(x.Text(i), "*/") {
		return 0, false
	}
	for j := i; j >= 0 && i-j < maxBlockCommentLines; j-- {
		line := x.Text(j)
		if j < i && strings.Contains(line, "*/") {
			return 0, false
		}
		if strings.Contains(line, "/*") {
			return j, strings.HasPrefix(line, "/*")
		}
	}
	return 0, false
}

// signatureLimit bounds a signature: a declaration line, not a function body.
const (
	signatureLines = 6
	signatureBytes = 240
)

// DeclSignature is the declaration as written: the symbol's first line, and
// the ones after it while a parenthesis or bracket it opened is still open, cut
// at the block opener. It is what `outline` shows when the server gives no
// detail of its own, and it is sliced from the file so that it says what the
// code says (a server's detail is often a type without the name).
func (x *LineIndex) DeclSignature(first, last int) string {
	var parts []string
	depth := 0
	for i := first; i <= last && i < first+signatureLines && i < x.Count(); i++ {
		line := x.Text(i)
		parts = append(parts, line)
		for _, r := range line {
			switch r {
			case '(', '[':
				depth++
			case ')', ']':
				depth--
			}
		}
		if depth <= 0 {
			break
		}
	}
	sig := strings.Join(parts, " ")
	sig = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(sig), "{"))
	if len(sig) > signatureBytes {
		cut := signatureBytes
		for cut > 0 && !utf8.RuneStart(sig[cut]) {
			cut--
		}
		sig = sig[:cut] + "…"
	}
	return sig
}
