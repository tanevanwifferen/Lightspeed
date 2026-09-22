package index

import (
	"path/filepath"
	"sort"

	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

func pathJoin(dir, rel string) string {
	return filepath.Join(dir, filepath.FromSlash(rel))
}

// buildSymbols turns a server's decoded outline of one file into index
// entries: the stable id, the position of the declaration, the signature as
// `outline` shows it, and the first sentence of the comment above it. The
// entries are in document order, with Parent renumbered to match.
func buildSymbols(rel, language string, content []byte, syms []symbols.Symbol) []Symbol {
	if len(syms) == 0 {
		return nil
	}
	ids := symbols.IDs(rel, syms)
	x := symbols.NewLineIndex(content)

	order := make([]int, len(syms))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return symbols.PosBefore(syms[order[a]].Range.Start, syms[order[b]].Range.Start)
	})
	newIndex := make([]int, len(syms)) // old index -> 1 + new index
	for n, old := range order {
		newIndex[old] = n + 1
	}

	out := make([]Symbol, len(syms))
	for n, old := range order {
		s := syms[old]
		first, last := int(s.Full.Start.Line), symbols.EndLine(s)
		sig := s.Signature
		if sig == "" {
			sig = x.DeclSignature(first, last)
		}
		doc := x.LeadingDoc(first)
		if doc == "" && language == "python" {
			doc = x.PythonDocstring(first, last)
		}
		container := s.Container
		parent := 0
		if s.Parent > 0 && s.Parent <= len(syms) {
			parent = newIndex[s.Parent-1]
			if container == "" {
				container = symbols.IDName(syms[s.Parent-1].Qualified)
			}
		}
		out[n] = Symbol{
			ID:        ids[old],
			Name:      plainName(s.Name),
			Qualified: symbols.IDName(s.Qualified),
			Kind:      symbols.IDKind(s.Kind),
			Container: symbols.IDName(container),
			Line:      first + 1,
			EndLine:   last + 1,
			Parent:    parent,
			Signature: sig,
			Doc:       doc,
		}
	}
	return out
}

// plainName is a symbol's own name: gopls names a method `(*T).M`, which the id
// spells `T.M` and a reader knows as `M`.
func plainName(name string) string {
	if spelled := symbols.IDName(name); spelled != name {
		return symbols.LastSegment(spelled)
	}
	return name
}
