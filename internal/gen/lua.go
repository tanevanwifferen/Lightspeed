package gen

import (
	"fmt"
	"strconv"
	"strings"
)

// This file is a reader for the declarative subset of a Lua file: the
// table constructor an nvim-lspconfig server module returns. It is not
// a Lua interpreter and must never become one — the corpus contains
// real code (a `root_dir` that shells out to `cargo metadata`, an
// `on_attach` that creates editor commands), and evaluating any of it
// would mean running upstream's logic at our build time.
//
// The contract is therefore: literals are imported, everything else is
// recorded as [Unsupported] with a reason, and the reason survives into
// the generated file so that a curated value can be justified rather
// than merely asserted.

// A Value is one Lua value from the declarative subset: string, int64,
// float64, bool, nil, *Table, or Unsupported for anything that would
// have to be executed to know.
type Value any

// Unsupported marks an expression the reader deliberately did not
// import, with the reason it did not.
type Unsupported struct {
	Reason string
}

func (u Unsupported) String() string { return "unsupported: " + u.Reason }

// A Table is a Lua table constructor, in source order: Lua tables are
// unordered, but the corpus uses positional entries as lists and the
// order of a root-marker list is significant to us.
type Table struct {
	Entries []Entry
}

// An Entry is one table entry. Key is empty for a positional entry.
type Entry struct {
	Key   string
	Value Value
}

// Get returns the value of the named key.
func (t *Table) Get(key string) (Value, bool) {
	if t == nil {
		return nil, false
	}
	for _, e := range t.Entries {
		if e.Key == key {
			return e.Value, true
		}
	}
	return nil, false
}

// Keys lists the named keys, in source order.
func (t *Table) Keys() []string {
	if t == nil {
		return nil
	}
	var out []string
	for _, e := range t.Entries {
		if e.Key != "" {
			out = append(out, e.Key)
		}
	}
	return out
}

// List returns the positional values, in source order.
func (t *Table) List() []Value {
	if t == nil {
		return nil
	}
	var out []Value
	for _, e := range t.Entries {
		if e.Key == "" {
			out = append(out, e.Value)
		}
	}
	return out
}

// Strings flattens the table into a list of strings. Nested tables are
// flattened in order, which is how nvim-lspconfig ≥ 0.11.3 writes
// equal-priority root-marker groups; the flattened order is a
// significance order for us, and that is a deliberate, documented
// narrowing rather than an accident.
//
// An entry that is not a string (or a table of them) is an error: a
// half-imported list is worse than none.
func (t *Table) Strings() ([]string, error) {
	var out []string
	for _, v := range t.List() {
		switch v := v.(type) {
		case string:
			out = append(out, v)
		case *Table:
			nested, err := v.Strings()
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
		case Unsupported:
			return nil, fmt.Errorf("list entry is %s", v)
		default:
			return nil, fmt.Errorf("list entry is %T, want a string", v)
		}
	}
	return out, nil
}

// Map converts the table to the map[string]any shape a server
// definition's free-form tables use. Positional entries and unsupported
// values are dropped, and their keys reported.
func (t *Table) Map() (map[string]any, []string) {
	out := map[string]any{}
	var dropped []string
	for _, e := range t.Entries {
		if e.Key == "" {
			dropped = append(dropped, "(positional entry)")
			continue
		}
		switch v := e.Value.(type) {
		case *Table:
			sub, subDropped := v.Map()
			for _, d := range subDropped {
				dropped = append(dropped, e.Key+"."+d)
			}
			out[e.Key] = sub
		case Unsupported:
			dropped = append(dropped, e.Key)
		default:
			out[e.Key] = v
		}
	}
	return out, dropped
}

// ReturnedTable parses the table constructor a Lua module returns: the
// last `return {` in the file, whose constructor must run to the end of
// the file. That last condition is the safety net — if the file returns
// something else, or has statements after the table, the parse fails
// loudly instead of importing a table from the middle of a function.
func ReturnedTable(src []byte) (*Table, error) {
	toks, err := lex(src)
	if err != nil {
		return nil, err
	}
	start := -1
	for i := 0; i < len(toks)-1; i++ {
		if toks[i].kind == tokName && toks[i].text == "return" && toks[i+1].kind == tokPunct && toks[i+1].text == "{" {
			start = i + 1
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no `return {` found: the file does not return a table constructor")
	}
	p := &luaParser{toks: toks, pos: start}
	tbl, err := p.table()
	if err != nil {
		return nil, err
	}
	if p.pos != len(toks) {
		return nil, fmt.Errorf("line %d: the returned table is followed by more code, so it is not the module's return value", toks[p.pos].line)
	}
	return tbl, nil
}

// --- lexer

type tokenKind int

const (
	tokName tokenKind = iota // identifier or keyword
	tokString
	tokNumber
	tokPunct
)

type token struct {
	kind tokenKind
	text string // for tokString, the decoded value
	line int
}

func lex(src []byte) ([]token, error) {
	var toks []token
	s := string(src)
	line := 1
	i := 0

	for i < len(s) {
		c := s[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			i += 2
			// A long comment is --[[ … ]] or --[==[ … ]==].
			if level, ok := longBracket(s, i); ok {
				end, endLine, err := skipLongBracket(s, i, level, line)
				if err != nil {
					return nil, fmt.Errorf("line %d: unterminated long comment", line)
				}
				i, line = end, endLine
				continue
			}
			for i < len(s) && s[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			value, end, err := shortString(s, i)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", line, err)
			}
			toks = append(toks, token{kind: tokString, text: value, line: line})
			i = end
		case c == '[' && func() bool { _, ok := longBracket(s, i); return ok }():
			level, _ := longBracket(s, i)
			start := i + level + 2
			end, endLine, err := skipLongBracket(s, i, level, line)
			if err != nil {
				return nil, fmt.Errorf("line %d: unterminated long string", line)
			}
			value := s[start : end-(level+2)]
			toks = append(toks, token{kind: tokString, text: strings.TrimPrefix(value, "\n"), line: line})
			i, line = end, endLine
		case isNameStart(c):
			j := i
			for j < len(s) && isNameByte(s[j]) {
				j++
			}
			toks = append(toks, token{kind: tokName, text: s[i:j], line: line})
			i = j
		case c >= '0' && c <= '9':
			j := scanNumber(s, i)
			toks = append(toks, token{kind: tokNumber, text: s[i:j], line: line})
			i = j
		default:
			// Operators: the longest ones the corpus uses.
			for _, op := range []string{"...", "==", "~=", "<=", ">=", "//", "::", ".."} {
				if strings.HasPrefix(s[i:], op) {
					toks = append(toks, token{kind: tokPunct, text: op, line: line})
					i += len(op)
					goto next
				}
			}
			toks = append(toks, token{kind: tokPunct, text: string(c), line: line})
			i++
		}
	next:
	}
	return toks, nil
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameByte(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// scanNumber returns the index just past the numeral starting at i. A
// sign is only part of the numeral directly after an exponent marker,
// so that `1-2` is three tokens and `1e-5` is one.
func scanNumber(s string, i int) int {
	j := i
	for j < len(s) {
		c := s[j]
		switch {
		case (c >= '0' && c <= '9') || c == '.' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || c == 'x' || c == 'X':
			j++
		case (c == '-' || c == '+') && j > i:
			switch prev := s[j-1]; prev {
			case 'e', 'E', 'p', 'P':
				j++
			default:
				return j
			}
		default:
			return j
		}
	}
	return j
}

// longBracket reports the level of a long bracket starting at i, that
// is the number of '=' between the two '['.
func longBracket(s string, i int) (int, bool) {
	if i >= len(s) || s[i] != '[' {
		return 0, false
	}
	j := i + 1
	for j < len(s) && s[j] == '=' {
		j++
	}
	if j < len(s) && s[j] == '[' {
		return j - i - 1, true
	}
	return 0, false
}

// skipLongBracket returns the index just past the closing bracket.
func skipLongBracket(s string, i, level, line int) (int, int, error) {
	closing := "]" + strings.Repeat("=", level) + "]"
	j := i + level + 2
	for j < len(s) {
		if s[j] == '\n' {
			line++
		}
		if strings.HasPrefix(s[j:], closing) {
			return j + len(closing), line, nil
		}
		j++
	}
	return 0, line, fmt.Errorf("unterminated")
}

// shortString decodes a quoted Lua string, returning the value and the
// index just past the closing quote.
func shortString(s string, i int) (string, int, error) {
	quote := s[i]
	var b strings.Builder
	j := i + 1
	for j < len(s) {
		c := s[j]
		switch c {
		case quote:
			return b.String(), j + 1, nil
		case '\n':
			return "", 0, fmt.Errorf("unterminated string")
		case '\\':
			j++
			if j >= len(s) {
				return "", 0, fmt.Errorf("unterminated escape")
			}
			switch e := s[j]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'a':
				b.WriteByte(7)
			case 'b':
				b.WriteByte(8)
			case 'f':
				b.WriteByte(12)
			case 'v':
				b.WriteByte(11)
			case '\\', '"', '\'':
				b.WriteByte(e)
			case '\n':
				b.WriteByte('\n')
			default:
				// \ddd and \xXX and \u{…} are not in the corpus; a
				// definition that needs one can be curated.
				return "", 0, fmt.Errorf("unsupported string escape \\%c", e)
			}
			j++
		default:
			b.WriteByte(c)
			j++
		}
	}
	return "", 0, fmt.Errorf("unterminated string")
}

// --- parser

type luaParser struct {
	toks []token
	pos  int
}

func (p *luaParser) peek() (token, bool) {
	if p.pos >= len(p.toks) {
		return token{}, false
	}
	return p.toks[p.pos], true
}

func (p *luaParser) next() (token, bool) {
	t, ok := p.peek()
	if ok {
		p.pos++
	}
	return t, ok
}

func (p *luaParser) isPunct(text string) bool {
	t, ok := p.peek()
	return ok && t.kind == tokPunct && t.text == text
}

func (p *luaParser) line() int {
	if p.pos < len(p.toks) {
		return p.toks[p.pos].line
	}
	if len(p.toks) > 0 {
		return p.toks[len(p.toks)-1].line
	}
	return 0
}

// table parses a table constructor, the current token being '{'.
func (p *luaParser) table() (*Table, error) {
	if !p.isPunct("{") {
		return nil, fmt.Errorf("line %d: expected '{'", p.line())
	}
	p.pos++
	tbl := &Table{}
	for {
		if p.pos >= len(p.toks) {
			return nil, fmt.Errorf("unterminated table constructor")
		}
		if p.isPunct("}") {
			p.pos++
			return tbl, nil
		}
		entry, err := p.entry()
		if err != nil {
			return nil, err
		}
		tbl.Entries = append(tbl.Entries, entry)
		if p.isPunct(",") || p.isPunct(";") {
			p.pos++
			continue
		}
		if p.isPunct("}") {
			p.pos++
			return tbl, nil
		}
		return nil, fmt.Errorf("line %d: expected ',' or '}' in table constructor", p.line())
	}
}

func (p *luaParser) entry() (Entry, error) {
	// name = value
	if t, ok := p.peek(); ok && t.kind == tokName && p.pos+1 < len(p.toks) &&
		p.toks[p.pos+1].kind == tokPunct && p.toks[p.pos+1].text == "=" && !isKeyword(t.text) {
		p.pos += 2
		v, err := p.value()
		return Entry{Key: t.text, Value: v}, err
	}
	// ['name'] = value, the corpus's way of writing a key that is not
	// an identifier, such as ['rust-analyzer'].
	if p.isPunct("[") && p.pos+3 < len(p.toks) &&
		p.toks[p.pos+1].kind == tokString &&
		p.toks[p.pos+2].kind == tokPunct && p.toks[p.pos+2].text == "]" &&
		p.toks[p.pos+3].kind == tokPunct && p.toks[p.pos+3].text == "=" {
		key := p.toks[p.pos+1].text
		p.pos += 4
		v, err := p.value()
		return Entry{Key: key, Value: v}, err
	}
	v, err := p.value()
	return Entry{Value: v}, err
}

// value parses one expression, importing literals and skipping over
// anything else.
func (p *luaParser) value() (Value, error) {
	t, ok := p.peek()
	if !ok {
		return nil, fmt.Errorf("unexpected end of file in table constructor")
	}
	// A literal only counts as a literal if the expression ends there:
	// `'a' .. b` is a concatenation, not the string 'a'.
	simple := func(v Value) (Value, error) {
		p.pos++
		if p.atEntryEnd() {
			return v, nil
		}
		if err := p.skipExpression(); err != nil {
			return nil, err
		}
		return Unsupported{Reason: "compound expression"}, nil
	}

	switch {
	case t.kind == tokString:
		return simple(t.text)
	case t.kind == tokNumber:
		n, err := parseNumber(t.text)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", t.line, err)
		}
		return simple(n)
	case t.kind == tokName && t.text == "true":
		return simple(true)
	case t.kind == tokName && t.text == "false":
		return simple(false)
	case t.kind == tokName && t.text == "nil":
		return simple(nil)
	case t.kind == tokPunct && t.text == "{":
		tbl, err := p.table()
		if err != nil {
			return nil, err
		}
		if p.atEntryEnd() {
			return tbl, nil
		}
		if err := p.skipExpression(); err != nil {
			return nil, err
		}
		return Unsupported{Reason: "compound expression"}, nil
	case t.kind == tokName && t.text == "function":
		if err := p.skipExpression(); err != nil {
			return nil, err
		}
		return Unsupported{Reason: "a Lua function; declarative config cannot express it"}, nil
	default:
		if err := p.skipExpression(); err != nil {
			return nil, err
		}
		return Unsupported{Reason: "a computed expression"}, nil
	}
}

// atEntryEnd reports whether the current token closes the entry.
func (p *luaParser) atEntryEnd() bool {
	t, ok := p.peek()
	if !ok {
		return true
	}
	return t.kind == tokPunct && (t.text == "," || t.text == ";" || t.text == "}")
}

// skipExpression consumes tokens until the end of the current entry,
// keeping brackets and Lua blocks balanced so that a function body — or
// a `vim.fn.has('nvim-0.11.3') == 1 and {…} or {…}` — is skipped whole.
func (p *luaParser) skipExpression() error {
	depth := 0     // (), [], {}
	blocks := 0    // function/if/for/while/do/repeat
	pendingDo := 0 // a `do` that belongs to a for/while and opens no new block
	for {
		t, ok := p.peek()
		if !ok {
			return fmt.Errorf("unexpected end of file while skipping an expression")
		}
		if depth == 0 && blocks == 0 && t.kind == tokPunct && (t.text == "," || t.text == ";" || t.text == "}") {
			return nil
		}
		p.pos++
		switch {
		case t.kind == tokPunct && (t.text == "(" || t.text == "[" || t.text == "{"):
			depth++
		case t.kind == tokPunct && (t.text == ")" || t.text == "]" || t.text == "}"):
			depth--
		case t.kind == tokName:
			switch t.text {
			case "function", "if", "repeat":
				blocks++
			case "for", "while":
				blocks++
				pendingDo++
			case "do":
				if pendingDo > 0 {
					pendingDo--
				} else {
					blocks++
				}
			case "end", "until":
				blocks--
			}
		}
	}
}

func isKeyword(s string) bool {
	switch s {
	case "and", "break", "do", "else", "elseif", "end", "false", "for", "function",
		"goto", "if", "in", "local", "nil", "not", "or", "repeat", "return",
		"then", "true", "until", "while":
		return true
	}
	return false
}

func parseNumber(text string) (Value, error) {
	if i, err := strconv.ParseInt(text, 0, 64); err == nil {
		return i, nil
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, fmt.Errorf("cannot parse number %q", text)
	}
	return f, nil
}
