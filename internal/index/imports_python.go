package index

import "strings"

// pythonExtractor finds `import a.b as c, d` and `from a.b import c, d`
// statements, including relative ones (`from ..pkg import y`), parenthesised
// multi-line name lists and imports inside functions. Comments, strings and
// docstrings (single or triple quoted, with r/b/u/f prefixes) are skipped by
// the lexer, and a statement is only recognised at the start of a logical line,
// so `x = "import os"` and `# import os` are not imports.
type pythonExtractor struct{}

func (pythonExtractor) Languages() []string { return []string{"python"} }

func (pythonExtractor) Extract(_ string, src []byte) []ImportRef {
	toks := lex(src, lexConfig{
		lineComment: []string{"#"}, quotes: `"'`, pyTriple: true, pyPrefix: true,
		newlines: true, backslashNL: true,
	})
	var refs []ImportRef
	depth := 0
	atStart := true
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		switch {
		case t.kind == tkNewline:
			if depth == 0 {
				atStart = true
			}
			continue
		case t.kind == tkPunct:
			switch t.text {
			case "(", "[", "{":
				depth++
			case ")", "]", "}":
				if depth > 0 {
					depth--
				}
			case ";":
				if depth == 0 {
					atStart = true
					continue
				}
			}
			atStart = false
			continue
		}
		if !atStart {
			continue
		}
		atStart = false
		switch {
		case t.ident("import"):
			i = pyImport(toks, i, &refs)
		case t.ident("from"):
			i = pyFrom(toks, i, &refs)
		}
	}
	return refs
}

// pyDotted reads `a.b.c` starting at i, returning the name and the next index.
func pyDotted(toks []token, i int) (string, int) {
	var parts []string
	for i < len(toks) && toks[i].kind == tkIdent && !(len(parts) == 0 && toks[i].text == "import") {
		parts = append(parts, toks[i].text)
		i++
		if i+1 < len(toks) && toks[i].punct(".") && toks[i+1].kind == tkIdent {
			i++
			continue
		}
		break
	}
	return strings.Join(parts, "."), i
}

// pyImport parses `import a.b as c, d` and returns the index of its last token.
func pyImport(toks []token, i int, refs *[]ImportRef) int {
	line := toks[i].line
	i++
	for i < len(toks) {
		name, next := pyDotted(toks, i)
		if name == "" {
			break
		}
		*refs = append(*refs, ImportRef{Spec: name, Line: line, Kind: "import"})
		i = next
		if i+1 < len(toks) && toks[i].ident("as") && toks[i+1].kind == tkIdent {
			i += 2
		}
		if i < len(toks) && toks[i].punct(",") {
			i++
			continue
		}
		break
	}
	return i - 1
}

// pyFrom parses `from .x import (a, b as c)` and returns the index of its last
// token.
func pyFrom(toks []token, i int, refs *[]ImportRef) int {
	line := toks[i].line
	i++
	dots := ""
	for i < len(toks) && toks[i].punct(".") {
		dots += "."
		i++
	}
	mod, next := pyDotted(toks, i)
	i = next
	if i >= len(toks) || !toks[i].ident("import") {
		return i - 1
	}
	i++
	paren := false
	if i < len(toks) && toks[i].punct("(") {
		paren = true
		i++
	}
	var names []string
	for i < len(toks) {
		t := toks[i]
		if t.kind == tkNewline {
			if paren {
				i++
				continue
			}
			break
		}
		if t.punct(")") {
			i++
			break
		}
		if t.punct(";") && !paren {
			break
		}
		if t.kind == tkIdent && !t.ident("as") {
			// `a as b`: a is the imported name, b the local one.
			names = append(names, t.text)
			if i+2 < len(toks) && toks[i+1].ident("as") {
				i += 2
			}
		}
		i++
	}
	*refs = append(*refs, ImportRef{Spec: dots + mod, Names: names, Line: line, Kind: "import"})
	return i - 1
}
