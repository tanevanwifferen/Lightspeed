package index

// luaExtractor finds require("mod"), require "mod" and require('mod'). The
// lexer reads Lua's comments (`--`, `--[==[ ]==]`) and strings (quotes and long
// brackets), so a require in either is not an import; `foo.require(...)` and
// `foo:require(...)` are method calls, not the global.
type luaExtractor struct{}

func (luaExtractor) Languages() []string { return []string{"lua"} }

func (luaExtractor) Extract(_ string, src []byte) []ImportRef {
	toks := lex(src, lexConfig{lineComment: []string{"--"}, quotes: `"'`, luaLong: true})
	var refs []ImportRef
	for i, t := range toks {
		if !t.ident("require") {
			continue
		}
		if i > 0 && (toks[i-1].punct(".") || toks[i-1].punct(":")) {
			continue
		}
		j := i + 1
		paren := false
		if j < len(toks) && toks[j].punct("(") {
			paren = true
			j++
		}
		if j >= len(toks) || toks[j].kind != tkString {
			continue
		}
		if paren && (j+1 >= len(toks) || !(toks[j+1].punct(")") || toks[j+1].punct(","))) {
			continue
		}
		refs = append(refs, ImportRef{Spec: toks[j].text, Line: t.line, Kind: "require"})
	}
	return refs
}
