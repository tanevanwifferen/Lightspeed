package index

// jsExtractor covers JavaScript and TypeScript (and their JSX/TSX forms):
// static imports, side-effect imports, re-exports, `import x = require()`,
// CommonJS require() and dynamic import(). The lexer skips comments, quoted
// strings, template literals (with their ${ } substitutions) and regular
// expression literals, so an import mentioned in any of them is not an edge.
type jsExtractor struct{}

func (jsExtractor) Languages() []string {
	return []string{"javascript", "javascriptreact", "typescript", "typescriptreact"}
}

// jsScanLimit bounds how far a statement is followed looking for its `from`.
const jsScanLimit = 400

func (jsExtractor) Extract(_ string, src []byte) []ImportRef {
	toks := lex(src, lexConfig{
		lineComment: []string{"//"}, blockOpen: "/*", blockClose: "*/",
		quotes: `"'`, backtick: true, dollarIdent: true, jsRegex: true,
	})
	var refs []ImportRef
	for i, t := range toks {
		if t.kind != tkIdent {
			continue
		}
		afterDot := i > 0 && toks[i-1].punct(".")
		switch t.text {
		case "import":
			if afterDot {
				continue
			}
			if r, ok := jsImport(toks, i); ok {
				refs = append(refs, r)
			}
		case "export":
			if afterDot {
				continue
			}
			if r, ok := jsReexport(toks, i); ok {
				refs = append(refs, r)
			}
		case "require":
			if afterDot {
				continue
			}
			if i+3 < len(toks) && toks[i+1].punct("(") && toks[i+2].kind == tkString &&
				(toks[i+3].punct(")") || toks[i+3].punct(",")) {
				refs = append(refs, ImportRef{Spec: toks[i+2].text, Line: t.line, Kind: "require"})
			}
		}
	}
	return refs
}

func jsImport(toks []token, i int) (ImportRef, bool) {
	line := toks[i].line
	if i+1 >= len(toks) {
		return ImportRef{}, false
	}
	next := toks[i+1]
	switch {
	case next.kind == tkString:
		return ImportRef{Spec: next.text, Line: line, Kind: "import"}, true
	case next.punct("("):
		if i+2 < len(toks) && toks[i+2].kind == tkString && i+3 < len(toks) &&
			(toks[i+3].punct(")") || toks[i+3].punct(",")) {
			return ImportRef{Spec: toks[i+2].text, Line: line, Kind: "dynamic"}, true
		}
		return ImportRef{}, false
	case next.punct("."):
		return ImportRef{}, false // import.meta
	}
	if spec, ok := jsFrom(toks, i+1, false); ok {
		return ImportRef{Spec: spec, Line: line, Kind: "import"}, true
	}
	return ImportRef{}, false
}

func jsReexport(toks []token, i int) (ImportRef, bool) {
	j := i + 1
	if j < len(toks) && toks[j].ident("type") {
		j++
	}
	if j >= len(toks) || !(toks[j].punct("*") || toks[j].punct("{")) {
		return ImportRef{}, false
	}
	if spec, ok := jsFrom(toks, j, true); ok {
		return ImportRef{Spec: spec, Line: toks[i].line, Kind: "reexport"}, true
	}
	return ImportRef{}, false
}

// jsFrom scans from j to the `from "spec"` that ends the statement, staying
// out of braces (a name may be called `from`) and giving up at a statement end.
func jsFrom(toks []token, j int, reexport bool) (string, bool) {
	depth := 0
	for k := j; k < len(toks) && k < j+jsScanLimit; k++ {
		t := toks[k]
		switch {
		case t.punct("{"):
			depth++
		case t.punct("}"):
			depth--
		case depth == 0 && t.punct(";"):
			return "", false
		case depth == 0 && t.punct("="):
			return "", false // import x = require(...) is found by the require rule
		case depth == 0 && t.ident("from"):
			if k+1 < len(toks) && toks[k+1].kind == tkString {
				return toks[k+1].text, true
			}
		}
	}
	return "", false
}
