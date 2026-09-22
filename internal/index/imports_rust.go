package index

import "strings"

// rustExtractor reads `use` trees, `mod foo;` declarations and
// `extern crate`. A use tree is expanded to one ImportRef per leaf
// (`use a::{b, c::d, self}` is a::b, a::c::d and a), and a glob leaf is the
// module it globs. Comments (nested), strings and raw strings are skipped by the
// lexer, so `r#"use x;"#` is text.
type rustExtractor struct{}

func (rustExtractor) Languages() []string { return []string{"rust"} }

func (rustExtractor) Extract(_ string, src []byte) []ImportRef {
	toks := lex(src, lexConfig{
		lineComment: []string{"//"}, blockOpen: "/*", blockClose: "*/", nestBlock: true,
		quotes: `"`, multilineQuotes: true, rustRaw: true, rustChar: true,
	})
	var refs []ImportRef
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t.kind != tkIdent {
			continue
		}
		afterPath := i > 0 && (toks[i-1].punct(".") || (toks[i-1].punct(":") && i > 1 && toks[i-2].punct(":")))
		if afterPath {
			continue
		}
		switch t.text {
		case "use":
			p := &useParser{toks: toks, i: i + 1, line: t.line}
			p.tree(nil)
			refs = append(refs, p.refs...)
			i = p.i
		case "mod":
			if i+2 < len(toks) && toks[i+1].kind == tkIdent && toks[i+2].punct(";") {
				refs = append(refs, ImportRef{Spec: toks[i+1].text, Line: t.line, Kind: "mod"})
			}
		case "extern":
			if i+2 < len(toks) && toks[i+1].ident("crate") && toks[i+2].kind == tkIdent {
				refs = append(refs, ImportRef{Spec: toks[i+2].text, Line: t.line, Kind: "extern"})
			}
		}
	}
	return refs
}

type useParser struct {
	toks []token
	i    int
	line int
	refs []ImportRef
}

func (p *useParser) peek() token {
	if p.i < len(p.toks) {
		return p.toks[p.i]
	}
	return token{}
}

func (p *useParser) leaf(path []string) {
	if len(path) == 0 {
		return
	}
	p.refs = append(p.refs, ImportRef{Spec: strings.Join(path, "::"), Line: p.line, Kind: "use"})
}

// tree parses one use tree below prefix, leaving the cursor after it (on the
// `,`, `}` or `;` that ends it).
func (p *useParser) tree(prefix []string) {
	path := append([]string(nil), prefix...)
	// A leading `::` is the 2015-style crate root; it names nothing.
	for p.peek().punct(":") {
		p.i++
	}
	for p.i < len(p.toks) {
		t := p.peek()
		switch {
		case t.kind == tkIdent && t.text == "as":
			p.i++
			if p.peek().kind == tkIdent {
				p.i++
			}
		case t.kind == tkIdent:
			path = append(path, t.text)
			p.i++
		case t.punct(":"):
			p.i++
		case t.punct("*"):
			p.i++
			p.leaf(path)
			return
		case t.punct("{"):
			p.i++
			for p.i < len(p.toks) && !p.peek().punct("}") {
				if p.peek().punct(",") {
					p.i++
					continue
				}
				before := p.i
				p.tree(path)
				if p.i == before {
					p.i++
				}
			}
			if p.peek().punct("}") {
				p.i++
			}
			return
		default:
			// `,`, `}` or `;` end this tree.
			if n := len(path); n > 0 && path[n-1] == "self" && n > len(prefix) {
				path = path[:n-1]
			}
			p.leaf(path)
			return
		}
	}
	p.leaf(path)
}
