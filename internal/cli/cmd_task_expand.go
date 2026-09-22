package cli

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

// Expansion of the best few symbols of `task_context`: whom they call and who
// calls them (call hierarchy, one level), what their file imports and what
// imports it (the import graph), and the symbols next to them (the outline).
// Each row says which evidence it comes from, and each direction is bounded and
// says when it was cut.

const (
	// perRelation is how many rows of one relation one symbol gets.
	perRelation = 4
	// siblingsPerSymbol is how many same-file neighbours a symbol gets.
	siblingsPerSymbol = 3
)

// The relations of a related row.
const (
	relCaller     = "caller"      // calls the symbol (call hierarchy)
	relCallee     = "callee"      // is called by the symbol (call hierarchy)
	relImports    = "imports"     // imported by the symbol's file (import graph)
	relImportedBy = "imported_by" // imports the symbol's file (import graph)
	relSibling    = "sibling"     // declared next to the symbol (outline)
)

// A taskRelated is one neighbour of a ranked symbol.
type taskRelated struct {
	// Of is the id of the ranked symbol this is a neighbour of.
	Of       string `json:"of"`
	Relation string `json:"relation"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Kind     string `json:"kind,omitempty"`
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	// Detail adds what the columns cannot: the import spec, or where the
	// import is.
	Detail string `json:"detail,omitempty"`
	// Evidence is where the row comes from: call_hierarchy, import_graph or
	// outline.
	Evidence string `json:"evidence"`
}

// expansion is everything expandTop learned.
type expansion struct {
	rows     []taskRelated
	callers  map[string]int // ranked id -> total callers, when asked
	callees  map[string]int
	warnings []string
}

// expandTop expands the first k symbols. Failures are warnings: the ranking
// stands without its neighbourhood, and the answer says what is missing.
func expandTop(e *env, common *commonFlags, run *indexRun, top []taskSymbol, k int) *expansion {
	x := &expansion{callers: map[string]int{}, callees: map[string]int{}}
	if k > len(top) {
		k = len(top)
	}
	top = top[:k]
	if k == 0 {
		return x
	}

	warn := func(format string, args ...any) { x.warnings = append(x.warnings, fmt.Sprintf(format, args...)) }
	unsupported := false

	// Files: one outline query for the files of the expanded symbols, whose
	// answer gives siblings now and ids for call-hierarchy rows later.
	fileSet := map[string]bool{}
	for _, s := range top {
		fileSet[s.File] = true
	}
	files := map[string][]index.Symbol{}

	for _, s := range top {
		// Call hierarchy, for what can be called.
		if callableKind(s.Kind) {
			rows, nIn, nOut, err := callNeighbours(e, common, run.root, s, &unsupported)
			switch {
			case err != nil:
				warn("no callers/callees for %s: %v", s.Name, err)
			default:
				x.rows = append(x.rows, rows...)
				if nIn >= 0 {
					x.callers[s.ID] = nIn
				}
				if nOut >= 0 {
					x.callees[s.ID] = nOut
				}
			}
		}
	}
	if unsupported {
		warn("the language server does not offer call hierarchy, so callers and callees are not part of this answer")
	}
	for _, r := range x.rows {
		if r.File != "" {
			fileSet[r.File] = true
		}
	}

	var fs []string
	for f := range fileSet {
		fs = append(fs, f)
	}
	slices.Sort(fs)
	var sym index.SymbolsOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSymbols, Symbols: &index.SymbolsQuery{Files: fs}}, &sym); err != nil {
		warn("no outline of the involved files: %v", err)
	} else {
		for _, f := range sym.Files {
			files[f.File] = f.Symbols
		}
	}
	// Ids for the call rows, from the outline: the innermost symbol at the
	// row's line, only if it is the one the row names.
	for i := range x.rows {
		r := &x.rows[i]
		if r.Evidence != "call_hierarchy" {
			continue
		}
		if s := enclosingIndexSymbol(files[r.File], r.Line); s != nil && (s.Name == r.Name || strings.HasSuffix(s.Qualified, r.Name) || strings.HasSuffix(s.Name, r.Name)) {
			r.ID = s.ID
		}
	}

	seenFile := map[string]bool{}
	for _, s := range top {
		x.siblings(s, files[s.File])
		if seenFile[s.File] {
			continue
		}
		seenFile[s.File] = true
		x.imports(run, s, warn)
	}
	return x
}

// callableKind says whether a symbol kind can have callers.
func callableKind(kind string) bool {
	switch strings.ToLower(kind) {
	case "function", "method", "constructor":
		return true
	}
	return false
}

// callNeighbours asks the symbol's server for its direct callers and callees.
// nIn and nOut are the totals (-1 when the direction was not asked); the rows
// are capped at perRelation each.
func callNeighbours(e *env, common *commonFlags, root string, s taskSymbol, unsupported *bool) (rows []taskRelated, nIn, nOut int, err error) {
	nIn, nOut = -1, -1
	line, col := nameColumn(root, index.Symbol{Name: s.Name, Line: s.Line, EndLine: s.EndLine}, s.File)
	loc := fmt.Sprintf("%s:%d:%d", filepath.Join(root, filepath.FromSlash(s.File)), line, col)
	q, cleanup, err := prepare(e, common, loc)
	defer cleanup()
	if err != nil {
		return nil, -1, -1, err
	}
	if !q.session.lsp.Supports(methodPrepareCallHierarchy) {
		*unsupported = true
		return nil, -1, -1, nil
	}
	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, methodPrepareCallHierarchy, textDocumentPosition(q.doc.URI, q.position))
	if err != nil {
		return nil, -1, -1, err
	}
	items, err := decodeCallHierarchyItems(res.Result)
	if err != nil {
		return nil, -1, -1, err
	}
	if len(items) == 0 {
		return nil, -1, -1, fmt.Errorf("the server reports no callable symbol at %s:%d", s.File, line)
	}
	for _, dir := range []struct {
		method, relation string
		total            *int
	}{{methodIncomingCalls, relCaller, &nIn}, {methodOutgoingCalls, relCallee, &nOut}} {
		res, err := q.session.query(ctx, dir.method, map[string]any{"item": items[0].Raw})
		if err != nil {
			return rows, nIn, nOut, err
		}
		calls, err := decodeCalls(res.Result, dir.method)
		if err != nil {
			return rows, nIn, nOut, err
		}
		n, kept := 0, 0
		for _, call := range calls {
			rel, in := relToRoot(root, call.item.URI.Path())
			if !in || (rel == s.File && call.item.Name == items[0].Name) {
				continue // outside the workspace, or the symbol calling itself
			}
			n++
			if kept >= perRelation {
				continue
			}
			kept++
			rows = append(rows, taskRelated{
				Of: s.ID, Relation: dir.relation, Name: call.item.Name, Kind: symbolKindName(call.item.Kind),
				File: rel, Line: int(call.item.SelectionRange.Start.Line) + 1, Evidence: "call_hierarchy",
			})
		}
		*dir.total = n
	}
	return rows, nIn, nOut, nil
}

// siblings adds the symbols declared nearest the ranked one in its file.
func (x *expansion) siblings(s taskSymbol, syms []index.Symbol) {
	type near struct {
		sym  index.Symbol
		dist int
	}
	var ns []near
	for _, o := range syms {
		if o.ID == s.ID || o.Kind == "field" || (o.Line >= s.Line && o.EndLine <= s.EndLine) || (o.Line <= s.Line && o.EndLine >= s.EndLine) {
			continue
		}
		d := o.Line - s.EndLine
		if o.Line < s.Line {
			d = s.Line - o.EndLine
		}
		ns = append(ns, near{o, d})
	}
	slices.SortStableFunc(ns, func(a, b near) int { return a.dist - b.dist })
	for i := 0; i < len(ns) && i < siblingsPerSymbol; i++ {
		o := ns[i].sym
		x.rows = append(x.rows, taskRelated{Of: s.ID, Relation: relSibling, ID: o.ID, Name: o.Qualified, Kind: o.Kind, File: s.File, Line: o.Line, Evidence: "outline"})
	}
}

// imports adds what the symbol's file imports inside the workspace and what
// imports the file.
func (x *expansion) imports(run *indexRun, s taskSymbol, warn func(string, ...any)) {
	var im index.ImportsOutcome
	switch err := run.do(daemon.IndexRequest{Op: daemon.IndexOpImports, Path: s.File}, &im); {
	case err != nil:
		warn("no imports of %s: %v", s.File, err)
	case !im.Covered:
		warn("no import extractor reads %s, so its imports and importers are not part of this answer", s.File)
		return
	default:
		n := 0
		for _, imp := range im.Imports {
			if imp.External || len(imp.Targets) == 0 {
				continue
			}
			for _, t := range imp.Targets {
				if n >= perRelation {
					break
				}
				n++
				x.rows = append(x.rows, taskRelated{Of: s.ID, Relation: relImports, Name: imp.Spec, File: t,
					Detail: fmt.Sprintf("imported at %s:%d", s.File, imp.Line), Evidence: "import_graph"})
			}
		}
	}
	var ib index.ImportersOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpImporters, Target: s.File}, &ib); err != nil {
		warn("no importers of %s: %v", s.File, err)
		return
	}
	// Source files before tests: an importer that is a test says little about
	// what the code is used for.
	imps := slices.Clone(ib.Importers)
	slices.SortStableFunc(imps, func(a, b index.Importer) int {
		return boolInt(isTestPath(a.File)) - boolInt(isTestPath(b.File))
	})
	for i := 0; i < len(imps) && i < perRelation; i++ {
		detail := imps[i].Spec
		if isTestPath(imps[i].File) {
			detail += " (test)"
		}
		x.rows = append(x.rows, taskRelated{Of: s.ID, Relation: relImportedBy, Name: imps[i].File, File: imps[i].File,
			Line: imps[i].Line, Detail: detail, Evidence: "import_graph"})
	}
	if len(imps) > perRelation {
		warn("%s has %d importers; %d are listed", s.File, len(imps), perRelation)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
