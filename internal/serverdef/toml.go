package serverdef

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tomlError is a parse error carrying the 1-based line it was found
// on, because the whole point of a config file error is being able to
// go and fix the line.
type tomlError struct {
	line int
	msg  string
}

func (e *tomlError) Error() string { return fmt.Sprintf("line %d: %s", e.line, e.msg) }

// parseTOML parses the TOML subset documented on the package into a
// tree of map[string]any whose leaves are string, int64, float64 and
// bool, and whose containers are map[string]any and []any.
func parseTOML(src []byte) (map[string]any, error) {
	root := map[string]any{}
	p := &tomlParser{src: src, line: 1, root: root, cur: root, headers: map[string]bool{}}
	if err := p.parse(); err != nil {
		return nil, err
	}
	return root, nil
}

type tomlParser struct {
	src  []byte
	pos  int
	line int

	root map[string]any

	// cur is the table that bare key/value pairs land in, and
	// curName its dotted name for error messages.
	cur     map[string]any
	curName string

	// headers records the [table] headers already seen, so a
	// redefinition is an error rather than a silent merge.
	headers map[string]bool
}

func (p *tomlParser) errf(format string, args ...any) error {
	return &tomlError{line: p.line, msg: fmt.Sprintf(format, args...)}
}

func (p *tomlParser) eof() bool { return p.pos >= len(p.src) }

// at returns the byte n positions ahead, or 0 past the end. 0 is safe
// as a sentinel: a NUL byte is not valid anywhere in the subset.
func (p *tomlParser) at(n int) byte {
	if p.pos+n >= len(p.src) {
		return 0
	}
	return p.src[p.pos+n]
}

func (p *tomlParser) peek() byte { return p.at(0) }

// adv consumes and returns one byte, maintaining the line counter.
func (p *tomlParser) adv() byte {
	c := p.src[p.pos]
	p.pos++
	if c == '\n' {
		p.line++
	}
	return c
}

// skipInline consumes spaces and tabs only.
func (p *tomlParser) skipInline() {
	for !p.eof() && (p.peek() == ' ' || p.peek() == '\t') {
		p.adv()
	}
}

// skipBlank consumes whitespace, newlines and comments.
func (p *tomlParser) skipBlank() {
	for !p.eof() {
		switch p.peek() {
		case ' ', '\t', '\r', '\n':
			p.adv()
		case '#':
			p.skipComment()
		default:
			return
		}
	}
}

func (p *tomlParser) skipComment() {
	for !p.eof() && p.peek() != '\n' {
		p.adv()
	}
}

func (p *tomlParser) parse() error {
	for {
		p.skipBlank()
		if p.eof() {
			return nil
		}
		if p.peek() == '[' {
			if err := p.parseHeader(); err != nil {
				return err
			}
		} else if err := p.parseKeyValue(p.cur, p.curName); err != nil {
			return err
		}
		if err := p.endOfLine(); err != nil {
			return err
		}
	}
}

// endOfLine requires that nothing but a comment follows on the line.
func (p *tomlParser) endOfLine() error {
	p.skipInline()
	if p.peek() == '#' {
		p.skipComment()
	}
	switch {
	case p.eof():
		return nil
	case p.peek() == '\n':
		p.adv()
		return nil
	case p.peek() == '\r' && p.at(1) == '\n':
		p.adv()
		p.adv()
		return nil
	default:
		return p.errf("unexpected %s after value; expected end of line", describe(p.peek()))
	}
}

func (p *tomlParser) parseHeader() error {
	p.adv() // '['
	if p.peek() == '[' {
		return p.errf("arrays of tables ([[...]]) are not supported; a definition file describes exactly one server")
	}
	path, err := p.parseKeyPath()
	if err != nil {
		return err
	}
	p.skipInline()
	if p.peek() != ']' {
		return p.errf("expected ']' to close table header [%s]", strings.Join(path, "."))
	}
	p.adv()

	name := strings.Join(path, ".")
	if p.headers[name] {
		return p.errf("table [%s] is defined more than once", name)
	}
	p.headers[name] = true

	tbl, err := p.descend(p.root, path, name)
	if err != nil {
		return err
	}
	p.cur, p.curName = tbl, name
	return nil
}

// parseKeyValue parses `key = value`, or `a.b.c = value`, into tbl.
// prefix is the dotted name of tbl, used only in error messages.
func (p *tomlParser) parseKeyValue(tbl map[string]any, prefix string) error {
	path, err := p.parseKeyPath()
	if err != nil {
		return err
	}
	p.skipInline()
	if p.peek() != '=' {
		return p.errf("expected '=' after key %q, found %s", qualify(prefix, strings.Join(path, ".")), describe(p.peek()))
	}
	p.adv()
	p.skipInline()
	v, err := p.parseValue()
	if err != nil {
		return err
	}

	parent := tbl
	if len(path) > 1 {
		parent, err = p.descend(tbl, path[:len(path)-1], qualify(prefix, strings.Join(path[:len(path)-1], ".")))
		if err != nil {
			return err
		}
	}
	last := path[len(path)-1]
	if _, dup := parent[last]; dup {
		return p.errf("duplicate key %q", qualify(prefix, strings.Join(path, ".")))
	}
	parent[last] = v
	return nil
}

// descend walks (creating as needed) the table at path under tbl.
// name is the dotted name of the target, for error messages.
func (p *tomlParser) descend(tbl map[string]any, path []string, name string) (map[string]any, error) {
	cur := tbl
	for _, part := range path {
		switch existing := cur[part].(type) {
		case nil:
			next := map[string]any{}
			cur[part] = next
			cur = next
		case map[string]any:
			cur = existing
		default:
			return nil, p.errf("cannot define table %q: %q is already a value", name, part)
		}
	}
	return cur, nil
}

func (p *tomlParser) parseKeyPath() ([]string, error) {
	var parts []string
	for {
		p.skipInline()
		part, err := p.parseKey()
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
		p.skipInline()
		if p.peek() != '.' {
			return parts, nil
		}
		p.adv()
	}
}

func (p *tomlParser) parseKey() (string, error) {
	switch p.peek() {
	case '"':
		return p.parseBasicString()
	case '\'':
		return p.parseLiteralString()
	}
	start := p.pos
	for !p.eof() && isBareKeyByte(p.peek()) {
		p.adv()
	}
	if p.pos == start {
		return "", p.errf("expected a key, found %s", describe(p.peek()))
	}
	return string(p.src[start:p.pos]), nil
}

func isBareKeyByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

func (p *tomlParser) parseValue() (any, error) {
	switch c := p.peek(); {
	case c == '"':
		if p.at(1) == '"' && p.at(2) == '"' {
			return nil, p.errf("multi-line strings are not supported")
		}
		return p.parseBasicString()
	case c == '\'':
		if p.at(1) == '\'' && p.at(2) == '\'' {
			return nil, p.errf("multi-line strings are not supported")
		}
		return p.parseLiteralString()
	case c == '[':
		return p.parseArray()
	case c == '{':
		return p.parseInlineTable()
	case c == 0:
		return nil, p.errf("expected a value, found end of input")
	default:
		return p.parseScalar()
	}
}

func (p *tomlParser) parseBasicString() (string, error) {
	p.adv() // '"'
	var b strings.Builder
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errf("unterminated string")
		}
		switch c := p.adv(); c {
		case '"':
			return b.String(), nil
		case '\\':
			if err := p.parseEscape(&b); err != nil {
				return "", err
			}
		default:
			b.WriteByte(c)
		}
	}
}

func (p *tomlParser) parseEscape(b *strings.Builder) error {
	if p.eof() {
		return p.errf("unterminated escape sequence")
	}
	switch c := p.adv(); c {
	case 'b':
		b.WriteByte('\b')
	case 't':
		b.WriteByte('\t')
	case 'n':
		b.WriteByte('\n')
	case 'f':
		b.WriteByte('\f')
	case 'r':
		b.WriteByte('\r')
	case '"':
		b.WriteByte('"')
	case '\\':
		b.WriteByte('\\')
	case 'u':
		return p.parseUnicodeEscape(b, 4)
	case 'U':
		return p.parseUnicodeEscape(b, 8)
	default:
		return p.errf("unknown escape sequence \\%c", c)
	}
	return nil
}

func (p *tomlParser) parseUnicodeEscape(b *strings.Builder, digits int) error {
	if p.pos+digits > len(p.src) {
		return p.errf("truncated unicode escape")
	}
	hex := string(p.src[p.pos : p.pos+digits])
	n, err := strconv.ParseUint(hex, 16, 32)
	if err != nil {
		return p.errf("invalid unicode escape %q", hex)
	}
	if !utf8.ValidRune(rune(n)) {
		return p.errf("unicode escape %q is not a valid code point", hex)
	}
	for range digits {
		p.adv()
	}
	b.WriteRune(rune(n))
	return nil
}

func (p *tomlParser) parseLiteralString() (string, error) {
	p.adv() // '\''
	start := p.pos
	for {
		if p.eof() || p.peek() == '\n' {
			return "", p.errf("unterminated literal string")
		}
		if p.adv() == '\'' {
			return string(p.src[start : p.pos-1]), nil
		}
	}
}

func (p *tomlParser) parseArray() (any, error) {
	p.adv() // '['
	out := []any{}
	for {
		p.skipBlank()
		if p.eof() {
			return nil, p.errf("unterminated array")
		}
		if p.peek() == ']' {
			p.adv()
			return out, nil
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.skipBlank()
		switch p.peek() {
		case ',':
			p.adv()
		case ']':
			p.adv()
			return out, nil
		default:
			return nil, p.errf("expected ',' or ']' in array, found %s", describe(p.peek()))
		}
	}
}

func (p *tomlParser) parseInlineTable() (any, error) {
	p.adv() // '{'
	tbl := map[string]any{}
	p.skipBlank()
	if p.peek() == '}' {
		p.adv()
		return tbl, nil
	}
	for {
		if err := p.parseKeyValue(tbl, ""); err != nil {
			return nil, err
		}
		p.skipBlank()
		switch p.peek() {
		case ',':
			p.adv()
			p.skipBlank()
			// A trailing comma before '}' is accepted, for the same
			// reason a newline is.
			if p.peek() == '}' {
				p.adv()
				return tbl, nil
			}
		case '}':
			p.adv()
			return tbl, nil
		default:
			return nil, p.errf("expected ',' or '}' in inline table, found %s", describe(p.peek()))
		}
	}
}

// parseScalar handles booleans, integers and floats, and gives a
// pointed error for the date-time forms the subset excludes.
func (p *tomlParser) parseScalar() (any, error) {
	start := p.pos
	for !p.eof() && !isValueDelim(p.peek()) {
		p.adv()
	}
	tok := string(p.src[start:p.pos])
	switch tok {
	case "":
		return nil, p.errf("expected a value, found %s", describe(p.peek()))
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	if looksLikeDateTime(tok) {
		return nil, p.errf("date and time values are not supported (%q); quote it if a string was meant", tok)
	}
	if n, err := strconv.ParseInt(tok, 0, 64); err == nil {
		return n, nil
	}
	if f, err := strconv.ParseFloat(tok, 64); err == nil {
		return f, nil
	}
	return nil, p.errf("invalid value %q", tok)
}

// isValueDelim reports whether c ends a bare scalar.
func isValueDelim(c byte) bool {
	switch c {
	case ' ', '\t', '\r', '\n', ',', ']', '}', '#':
		return true
	}
	return false
}

// looksLikeDateTime spots TOML's date-time forms (1979-05-27,
// 07:32:00, 1979-05-27T07:32:00Z) so they get a specific error rather
// than "invalid value".
func looksLikeDateTime(tok string) bool {
	if strings.ContainsRune(tok, ':') {
		return true
	}
	if len(tok) < 8 || tok[4] != '-' {
		return false
	}
	for i := range 4 {
		if tok[i] < '0' || tok[i] > '9' {
			return false
		}
	}
	return true
}

// describe renders a byte for an error message.
func describe(c byte) string {
	switch c {
	case 0:
		return "end of input"
	case '\n':
		return "end of line"
	case '\t':
		return "a tab"
	}
	return strconv.QuoteRune(rune(c))
}

// qualify joins a table prefix and a key into a dotted name.
func qualify(prefix, key string) string {
	if prefix == "" {
		return key
	}
	return prefix + "." + key
}
