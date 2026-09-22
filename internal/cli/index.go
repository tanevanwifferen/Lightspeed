package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// The whole-repo queries (docs/DECISIONS.md D32–D36): `index`, `search_symbols`,
// `repo_map`, `find_importers`, `imports`, `dependency_graph` and
// `dependency_cycles`. The index they read is the daemon's — or, with
// --no-daemon, this process's — and every one of them revalidates the files it
// reports on before answering, so what comes back is never older than the disk.

const (
	defaultSymbolLimit = 20
	defaultMapBudget   = 2000
	defaultMapPerFile  = 8
	defaultGraphDepth  = 2
	// The bounds of the list commands that had none (D42). They are token
	// discipline, not limits of the index: each is reported when it bites, and
	// --limit 0 lifts it.
	defaultImportersLimit = 50
	defaultImportsLimit   = 100
	defaultEdgesLimit     = 200
	defaultCyclesLimit    = 20
)

// The --detail levels of `search_symbols`.
const (
	detailCompact  = "compact"
	detailStandard = "standard"
	detailFull     = "full"
)

// An indexRun is one command's connection to the workspace's index.
type indexRun struct {
	e      *env
	common *commonFlags
	root   string
	h      daemon.Handle
}

// startIndex resolves the workspace from anchor and connects to its daemon (or
// starts the in-process service).
func startIndex(e *env, common *commonFlags, anchor string) (*indexRun, error) {
	root, err := commandWorkspace(anchor)
	if err != nil {
		return nil, err
	}
	h, err := openHandle(e.base(), e, root)
	if err != nil {
		return nil, err
	}
	return &indexRun{e: e, common: common, root: root, h: h}, nil
}

func (r *indexRun) close() { _ = r.h.Close() }

// do runs one index operation and decodes its answer into out.
func (r *indexRun) do(req daemon.IndexRequest, out any) error {
	req.GateTimeout, req.Settle = r.common.timeout, r.common.settle
	resp, err := r.h.Index(r.e.base(), req)
	if err != nil {
		return indexFailure(err)
	}
	if err := json.Unmarshal(resp.Data, out); err != nil {
		return render.Errorf(render.CodeProtocolError, "malformed index answer from the daemon: %v", err)
	}
	return nil
}

// indexFailure puts a daemon-reported failure in the CLI's taxonomy, keeping
// the code and exit status the daemon decided (not_ready is exit 5).
func indexFailure(err error) error {
	var remote *daemon.Error
	switch {
	case errors.As(err, &remote):
		switch remote.Code {
		case daemon.CodeCancelled:
			return render.Errorf(render.CodeCancelled, "index: cancelled")
		case daemon.CodeServerNotInstalled:
			return render.Errorf(render.CodeServerNotInstalled, "%s", remote.Message)
		}
		return remoteFailure{remote}
	case errors.Is(err, context.Canceled):
		return render.Errorf(render.CodeCancelled, "index: cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		return render.Errorf(render.CodeTimeout, "index: timed out")
	}
	return err
}

// syncWarnings are the warnings a revalidation earns: that the index was built
// for this query and how long that took, what was thrown away and why, and what
// the servers said about individual files.
func syncWarnings(rep *index.SyncReport) []string { return reportWarnings(rep, true) }

// reportSummary is the default shape of an index-backed command's `index`
// field: a one-line summary ("269 files, fresh, 6 skipped") and whether the
// revalidation rebuilt anything. --report (MCP report) restores the full
// index.SyncReport in its place (docs/DECISIONS.md D46); `index status`
// always carries the full report — reporting it in full is that command's
// job, so it never uses this.
type reportSummary struct {
	Summary string `json:"summary"`
	Stale   bool   `json:"stale"`
}

// indexReportField is what a command's `index` field holds: the full report
// with full, a one-line reportSummary otherwise. A nil report (nothing was
// queried) stays nil either way, so `omitempty` on the field still applies.
func indexReportField(rep *index.SyncReport, full bool) any {
	if rep == nil {
		return nil
	}
	if full {
		return rep
	}
	return &reportSummary{Summary: rep.Summary(), Stale: rep.Stale()}
}

// reportWarnings is syncWarnings, except that the "built lazily" notice is only
// for a query that had to build: an explicit `index build` is the warm-up the
// notice recommends, so it says nothing of the kind (lazy is false).
func reportWarnings(rep *index.SyncReport, lazy bool) []string {
	if rep == nil {
		return nil
	}
	var w []string
	if n := rep.Notice(); n != "" && lazy {
		w = append(w, n)
	}
	for _, d := range rep.Discarded {
		w = append(w, fmt.Sprintf("discarded the cached index part %s: %s; it was rebuilt from the workspace", d.Name, d.Reason))
	}
	const showWarnings = 5
	for i, s := range rep.Warnings {
		if i == showWarnings {
			w = append(w, fmt.Sprintf("… and %d more index warnings", len(rep.Warnings)-showWarnings))
			break
		}
		w = append(w, s)
	}
	return w
}

// scopeRel is the workspace-relative form of a --path scope. It must be inside
// the workspace, symlinks followed, whether or not it exists (D23, D26).
func scopeRel(root, p string) (string, error) {
	if p == "" {
		return "", nil
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", render.Errorf(render.CodeUsage, "resolving %s: %v", p, err)
	}
	rel, in := relToRoot(root, abs)
	if !in {
		return "", render.Errorf(render.CodeOutsideWorkspace,
			"%s is outside the workspace %s; only paths inside it can be searched", p, root)
	}
	if rel == "." {
		return "", nil
	}
	if _, err := os.Lstat(abs); err != nil {
		return "", render.Errorf(render.CodeNoSuchFile, "%s: no such file or directory", p)
	}
	return rel, nil
}

// targetRel resolves a file, directory or Go import path argument: a path that
// exists (relative to the working directory, then to the workspace) and lies
// inside the workspace becomes its workspace-relative form; anything else is
// passed as given, for the index to recognise as an import path or a node.
func targetRel(root, arg string) (string, error) {
	if arg == "" {
		return "", nil
	}
	try := func(abs string) (string, bool, error) {
		if _, err := os.Lstat(abs); err != nil {
			return "", false, nil
		}
		rel, in := relToRoot(root, abs)
		if !in {
			return "", false, render.Errorf(render.CodeOutsideWorkspace,
				"%s is outside the workspace %s", arg, root)
		}
		return rel, true, nil
	}
	if abs, err := filepath.Abs(arg); err == nil {
		if rel, ok, err := try(abs); err != nil {
			return "", err
		} else if ok {
			return rel, nil
		}
	}
	if !filepath.IsAbs(arg) {
		if rel, ok, err := try(filepath.Join(root, arg)); err != nil {
			return "", err
		} else if ok {
			return rel, nil
		}
	}
	// Not a path that exists: an import path, or a node the index will say it does
	// not know. An absolute path, or one that climbs out with `..`, is never an
	// import path: it is a place, and is refused when it is outside the
	// workspace whether or not anything is there (and named by its workspace-
	// relative form when it is inside).
	clean := path.Clean(filepath.ToSlash(arg))
	if filepath.IsAbs(arg) || clean == ".." || strings.HasPrefix(clean, "../") {
		candidates := []string{arg}
		if !filepath.IsAbs(arg) {
			if abs, err := filepath.Abs(arg); err == nil {
				candidates = []string{abs, filepath.Join(root, filepath.FromSlash(clean))}
			}
		}
		for _, cand := range candidates {
			if rel, in := relToRoot(root, cand); in {
				return rel, nil
			}
		}
		return "", render.Errorf(render.CodeOutsideWorkspace, "%s is outside the workspace %s", arg, root)
	}
	return strings.TrimPrefix(filepath.ToSlash(arg), "./"), nil
}

// splitList flattens repeatable, comma-separated flag values.
func splitList(in []string) []string {
	var out []string
	for _, v := range in {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// --- index ---

const indexSubcommands = "status|build|clear"

// indexCommand implements `lightspeed index status|build|clear`.
func indexCommand(e *env, c *command, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return e.usagef("index: missing subcommand (want one of %s)", indexSubcommands)
	}
	sub := args[0]
	switch sub {
	case "status", "build", "clear":
	default:
		return e.usagef("index: unknown subcommand %q (want one of %s)", sub, indexSubcommands)
	}
	var root string
	cc := *c
	cc.Name = "index " + sub
	cc.Args = "[--root DIR]"
	common, _, err := parseFlagsRange(e, &cc, args[1:], 0, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&root, "root", ".", "any directory inside the workspace")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, cc.Name, e.stdout)
	if err != nil {
		return e.fail(err)
	}
	run, err := startIndex(e, common, root)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()

	switch sub {
	case "status":
		var st index.Status
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpStatus}, &st); err != nil {
			return e.fail(err)
		}
		warnings := statusWarnings(&st)
		if format == render.FormatText {
			writeIndexStatusText(e.stdout, &st)
			return ExitOK
		}
		return e.writeData(common, st, warnings, ExitOK)
	case "build":
		var res index.BuildResult
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpBuild}, &res); err != nil {
			return e.fail(err)
		}
		warnings := reportWarnings(res.Report, false)
		warnings = append(warnings, statusWarnings(res.Status)...)
		if format == render.FormatText {
			writeIndexBuildText(e.stdout, &res)
			return ExitOK
		}
		return e.writeData(common, res, warnings, ExitOK)
	default:
		var res index.ClearResult
		if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpClear}, &res); err != nil {
			return e.fail(err)
		}
		if format == render.FormatText {
			fmt.Fprintf(e.stdout, "cleared %d entries; removed %d cache files (%d bytes) from %s\n",
				res.Entries, res.Parts, res.Bytes, res.CacheDir)
			return ExitOK
		}
		return e.writeData(common, res, nil, ExitOK)
	}
}

// statusWarnings are what an index status says that a reader would miss in the
// numbers: languages nothing covers, files skipped, parts thrown away.
func statusWarnings(st *index.Status) []string {
	if st == nil {
		return nil
	}
	var w []string
	for _, d := range st.Discarded {
		w = append(w, fmt.Sprintf("discarded the cached index part %s: %s", d.Name, d.Reason))
	}
	if len(st.Uncovered) > 0 {
		parts := make([]string, 0, len(st.Uncovered))
		for i, u := range st.Uncovered {
			if i == 6 {
				parts = append(parts, fmt.Sprintf("+%d more", len(st.Uncovered)-6))
				break
			}
			name := u.Language
			if name == "" {
				name = "(unknown)"
			}
			parts = append(parts, fmt.Sprintf("%s %d", name, u.Files))
		}
		w = append(w, "files no server and no import extractor covers are not indexed: "+strings.Join(parts, ", "))
	}
	return w
}

func writeIndexStatusText(w io.Writer, st *index.Status) {
	state := "cold"
	switch {
	case st.Warm:
		state = "warm"
	case st.Indexed > 0:
		state = "partial"
	}
	fmt.Fprintf(w, "index for %s: %s\n", st.Root, state)
	fmt.Fprintf(w, "  files:    %d in the workspace, %d covered: %d fresh, %d stale, %d not indexed", st.Files, st.Covered, st.Fresh, st.Stale, st.Missing)
	if st.Removed > 0 {
		fmt.Fprintf(w, ", %d entries for deleted files", st.Removed)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  symbols:  %d in %d outlined files; %d imports\n", st.Symbols, st.Outlined, st.Imports)
	for _, s := range st.Servers {
		v := ""
		if s.Version != "" {
			v = " " + s.Version
		}
		fmt.Fprintf(w, "  server:   %s%s: %d of %d files outlined, %d symbols\n", s.Name, v, s.Indexed, s.Files, s.Symbols)
	}
	for _, l := range st.Languages {
		cov := []string{}
		if l.Server != "" {
			cov = append(cov, "outline: "+l.Server)
		}
		if l.Imports {
			cov = append(cov, "imports")
		}
		fmt.Fprintf(w, "  language: %-16s %4d files, %4d indexed (%s)\n", l.Language, l.Files, l.Indexed, strings.Join(cov, ", "))
	}
	for _, g := range st.Skipped {
		fmt.Fprintf(w, "  skipped:  %s: %d (e.g. %s)\n", g.Reason, g.Count, strings.Join(g.Examples, ", "))
	}
	for _, u := range st.Uncovered {
		name := u.Language
		if name == "" {
			name = "(unknown)"
		}
		fmt.Fprintf(w, "  uncovered: %s: %d files\n", name, u.Files)
	}
	if st.CacheDir != "" {
		fmt.Fprintf(w, "  cache:    %s (%d bytes)\n", st.CacheDir, st.CacheBytes)
	}
	if st.LastBuild != nil {
		fmt.Fprintf(w, "  last build: %d files (%d outlines) in %s\n", st.LastBuild.Files, st.LastBuild.Outlines,
			st.LastBuild.Duration.Round(time.Millisecond))
	}
	for _, d := range st.Discarded {
		fmt.Fprintf(w, "# discarded %s: %s\n", d.Name, d.Reason)
	}
}

func writeIndexBuildText(w io.Writer, res *index.BuildResult) {
	r := res.Report
	fmt.Fprintf(w, "built %d files (%d outlines) in %s: %d already current, %d touched, %d changed, %d added, %d removed\n",
		r.Built, r.Outlines, r.Elapsed.Round(time.Millisecond), r.Fresh, r.Touched, r.Changed, r.Added, r.Removed)
	writeIndexStatusText(w, res.Status)
}

// --- search_symbols ---

// A symbolHit is one result of `search_symbols`. Compact carries the id and
// where the symbol is; standard adds what a reader needs to pick; full adds the
// source.
type symbolHit struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
	// Match is the quality of the match, exact, prefix, token or fuzzy, and Score
	// the rank within it; both at every detail level (D42).
	Match   string  `json:"match"`
	EndLine int     `json:"end_line,omitempty"`
	Score   float64 `json:"score"`
	// Container is the enclosing symbol, Signature the declaration and Doc the
	// first sentence of its comment.
	Container string      `json:"container,omitempty"`
	Signature string      `json:"signature,omitempty"`
	Doc       string      `json:"doc,omitempty"`
	Language  string      `json:"language,omitempty"`
	Source    *sourceItem `json:"source,omitempty"`
}

// searchSymbolsData is the payload of `search_symbols`.
type searchSymbolsData struct {
	Root    string      `json:"root"`
	Query   string      `json:"query"`
	Detail  string      `json:"detail"`
	Results []symbolHit `json:"results"`
	// Count is how many results this output lists, Total how many matched
	// before --limit. They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	// Candidates is how many symbols passed the filters and were ranked.
	Candidates int `json:"candidates"`
	// Fuzzy says the results are approximate matches: nothing matched the
	// query, and --fuzzy asked for the nearest names instead.
	Fuzzy bool `json:"fuzzy,omitempty"`
	// Notice is said before anything else, when the query looks like an
	// identifier and the best match is not that identifier (no exact or prefix
	// hit): the results are then words that happen to overlap, and reading them
	// as "the symbol" would be the mistake.
	Notice string `json:"notice,omitempty"`
	// Nearest are the closest names when nothing matched, so that an empty
	// answer is authoritative and not a guess.
	Nearest []string `json:"nearest,omitempty"`
	// Index is a reportSummary by default, the full *index.SyncReport with
	// --report (D46).
	Index any `json:"index,omitempty"`
}

// searchSymbolsCommand implements `lightspeed search_symbols <query>`.
func searchSymbolsCommand(e *env, c *command, args []string) int {
	var (
		kinds, langs, globs stringList
		pathFlag, rootFlag  string
		detail              string
		fuzzy               bool
		maxLines, maxBytes  int
		fset                *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 1, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.Var(&kinds, "kind", "only symbols of this kind, e.g. function, method, struct, class (repeatable or comma-separated)")
		fs.Var(&langs, "lang", "only files of this language id, e.g. go, python, typescript (repeatable or comma-separated)")
		fs.Var(&globs, "glob", "only files matching this glob, relative to the workspace (repeatable; a leading ! excludes; no / matches at any depth)")
		fs.StringVar(&pathFlag, "path", "", "only files under this directory, or this file, inside the workspace")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace to search; --path must be inside it too")
		fs.StringVar(&detail, "detail", detailStandard, "how much of each result to return: compact, standard or full (adds the source)")
		fs.BoolVar(&fuzzy, "fuzzy", false, "when nothing matches, fall back to approximate name matching (trigrams and edit distance), flagged as such")
		fs.IntVar(&maxLines, "max-lines", 0, "with --detail full: cap each symbol's source at this many lines")
		fs.IntVar(&maxBytes, "max-bytes", 0, "with --detail full: cap each symbol's source at this many bytes")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "search_symbols", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	switch detail {
	case detailCompact, detailStandard, detailFull:
	default:
		return e.usagef("search_symbols: --detail must be compact, standard or full (got %q)", detail)
	}
	if maxLines < 0 || maxBytes < 0 {
		return e.usagef("search_symbols: --max-lines and --max-bytes must not be negative")
	}
	query := strings.TrimSpace(positional[0])
	if query == "" {
		return e.usagef("search_symbols: the query is empty")
	}
	for _, k := range splitList(kinds) {
		if !symbols.KnownKind(strings.ToLower(k)) {
			return e.usagef("search_symbols: --kind %q is not a symbol kind (want one of %s)", k, strings.Join(symbols.KindNames(), ", "))
		}
	}
	limit := common.limit
	if limit == 0 {
		limited := false
		fset.Visit(func(f *flag.Flag) { limited = limited || f.Name == "limit" })
		if !limited {
			limit = defaultSymbolLimit
		}
	}

	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	scope, err := scopeRel(run.root, pathFlag)
	if err != nil {
		return e.fail(err)
	}

	var out index.SearchOutcome
	err = run.do(daemon.IndexRequest{Op: daemon.IndexOpSearch, Search: &index.SearchQuery{
		Text: query, Kinds: splitList(kinds), Languages: splitList(langs), Globs: globs,
		Path: scope, Limit: limit, Fuzzy: fuzzy,
	}}, &out)
	if err != nil {
		return e.fail(err)
	}

	data := searchSymbolsData{
		Root: run.root, Query: query, Detail: detail, Results: make([]symbolHit, 0, len(out.Hits)),
		Count: len(out.Hits), Total: out.Total, Truncated: out.Truncated, Limit: limit,
		Candidates: out.Candidates, Fuzzy: out.UsedFuzzy, Nearest: out.Nearest, Index: indexReportField(out.Report, common.report),
	}
	warnings := syncWarnings(out.Report)
	for _, h := range out.Hits {
		hit := symbolHit{ID: h.Symbol.ID, Name: h.Symbol.Name, Kind: h.Symbol.Kind, File: h.File,
			Line: h.Symbol.Line, Match: h.Match, Score: roundScore(h.Score)}
		if detail != detailCompact {
			hit.EndLine = h.Symbol.EndLine
			hit.Container, hit.Signature, hit.Doc, hit.Language = h.Symbol.Container, h.Symbol.Signature, h.Symbol.Doc, h.Language
		}
		data.Results = append(data.Results, hit)
	}
	if detail == detailFull {
		warnings = append(warnings, inlineSources(run.root, &data, out.FileHashes, sourceCaps{context: common.context, maxLines: maxLines, maxBytes: maxBytes})...)
	}
	if out.UsedFuzzy {
		warnings = append(warnings, fmt.Sprintf("nothing matched %q; these are the nearest names by trigram and edit distance (--fuzzy), not matches", query))
	}
	if notice := identifierNotice(query, out.Hits); notice != "" {
		data.Notice = notice
		warnings = append([]string{notice}, warnings...)
	}
	if data.Truncated {
		warnings = append(warnings, fmt.Sprintf("listed %d of %d matches; narrow it with --kind, --lang, --glob or --path, or raise --limit", data.Count, data.Total))
	}
	exit := ExitOK
	if len(data.Results) == 0 {
		exit = ExitProblems
		// The authoritative empty answer is the first thing said, like the
		// identifier notice: it is the whole answer.
		msg := fmt.Sprintf("no symbol matches %q among %d candidates", query, out.Candidates)
		if len(out.Nearest) > 0 {
			msg = fmt.Sprintf("no symbol matches %q among %d candidates; the nearest names are %s", query, out.Candidates, strings.Join(out.Nearest, ", "))
		}
		data.Notice = msg
		warnings = append([]string{msg}, warnings...)
	}
	if format == render.FormatText {
		if data.Notice != "" {
			warnings = warnings[1:] // it is the first line of the text, not a trailer
		}
		writeSymbolHitsText(e.stdout, data, warnings, common.absolute, run.root)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// identifierRE is what a code identifier looks like as a query: one or more
// name segments joined by `.` or `::`, no spaces.
var identifierRE = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}_]*(?:(?:\.|::)[\p{L}_][\p{L}\p{N}_]*)*$`)

// identifierNotice is the sentence that goes first when the query is shaped like
// an identifier and no result is an exact or prefix match of it: "no symbol
// named X", then what the results are instead. "" when it does not apply (the
// query is words, or something matched by name, or nothing matched at all, which
// has its own authoritative message).
func identifierNotice(query string, hits []index.Hit) string {
	if len(hits) == 0 || !identifierRE.MatchString(query) {
		return ""
	}
	weakest := ""
	for _, h := range hits {
		switch h.Match {
		case index.MatchExact, index.MatchPrefix:
			return ""
		case index.MatchToken:
			weakest = index.MatchToken
		case index.MatchFuzzy:
			if weakest == "" {
				weakest = index.MatchFuzzy
			}
		}
	}
	what := "token matches: symbols that contain the words of the query, not a symbol of that name"
	if weakest == index.MatchFuzzy {
		what = "fuzzy near-misses by trigram and edit distance, not symbols of that name"
	}
	return fmt.Sprintf("no symbol named %s (exact or prefix); these are %s", query, what)
}

// roundScore keeps a score readable: three decimals are all a ranking has.
func roundScore(s float64) float64 { return float64(int64(s*1000+0.5)) / 1000 }

// inlineSources reads the files of the hits and cuts each symbol's source out
// of them, as `source` would. A file that is not the one that was indexed is
// left without source and named: its line numbers mean nothing any more.
func inlineSources(root string, data *searchSymbolsData, hashes map[string]string, caps sourceCaps) []string {
	files := map[string]*lineIndex{}
	bad := map[string]bool{}
	var warnings []string
	for i := range data.Results {
		h := &data.Results[i]
		if bad[h.File] {
			continue
		}
		x, ok := files[h.File]
		if !ok {
			content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(h.File)))
			switch {
			case err != nil:
				bad[h.File] = true
				warnings = append(warnings, fmt.Sprintf("%s: source not inlined: %v", h.File, err))
				continue
			case hashes[h.File] != "" && index.HashBytes(content) != hashes[h.File]:
				bad[h.File] = true
				warnings = append(warnings, fmt.Sprintf("%s changed while the query ran; its source was not inlined — run the search again", h.File))
				continue
			}
			x = newLineIndex(content)
			files[h.File] = x
		}
		item := sliceSource(x, h.Line-1, h.EndLine-1, caps)
		if h.EndLine == 0 {
			item = sliceSource(x, h.Line-1, h.Line-1, caps)
		}
		item.ID, item.File, item.Kind = h.ID, h.File, h.Kind
		h.Source = &item
		if item.Truncated {
			warnings = append(warnings, cutWarning(item))
		}
	}
	return warnings
}

// writeSymbolHitsText is one symbol per line, `file:line: kind Name  signature`,
// with the source indented under it for --detail full.
func writeSymbolHitsText(w io.Writer, d searchSymbolsData, warnings []string, absolute bool, root string) {
	if d.Notice != "" {
		fmt.Fprintf(w, "# %s\n", d.Notice)
	}
	for _, h := range d.Results {
		file := h.File
		if absolute {
			file = filepath.Join(root, filepath.FromSlash(h.File))
		}
		// The id says the symbol the way the code reads: Container.Name.
		name := h.Name
		if i, j := strings.Index(h.ID, "::"), strings.LastIndex(h.ID, "#"); i >= 0 && j > i+2 {
			name = h.ID[i+2 : j]
		}
		fmt.Fprintf(w, "%s:%d: %s %s", file, h.Line, h.Kind, name)
		if h.Signature != "" {
			fmt.Fprintf(w, "  %s", h.Signature)
		}
		// An exact match is the unmarked case; anything weaker says so, so a
		// reader skimming the rows cannot mistake a word overlap for the symbol.
		if h.Match != index.MatchExact && h.Match != "" {
			fmt.Fprintf(w, "  [~%s]", h.Match)
		}
		fmt.Fprintln(w)
		if h.Source != nil {
			for _, line := range strings.Split(h.Source.Source, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		}
	}
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}

// --- repo_map ---

// repoMapData is the payload of `repo_map`.
type repoMapData struct {
	Root string `json:"root"`
	index.RepoMapOutcome
	// Report shadows RepoMapOutcome's own (always-full) Report field: a
	// reportSummary by default, the full *index.SyncReport with --report
	// (D46). RepoMapOutcome.Report is cleared before this struct is built, so
	// only this field reaches the JSON.
	Report any `json:"report,omitempty"`
	// Limit echoes --limit, the cap on the files listed that comes on top of the
	// token budget (D42).
	Limit int `json:"limit,omitempty"`
}

// repoMapCommand implements `lightspeed repo_map`.
func repoMapCommand(e *env, c *command, args []string) int {
	var (
		budget, perFile    int
		globs, langs       stringList
		pathFlag, rootFlag string
	)
	common, _, err := parseFlagsRange(e, c, args, 0, 0, func(fs *flag.FlagSet) {
		fs.IntVar(&budget, "budget", defaultMapBudget, "token budget for the map (about 4 bytes per token); the map stops when the next line would not fit")
		fs.IntVar(&perFile, "per-file", defaultMapPerFile, "at most this many symbols for each of the highest-ranked files (the rest get at most 3)")
		fs.Var(&globs, "glob", "only files matching this glob, relative to the workspace (repeatable; a leading ! excludes)")
		fs.Var(&langs, "lang", "only files of this language id (repeatable or comma-separated)")
		fs.StringVar(&pathFlag, "path", "", "only files under this directory, or this file, inside the workspace")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace to map; --path must be inside it too")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "repo_map", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if budget < 1 || perFile < 1 {
		return e.usagef("repo_map: --budget and --per-file must be at least 1")
	}
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	scope, err := scopeRel(run.root, pathFlag)
	if err != nil {
		return e.fail(err)
	}
	var out index.RepoMapOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpRepoMap, RepoMap: &index.RepoMapOptions{
		BudgetTokens: budget, PerFile: perFile, Path: scope, Globs: globs, Languages: splitList(langs),
	}}, &out); err != nil {
		return e.fail(err)
	}
	warnings := syncWarnings(out.Report)
	report := indexReportField(out.Report, common.report)
	out.Report = nil
	if common.limit > 0 && len(out.Files) > common.limit {
		out.Files = out.Files[:common.limit]
		out.FilesListed, out.Truncated = common.limit, true
		warnings = append(warnings, truncationWarning("files", common.limit, out.FilesTotal, "raise --limit, or scope it with --path or --glob"))
	} else if out.Truncated {
		warnings = append(warnings, fmt.Sprintf("the map lists %d of %d files within the %d-token budget; raise --budget, or scope it with --path or --glob",
			out.FilesListed, out.FilesTotal, out.Budget))
	}
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "no import extractor covers "+strings.Join(out.UncoveredLanguages, ", ")+
			"; those files are ranked by their symbols alone, not by the import graph")
	}
	if !out.Ranked && out.FilesTotal > 0 {
		warnings = append(warnings, "no imports were found among the mapped files, so they are ordered by symbol count, not by import centrality")
	}
	exit := ExitOK
	if len(out.Files) == 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		index.RenderRepoMapText(e.stdout, out.RepoMapResult)
		for _, msg := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", msg)
		}
		return exit
	}
	return e.writeData(common, repoMapData{Root: run.root, RepoMapOutcome: out, Report: report, Limit: common.limit}, warnings, exit)
}

// --- imports, find_importers, dependency_graph, dependency_cycles ---

// importsData is the payload of `imports`.
type importsData struct {
	Root string `json:"root"`
	index.ImportsOutcome
	// Report shadows ImportsOutcome's own (always-full) Report field: a
	// reportSummary by default, the full *index.SyncReport with --report
	// (D46). ImportsOutcome.Report is cleared before this struct is built,
	// so only this field reaches the JSON.
	Report any `json:"report,omitempty"`
	// Count is how many imports this output lists, Total how many the file has.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
}

// importsCommand implements `lightspeed imports <file>`.
func importsCommand(e *env, c *command, args []string) int {
	var rootFlag string
	var fset *flag.FlagSet
	common, positional, err := parseFlagsRange(e, c, args, 1, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "imports", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultImportsLimit)
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	rel, err := scopeRel(run.root, positional[0])
	if err != nil {
		return e.fail(err)
	}
	if rel == "" {
		return e.usagef("imports: %s is the workspace, not a file", positional[0])
	}
	var out index.ImportsOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpImports, Path: rel}, &out); err != nil {
		return e.fail(err)
	}
	if !out.Covered {
		return e.fail(notCoveredError(rel, out.Language, "imports"))
	}
	warnings := syncWarnings(out.Report)
	report := indexReportField(out.Report, common.report)
	out.Report = nil
	if out.Test {
		warnings = append(warnings, "a Go test file's imports are listed, but they are not edges of the dependency graph")
	}
	kept, truncated, total := capRows(out.Imports, limit)
	out.Imports = kept
	if truncated {
		warnings = append(warnings, truncationWarning("imports", len(kept), total, "raise --limit (0 for all)"))
	}
	if format == render.FormatText {
		writeImportsText(e.stdout, out.ImportsResult, warnings)
		return ExitOK
	}
	return e.writeData(common, importsData{Root: run.root, ImportsOutcome: out, Report: report, Count: len(kept), Total: total,
		Truncated: truncated, Limit: limit}, warnings, ExitOK)
}

// notCoveredError is the answer for a language no import extractor reads: the
// honest "unknown" and not an empty list (D35).
func notCoveredError(target, language, what string) error {
	lang := language
	if lang == "" {
		lang = "an unrecognised language"
	}
	return render.Errorf(render.CodeNotCovered,
		"no import extractor covers %s (%s), so %s of %s is unknown, not empty; covered languages: %s",
		target, lang, what, target, strings.Join(index.ExtractedLanguages(), ", "))
}

func writeImportsText(w io.Writer, r index.ImportsResult, warnings []string) {
	for _, im := range r.Imports {
		switch {
		case im.External:
			fmt.Fprintf(w, "%s:%d: %s  (external: %s)\n", r.File, im.Line, im.Spec, im.Category)
		case len(im.Targets) == 0:
			fmt.Fprintf(w, "%s:%d: %s  (%s)\n", r.File, im.Line, im.Spec, im.Category)
		default:
			fmt.Fprintf(w, "%s:%d: %s -> %s\n", r.File, im.Line, im.Spec, strings.Join(im.Targets, ", "))
		}
	}
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}

// importersData is the payload of `find_importers`.
type importersData struct {
	Root string `json:"root"`
	index.ImportersOutcome
	// Report shadows ImportersOutcome's own (always-full) Report field: a
	// reportSummary by default, the full *index.SyncReport with --report
	// (D46). ImportersOutcome.Report is cleared before this struct is built,
	// so only this field reaches the JSON.
	Report any `json:"report,omitempty"`
	// Count is how many importers this output lists, Total how many there are.
	// They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	// TestOnly says which importers the graph left out: Go test files whose
	// package imports the target only from tests. They are counted, and listed
	// up to the limit, but are not in Importers unless --include-tests merged
	// them (Included).
	TestOnly *testOnlyImporters `json:"test_only_importers,omitempty"`
}

// testOnlyImporters is the test-only importers of a find_importers answer.
type testOnlyImporters struct {
	Count     int              `json:"count"`
	Included  bool             `json:"included,omitempty"`
	Files     []index.Importer `json:"files,omitempty"`
	Truncated bool             `json:"truncated,omitempty"`
}

// findImportersCommand implements `lightspeed find_importers <file|package>`.
func findImportersCommand(e *env, c *command, args []string) int {
	var (
		rootFlag     string
		includeTests bool
		fset         *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 1, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
		fs.BoolVar(&includeTests, "include-tests", false, "also list the Go test files whose package imports the target only from tests (they are counted and named either way)")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "find_importers", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultImportersLimit)
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	target, err := targetRel(run.root, positional[0])
	if err != nil {
		return e.fail(err)
	}
	var out index.ImportersOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpImporters, Target: target}, &out); err != nil {
		return e.fail(err)
	}
	if !out.Covered {
		return e.fail(notCoveredError(out.Target, "", "the importers"))
	}
	warnings := syncWarnings(out.Report)
	report := indexReportField(out.Report, common.report)
	out.Report = nil
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "no import extractor covers "+strings.Join(out.UncoveredLanguages, ", ")+
			"; files in those languages are not among the importers, if any import this")
	}
	testOnly := out.TestOnly
	out.TestOnly = nil
	all := out.Importers
	var info *testOnlyImporters
	if len(testOnly) > 0 {
		info = &testOnlyImporters{Count: len(testOnly), Included: includeTests}
		if includeTests {
			all = append(slices.Clone(all), testOnly...)
			sort.SliceStable(all, func(i, j int) bool {
				if all[i].File != all[j].File {
					return all[i].File < all[j].File
				}
				return all[i].Line < all[j].Line
			})
		} else {
			info.Files, info.Truncated, _ = capRows(testOnly, limit)
			names := make([]string, 0, len(info.Files))
			for _, im := range info.Files {
				names = append(names, im.File)
			}
			msg := fmt.Sprintf("%d test-only importer(s) left out (Go test files whose package imports this only from tests): %s", len(testOnly), strings.Join(names, ", "))
			if info.Truncated {
				msg += fmt.Sprintf(", … and %d more", len(testOnly)-len(info.Files))
			}
			warnings = append(warnings, msg+"; --include-tests lists them with the importers")
		}
	}
	kept, truncated, total := capRows(all, limit)
	out.Importers = kept
	if truncated {
		warnings = append(warnings, truncationWarning("importers", len(kept), total, "raise --limit (0 for all)"))
	}
	exit := ExitOK
	if len(all) == 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		for _, im := range kept {
			mark := ""
			if im.Test {
				mark = "  [test]"
			}
			fmt.Fprintf(e.stdout, "%s:%d: %s%s\n", im.File, im.Line, im.Spec, mark)
		}
		for _, msg := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", msg)
		}
		return exit
	}
	return e.writeData(common, importersData{Root: run.root, ImportersOutcome: out, Report: report, Count: len(kept), Total: total,
		Truncated: truncated, Limit: limit, TestOnly: info}, warnings, exit)
}

// graphData is the payload of `dependency_graph`.
type graphData struct {
	Root string `json:"root"`
	index.GraphOutcome
	// Report shadows GraphOutcome's own (always-full) Report field: a
	// reportSummary by default, the full *index.SyncReport with --report
	// (D46). GraphOutcome.Report is cleared before this struct is built, so
	// only this field reaches the JSON.
	Report any `json:"report,omitempty"`
	// Count is how many edges this output lists, Total how many the walk found;
	// the nodes listed are the start and the ends of the edges listed. Truncated
	// (the embedded one) is set for either cut: the limit or the walk's node
	// budget.
	Count      int `json:"count"`
	Total      int `json:"total"`
	TotalNodes int `json:"total_nodes"`
	Limit      int `json:"limit,omitempty"`
}

// dependencyGraphCommand implements `lightspeed dependency_graph [path]`.
func dependencyGraphCommand(e *env, c *command, args []string) int {
	var (
		depth     int
		direction string
		external  bool
		rootFlag  string
		fset      *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.IntVar(&depth, "depth", defaultGraphDepth, "how many import edges to follow from the start")
		fs.StringVar(&direction, "direction", "out", "out (what it imports), in (what imports it) or both")
		fs.BoolVar(&external, "external", false, "include imports of things outside the workspace (standard library, third-party) as leaf edges")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "dependency_graph", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if depth < 1 {
		return e.usagef("dependency_graph: --depth must be at least 1 (got %d)", depth)
	}
	if _, err := index.ParseDirection(direction); err != nil {
		return e.usagef("dependency_graph: %v", err)
	}
	limit := effectiveLimit(fset, common, defaultEdgesLimit)
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	start := ""
	if len(positional) == 1 {
		if start, err = targetRel(run.root, positional[0]); err != nil {
			return e.fail(err)
		}
	}
	var out index.GraphOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpGraph, Root: start, Depth: depth, Direction: direction, External: external}, &out); err != nil {
		return e.fail(err)
	}
	warnings := syncWarnings(out.Report)
	report := indexReportField(out.Report, common.report)
	out.Report = nil
	walkCut := out.Truncated
	totalEdges, totalNodes := len(out.Edges), len(out.Nodes)
	var cut bool
	out.Edges, cut, _ = capRows(out.Edges, limit)
	if cut {
		out.Nodes = nodesOfEdges(out.Nodes, out.Edges, start)
		out.Truncated = true
		warnings = append(warnings, truncationWarning("edges", len(out.Edges), totalEdges,
			"give a start path or a smaller --depth, or raise --limit (0 for all)"))
	}
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "no import extractor covers "+strings.Join(out.UncoveredLanguages, ", ")+
			"; files in those languages have no edges, which is not the same as no imports")
	}
	if walkCut {
		warnings = append(warnings, "the walk was cut at its node limit; give a start path or a smaller --depth")
	}
	if len(out.Cycles) > 0 {
		warnings = append(warnings, fmt.Sprintf("%d import cycle(s) touch this graph; dependency_cycles lists them", len(out.Cycles)))
	}
	if format == render.FormatText {
		index.RenderWalkTree(e.stdout, out.WalkResult)
		for _, msg := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", msg)
		}
		return ExitOK
	}
	return e.writeData(common, graphData{Root: run.root, GraphOutcome: out, Report: report, Count: len(out.Edges), Total: totalEdges,
		TotalNodes: totalNodes, Limit: limit}, warnings, ExitOK)
}

// nodesOfEdges keeps the nodes a cut walk still shows: the named start, if
// there is one, and the two ends of every edge that is left. A walk of the whole
// workspace has every file as a start (depth 0), so depth is no reason to keep a
// node there: the nodes must match the edges listed.
func nodesOfEdges(nodes []index.Node, edges []index.Edge, start string) []index.Node {
	used := map[string]bool{}
	for _, e := range edges {
		used[e.From], used[e.To] = true, true
	}
	out := make([]index.Node, 0, len(nodes))
	for _, n := range nodes {
		if used[n.ID] || (start != "" && n.ID == start) {
			out = append(out, n)
		}
	}
	return out
}

// cyclesData is the payload of `dependency_cycles`.
type cyclesData struct {
	Root string `json:"root"`
	index.CyclesOutcome
	// Report shadows CyclesOutcome's own (always-full) Report field: a
	// reportSummary by default, the full *index.SyncReport with --report
	// (D46). CyclesOutcome.Report is cleared before this struct is built, so
	// only this field reaches the JSON.
	Report any `json:"report,omitempty"`
	// Count (the embedded one) is how many cycles this output lists; Total is
	// how many there are.
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
}

// dependencyCyclesCommand implements `lightspeed dependency_cycles`.
func dependencyCyclesCommand(e *env, c *command, args []string) int {
	var rootFlag string
	var fset *flag.FlagSet
	common, _, err := parseFlagsRange(e, c, args, 0, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "dependency_cycles", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultCyclesLimit)
	run, err := startIndex(e, common, rootFlag)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	var out index.CyclesOutcome
	if err := run.do(daemon.IndexRequest{Op: daemon.IndexOpCycles}, &out); err != nil {
		return e.fail(err)
	}
	warnings := syncWarnings(out.Report)
	report := indexReportField(out.Report, common.report)
	out.Report = nil
	if len(out.UncoveredLanguages) > 0 {
		warnings = append(warnings, "no import extractor covers "+strings.Join(out.UncoveredLanguages, ", ")+
			"; cycles through files in those languages are not detected")
	}
	// Cycles are problems found: exit 1, as `check` with errors is. The exit
	// is decided on all of them, before the limit can cut the list.
	exit := ExitOK
	if out.Count > 0 {
		exit = ExitProblems
	}
	kept, truncated, total := capRows(out.Cycles, limit)
	out.Cycles, out.Count = kept, len(kept)
	if truncated {
		warnings = append(warnings, truncationWarning("cycles", len(kept), total, "raise --limit (0 for all)"))
	}
	if format == render.FormatText {
		for _, cyc := range out.Cycles {
			fmt.Fprintf(e.stdout, "%s -> %s\n", strings.Join(cyc, " -> "), cyc[0])
		}
		for _, msg := range warnings {
			fmt.Fprintf(e.stdout, "# %s\n", msg)
		}
		return exit
	}
	return e.writeData(common, cyclesData{Root: run.root, CyclesOutcome: out, Report: report, Total: total, Truncated: truncated, Limit: limit}, warnings, exit)
}
