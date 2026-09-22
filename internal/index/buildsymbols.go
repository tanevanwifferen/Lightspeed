package index

import "github.com/tanevanwifferen/Lightspeed/internal/symbols"

// BuildSymbols turns a server's decoded outline of one file's content into index
// entries — ids, 1-based inclusive declaration lines, signatures — exactly as the
// index records them. It is exported for the commands that outline content the
// index does not hold (changed_symbols outlines the old version of a file from
// git) and must speak the index's coordinates and ids to compare with it.
func BuildSymbols(rel, language string, content []byte, syms []symbols.Symbol) []Symbol {
	return buildSymbols(rel, language, content, syms)
}
