package index

import (
	"bytes"
	"sort"
)

// An Extractor reads the imports out of one language's source text. It is fed
// file text only — there is no parser for any language but Go behind it, and no
// language server — so each implementation is a careful scanner: it sees the
// text through a small lexer that skips comments and string literals, which is
// what keeps an `import` inside a comment, a docstring or a template literal
// from becoming an edge.
type Extractor interface {
	// Languages are the router language ids the extractor serves.
	Languages() []string
	// Extract returns the imports of src in source order. path is the file's
	// workspace-relative path, for the languages whose syntax depends on it.
	Extract(path string, src []byte) []ImportRef
}

var extractors = map[string]Extractor{}

func registerExtractor(e Extractor) {
	for _, l := range e.Languages() {
		extractors[l] = e
	}
}

func init() {
	registerExtractor(goExtractor{})
	registerExtractor(pythonExtractor{})
	registerExtractor(jsExtractor{})
	registerExtractor(rustExtractor{})
	registerExtractor(cExtractor{})
	registerExtractor(luaExtractor{})
}

// ExtractorFor is the extractor for a router language id, nil when the
// language has none. A nil answer is what makes a file "not covered" rather
// than "importing nothing".
func ExtractorFor(language string) Extractor { return extractors[language] }

// ExtractedLanguages lists the language ids that have an extractor, sorted.
func ExtractedLanguages() []string {
	out := make([]string, 0, len(extractors))
	for l := range extractors {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

// ExtractImports scans src for imports. covered is false when the language has
// no extractor: the caller must then say the file is uncovered, not that it has
// no imports.
func ExtractImports(language, path string, src []byte) (refs []ImportRef, covered bool) {
	e := ExtractorFor(language)
	if e == nil {
		return nil, false
	}
	return e.Extract(path, src), true
}

// --- the shared lexer ---

type tokKind uint8

const (
	tkIdent tokKind = iota + 1
	tkPunct
	tkString
	tkNewline
)

// A token is a lexeme with the line it starts on. Strings carry their raw
// contents (escapes are not decoded: an import specifier has none worth
// decoding); comments never become tokens.
type token struct {
	kind tokKind
	text string
	line int
}

func (t token) is(kind tokKind, text string) bool { return t.kind == kind && t.text == text }
func (t token) punct(text string) bool            { return t.kind == tkPunct && t.text == text }
func (t token) ident(text string) bool            { return t.kind == tkIdent && t.text == text }

// lexConfig says what the language's comments and strings look like.
type lexConfig struct {
	lineComment []string
	blockOpen   string
	blockClose  string
	nestBlock   bool
	// quotes are the characters that open an escaped, single-line string.
	quotes string
	// multilineQuotes lets those strings run over lines (Rust); otherwise an
	// unterminated one ends at its line, which bounds the damage of an
	// apostrophe in JSX text.
	multilineQuotes bool
	backtick        bool // JS template literals, with ${ } nesting
	rustRaw         bool // r"..", r#".."#, br#".."#
	rustChar        bool // 'x' is a char literal, 'a is a lifetime
	luaLong         bool // [[..]] strings, --[[..]] comments
	pyTriple        bool // '''..''' and """..""" strings
	pyPrefix        bool // r"..", b"..", f".." prefixes
	newlines        bool // emit tkNewline
	dollarIdent     bool
	jsRegex         bool // skip regular-expression literals
	backslashNL     bool // a backslash before a newline continues the line
}

type lexer struct {
	src  []byte
	i    int
	line int
	cfg  lexConfig
	toks []token
}

func isIdentByte(c byte, dollar bool) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c >= 0x80:
		return true
	}
	return dollar && c == '$'
}

func lex(src []byte, cfg lexConfig) []token {
	l := &lexer{src: src, line: 1, cfg: cfg}
	l.run()
	return l.toks
}

func (l *lexer) emit(k tokKind, text string, line int) {
	l.toks = append(l.toks, token{kind: k, text: text, line: line})
}

func (l *lexer) hasPrefix(s string) bool { return bytes.HasPrefix(l.src[l.i:], []byte(s)) }

func (l *lexer) run() {
	src, cfg := l.src, l.cfg
	n := len(src)
	for l.i < n {
		c := src[l.i]
		switch c {
		case '\n':
			if cfg.newlines {
				l.emit(tkNewline, "\n", l.line)
			}
			l.line++
			l.i++
			continue
		case ' ', '\t', '\r', '\f', '\v':
			l.i++
			continue
		case '\\':
			if cfg.backslashNL {
				j := l.i + 1
				if j < n && src[j] == '\r' {
					j++
				}
				if j < n && src[j] == '\n' {
					l.line++
					l.i = j + 1
					continue
				}
			}
		}

		if l.skipComment() {
			continue
		}
		if l.lexString() {
			continue
		}
		if isIdentByte(c, cfg.dollarIdent) {
			l.lexIdent()
			continue
		}
		if c == '/' && cfg.jsRegex && l.skipRegex() {
			continue
		}
		l.emit(tkPunct, string(c), l.line)
		l.i++
	}
}

// skipComment consumes a comment at the cursor, reporting whether there was one.
func (l *lexer) skipComment() bool {
	cfg := l.cfg
	if cfg.luaLong && l.hasPrefix("--") {
		if level, ok := longOpen(l.src, l.i+2); ok {
			l.i = l.skipLong(l.i+2, level)
			return true
		}
	}
	for _, lc := range cfg.lineComment {
		if l.hasPrefix(lc) {
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
			return true
		}
	}
	if cfg.blockOpen != "" && l.hasPrefix(cfg.blockOpen) {
		depth := 1
		l.i += len(cfg.blockOpen)
		for l.i < len(l.src) && depth > 0 {
			switch {
			case l.hasPrefix(cfg.blockClose):
				depth--
				l.i += len(cfg.blockClose)
			case cfg.nestBlock && l.hasPrefix(cfg.blockOpen):
				depth++
				l.i += len(cfg.blockOpen)
			default:
				if l.src[l.i] == '\n' {
					l.line++
				}
				l.i++
			}
		}
		return true
	}
	return false
}

// longOpen reports whether src[i:] opens a Lua long bracket, `[`, `=`*, `[`,
// and its level.
func longOpen(src []byte, i int) (level int, ok bool) {
	if i >= len(src) || src[i] != '[' {
		return 0, false
	}
	j := i + 1
	for j < len(src) && src[j] == '=' {
		j++
	}
	if j < len(src) && src[j] == '[' {
		return j - i - 1, true
	}
	return 0, false
}

// skipLong returns the index after the long bracket that opens at i, counting
// lines.
func (l *lexer) skipLong(i, level int) int {
	closer := "]" + string(bytes.Repeat([]byte("="), level)) + "]"
	start := i + level + 2
	end := bytes.Index(l.src[start:], []byte(closer))
	if end < 0 {
		l.line += bytes.Count(l.src[start:], []byte("\n"))
		return len(l.src)
	}
	l.line += bytes.Count(l.src[start:start+end], []byte("\n"))
	return start + end + len(closer)
}

func (l *lexer) lexIdent() {
	src, cfg := l.src, l.cfg
	start := l.i
	j := start
	for j < len(src) && isIdentByte(src[j], cfg.dollarIdent) {
		j++
	}
	word := string(src[start:j])
	next := byte(0)
	if j < len(src) {
		next = src[j]
	}

	if cfg.pyPrefix && len(word) <= 2 && (next == '"' || next == '\'') && allIn(word, "rRbBuUfF") {
		l.i = j
		l.lexString()
		return
	}
	if cfg.rustRaw {
		switch word {
		case "r", "br", "cr":
			k := j
			for k < len(src) && src[k] == '#' {
				k++
			}
			if k < len(src) && src[k] == '"' {
				l.lexRawString(k, k-j)
				return
			}
			if word == "r" && k == j+1 && k < len(src) && isIdentByte(src[k], false) {
				// r#ident: a raw identifier, not a keyword.
				e := k
				for e < len(src) && isIdentByte(src[e], false) {
					e++
				}
				l.emit(tkIdent, string(src[start:e]), l.line)
				l.i = e
				return
			}
		case "b", "c":
			if next == '"' {
				l.i = j
				l.lexString()
				return
			}
		}
	}
	l.emit(tkIdent, word, l.line)
	l.i = j
}

func allIn(s, set string) bool {
	for i := 0; i < len(s); i++ {
		if bytes.IndexByte([]byte(set), s[i]) < 0 {
			return false
		}
	}
	return true
}

// lexRawString lexes r#"…"# whose opening quote is at q with hashes hashes.
func (l *lexer) lexRawString(q, hashes int) {
	line := l.line
	closer := "\"" + string(bytes.Repeat([]byte("#"), hashes))
	start := q + 1
	end := bytes.Index(l.src[start:], []byte(closer))
	if end < 0 {
		end = len(l.src) - start
		closer = ""
	}
	text := string(l.src[start : start+end])
	l.line += bytes.Count(l.src[start:start+end], []byte("\n"))
	l.i = start + end + len(closer)
	l.emit(tkString, text, line)
}

// lexString consumes a string literal at the cursor, reporting whether there
// was one.
func (l *lexer) lexString() bool {
	src, cfg := l.src, l.cfg
	if l.i >= len(src) {
		return false
	}
	c := src[l.i]
	line := l.line

	if cfg.luaLong && c == '[' {
		if level, ok := longOpen(src, l.i); ok {
			start := l.i + level + 2
			end := l.skipLong(l.i, level)
			closeLen := level + 2
			stop := end - closeLen
			if stop < start {
				stop = start
			}
			if stop > len(src) {
				stop = len(src)
			}
			l.emit(tkString, string(src[start:stop]), line)
			l.i = end
			return true
		}
	}
	if cfg.backtick && c == '`' {
		l.i = l.skipTemplate(l.i + 1)
		l.emit(tkString, "", line)
		return true
	}
	if cfg.rustChar && c == '\'' {
		l.lexRustQuote()
		return true
	}
	if bytes.IndexByte([]byte(cfg.quotes), c) < 0 {
		return false
	}

	if cfg.pyTriple && l.hasPrefix(string([]byte{c, c, c})) {
		trip := string([]byte{c, c, c})
		start := l.i + 3
		j := start
		for j < len(src) {
			if src[j] == '\\' && j+1 < len(src) {
				if src[j+1] == '\n' {
					l.line++
				}
				j += 2
				continue
			}
			if bytes.HasPrefix(src[j:], []byte(trip)) {
				break
			}
			if src[j] == '\n' {
				l.line++
			}
			j++
		}
		l.emit(tkString, string(src[start:min(j, len(src))]), line)
		l.i = min(j+3, len(src))
		return true
	}

	start := l.i + 1
	j := start
	for j < len(src) {
		ch := src[j]
		if ch == '\\' && j+1 < len(src) {
			if src[j+1] == '\n' {
				l.line++
			}
			j += 2
			continue
		}
		if ch == c {
			l.emit(tkString, string(src[start:j]), line)
			l.i = j + 1
			return true
		}
		if ch == '\n' {
			if cfg.multilineQuotes {
				l.line++
				j++
				continue
			}
			break // unterminated: it ends with its line
		}
		j++
	}
	l.emit(tkString, string(src[start:min(j, len(src))]), line)
	l.i = min(j, len(src))
	return true
}

// lexRustQuote handles `'` in Rust: a char literal is skipped, a lifetime is a
// punctuation token.
func (l *lexer) lexRustQuote() {
	src := l.src
	i := l.i
	switch {
	case i+1 < len(src) && src[i+1] == '\\':
		j := i + 2
		for j < len(src) && src[j] != '\'' && src[j] != '\n' {
			j++
		}
		if j < len(src) && src[j] == '\'' {
			j++
		}
		l.i = j
	case i+2 < len(src) && src[i+2] == '\'' && src[i+1] != '\'':
		l.i = i + 3
	default:
		// A multi-byte char literal: 'é'.
		if i+1 < len(src) && src[i+1] >= 0x80 {
			j := i + 1
			for j < len(src) && j < i+6 && src[j] != '\'' {
				j++
			}
			if j < len(src) && src[j] == '\'' {
				l.i = j + 1
				return
			}
		}
		l.emit(tkPunct, "'", l.line)
		l.i++
	}
}

// skipTemplate returns the index after the template literal whose contents
// start at i, following ${ } substitutions (which can hold strings and further
// templates).
func (l *lexer) skipTemplate(i int) int {
	src := l.src
	for i < len(src) {
		switch src[i] {
		case '\\':
			if i+1 < len(src) && src[i+1] == '\n' {
				l.line++
			}
			i += 2
			continue
		case '`':
			return i + 1
		case '\n':
			l.line++
		case '$':
			if i+1 < len(src) && src[i+1] == '{' {
				i = l.skipSubstitution(i + 2)
				continue
			}
		}
		i++
	}
	return len(src)
}

func (l *lexer) skipSubstitution(i int) int {
	src := l.src
	depth := 1
	for i < len(src) && depth > 0 {
		switch c := src[i]; c {
		case '{':
			depth++
		case '}':
			depth--
		case '\n':
			l.line++
		case '`':
			i = l.skipTemplate(i + 1)
			continue
		case '"', '\'':
			i++
			for i < len(src) && src[i] != c && src[i] != '\n' {
				if src[i] == '\\' {
					i++
				}
				i++
			}
		}
		i++
	}
	return i
}

var regexPrecedingWords = map[string]bool{
	"return": true, "typeof": true, "case": true, "do": true, "else": true, "in": true,
	"of": true, "void": true, "delete": true, "throw": true, "new": true, "yield": true, "await": true,
}

// skipRegex consumes a regular-expression literal when the `/` at the cursor
// can start one — after an operator or a keyword, not after a value — so that a
// quote inside it does not open a string.
func (l *lexer) skipRegex() bool {
	if n := len(l.toks); n > 0 {
		p := l.toks[n-1]
		switch p.kind {
		case tkString:
			return false
		case tkIdent:
			if !regexPrecedingWords[p.text] {
				return false
			}
		case tkPunct:
			if p.text == ")" || p.text == "]" || p.text == "}" {
				return false
			}
		}
	}
	src := l.src
	j := l.i + 1
	inClass := false
	for j < len(src) {
		switch src[j] {
		case '\\':
			j += 2
			continue
		case '\n':
			return false
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				l.i = j + 1
				return true
			}
		}
		j++
	}
	return false
}
