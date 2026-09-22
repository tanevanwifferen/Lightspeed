package symbols

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// DecodeFile decodes a documentSymbol answer for the file whose text m holds
// and pinpoints the names of a flat one. The warnings say how many names could
// not be located.
func DecodeFile(raw json.RawMessage, uri protocol.DocumentURI, m *protocol.Mapper) ([]Symbol, []string, error) {
	syms, err := DecodeDocumentSymbols(raw, uri)
	if err != nil {
		return nil, nil, err
	}
	return syms, PinpointNames(m, syms), nil
}

// maxDocRunes bounds the doc sentence an index keeps per symbol.
const maxDocRunes = 200

// docDirectives are comment lines that are instructions to a tool and not
// prose: `//go:generate`, `//nolint`, `// +build`, `# type: ignore`.
var docDirectives = []string{"go:", "nolint", "+build", "noqa", "nosec", "eslint-", "@ts-", "lint:"}

// LeadingDoc is the first sentence of the comment directly above the
// declaration that starts at 0-based line first, or "" when there is none. The
// comment block is the one [LineIndex.LeadingStart] finds, so it stops at a
// blank line: a comment separated from the code is not its documentation.
// Decorators and attributes between the comment and the declaration are
// skipped over, and the comment markers of the common languages (`//`, `///`,
// `/** */`, `*`, `#`, `--`, `;;`) are removed.
func (x *LineIndex) LeadingDoc(first int) string {
	start := x.LeadingStart(first)
	if start >= first {
		return ""
	}
	var lines []string
	for i := start; i < first; i++ {
		raw := x.Text(i)
		if isDecoratorLine(raw) {
			continue
		}
		text, ok := stripCommentMarker(raw)
		if !ok {
			continue
		}
		lines = append(lines, text)
	}
	return firstSentence(lines)
}

// PythonDocstring is the first sentence of the docstring that opens the body of
// the Python declaration spanning 0-based lines first..last, or "".
func (x *LineIndex) PythonDocstring(first, last int) string {
	limit := min(last, first+12, x.Count()-1)
	for i := first; i <= limit; i++ {
		line := x.Text(i)
		for _, p := range []string{"r", "R", "u", "U", "b", "B"} {
			line = strings.TrimPrefix(line, p)
		}
		for _, q := range []string{`"""`, `'''`} {
			if !strings.HasPrefix(line, q) {
				continue
			}
			var lines []string
			rest := strings.TrimPrefix(line, q)
			for j := i; j <= last && j < i+20; j++ {
				if j > i {
					rest = x.Text(j)
				}
				if end := strings.Index(rest, q); end >= 0 {
					lines = append(lines, strings.TrimSpace(rest[:end]))
					return firstSentence(lines)
				}
				lines = append(lines, strings.TrimSpace(rest))
			}
			return firstSentence(lines)
		}
	}
	return ""
}

// isDecoratorLine reports whether a line between a comment and its declaration
// is a decorator or attribute (`@Override`, `#[derive(Debug)]`) and so carries
// no prose.
func isDecoratorLine(line string) bool {
	return strings.HasPrefix(line, "@") || strings.HasPrefix(line, "#[") || strings.HasPrefix(line, "#![")
}

// stripCommentMarker removes the comment syntax from one line. ok is false for a
// line that is not prose: a directive, or a line with nothing left.
func stripCommentMarker(line string) (string, bool) {
	s := line
	for _, p := range []string{"///", "//!", "//", "/**", "/*!", "/*", ";;", "--", "#"} {
		if strings.HasPrefix(s, p) {
			s = strings.TrimPrefix(s, p)
			break
		}
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSpace(strings.TrimSuffix(s, "*/"))
	s = strings.TrimSpace(strings.TrimPrefix(s, "*"))
	if s == "" {
		return "", false
	}
	for _, d := range docDirectives {
		if strings.HasPrefix(s, d) {
			return "", false
		}
	}
	return s, true
}

// firstSentence joins comment lines and cuts at the end of the first sentence:
// a full stop followed by a space or the end of the text, or the first
// paragraph break, whichever comes first, capped at maxDocRunes.
func firstSentence(lines []string) string {
	var b strings.Builder
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			if b.Len() > 0 {
				break // a blank line in the comment is a paragraph break
			}
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(l)
		if endsSentence(l) {
			break
		}
	}
	s := b.String()
	for i := 0; i < len(s); i++ {
		if s[i] == '.' && (i+1 == len(s) || s[i+1] == ' ') {
			s = s[:i+1]
			break
		}
	}
	if utf8.RuneCountInString(s) > maxDocRunes {
		r := []rune(s)
		s = string(r[:maxDocRunes]) + "…"
	}
	return s
}

func endsSentence(l string) bool {
	return strings.HasSuffix(l, ".")
}
