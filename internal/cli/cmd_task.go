package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// taskCommands is `task_context`: the answer to "I am about to do this; what in
// the code is it about?" without a model (docs/DECISIONS.md D41).
func taskCommands() []*command {
	return []*command{{
		Name:    "task_context",
		Args:    "<task>",
		Summary: "rank the symbols a free-text task is about and return a token-budgeted capsule of them, with a confidence",
		Run:     taskContextCommand,
		MCP: oneTool("task_context", "Start here for a coding task: task in plain words, ranked symbols with source, callers, files and an honest confidence out. Low: not implemented here.",
			[]param{
				{Name: "task", Type: typeString, Required: true, Desc: "The task or question in plain words; code names (camelCase, snake_case, pkg.Type.Method, `quoted`, --flags) and file paths in it count most."},
				{Flag: "budget", Type: typeInt, Min: intp(1), Desc: "Token budget of the capsule (default 4000; about 4 bytes per token). What does not fit is dropped and reported."},
				{Flag: "with-source", Type: typeInt, Min: intp(0), Desc: "Include the source of the best N symbols (default 0: none)."},
				{Flag: "expand", Type: typeInt, Min: intp(0), Desc: "Expand the best N symbols with callers, callees, imports and neighbours (default 3, 0 for none)."},
				{Flag: "max-lines", Type: typeInt, Min: intp(1), Desc: "Cap each included source at this many lines (default 60)."},
				{Flag: "rule", Type: typeBool, Desc: "Restore what the default answer trims: the full confidence rule, the extracted terms, the verdict sentence, and full detail on more than the first 5 related rows."},
				indexRootParam(),
			}, commonParams("limit", "timeout", "report")),
	}}
}

// Defaults of `task_context`. defaultTaskSymbols, defaultTaskSources and
// defaultTaskRelated were cut for the default JSON answer's token cost
// (docs/DECISIONS.md D46): no inlined source, fewer ranked symbols, and a
// hard cap on the related rows kept inline — each is still one flag away
// (--with-source, --limit, and the standalone `related` command for the
// full neighbour list).
const (
	defaultTaskBudget   = 4000
	defaultTaskSymbols  = 8
	defaultTaskSources  = 0
	defaultTaskRelated  = 5
	defaultTaskExpand   = 3
	defaultTaskMaxLines = 60
	taskBytesPerToken   = 4
	taskSymbolShare     = 0.35 // of the budget, the most the ranked list may take
	taskRelatedReserve  = 0.20 // of the budget, kept back from source for neighbours
	taskMinSourceBytes  = 400  // a source cut smaller than this is dropped instead
)

// A taskSymbol is one ranked symbol of the capsule.
type taskSymbol struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Kind      string   `json:"kind"`
	File      string   `json:"file"`
	Line      int      `json:"line"`
	EndLine   int      `json:"end_line,omitempty"`
	Signature string   `json:"signature,omitempty"`
	Doc       string   `json:"doc,omitempty"`
	Score     float64  `json:"score"`
	Evidence  []string `json:"evidence"`
	// CallersTotal and CalleesTotal are the direct call counts the call
	// hierarchy gave, when it was asked; the related rows hold a few of them.
	CallersTotal *int        `json:"callers_total,omitempty"`
	CalleesTotal *int        `json:"callees_total,omitempty"`
	Source       *sourceItem `json:"source,omitempty"`
}

// A taskFile is a file the capsule involves and why.
type taskFile struct {
	File string   `json:"file"`
	Why  []string `json:"why"`
}

// taskSymbolCompact is the default JSON shape of a ranked symbol: its id
// (which already spells out name, kind and file — `path::Container.Name#kind`,
// D21), its position, a signature and its score. --rule restores the full
// taskSymbol: name, kind, file, end_line, doc, every evidence entry and the
// call totals (docs/DECISIONS.md D46).
type taskSymbolCompact struct {
	ID        string      `json:"id"`
	Line      int         `json:"line"`
	Signature string      `json:"signature,omitempty"`
	Score     float64     `json:"score"`
	Source    *sourceItem `json:"source,omitempty"`
}

// compactSymbols drops every taskSymbol field the id already implies (name,
// kind, file) and the ones --rule alone restores (end_line, doc, evidence,
// the call totals): what is left is enough to locate, read and rank each
// symbol, not why it was ranked there.
func compactSymbols(syms []taskSymbol) []taskSymbolCompact {
	out := make([]taskSymbolCompact, len(syms))
	for i, s := range syms {
		out[i] = taskSymbolCompact{ID: s.ID, Line: s.Line, Signature: s.Signature, Score: s.Score, Source: s.Source}
	}
	return out
}

// taskRelatedCompact is the default JSON shape of a related row: which
// ranked symbol it neighbours — OfIndex, its position in `symbols`, cheaper
// than repeating that symbol's id — how (Relation), and its own identity:
// the id when the index could name one, its file when it could not. --rule
// restores the full taskRelated (kind, line, detail, evidence, and Of as the
// id it names there) and lifts the row cap; the standalone `related` command
// always answers with every neighbour, at any confidence (D46).
type taskRelatedCompact struct {
	OfIndex  int    `json:"of_index"`
	Relation string `json:"relation"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	File     string `json:"file,omitempty"`
}

// compactRelatedRows caps rows to n (0 for no cap) and drops every field but
// identity and which ranked symbol (by index into syms) each neighbours,
// reporting how many rows the cap itself cut (beyond whatever the budget had
// already dropped). A row whose Of no longer names a ranked symbol (dropped
// by the budget) is left out rather than pointing at nothing.
func compactRelatedRows(rows []taskRelated, syms []taskSymbol, n int) (out []taskRelatedCompact, cut int) {
	rank := make(map[string]int, len(syms))
	for i, s := range syms {
		rank[s.ID] = i
	}
	kept := rows
	if n > 0 && len(rows) > n {
		kept, cut = rows[:n], len(rows)-n
	}
	for _, r := range kept {
		idx, ok := rank[r.Of]
		if !ok {
			continue
		}
		row := taskRelatedCompact{OfIndex: idx, Relation: r.Relation, ID: r.ID}
		switch {
		case r.ID != "":
			row.Name = r.Name
		case r.Name != r.File:
			// An import-graph row (no id) usually names itself after its
			// file; only carry Name when it says something File does not.
			row.Name, row.File = r.Name, r.File
		default:
			row.File = r.File
		}
		out = append(out, row)
	}
	if out == nil {
		out = []taskRelatedCompact{}
	}
	return out, cut
}

// taskContextJSONView is the JSON shape of a task_context answer:
// taskContextData with Terms, Verdict, Rule, Symbols and Related trimmed to
// their default, token-cheap form unless --rule asks for the full ones
// (D46). Go's field shadowing (not a JSON-specific rule) makes these five
// hide the embedded ones for every encoding/json purpose, including Marshal.
// Text output is unaffected: it renders taskContextData directly, never this
// type.
type taskContextJSONView struct {
	taskContextData
	Terms   []taskTerm `json:"terms,omitempty"`
	Verdict string     `json:"verdict,omitempty"`
	Rule    string     `json:"confidence_rule"`
	Symbols any        `json:"symbols"`
	Related any        `json:"related,omitempty"`
}

// A taskBudget says what the capsule cost and what did not fit.
type taskBudget struct {
	// Limit is --budget and Used the estimated size of this data (about 4 bytes
	// per token), not counting the envelope's warnings.
	Limit int `json:"limit"`
	Used  int `json:"used"`
	// Truncated is true when the budget dropped or cut anything; Dropped counts
	// what, per section.
	Truncated bool           `json:"truncated"`
	Dropped   map[string]int `json:"dropped,omitempty"`
	// Over is set when Used is still above Limit after everything that can be
	// shed was: the verdict, its reason and the best symbol alone are larger.
	Over bool `json:"over,omitempty"`
}

// taskContextData is the payload of `task_context`.
type taskContextData struct {
	Root string `json:"root"`
	Task string `json:"task"`
	// Terms are what was read out of the task, and what each did.
	Terms        []taskTerm `json:"terms"`
	TermsDropped int        `json:"terms_dropped,omitempty"`
	// Confidence is high, medium or low; Reason the evidence that decided it and
	// Rule the rule it was decided by. Verdict says it in words.
	Confidence string `json:"confidence"`
	Reason     string `json:"reason"`
	Rule       string `json:"confidence_rule"`
	Verdict    string `json:"verdict"`

	Symbols []taskSymbol `json:"symbols"`
	// Count is how many symbols are listed, Total how many cleared the
	// relevance floor; they differ exactly when Truncated is set (--limit or the
	// budget).
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	// Withheld is how many weaker candidates were not listed: below the
	// relevance floor, or (at low confidence) all of them.
	Withheld int           `json:"withheld,omitempty"`
	Related  []taskRelated `json:"related,omitempty"`
	Files    []taskFile    `json:"files,omitempty"`
	// Nearest are names close to the task's, at low confidence only: a hint of
	// what to look for, not matches.
	Nearest []string   `json:"nearest,omitempty"`
	Budget  taskBudget `json:"budget"`
	// Index is a reportSummary by default, the full *index.SyncReport with
	// --report (D46).
	Index any `json:"index,omitempty"`
}

// tokensOf estimates the tokens of a value by its JSON size.
func tokensOf(v any) int {
	b, _ := json.Marshal(v)
	return (len(b) + taskBytesPerToken - 1) / taskBytesPerToken
}

// taskContextCommand implements `lightspeed task_context <task>`.
func taskContextCommand(e *env, c *command, args []string) int {
	var (
		budget, withSource, expand, maxLines int
		rootFlag                             string
		fullRule                             bool
		fset                                 *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 1, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.IntVar(&budget, "budget", defaultTaskBudget, "token budget of the capsule (about 4 bytes per token); what does not fit is dropped and reported")
		fs.IntVar(&withSource, "with-source", defaultTaskSources, "include the source of the best N symbols (default 0: none)")
		fs.IntVar(&expand, "expand", defaultTaskExpand, "expand the best N symbols with callers, callees, imports and neighbours (0 for none)")
		fs.IntVar(&maxLines, "max-lines", defaultTaskMaxLines, "cap each included source at this many lines")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
		fs.BoolVar(&fullRule, "rule", false, "restore what the default answer trims: the full confidence rule, the extracted terms, the verdict sentence, and full detail on more than the first 5 related rows")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "task_context", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	task := strings.TrimSpace(positional[0])
	switch {
	case task == "":
		return e.usagef("task_context: the task is empty; say what you are about to do, in words")
	case budget < 1 || maxLines < 1:
		return e.usagef("task_context: --budget and --max-lines must be at least 1")
	case withSource < 0 || expand < 0:
		return e.usagef("task_context: --with-source and --expand must not be negative")
	}
	limit := effectiveLimit(fset, common, defaultTaskSymbols)

	terms, droppedTerms := extractTerms(task)
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()

	data := taskContextData{Root: run.root, Task: task, Terms: terms, TermsDropped: droppedTerms, Rule: confidenceRuleShort,
		Symbols: []taskSymbol{}, Limit: limit}
	if fullRule {
		data.Rule = confidenceRule
	}
	var warnings []string
	if droppedTerms > 0 {
		warnings = append(warnings, fmt.Sprintf("the task has %d more terms than the %d searched; the lightest were dropped", droppedTerms, maxTaskTerms))
	}

	cands, hashes, report, ws, err := rankTask(run, run.root, task, data.Terms)
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, ws...)
	warnings = append(warnings, syncWarnings(report)...)
	data.Index = indexReportField(report, common.report)
	data.Confidence, data.Reason = judgeTask(cands, data.Terms)

	switch data.Confidence {
	case confLow:
		data.Verdict = "probably not implemented here: " + data.Reason + "; there is no symbol to start from, so treat it as new code, and check the nearest names below only as a hint"
		data.Withheld = len(cands)
		data.Nearest = nearestNames(run, cands, data.Terms)
	default:
		if data.Confidence == confHigh {
			data.Verdict = "found: " + data.Reason
		} else {
			data.Verdict = "candidates found, none certain: " + data.Reason + "; read them before relying on them"
		}
		floor := cands[0].score * relevanceFloor
		var kept []*taskCand
		for _, c := range cands {
			if c.score >= floor {
				kept = append(kept, c)
			}
		}
		data.Withheld = len(cands) - len(kept)
		data.Total = len(kept)
		if limit > 0 && len(kept) > limit {
			kept = kept[:limit]
			data.Truncated = true
		}
		for _, c := range kept {
			data.Symbols = append(data.Symbols, taskSymbol{ID: c.sym.ID, Name: c.sym.Name, Kind: c.sym.Kind, File: c.file,
				Line: c.sym.Line, EndLine: c.sym.EndLine, Signature: clipSignature(c.sym.Signature), Doc: c.sym.Doc,
				Score: roundScore(c.score), Evidence: c.evidence})
		}
		if data.Truncated {
			warnings = append(warnings, truncationWarning("symbols", len(kept), data.Total, "raise --limit, or make the task more specific"))
		}
		var ex *expansion
		if expand > 0 {
			ex = expandTop(e, common, run, data.Symbols, expand)
			warnings = append(warnings, ex.warnings...)
		}
		warnings = append(warnings, assembleCapsule(&data, run.root, hashes, ex, budget, withSource, maxLines)...)
	}
	data.Count = len(data.Symbols)
	data.Budget.Limit = budget
	if data.Confidence == confLow {
		// No capsule to assemble, but the diagnostics still count against it.
		warnings = append(warnings, shedDiagnostics(&data, budget, 0)...)
	}

	exit := ExitOK
	if data.Confidence == confLow {
		exit = ExitProblems
	}

	// Text is unaffected by the JSON compaction below (D46): its budget
	// accounting is the full answer's, exactly as before.
	if format == render.FormatText {
		data.Budget.Used = tokensOf(data)
		if data.Budget.Used > budget {
			data.Budget.Over = true
			warnings = append(warnings, budgetOverWarning(budget, data.Budget.Used))
		}
		writeTaskContextText(e.stdout, data, warnings, common.absolute)
		return exit
	}

	// JSON defaults to the token-cheap shape: id-only symbols (no name, kind,
	// file, end_line, doc or evidence beyond the strongest match), no terms
	// array, no composed verdict sentence (confidence + reason already say
	// it), a bare rule name instead of the rule's text (the reason already
	// carries the per-answer evidence), and at most defaultTaskRelated
	// related rows with only their identity. --rule restores all of it
	// (D46). This is a default-shape decision, not the --budget accounting:
	// it does not set Budget.Truncated, which stays what it means elsewhere
	// — the token budget bit — and the related cut is said in a warning
	// instead, so it is never silent.
	view := taskContextJSONView{taskContextData: data, Terms: data.Terms, Verdict: data.Verdict, Rule: data.Rule,
		Symbols: data.Symbols, Related: data.Related}
	if !fullRule {
		view.Terms = nil
		view.Verdict = ""
		view.Rule = confidenceRuleName
		view.Symbols = compactSymbols(data.Symbols)
		compact, cut := compactRelatedRows(data.Related, data.Symbols, defaultTaskRelated)
		view.Related = compact
		if cut > 0 {
			warnings = append(warnings, fmt.Sprintf(
				"%d more related rows are not shown by default (of %d); --rule shows them all, with full detail, or call `related` on a symbol's id",
				cut, len(data.Related)))
		}
	}
	view.Budget.Used = tokensOf(view)
	if view.Budget.Used > budget {
		view.Budget.Over = true
		warnings = append(warnings, budgetOverWarning(budget, view.Budget.Used))
	}
	return e.writeData(common, view, warnings, exit)
}

// budgetOverWarning is the warning for a --budget too small for even the
// smallest answer.
func budgetOverWarning(budget, used int) string {
	return fmt.Sprintf("the %d-token budget is below the smallest answer (%d tokens: the verdict, its reason and the best symbol, if any); raise --budget", budget, used)
}

// maxSignatureBytes caps a signature in the capsule: a declaration written on
// one very long line is not worth its tokens twice, once as signature and once
// as source.
const maxSignatureBytes = 240

// clipSignature shortens a signature to maxSignatureBytes on a character
// boundary, marking the cut.
func clipSignature(sig string) string {
	if len(sig) <= maxSignatureBytes {
		return sig
	}
	cut := maxSignatureBytes
	for cut > 0 && !utf8.RuneStart(sig[cut]) {
		cut--
	}
	return sig[:cut] + "…"
}

// nearestNames are the hint at low confidence: the names of the weak matches,
// or, when there are none, the closest names by trigram and edit distance to the
// heaviest term.
func nearestNames(run *indexRun, weak []*taskCand, terms []taskTerm) []string {
	var names []string
	add := func(n string) {
		if n != "" && !slices.Contains(names, n) && len(names) < 5 {
			names = append(names, n)
		}
	}
	for _, c := range weak {
		add(c.sym.Qualified)
	}
	if len(names) > 0 {
		return names
	}
	for _, t := range terms {
		if t.Kind == termPath {
			continue
		}
		var out index.SearchOutcome
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSearch, Search: &index.SearchQuery{Text: t.Term, Limit: 5, Fuzzy: true}}, &out); err != nil {
			return nil
		}
		for _, h := range out.Hits {
			add(h.Symbol.Qualified)
		}
		for _, n := range out.Nearest {
			add(n)
		}
		break
	}
	return names
}

// assembleCapsule adds source, neighbours and files to the ranked symbols under
// the budget, in that order of priority, and records what did not fit. The
// symbols themselves were listed first; they may use a share of the budget, the
// best ones' source most of the rest, and the neighbours what is left, so that
// no section starves the others.
func assembleCapsule(d *taskContextData, root string, hashes map[string]string, ex *expansion, budget, withSource, maxLines int) (warnings []string) {
	dropped := map[string]int{}
	// The fixed part is measured, not guessed: the answer with its variable
	// sections emptied and a budget block as large as it can get. When it and
	// the best symbol already exceed the budget, the diagnostics go first.
	first := 0
	if len(d.Symbols) > 0 {
		first = tokensOf(d.Symbols[0])
	}
	shedDiagnostics(d, budget, first) // reported with the rest of the drops below
	for k, n := range d.Budget.Dropped {
		dropped[k] = n
	}
	used := fixedTokens(d, budget)

	// 1. The ranked symbols, as many as fit in their share; one always does.
	symbolCeiling := used + int(float64(budget)*taskSymbolShare)
	for i := range d.Symbols {
		s := d.Symbols[i]
		if ex != nil {
			if n, ok := ex.callers[s.ID]; ok {
				d.Symbols[i].CallersTotal = intp(n)
			}
			if n, ok := ex.callees[s.ID]; ok {
				d.Symbols[i].CalleesTotal = intp(n)
			}
		}
		cost := tokensOf(d.Symbols[i])
		if i > 0 && used+cost > symbolCeiling {
			dropped["symbols"] = len(d.Symbols) - i
			d.Symbols = d.Symbols[:i]
			d.Truncated = true
			break
		}
		used += cost
	}
	d.Total = max(d.Total, len(d.Symbols))

	// 2. Source of the best few, cut to what remains before the neighbours'
	// reserve. A symbol cut to fewer than taskMinSourceBytes says less than its
	// signature does, so it is dropped instead.
	sourceCeiling := budget - int(float64(budget)*taskRelatedReserve)
	files := map[string]*lineIndex{}
	for i := 0; i < len(d.Symbols) && i < withSource; i++ {
		s := &d.Symbols[i]
		x, ok := files[s.File]
		if !ok {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(s.File)))
			switch {
			case err != nil:
				warnings = append(warnings, fmt.Sprintf("%s: source not included: %v", s.File, err))
				files[s.File] = nil
				continue
			case hashes[s.File] != "" && index.HashBytes(content) != hashes[s.File]:
				warnings = append(warnings, fmt.Sprintf("%s changed while the query ran; its source was not included — run the task again", s.File))
				files[s.File] = nil
				continue
			}
			x = newLineIndex(content)
			files[s.File] = x
		}
		if x == nil {
			continue
		}
		last := s.EndLine - 1
		if s.EndLine == 0 {
			last = s.Line - 1
		}
		item := sliceSource(x, s.Line-1, last, sourceCaps{maxLines: maxLines})
		item.ID, item.File, item.Kind = s.ID, s.File, s.Kind
		cost := tokensOf(item)
		if used+cost > sourceCeiling {
			room := (sourceCeiling-used)*taskBytesPerToken - 300 // the row's own fields
			if room < taskMinSourceBytes {
				dropped["source"]++
				d.Budget.Truncated = true
				continue
			}
			item = sliceSource(x, s.Line-1, last, sourceCaps{maxLines: maxLines, maxBytes: room})
			item.ID, item.File, item.Kind = s.ID, s.File, s.Kind
			cost = tokensOf(item)
			d.Budget.Truncated = true
			dropped["source_bytes"]++
		}
		if item.Truncated {
			warnings = append(warnings, cutWarning(item))
		}
		s.Source = &item
		used += cost
	}

	// The files of the ranked symbols are the cheapest and most useful part of
	// the file list, so their cost is reserved before the neighbours spend the
	// rest.
	filesReserve := 0
	for _, f := range symbolFiles(d.Symbols) {
		filesReserve += tokensOf(taskFile{File: f, Why: []string{"symbol"}})
	}

	// 3. Neighbours, in the order an agent wants them: who calls it, whom it
	// calls, what imports its file, what it imports, what is next to it.
	if ex != nil {
		order := map[string]int{relCaller: 0, relCallee: 1, relImportedBy: 2, relImports: 3, relSibling: 4}
		rows := slices.Clone(ex.rows)
		rank := map[string]int{}
		for i, s := range d.Symbols {
			rank[s.ID] = i
		}
		slices.SortStableFunc(rows, func(a, b taskRelated) int {
			if order[a.Relation] != order[b.Relation] {
				return order[a.Relation] - order[b.Relation]
			}
			return rank[a.Of] - rank[b.Of]
		})
		shown := map[string]bool{}
		for _, s := range d.Symbols {
			shown[s.ID] = true
		}
		for _, r := range rows {
			if !shown[r.Of] {
				continue // its symbol was dropped by the budget
			}
			if cost := tokensOf(r); used+cost <= budget-filesReserve {
				d.Related = append(d.Related, r)
				used += cost
			} else {
				dropped["related"]++
			}
		}
		slices.SortStableFunc(d.Related, func(a, b taskRelated) int {
			if rank[a.Of] != rank[b.Of] {
				return rank[a.Of] - rank[b.Of]
			}
			return order[a.Relation] - order[b.Relation]
		})
	}

	// 4. The files involved.
	why := map[string][]string{}
	add := func(file, reason string) {
		if file != "" && !slices.Contains(why[file], reason) {
			why[file] = append(why[file], reason)
		}
	}
	for _, s := range d.Symbols {
		add(s.File, "symbol")
	}
	for _, r := range d.Related {
		add(r.File, r.Relation)
	}
	names := symbolFiles(d.Symbols)
	var rest []string
	for f := range why {
		if !slices.Contains(names, f) {
			rest = append(rest, f)
		}
	}
	slices.Sort(rest)
	names = append(names, rest...)
	for i, f := range names {
		row := taskFile{File: f, Why: why[f]}
		if cost := tokensOf(row); used+cost <= budget {
			d.Files = append(d.Files, row)
			used += cost
		} else {
			dropped["files"] = len(names) - i
			break
		}
	}

	if len(dropped) > 0 {
		d.Budget.Truncated = true
		d.Budget.Dropped = dropped
		warnings = append(warnings, fmt.Sprintf("the %d-token budget bit (%s); raise --budget for more", budget, droppedSummary(dropped)))
	}
	return warnings
}

// fixedTokens measures the answer with its variable sections emptied and a
// budget block as large as it can get.
func fixedTokens(d *taskContextData, budget int) int {
	base := *d
	base.Symbols, base.Related, base.Files = nil, nil, nil
	base.Budget = taskBudget{Limit: budget, Used: budget, Truncated: true, Over: true,
		Dropped: map[string]int{"symbols": 99, "source": 99, "source_bytes": 99, "related": 99, "files": 99, "index": 1, "terms": 99}}
	return tokensOf(base)
}

// shedDiagnostics drops what describes the search rather than answers the task
// — the index report (its warnings stay in the envelope), then the term list —
// while the fixed part and reserve (the best symbol's cost) do not fit the
// budget, and records it in Budget.Dropped.
func shedDiagnostics(d *taskContextData, budget, reserve int) (warnings []string) {
	if fixedTokens(d, budget)+reserve <= budget {
		return nil
	}
	shed := map[string]int{}
	if d.Index != nil {
		d.Index = nil
		shed["index"] = 1
	}
	if fixedTokens(d, budget)+reserve > budget && len(d.Terms) > 0 {
		shed["terms"] = len(d.Terms)
		d.Terms = []taskTerm{}
	}
	if len(shed) == 0 {
		return nil
	}
	if d.Budget.Dropped == nil {
		d.Budget.Dropped = map[string]int{}
	}
	for k, n := range shed {
		d.Budget.Dropped[k] = n
	}
	d.Budget.Truncated = true
	return []string{fmt.Sprintf("the %d-token budget is tight: %s", budget, droppedSummary(shed))}
}

// symbolFiles are the distinct files of the ranked symbols, in rank order.
func symbolFiles(syms []taskSymbol) []string {
	var out []string
	for _, s := range syms {
		if !slices.Contains(out, s.File) {
			out = append(out, s.File)
		}
	}
	return out
}

// droppedSummary renders the per-section drops in a fixed order.
func droppedSummary(dropped map[string]int) string {
	var parts []string
	for _, k := range []string{"symbols", "source", "source_bytes", "related", "files", "index", "terms"} {
		if n := dropped[k]; n > 0 {
			label := map[string]string{"symbols": "symbols dropped", "source": "sources dropped", "source_bytes": "sources cut short",
				"related": "neighbours dropped", "files": "files dropped", "index": "index report dropped", "terms": "terms dropped"}[k]
			parts = append(parts, fmt.Sprintf("%d %s", n, label))
		}
	}
	return strings.Join(parts, ", ")
}

// writeTaskContextText renders the capsule for a person or a terminal: the
// verdict first, the rule behind it, then the symbols with their source, the
// neighbours and the files. It carries the same content as the JSON.
func writeTaskContextText(w io.Writer, d taskContextData, warnings []string, absolute bool) {
	fmt.Fprintf(w, "confidence: %s — %s\n", d.Confidence, d.Verdict)
	fmt.Fprintf(w, "rule: %s\n", d.Rule)
	var terms []string
	for _, t := range d.Terms {
		terms = append(terms, fmt.Sprintf("%s(%s %.1f, %s)", t.Term, t.Kind, t.Weight, matchNote(t)))
	}
	if len(terms) > 0 {
		fmt.Fprintf(w, "terms: %s\n", strings.Join(terms, "  "))
	}
	if len(d.Nearest) > 0 {
		fmt.Fprintf(w, "nearest names (a hint, not matches): %s\n", strings.Join(d.Nearest, ", "))
	}
	if d.Confidence == confLow && d.Withheld > 0 {
		fmt.Fprintf(w, "%d weak matches withheld\n", d.Withheld)
	}
	path := func(p string) string {
		if absolute {
			return filepath.Join(d.Root, filepath.FromSlash(p))
		}
		return p
	}
	for i, s := range d.Symbols {
		sig := s.Signature
		if sig == "" {
			sig = s.Name
		}
		fmt.Fprintf(w, "%d. %s:%d: %s %s  [score %.1f; %s]\n   id %s\n", i+1, path(s.File), s.Line, s.Kind, sig, s.Score, strings.Join(s.Evidence, " "), s.ID)
		if s.CallersTotal != nil || s.CalleesTotal != nil {
			fmt.Fprintf(w, "   calls: %s callers, %s callees\n", countOrDash(s.CallersTotal), countOrDash(s.CalleesTotal))
		}
		if s.Source != nil {
			for _, line := range strings.Split(strings.TrimRight(s.Source.Source, "\n"), "\n") {
				fmt.Fprintf(w, "   | %s\n", line)
			}
			if s.Source.Truncated {
				fmt.Fprintf(w, "   | … cut (%d of %d lines)\n", s.Source.ReturnedLines, s.Source.TotalLines)
			}
		}
	}
	if len(d.Related) > 0 {
		fmt.Fprintln(w, "related:")
		for _, r := range d.Related {
			at := path(r.File)
			if r.Line > 0 {
				at = fmt.Sprintf("%s:%d", at, r.Line)
			}
			fmt.Fprintf(w, "  %s of %s: %s  %s  (%s)\n", r.Relation, r.Of, at, strings.TrimSpace(r.Name+" "+r.Detail), r.Evidence)
		}
	}
	if len(d.Files) > 0 {
		var fs []string
		for _, f := range d.Files {
			fs = append(fs, path(f.File))
		}
		fmt.Fprintf(w, "files: %s\n", strings.Join(fs, ", "))
	}
	fmt.Fprintf(w, "budget: %d of %d tokens", d.Budget.Used, d.Budget.Limit)
	if d.Budget.Truncated {
		fmt.Fprintf(w, " (truncated: %s)", droppedSummary(d.Budget.Dropped))
	}
	fmt.Fprintln(w)
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}

func matchNote(t taskTerm) string {
	if t.Matches == 0 {
		return "no match"
	}
	return fmt.Sprintf("%d %s", t.Matches, t.Best)
}

func countOrDash(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprint(*n)
}
