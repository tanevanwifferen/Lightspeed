package serverdef

// The built-in default layer, PLAN §6 item 3.
//
// The table itself lives in builtins_gen.go and is generated from the
// vendored nvim-lspconfig snapshot by internal/gen; see generate.go for
// the go:generate directive and internal/gen/curate.go for what is
// taken from the corpus, what is curated, and why. This file is only
// the accessors, which are hand-written because their contract — every
// caller gets its own copy — matters more than the data.
//
// The table covers exactly the six servers PLAN §8 M4 names: gopls,
// clangd, lua-ls, pyright, rust-analyzer and vtsls. Adding a seventh is
// a curation change, not a code change.

// Builtins returns the built-in server definitions, in a stable order:
// gopls first, because it is the reference implementation lightspeed
// generalises and internal/router treats load order as preference
// order, then alphabetically.
//
// The result is freshly copied on every call, so a caller that layers
// overrides on top cannot corrupt the defaults for the next caller.
func Builtins() []*ServerDef {
	out := make([]*ServerDef, len(generatedBuiltins))
	for i, d := range generatedBuiltins {
		out[i] = d.Clone()
	}
	return out
}

// Builtin returns a copy of the built-in definition with the given
// name.
func Builtin(name string) (*ServerDef, bool) {
	for _, d := range generatedBuiltins {
		if d.Name == name {
			return d.Clone(), true
		}
	}
	return nil, false
}

// BuiltinNames lists the built-in server names in table order.
func BuiltinNames() []string {
	out := make([]string, len(generatedBuiltins))
	for i, d := range generatedBuiltins {
		out[i] = d.Name
	}
	return out
}
