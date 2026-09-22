package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed blast_radius <sym|file>` — what does changing this touch?
//
// It composes three sources of evidence and says which each row comes from:
//
//	references      the language server's textDocument/references, grouped by
//	                file and enclosing symbol (the symbol you would have to
//	                re-read), from the index's outline of each file;
//	call_hierarchy  transitive incoming callers, by the same bounded walker
//	                `call_hierarchy` uses (D13: depth, node budget, cycles);
//	imports         the files (and, deeper, packages) that import the subject's
//	                file, from the import graph (D35).
//
// The summary is always complete: the counts are taken over every row before
// --limit cuts the list, and the summary says when the rows were cut. A source
// that could not answer — a server without call hierarchy, a language with no
// import extractor — is listed under `evidence` with the reason, never left out
// silently (D39).
const (
	defaultBlastDepth = 2
	defaultBlastLimit = 50
)

// The evidence names, and what an evidence source's status can be.
const (
	blastRefs    = "references"
	blastCalls   = "call_hierarchy"
	blastImports = "imports"

	blastOK            = "ok"
	blastUnavailable   = "unavailable"
	blastNotApplicable = "not_applicable"
	blastNotCovered    = "not_covered"
	blastNotAnalysed   = "not_analysed"
	blastNoNodeNote    = "the file is not a node of the import graph"
)

// blastRow is one row of the answer: one thing affected, and the evidence for it.
type blastRow struct {
	Evidence string `json:"evidence"`
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	// ID and Name identify the affected symbol: the enclosing symbol of a
	// reference, the caller of a call.
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind,omitempty"`
	// Depth is the level of a caller (1 = direct) or importer.
	Depth int `json:"depth,omitempty"`
	// Count is how many references the row groups (evidence references only).
	Count int `json:"count,omitempty"`
	// Test marks a row in a test file (a heuristic, isTestPath).
	Test   bool   `json:"test,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// blastEvidence says what one source of evidence contributed.
type blastEvidence struct {
	Source string `json:"source"`
	Status string `json:"status"`
	Rows   int    `json:"rows"`
	Note   string `json:"note,omitempty"`
}

// blastSummary counts everything found, before --limit.
type blastSummary struct {
	// References is the number of reference locations; ReferencingSymbols how
	// many enclosing symbols they group into (a reference outside any known
	// symbol counts as its own group).
	References         int `json:"references"`
	ReferencingSymbols int `json:"referencing_symbols"`
	// Callers is every caller found by the call hierarchy; CallersTransitive
	// those beyond the direct ones.
	Callers           int `json:"callers"`
	CallersTransitive int `json:"callers_transitive"`
	// Importers are the files importing the subject's file (depth 1);
	// ImportersTransitive the nodes (packages, or files) reached beyond that.
	Importers           int `json:"importers"`
	ImportersTransitive int `json:"importers_transitive"`
	// Files and Packages are the distinct files and directories across all rows.
	Files    int `json:"files"`
	Packages int `json:"packages"`
	// TestFiles is how many of those files are tests.
	TestFiles int `json:"test_files"`
	// Rows is the number of rows there are; the answer lists Count of them.
	Rows int `json:"rows"`
}

// blastBounds are the limits the walk ran under and whether any bit.
type blastBounds struct {
	CallDepth   int `json:"call_depth"`
	ImportDepth int `json:"import_depth"`
	CallNodes   int `json:"call_nodes,omitempty"`
	// CallsTruncated says the call walk stopped at the node budget, CallCycles
	// how many branches were not followed because they lead back to a symbol
	// already shown, ImportsTruncated that the import walk hit its own bound.
	CallsTruncated bool `json:"calls_truncated,omitempty"`
	// CallsAtLimit and ImportsAtLimit count the rows at the last level walked:
	// their own callers or importers were not followed, so the answer may end
	// there only because the depth did.
	CallsAtLimit     int  `json:"calls_at_limit,omitempty"`
	ImportsAtLimit   int  `json:"imports_at_limit,omitempty"`
	CallCycles       int  `json:"call_cycles,omitempty"`
	ImportsTruncated bool `json:"imports_truncated,omitempty"`
}

// blastRadiusData is the payload of `blast_radius`.
type blastRadiusData struct {
	Root string `json:"root"`
	// Kind is "symbol" or "file".
	Kind     string          `json:"kind"`
	Subject  subject         `json:"subject"`
	Summary  blastSummary    `json:"summary"`
	Evidence []blastEvidence `json:"evidence"`
	Bounds   blastBounds     `json:"bounds"`
	Rows     []blastRow      `json:"rows"`
	// Count rows are listed of Total; Truncated is set when --limit cut them.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
}

func blastRadiusCommand(e *env, c *command, args []string) int {
	var (
		depth, importDepth int
		fset               *flag.FlagSet
	)
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.IntVar(&depth, "depth", defaultBlastDepth, "levels of transitive callers to follow (1 = direct callers only)")
		fs.IntVar(&importDepth, "import-depth", 0, "levels of importing files to follow (default: the same as --depth)")
	})
	if err != nil {
		return e.flagError(err)
	}
	if depth < 1 || depth > maxCallDepth {
		return e.usagef("blast_radius: --depth must be between 1 and %d (got %d)", maxCallDepth, depth)
	}
	if importDepth == 0 {
		importDepth = depth
	}
	if importDepth < 1 || importDepth > maxCallDepth {
		return e.usagef("blast_radius: --import-depth must be between 1 and %d (got %d)", maxCallDepth, importDepth)
	}
	format, err := managementFormat(common, "blast_radius", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultBlastLimit)

	run := &blastRun{e: e, common: common, depth: depth, importDepth: importDepth}
	data, warnings, err := run.execute(sf, locArg)
	run.close()
	if err != nil {
		return e.fail(err)
	}
	data.finish(limit)
	if data.Truncated {
		warnings = append(warnings, truncationWarning("rows", data.Count, data.Total,
			"the summary counts all of them; raise --limit, or lower --depth"))
	}

	exit := ExitOK
	if data.Total == 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeBlastText(e.stdout, data, warnings, common.absolute)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// A blastRun holds what the answer is built from and what has to be closed.
type blastRun struct {
	e           *env
	common      *commonFlags
	depth       int
	importDepth int

	subj *composed
	idx  *indexRun
}

func (r *blastRun) close() {
	r.subj.Close()
	if r.idx != nil {
		r.idx.close()
	}
}

// execute gathers the evidence. A failure of the subject (no server, not ready)
// is the command's failure; a failure of one source of evidence is recorded.
func (r *blastRun) execute(sf *symbolFlags, locArg string) (*blastRadiusData, []string, error) {
	fileOnly := false
	var filePath string
	if sf.id == "" && sf.query == "" {
		// A bare path that exists is a file (or directory) subject. gopls's own
		// syntax reads it as "the whole file, offset 0", which is a position; a
		// symbol is named by a location with a line, or by --symbol or --id.
		if info, err := os.Stat(locArg); err == nil {
			fileOnly = true
			if filePath, err = filepath.Abs(locArg); err != nil {
				filePath = locArg
			}
			_ = info
		}
	}
	if fileOnly {
		return r.executeFile(filePath)
	}
	return r.executeSymbol(sf, locArg)
}

// --- a symbol ---

func (r *blastRun) executeSymbol(sf *symbolFlags, locArg string) (*blastRadiusData, []string, error) {
	cs, err := openSubject(r.e, r.common, sf, locArg)
	if err != nil {
		return nil, nil, err
	}
	r.subj = cs
	data := &blastRadiusData{Root: cs.Root, Kind: "symbol", Subject: cs.Subject,
		Bounds: blastBounds{CallDepth: r.depth, ImportDepth: r.importDepth}}
	warnings := append([]string(nil), cs.Warnings...)

	// A server that is still indexing answers "no references" with authority it
	// does not have: the gate turns that into exit 5, and it applies here first.
	locs, w, err := cs.References(false)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, w...)

	idx, err := startIndex(r.e, r.common, cs.Root)
	if err != nil {
		return nil, nil, err
	}
	r.idx = idx

	refRows, refWarn, err := r.referenceRows(cs, locs)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, refWarn...)
	data.Rows = append(data.Rows, refRows...)
	data.Evidence = append(data.Evidence, blastEvidence{Source: blastRefs, Status: blastOK, Rows: len(refRows),
		Note: fmt.Sprintf("%d reference(s) outside the declaration, in %d symbol(s)", len(locs), len(refRows))})
	data.Summary.References = len(locs)
	data.Summary.ReferencingSymbols = len(refRows)

	callRows, ev, cw, err := r.callerRows(cs, &data.Bounds)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, cw...)
	data.Rows = append(data.Rows, callRows...)
	data.Evidence = append(data.Evidence, ev)

	impRows, ev, iw, err := r.importRows(cs.Subject.File, &data.Bounds)
	if err != nil {
		return nil, nil, err
	}
	warnings = append(warnings, iw...)
	data.Rows = append(data.Rows, impRows...)
	data.Evidence = append(data.Evidence, ev)
	return data, warnings, nil
}

// referenceRows groups the reference locations by the symbol that contains each,
// so a caller reads "these 4 symbols" and not 30 lines.
func (r *blastRun) referenceRows(cs *composed, locs []protocol.Location) ([]blastRow, []string, error) {
	type where struct {
		file string
		line int
	}
	var (
		places   []where
		files    = map[string]bool{}
		outside  int
		warnings []string
	)
	for _, loc := range locs {
		l, ok := cs.Locus(loc)
		if !ok {
			outside++
			continue
		}
		places = append(places, where{l.File, l.Line})
		files[l.File] = true
	}
	if outside > 0 {
		warnings = append(warnings, fmt.Sprintf("%d reference(s) are in files outside the workspace or unreadable, and are not listed", outside))
	}

	bySym := map[string]map[int]*index.Symbol{}
	if len(files) > 0 {
		list := make([]string, 0, len(files))
		for f := range files {
			list = append(list, f)
		}
		sort.Strings(list)
		out, err := r.idx.Symbols(index.SymbolsQuery{Files: list})
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, syncWarnings(out.Report)...)
		for i := range out.Files {
			fs := &out.Files[i]
			m := map[int]*index.Symbol{}
			for _, p := range places {
				if p.file == fs.File {
					m[p.line] = enclosingIndexSymbol(fs.Symbols, p.line)
				}
			}
			bySym[fs.File] = m
		}
	}

	type groupKey struct{ file, id string }
	groups := map[groupKey]*blastRow{}
	var order []groupKey
	for _, p := range places {
		sym := bySym[p.file][p.line]
		key := groupKey{file: p.file}
		row := blastRow{Evidence: blastRefs, File: p.file, Line: p.line, Test: isTestPath(p.file)}
		if sym != nil {
			key.id = sym.ID
			row.ID, row.Name, row.Kind, row.Line = sym.ID, sym.Qualified, sym.Kind, p.line
		} else {
			// Not inside any declaration the outline knows: the file's top level
			// (an import, a package-level initialiser) or a language with no
			// outline. Grouped per file.
			key.id = ""
			row.Detail = "outside any declaration in the outline"
		}
		if g, ok := groups[key]; ok {
			g.Count++
			if p.line < g.Line {
				g.Line = p.line
			}
			continue
		}
		row.Count = 1
		groups[key] = &row
		order = append(order, key)
	}
	rows := make([]blastRow, 0, len(order))
	for _, k := range order {
		rows = append(rows, *groups[k])
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})
	return rows, warnings, nil
}

// callerRows walks incoming calls with the call_hierarchy walker and its bounds.
func (r *blastRun) callerRows(cs *composed, bounds *blastBounds) ([]blastRow, blastEvidence, []string, error) {
	ev := blastEvidence{Source: blastCalls}
	s := cs.Q.session
	if !s.lsp.Supports(methodPrepareCallHierarchy) {
		ev.Status = blastUnavailable
		ev.Note = fmt.Sprintf("server %s does not advertise call hierarchy; transitive callers are not known (the references above are the direct uses)", s.lsp.ServerName())
		return nil, ev, []string{"call_hierarchy unavailable: " + ev.Note}, nil
	}
	ctx, cancel := cs.Q.queryContext()
	res, err := s.query(ctx, methodPrepareCallHierarchy, textDocumentPosition(cs.Q.doc.URI, cs.Q.position))
	cancel()
	if err != nil {
		// A JSON-RPC error answers the question: gopls says "Gate is not a
		// function" for a type, constant or field rather than an empty list. It
		// costs the callers, never the references and importers already found.
		// Anything else (not ready, timeout, crash) is not an answer and fails.
		ce, ok := errCodedAs(err, render.CodeServerError)
		if !ok {
			return nil, ev, nil, err
		}
		ev.Status = blastNotApplicable
		ev.Note = "the server has no call hierarchy for this symbol (" + oneLine(ce.Message) + "); a type, constant or field has no callers, its uses are the references"
		return nil, ev, []string{"call_hierarchy not applicable: " + oneLine(ce.Message)}, nil
	}
	warnings := append([]string(nil), res.Warnings...)
	items, err := decodeCallHierarchyItems(res.Result)
	if err != nil {
		return nil, ev, nil, err
	}
	if len(items) == 0 {
		ev.Status = blastNotApplicable
		ev.Note = "the server reports no callable symbol here (a type, constant or field has no callers; its uses are the references)"
		return nil, ev, warnings, nil
	}
	if len(items) > 1 {
		warnings = append(warnings, fmt.Sprintf("the server reports %d callable symbols at this position (%s); callers are of %q only",
			len(items), strings.Join(itemNames(items), ", "), items[0].Name))
	}

	walker := &callWalker{q: cs.Q, depth: r.depth, ids: newIDIndex(r.e, s, cs.Root)}
	rs := render.ResultSet{Kind: "call_hierarchy"}
	if err := walker.walk(&rs, items[0], methodIncomingCalls, 1); err != nil {
		return nil, ev, nil, err
	}
	bounds.CallNodes = walker.nodes
	bounds.CallsTruncated = walker.truncated
	bounds.CallCycles = walker.cycles
	if walker.truncated {
		warnings = append(warnings, fmt.Sprintf("the call graph was cut off at %d callers; lower --depth for the complete near end", maxCallNodes))
	}
	if walker.cycles > 0 {
		warnings = append(warnings, fmt.Sprintf("%d caller branch(es) were not followed because they lead back to a symbol already shown", walker.cycles))
	}
	warnings = append(warnings, walker.warnings...)
	warnings = append(warnings, walker.ids.summary()...)

	var rows []blastRow
	outside := 0
	for _, res := range rs.Results {
		rel, in := relToRoot(cs.Root, res.Span.Path)
		if !in {
			outside++
			continue
		}
		rows = append(rows, blastRow{
			Evidence: blastCalls, File: rel, Line: res.Span.Start.Line,
			ID: res.ID, Name: strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(res.Label), "<-> ")), Kind: res.Kind,
			Depth: blastLabelDepth(res.Label), Test: isTestPath(rel), Detail: res.Detail,
		})
	}
	if outside > 0 {
		warnings = append(warnings, fmt.Sprintf("%d caller(s) are outside the workspace and are not listed", outside))
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Depth != rows[j].Depth {
			return rows[i].Depth < rows[j].Depth
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})
	for _, row := range rows {
		if row.Depth == r.depth {
			bounds.CallsAtLimit++
		}
	}
	if bounds.CallsAtLimit > 0 {
		warnings = append(warnings, fmt.Sprintf("%d caller(s) are at the depth limit of %d: their own callers were not followed (raise --depth)", bounds.CallsAtLimit, r.depth))
	}
	ev.Status, ev.Rows = blastOK, len(rows)
	ev.Note = fmt.Sprintf("incoming calls to depth %d (at most %d)", r.depth, maxCallNodes)
	return rows, ev, warnings, nil
}

// blastLabelDepth reads the level out of a call-hierarchy row's label: two spaces of
// indentation per level below the first (callLabel).
func blastLabelDepth(label string) int {
	n := len(label) - len(strings.TrimLeft(label, " "))
	return n/2 + 1
}

// importRows finds who imports the subject's file, then who imports them.
func (r *blastRun) importRows(rel string, bounds *blastBounds) ([]blastRow, blastEvidence, []string, error) {
	ev := blastEvidence{Source: blastImports}
	var out index.ImportersOutcome
	if err := r.idx.do(daemon.IndexRequest{Op: daemon.IndexOpImporters, Target: rel}, &out); err != nil {
		if render.CodeForError(err) == render.CodeNoSuchFile {
			ev.Status = blastNotCovered
			ev.Note = blastNoNodeNote + "; no import extractor reads this language, so importers are unknown, not none"
			return nil, ev, []string{"imports: " + ev.Note}, nil
		}
		return nil, ev, nil, err
	}
	warnings := syncWarnings(out.Report)
	if !out.Covered {
		ev.Status = blastNotCovered
		ev.Note = "no import extractor covers this file's language, so its importers are unknown, not none"
		return nil, ev, append(warnings, "imports: "+ev.Note), nil
	}
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "no import extractor covers "+strings.Join(out.UncoveredLanguages, ", ")+
			"; files in those languages are not among the importers, if any import this")
	}
	var rows []blastRow
	for _, im := range out.Importers {
		rows = append(rows, blastRow{Evidence: blastImports, File: im.File, Line: im.Line, Depth: 1,
			Name: im.Spec, Test: isTestPath(im.File)})
	}
	if r.importDepth >= 2 {
		var g index.GraphOutcome
		err := r.idx.do(daemon.IndexRequest{Op: daemon.IndexOpGraph, Root: rel, Depth: r.importDepth, Direction: "in"}, &g)
		if err != nil {
			return nil, ev, nil, err
		}
		bounds.ImportsTruncated = g.Truncated
		if g.Truncated {
			warnings = append(warnings, "the import walk was cut off by the graph's own bound; lower --import-depth")
		}
		for _, n := range g.Nodes {
			if n.Depth < 2 {
				continue
			}
			rows = append(rows, blastRow{Evidence: blastImports, File: n.ID, Kind: n.Kind, Depth: n.Depth,
				Test: isTestPath(n.ID), Detail: "imports it through " + fmt.Sprint(n.Depth-1) + " other import(s)"})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Depth != rows[j].Depth {
			return rows[i].Depth < rows[j].Depth
		}
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})
	for _, row := range rows {
		if row.Depth == r.importDepth {
			bounds.ImportsAtLimit++
		}
	}
	if bounds.ImportsAtLimit > 0 {
		warnings = append(warnings, fmt.Sprintf("%d importer(s) are at the depth limit of %d: what imports them was not followed (raise --import-depth)", bounds.ImportsAtLimit, r.importDepth))
	}
	ev.Status, ev.Rows = blastOK, len(rows)
	ev.Note = fmt.Sprintf("importers to depth %d; Go test files are not part of the import graph (D36), so test importers of a Go package show only as references", r.importDepth)
	return rows, ev, warnings, nil
}

// --- a file ---

func (r *blastRun) executeFile(abs string) (*blastRadiusData, []string, error) {
	info, err := os.Stat(abs)
	switch {
	case os.IsNotExist(err):
		return nil, nil, render.Errorf(render.CodeNoSuchFile, "%s: no such file", abs)
	case err != nil:
		return nil, nil, render.Errorf(render.CodeIOError, "%s: %v", abs, err)
	}
	root, err := commandWorkspace(abs)
	if err != nil {
		return nil, nil, err
	}
	rel, in := relToRoot(root, abs)
	if !in {
		return nil, nil, render.Errorf(render.CodeOutsideWorkspace, "%s is outside the workspace %s", abs, root)
	}
	idx, err := startIndex(r.e, r.common, root)
	if err != nil {
		return nil, nil, err
	}
	r.idx = idx
	kind := "file"
	if info.IsDir() {
		kind = "directory"
	}
	data := &blastRadiusData{Root: root, Kind: "file", Subject: subject{File: rel, Kind: kind},
		Bounds: blastBounds{CallDepth: r.depth, ImportDepth: r.importDepth}}
	impRows, ev, warnings, err := r.importRows(rel, &data.Bounds)
	if err != nil {
		return nil, nil, err
	}
	data.Rows = impRows
	data.Evidence = []blastEvidence{
		{Source: blastRefs, Status: blastNotAnalysed, Note: "a file has no single position to ask the server about; name a symbol of it (--id from outline) for its references and callers"},
		{Source: blastCalls, Status: blastNotAnalysed, Note: "callers are per symbol; name a symbol of this file"},
		ev,
	}
	warnings = append(warnings, "a file subject reports its importers only; references and callers of the symbols it declares were not analysed")
	return data, warnings, nil
}

// --- assembling the answer ---

// finish computes the summary over every row, then cuts the rows to limit,
// sharing the room fairly between the sources of evidence so a long list of
// references cannot hide every importer.
func (d *blastRadiusData) finish(limit int) {
	files, pkgs, tests := map[string]bool{}, map[string]bool{}, map[string]bool{}
	d.Summary.Callers, d.Summary.CallersTransitive = 0, 0
	d.Summary.Importers, d.Summary.ImportersTransitive = 0, 0
	for _, row := range d.Rows {
		switch row.Evidence {
		case blastCalls:
			d.Summary.Callers++
			if row.Depth >= 2 {
				d.Summary.CallersTransitive++
			}
		case blastImports:
			if row.Depth <= 1 {
				d.Summary.Importers++
			} else {
				d.Summary.ImportersTransitive++
			}
		}
		if row.File == "" {
			continue
		}
		if row.Kind == "package" || row.Kind == "directory" || row.Kind == "dir" {
			pkgs[row.File] = true
			continue
		}
		files[row.File] = true
		pkgs[packageOf(row.File)] = true
		if row.Test {
			tests[row.File] = true
		}
	}
	d.Summary.Files, d.Summary.Packages, d.Summary.TestFiles = len(files), len(pkgs), len(tests)
	d.Summary.Rows = len(d.Rows)
	d.Total = len(d.Rows)
	d.Limit = limit
	d.Rows = blastFairCap(d.Rows, limit)
	d.Count = len(d.Rows)
	d.Truncated = d.Count < d.Total
	if d.Rows == nil {
		d.Rows = []blastRow{}
	}
}

// blastFairCap keeps at most limit rows (0 keeps all), dividing the room equally
// among the evidence sources that have rows and giving what a small source does
// not use to the others. The order of the input is kept.
func blastFairCap(rows []blastRow, limit int) []blastRow {
	if limit <= 0 || len(rows) <= limit {
		return rows
	}
	bySource := map[string][]int{}
	var sources []string
	for i, r := range rows {
		if _, ok := bySource[r.Evidence]; !ok {
			sources = append(sources, r.Evidence)
		}
		bySource[r.Evidence] = append(bySource[r.Evidence], i)
	}
	take := map[string]int{}
	room := limit
	pending := append([]string(nil), sources...)
	for room > 0 && len(pending) > 0 {
		share := room / len(pending)
		if share == 0 {
			share = 1
		}
		var next []string
		for _, s := range pending {
			if room == 0 {
				break
			}
			want := len(bySource[s]) - take[s]
			n := min(share, want, room)
			take[s] += n
			room -= n
			if take[s] < len(bySource[s]) {
				next = append(next, s)
			}
		}
		pending = next
	}
	keep := map[int]bool{}
	for s, idxs := range bySource {
		for _, i := range idxs[:take[s]] {
			keep[i] = true
		}
	}
	out := make([]blastRow, 0, limit)
	for i, r := range rows {
		if keep[i] {
			out = append(out, r)
		}
	}
	return out
}

// --- text ---

func writeBlastText(w io.Writer, d *blastRadiusData, warnings []string, absolute bool) {
	s := d.Summary
	name := d.Subject.Name
	if name == "" {
		name = d.Subject.File
	}
	fmt.Fprintf(w, "blast_radius %s: %s\n", d.Kind, name)
	fmt.Fprintf(w, "summary: %d references in %d symbols; %d callers (%d beyond direct); %d importers (+%d transitive); %d files, %d packages, %d test files\n",
		s.References, s.ReferencingSymbols, s.Callers, s.CallersTransitive, s.Importers, s.ImportersTransitive, s.Files, s.Packages, s.TestFiles)
	for _, ev := range d.Evidence {
		fmt.Fprintf(w, "evidence: %s %s", ev.Source, ev.Status)
		if ev.Note != "" {
			fmt.Fprintf(w, " (%s)", ev.Note)
		}
		fmt.Fprintln(w)
	}
	for _, row := range d.Rows {
		file := row.File
		if absolute {
			file = filepath.Join(d.Root, file)
		}
		loc := file
		if row.Line > 0 {
			loc = fmt.Sprintf("%s:%d", file, row.Line)
		}
		line := fmt.Sprintf("%s: %s", loc, row.Evidence)
		if row.Depth > 0 {
			line += fmt.Sprintf(" d%d", row.Depth)
		}
		if row.Kind != "" {
			line += " " + row.Kind
		}
		if row.Name != "" {
			line += " " + row.Name
		}
		if row.Count > 1 {
			line += fmt.Sprintf(" x%d", row.Count)
		}
		if row.Test {
			line += " [test]"
		}
		if row.ID != "" {
			line += "  " + row.ID
		}
		fmt.Fprintln(w, line)
	}
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}
