package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// `search_text`: full-text search over the workspace's files, read from the
// working tree at the moment of the call, with no index to go stale and no
// language server needed (docs/DECISIONS.md D26). It is the call an agent makes
// for what a symbol search cannot find: a string, a comment, a config value, a
// name that is not a declaration.

const (
	// defaultSearchLimit is how many matching lines `search_text` returns
	// unless told otherwise.
	defaultSearchLimit = 50
	// defaultMaxFileBytes is the largest file that is searched; a larger one
	// is skipped and named in the warnings. It is a search, not a read: a
	// generated bundle or a data dump that big is nearly always noise.
	defaultMaxFileBytes = 1 << 20
	// searchLineBytes is the longest line (or context line) printed whole. A
	// longer one is cut to a window around the match, with a marker.
	searchLineBytes = 240
	// maxSkipNames is how many skipped files a warning names.
	maxSkipNames = 5
	// symbolTimeout bounds --with-symbol's wait for a server: the symbol is
	// an extra, and a workspace that is still indexing must not hold the
	// search for the whole --timeout.
	symbolTimeout = 10 * time.Second
	// maxQueryBytes bounds the pattern. A longer one is a mistake, not a search.
	maxQueryBytes = 4096
)

// A searchMatch is one matching line.
type searchMatch struct {
	// File is the path relative to the workspace root, with forward slashes.
	File string `json:"file"`
	// Line is 1-based and Col is the 1-based *byte* column of the first
	// match on the line, in the line as it is on disk (not as clipped).
	Line int `json:"line"`
	Col  int `json:"col"`
	// Text is the line, without its terminator; a line longer than
	// searchLineBytes is cut to a window around the match and carries a
	// `[+NB]…` marker at each cut end, and Clipped.
	Text    string `json:"text"`
	Clipped bool   `json:"clipped,omitempty"`
	// Hits is how many matches the line has, when more than one.
	Hits int `json:"hits,omitempty"`
	// Before and After are the --context lines, nearest the match last and
	// first respectively.
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
	// Symbol is the id of the innermost symbol that contains the match, with
	// --with-symbol and a server that could say.
	Symbol string `json:"symbol,omitempty"`

	// pos16 is the UTF-16 column of the match, for the symbol lookup.
	pos16 int
}

// searchData is the payload of `search_text`.
type searchData struct {
	// Root is the workspace root every path is relative to.
	Root string `json:"root"`
	// Dir is the searched directory or file relative to Root, when it is not
	// the root.
	Dir           string   `json:"dir,omitempty"`
	Query         string   `json:"query"`
	Regex         bool     `json:"regex,omitempty"`
	CaseSensitive bool     `json:"case_sensitive,omitempty"`
	Word          bool     `json:"word,omitempty"`
	Globs         []string `json:"globs,omitempty"`
	// Source is how the files were listed: "git", "walk", or "file".
	Source  string        `json:"source"`
	Matches []searchMatch `json:"matches"`
	// Count is how many matching lines this output lists, Total how many
	// there were. They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
	// FilesSearched counts the files that were read and searched, FilesMatched
	// those with a match, FilesSkipped those left out (binary, too large,
	// unreadable, outside the workspace); each skip is in the warnings.
	FilesSearched int `json:"files_searched"`
	FilesMatched  int `json:"files_matched"`
	FilesSkipped  int `json:"files_skipped,omitempty"`
}

// stringList is a repeatable string flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(s string) error { *l = append(*l, s); return nil }

// A matcher finds the matches in one line.
type matcher struct {
	// lit is the pattern for a case-sensitive substring search; otherwise re
	// is set (regex mode, or a case-insensitive substring, which RE2's case
	// folding does correctly for non-ASCII too).
	lit  []byte
	re   *regexp.Regexp
	word bool
	// whole is the file-level prefilter: a file it rejects has no matching
	// line. Nil when nothing safe can be said (regex mode, where ^ and \A
	// mean different things on a line and on a file).
	whole func(content []byte) bool
}

// newMatcher compiles the query. The pattern is RE2 (Go's regexp), so its
// running time is linear in the input and it has no catastrophic
// backtracking; one that does not compile is a usage error.
func newMatcher(query string, regex, caseSensitive, word bool) (*matcher, error) {
	if query == "" {
		return nil, render.Errorf(render.CodeUsage, "search_text: the query is empty")
	}
	if len(query) > maxQueryBytes {
		return nil, render.Errorf(render.CodeUsage, "search_text: the query is %d bytes; the limit is %d", len(query), maxQueryBytes)
	}
	m := &matcher{word: word}
	switch {
	case !regex && caseSensitive:
		m.lit = []byte(query)
		m.whole = func(content []byte) bool { return bytes.Contains(content, m.lit) }
	default:
		pattern := query
		if !regex {
			pattern = regexp.QuoteMeta(query)
		}
		if !caseSensitive {
			pattern = "(?i)" + pattern
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, render.Errorf(render.CodeUsage, "search_text: %q is not a valid regular expression (RE2): %v", query, err)
		}
		m.re = re
		if !regex {
			m.whole = re.Match
		}
	}
	return m, nil
}

// isWordRune is the --word rule: letters and digits of any script, and the
// underscore. \b would be ASCII-only, and a word of CJK has no ASCII edge.
func isWordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// boundaryOK reports whether line[s:e] stands alone as a word.
func boundaryOK(line []byte, s, e int) bool {
	if s > 0 {
		if r, _ := utf8.DecodeLastRune(line[:s]); isWordRune(r) {
			return false
		}
	}
	if e < len(line) {
		if r, _ := utf8.DecodeRune(line[e:]); isWordRune(r) {
			return false
		}
	}
	return true
}

// find reports the first accepted match in the line and how many there are;
// hits is 0 for none.
func (m *matcher) find(line []byte) (start, end, hits int) {
	accept := func(s, e int) {
		if m.word && (s == e || !boundaryOK(line, s, e)) {
			return
		}
		if hits == 0 {
			start, end = s, e
		}
		hits++
	}
	if m.lit != nil {
		for pos := 0; pos <= len(line); {
			i := bytes.Index(line[pos:], m.lit)
			if i < 0 {
				break
			}
			s := pos + i
			accept(s, s+len(m.lit))
			pos = s + len(m.lit)
		}
		return start, end, hits
	}
	if !m.re.Match(line) {
		return 0, 0, 0
	}
	for _, loc := range m.re.FindAllIndex(line, -1) {
		accept(loc[0], loc[1])
	}
	return start, end, hits
}

// clipLine is the line as printed: whole if it fits, and otherwise a window of
// about max bytes around [start,end), on rune boundaries, with `[+NB]…` and
// `…[+NB]` saying how many bytes were cut from each end.
func clipLine(line []byte, start, end, max int) (string, bool) {
	if len(line) <= max {
		return string(line), false
	}
	lo, hi := start, end
	if hi-lo > max {
		hi = lo + max
	} else {
		pad := (max - (hi - lo)) / 2
		lo, hi = lo-pad, hi+pad
	}
	if lo < 0 {
		hi -= lo
		lo = 0
	}
	if hi > len(line) {
		lo -= hi - len(line)
		hi = len(line)
		if lo < 0 {
			lo = 0
		}
	}
	for lo < len(line) && !utf8.RuneStart(line[lo]) {
		lo++
	}
	for hi > lo && hi < len(line) && !utf8.RuneStart(line[hi]) {
		hi--
	}
	var b strings.Builder
	if lo > 0 {
		fmt.Fprintf(&b, "[+%dB]…", lo)
	}
	b.Write(line[lo:hi])
	if hi < len(line) {
		fmt.Fprintf(&b, "…[+%dB]", len(line)-hi)
	}
	return b.String(), true
}

// A globSet is the --glob include and exclude patterns.
type globSet struct {
	include, exclude []*router.Glob
	root             string
}

// compileGlobs reads --glob patterns: a pattern starting with `!` excludes, any
// other includes, and an included file must match at least one include when
// there are any. A pattern with no `/` matches at any depth (`*.go` is
// `**/*.go`), and any pattern also matches everything below a directory of
// that name (`internal/cli` is `internal/cli/**`), as in ripgrep. Patterns are
// the router's globs (`**`, `{a,b}`, `[a-z]`), matched against the path
// relative to the workspace.
func compileGlobs(root string, patterns []string) (*globSet, error) {
	gs := &globSet{root: root}
	for _, p := range patterns {
		exclude := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if p == "" {
			return nil, render.Errorf(render.CodeUsage, "search_text: empty --glob pattern")
		}
		forms := []string{p}
		if !strings.HasPrefix(p, "/") {
			p = strings.TrimPrefix(p, "./")
			if !strings.Contains(p, "/") {
				p = "**/" + p
			}
			forms = []string{p}
		}
		if !strings.HasSuffix(forms[0], "**") {
			forms = append(forms, strings.TrimSuffix(forms[0], "/")+"/**")
		}
		for _, f := range forms {
			g, err := router.CompileGlob(f)
			if err != nil {
				return nil, render.Errorf(render.CodeUsage, "search_text: --glob: %v", err)
			}
			if exclude {
				gs.exclude = append(gs.exclude, g)
			} else {
				gs.include = append(gs.include, g)
			}
		}
	}
	return gs, nil
}

func (gs *globSet) matches(globs []*router.Glob, rel string) bool {
	for _, g := range globs {
		p := rel
		if g.Absolute() {
			p = filepath.ToSlash(filepath.Join(gs.root, filepath.FromSlash(rel)))
		}
		if g.Match(p) {
			return true
		}
	}
	return false
}

// allows reports whether a workspace-relative path passes the globs.
func (gs *globSet) allows(rel string) bool {
	if gs == nil {
		return true
	}
	if len(gs.include) > 0 && !gs.matches(gs.include, rel) {
		return false
	}
	return !gs.matches(gs.exclude, rel)
}

// Why a file was not searched.
const (
	skipBinary = iota
	skipLarge
	skipUnreadable
	skipOutside
	skipSpecial
	skipKinds
)

// A fileScan is the result of searching one file.
type fileScan struct {
	skipped int // a skip* + 1, 0 when searched
	detail  string
	total   int
	matches []searchMatch
}

// scanFile searches one file for the matcher. Only the first `keep` matching
// lines are kept, with their context, but every one is counted, so the total is
// exact whatever the limit.
func scanFile(root, rel string, m *matcher, keep, context, maxBytes int, withSymbol bool) fileScan {
	abs := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		return fileScan{skipped: skipUnreadable + 1, detail: rel}
	}
	if info.Mode()&os.ModeSymlink != 0 {
		// A link in the workspace that leads out of it is out of it: what it
		// points at must not be read (D23).
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return fileScan{skipped: skipUnreadable + 1, detail: rel}
		}
		if _, in := relToRoot(root, real); !in {
			return fileScan{skipped: skipOutside + 1, detail: rel}
		}
		if info, err = os.Stat(real); err != nil {
			return fileScan{skipped: skipUnreadable + 1, detail: rel}
		}
	}
	if !info.Mode().IsRegular() {
		return fileScan{skipped: skipSpecial + 1, detail: rel}
	}
	if info.Size() > int64(maxBytes) {
		return fileScan{skipped: skipLarge + 1, detail: fmt.Sprintf("%s (%d bytes)", rel, info.Size())}
	}
	content, err := os.ReadFile(abs)
	if err != nil {
		return fileScan{skipped: skipUnreadable + 1, detail: rel}
	}
	if bytes.IndexByte(content[:min(len(content), binarySniff)], 0) >= 0 {
		return fileScan{skipped: skipBinary + 1, detail: rel}
	}
	var out fileScan
	if m.whole != nil && !m.whole(content) {
		return out
	}

	for lineNo, off := 1, 0; off < len(content); lineNo++ {
		end := bytes.IndexByte(content[off:], '\n')
		next := len(content)
		if end < 0 {
			end = len(content)
		} else {
			end += off
			next = end + 1
		}
		line := content[off:end]
		line = bytes.TrimSuffix(line, []byte("\r"))
		if s, e, hits := m.find(line); hits > 0 {
			out.total++
			if len(out.matches) < keep {
				sm := searchMatch{File: rel, Line: lineNo, Col: s + 1}
				sm.Text, sm.Clipped = clipLine(line, s, e, searchLineBytes)
				if hits > 1 {
					sm.Hits = hits
				}
				if withSymbol {
					sm.pos16 = len(utf16.Encode([]rune(string(line[:s]))))
				}
				if context > 0 {
					sm.Before, sm.After = contextLines(content, off, next, context)
				}
				out.matches = append(out.matches, sm)
			}
		}
		off = next
	}
	return out
}

// contextLines are up to n lines before the line at [off,next) and after it.
func contextLines(content []byte, off, next, n int) (before, after []string) {
	clip := func(l []byte) string {
		s, _ := clipLine(bytes.TrimSuffix(l, []byte("\r")), 0, 0, searchLineBytes)
		return s
	}
	for i, start := 0, off; i < n && start > 0; i++ {
		prev := bytes.LastIndexByte(content[:start-1], '\n') + 1
		before = append(before, clip(content[prev:start-1]))
		start = prev
	}
	slices.Reverse(before)
	for i, start := 0, next; i < n && start < len(content); i++ {
		end := bytes.IndexByte(content[start:], '\n')
		nextStart := len(content)
		if end < 0 {
			end = len(content)
		} else {
			end += start
			nextStart = end + 1
		}
		after = append(after, clip(content[start:end]))
		start = nextStart
	}
	return before, after
}

// skipNote words a skip warning, naming the first few files.
func skipNote(what string, names []string) string {
	sort.Strings(names)
	shown := names
	more := ""
	if len(shown) > maxSkipNames {
		more = fmt.Sprintf(" and %d more", len(shown)-maxSkipNames)
		shown = shown[:maxSkipNames]
	}
	return fmt.Sprintf("skipped %d %s: %s%s", len(names), what, strings.Join(shown, ", "), more)
}

// searchTextCommand implements `lightspeed search_text <query>`.
func searchTextCommand(e *env, c *command, args []string) int {
	var (
		regex, caseSensitive, word, withSymbol bool
		globs                                  stringList
		pathFlag, rootFlag                     string
		maxFileBytes                           int
		fset                                   *flag.FlagSet
	)
	common, positional, err := parseFlagsRange(e, c, args, 1, 1, func(fs *flag.FlagSet) {
		fset = fs
		fs.BoolVar(&regex, "regex", false, "treat the query as a regular expression (Go RE2: linear time, no backtracking)")
		fs.BoolVar(&caseSensitive, "case-sensitive", false, "match case (the default is case-insensitive)")
		fs.BoolVar(&word, "word", false, "match whole words only (letters, digits and _ of any script)")
		fs.Var(&globs, "glob", "only files matching this glob, relative to the workspace (repeatable; a leading ! excludes; no / matches at any depth)")
		fs.StringVar(&pathFlag, "path", "", "search only this directory or file, inside the workspace (default: the workspace, or --root)")
		fs.StringVar(&rootFlag, "root", ".", "any directory inside the workspace to search; --path must be inside it too")
		fs.IntVar(&maxFileBytes, "max-file-bytes", defaultMaxFileBytes, "skip files larger than this, and say so")
		fs.BoolVar(&withSymbol, "with-symbol", false, "add the id of the symbol containing each match, when a language server can say")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "search_text", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if maxFileBytes < 1 {
		return e.usagef("search_text: --max-file-bytes must be at least 1 (got %d)", maxFileBytes)
	}
	limit := common.limit
	if limit == 0 {
		limited := false
		fset.Visit(func(f *flag.Flag) { limited = limited || f.Name == "limit" })
		if !limited {
			limit = defaultSearchLimit
		}
	}
	m, err := newMatcher(positional[0], regex, caseSensitive, word)
	if err != nil {
		return e.fail(err)
	}

	root, err := commandWorkspace(rootFlag)
	if err != nil {
		return e.fail(err)
	}
	gs, err := compileGlobs(root, globs)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), common.timeout)
	defer cancel()
	target := pathFlag
	if target == "" {
		target = rootFlag
	}
	listing, err := searchFileSet(ctx, target, root)
	if err != nil {
		return e.fail(err)
	}
	var files []string
	for _, f := range listing.Files {
		if gs.allows(f) {
			files = append(files, f)
		}
	}

	keep := limit
	if limit == 0 {
		keep = int(^uint(0) >> 1)
	}
	scans := make([]fileScan, len(files))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), max(len(files), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				scans[i] = scanFile(root, files[i], m, keep, common.context, maxFileBytes, withSymbol)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return e.fail(render.Errorf(render.CodeTimeout,
				"search_text did not finish within --timeout %s; narrow it with --path or --glob", common.timeout))
		}
		return e.fail(render.Errorf(render.CodeCancelled, "search_text was cancelled"))
	}

	data := searchData{
		Root: root, Query: positional[0], Regex: regex, CaseSensitive: caseSensitive, Word: word,
		Globs: globs, Source: listing.Source, Limit: limit, Matches: []searchMatch{},
	}
	if abs, err := filepath.Abs(target); err == nil {
		if rel, in := relToRoot(root, abs); in && rel != "." {
			data.Dir = rel
		}
	}
	skips := make([][]string, skipKinds)
	for _, s := range scans {
		if s.skipped > 0 {
			data.FilesSkipped++
			skips[s.skipped-1] = append(skips[s.skipped-1], s.detail)
			continue
		}
		data.FilesSearched++
		if s.total > 0 {
			data.FilesMatched++
		}
		data.Total += s.total
		for _, sm := range s.matches {
			if limit == 0 || len(data.Matches) < limit {
				data.Matches = append(data.Matches, sm)
			}
		}
	}
	data.Count = len(data.Matches)
	data.Truncated = data.Count < data.Total

	warnings := slices.Clone(listing.Warnings)
	for kind, what := range []string{
		fmt.Sprintf("binary file(s) (a NUL byte in the first %d bytes)", binarySniff),
		fmt.Sprintf("file(s) over --max-file-bytes %d", maxFileBytes),
		"unreadable file(s)",
		"symlink(s) that lead outside the workspace",
		"file(s) that are not regular files",
	} {
		if len(skips[kind]) > 0 {
			warnings = append(warnings, skipNote(what, skips[kind]))
		}
	}
	if data.Truncated {
		warnings = append(warnings, fmt.Sprintf(
			"listed %d of %d matching lines; narrow it with --path or --glob, or raise --limit", data.Count, data.Total))
	}
	if withSymbol && len(data.Matches) > 0 {
		warnings = append(warnings, attachSymbols(e, common, root, data.Matches)...)
	}

	exit := ExitOK
	if data.Total == 0 {
		exit = ExitProblems // an authoritative empty answer, as grep's
	}
	if format == render.FormatText {
		writeSearchText(e.stdout, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// searchFileSet lists what a search covers: the files under a directory, or
// the one file. It is confined to the workspace as `tree` and `file` are.
func searchFileSet(ctx context.Context, target, root string) (fileListing, error) {
	abs, err := filepath.Abs(target)
	if err == nil {
		if st, serr := os.Stat(abs); serr == nil && !st.IsDir() {
			_, rel, err := insideWorkspace(root, target)
			if err != nil {
				return fileListing{}, err
			}
			return fileListing{Files: []string{rel}, Source: "file"}, nil
		}
	}
	return listWorkspaceFiles(ctx, target, root)
}

// attachSymbols adds to each match the id of the symbol that contains it,
// asking the language server of its file for the outline. It is best effort:
// a file with no server, or one that cannot answer, leaves its matches without
// an id and is said once in the warnings, and a language whose first file
// failed is not asked again.
func attachSymbols(e *env, common *commonFlags, root string, matches []searchMatch) []string {
	bounded := *common
	bounded.timeout = min(bounded.timeout, symbolTimeout)
	ss := newSessionSet(e, &bounded, root)
	defer ss.close()

	failed := map[string]string{} // extension -> why
	var order []string
	for i := range matches {
		m := &matches[i]
		ext := strings.ToLower(path.Ext(m.File))
		if _, bad := failed[ext]; bad {
			continue
		}
		ff, err := ss.load(filepath.Join(root, filepath.FromSlash(m.File)))
		if err != nil {
			why := err.Error()
			var coded render.Coder
			if errors.As(err, &coded) {
				why = string(coded.ErrorCode())
			}
			failed[ext] = why
			order = append(order, ext)
			continue
		}
		pos := protocol.Position{Line: uint32(m.Line - 1), Character: uint32(m.pos16)}
		if idx := enclosingSymbol(ff.syms, pos); idx >= 0 && ff.ids != nil {
			m.Symbol = ff.ids[idx]
		}
	}
	warnings := ss.warnings()
	if len(order) > 0 {
		var parts []string
		for _, ext := range order {
			if ext == "" {
				ext = "(no extension)"
			}
			parts = append(parts, ext+": "+failed[ext])
		}
		warnings = append(warnings, "--with-symbol: no symbol ids for some matches ("+strings.Join(parts, "; ")+")")
	}
	return warnings
}

// writeSearchText prints matches grep-style, `file:line:col: text`, with
// context lines as `file-line- text` and `--` between groups that are not
// adjacent (only when there is context). The warnings — files skipped and why,
// truncation, --with-symbol gaps — follow as `# ` notice lines, as in every
// text format: a skip is never silent, whatever the format.
func writeSearchText(w io.Writer, d searchData, warnings []string) {
	type entry struct {
		line  int
		text  string
		match *searchMatch
	}
	var files []string
	byFile := map[string]map[int]entry{}
	for i := range d.Matches {
		m := &d.Matches[i]
		lines, ok := byFile[m.File]
		if !ok {
			lines = map[int]entry{}
			byFile[m.File] = lines
			files = append(files, m.File)
		}
		for j, t := range m.Before {
			n := m.Line - len(m.Before) + j
			if _, seen := lines[n]; !seen {
				lines[n] = entry{n, t, nil}
			}
		}
		for j, t := range m.After {
			n := m.Line + 1 + j
			if _, seen := lines[n]; !seen {
				lines[n] = entry{n, t, nil}
			}
		}
		lines[m.Line] = entry{m.Line, m.Text, m}
	}
	first, separate := true, d.hasContext()
	for _, f := range files {
		nums := make([]int, 0, len(byFile[f]))
		for n := range byFile[f] {
			nums = append(nums, n)
		}
		sort.Ints(nums)
		prev := -1
		for _, n := range nums {
			en := byFile[f][n]
			if !first && (prev < 0 || n != prev+1) && separate {
				fmt.Fprintln(w, "--")
			}
			first = false
			prev = n
			if en.match == nil {
				fmt.Fprintf(w, "%s-%d- %s\n", f, n, en.text)
				continue
			}
			fmt.Fprintf(w, "%s:%d:%d: %s", f, n, en.match.Col, en.text)
			if en.match.Symbol != "" {
				// The whole id, so that what the line says can be handed to
				// `source` or `context` as it is (D26, D31).
				fmt.Fprintf(w, "  [in %s]", en.match.Symbol)
			}
			fmt.Fprintln(w)
		}
	}
	for _, n := range warnings {
		fmt.Fprintf(w, "# %s\n", oneLine(n))
	}
}

// hasContext reports whether any match carries context lines.
func (d searchData) hasContext() bool {
	for _, m := range d.Matches {
		if len(m.Before) > 0 || len(m.After) > 0 {
			return true
		}
	}
	return false
}
