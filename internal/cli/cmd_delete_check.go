package cli

import (
	"flag"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed delete_check <loc>` — would deleting this break anything?
//
// A read-only verdict from three sources: the references outside the symbol's
// own declaration (a self-reference inside its body is not a use), whether it is
// exported and how that was decided (an exported symbol may be used by code this
// workspace cannot see, whatever its references say), and, when it is the last
// symbol of its file or package, who imports that file or package and would be
// left importing nothing (docs/DECISIONS.md D38).
//
// Exit 0 means the evidence does not stand against deleting it; exit 1 means it
// does. That is the reverse of check_references' "nothing found" exit 1: the
// question here is whether it is safe, so the problem is a reference, not an
// absence.

// The verdicts of delete_check.
const (
	deleteInUse        = "in_use"
	deleteTestsOnly    = "used_by_tests_only"
	deleteDangling     = "leaves_importers_dangling"
	deleteUnused       = "unused"
	defaultDeleteLimit = 20
)

// deleteRefs is the references section of delete_check.
type deleteRefs struct {
	refSection
	// InsideDeclaration counts references inside the symbol's own declaration
	// (recursion, a self-reference in its body), which are not listed and do not
	// count as uses.
	InsideDeclaration int `json:"inside_declaration"`
	// NonTest is how many references are in files that do not look like tests.
	NonTest int `json:"non_test"`
}

// lastSection says whether the symbol is the last of its file or package.
type lastSection struct {
	Evidence string `json:"evidence"`
	// Checked is false when the index could not say (or the symbol is nested,
	// which is never "the last symbol of a file"); Detail says why.
	Checked            bool   `json:"checked"`
	Detail             string `json:"detail,omitempty"`
	OfFile             bool   `json:"of_file"`
	OfPackage          bool   `json:"of_package"`
	RemainingInFile    int    `json:"remaining_in_file"`
	RemainingInPackage int    `json:"remaining_in_package"`
}

// deleteCheckData is the payload of `delete_check`.
type deleteCheckData struct {
	Root    string  `json:"root"`
	Symbol  subject `json:"symbol"`
	Verdict string  `json:"verdict"`
	Rule    string  `json:"rule"`
	// Exported is exported, unexported or unknown, and ExportedBasis is the rule
	// that decided it for the language.
	Exported      string      `json:"exported"`
	ExportedBasis string      `json:"exported_basis"`
	References    deleteRefs  `json:"references"`
	Last          lastSection `json:"last_symbol"`
	// Importers is present when the symbol is the last of its file or package:
	// the files that import that file or package.
	Importers *refSection `json:"importers,omitempty"`
	// ImportersTarget is the file or directory whose importers are listed.
	ImportersTarget string   `json:"importers_target,omitempty"`
	Caveats         []string `json:"caveats,omitempty"`
	// Truncated and Total summarize the lists: Truncated is set when --limit
	// cut references or importers, Total is the rows they had before it.
	Truncated bool `json:"truncated"`
	Total     int  `json:"total"`
}

func deleteCheckCommand(e *env, c *command, args []string) int {
	var fset *flag.FlagSet
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) { fset = fs })
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "delete_check", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultDeleteLimit)

	cs, err := openSubject(e, common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}
	defer cs.Close()
	warnings := slices.Clone(cs.Warnings)

	var run *indexRun
	if r, rerr := startIndex(e, common, cs.Root); rerr == nil {
		run = r
		defer run.close()
	} else {
		warnings = append(warnings, "the index is unavailable: "+rerr.Error())
	}
	decl, w := cs.declaration(run)
	warnings = append(warnings, w...)

	data := deleteCheckData{Root: cs.Root, Symbol: cs.Subject, Exported: decl.Exported, ExportedBasis: decl.ExportBasis}
	if decl.ID != "" {
		data.Symbol = subject{ID: decl.ID, Name: decl.Name, Kind: decl.Kind, File: decl.File, Line: decl.Line, EndLine: decl.EndLine}
	}

	// 1. References outside the declaration.
	locs, rw, err := cs.References(false)
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, rw...)
	var external []refRow
	outside := 0
	for _, loc := range locs {
		row, ok := cs.rowFor(loc, evidenceReferences)
		switch {
		case !ok:
			outside++
		case decl.EndLine > 0 && row.File == decl.File && row.Line >= decl.Line && row.Line <= decl.EndLine:
			data.References.InsideDeclaration++
		default:
			external = append(external, row)
		}
	}
	if outside > 0 {
		warnings = append(warnings, fmt.Sprintf("%d reference(s) are outside the workspace or unreadable and are not counted", outside))
	}
	sortRefRows(external)
	tests := 0
	for _, r := range external {
		if r.Test {
			tests++
		}
	}
	data.References.NonTest = len(external) - tests
	shown, truncated, total := capRows(external, limit)
	data.References.refSection = refSection{Evidence: evidenceReferences, Total: total, Truncated: truncated, Limit: limit, Rows: append([]refRow{}, shown...)}
	if run != nil {
		warnings = append(warnings, attachEnclosing(run, data.References.Rows)...)
	}
	data.References.Count, data.References.Files = len(data.References.Rows), distinctRowFiles(data.References.Rows)
	for _, r := range data.References.Rows {
		if r.Test {
			data.References.InTests++
		}
	}
	if truncated {
		warnings = append(warnings, truncationWarning("references", data.References.Count, total, "raise --limit"))
	}

	// 2. Is it the last symbol of its file or package, and who imports that.
	data.Last = lastSection{Evidence: evidenceIndex}
	if run != nil {
		data.Last = lastOfScope(run, decl)
		if data.Last.OfFile || data.Last.OfPackage {
			target := decl.File
			if dirScopedLangs[decl.Language] {
				target = path.Dir(decl.File)
			}
			if (dirScopedLangs[decl.Language] && data.Last.OfPackage) || (!dirScopedLangs[decl.Language] && data.Last.OfFile) {
				sec, iw := importerRows(run, target, limit)
				warnings = append(warnings, iw...)
				data.Importers, data.ImportersTarget = sec, target
			}
		}
	} else {
		data.Last.Detail = "not checked: the workspace index could not be consulted"
	}

	// 3. The verdict, and what it cannot see.
	nonTestImporters := 0
	if data.Importers != nil {
		nonTestImporters = data.Importers.Total - data.Importers.InTests
	}
	switch {
	case data.References.NonTest > 0:
		data.Verdict = deleteInUse
		data.Rule = fmt.Sprintf("in_use: %d reference(s) outside the declaration in non-test code (%d in tests) would be left dangling", data.References.NonTest, tests)
	case tests > 0:
		data.Verdict = deleteTestsOnly
		data.Rule = fmt.Sprintf("used_by_tests_only: no reference outside the declaration in non-test code, but %d in test files", tests)
	case nonTestImporters > 0:
		data.Verdict = deleteDangling
		data.Rule = fmt.Sprintf("leaves_importers_dangling: nothing references the symbol, but it is the last symbol of %s and %d file(s) import that", data.ImportersTarget, nonTestImporters)
	default:
		data.Verdict = deleteUnused
		data.Rule = "unused: the language server reports no reference outside the symbol's own declaration"
		if data.Last.OfFile || data.Last.OfPackage {
			data.Rule += fmt.Sprintf("; it is the last symbol of its %s, and no importer of %s was found", lastScopeName(data.Last), dotIfEmpty(data.ImportersTarget))
		}
	}
	if data.References.InsideDeclaration > 0 {
		data.Caveats = append(data.Caveats, fmt.Sprintf("%d reference(s) inside the symbol's own declaration (recursion or self-reference) are not counted as uses", data.References.InsideDeclaration))
	}
	switch data.Exported {
	case "exported":
		data.Caveats = append(data.Caveats, "it is exported ("+data.ExportedBasis+"): code outside this workspace may use it, and references inside the workspace cannot show that")
	case "unknown":
		data.Caveats = append(data.Caveats, "whether it is exported is unknown ("+data.ExportedBasis+"): code outside this workspace may use it")
	}
	if data.Importers != nil && data.Importers.Total > 0 && nonTestImporters == 0 {
		data.Caveats = append(data.Caveats, "the only importers are test files")
	}

	data.Truncated, data.Total = data.References.Truncated, data.References.Total
	if data.Importers != nil {
		data.Truncated = data.Truncated || data.Importers.Truncated
		data.Total += data.Importers.Total
	}
	exit := ExitOK
	if data.Verdict != deleteUnused {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeDeleteCheckText(e.stdout, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

func lastScopeName(l lastSection) string {
	if l.OfPackage {
		return "package"
	}
	return "file"
}

func dotIfEmpty(s string) string {
	if s == "" {
		return "."
	}
	return s
}

// lastOfScope decides from the index whether the declaration is the last
// top-level symbol of its file, and of its directory. A nested symbol (a method,
// a field) is never the last symbol of a file. The symbols that go with it — a
// type's own members — do not count as remaining.
func lastOfScope(run *indexRun, decl symbolDecl) lastSection {
	sec := lastSection{Evidence: evidenceIndex}
	switch {
	case decl.ID == "":
		sec.Detail = "not checked: the declaration could not be identified in its file's outline"
		return sec
	case decl.Container != "":
		sec.Detail = "not applicable: the symbol is nested in " + decl.Container + ", so deleting it leaves its container in place"
		return sec
	}
	remaining := func(fs index.FileSymbols) int {
		n := 0
		for _, s := range fs.Symbols {
			if s.Container != "" || s.ID == decl.ID {
				continue // a member of something, or the symbol itself
			}
			n++
		}
		return n
	}
	out, err := run.Symbols(index.SymbolsQuery{Files: []string{decl.File}})
	if err != nil {
		sec.Detail = "not checked: " + err.Error()
		return sec
	}
	for _, f := range out.Files {
		if f.File == decl.File {
			sec.RemainingInFile = remaining(f)
		}
	}
	sec.Checked, sec.OfFile = true, sec.RemainingInFile == 0
	sec.RemainingInPackage = sec.RemainingInFile
	if sec.OfFile {
		dir := path.Dir(decl.File)
		q := index.SymbolsQuery{}
		if dir != "." {
			q.Path = dir
		}
		pkg, perr := run.Symbols(q)
		if perr != nil {
			sec.Detail = "the package was not checked: " + perr.Error()
			return sec
		}
		for _, f := range pkg.Files {
			if f.File != decl.File && path.Dir(f.File) == dir {
				sec.RemainingInPackage += remaining(f)
			}
		}
		sec.OfPackage = sec.RemainingInPackage == 0
	}
	return sec
}

// importerRows lists who imports target, from the index's import graph, as rows.
// A language with no extractor is said, not treated as "no importers".
func importerRows(run *indexRun, target string, limit int) (*refSection, []string) {
	sec := &refSection{Evidence: evidenceImporters, Rows: []refRow{}}
	var out index.ImportersOutcome
	req := target
	if req == "." {
		req = ""
	}
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpImporters, Target: req}, &out); err != nil {
		return sec, []string{"importers of " + dotIfEmpty(target) + " were not listed: " + err.Error()}
	}
	var warnings []string
	if !out.Covered {
		warnings = append(warnings, "no import extractor covers "+dotIfEmpty(target)+", so its importers are unknown, not none")
	}
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "files in "+strings.Join(out.UncoveredLanguages, ", ")+" are not covered by an import extractor and are not among the importers")
	}
	var rows []refRow
	for _, im := range out.Importers {
		rows = append(rows, refRow{File: im.File, Line: im.Line, Text: "imports " + im.Spec, Test: isTestPath(im.File), Evidence: evidenceImporters})
	}
	sortRefRows(rows)
	shown, truncated, total := capRows(rows, limit)
	sec.Rows = append(sec.Rows, shown...)
	sec.Total, sec.Truncated, sec.Limit = total, truncated, limit
	sec.Count, sec.Files = len(sec.Rows), distinctRowFiles(sec.Rows)
	for _, r := range rows {
		if r.Test {
			sec.InTests++
		}
	}
	if truncated {
		warnings = append(warnings, truncationWarning("importers", sec.Count, total, "raise --limit"))
	}
	return sec, warnings
}

func writeDeleteCheckText(w io.Writer, d deleteCheckData, warnings []string) {
	fmt.Fprintf(w, "# delete_check %s: %s\n", d.Symbol.Name, d.Rule)
	fmt.Fprintf(w, "# exported: %s (%s)\n", d.Exported, d.ExportedBasis)
	fmt.Fprintf(w, "# references outside the declaration (evidence: references): %d listed of %d, %d in non-test code, %d inside the declaration itself\n",
		d.References.Count, d.References.Total, d.References.NonTest, d.References.InsideDeclaration)
	writeRefRows(w, d.References.Rows)
	last := "no"
	if d.Last.OfPackage {
		last = "yes, of its package"
	} else if d.Last.OfFile {
		last = "yes, of its file"
	} else if !d.Last.Checked {
		last = "unknown"
	}
	fmt.Fprintf(w, "# last symbol (evidence: index): %s", last)
	if d.Last.Detail != "" {
		fmt.Fprintf(w, " — %s", d.Last.Detail)
	}
	fmt.Fprintln(w)
	if d.Importers != nil {
		fmt.Fprintf(w, "# importers of %s (evidence: importers): %d listed of %d\n", dotIfEmpty(d.ImportersTarget), d.Importers.Count, d.Importers.Total)
		writeRefRows(w, d.Importers.Rows)
	}
	for _, c := range d.Caveats {
		fmt.Fprintf(w, "# caveat: %s\n", c)
	}
	writeWarningLines(w, warnings)
}
