package index

import "bytes"

// cExtractor finds #include and #import directives in C and C++. It blanks
// comments and raw strings first (ordinary strings and char literals are read
// so that a `//` inside one is not a comment), then scans line by line, so a
// directive inside a comment or a raw string is not an include. A quoted include
// is Kind "include", an angle include is Kind "system".
type cExtractor struct{}

func (cExtractor) Languages() []string { return []string{"c", "cpp"} }

func (cExtractor) Extract(_ string, src []byte) []ImportRef {
	code := blankCComments(src)
	var refs []ImportRef
	line := 0
	for len(code) > 0 {
		line++
		var text []byte
		if nl := bytes.IndexByte(code, '\n'); nl >= 0 {
			text, code = code[:nl], code[nl+1:]
		} else {
			text, code = code, nil
		}
		if r, ok := parseInclude(text); ok {
			r.Line = line
			refs = append(refs, r)
		}
	}
	return refs
}

func parseInclude(text []byte) (ImportRef, bool) {
	s := bytes.TrimLeft(text, " \t\r\f\v")
	if len(s) == 0 || s[0] != '#' {
		return ImportRef{}, false
	}
	s = bytes.TrimLeft(s[1:], " \t")
	var rest []byte
	switch {
	case bytes.HasPrefix(s, []byte("include_next")):
		rest = s[len("include_next"):]
	case bytes.HasPrefix(s, []byte("include")):
		rest = s[len("include"):]
	case bytes.HasPrefix(s, []byte("import")):
		rest = s[len("import"):]
	default:
		return ImportRef{}, false
	}
	if len(rest) == 0 || (rest[0] != ' ' && rest[0] != '\t' && rest[0] != '"' && rest[0] != '<') {
		return ImportRef{}, false
	}
	rest = bytes.TrimLeft(rest, " \t")
	if len(rest) < 2 {
		return ImportRef{}, false
	}
	switch rest[0] {
	case '"':
		if end := bytes.IndexByte(rest[1:], '"'); end > 0 {
			return ImportRef{Spec: string(rest[1 : 1+end]), Kind: "include"}, true
		}
	case '<':
		if end := bytes.IndexByte(rest[1:], '>'); end > 0 {
			return ImportRef{Spec: string(rest[1 : 1+end]), Kind: "system"}, true
		}
	}
	return ImportRef{}, false
}

// blankCComments returns src with comments and C++ raw-string contents replaced
// by spaces, newlines kept, so that line numbers survive.
func blankCComments(src []byte) []byte {
	out := make([]byte, len(src))
	copy(out, src)
	blank := func(from, to int) {
		for k := from; k < to && k < len(out); k++ {
			if out[k] != '\n' {
				out[k] = ' '
			}
		}
	}
	for i := 0; i < len(src); {
		c := src[i]
		switch {
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			j := i
			for j < len(src) && src[j] != '\n' {
				if src[j] == '\\' && j+1 < len(src) && src[j+1] == '\n' {
					j++ // a backslash continues a // comment onto the next line
				}
				j++
			}
			blank(i, j)
			i = j
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			j := i + 2
			for j < len(src) && !(src[j] == '*' && j+1 < len(src) && src[j+1] == '/') {
				j++
			}
			j = min(j+2, len(src))
			blank(i, j)
			i = j
		case c == 'R' && i+1 < len(src) && src[i+1] == '"' && (i == 0 || !isIdentByte(src[i-1], false) || src[i-1] == 'L' || src[i-1] == 'u' || src[i-1] == 'U' || src[i-1] == '8'):
			// R"delim( ... )delim"
			p := bytes.IndexByte(src[i+2:], '(')
			if p < 0 || p > 16 {
				i++
				continue
			}
			delim := string(src[i+2 : i+2+p])
			closer := ")" + delim + "\""
			start := i + 2 + p + 1
			end := bytes.Index(src[start:], []byte(closer))
			if end < 0 {
				blank(start, len(src))
				i = len(src)
				continue
			}
			blank(start, start+end)
			i = start + end + len(closer)
		case c == '"' || c == '\'':
			j := i + 1
			for j < len(src) && src[j] != c && src[j] != '\n' {
				if src[j] == '\\' && j+1 < len(src) {
					j++
				}
				j++
			}
			i = min(j+1, len(src))
		default:
			i++
		}
	}
	return out
}
