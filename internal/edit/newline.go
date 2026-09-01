package edit

import (
	"bytes"
	"strings"
)

// lineEnding is the line terminator a file uses, as far as it can be
// determined from its bytes.
type lineEnding int

const (
	// eolUnknown is a file with no line terminators at all, or one
	// that mixes them. Nothing is rewritten in either case: with no
	// evidence, or with contradictory evidence, guessing is worse than
	// leaving the server's text alone.
	eolUnknown lineEnding = iota
	// eolLF is a file where every newline is a bare \n.
	eolLF
	// eolCRLF is a file where every newline is preceded by \r.
	eolCRLF
)

func (e lineEnding) String() string {
	switch e {
	case eolLF:
		return "LF"
	case eolCRLF:
		return "CRLF"
	default:
		return "unknown"
	}
}

// detectEOL reports the line terminator content uses.
//
// A file counts as CRLF only if *every* newline in it is preceded by a
// carriage return. One stray bare \n makes the file mixed, and mixed
// files are left exactly as they are: rewriting them would change
// bytes no edit asked to change.
func detectEOL(content []byte) lineEnding {
	total := bytes.Count(content, []byte("\n"))
	if total == 0 {
		return eolUnknown
	}
	crlf := bytes.Count(content, []byte("\r\n"))
	switch crlf {
	case 0:
		return eolLF
	case total:
		return eolCRLF
	default:
		return eolUnknown
	}
}

// normalizeEOL rewrites the line terminators of text a server wants to
// insert so that they match the file it is being inserted into.
//
// Servers overwhelmingly emit \n regardless of what the file uses;
// inserting that verbatim into a CRLF file leaves the file mixed,
// which is a change to the file's line endings that the edit did not
// ask for and that shows up as a whole-file diff in the next commit.
// Only the CRLF direction is rewritten — see eolUnknown for why the
// ambiguous cases are left alone.
func normalizeEOL(text string, eol lineEnding) string {
	if eol != eolCRLF || !strings.Contains(text, "\n") {
		return text
	}
	// Collapse first so that text already using \r\n is not doubled.
	return strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
}

// hasFinalNewline reports whether content ends with a line terminator.
// Nothing in this package ever adds or removes one; the value exists so
// that a test can assert the file it started with is the file it ends
// with in that respect.
func hasFinalNewline(content []byte) bool {
	return len(content) > 0 && content[len(content)-1] == '\n'
}
