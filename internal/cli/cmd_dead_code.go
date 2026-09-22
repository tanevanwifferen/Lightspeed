package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// `lightspeed dead_code [path]` — symbols nothing refers to.
//
// It is N reference queries, so it is bounded and resumable rather than fast:
//
//  1. the index lists the symbols in scope (unranked, revalidated: D32);
//  2. each is put through exclusions that need no query — test files, entry
//     points, symbols the ignore lists name, locals, exported API;
//  3. what is left, in a stable order (file, line, id), is examined: one
//     textDocument/references per symbol, the declaration itself and any
//     reference inside it (a recursive call) not counting;
//  4. a method with no references is asked textDocument/implementation: one
//     that implements an interface is not dead, whoever calls the interface;
//  5. what still has none is a candidate, with a confidence.
//
// --budget caps the reference queries of one call and next_cursor resumes after
// the last symbol examined, so a big repository is swept in slices and a slice
// never runs unbounded. --limit caps the rows shown, as everywhere; the count
// of candidates found is `total`. A reference is evidence of use and its absence
// is only evidence of disuse: reflection, build tags, cgo, code generated at
// build time and callers outside the workspace can all hide a use, which is why
// results are candidates and the answer says so (D39).
const (
	defaultDeadBudget = 100
	defaultDeadLimit  = 50
)

// defaultDeadKinds are the kinds examined unless --kind says otherwise. A
// variable, field or parameter is left out: fields are read through reflection
// and serialisation far more often than functions are, and the compiler already
// reports an unused local.
var defaultDeadKinds = []string{"function", "method", "class", "struct", "interface", "enum", "constant"}

// The exclusion reasons, as reported in `excluded`.
const (
	deadTestFile   = "test_file"
	deadEntryPoint = "entry_point"
	deadIgnored    = "ignored"
	deadLocal      = "local"
	deadExported   = "exported"
	deadImplements = "implements"
	deadUnresolved = "unresolved"
)

// deadIgnoreNames are the names never reported, on top of the workspace's own
// [dead_code] ignore_names. Each is a method a language's own protocols call by
// name, so that no reference to it exists in the code: the compiler, the
// runtime or a library calls it.
//
//	Go      String Error GoString Format Unwrap Is As MarshalJSON UnmarshalJSON
//	        MarshalText UnmarshalText MarshalBinary UnmarshalBinary Read Write
//	        Close Len Less Swap ServeHTTP Scan Value
//	JS/TS   constructor toString toJSON valueOf
//	Rust    fmt drop deref deref_mut next from into default eq cmp partial_cmp
//	        hash clone try_from from_str as_ref
//
// They apply to methods only. `main` and `init` are entry points, below.
var deadIgnoreMethodNames = []string{
	"String", "Error", "GoString", "Format", "Unwrap", "Is", "As",
	"MarshalJSON", "UnmarshalJSON", "MarshalText", "UnmarshalText", "MarshalBinary", "UnmarshalBinary",
	"Read", "Write", "Close", "Len", "Less", "Swap", "ServeHTTP", "Scan", "Value",
	"constructor", "toString", "toJSON", "valueOf",
	"fmt", "drop", "deref", "deref_mut", "next", "from", "into", "default", "eq", "cmp", "partial_cmp",
	"hash", "clone", "try_from", "from_str", "as_ref",
}

// A deadCandidate is a symbol with no references outside its own declaration.
type deadCandidate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	File    string `json:"file"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
	// Exported is "yes", "no" or "unknown": whether the symbol is part of the
	// public API by its language's rules. Unknown is not a guess in either
	// direction (Python has only a convention; C has headers).
	Exported string `json:"exported"`
	// Confidence is high, medium or low: how likely the symbol is really
	// unused. Reasons says what lowered it; it is empty at high.
	Confidence string   `json:"confidence"`
	Reasons    []string `json:"reasons,omitempty"`
}

// deadQueries counts the requests a call made.
type deadQueries struct {
	References     int `json:"references"`
	Implementation int `json:"implementation"`
}

// deadCodeData is the payload of `dead_code`.
type deadCodeData struct {
	Root  string   `json:"root"`
	Scope string   `json:"scope,omitempty"`
	Kinds []string `json:"kinds"`
	// Candidates are listed in examination order; Count of Total are shown,
	// and Truncated says --limit cut them.
	Candidates []deadCandidate `json:"candidates"`
	Count      int             `json:"count"`
	Total      int             `json:"total"`
	Truncated  bool            `json:"truncated"`
	Limit      int             `json:"limit,omitempty"`
	// Symbols is how many symbols of the wanted kinds are in scope, Eligible how
	// many survive the exclusions and so need a query, Examined how many this
	// call examined, Used how many of those have references.
	Symbols  int `json:"symbols"`
	Eligible int `json:"eligible"`
	Examined int `json:"examined"`
	Used     int `json:"used"`
	// SkippedByCursor are the eligible symbols before --cursor, examined by an
	// earlier call.
	SkippedByCursor int            `json:"skipped_by_cursor,omitempty"`
	Excluded        map[string]int `json:"excluded"`
	Queries         deadQueries    `json:"queries"`
	Budget          int            `json:"budget"`
	// Complete is true when every eligible symbol after the cursor was
	// examined; otherwise NextCursor resumes.
	Complete   bool   `json:"complete"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func deadCodeCommand(e *env, c *command, args []string) int {
	var (
		kinds                      stringList
		pathFlag, rootFlag, cursor string
		includeExported            bool
		budget                     int
		fset                       *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.Var(&kinds, "kind", "symbol kinds to examine (repeatable or comma-separated; default function, method, class, struct, interface, enum, constant)")
		fs.StringVar(&pathFlag, "path", "", "directory or file to examine, inside the workspace (default: all of it)")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace to examine; --path must be inside it too")
		fs.BoolVar(&includeExported, "include-exported", false, "also examine exported symbols (public API), reported with low confidence")
		fs.IntVar(&budget, "budget", defaultDeadBudget, "most reference queries this call makes; next_cursor continues after them")
		fs.StringVar(&cursor, "cursor", "", "continue after this candidate: the next_cursor of the previous answer")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "dead_code", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if budget < 1 {
		return e.usagef("dead_code: --budget must be at least 1 (got %d)", budget)
	}
	if len(positional) == 1 {
		if pathFlag != "" && pathFlag != positional[0] {
			return e.usagef("dead_code: %q and --path %q both name the scope; give one", positional[0], pathFlag)
		}
		pathFlag = positional[0]
	}
	wantKinds := splitList(kinds)
	if len(wantKinds) == 0 {
		wantKinds = defaultDeadKinds
	}
	for _, k := range wantKinds {
		if !symbols.KnownKind(k) {
			return e.usagef("dead_code: unknown --kind %q (want one of %s)", k, strings.Join(symbols.KindNames(), ", "))
		}
	}
	after, err := parseDeadCursor(cursor)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultDeadLimit)

	anchor := rootFlag
	if rootFlag == "." && pathFlag != "" {
		// A scope names a place; that place is what identifies the workspace
		// when --root was not given.
		anchor = pathFlag
	}
	run, err := startIndex(e, common, anchor)
	if err != nil {
		return e.fail(err)
	}
	defer run.close()
	scope, err := scopeRel(run.root, pathFlag)
	if err != nil {
		return e.fail(err)
	}
	cfg, err := serverdef.LoadDeadCodeConfig(run.root)
	if err != nil {
		return e.fail(render.Errorf(render.CodeInvalidConfig, "%v", err))
	}
	rules, err := newDeadRules(run.root, cfg)
	if err != nil {
		return e.fail(err)
	}

	out, err := run.Symbols(index.SymbolsQuery{Path: scope, Kinds: wantKinds})
	if err != nil {
		return e.fail(err)
	}
	warnings := syncWarnings(out.Report)

	d := &deadCodeData{Root: run.root, Scope: scope, Kinds: wantKinds, Candidates: []deadCandidate{},
		Excluded: map[string]int{}, Budget: budget, Limit: limit}
	sweep := &deadSweep{e: e, common: common, root: run.root, rules: rules, includeExported: includeExported,
		texts: map[string][]string{}, sessions: map[string]*session{}, files: map[string]*deadFile{}, seenWarn: map[string]bool{}}
	defer sweep.close()

	eligible := sweep.eligible(out, d)
	d.Eligible = len(eligible)
	if err := sweep.examine(eligible, after, d); err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, sweep.warnings...)

	d.Total = len(d.Candidates)
	d.Candidates, d.Truncated, _ = capRows(d.Candidates, limit)
	d.Count = len(d.Candidates)
	if d.Truncated {
		warnings = append(warnings, truncationWarning("candidates", d.Count, d.Total, "raise --limit to see the rest"))
	}
	if !d.Complete {
		warnings = append(warnings, fmt.Sprintf("examined %d of the %d eligible symbols after the cursor before the budget of %d reference queries ran out; run again with --cursor %s",
			d.Examined, d.Eligible-d.SkippedByCursor, budget, strconv.Quote(d.NextCursor)))
	}
	warnings = append(warnings, "candidates, not verdicts: reflection, build tags, cgo, generated code and callers outside this workspace can hide a use; read the reasons before deleting")

	exit := ExitOK
	if d.Total > 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeDeadText(e.stdout, d, warnings, common.absolute)
		return exit
	}
	return e.writeData(common, d, warnings, exit)
}

// --- cursor ---

// A deadKey orders candidates: file, line, id.
type deadKey struct {
	file string
	line int
	id   string
}

func (k deadKey) less(o deadKey) bool {
	if k.file != o.file {
		return k.file < o.file
	}
	if k.line != o.line {
		return k.line < o.line
	}
	return k.id < o.id
}

// cursor is `<line>|<id>`; the file is the id's path.
func (k deadKey) cursor() string { return strconv.Itoa(k.line) + "|" + k.id }

func parseDeadCursor(s string) (*deadKey, error) {
	if s == "" {
		return nil, nil
	}
	line, id, ok := strings.Cut(s, "|")
	n, err := strconv.Atoi(line)
	file, _, hasID := strings.Cut(id, idSeparator)
	if !ok || err != nil || n < 0 || !hasID || file == "" {
		return nil, render.Errorf(render.CodeUsage, "dead_code: --cursor %q is not a next_cursor of a previous answer (want <line>|<symbol id>)", s)
	}
	return &deadKey{file: file, line: n, id: id}, nil
}

// --- exclusions ---

type deadRules struct {
	names []string
	paths *index.GlobSet
}

func newDeadRules(root string, cfg *serverdef.DeadCodeConfig) (*deadRules, error) {
	r := &deadRules{names: append([]string(nil), cfg.IgnoreNames...)}
	if len(cfg.IgnorePaths) > 0 {
		gs, err := index.CompileGlobs(root, cfg.IgnorePaths)
		if err != nil {
			return nil, render.Errorf(render.CodeInvalidConfig, ".lightspeed.toml [dead_code] ignore_paths: %v", err)
		}
		r.paths = gs
	}
	return r, nil
}

// ignoredBy reports whether the workspace's own ignore lists name the symbol: a
// bare name, a `Type.Name` or a pattern with `*`, against the symbol's name and
// its qualified name; or a path glob against its file.
func (r *deadRules) ignoredBy(file string, s index.Symbol) bool {
	if r.paths != nil && r.paths.Allows(file) {
		return true
	}
	qualified := symbols.IDName(s.Qualified)
	for _, pat := range r.names {
		for _, name := range []string{s.Name, s.Qualified, qualified, lastSegment(qualified)} {
			if ok, _ := path.Match(pat, name); ok {
				return true
			}
		}
	}
	return false
}

// entryPoint reports whether a symbol is called by the runtime or the test
// framework rather than by code: main, init, Go's Test/Benchmark/Example/Fuzz,
// Python's dunder names.
func isDeadEntryPoint(lang string, s index.Symbol) bool {
	name := lastSegment(symbols.IDName(s.Qualified))
	switch {
	case name == "main" || name == "init" || name == "TestMain":
		return true
	case lang == "go":
		for _, prefix := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
			if strings.HasPrefix(name, prefix) {
				rest := strings.TrimPrefix(name, prefix)
				r, _ := utf8.DecodeRuneInString(rest)
				if rest == "" || !unicode.IsLower(r) {
					return s.Kind == "function"
				}
			}
		}
	case lang == "python":
		return strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
	}
	return false
}

// deadExport is whether a symbol is part of its language's public API, by the
// only rule each language gives: Go's capital letter, Rust's `pub`, a JS/TS
// `export`, C's absence of `static`, Lua's `local`, Python's leading
// underscore (which is a convention, so a name without one is "unknown", not
// "yes"). Any other language is "unknown".
func deadExport(lang string, s index.Symbol, declLine string) string {
	name := lastSegment(symbols.IDName(s.Qualified))
	line := strings.TrimSpace(declLine)
	switch lang {
	case "go":
		r, _ := utf8.DecodeRuneInString(name)
		if unicode.IsUpper(r) {
			return "yes"
		}
		return "no"
	case "python":
		if strings.HasPrefix(name, "_") {
			return "no"
		}
		return "unknown"
	case "typescript", "javascript", "tsx", "jsx":
		switch {
		case strings.HasPrefix(line, "export"):
			return "yes"
		case s.Kind == "method" || s.Container != "":
			if strings.HasPrefix(line, "private") || strings.HasPrefix(line, "#") || strings.HasPrefix(name, "#") {
				return "no"
			}
			return "unknown"
		}
		return "no"
	case "rust":
		if strings.HasPrefix(line, "pub(") {
			return "no"
		}
		if strings.HasPrefix(line, "pub ") || line == "pub" {
			return "yes"
		}
		return "no"
	case "c", "cpp":
		if strings.HasPrefix(line, "static") {
			return "no"
		}
		return "unknown"
	case "lua":
		if strings.HasPrefix(line, "local") {
			return "no"
		}
		return "unknown"
	}
	return "unknown"
}

// needsDeclLine says whether deadExport reads the declaration's source line.
func needsDeclLine(lang string) bool {
	switch lang {
	case "typescript", "javascript", "tsx", "jsx", "rust", "c", "cpp", "lua":
		return true
	}
	return false
}

// reflective are the languages where a name can be built from a string, so that
// no reference exists in the text.
var deadReflective = map[string]bool{"python": true, "javascript": true, "typescript": true, "tsx": true, "jsx": true, "ruby": true, "lua": true, "php": true}

var goBuildSuffix = regexp.MustCompile(`_(aix|android|darwin|dragonfly|freebsd|hurd|illumos|ios|js|linux|nacl|netbsd|openbsd|plan9|solaris|wasip1|windows|zos|386|amd64|arm|arm64|loong64|mips|mipsle|mips64|mips64le|ppc64|ppc64le|riscv64|s390x|wasm)(_[a-z0-9]+)?\.go$`)

// goBuildSensitive reports whether a Go file is compiled only under some
// conditions — a build constraint, a GOOS/GOARCH file name, or cgo — so that a
// use of one of its symbols can live in a file the server did not load.
func goBuildSensitive(rel, text string) bool {
	if goBuildSuffix.MatchString(rel) {
		return true
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "package ") {
			break
		}
		if strings.HasPrefix(t, "//go:build") || strings.HasPrefix(t, "// +build") {
			return true
		}
	}
	return strings.Contains(text, "import \"C\"")
}

// --- the sweep ---

// A deadEntry is a symbol that has passed every exclusion and needs a query.
type deadEntry struct {
	key      deadKey
	file     string
	lang     string
	sym      index.Symbol
	exported string
	// sensitive marks a Go file compiled only under some conditions.
	sensitive bool
}

// A deadFile is one file as a server outlines it, for the positions of its symbols.
type deadFile struct {
	ff   *fileSymbols
	byID map[string]int
}

type deadSweep struct {
	e               *env
	common          *commonFlags
	root            string
	rules           *deadRules
	includeExported bool

	texts    map[string][]string
	sessions map[string]*session
	files    map[string]*deadFile
	warnings []string
	seenWarn map[string]bool
	// noImpl remembers servers that cannot answer implementation.
	noImpl map[string]bool
}

func (s *deadSweep) close() {
	for _, sess := range s.sessions {
		sess.close()
	}
}

func (s *deadSweep) warn(msg string) {
	if !s.seenWarn[msg] {
		s.seenWarn[msg] = true
		s.warnings = append(s.warnings, msg)
	}
}

// fileLines reads a workspace file's lines, once.
func (s *deadSweep) fileLines(rel string) []string {
	if lines, ok := s.texts[rel]; ok {
		return lines
	}
	data, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(rel)))
	var lines []string
	if err == nil {
		lines = strings.Split(string(data), "\n")
	}
	s.texts[rel] = lines
	return lines
}

// eligible applies the exclusions that need no query and returns what is left,
// sorted, counting each exclusion in d.
func (s *deadSweep) eligible(out *index.SymbolsOutcome, d *deadCodeData) []deadEntry {
	var list []deadEntry
	for i := range out.Files {
		fs := &out.Files[i]
		for _, sym := range fs.Symbols {
			d.Symbols++
			switch {
			case isTestPath(fs.File):
				d.Excluded[deadTestFile]++
				continue
			case isDeadEntryPoint(fs.Language, sym):
				d.Excluded[deadEntryPoint]++
				continue
			case s.rules.ignoredBy(fs.File, sym) || (sym.Kind == "method" && matchesAny(deadIgnoreMethodNames, lastSegment(symbols.IDName(sym.Qualified)))):
				d.Excluded[deadIgnored]++
				continue
			case s.isLocal(fs, sym):
				d.Excluded[deadLocal]++
				continue
			}
			decl := ""
			if needsDeclLine(fs.Language) {
				if lines := s.fileLines(fs.File); sym.Line >= 1 && sym.Line <= len(lines) {
					decl = lines[sym.Line-1]
				}
			}
			exported := deadExport(fs.Language, sym, decl)
			if exported == "yes" && !s.includeExported {
				d.Excluded[deadExported]++
				continue
			}
			sensitive := false
			if fs.Language == "go" {
				sensitive = goBuildSensitive(fs.File, strings.Join(s.fileLines(fs.File), "\n"))
			}
			list = append(list, deadEntry{key: deadKey{fs.File, sym.Line, sym.ID}, file: fs.File, lang: fs.Language,
				sym: sym, exported: exported, sensitive: sensitive})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].key.less(list[j].key) })
	return list
}

// isLocal reports a symbol declared inside a function or method body: reached
// only from that body, and the compiler's business.
func (s *deadSweep) isLocal(fs *index.FileSymbols, sym index.Symbol) bool {
	for p := sym.Parent; p > 0 && p <= len(fs.Symbols); p = fs.Symbols[p-1].Parent {
		if k := fs.Symbols[p-1].Kind; k == "function" || k == "method" {
			return true
		}
	}
	return false
}

func matchesAny(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// examine queries the eligible symbols after the cursor, within the budget.
func (s *deadSweep) examine(list []deadEntry, after *deadKey, d *deadCodeData) error {
	d.Complete = true
	var last deadKey
	for _, ent := range list {
		if after != nil && !after.less(ent.key) {
			d.SkippedByCursor++
			continue
		}
		if d.Queries.References >= d.Budget {
			d.Complete = false
			d.NextCursor = last.cursor()
			return nil
		}
		last = ent.key
		d.Examined++
		verdict, err := s.check(ent, d)
		if err != nil {
			return err
		}
		switch verdict.state {
		case deadUsed:
			d.Used++
		case deadUnresolved:
			d.Excluded[deadUnresolved]++
		case deadImplements:
			d.Excluded[deadImplements]++
		default:
			d.Candidates = append(d.Candidates, verdict.candidate)
		}
	}
	return nil
}

const deadUsed = "used"

type deadVerdict struct {
	state     string
	candidate deadCandidate
}

// sessionFor finds (or opens) the session of the server that handles file.
func (s *deadSweep) sessionFor(abs string) (*session, error) {
	match, err := s.e.resolveTarget(abs, "", s.common.server)
	if err != nil {
		return nil, err
	}
	key := match.Server.Name + "\x00" + match.Root
	if sess, ok := s.sessions[key]; ok {
		return sess, nil
	}
	ctx, cancel := context.WithTimeout(s.e.base(), s.common.timeout)
	defer cancel()
	sess, err := startSessionWith(ctx, s.e, match, sessionOptions{gate: s.common.gateOptions()})
	if err != nil {
		return nil, err
	}
	s.sessions[key] = sess
	return sess, nil
}

// outline is the server's own outline of a file, cached.
func (s *deadSweep) outline(sess *session, rel string) (*deadFile, error) {
	if f, ok := s.files[rel]; ok {
		return f, nil
	}
	ff, err := sess.loadFile(s.root, filepath.Join(s.root, filepath.FromSlash(rel)))
	if err != nil {
		return nil, err
	}
	f := &deadFile{ff: ff, byID: map[string]int{}}
	for i, id := range ff.ids {
		f.byID[id] = i
	}
	s.files[rel] = f
	return f, nil
}

// check runs the queries for one symbol.
func (s *deadSweep) check(ent deadEntry, d *deadCodeData) (deadVerdict, error) {
	abs := filepath.Join(s.root, filepath.FromSlash(ent.file))
	sess, err := s.sessionFor(abs)
	if err != nil {
		return deadVerdict{}, err
	}
	f, err := s.outline(sess, ent.file)
	if err != nil {
		if code := render.CodeForError(err); code == render.CodeNotReady || code == render.CodeUnsupportedMethod ||
			code == render.CodeServerNotInstalled || code == render.CodeCancelled || code == render.CodeTimeout {
			return deadVerdict{}, err
		}
		s.warn(fmt.Sprintf("could not outline %s to find the position of its symbols: %v", ent.file, err))
		return deadVerdict{state: deadUnresolved}, nil
	}
	i, ok := f.byID[ent.sym.ID]
	if !ok || !f.ff.syms[i].HasRange {
		s.warn(fmt.Sprintf("%s: the server's outline of %s does not list it (the index and the server disagree); skipped", ent.sym.ID, ent.file))
		return deadVerdict{state: deadUnresolved}, nil
	}
	sym := f.ff.syms[i]
	params := textDocumentPosition(f.ff.doc.URI, sym.Range.Start)
	params["context"] = map[string]any{"includeDeclaration": false}

	locs, err := s.locations(sess, methodReferences, params)
	if err != nil {
		return deadVerdict{}, err
	}
	d.Queries.References++
	decl := sym.Full
	if decl == (protocol.Range{}) {
		decl = sym.Range
	}
	for _, loc := range locs {
		if loc.URI.Path() == f.ff.doc.URI.Path() && !posBefore(loc.Range.Start, decl.Start) && !posBefore(decl.End, loc.Range.Start) {
			continue // inside its own declaration: recursion, not a use
		}
		return deadVerdict{state: deadUsed}, nil
	}

	canImpl := sess.lsp.Supports(methodImplementation)
	if ent.sym.Kind == "method" {
		if canImpl {
			impl, err := s.locations(sess, methodImplementation, textDocumentPosition(f.ff.doc.URI, sym.Range.Start))
			if err != nil {
				return deadVerdict{}, err
			}
			d.Queries.Implementation++
			for _, loc := range impl {
				if loc.URI.Path() == f.ff.doc.URI.Path() && loc.Range.Start.Line == sym.Range.Start.Line {
					continue // itself
				}
				return deadVerdict{state: deadImplements}, nil
			}
		}
	}
	return deadVerdict{state: "candidate", candidate: s.candidate(ent, canImpl)}, nil
}

// locations issues one gated locations request.
func (s *deadSweep) locations(sess *session, method string, params any) ([]protocol.Location, error) {
	ctx, cancel := context.WithTimeout(sess.base, s.common.timeout+gateSlack)
	defer cancel()
	res, err := sess.query(ctx, method, params)
	if err != nil {
		return nil, err
	}
	for _, w := range res.Warnings {
		s.warn(w)
	}
	return decodeLocations(res.Result)
}

// candidate assembles the row and its confidence: the lowest any reason allows.
func (s *deadSweep) candidate(ent deadEntry, canImpl bool) deadCandidate {
	conf := "high"
	var reasons []string
	lower := func(to, why string) {
		reasons = append(reasons, why)
		if to == "low" || (to == "medium" && conf == "high") {
			conf = to
		}
	}
	switch ent.exported {
	case "yes":
		lower("low", "exported: part of the public API, so callers outside this workspace are invisible")
	case "unknown":
		lower("low", "cannot tell whether it is public API in "+orUnknown(ent.lang))
	}
	if ent.sensitive {
		lower("low", "the file has a build constraint, a GOOS/GOARCH name or cgo: a use may be in a file that was not built")
	}
	if ent.sym.Kind == "method" {
		if canImpl {
			lower("medium", "a method: it may satisfy an interface declared outside this workspace, or be called by reflection")
		} else {
			lower("low", "a method, and the server cannot say whether it implements an interface")
		}
	}
	if deadReflective[ent.lang] {
		lower("medium", ent.lang+" resolves names at run time: a use may be built from a string")
	}
	return deadCandidate{ID: ent.sym.ID, Name: ent.sym.Qualified, Kind: ent.sym.Kind, File: ent.file, Line: ent.sym.Line,
		EndLine: ent.sym.EndLine, Exported: ent.exported, Confidence: conf, Reasons: reasons}
}

func orUnknown(lang string) string {
	if lang == "" {
		return "this language"
	}
	return lang
}

// --- text ---

func writeDeadText(w io.Writer, d *deadCodeData, warnings []string, absolute bool) {
	fmt.Fprintf(w, "dead_code: %d candidate(s); examined %d of %d eligible symbols (%d in scope; %d have references)\n",
		d.Total, d.Examined, d.Eligible-d.SkippedByCursor, d.Symbols, d.Used)
	if len(d.Excluded) > 0 {
		keys := make([]string, 0, len(d.Excluded))
		for k := range d.Excluded {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("%s %d", k, d.Excluded[k]))
		}
		fmt.Fprintf(w, "excluded: %s\n", strings.Join(parts, ", "))
	}
	for _, c := range d.Candidates {
		file := c.File
		if absolute {
			file = filepath.Join(d.Root, file)
		}
		fmt.Fprintf(w, "%s:%d: %s %s [%s]", file, c.Line, c.Kind, c.Name, c.Confidence)
		if len(c.Reasons) > 0 {
			fmt.Fprintf(w, " (%s)", strings.Join(c.Reasons, "; "))
		}
		fmt.Fprintf(w, "  %s\n", c.ID)
	}
	if d.NextCursor != "" {
		fmt.Fprintf(w, "next_cursor: %s\n", d.NextCursor)
	}
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}
