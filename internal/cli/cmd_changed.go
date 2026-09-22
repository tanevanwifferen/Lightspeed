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

	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed changed_symbols` (docs/DECISIONS.md D40): which symbols a diff
// touches. The hunks of `git diff -U0` are mapped onto the symbol ranges the
// index holds for the new side — revalidated against the disk, so the hunks and
// the ranges describe the same bytes — and onto an outline of the old blob for
// the old side, which is what makes a *removed* symbol nameable at all.

const (
	// emptyTree is git's well-known empty tree, the base of a repository with no
	// commit.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
	// changedFileCap bounds how many changed files are mapped in one call, and
	// changedOldCap how many of them have their old version outlined by a server.
	changedFileCap = 200
	changedOldCap  = 40
	// changedHunkCap bounds the hunks listed on one row.
	changedHunkCap = 8
	// defaultChangedLimit is the row limit when --limit is not given.
	defaultChangedLimit = 100
)

// changedGit says which diff the answer is about, or why there is none.
type changedGit struct {
	Repo    bool   `json:"repo"`
	Reason  string `json:"reason,omitempty"`
	Base    string `json:"base,omitempty"`
	BaseSHA string `json:"base_sha,omitempty"`
	Staged  bool   `json:"staged,omitempty"`
}

// changedRow is one changed symbol, or one changed file with no symbol to name.
type changedRow struct {
	// Status is added, modified or removed for a symbol; for a file-level row it
	// is also renamed.
	Status string `json:"status"`
	// Kind is "symbol", or "file" for a row that names no symbol: changes outside
	// every declaration, a language with no outline, a binary file, a rename.
	Kind       string `json:"kind"`
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	SymbolKind string `json:"symbol_kind,omitempty"`
	File       string `json:"file"`
	Line       int    `json:"line,omitempty"`
	EndLine    int    `json:"end_line,omitempty"`
	// OldFile, OldLine and OldEndLine say where a removed symbol was, or where a
	// renamed file came from.
	OldFile    string `json:"old_file,omitempty"`
	OldLine    int    `json:"old_line,omitempty"`
	OldEndLine int    `json:"old_end_line,omitempty"`
	// Hunks are the diff hunks behind the row (at most changedHunkCap).
	Hunks []hunk `json:"hunks,omitempty"`
	// Evidence says where the row comes from: "index+old_outline" (new side from
	// the index, old side from an outline of the old blob) or "index+hunks" (new
	// side only; a removal is invisible).
	Evidence string `json:"evidence"`
	Note     string `json:"note,omitempty"`
}

type changedSummary struct {
	Files    int `json:"files"`
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Removed  int `json:"removed"`
	// FileLevel counts the rows that name a file and no symbol.
	FileLevel int `json:"file_level"`
	// OldOutlined counts the files whose old version was outlined; OldUnavailable
	// the changed files where it was not (no server, a cap, a failure), so that
	// removed symbols there are not visible.
	OldOutlined    int `json:"old_outlined"`
	OldUnavailable int `json:"old_unavailable"`
}

// changedData is the payload of `changed_symbols`.
type changedData struct {
	Root    string         `json:"root"`
	Git     changedGit     `json:"git"`
	Summary changedSummary `json:"summary"`
	Rows    []changedRow   `json:"rows"`
	Count   int            `json:"count"`
	Total   int            `json:"total"`
	// Truncated says --limit cut the rows; Total is how many there were.
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
}

// changedSymbolsCommand implements `lightspeed changed_symbols`.
func changedSymbolsCommand(e *env, c *command, args []string) int {
	var (
		base, pathFlag, rootFlag string
		staged                   bool
		fset                     *flag.FlagSet
	)
	common, _, err := parseFlagsRange(e, c, args, 0, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&base, "base", "", "compare against this revision instead of HEAD (the working tree, or the index with --staged, against it)")
		fs.BoolVar(&staged, "staged", false, "only the staged changes: the index against the base")
		fs.StringVar(&pathFlag, "path", "", "only changes under this directory, or this file, inside the workspace")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "changed_symbols", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if base != "" {
		if err := validRev(base); err != nil {
			return e.fail(err)
		}
	}
	limit := effectiveLimit(fset, common, defaultChangedLimit)

	root, err := commandWorkspace(rootFlag)
	if err != nil {
		return e.fail(err)
	}
	scope, err := scopeRel(root, pathFlag)
	if err != nil {
		return e.fail(err)
	}
	data := changedData{Root: root, Rows: []changedRow{}, Limit: limit, Git: changedGit{Staged: staged}}

	g, gerr := openGitRepo(root)
	if gerr != nil {
		data.Git.Reason = gitDegraded(gerr)
		return e.finishChanged(common, format, data, []string{data.Git.Reason}, ExitOK)
	}
	data.Git.Repo = true
	ctx := e.base()

	rev, warnings, err := changedBase(ctx, g, base)
	if err != nil {
		return e.fail(err)
	}
	data.Git.Base = base
	if data.Git.Base == "" {
		data.Git.Base = "HEAD"
	}
	data.Git.BaseSHA = rev

	files, warns, err := changedFiles(ctx, g, rev, staged, scope)
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, warns...)
	data.Summary.Files = len(files)
	if len(files) > changedFileCap {
		warnings = append(warnings, fmt.Sprintf("%d files changed; only the first %d are mapped onto symbols (narrow it with --path)", len(files), changedFileCap))
		files = files[:changedFileCap]
	}
	if len(files) == 0 {
		return e.finishChanged(common, format, data, warnings, ExitProblems)
	}

	// The new side: the index, revalidated, so its ranges are the file's now.
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	var newPaths []string
	for _, f := range files {
		if f.NewPath != "" && !f.Binary {
			newPaths = append(newPaths, f.NewPath)
		}
	}
	bySymbols := map[string]*index.FileSymbols{}
	if len(newPaths) > 0 {
		out, err := run.Symbols(index.SymbolsQuery{Files: newPaths})
		if err != nil {
			return e.fail(err)
		}
		warnings = append(warnings, syncWarnings(out.Report)...)
		for i := range out.Files {
			bySymbols[out.Files[i].File] = &out.Files[i]
		}
	}
	if staged {
		warnings = append(warnings, unstagedOverlap(ctx, g, newPaths)...)
	}

	// The old side, where a server can outline it.
	old := &oldOutliner{e: e, common: common, root: root, g: g, rev: rev, sessions: map[string]*session{}}
	defer old.close()
	var rows []changedRow
	for _, f := range files {
		var oldSyms []index.Symbol
		oldKnown := false
		// A file added has no old side; a pure rename or a binary file has
		// nothing to compare; everything else is outlined, up to the cap.
		if needsOld := f.Status != "A" && !f.Binary && (f.Status == "D" || len(f.Hunks) > 0); needsOld {
			if old.attempted < changedOldCap {
				oldSyms, oldKnown = old.outline(ctx, f)
			}
			if oldKnown {
				data.Summary.OldOutlined++
			} else {
				data.Summary.OldUnavailable++
			}
		}
		newFS := bySymbols[f.NewPath]
		rows = append(rows, mapFileChange(f, newFS, oldSyms, oldKnown)...)
	}
	warnings = append(warnings, old.summary()...)
	if data.Summary.OldUnavailable > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"the old version of %d changed file(s) could not be outlined, so a symbol removed from them is not listed; the hunks are (rows with kind \"file\", or the modified symbol around the gap)",
			data.Summary.OldUnavailable))
	}

	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rowLine(rows[i]) < rowLine(rows[j])
	})
	for _, r := range rows {
		switch {
		case r.Kind == "file":
			data.Summary.FileLevel++
		case r.Status == "added":
			data.Summary.Added++
		case r.Status == "modified":
			data.Summary.Modified++
		case r.Status == "removed":
			data.Summary.Removed++
		}
	}
	var truncated bool
	data.Rows, truncated, data.Total = capRows(rows, limit)
	data.Truncated = truncated
	if truncated {
		warnings = append(warnings, truncationWarning("changed symbols", len(data.Rows), data.Total, "narrow it with --path, or raise --limit"))
	}
	exit := ExitOK
	if len(rows) == 0 {
		exit = ExitProblems
	}
	return e.finishChanged(common, format, data, warnings, exit)
}

func rowLine(r changedRow) int {
	if r.Line > 0 {
		return r.Line
	}
	return r.OldLine
}

func (e *env) finishChanged(common *commonFlags, format render.Format, data changedData, warnings []string, exit int) int {
	data.Count = len(data.Rows)
	if data.Rows == nil {
		data.Rows = []changedRow{}
	}
	if format == render.FormatText {
		writeChangedText(e, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

func writeChangedText(e *env, d changedData, warnings []string) {
	for _, r := range d.Rows {
		loc := r.File
		switch {
		case r.Line > 0:
			loc = fmt.Sprintf("%s:%d", r.File, r.Line)
		case r.OldLine > 0:
			loc = fmt.Sprintf("%s:%d", r.OldFile, r.OldLine)
		}
		what := r.ID
		if what == "" {
			what = "(" + r.Kind + ")"
			if r.Note != "" {
				what += " " + r.Note
			}
		}
		fmt.Fprintf(e.stdout, "%s: %s %s\n", loc, r.Status, what)
	}
	for _, w := range warnings {
		fmt.Fprintf(e.stdout, "# %s\n", w)
	}
}

// changedBase resolves the base of the diff to a revision git can be asked for:
// the given one, HEAD, or — in a repository with no commit — the empty tree.
func changedBase(ctx context.Context, g *gitRepo, base string) (string, []string, error) {
	if base != "" {
		sha, err := g.verifyRev(ctx, base)
		return sha, nil, err
	}
	if g.hasHead(ctx) {
		sha, err := g.verifyRev(ctx, "HEAD")
		return sha, nil, err
	}
	return emptyTree, []string{"the repository has no commit yet: everything is compared with the empty tree"}, nil
}

// changedFiles lists what changed against rev: the tracked files from `git
// diff`, and, unless staged, the untracked files (which have no diff, and are
// entirely new).
func changedFiles(ctx context.Context, g *gitRepo, rev string, staged bool, scope string) ([]fileDiff, []string, error) {
	spec := []string{"--"}
	if scope != "" {
		spec = append(spec, ":(literal)"+scope)
	}
	args := []string{"diff", "-U0", "--no-prefix", "-M", "--no-ext-diff", "--relative"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, rev)
	args = append(args, spec...)
	out, err := g.run(ctx, gitHistoryTimeout, args...)
	var warnings []string
	if errors.Is(err, errGitOutputCapped) {
		warnings = append(warnings, "the diff was larger than the cap and was cut; narrow it with --path")
	} else if err != nil {
		return nil, nil, render.Errorf(render.CodeIOError, "git diff: %v", err)
	}
	files := parseDiff(out)
	if !staged {
		ul, err := g.run(ctx, gitTimeout, append([]string{"ls-files", "--others", "--exclude-standard", "-z"}, spec...)...)
		if err == nil {
			seen := map[string]bool{}
			for _, f := range files {
				seen[f.NewPath] = true
			}
			for _, p := range strings.Split(string(ul), "\x00") {
				if p != "" && !seen[p] {
					files = append(files, fileDiff{NewPath: p, Status: "A"})
				}
			}
		}
	}
	sort.SliceStable(files, func(i, j int) bool {
		return firstNonEmpty(files[i].NewPath, files[i].OldPath) < firstNonEmpty(files[j].NewPath, files[j].OldPath)
	})
	return files, warnings, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// unstagedOverlap warns about staged files that also have unstaged edits: the
// index maps the working tree's symbols, and the staged hunks' line numbers are
// the index's.
func unstagedOverlap(ctx context.Context, g *gitRepo, paths []string) []string {
	out, err := g.run(ctx, gitTimeout, "diff", "--name-only", "-z", "--relative")
	if err != nil {
		return nil
	}
	dirty := map[string]bool{}
	for _, p := range strings.Split(string(out), "\x00") {
		dirty[p] = true
	}
	var both []string
	for _, p := range paths {
		if dirty[p] {
			both = append(both, p)
		}
	}
	if len(both) == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d staged file(s) also have unstaged edits (%s); their symbols are the working tree's, so staged line numbers may not line up",
		len(both), strings.Join(both[:min(3, len(both))], ", "))}
}

// --- the old side ---

// An oldOutliner outlines the old version of a changed file. The old content is
// not on disk, so it is written to a scratch file *outside* the repository and
// opened there — a transient didOpen/didClose on the warm server, which the
// session's close undoes, and no write to the workspace. Where that is not
// possible (no server for the language, one that refuses, a file over the cap)
// the file is reported hunk-only.
type oldOutliner struct {
	e        *env
	common   *commonFlags
	root     string
	g        *gitRepo
	rev      string
	sessions map[string]*session
	// attempted counts files tried (against changedOldCap), reasons why some
	// could not be outlined.
	attempted int
	reasons   map[string]int
}

func (o *oldOutliner) fail(reason string) ([]index.Symbol, bool) {
	if o.reasons == nil {
		o.reasons = map[string]int{}
	}
	o.reasons[reason]++
	return nil, false
}

func (o *oldOutliner) summary() []string {
	var out []string
	for _, r := range sortedKeys(o.reasons) {
		out = append(out, fmt.Sprintf("old version not outlined for %d file(s): %s", o.reasons[r], r))
	}
	return out
}

func (o *oldOutliner) close() {
	for _, s := range o.sessions {
		s.close()
	}
}

// outline returns the symbols of f's old version, in the index's coordinates and
// ids (the *old* path's ids: a rename compares by the part after the path).
func (o *oldOutliner) outline(ctx context.Context, f fileDiff) ([]index.Symbol, bool) {
	o.attempted++
	if o.rev == emptyTree {
		return nil, true // nothing existed: the old side is known, and empty
	}
	abs := filepath.Join(o.root, filepath.FromSlash(f.OldPath))
	match, err := o.e.resolveTarget(abs, "", o.common.server)
	if err != nil {
		var coded interface{ ErrorCode() render.Code }
		if errors.As(err, &coded) && coded.ErrorCode() == render.CodeNoServer {
			return o.fail("no language server handles it")
		}
		return o.fail(oneLine(err.Error()))
	}
	blob, err := o.g.run(ctx, gitTimeout, "show", o.rev+":./"+f.OldPath)
	if err != nil {
		return o.fail("git could not read the old content")
	}
	if len(blob) > gitBlobCap {
		return o.fail("the old content is over the size cap")
	}
	key := match.Server.Name + "\x00" + match.Root
	s := o.sessions[key]
	if s == nil {
		connect, cancel := context.WithTimeout(o.e.base(), o.common.timeout)
		defer cancel()
		s, err = startSession(connect, o.e, match, o.common.gateOptions())
		if err != nil {
			return o.fail(oneLine(err.Error()))
		}
		o.sessions[key] = s
	}
	dir, err := os.MkdirTemp("", "lightspeed-old-")
	if err != nil {
		return o.fail("no scratch directory")
	}
	defer os.RemoveAll(dir)
	scratch := filepath.Join(dir, filepath.Base(f.OldPath))
	if err := os.WriteFile(scratch, blob, 0o600); err != nil {
		return o.fail("no scratch file")
	}
	doc, err := s.open(scratch)
	if err != nil {
		return o.fail(oneLine(err.Error()))
	}
	syms, _, err := s.documentSymbols(doc)
	if err != nil {
		return o.fail(oneLine(err.Error()))
	}
	return index.BuildSymbols(f.OldPath, match.LanguageID, blob, syms), true
}

// --- mapping ---

// idKey is a symbol id without its file: `Container.Name#kind[~N]`, which is what
// stays the same when a file is renamed.
func idKey(id string) string {
	if _, rest, ok := strings.Cut(id, idSeparator); ok {
		return rest
	}
	return id
}

// touchesSymbol reports whether a hunk changed lines of the symbol, on the new
// side: an added or replaced line inside it, or a deletion strictly between its
// first and last line.
func touchesSymbol(h hunk, s index.Symbol) bool {
	if lo, hi, ok := h.newRange(); ok {
		return lo <= s.EndLine && hi >= s.Line
	}
	return s.Line <= h.NewStart && h.NewStart < s.EndLine
}

// insideChild reports whether everything the hunk changed inside s lies within
// one of s's nested symbols, in which case the change is the child's and not s's.
func insideChild(h hunk, s index.Symbol, syms []index.Symbol) bool {
	a, b := h.NewStart, h.NewStart
	if lo, hi, ok := h.newRange(); ok {
		a, b = max(lo, s.Line), min(hi, s.EndLine)
	}
	for _, c := range syms {
		if c.ID == s.ID || c.Line < s.Line || c.EndLine > s.EndLine || (c.Line == s.Line && c.EndLine == s.EndLine) {
			continue
		}
		if h.NewCount > 0 && c.Line <= a && b <= c.EndLine {
			return true
		}
		if h.NewCount == 0 && c.Line <= a && a < c.EndLine {
			return true
		}
	}
	return false
}

// coveredByAddition reports whether one hunk is an insertion that spans the
// whole symbol: the only sign, without the old outline, that it is new.
func coveredByAddition(hs []hunk, s index.Symbol) bool {
	for _, h := range hs {
		if lo, hi, ok := h.newRange(); ok && h.OldCount == 0 && lo <= s.Line && s.EndLine <= hi {
			return true
		}
	}
	return false
}

func capHunks(hs []hunk) []hunk {
	if len(hs) > changedHunkCap {
		return hs[:changedHunkCap]
	}
	return hs
}

// mapFileChange maps one file's diff onto its symbols. newFS is the index's
// symbols of the new file (nil when the file has no outline: an uncovered
// language, an ignored file), oldSyms the outline of the old version when
// oldKnown.
func mapFileChange(f fileDiff, newFS *index.FileSymbols, oldSyms []index.Symbol, oldKnown bool) []changedRow {
	file := firstNonEmpty(f.NewPath, f.OldPath)
	evidence := "index+hunks"
	if oldKnown {
		evidence = "index+old_outline"
	}
	fileRow := func(status, note string, hs []hunk) changedRow {
		r := changedRow{Status: status, Kind: "file", File: file, Hunks: capHunks(hs), Evidence: evidence, Note: note}
		if f.Status == "R" {
			r.OldFile = f.OldPath
		}
		return r
	}
	if f.Binary {
		return []changedRow{fileRow(diffStatusName(f), "binary file", nil)}
	}

	var newSyms []index.Symbol
	if newFS != nil {
		newSyms = newFS.Symbols
	}
	var rows []changedRow

	switch f.Status {
	case "D":
		if !oldKnown {
			return []changedRow{fileRow("removed", "file deleted; its symbols are unknown (old version not outlined)", nil)}
		}
		for _, o := range oldSyms {
			rows = append(rows, removedRow(f, o, evidence))
		}
		if len(rows) == 0 {
			rows = append(rows, fileRow("removed", "file deleted (it had no symbols)", nil))
		}
		return rows
	case "A":
		if newFS == nil {
			return []changedRow{fileRow("added", "new file with no symbol information (no server, or not indexed)", nil)}
		}
		for _, s := range newSyms {
			rows = append(rows, symbolRow(f, s, "added", nil, evidence))
		}
		if len(rows) == 0 {
			rows = append(rows, fileRow("added", "new file with no symbols", nil))
		}
		return rows
	}

	if f.Status == "R" && len(f.Hunks) == 0 {
		return []changedRow{fileRow("renamed", "renamed without content changes", nil)}
	}
	if newFS == nil {
		return []changedRow{fileRow("modified", "no symbol information for this file (no server, or not indexed); hunks only", f.Hunks)}
	}

	oldKeys := map[string]bool{}
	for _, o := range oldSyms {
		oldKeys[idKey(o.ID)] = true
	}
	newKeys := map[string]bool{}
	for _, s := range newSyms {
		newKeys[idKey(s.ID)] = true
	}
	explained := make([]bool, len(f.Hunks))
	for _, s := range newSyms {
		var hs []hunk
		for i, h := range f.Hunks {
			if touchesSymbol(h, s) {
				explained[i] = true
				if !insideChild(h, s, newSyms) {
					hs = append(hs, h)
				}
			}
		}
		switch {
		case oldKnown && !oldKeys[idKey(s.ID)]:
			rows = append(rows, symbolRow(f, s, "added", hs, evidence))
		case !oldKnown && len(hs) > 0 && coveredByAddition(f.Hunks, s):
			rows = append(rows, symbolRow(f, s, "added", hs, evidence))
		case len(hs) > 0:
			rows = append(rows, symbolRow(f, s, "modified", hs, evidence))
		}
	}
	if oldKnown {
		for _, o := range oldSyms {
			if newKeys[idKey(o.ID)] {
				continue
			}
			rows = append(rows, removedRow(f, o, evidence))
			for i, h := range f.Hunks {
				if lo, hi, ok := h.oldRange(); ok && lo <= o.EndLine && hi >= o.Line {
					explained[i] = true
				}
			}
		}
	}
	var loose []hunk
	for i, h := range f.Hunks {
		if !explained[i] {
			loose = append(loose, h)
		}
	}
	if len(loose) > 0 {
		rows = append(rows, fileRow("modified", "changes outside every declaration (package clause, imports, comments, blank lines)", loose))
	}
	return rows
}

func diffStatusName(f fileDiff) string {
	switch f.Status {
	case "A":
		return "added"
	case "D":
		return "removed"
	case "R":
		return "renamed"
	}
	return "modified"
}

func symbolRow(f fileDiff, s index.Symbol, status string, hs []hunk, evidence string) changedRow {
	r := changedRow{Status: status, Kind: "symbol", ID: s.ID, Name: s.Qualified, SymbolKind: s.Kind,
		File: f.NewPath, Line: s.Line, EndLine: s.EndLine, Hunks: capHunks(hs), Evidence: evidence}
	if f.Status == "R" {
		r.OldFile = f.OldPath
	}
	return r
}

func removedRow(f fileDiff, o index.Symbol, evidence string) changedRow {
	// A removed symbol has no new position; its id is the old file's.
	return changedRow{Status: "removed", Kind: "symbol", ID: o.ID, Name: o.Qualified, SymbolKind: o.Kind,
		File: firstNonEmpty(f.NewPath, f.OldPath), OldFile: f.OldPath, OldLine: o.Line, OldEndLine: o.EndLine, Evidence: evidence}
}
