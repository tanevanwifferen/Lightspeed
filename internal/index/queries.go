package index

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// This file is the query surface of the index: what the daemon's `index`
// request dispatches to. Every query begins with a revalidation of the files it
// is about to report on (sync), builds what is stale, and only then reads the
// entries, so an answer is never older than the file it describes (D32).

// SearchOutcome is the answer to a symbol search and how fresh it is.
type SearchOutcome struct {
	SearchResult
	Report *SyncReport `json:"report"`
	// FileHashes are the content hashes the hits' files were indexed at, for a
	// caller that reads the files to inline source: a file whose hash is not
	// this one has changed since, and its line numbers are not to be trusted.
	FileHashes map[string]string `json:"file_hashes,omitempty"`
}

// Search revalidates the files the query can reach and ranks their symbols.
func (m *Manager) Search(ctx context.Context, q SearchQuery) (*SearchOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm, err := m.compileScope(Scope{Path: q.Path, Globs: q.Globs, Languages: q.Languages})
	if err != nil {
		return nil, err
	}
	_, rep, err := m.sync(ctx, NeedOutline, sm)
	if err != nil {
		return nil, err
	}
	var idx *SearchIndex
	if sm.empty() {
		if m.search == nil || m.searchGen != m.gen {
			m.search, m.searchGen = NewSearchIndex(m.fileList(nil, NeedOutline)), m.gen
		}
		idx = m.search
	} else {
		// Only what was revalidated: an entry outside the scope may be stale
		// and must not be able to reach the answer.
		idx = NewSearchIndex(m.fileList(sm, NeedOutline))
	}
	res, err := idx.Search(q)
	if err != nil {
		return nil, err
	}
	out := &SearchOutcome{SearchResult: res, Report: rep, FileHashes: map[string]string{}}
	for _, h := range res.Hits {
		if e := m.files[h.File]; e != nil {
			out.FileHashes[h.File] = e.Hash
		}
	}
	return out, nil
}

// RepoMapOutcome is a repository map and how fresh it is.
type RepoMapOutcome struct {
	RepoMapResult
	Report *SyncReport `json:"report"`
	// UncoveredLanguages are the languages among the mapped files that have
	// imports and no extractor: those files are ranked by their symbol count
	// alone, not by the import graph.
	UncoveredLanguages []string `json:"uncovered_languages,omitempty"`
}

// RepoMap revalidates the workspace (or the scope) and lays out its files by
// import-graph centrality.
func (m *Manager) RepoMap(ctx context.Context, opts RepoMapOptions) (*RepoMapOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm, err := m.compileScope(Scope{Path: opts.Path, Globs: opts.Globs, Languages: opts.Languages})
	if err != nil {
		return nil, err
	}
	snap, rep, err := m.sync(ctx, NeedOutline|NeedImports, sm)
	if err != nil {
		return nil, err
	}
	files := m.fileList(sm, NeedOutline)
	g := BuildGraph(files, newWorkspace(m.opts.Root, snap))
	res, err := RepoMap(files, g.PageRank(), opts)
	if err != nil {
		return nil, err
	}
	out := &RepoMapOutcome{RepoMapResult: res, Report: rep}
	out.UncoveredLanguages = uncovered(g)
	return out, nil
}

// ImportsOutcome is one file's imports.
type ImportsOutcome struct {
	ImportsResult
	Report *SyncReport `json:"report"`
}

// Imports lists what one file imports, resolved against the workspace.
func (m *Manager) Imports(ctx context.Context, rel string) (*ImportsOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm, _ := m.compileScope(Scope{Files: []string{rel}})
	snap, rep, err := m.sync(ctx, NeedImports, sm)
	if err != nil {
		return nil, err
	}
	if _, ok := snap.State[rel]; !ok {
		return nil, &NotFoundError{Path: rel}
	}
	files := m.fileList(sm, 0)
	if len(files) == 0 {
		// A file of a language nothing covers has no entry; the graph is told
		// so, and says "not covered" rather than "no imports".
		files = []*File{{Path: rel, Language: languageOf(rel)}}
	}
	g := BuildGraph(files, newWorkspace(m.opts.Root, snap))
	return &ImportsOutcome{ImportsResult: g.ImportsOf(rel), Report: rep}, nil
}

// A UsageError is a query the caller got wrong (a malformed glob). It carries
// the code and exit status of a usage error (exit 2) so that it survives the
// daemon socket as one.
type UsageError struct{ Msg string }

func (e *UsageError) Error() string { return e.Msg }
func (e *UsageError) Code() string  { return "usage" }
func (e *UsageError) ExitCode() int { return 2 }

// A NotFoundError says a path is not a file of the workspace (or not listed: it
// is ignored, or a symlink).
type NotFoundError struct{ Path string }

func (e *NotFoundError) Code() string  { return "no_such_file" }
func (e *NotFoundError) ExitCode() int { return 2 }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("%s is not a file of the workspace (missing, ignored, or not a regular file)", e.Path)
}

// ImportersOutcome is the files that import a target.
type ImportersOutcome struct {
	ImportersResult
	Report *SyncReport `json:"report"`
	// UncoveredLanguages are the languages present in the workspace with no
	// import extractor: their importers, if any, are not in the answer.
	UncoveredLanguages []string `json:"uncovered_languages,omitempty"`
}

// Importers lists the files that import a file, a directory (a Go package) or a
// Go import path.
func (m *Manager) Importers(ctx context.Context, target string) (*ImportersOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, snap, rep, err := m.wholeGraph(ctx)
	if err != nil {
		return nil, err
	}
	_ = snap
	res, err := g.ImportersOf(target)
	if err != nil {
		return nil, err
	}
	out := &ImportersOutcome{ImportersResult: res, Report: rep}
	out.UncoveredLanguages = uncovered(g)
	return out, nil
}

// GraphOutcome is a walk over the import graph.
type GraphOutcome struct {
	WalkResult
	Report             *SyncReport `json:"report"`
	UncoveredLanguages []string    `json:"uncovered_languages,omitempty"`
}

// DependencyGraph walks the import graph from root (empty: the whole graph).
func (m *Manager) DependencyGraph(ctx context.Context, root string, depth int, dir Direction, external bool) (*GraphOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, _, rep, err := m.wholeGraph(ctx)
	if err != nil {
		return nil, err
	}
	res, err := g.Walk(root, depth, dir, external)
	if err != nil {
		return nil, err
	}
	return &GraphOutcome{WalkResult: res, Report: rep, UncoveredLanguages: uncovered(g)}, nil
}

// CyclesOutcome is the import cycles of the workspace.
type CyclesOutcome struct {
	Cycles             [][]string  `json:"cycles"`
	Count              int         `json:"count"`
	Report             *SyncReport `json:"report"`
	UncoveredLanguages []string    `json:"uncovered_languages,omitempty"`
}

// Cycles reports the import cycles.
func (m *Manager) Cycles(ctx context.Context) (*CyclesOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g, _, rep, err := m.wholeGraph(ctx)
	if err != nil {
		return nil, err
	}
	cs := g.Cycles()
	if cs == nil {
		cs = [][]string{}
	}
	return &CyclesOutcome{Cycles: cs, Count: len(cs), Report: rep, UncoveredLanguages: uncovered(g)}, nil
}

// wholeGraph revalidates every file's imports and returns the workspace's
// graph. It needs no language server.
func (m *Manager) wholeGraph(ctx context.Context) (*Graph, *Snapshot, *SyncReport, error) {
	snap, rep, err := m.sync(ctx, NeedImports, nil)
	if err != nil {
		return nil, nil, nil, err
	}
	return BuildGraph(m.fileList(nil, 0), newWorkspace(m.opts.Root, snap)), snap, rep, nil
}

// noImportConcept are languages that have no imports to extract — data, prose
// and build files — and so are not worth warning about as "uncovered".
var noImportConcept = map[string]bool{
	"": true, "gomod": true, "gowork": true, "gosum": true, "json": true, "jsonc": true, "yaml": true,
	"toml": true, "markdown": true, "mdx": true, "restructuredtext": true, "xml": true, "html": true,
	"css": true, "scss": true, "less": true, "dockerfile": true, "make": true, "cmake": true,
	"latex": true, "sql": true, "gotmpl": true,
}

// uncovered lists the languages present in the graph that have imports and no
// extractor: their files have no edges, which is not the same as none.
func uncovered(g *Graph) []string {
	var out []string
	for _, c := range g.Coverage() {
		if !c.Covered && !noImportConcept[c.Language] {
			out = append(out, c.Language)
		}
	}
	return out
}

func languageOf(rel string) string {
	return routerLanguage(rel)
}

// BuildResult is what `index build` reports.
type BuildResult struct {
	Report *SyncReport `json:"report"`
	Status *Status     `json:"status"`
}

// Build revalidates and builds the whole workspace: the explicit warm-up.
func (m *Manager) Build(ctx context.Context) (*BuildResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, rep, err := m.sync(ctx, NeedOutline|NeedImports, nil)
	if err != nil {
		return nil, err
	}
	st, err := m.status(ctx)
	if err != nil {
		return nil, err
	}
	return &BuildResult{Report: rep, Status: st}, nil
}

// ClearResult is what `index clear` reports.
type ClearResult struct {
	CacheDir string `json:"cache_dir,omitempty"`
	// Entries is how many files the in-memory index held, Parts and Bytes what
	// was removed from disk.
	Entries int   `json:"entries"`
	Parts   int   `json:"parts"`
	Bytes   int64 `json:"bytes"`
}

// Clear forgets everything, in memory and on disk. The next query builds what
// it needs again.
func (m *Manager) Clear(context.Context) (*ClearResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	res := &ClearResult{CacheDir: m.opts.CacheDir, Entries: len(m.files)}
	m.files = map[string]*File{}
	m.parts = map[string]*partState{}
	m.skips = map[string]skipInfo{}
	m.dirty = map[string]bool{}
	m.search, m.lastBuild = nil, BuildInfo{}
	m.gen++
	if m.opts.CacheDir != "" {
		n, b, err := removeParts(m.opts.CacheDir)
		res.Parts, res.Bytes = n, b
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

// A ServerStatus is the index's coverage through one server.
type ServerStatus struct {
	Name string `json:"name"`
	// Files is how many workspace files the server handles, Indexed how many of
	// them have a current outline.
	Files   int `json:"files"`
	Indexed int `json:"indexed"`
	Symbols int `json:"symbols"`
	// Version is what the server reported when its outlines were built, "" when
	// they were loaded from disk and it has not run since.
	Version string `json:"version,omitempty"`
	Cached  bool   `json:"cached"`
}

// A LanguageStatus is the index's coverage of one language.
type LanguageStatus struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
	Server   string `json:"server,omitempty"`
	// Imports is whether an import extractor covers the language.
	Imports bool `json:"imports"`
	Indexed int  `json:"indexed"`
}

// A SkipGroup is files that are not indexed for one reason.
type SkipGroup struct {
	Reason   string   `json:"reason"`
	Count    int      `json:"count"`
	Examples []string `json:"examples,omitempty"`
}

// Status is what `index status` reports. It builds nothing.
type Status struct {
	Root         string `json:"root"`
	CacheDir     string `json:"cache_dir,omitempty"`
	CachePresent bool   `json:"cache_present"`
	CacheBytes   int64  `json:"cache_bytes"`
	Schema       int    `json:"schema"`
	Build        string `json:"build,omitempty"`
	// Files is every file of the workspace, Covered those a server or an
	// import extractor handles, Indexed those with an entry, Fresh those whose
	// entry is current by its stat, Stale those with an entry whose stat moved
	// (the next query decides by hash), Missing those with none.
	Files   int `json:"files"`
	Covered int `json:"covered"`
	Indexed int `json:"indexed"`
	Fresh   int `json:"fresh"`
	Stale   int `json:"stale"`
	Missing int `json:"missing"`
	// Removed is entries whose file is gone.
	Removed  int `json:"removed"`
	Symbols  int `json:"symbols"`
	Imports  int `json:"imports"`
	Outlined int `json:"outlined"`
	// Ready is whether every covered file has a current entry: a query would
	// build nothing.
	Warm      bool             `json:"warm"`
	Servers   []ServerStatus   `json:"servers"`
	Languages []LanguageStatus `json:"languages"`
	Skipped   []SkipGroup      `json:"skipped,omitempty"`
	// Uncovered are files no server and no import extractor covers, by
	// language, and are not skipped for a reason of their own.
	Uncovered []LanguageCount `json:"uncovered,omitempty"`
	Discarded []discard       `json:"discarded,omitempty"`
	LastBuild *BuildInfo      `json:"last_build,omitempty"`
	Source    string          `json:"source"`
	Scan      time.Duration   `json:"scan_ns"`
}

// A LanguageCount is a language and how many files it has.
type LanguageCount struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
}

// Status describes the index and how far it is from the disk, without building.
func (m *Manager) Status(ctx context.Context) (*Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status(ctx)
}

func (m *Manager) status(ctx context.Context) (*Status, error) {
	t0 := time.Now()
	m.keys = nil
	m.load()
	snap, err := m.tracker.Scan(ctx)
	if err != nil {
		return nil, err
	}
	st := &Status{Root: m.opts.Root, CacheDir: m.opts.CacheDir, Schema: SchemaVersion, Build: m.opts.Build,
		Files: len(snap.Files), Source: snap.Source, Scan: time.Since(t0), Discarded: m.discards}
	if m.opts.CacheDir != "" {
		st.CacheBytes = dirSize(m.opts.CacheDir)
		if _, err := os.Stat(m.opts.CacheDir); err == nil {
			st.CachePresent = true
		} else if !errors.Is(err, fs.ErrNotExist) {
			st.CachePresent = false
		}
	}
	if m.lastBuild.At != (time.Time{}) {
		lb := m.lastBuild
		st.LastBuild = &lb
	}

	servers := map[string]*ServerStatus{}
	langs := map[string]*LanguageStatus{}
	skips := map[string]*SkipGroup{}
	unc := map[string]int{}
	addSkip := func(reason, rel string) {
		g := skips[reason]
		if g == nil {
			g = &SkipGroup{Reason: reason}
			skips[reason] = g
		}
		g.Count++
		if len(g.Examples) < 3 {
			g.Examples = append(g.Examples, rel)
		}
	}
	for rel, why := range snap.Skipped {
		addSkip(why, rel)
	}
	for _, rel := range snap.Files {
		language := routerLanguage(rel)
		server := m.opts.Backend.Server(rel)
		extract := ExtractorFor(language) != nil
		if server == "" && !extract {
			unc[language]++
			continue
		}
		stt := snap.State[rel]
		st.Covered++
		ls := langs[language]
		if ls == nil {
			ls = &LanguageStatus{Language: language, Server: server, Imports: extract}
			langs[language] = ls
		}
		ls.Files++
		var ss *ServerStatus
		if server != "" {
			ss = servers[server]
			if ss == nil {
				ss = &ServerStatus{Name: server}
				if ps := m.parts[server]; ps != nil {
					ss.Version, ss.Cached = shortVersion(ps.version), true
				}
				servers[server] = ss
			}
			ss.Files++
		}
		if stt.Size > m.opts.MaxFileBytes {
			addSkip("too_large", rel)
			continue
		}
		unavailable := m.serverUnavailable(server)
		if unavailable {
			addSkip("server_unavailable", rel)
			if !extract {
				continue
			}
		}
		if sk, ok := m.skips[rel]; ok {
			addSkip(sk.Reason, rel)
			continue
		}
		e := m.files[rel]
		switch {
		case e == nil:
			st.Missing++
		case e.Server != server || !statFresh(e, stt):
			st.Stale++
			st.Indexed++
		default:
			st.Fresh++
			st.Indexed++
			ls.Indexed++
			st.Symbols += len(e.Symbols)
			st.Imports += len(e.Imports)
			if e.HasOutline {
				st.Outlined++
				if ss != nil {
					ss.Indexed++
					ss.Symbols += len(e.Symbols)
				}
			}
		}
	}
	for p := range m.files {
		if _, ok := snap.State[p]; !ok {
			st.Removed++
		}
	}
	for _, s := range servers {
		st.Servers = append(st.Servers, *s)
	}
	sort.Slice(st.Servers, func(i, j int) bool { return st.Servers[i].Name < st.Servers[j].Name })
	for _, l := range langs {
		st.Languages = append(st.Languages, *l)
	}
	sort.Slice(st.Languages, func(i, j int) bool { return st.Languages[i].Language < st.Languages[j].Language })
	for _, g := range skips {
		sort.Strings(g.Examples)
		st.Skipped = append(st.Skipped, *g)
	}
	sort.Slice(st.Skipped, func(i, j int) bool { return st.Skipped[i].Reason < st.Skipped[j].Reason })
	for l, n := range unc {
		st.Uncovered = append(st.Uncovered, LanguageCount{Language: l, Files: n})
	}
	sort.Slice(st.Uncovered, func(i, j int) bool {
		if st.Uncovered[i].Files != st.Uncovered[j].Files {
			return st.Uncovered[i].Files > st.Uncovered[j].Files
		}
		return st.Uncovered[i].Language < st.Uncovered[j].Language
	})
	st.Warm = st.Covered > 0 && st.Missing == 0 && st.Stale == 0 && st.Removed == 0
	return st, nil
}

// A FileCount is the symbols of one indexed file.
type FileCount struct {
	Symbols int            `json:"symbols"`
	Kinds   map[string]int `json:"kinds,omitempty"`
}

// Counts is the warm part of the index, for `repo_outline`: the symbols of the
// files whose entries are current by their stat. It never builds and never
// starts a server, and says how much of the workspace it does not cover.
type Counts struct {
	// Warm is whether any entry was current.
	Warm bool `json:"warm"`
	// Files maps a workspace-relative path to its symbol counts, for the
	// current entries with an outline only.
	Files map[string]FileCount `json:"files"`
	// NotCurrent is how many covered files have no current entry (never
	// indexed, or changed since): their symbols are not in Files. Paths lists
	// them, up to a limit.
	NotCurrent      int      `json:"not_current"`
	NotCurrentPaths []string `json:"not_current_paths,omitempty"`
}

// maxNotCurrentPaths bounds Counts.NotCurrentPaths.
const maxNotCurrentPaths = 20000

// Counts reads what is warm.
func (m *Manager) Counts(ctx context.Context) (*Counts, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.load()
	snap, err := m.tracker.Scan(ctx)
	if err != nil {
		return nil, err
	}
	c := &Counts{Files: map[string]FileCount{}}
	for _, rel := range snap.Files {
		language := routerLanguage(rel)
		server := m.opts.Backend.Server(rel)
		if server == "" {
			continue // no outline to count
		}
		_ = language
		e := m.files[rel]
		if e == nil || !e.HasOutline || e.Server != server || !statFresh(e, snap.State[rel]) {
			c.NotCurrent++
			if len(c.NotCurrentPaths) < maxNotCurrentPaths {
				c.NotCurrentPaths = append(c.NotCurrentPaths, rel)
			}
			continue
		}
		fc := FileCount{Symbols: len(e.Symbols)}
		if len(e.Symbols) > 0 {
			fc.Kinds = map[string]int{}
			for _, s := range e.Symbols {
				fc.Kinds[s.Kind]++
			}
		}
		c.Files[rel] = fc
	}
	c.Warm = len(c.Files) > 0
	return c, nil
}

// shortVersion is a server's version as a person reads it. gopls reports its
// whole build info as a JSON object; the module version is what is meant.
func shortVersion(v string) string {
	if strings.HasPrefix(v, "{") {
		var info struct{ Version string }
		if json.Unmarshal([]byte(v), &info) == nil && info.Version != "" {
			return info.Version
		}
	}
	if len(v) > 60 {
		return v[:60] + "…"
	}
	return v
}
