package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed related <sym>` (docs/DECISIONS.md D40): what to look at next to a
// symbol, small and ranked, every row saying which evidence put it there.

const (
	defaultRelatedLimit     = 20
	defaultRelatedPerSource = 5
	defaultRelatedHistory   = 500
	maxRelatedHistory       = 5000
	// bulkCommitFiles is how many files a commit may touch before it says nothing
	// about which of them belong together (a mass rename, a formatter run).
	bulkCommitFiles = 30
)

// The evidence a row can carry.
const (
	evSibling   = "sibling"
	evCall      = "call_hierarchy"
	evCoChange  = "co_change"
	evSimilar   = "similar_name"
	weightCall  = 4.0
	weightSib   = 2.0
	weightCo    = 2.0
	weightNames = 1.0
)

// relatedRow is one thing related to the subject. A symbol that several kinds of
// evidence agree on is one row with all of them.
type relatedRow struct {
	Evidence []string `json:"evidence"`
	ID       string   `json:"id,omitempty"`
	Name     string   `json:"name,omitempty"`
	// Kind is the symbol kind, or "file" for a co-changed file.
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line,omitempty"`
	// Detail says how it is related: "caller", "callee", "3 lines below",
	// "changed in 7 of the last 500 commits that touched this file", "name matches (prefix)".
	Detail string  `json:"detail,omitempty"`
	Score  float64 `json:"score"`

	order int
}

// relatedSource says what one kind of evidence found and what was shown of it.
type relatedSource struct {
	Evidence string `json:"evidence"`
	Found    int    `json:"found"`
	Note     string `json:"note,omitempty"`
}

// relatedData is the payload of `related`.
type relatedData struct {
	Root    string          `json:"root"`
	Subject subject         `json:"subject"`
	Sources []relatedSource `json:"sources"`
	Rows    []relatedRow    `json:"rows"`
	Count   int             `json:"count"`
	Total   int             `json:"total"`
	// Truncated says --limit cut the rows.
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	PerSource int  `json:"per_source"`
	// History is how many commits of the repository the co-change evidence read.
	History int `json:"history,omitempty"`
	// BulkSkipped is how many of them touched too many files to count.
	BulkSkipped int `json:"bulk_commits_ignored,omitempty"`
}

// relatedCommand implements `lightspeed related <loc>`.
func relatedCommand(e *env, c *command, args []string) int {
	var (
		perSource, history int
		fset               *flag.FlagSet
	)
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.IntVar(&perSource, "per-source", defaultRelatedPerSource, "at most this many rows from each kind of evidence (callers and callees each)")
		fs.IntVar(&history, "history", defaultRelatedHistory, fmt.Sprintf("how many recent commits the co-change evidence reads (at most %d)", maxRelatedHistory))
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "related", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if perSource < 1 {
		return e.usagef("related: --per-source must be at least 1 (got %d)", perSource)
	}
	if history < 1 || history > maxRelatedHistory {
		return e.usagef("related: --history must be between 1 and %d (got %d)", maxRelatedHistory, history)
	}
	limit := effectiveLimit(fset, common, defaultRelatedLimit)

	cs, err := openSubject(e, common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}
	defer cs.Close()
	ctx := e.base()
	data := relatedData{Root: cs.Root, Subject: cs.Subject, Rows: []relatedRow{}, Limit: limit, PerSource: perSource, History: 0}
	warnings := slicesClone(cs.Warnings)
	sub := cs.Subject

	rows := map[string]*relatedRow{}
	order := 0
	add := func(key string, r relatedRow, weight float64) {
		if have := rows[key]; have != nil {
			have.Evidence = append(have.Evidence, r.Evidence...)
			have.Score += weight
			if have.Detail != "" && r.Detail != "" {
				have.Detail += "; " + r.Detail
			} else if r.Detail != "" {
				have.Detail = r.Detail
			}
			return
		}
		r.Score, r.order = weight, order
		order++
		rows[key] = &r
	}
	rowKey := func(id, file string, line int) string {
		if id != "" {
			return id
		}
		return fmt.Sprintf("%s:%d", file, line)
	}
	isSubject := func(id, file string, line int) bool {
		return (id != "" && id == sub.ID) || (id == "" && file == sub.File && line == sub.Line)
	}

	run, err := startIndex(e, common, cs.Root)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()

	// 1. Siblings: the other symbols of the file, nearest first.
	var siblingIDs = map[string]bool{}
	if fs, err := run.Symbols(index.SymbolsQuery{Files: []string{sub.File}}); err != nil {
		warnings = append(warnings, "siblings unavailable: "+oneLine(err.Error()))
		data.Sources = append(data.Sources, relatedSource{Evidence: evSibling, Note: "unavailable"})
	} else {
		warnings = append(warnings, syncWarnings(fs.Report)...)
		var sibs []index.Symbol
		for _, f := range fs.Files {
			for _, s := range f.Symbols {
				if s.ID != sub.ID {
					sibs = append(sibs, s)
				}
			}
		}
		sort.SliceStable(sibs, func(i, j int) bool { return lineGap(sibs[i], sub.Line) < lineGap(sibs[j], sub.Line) })
		src := relatedSource{Evidence: evSibling, Found: len(sibs)}
		if len(fs.Files) == 0 {
			src.Note = "no symbol information for this file"
		}
		data.Sources = append(data.Sources, src)
		for i, s := range sibs {
			if i >= perSource {
				break
			}
			gap := lineGap(s, sub.Line)
			siblingIDs[s.ID] = true
			add(rowKey(s.ID, sub.File, s.Line), relatedRow{Evidence: []string{evSibling}, ID: s.ID, Name: s.Qualified, Kind: s.Kind,
				File: sub.File, Line: s.Line, Detail: fmt.Sprintf("in the same file, %d line(s) away", gap)}, weightSib+1/float64(1+gap))
		}
	}

	// 2. Callers and callees, one level, when the server has a call hierarchy.
	calls, callNote, callWarns := relatedCalls(ctx, cs, perSource)
	warnings = append(warnings, callWarns...)
	callSrc := relatedSource{Evidence: evCall, Note: callNote}
	for _, r := range calls {
		if isSubject(r.ID, r.File, r.Line) {
			continue
		}
		callSrc.Found++
		add(rowKey(r.ID, r.File, r.Line), r, weightCall)
	}
	data.Sources = append(data.Sources, callSrc)

	// 3. Co-changed files, from git.
	coSrc := relatedSource{Evidence: evCoChange}
	if g, gerr := openGitRepo(cs.Root); gerr != nil {
		coSrc.Note = gitDegraded(gerr)
		warnings = append(warnings, "co-change evidence unavailable: "+coSrc.Note)
	} else {
		co, read, bulk, note := coChanged(ctx, g, cs.Root, sub.File, history)
		data.History, data.BulkSkipped = read, bulk
		coSrc.Found, coSrc.Note = len(co), note
		maxN := 1
		if len(co) > 0 {
			maxN = co[0].n
		}
		for i, f := range co {
			if i >= perSource {
				break
			}
			add(f.path, relatedRow{Evidence: []string{evCoChange}, Kind: "file", File: f.path,
				Detail: fmt.Sprintf("changed in %d of the last %d commits that touched %s", f.n, f.of, sub.File)},
				weightCo*float64(f.n)/float64(maxN))
		}
	}
	data.Sources = append(data.Sources, coSrc)

	// 4. Similar names, from the index.
	simSrc := relatedSource{Evidence: evSimilar}
	if name := shortName(sub.Name); name == "" {
		simSrc.Note = "the position is not on a named symbol"
	} else {
		var out index.SearchOutcome
		err := run.do(daemon.IndexRequest{Op: daemon.IndexOpSearch, Search: &index.SearchQuery{Text: name, Limit: perSource*3 + 5}}, &out)
		if err != nil {
			simSrc.Note = "unavailable"
			warnings = append(warnings, "similar-name evidence unavailable: "+oneLine(err.Error()))
		} else {
			shown := 0
			for _, h := range out.Hits {
				s := h.Symbol
				if s.ID == sub.ID || (s.ID != "" && siblingIDs[s.ID]) || h.Match == "fuzzy" {
					continue
				}
				simSrc.Found++
				if shown >= perSource {
					continue
				}
				shown++
				bonus := map[string]float64{"exact": 0.5, "prefix": 0.25}[h.Match]
				add(rowKey(s.ID, h.File, s.Line), relatedRow{Evidence: []string{evSimilar}, ID: s.ID, Name: s.Qualified, Kind: s.Kind,
					File: h.File, Line: s.Line, Detail: "name matches (" + h.Match + ")"}, weightNames+bonus)
			}
		}
	}
	data.Sources = append(data.Sources, simSrc)

	all := make([]relatedRow, 0, len(rows))
	for _, r := range rows {
		r.Score = roundScore(r.Score)
		all = append(all, *r)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].order < all[j].order
	})
	var truncated bool
	data.Rows, truncated, data.Total = capRows(all, limit)
	data.Truncated = truncated
	if truncated {
		warnings = append(warnings, truncationWarning("related items", len(data.Rows), data.Total, "raise --limit, or lower --per-source to see fewer of each kind"))
	}
	data.Count = len(data.Rows)
	exit := ExitOK
	if len(all) == 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		for _, r := range data.Rows {
			what := r.ID
			if what == "" {
				what = "(" + r.Kind + ") " + r.Name
			}
			loc := r.File
			if r.Line > 0 {
				loc = fmt.Sprintf("%s:%d", r.File, r.Line)
			}
			fmt.Fprintf(e.stdout, "%s: [%s] %s  %s\n", loc, strings.Join(r.Evidence, ","), strings.TrimSpace(what), r.Detail)
		}
		for _, w := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", w)
		}
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

// lineGap is how far a symbol is from a line: 0 if the line is inside it.
func lineGap(s index.Symbol, line int) int {
	switch {
	case line < s.Line:
		return s.Line - line
	case line > s.EndLine:
		return line - s.EndLine
	}
	return 0
}

// shortName is the last segment of a qualified name: `Server.Handle` is `Handle`.
func shortName(qualified string) string {
	if i := strings.LastIndexAny(qualified, "."); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
}

// relatedCalls asks for the direct callers and callees of the subject. It is
// capability-guarded: a server with no call hierarchy is a note and a warning,
// not an error, because the other evidence stands without it.
func relatedCalls(ctx context.Context, cs *composed, perSource int) (rows []relatedRow, note string, warnings []string) {
	s := cs.Q.session
	if !s.lsp.Supports(methodPrepareCallHierarchy) {
		note = "the server does not advertise a call hierarchy"
		return nil, note, []string{"callers and callees unavailable: " + note}
	}
	qctx, cancel := cs.Q.queryContext()
	defer cancel()
	res, err := s.query(qctx, methodPrepareCallHierarchy, textDocumentPosition(cs.Q.doc.URI, cs.Q.position))
	if err != nil {
		var coded interface{ ExitCode() int }
		if errors.As(err, &coded) && coded.ExitCode() == render.ExitNotReady {
			// Not authoritative: better no callers than a wrong empty list.
			return nil, "not ready", []string{"callers and callees unavailable: " + oneLine(err.Error())}
		}
		return nil, "unavailable", []string{"callers and callees unavailable: " + oneLine(err.Error())}
	}
	items, err := decodeCallHierarchyItems(res.Result)
	if err != nil || len(items) == 0 {
		return nil, "the server reports no callable symbol at this position", nil
	}
	ids := newIDIndex(cs.E, s, cs.Root)
	for _, dir := range []struct{ method, label string }{{methodIncomingCalls, "caller"}, {methodOutgoingCalls, "callee"}} {
		qctx, cancel := cs.Q.queryContext()
		res, err := s.query(qctx, dir.method, map[string]any{"item": items[0].Raw})
		cancel()
		if err != nil {
			warnings = append(warnings, dir.label+"s unavailable: "+oneLine(err.Error()))
			continue
		}
		rels, err := decodeCalls(res.Result, dir.method)
		if err != nil {
			warnings = append(warnings, dir.label+"s unavailable: "+oneLine(err.Error()))
			continue
		}
		n := 0
		for _, rel := range rels {
			l, ok := cs.Locus(protocol.Location{URI: rel.item.URI, Range: rel.item.SelectionRange})
			if !ok {
				continue // outside the workspace: nothing to point at
			}
			n++
			if n > perSource {
				continue // capped per direction
			}
			rows = append(rows, relatedRow{Evidence: []string{evCall}, ID: ids.idAt(rel.item.URI, rel.item.SelectionRange),
				Name: rel.item.Name, Kind: symbolKindName(rel.item.Kind), File: l.File, Line: l.Line, Detail: dir.label})
		}
	}
	warnings = append(warnings, ids.summary()...)
	return rows, "", warnings
}

// A coChange is a file and the number of the subject's commits it shared.
type coChange struct {
	path string
	n    int
	// of is how many commits touched the subject's file.
	of int
}

// coChanged reads the last `history` commits of the repository and counts, for
// each other file, the commits that touched it together with file. Commits that
// touch more than bulkCommitFiles files are ignored (and counted): a formatter
// run says nothing about which files belong together. Files that no longer
// exist are left out.
func coChanged(ctx context.Context, g *gitRepo, root, file string, history int) (out []coChange, read, bulk int, note string) {
	raw, err := g.run(ctx, gitHistoryTimeout, "log", "-n", fmt.Sprint(history), "-z", "--name-only", "--no-renames", "--no-merges", "--relative", logFormat, "--", ".")
	switch {
	case errors.Is(err, errGitOutputCapped):
		note = "history was cut at the size cap"
	case err != nil:
		if !g.hasHead(ctx) {
			return nil, 0, 0, "the repository has no commit yet"
		}
		return nil, 0, 0, "git log failed: " + oneLine(err.Error())
	}
	commits := parseLog(raw, false, false)
	read = len(commits)
	counts := map[string]int{}
	of := 0
	for _, c := range commits {
		has := false
		for _, f := range c.Files {
			if f.Path == file {
				has = true
			}
		}
		if !has {
			continue
		}
		if len(c.Files) > bulkCommitFiles {
			bulk++
			continue
		}
		of++
		for _, f := range c.Files {
			if f.Path != file {
				counts[f.Path]++
			}
		}
	}
	for p, n := range counts {
		if info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(p))); err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, coChange{path: p, n: n, of: of})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].n != out[j].n {
			return out[i].n > out[j].n
		}
		return out[i].path < out[j].path
	})
	if of == 0 && note == "" {
		note = fmt.Sprintf("no commit among the last %d touches %s", read, file)
	}
	return out, read, bulk, note
}
