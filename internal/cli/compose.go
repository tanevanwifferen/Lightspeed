package cli

import (
	"flag"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// This file is the kit the composed analysis commands are built from
// (type_hierarchy, blast_radius, check_references, rename_check, delete_check,
// dead_code, changed_symbols, churn, hotspots, related, task_context; docs/
// DECISIONS.md D38–D43). Each of them answers one question by combining
// evidence that already exists — language-server queries (references,
// implementation, call and type hierarchy), the workspace index and its import
// graph, `search_text`, git — so the kit is only the parts they share: naming
// the symbol the question is about, mapping a line to its enclosing symbol,
// enumerating symbols through the index, bounding a list, and telling a test
// file from a source file.

// A subject is the symbol a composed command is about, identified.
type subject struct {
	// ID is the stable symbol id when the file's outline could name the
	// symbol, "" otherwise (a location that is not on a declaration).
	ID string `json:"id,omitempty"`
	// Name is the qualified name, Kind the symbol kind.
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
	// File is the workspace-relative path, Line the 1-based line of the name.
	File string `json:"file"`
	Line int    `json:"line"`
	// EndLine is the last line of the declaration, when the outline says.
	EndLine int `json:"end_line,omitempty"`
	// Location is the absolute `file:line:col` the LSP queries were made at. It
	// is not part of the output: paths in output are workspace-relative (D30).
	Location string `json:"-"`
}

// A composed is the open state of a command that is about one symbol: the
// session and document the queries go through, and the identified subject. The
// caller closes it.
type composed struct {
	E       *env
	Common  *commonFlags
	Root    string
	Q       *locationQuery
	Subject subject
	// Warnings are what resolving the subject earned (which symbol an ambiguous
	// name chose, an id that resolved by candidate, …); pass them on.
	Warnings []string
	cleanup  func()
}

// Close releases the session.
func (c *composed) Close() {
	if c != nil && c.cleanup != nil {
		c.cleanup()
	}
}

// openSubject resolves how the caller named the position — a location argument,
// --symbol or --id, exactly as every location command does — opens the file
// under the readiness gate and identifies the symbol there. locArg is the
// location argument as parseLocationFlags returned it ("" when --symbol or --id
// named it).
func openSubject(e *env, common *commonFlags, sf *symbolFlags, locArg string) (*composed, error) {
	where, warnings, err := e.location(common, sf, locArg)
	if err != nil {
		return nil, err
	}
	q, cleanup, err := prepare(e, common, where)
	if err != nil {
		cleanup()
		return nil, err
	}
	c := &composed{E: e, Common: common, Root: q.session.match.Root, Q: q, Warnings: warnings, cleanup: cleanup}
	c.Subject, c.Warnings = c.identify(where, c.Warnings)
	return c, nil
}

// identify names the symbol at the query's position from the file's outline: the
// declaration whose name is at the position, else the innermost declaration
// that contains it. A position on no declaration is still a subject — a file
// and a line — without an id.
func (c *composed) identify(where string, warnings []string) (subject, []string) {
	doc := c.Q.doc
	rel, _ := relToRoot(c.Root, doc.URI.Path())
	sub := subject{File: rel, Line: int(c.Q.position.Line) + 1, Location: where}
	ff, err := c.Q.session.loadFile(c.Root, doc.URI.Path())
	if err != nil {
		return sub, append(warnings, fmt.Sprintf("could not outline %s to name the symbol at %s: %v", rel, where, err))
	}
	// A declaration whose name is at the position is the symbol, whatever
	// contains it; otherwise the innermost declaration that contains it.
	best := -1
	for i, s := range ff.syms {
		if s.HasRange && containsPos(s.Range, c.Q.position) {
			best = i
			break
		}
	}
	if best < 0 {
		best = enclosingSymbol(ff.syms, c.Q.position)
	}
	if best >= 0 {
		s := ff.syms[best]
		sub.Name, sub.Kind = s.Qualified, s.Kind
		if best < len(ff.ids) {
			sub.ID = ff.ids[best]
		}
		if s.HasRange {
			sub.Line = int(s.Range.Start.Line) + 1
		}
		if s.Full != (protocol.Range{}) {
			sub.EndLine = int(s.Full.End.Line) + 1
		}
	}
	return sub, append(warnings, ff.warnings...)
}

// containsPos reports whether p is inside r (start inclusive, end inclusive, so
// a caret at the end of a name still counts).
func containsPos(r protocol.Range, p protocol.Position) bool {
	return !posBefore(p, r.Start) && !posBefore(r.End, p)
}

// A locus is where a query result points: a workspace-relative file and a line.
type locus struct {
	File string
	Line int
	// Column is the 1-based byte column, 0 when unknown.
	Column int
}

// Locus renders a protocol location as a workspace-relative file, 1-based line
// and byte column, resolving it through the session's Mapper cache. ok is false
// for a file outside the workspace or one the store cannot read; the caller
// decides whether that is a warning.
func (c *composed) Locus(loc protocol.Location) (locus, bool) {
	rel, in := relToRoot(c.Root, loc.URI.Path())
	if !in {
		return locus{}, false
	}
	m, err := c.Q.session.docs.MapperForURI(loc.URI)
	if err != nil {
		return locus{}, false
	}
	span, err := render.NewSpanFromLocation(m, loc)
	if err != nil {
		return locus{}, false
	}
	return locus{File: rel, Line: span.Start.Line, Column: span.Start.Column}, true
}

// References asks the server for every reference to the subject, under the
// readiness gate (exit 5 rather than an empty list from a server that is still
// indexing). includeDeclaration is passed through.
func (c *composed) References(includeDeclaration bool) ([]protocol.Location, []string, error) {
	params := textDocumentPosition(c.Q.doc.URI, c.Q.position)
	params["context"] = map[string]any{"includeDeclaration": includeDeclaration}
	ctx, cancel := c.Q.queryContext()
	defer cancel()
	res, err := c.Q.session.query(ctx, methodReferences, params)
	if err != nil {
		return nil, nil, err
	}
	locs, err := decodeLocations(res.Result)
	return locs, res.Warnings, err
}

// --- the index ---

// Symbols lists the symbols of the files the query names, revalidated against
// the disk (the index's guarantee, D32), through the workspace's daemon or the
// in-process service. It is unranked: for ranked search use the search op.
func (r *indexRun) Symbols(q index.SymbolsQuery) (*index.SymbolsOutcome, error) {
	var out index.SymbolsOutcome
	if err := r.do(daemon.IndexRequest{Op: daemon.IndexOpSymbols, Symbols: &q}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// enclosingIndexSymbol is the innermost symbol of a file whose lines contain line
// (1-based), or nil. syms is one file's symbols in document order.
func enclosingIndexSymbol(syms []index.Symbol, line int) *index.Symbol {
	var best *index.Symbol
	for i := range syms {
		s := &syms[i]
		if line < s.Line || line > s.EndLine {
			continue
		}
		if best == nil || s.EndLine-s.Line <= best.EndLine-best.Line {
			best = s
		}
	}
	return best
}

// symbolByID finds a symbol of an indexed file by its id.
func symbolByID(fs *index.FileSymbols, id string) *index.Symbol {
	for i := range fs.Symbols {
		if fs.Symbols[i].ID == id {
			return &fs.Symbols[i]
		}
	}
	return nil
}

// --- bounds ---

// effectiveLimit is the row limit of a composed command: the caller's --limit
// when they gave one (0 meaning no limit), def otherwise. fset is the flag set
// the command registered its flags on (capture it in the extra func of
// parseFlagsRange), which is how "not given" is told from "given as 0".
func effectiveLimit(fset *flag.FlagSet, common *commonFlags, def int) int {
	given := false
	if fset != nil {
		fset.Visit(func(f *flag.Flag) { given = given || f.Name == "limit" })
	}
	if given {
		return common.limit
	}
	return def
}

// capRows keeps the first limit rows (0 keeps all) and says whether it cut and
// how many there were.
func capRows[T any](rows []T, limit int) (out []T, truncated bool, total int) {
	total = len(rows)
	if limit > 0 && total > limit {
		return rows[:limit], true, total
	}
	return rows, false, total
}

// truncationWarning is the one sentence every bounded list says when it bit.
func truncationWarning(what string, shown, total int, narrow string) string {
	return fmt.Sprintf("listed %d of %d %s; %s", shown, total, what, narrow)
}

// --- tests ---

// isTestPath reports whether a workspace-relative path is a test file by the
// conventions of the languages the index reads: Go `_test.go`; Python
// `test_*.py` / `*_test.py` / `conftest.py`; JS/TS `*.test.*` / `*.spec.*` and
// `__tests__/`; Rust `tests/`; Ruby `_spec.rb`; Java/Kotlin `*Test.java` and
// `*Tests.kt`; and any file under a directory named test, tests, spec or
// __tests__. It is a heuristic and is reported as one.
func isTestPath(rel string) bool {
	rel = strings.ToLower(path.Clean(strings.ReplaceAll(rel, "\\", "/")))
	base := path.Base(rel)
	stem := strings.TrimSuffix(base, path.Ext(base))
	switch {
	case strings.HasSuffix(base, "_test.go"),
		strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py"),
		strings.HasSuffix(stem, "_test"),
		base == "conftest.py",
		strings.Contains(base, ".test."), strings.Contains(base, ".spec."),
		strings.HasSuffix(stem, "_spec"),
		strings.HasSuffix(stem, "test") && (strings.HasSuffix(base, ".java") || strings.HasSuffix(base, ".kt")),
		strings.HasSuffix(stem, "tests") && (strings.HasSuffix(base, ".java") || strings.HasSuffix(base, ".kt")):
		return true
	}
	for _, dir := range strings.Split(path.Dir(rel), "/") {
		if slices.Contains([]string{"test", "tests", "spec", "specs", "__tests__"}, dir) {
			return true
		}
	}
	return false
}

// packageOf is the grouping a "distinct packages" count uses: the file's
// directory, "." at the workspace root.
func packageOf(rel string) string { return path.Dir(rel) }
