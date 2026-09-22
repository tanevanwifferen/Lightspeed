package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// `lightspeed churn` and `lightspeed hotspots` (docs/DECISIONS.md D40): how much
// a file, and a symbol in it, has been changed in a window of history; and the
// ranking of that by how big the thing is.

const (
	defaultChurnSince = "90 days"
	defaultChurnLimit = 20
	// defaultSymbolFiles is how many of the most-changed files have their history
	// attributed to symbols, and maxSymbolFiles the ceiling: attribution reads the
	// patches of the window, which is the expensive part.
	defaultSymbolFiles = 15
	maxSymbolFiles     = 60
	// churnLOCCap is the largest file whose lines are counted for a hotspot score.
	churnLOCCap = 4 << 20
)

// hotspotFormula is stated in every hotspots answer, so that a score is never a
// number without its meaning.
const (
	hotspotFileFormula   = "score = commits × (1 + ln(1 + loc)), where loc is the file's current line count"
	hotspotSymbolFormula = "score = commits × (1 + ln(1 + lines)), where lines is the symbol's current line count"
)

// hotspotNonCode are the languages `hotspots` leaves out unless --all: prose,
// data and configuration. A README or a decision log changes with every commit
// and is long, so churn × size ranks it above every source file, and it is not
// what a ranking of code to look at is for. A file of no known language (a
// lock file, a LICENSE) is left out with them.
var hotspotNonCode = map[string]bool{
	"": true, "markdown": true, "mdx": true, "restructuredtext": true, "latex": true,
	"json": true, "jsonc": true, "yaml": true, "toml": true, "xml": true,
	"gomod": true, "gosum": true, "gowork": true,
}

// sampleList names the first n of a list, and says when there are more.
func sampleList(items []string, n int) string {
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + ", …"
}

// isHotspotCode reports whether hotspots ranks the file by default.
func isHotspotCode(rel string) bool { return !hotspotNonCode[router.LanguageID(rel)] }

// churnFile is one file's history in the window.
type churnFile struct {
	File    string `json:"file"`
	Commits int    `json:"commits"`
	Authors int    `json:"authors"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	// Last is the author date of the most recent commit in the window.
	Last string `json:"last,omitempty"`
	// Deleted marks a file that is in the window's history and no longer on disk.
	Deleted bool `json:"deleted,omitempty"`
	// LOC, Symbols and Score are hotspots' figures.
	LOC     int     `json:"loc,omitempty"`
	Symbols int     `json:"symbols,omitempty"`
	Score   float64 `json:"score,omitempty"`
}

// churnSymbol is one symbol's share of the history.
type churnSymbol struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	File    string  `json:"file"`
	Line    int     `json:"line"`
	EndLine int     `json:"end_line"`
	Commits int     `json:"commits"`
	Authors int     `json:"authors"`
	Added   int     `json:"added"`
	Removed int     `json:"removed"`
	Score   float64 `json:"score,omitempty"`
}

// churnData is the payload of `churn` and `hotspots`.
type churnData struct {
	Root  string     `json:"root"`
	Git   changedGit `json:"git"`
	Since string     `json:"since"`
	Path  string     `json:"path,omitempty"`
	// Commits is how many commits the window holds (merges excluded). A commit
	// that touches hundreds of files (a mass reformat) counts like any other.
	Commits int `json:"commits"`
	// Formula is the ranking rule (hotspots).
	Formula string        `json:"formula,omitempty"`
	Files   []churnFile   `json:"files"`
	Symbols []churnSymbol `json:"symbols,omitempty"`
	Count   int           `json:"count"`
	// Total is how many files there were before --limit; SymbolsTotal likewise.
	Total        int  `json:"total"`
	SymbolsTotal int  `json:"symbols_total,omitempty"`
	Truncated    bool `json:"truncated"`
	Limit        int  `json:"limit,omitempty"`
	// NonCode is how many changed files hotspots left out because they are docs,
	// data or configuration (--all ranks them).
	NonCode int `json:"non_code_left_out,omitempty"`
	// Approximate says how the symbol figures were made.
	Approximate string `json:"approximate,omitempty"`
}

const symbolApproximation = "per-symbol figures attribute each historical hunk to the symbol whose current lines it maps to, carrying line numbers forward through later commits and the uncommitted diff; a hunk whose lines were later rewritten or removed is attributed to the place they were in (counted in the warnings), so they are approximations"

// churnCommand implements `lightspeed churn [path]`.
func churnCommand(e *env, c *command, args []string) int { return churnOrHotspots(e, c, args, false) }

// hotspotsCommand implements `lightspeed hotspots`.
func hotspotsCommand(e *env, c *command, args []string) int { return churnOrHotspots(e, c, args, true) }

func churnOrHotspots(e *env, c *command, args []string, hot bool) int {
	var (
		since, rootFlag, pathFlag string
		symbolFiles               int
		allFiles                  bool
		fset                      *flag.FlagSet
	)
	positional := 1
	if hot {
		positional = 0
	}
	common, pos, err := parseFlagsRange(e, c, args, 0, positional, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&since, "since", defaultChurnSince, "the window: 30d, 2w, 12h, 6m, 1y, or anything git reads as a date: '30 days', '2 weeks ago', 2026-01-31")
		fs.IntVar(&symbolFiles, "symbol-files", defaultSymbolFiles, fmt.Sprintf("attribute history to symbols for this many of the most-changed files (0 for files only, at most %d)", maxSymbolFiles))
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
		if hot {
			fs.StringVar(&pathFlag, "path", "", "only files under this directory, or this file, inside the workspace")
			fs.BoolVar(&allFiles, "all", false, "rank every file, not only code: docs (markdown), data and config (json, yaml, toml, lock files) too")
		}
	})
	if err != nil {
		return e.flagError(err)
	}
	if !hot && len(pos) == 1 {
		pathFlag = pos[0]
	}
	format, err := managementFormat(common, c.Name, e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if err := validSince(since); err != nil {
		return e.fail(err)
	}
	if symbolFiles < 0 || symbolFiles > maxSymbolFiles {
		return e.usagef("%s: --symbol-files must be between 0 and %d (got %d)", c.Name, maxSymbolFiles, symbolFiles)
	}
	limit := effectiveLimit(fset, common, defaultChurnLimit)

	root, err := commandWorkspace(rootFlag)
	if err != nil {
		return e.fail(err)
	}
	scope, err := scopeRel(root, pathFlag)
	if err != nil {
		return e.fail(err)
	}
	data := churnData{Root: root, Since: since, Path: scope, Files: []churnFile{}, Limit: limit}
	if hot {
		data.Formula = hotspotFileFormula + "; " + hotspotSymbolFormula
	}

	g, gerr := openGitRepo(root)
	if gerr != nil {
		data.Git.Reason = gitDegraded(gerr)
		return e.finishChurn(common, format, data, []string{data.Git.Reason}, ExitOK)
	}
	data.Git.Repo = true
	ctx := e.base()

	files, commits, warnings, err := collectChurn(ctx, g, since, scope)
	if err != nil {
		return e.fail(err)
	}
	data.Commits = commits
	if len(files) == 0 {
		warnings = append(warnings, fmt.Sprintf("no commit in the last %s touches %s", since, firstNonEmpty(scope, "the workspace")))
		return e.finishChurn(common, format, data, warnings, ExitProblems)
	}

	// Files still on disk carry their size; the ones the window deleted are
	// history and, for a ranking of what to look at, noise.
	for i := range files {
		abs := filepath.Join(root, filepath.FromSlash(files[i].File))
		if info, err := os.Lstat(abs); err != nil || !info.Mode().IsRegular() {
			files[i].Deleted = true
		}
	}
	if hot {
		files = slicesDeleteDeleted(files)
		if !allFiles {
			var left []string
			files = slices.DeleteFunc(files, func(f churnFile) bool {
				if isHotspotCode(f.File) {
					return false
				}
				left = append(left, f.File)
				return true
			})
			if len(left) > 0 {
				slices.Sort(left)
				warnings = append(warnings, fmt.Sprintf("%d changed files that are not code were left out of the ranking (%s); --all ranks them too",
					len(left), sampleList(left, 3)))
			}
			data.NonCode = len(left)
		}
		for i := range files {
			files[i].LOC = countLines(filepath.Join(root, filepath.FromSlash(files[i].File)))
			files[i].Score = hotScore(files[i].Commits, files[i].LOC)
		}
		sort.SliceStable(files, func(i, j int) bool {
			if files[i].Score != files[j].Score {
				return files[i].Score > files[j].Score
			}
			return files[i].File < files[j].File
		})
	} else {
		sort.SliceStable(files, func(i, j int) bool {
			if files[i].Commits != files[j].Commits {
				return files[i].Commits > files[j].Commits
			}
			if a, b := files[i].Added+files[i].Removed, files[j].Added+files[j].Removed; a != b {
				return a > b
			}
			return files[i].File < files[j].File
		})
	}
	if len(files) == 0 {
		if data.NonCode > 0 {
			return e.finishChurn(common, format, data, warnings, ExitProblems)
		}
		warnings = append(warnings, "every file changed in the window has since been deleted")
		return e.finishChurn(common, format, data, warnings, ExitProblems)
	}

	var truncated bool
	all := files
	files, truncated, data.Total = capRows(files, limit)
	data.Truncated = truncated
	data.Files = files
	if truncated {
		warnings = append(warnings, truncationWarning("files", len(files), data.Total, "narrow it with a path, or raise --limit"))
	}

	// Symbols: one index query and one patch log over the top files.
	if n := min(symbolFiles, len(all)); n > 0 {
		top := all[:n]
		syms, warns := symbolChurn(ctx, e, common, g, rootFlag, since, top, hot)
		warnings = append(warnings, warns...)
		data.SymbolsTotal = len(syms)
		if limit > 0 && len(syms) > limit {
			syms = syms[:limit]
			data.Truncated = true
			warnings = append(warnings, truncationWarning("symbols", len(syms), data.SymbolsTotal, "narrow it with a path, or raise --limit"))
		}
		data.Symbols = syms
		if len(syms) > 0 {
			data.Approximate = symbolApproximation
		}
		if hot && len(data.Files) > 0 {
			fillSymbolCounts(ctx, e, common, rootFlag, data.Files, &warnings)
		}
	}
	return e.finishChurn(common, format, data, warnings, ExitOK)
}

func slicesDeleteDeleted(files []churnFile) []churnFile {
	out := files[:0]
	for _, f := range files {
		if !f.Deleted {
			out = append(out, f)
		}
	}
	return out
}

func hotScore(commits, size int) float64 {
	return roundScore(float64(commits) * (1 + math.Log(1+float64(size))))
}

// countLines counts a file's lines, 0 for one that is unreadable, binary or over
// the cap.
func countLines(path string) int {
	info, err := os.Stat(path)
	if err != nil || info.Size() > churnLOCCap {
		return 0
	}
	b, err := os.ReadFile(path)
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return 0
	}
	n := bytes.Count(b, []byte{'\n'})
	if len(b) > 0 && b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func (e *env) finishChurn(common *commonFlags, format render.Format, data churnData, warnings []string, exit int) int {
	data.Count = len(data.Files)
	if data.Files == nil {
		data.Files = []churnFile{}
	}
	if format == render.FormatText {
		for _, f := range data.Files {
			fmt.Fprintf(e.stdout, "%s: %d commits, %d authors, +%d -%d", f.File, f.Commits, f.Authors, f.Added, f.Removed)
			if f.Score > 0 {
				fmt.Fprintf(e.stdout, ", loc %d, score %.2f", f.LOC, f.Score)
			}
			fmt.Fprintln(e.stdout)
		}
		for _, s := range data.Symbols {
			fmt.Fprintf(e.stdout, "%s:%d: %s: %d commits, %d authors, +%d -%d\n", s.File, s.Line, s.ID, s.Commits, s.Authors, s.Added, s.Removed)
		}
		for _, w := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", w)
		}
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// collectChurn reads the window's history: per file, commits, authors and lines.
// Merges are excluded (their changes are their parents'), renames are not
// followed (a renamed file starts a new history), and paths are the workspace's.
func collectChurn(ctx context.Context, g *gitRepo, since, scope string) ([]churnFile, int, []string, error) {
	args := []string{"log", "-z", "--numstat", "--no-renames", "--no-merges", "--relative", "--since=" + gitSince(since), logFormat, "--", pathspec(scope)}
	out, err := g.run(ctx, gitHistoryTimeout, args...)
	var warnings []string
	switch {
	case errors.Is(err, errGitOutputCapped):
		warnings = append(warnings, "the history was larger than the cap and was cut: the oldest commits of the window are missing")
	case err != nil:
		if !g.hasHead(ctx) {
			return nil, 0, []string{"the repository has no commit yet"}, nil
		}
		return nil, 0, nil, render.Errorf(render.CodeIOError, "git log: %v", err)
	}
	commits := parseLog(out, true, false)
	type acc struct {
		commits int
		authors map[string]bool
		added   int
		removed int
		last    string
	}
	byFile := map[string]*acc{}
	for _, c := range commits {
		for _, f := range c.Files {
			a := byFile[f.Path]
			if a == nil {
				a = &acc{authors: map[string]bool{}, last: c.Date}
				byFile[f.Path] = a
			}
			a.commits++
			a.authors[c.Author] = true
			a.added += f.Added
			a.removed += f.Removed
		}
	}
	files := make([]churnFile, 0, len(byFile))
	for p, a := range byFile {
		files = append(files, churnFile{File: p, Commits: a.commits, Authors: len(a.authors), Added: a.added, Removed: a.removed, Last: a.last})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	return files, len(commits), warnings, nil
}

// pathspec is a workspace-relative scope as a literal git pathspec; "" is the
// whole workspace (git's `.` is the working directory, which is the workspace).
func pathspec(scope string) string {
	if scope == "" {
		return "."
	}
	return ":(literal)" + scope
}

// symbolChurn attributes the history of the top files to their current symbols.
func symbolChurn(ctx context.Context, e *env, common *commonFlags, g *gitRepo, rootFlag, since string, top []churnFile, hot bool) ([]churnSymbol, []string) {
	var warnings []string
	var paths []string
	for _, f := range top {
		if !f.Deleted {
			paths = append(paths, f.File)
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return nil, []string{"per-symbol figures unavailable: " + oneLine(err.Error())}
	}
	defer run.close()
	idx, err := run.Symbols(index.SymbolsQuery{Files: paths})
	if err != nil {
		return nil, []string{"per-symbol figures unavailable: " + oneLine(err.Error())}
	}
	warnings = append(warnings, syncWarnings(idx.Report)...)
	current := map[string][]index.Symbol{}
	for _, f := range idx.Files {
		current[f.File] = f.Symbols
	}
	if len(current) == 0 {
		return nil, append(warnings, "no symbol information for the most-changed files (no server, or not indexed): files only")
	}

	spec := make([]string, len(paths))
	for i, p := range paths {
		spec[i] = ":(literal)" + p
	}
	// The shifters start with the uncommitted diff, the newest change of all.
	shift := map[string]*shifter{}
	for _, p := range paths {
		shift[p] = &shifter{}
	}
	if g.hasHead(ctx) {
		if out, err := g.run(ctx, gitHistoryTimeout, append([]string{"diff", "-U0", "--no-prefix", "--no-ext-diff", "--relative", "HEAD", "--"}, spec...)...); err == nil {
			for _, fd := range parseDiff(out) {
				if s := shift[fd.NewPath]; s != nil {
					s.push(fd.Hunks)
				}
			}
		}
	}
	args := append([]string{"log", "-p", "-U0", "--no-prefix", "--no-renames", "--no-merges", "--no-ext-diff", "--relative", "--since=" + gitSince(since), logFormat, "--"}, spec...)
	out, err := g.run(ctx, gitHistoryTimeout, args...)
	if errors.Is(err, errGitOutputCapped) {
		warnings = append(warnings, "the patches of the window were larger than the cap and were cut: the oldest commits are missing from the per-symbol figures")
	} else if err != nil {
		return nil, append(warnings, "per-symbol figures unavailable: git log -p: "+oneLine(err.Error()))
	}

	type acc struct {
		sym     index.Symbol
		file    string
		commits int
		authors map[string]bool
		added   int
		removed int
	}
	accs := map[string]*acc{}
	rewritten := 0
	for _, c := range parseLog(out, false, true) {
		touchedNow := map[string]bool{}
		for _, fd := range parseDiff(c.Diff) {
			syms := current[fd.NewPath]
			s := shift[fd.NewPath]
			if s == nil {
				continue
			}
			for _, h := range fd.Hunks {
				at, end := h.NewStart, h.NewStart
				if lo, hi, ok := h.newRange(); ok {
					var l1, l2 bool
					at, l1 = s.carry(lo)
					end, l2 = s.carry(hi)
					if l1 && l2 {
						rewritten++
					}
					if end < at {
						end = at
					}
				} else {
					var l bool
					at, l = s.carry(h.NewStart)
					end = at
					if l {
						rewritten++
					}
				}
				pseudo := hunk{NewStart: at, NewCount: end - at + 1}
				if h.NewCount == 0 {
					pseudo = hunk{NewStart: at}
				}
				removedGiven := false
				for _, sym := range syms {
					if !touchesSymbol(pseudo, sym) || insideChild(pseudo, sym, syms) {
						continue
					}
					a := accs[sym.ID]
					if a == nil {
						a = &acc{sym: sym, file: fd.NewPath, authors: map[string]bool{}}
						accs[sym.ID] = a
					}
					if !touchedNow[sym.ID] {
						touchedNow[sym.ID] = true
						a.commits++
					}
					a.authors[c.Author] = true
					if lo, hi, ok := pseudo.newRange(); ok {
						a.added += min(hi, sym.EndLine) - max(lo, sym.Line) + 1
					}
					if !removedGiven {
						a.removed += h.OldCount
						removedGiven = true
					}
				}
			}
			s.push(fd.Hunks)
		}
	}
	if rewritten > 0 {
		warnings = append(warnings, fmt.Sprintf("%d historical hunk(s) touched lines that later commits rewrote or removed; they are attributed to the symbol that holds the place those lines were in", rewritten))
	}
	rows := make([]churnSymbol, 0, len(accs))
	for _, a := range accs {
		r := churnSymbol{ID: a.sym.ID, Name: a.sym.Qualified, Kind: a.sym.Kind, File: a.file, Line: a.sym.Line, EndLine: a.sym.EndLine,
			Commits: a.commits, Authors: len(a.authors), Added: a.added, Removed: a.removed}
		if hot {
			r.Score = hotScore(r.Commits, a.sym.EndLine-a.sym.Line+1)
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if hot && a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Commits != b.Commits {
			return a.Commits > b.Commits
		}
		if x, y := a.Added+a.Removed, b.Added+b.Removed; x != y {
			return x > y
		}
		return a.ID < b.ID
	})
	return rows, warnings
}

// fillSymbolCounts adds each listed file's symbol count, the "symbol-count" half
// of a hotspot's size, from the index.
func fillSymbolCounts(ctx context.Context, e *env, common *commonFlags, rootFlag string, files []churnFile, warnings *[]string) {
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return
	}
	defer run.close()
	paths := make([]string, len(files))
	for i, f := range files {
		paths[i] = f.File
	}
	out, err := run.Symbols(index.SymbolsQuery{Files: paths})
	if err != nil {
		*warnings = append(*warnings, "symbol counts unavailable: "+oneLine(err.Error()))
		return
	}
	count := map[string]int{}
	for _, f := range out.Files {
		count[f.File] = len(f.Symbols)
	}
	for i := range files {
		files[i].Symbols = count[files[i].File]
	}
}
