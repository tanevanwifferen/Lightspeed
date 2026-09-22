package index

import (
	"go/parser"
	gotoken "go/token"
	"strconv"
)

// goExtractor reads Go imports with go/parser in ImportsOnly mode: the real
// grammar, so comments, strings and grouped declarations are exact, and a file
// that does not fully parse still yields the imports before the error.
type goExtractor struct{}

func (goExtractor) Languages() []string { return []string{"go"} }

func (goExtractor) Extract(path string, src []byte) []ImportRef {
	fset := gotoken.NewFileSet()
	f, _ := parser.ParseFile(fset, path, src, parser.ImportsOnly)
	if f == nil {
		return nil
	}
	var refs []ImportRef
	for _, imp := range f.Imports {
		spec, err := strconv.Unquote(imp.Path.Value)
		if err != nil || spec == "" || spec == "C" {
			continue // cgo's pseudo-package is not an import of anything
		}
		refs = append(refs, ImportRef{
			Spec: spec,
			Line: fset.Position(imp.Path.Pos()).Line,
			Kind: "import",
		})
	}
	return refs
}
