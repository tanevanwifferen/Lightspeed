package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed check_references <name|loc>` — is this used anywhere?
//
// One call in place of a `references` followed by a grep. When the argument
// resolves to exactly one symbol it lists the semantic references, and, apart
// from them, the whole-word text occurrences of the identifier that no reference
// accounts for — strings, comments, configuration, other languages, dynamic uses
// a language server cannot see. The two lists are never merged: a verdict of
// "used" rests on the first, "used_only_in_text" says the second is all there is
// (docs/DECISIONS.md D38). A bare name that matches several symbols is its own
// verdict, "ambiguous": no semantic query was asked for the name as a whole, so
// neither "used_only_in_text" nor "unused" would be true of it (D44).

// The verdicts of check_references.
const (
	verdictUsed           = "used"
	verdictUsedOnlyInText = "used_only_in_text"
	verdictUnused         = "unused"
	verdictAmbiguous      = "ambiguous"
)

// maxCandidateChecks is how many candidates of an ambiguous name get their own
// semantic reference count (each is one references query).
const maxCandidateChecks = 5

// defaultRefLimit is how many rows of each list are printed unless --limit says.
const defaultRefLimit = 30

// A refSection is one of the two lists, with the counts behind it.
type refSection struct {
	// Evidence is the query the rows come from.
	Evidence string `json:"evidence"`
	// Count is the rows listed, Total how many there are; they differ exactly
	// when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	// Files is how many distinct files hold the Total rows' listed part, InTests
	// how many listed rows are in test files.
	Files   int      `json:"files"`
	InTests int      `json:"in_tests"`
	Rows    []refRow `json:"rows"`
}

// A refResolution says what the argument resolved to, so that a text-only
// answer is not mistaken for a semantic one.
type refResolution struct {
	// Kind is "symbol" (one symbol: semantic references were asked), "ambiguous"
	// (several matched), "not_found" (none), or "unavailable" (no language
	// server can answer for it). Everything but "symbol" is a text-only answer.
	Kind string `json:"kind"`
	// Detail is the reason, when it is not "symbol".
	Detail     string         `json:"detail,omitempty"`
	Candidates []refCandidate `json:"candidates,omitempty"`
}

// A refCandidate is one symbol an ambiguous name could mean.
type refCandidate struct {
	Symbol   string `json:"symbol"`
	Kind     string `json:"kind,omitempty"`
	Location string `json:"location"`
	// References is the number of semantic references the server reports to this
	// candidate outside its declaration; absent when it was not counted (the
	// first maxCandidateChecks candidates are, and only when the server answers).
	References *int `json:"references,omitempty"`
}

// checkReferencesData is the payload of `check_references`.
type checkReferencesData struct {
	Root       string        `json:"root"`
	Identifier string        `json:"identifier"`
	Resolution refResolution `json:"resolution"`
	// Symbol is the symbol the semantic references are of, when there is one.
	Symbol *subject `json:"symbol,omitempty"`
	// Verdict is used, used_only_in_text, unused or ambiguous, and Rule says which
	// evidence each rests on.
	Verdict  string     `json:"verdict"`
	Rule     string     `json:"rule"`
	Semantic refSection `json:"semantic"`
	TextOnly refSection `json:"text_only"`
	// Truncated and Total summarize both lists: Truncated is set when either
	// was cut by --limit, Total is the rows they had before it.
	Truncated bool `json:"truncated"`
	Total     int  `json:"total"`
	// TextCapped is set when a file had more matching lines than a text scan
	// keeps, so the text_only total is a lower bound.
	TextCapped bool `json:"text_capped,omitempty"`
}

func checkReferencesCommand(e *env, c *command, args []string) int {
	var (
		globs stringList
		fset  *flag.FlagSet
	)
	common, sf, target, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.Var(&globs, "glob", "only search these files for text hits (repeatable; a leading ! excludes; no / matches at any depth)")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "check_references", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultRefLimit)
	root, err := commandWorkspace(sf.path)
	if err != nil {
		return e.fail(err)
	}

	data := checkReferencesData{Root: root}
	var warnings []string

	// Resolve the argument. A name that does not resolve to one symbol is not
	// an error: it is a text-only answer that says why.
	var cs *composed
	name := sf.query
	switch {
	case sf.id != "":
		cs, err = openSubject(e, common, sf, "")
	case sf.query != "":
	case isSymbolID(target):
		sf.id = target
		cs, err = openSubject(e, common, sf, "")
	default:
		if loc, ok := looksLikeLocation(target, root); ok {
			cs, err = openSubject(e, common, sf, loc)
		} else {
			name = target
		}
	}
	if err != nil {
		return e.fail(err)
	}
	if cs == nil {
		where, w, rerr := resolveSymbol(e, common, &symbolFlags{query: name, path: sf.path})
		warnings = append(warnings, w...)
		switch {
		case rerr == nil:
			cs, err = openSubject(e, common, &symbolFlags{path: sf.path}, where)
			if err != nil {
				return e.fail(err)
			}
		default:
			res, ok := unresolved(rerr)
			if !ok {
				return e.fail(rerr)
			}
			data.Resolution = res
			data.Identifier = lastNameSegment(name)
			if res.Kind == "ambiguous" {
				warnings = append(warnings, countCandidateRefs(e, common, sf, res.Candidates)...)
			}
		}
	}
	if cs != nil {
		defer cs.Close()
	}

	var (
		run      *indexRun
		semantic []refRow
		declFile string
		declLine int
	)
	if cs != nil {
		warnings = append(warnings, cs.Warnings...)
		if r, rerr := startIndex(e, common, cs.Root); rerr == nil {
			run = r
			defer run.close()
		} else {
			warnings = append(warnings, "no enclosing symbol ids: "+rerr.Error())
		}
		decl, w := cs.declaration(run)
		warnings = append(warnings, w...)
		data.Identifier = decl.Word
		if data.Identifier == "" {
			data.Identifier = lastNameSegment(cs.Subject.Name)
		}
		declFile, declLine = decl.File, decl.Line
		locs, w, rerr := cs.References(false)
		warnings = append(warnings, w...)
		switch {
		case rerr == nil:
			data.Resolution = refResolution{Kind: "symbol"}
			sym := cs.Subject
			if decl.ID != "" {
				sym = subject{ID: decl.ID, Name: decl.Name, Kind: decl.Kind, File: decl.File, Line: decl.Line, EndLine: decl.EndLine}
			}
			data.Symbol = &sym
			outside := 0
			for _, loc := range locs {
				if row, ok := cs.rowFor(loc, evidenceReferences); ok {
					semantic = append(semantic, row)
				} else {
					outside++
				}
			}
			if outside > 0 {
				warnings = append(warnings, fmt.Sprintf("%d reference(s) are outside the workspace or unreadable and are not listed", outside))
			}
		default:
			if _, ok := errCodedAs(rerr, render.CodeUnsupportedMethod); !ok {
				return e.fail(rerr)
			}
			data.Resolution = refResolution{Kind: "unavailable",
				Detail: "the server does not answer textDocument/references; text hits only: " + rerr.Error()}
		}
	}
	if data.Identifier == "" {
		return e.fail(render.Errorf(render.CodeUsage, "check_references: there is no identifier at the position to look for"))
	}

	sortRefRows(semantic)
	var truncated bool
	data.Semantic = refSection{Evidence: evidenceReferences, Rows: []refRow{}}
	shown, truncated, total := capRows(semantic, limit)
	data.Semantic.Total, data.Semantic.Truncated, data.Semantic.Limit = total, truncated, limit
	data.Semantic.Rows = append(data.Semantic.Rows, shown...)
	if run != nil {
		warnings = append(warnings, attachEnclosing(run, data.Semantic.Rows)...)
	}

	// The text hits: the identifier as a whole word wherever the semantic
	// references do not already account for the line.
	data.TextOnly = refSection{Evidence: evidenceSearchText, Rows: []refRow{}}
	var textRows []refRow
	if strings.IndexFunc(data.Identifier, isWordRune) >= 0 {
		ctx, cancel := scanContext(e, common)
		scan, serr := scanIdentifier(ctx, root, data.Identifier, globs)
		cancel()
		if serr != nil {
			return e.fail(serr)
		}
		warnings = append(warnings, scan.Warnings...)
		data.TextCapped = scan.Capped
		covered := map[string]bool{declFile + ":" + fmt.Sprint(declLine): declFile != ""}
		for _, r := range semantic {
			covered[fmt.Sprintf("%s:%d", r.File, r.Line)] = true
		}
		for _, m := range scan.Matches {
			if covered[fmt.Sprintf("%s:%d", m.File, m.Line)] {
				continue
			}
			textRows = append(textRows, refRow{File: m.File, Line: m.Line, Column: m.Col, Text: clipRowText(m.Text),
				Test: isTestPath(m.File), Evidence: evidenceSearchText})
		}
		if scan.Capped {
			warnings = append(warnings, fmt.Sprintf("a file had more than %d matching lines; the text_only total is a lower bound", perFileKeep))
		}
	}
	sortRefRows(textRows)
	shown, truncated, total = capRows(textRows, limit)
	data.TextOnly.Total, data.TextOnly.Truncated, data.TextOnly.Limit = total, truncated, limit
	data.TextOnly.Rows = append(data.TextOnly.Rows, shown...)
	for _, sec := range []*refSection{&data.Semantic, &data.TextOnly} {
		sec.Count, sec.Files = len(sec.Rows), distinctRowFiles(sec.Rows)
		for _, r := range sec.Rows {
			if r.Test {
				sec.InTests++
			}
		}
		if sec.Truncated {
			warnings = append(warnings, truncationWarning(strings.ReplaceAll(sec.Evidence, "_", " ")+" rows",
				sec.Count, sec.Total, "narrow the text scan with --glob, or raise --limit"))
		}
	}

	data.Truncated = data.Semantic.Truncated || data.TextOnly.Truncated
	data.Total = data.Semantic.Total + data.TextOnly.Total
	data.Verdict, data.Rule = referenceVerdict(data)
	if data.Semantic.Total > 0 && data.Semantic.InTests == data.Semantic.Count && data.Semantic.Count == data.Semantic.Total {
		warnings = append(warnings, "every semantic reference is in a test file")
	}
	exit := ExitOK
	if data.Verdict == verdictUnused {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeCheckReferencesText(e.stdout, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// referenceVerdict states the verdict and the rule that produced it.
func referenceVerdict(d checkReferencesData) (verdict, rule string) {
	semantic := d.Resolution.Kind == "symbol"
	switch {
	case d.Resolution.Kind == "ambiguous":
		return verdictAmbiguous, ambiguousRule(d)
	case d.Semantic.Total > 0:
		return verdictUsed, fmt.Sprintf("used: the language server reports %d reference(s) to the symbol outside its declaration", d.Semantic.Total)
	case d.TextOnly.Total > 0 && semantic:
		return verdictUsedOnlyInText, fmt.Sprintf("used_only_in_text: the server reports no reference outside the declaration, but the identifier occurs as a whole word on %d other line(s) (strings, comments, configuration, other languages or dynamic use)", d.TextOnly.Total)
	case d.TextOnly.Total > 0:
		return verdictUsedOnlyInText, fmt.Sprintf("used_only_in_text: the name did not resolve to one symbol (%s), so no semantic references were asked; the identifier occurs as a whole word on %d line(s)", d.Resolution.Kind, d.TextOnly.Total)
	case semantic:
		return verdictUnused, "unused: no semantic reference outside the declaration and no whole-word occurrence of the identifier in any searched file (reflection, code generation and files skipped for size or type are not seen)"
	}
	return verdictUnused, fmt.Sprintf("unused: the name did not resolve to one symbol (%s) and no whole-word occurrence of it was found in any searched file", d.Resolution.Kind)
}

// ambiguousRule words the ambiguous verdict, with the per-candidate counts that
// were taken so the answer to "is this used" is not left to a second call.
func ambiguousRule(d checkReferencesData) string {
	var counted []string
	used, checked := 0, 0
	for _, c := range d.Resolution.Candidates {
		if c.References == nil {
			continue
		}
		checked++
		if *c.References > 0 {
			used++
		}
		counted = append(counted, fmt.Sprintf("%s %d", c.Symbol, *c.References))
	}
	rule := fmt.Sprintf("ambiguous: the name matches %d symbols, so no single symbol was queried and it is NOT known whether the name is unused", len(d.Resolution.Candidates))
	if checked > 0 {
		rule += fmt.Sprintf("; semantic references per candidate (%d of %d counted): %s", checked, len(d.Resolution.Candidates), strings.Join(counted, ", "))
		if used > 0 {
			rule += fmt.Sprintf("; %d candidate(s) are used", used)
		}
	}
	return rule + "; the text-only rows are for the bare identifier. Pass a candidate's location, or --id, for its own verdict"
}

// countCandidateRefs asks the server how many references each of the first
// maxCandidateChecks candidates has. A candidate that cannot be counted is left
// uncounted and named in a warning; it never turns into a zero.
func countCandidateRefs(e *env, common *commonFlags, sf *symbolFlags, cands []refCandidate) []string {
	var warnings []string
	for i := range cands {
		if i == maxCandidateChecks {
			warnings = append(warnings, fmt.Sprintf("semantic references were counted for the first %d of %d candidates", maxCandidateChecks, len(cands)))
			break
		}
		cs, err := openSubject(e, common, &symbolFlags{path: sf.path}, cands[i].Location)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("references of %s not counted: %s", cands[i].Symbol, oneLine(err.Error())))
			continue
		}
		locs, _, err := cs.References(false)
		cs.Close()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("references of %s not counted: %s", cands[i].Symbol, oneLine(err.Error())))
			continue
		}
		n := len(locs)
		cands[i].References = &n
	}
	return warnings
}

// unresolved turns a --symbol resolution failure that is an answer rather than a
// fault into a resolution. It is false for anything else (not ready, timeout,
// a broken config), which stays an error.
func unresolved(err error) (refResolution, bool) {
	if ce, ok := errCodedAs(err, render.CodeUsage); ok {
		if m, ok := ce.Details.(map[string]any); ok {
			if raw, ok := m["candidates"]; ok {
				res := refResolution{Kind: "ambiguous", Detail: ce.Message}
				if b, merr := json.Marshal(raw); merr == nil {
					_ = json.Unmarshal(b, &res.Candidates)
				}
				if len(res.Candidates) > symbolCandidateLimit {
					res.Candidates = res.Candidates[:symbolCandidateLimit]
				}
				res.Detail = fmt.Sprintf("the name matches %d symbols; the text hits below are for the bare identifier. Pass one candidate's location, or --id, for semantic references", len(res.Candidates))
				return res, true
			}
		}
		return refResolution{}, false
	}
	if ce, ok := errCodedAs(err, render.CodeNotFound); ok {
		return refResolution{Kind: "not_found", Detail: ce.Message + "; text hits only"}, true
	}
	for _, code := range []render.Code{render.CodeNoServer, render.CodeServerNotInstalled, render.CodeUnsupportedMethod} {
		if ce, ok := errCodedAs(err, code); ok {
			return refResolution{Kind: "unavailable", Detail: "no language server can resolve the name (" + ce.Message + "); text hits only"}, true
		}
	}
	return refResolution{}, false
}

func writeCheckReferencesText(w io.Writer, d checkReferencesData, warnings []string) {
	fmt.Fprintf(w, "# %s %s: %s\n", "check_references", d.Identifier, d.Rule)
	resolution := d.Resolution.Kind
	if d.Symbol != nil && d.Symbol.ID != "" {
		resolution += " " + d.Symbol.ID
	}
	fmt.Fprintf(w, "# resolution: %s\n", resolution)
	if d.Resolution.Detail != "" {
		fmt.Fprintf(w, "# %s\n", d.Resolution.Detail)
	}
	for _, cand := range d.Resolution.Candidates {
		refs := ""
		if cand.References != nil {
			refs = fmt.Sprintf(", %d reference(s)", *cand.References)
		}
		fmt.Fprintf(w, "#   candidate %s (%s) at %s%s\n", cand.Symbol, cand.Kind, cand.Location, refs)
	}
	for _, sec := range []struct {
		title string
		s     refSection
	}{{"semantic references", d.Semantic}, {"text-only mentions", d.TextOnly}} {
		fmt.Fprintf(w, "# %s (evidence: %s): %d listed of %d in %d file(s), %d in tests\n",
			sec.title, sec.s.Evidence, sec.s.Count, sec.s.Total, sec.s.Files, sec.s.InTests)
		writeRefRows(w, sec.s.Rows)
	}
	writeWarningLines(w, slices.Clone(warnings))
}
