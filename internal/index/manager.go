package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// DefaultMaxFileBytes is the size above which a file is not indexed: a
// generated bundle or a data dump has no symbols an agent wants, and reading
// one costs every revalidation. The same 1 MiB `search_text` stops at (D26).
const DefaultMaxFileBytes = 1 << 20

// binarySniff is how much of a file is looked at for a NUL byte, the rule
// `file` and `search_text` use.
const binarySniff = 8 << 10

// outlineTimeout bounds one documentSymbol request.
const outlineTimeout = 60 * time.Second

// checkpointEvery is how many files a build records between writes of the
// cache, so that a build that is killed resumes instead of starting over.
const checkpointEvery = 500

// A Backend is what the index needs from the outside. The daemon implements it
// over its warm language servers; tests implement it over a table.
type Backend interface {
	// Server is the name of the server definition that handles rel, "" when
	// none does.
	Server(rel string) string
	// ServerKey is a cheap identity of the server's executable — path, size and
	// modification time — that changes when the server is upgraded, and is
	// available without starting it. "" when unknown.
	ServerKey(server string) string
	// Ready waits until the named server's answers can be believed (D6) and
	// returns the version it reported, "" when unknown. rel is one of the files
	// it will be asked about, to say which session. A server that is still
	// indexing is an error whose exit code is 5; the index records nothing that
	// depended on it.
	Ready(ctx context.Context, server, rel string) (version string, err error)
	// Outline is the server's documentSymbol answer for one file, decoded.
	// content is the file's bytes as the index read them: the server must be
	// told those bytes, not re-read the file. An error that is only about this
	// file is a *FileError; any other error stops the build.
	Outline(ctx context.Context, server, rel string, content []byte) ([]symbols.Symbol, error)
}

// A FileError is a failure that is about one file and not about the server: it
// is recorded as a reason the file is not indexed, and the build goes on.
type FileError struct{ Err error }

func (e *FileError) Error() string { return e.Err.Error() }
func (e *FileError) Unwrap() error { return e.Err }

// A ServerUnavailableError says a server cannot be used at all — it is not
// installed, or it will not start — as opposed to being busy (not ready, exit
// 5) or failing on a file. The files it handles are indexed for their imports
// alone, the build goes on with the other servers, and the server is asked
// again only when its executable changes (D34): a missing lua-language-server
// must not make `index build` unusable in a repository that has one Lua file.
type ServerUnavailableError struct {
	Server string
	Err    error
}

func (e *ServerUnavailableError) Error() string {
	return fmt.Sprintf("server %s is unavailable: %v", e.Server, e.Err)
}
func (e *ServerUnavailableError) Unwrap() error { return e.Err }

// Options configures a [Manager].
type Options struct {
	// Root is the workspace root; paths are relative to it.
	Root string
	// CacheDir is the directory the parts are persisted in. Empty means the
	// index is memory-only.
	CacheDir string
	// Build identifies the lightspeed build; a part written by another build is
	// discarded.
	Build   string
	Backend Backend
	// Tracker is the change detection to use, shared with whoever else in the
	// process needs it (the daemon's reconciler). Nil makes one.
	Tracker *Tracker
	// MaxFileBytes is the largest file indexed; zero means DefaultMaxFileBytes.
	MaxFileBytes int64
	// Parallelism bounds concurrent file reads and outline requests; zero means
	// min(8, 2×CPUs).
	Parallelism int
	// Logf logs index events. Nil discards them.
	Logf func(format string, args ...any)
}

// A Need says which parts of an entry a query needs to be current.
type Need uint8

const (
	// NeedImports is the import list, which needs no language server.
	NeedImports Need = 1 << iota
	// NeedOutline is the symbol outline, which needs the file's server ready.
	NeedOutline
)

// A Scope limits which files a query revalidates, and therefore builds: a
// search restricted to internal/cli does not pay for the rest of the tree.
// Every filter that narrows a query's answer must be in its scope, so that no
// entry outside the scope can appear in the answer without having been
// revalidated.
type Scope struct {
	// Path is a workspace-relative directory or file.
	Path      string
	Globs     []string
	Languages []string
	// Files, when non-empty, is an exact list of workspace-relative files.
	Files []string
}

// scopeMatcher is a compiled Scope.
type scopeMatcher struct {
	path  string
	globs *GlobSet
	langs map[string]bool
	files map[string]bool
}

func (m *Manager) compileScope(s Scope) (*scopeMatcher, error) {
	sm := &scopeMatcher{path: strings.Trim(path.Clean(strings.ReplaceAll(s.Path, "\\", "/")), "/")}
	if sm.path == "." {
		sm.path = ""
	}
	if len(s.Globs) > 0 {
		g, err := CompileGlobs(m.opts.Root, s.Globs)
		if err != nil {
			return nil, &UsageError{Msg: "--glob: " + err.Error()}
		}
		sm.globs = g
	}
	if len(s.Languages) > 0 {
		sm.langs = map[string]bool{}
		for _, l := range s.Languages {
			sm.langs[l] = true
		}
	}
	if len(s.Files) > 0 {
		sm.files = map[string]bool{}
		for _, f := range s.Files {
			sm.files[f] = true
		}
	}
	return sm, nil
}

func (sm *scopeMatcher) empty() bool {
	return sm == nil || (sm.path == "" && sm.globs == nil && sm.langs == nil && sm.files == nil)
}

func (sm *scopeMatcher) allows(rel, language string) bool {
	if sm == nil {
		return true
	}
	if sm.files != nil && !sm.files[rel] {
		return false
	}
	if sm.path != "" && rel != sm.path && !strings.HasPrefix(rel, sm.path+"/") {
		return false
	}
	if sm.langs != nil && !sm.langs[language] {
		return false
	}
	return sm.globs.Allows(rel)
}

// A Manager is one workspace's index: the entries, their persisted parts and
// the queries over them. It is safe for concurrent use; queries are serialised,
// because each begins by revalidating the files it is about to report on.
type Manager struct {
	opts    Options
	tracker *Tracker

	mu       sync.Mutex
	loaded   bool
	files    map[string]*File // entries are immutable once published
	parts    map[string]*partState
	discards []discard
	// skips are the files that are eligible but not indexable, by path: too
	// large, binary, or the server refused them.
	skips    map[string]skipInfo
	prevSnap *Snapshot
	// unavailable are the servers found unusable, and the executable identity
	// they were found so under.
	unavailable map[string]unavail
	// gen changes whenever an entry or the file set does; the search index and
	// the graph are cached against it.
	gen uint64
	// keys memoises Backend.ServerKey for the duration of one query: it is a
	// PATH lookup and a stat, and asked per file it would be most of the cost.
	keys      map[string]string
	search    *SearchIndex
	searchGen uint64
	lastBuild BuildInfo

	applyMu sync.Mutex // guards files/parts/skips while workers apply results
	dirty   map[string]bool
}

type unavail struct {
	Reason string
	Key    string
}

type partState struct {
	key     partKey
	version string
}

type skipInfo struct {
	Reason string
	// Detail is the server's words for an outline that failed.
	Detail  string
	Size    int64
	MTimeNs int64
}

// A BuildInfo describes the last time the index built anything.
type BuildInfo struct {
	At       time.Time     `json:"at,omitempty"`
	Duration time.Duration `json:"duration_ns,omitempty"`
	Files    int           `json:"files,omitempty"`
	Outlines int           `json:"outlines,omitempty"`
}

// New returns a manager. Nothing is read until the first query.
func New(opts Options) *Manager {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.Parallelism <= 0 {
		opts.Parallelism = min(8, 2*runtime.NumCPU())
	}
	tr := opts.Tracker
	if tr == nil {
		tr = NewTracker(opts.Root)
	}
	return &Manager{opts: opts, tracker: tr, files: map[string]*File{}, parts: map[string]*partState{},
		skips: map[string]skipInfo{}, dirty: map[string]bool{}, unavailable: map[string]unavail{}}
}

func (m *Manager) logf(format string, args ...any) {
	if m.opts.Logf != nil {
		m.opts.Logf(format, args...)
	}
}

// load reads the persisted parts once, discarding any that do not match this
// build and this server, or are damaged.
func (m *Manager) load() {
	if m.loaded {
		return
	}
	m.loaded = true
	if m.opts.CacheDir == "" {
		return
	}
	sweepTemp(m.opts.CacheDir, time.Hour)
	parts, discards, err := readParts(m.opts.CacheDir, func(k partKey) (bool, string) {
		switch {
		case k.Schema != SchemaVersion:
			return false, fmt.Sprintf("schema version %d, this build reads %d", k.Schema, SchemaVersion)
		case k.Build != m.opts.Build:
			return false, "written by another build of lightspeed"
		case k.Server != noServerPart && k.ServerKey != m.opts.Backend.ServerKey(k.Server):
			return false, fmt.Sprintf("server %s is not the one it was built with (executable changed or gone)", k.Server)
		}
		return true, ""
	})
	if err != nil {
		m.discards = append(m.discards, discard{Name: m.opts.CacheDir, Reason: err.Error()})
		return
	}
	m.discards = append(m.discards, discards...)
	for server, p := range parts {
		m.parts[server] = &partState{key: p.partKey, version: p.serverVersion}
		for _, f := range p.files {
			if want := serverOf(f); want != server {
				continue // an entry filed under the wrong part is not trusted
			}
			m.files[f.Path] = f
		}
	}
	for _, d := range discards {
		m.logf("index: discarded %s: %s", d.Name, d.Reason)
	}
}

// serverOf is the part an entry belongs to.
func serverOf(f *File) string {
	if f.Server == "" {
		return noServerPart
	}
	return f.Server
}

// A SyncReport says what revalidating the workspace found and did.
type SyncReport struct {
	// Files is how many files in scope the index covers; Fresh how many were
	// current by their stat alone, Touched how many had a new mtime and the
	// same content, Changed how many had new content, Added how many were not
	// indexed, Removed how many entries were dropped because the file is gone
	// (or is now ignored).
	Files   int `json:"files"`
	Fresh   int `json:"fresh"`
	Touched int `json:"touched,omitempty"`
	Changed int `json:"changed,omitempty"`
	Added   int `json:"added,omitempty"`
	Removed int `json:"removed,omitempty"`
	// Built is how many entries were (re)made, Outlines how many of those asked
	// a server.
	Built    int `json:"built,omitempty"`
	Outlines int `json:"outlines,omitempty"`
	// Servers are the servers that were asked, and Skipped counts the files not
	// indexed by reason.
	Servers []string       `json:"servers,omitempty"`
	Skipped map[string]int `json:"skipped,omitempty"`
	// Uncovered counts the in-scope files no server and no import extractor
	// covers, by language ("" for a file of no known language).
	Uncovered map[string]int `json:"uncovered,omitempty"`
	// Listed is whether the file list was re-made, ScanMs and TotalMs are how
	// long the scan and the whole revalidation took.
	Listed  bool          `json:"listed"`
	Scan    time.Duration `json:"scan_ns"`
	Elapsed time.Duration `json:"elapsed_ns"`
	// Discarded are cached parts thrown away while loading, with the reason.
	Discarded []discard `json:"discarded,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Notice is the one-line warning a query that had to build attaches, or "".
func (r *SyncReport) Notice() string {
	if r == nil || r.Built == 0 {
		return ""
	}
	what := fmt.Sprintf("%d files", r.Built)
	if r.Outlines > 0 {
		what += fmt.Sprintf(" (%d outlines from %s)", r.Outlines, strings.Join(r.Servers, ", "))
	}
	return fmt.Sprintf("the index was built lazily for this query: %s in %s; `index build` warms the whole workspace ahead of time",
		what, r.Elapsed.Round(time.Millisecond))
}

// Stale reports whether the revalidation this report describes rebuilt
// anything (any file was touched, changed, added or removed) rather than
// finding the cache entirely current.
func (r *SyncReport) Stale() bool {
	if r == nil {
		return false
	}
	return r.Touched+r.Changed+r.Added+r.Removed+r.Built > 0
}

// Summary is the one-line form of the report a query-command answer carries
// by default in place of the full report ("269 files, fresh, 6 skipped");
// the full report is one flag away (--report, MCP report; docs/DECISIONS.md
// D46). It never reports on a nil report.
func (r *SyncReport) Summary() string {
	if r == nil {
		return ""
	}
	parts := []string{fmt.Sprintf("%d files", r.Files)}
	if !r.Stale() {
		parts = append(parts, "fresh")
	} else {
		var bits []string
		for _, kv := range []struct {
			n     int
			label string
		}{{r.Added, "added"}, {r.Changed, "changed"}, {r.Touched, "touched"}, {r.Removed, "removed"}} {
			if kv.n > 0 {
				bits = append(bits, fmt.Sprintf("%d %s", kv.n, kv.label))
			}
		}
		parts = append(parts, strings.Join(bits, ", "))
	}
	if skipped := sumCounts(r.Skipped); skipped > 0 {
		parts = append(parts, fmt.Sprintf("%d skipped", skipped))
	}
	if uncovered := sumCounts(r.Uncovered); uncovered > 0 {
		parts = append(parts, fmt.Sprintf("%d uncovered", uncovered))
	}
	return strings.Join(parts, ", ")
}

// sumCounts totals a by-reason or by-language count map.
func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

// pending is one file that has to be read: because its stat moved, because it
// was never indexed, or because a part of its entry the query needs is
// missing.
type pending struct {
	rel      string
	language string
	server   string
	state    FileState
	old      *File
	// extract is whether the language has an import extractor.
	extract bool
	// noOutline is set when the file's server is known to be unavailable: the
	// entry is made without an outline.
	noOutline bool
}

// A built file is the outcome of reading one pending file.
type built struct {
	p       pending
	content []byte
	hash    string
	entry   *File // the new entry, or nil for "unchanged, only re-stat"
	kind    string
	// needOutline says the entry still lacks an outline the query needs.
	needOutline bool
	skip        *skipInfo
	// applied is set once the entry has been published, so that the checkpoints
	// a long build makes are not published twice.
	applied bool
}

// sync revalidates the files in scope against the disk and rebuilds what is
// stale, under the caller's lock. It returns the snapshot it validated against.
func (m *Manager) sync(ctx context.Context, need Need, sm *scopeMatcher) (*Snapshot, *SyncReport, error) {
	t0 := time.Now()
	m.keys = nil
	m.load()
	rep := &SyncReport{Discarded: m.discards}
	m.discards = nil

	snap, err := m.tracker.Scan(ctx)
	if err != nil {
		return nil, nil, err
	}
	rep.Scan, rep.Listed = time.Since(t0), snap.Listed
	if len(Diff(m.prevSnap, snap)) > 0 || m.prevSnap == nil {
		m.gen++
	}
	m.prevSnap = snap

	// Entries for files that are no longer there (deleted, renamed, newly
	// ignored, replaced by a symlink) go, whatever the scope: nothing may report
	// a file that does not exist.
	for p, e := range m.files {
		if _, ok := snap.State[p]; !ok {
			delete(m.files, p)
			m.dirty[serverOf(e)] = true
			rep.Removed++
		}
	}
	for p := range m.skips {
		if _, ok := snap.State[p]; !ok {
			delete(m.skips, p)
		}
	}

	var todo []pending
	for _, rel := range snap.Files {
		language := router.LanguageID(rel)
		if !sm.allows(rel, language) {
			continue
		}
		server := m.opts.Backend.Server(rel)
		extractor := ExtractorFor(language)
		if server == "" && extractor == nil {
			if rep.Uncovered == nil {
				rep.Uncovered = map[string]int{}
			}
			rep.Uncovered[language]++
			continue
		}
		st := snap.State[rel]
		old := m.files[rel]
		if st.Size > m.opts.MaxFileBytes {
			m.skip(rep, rel, skipInfo{Reason: "too_large", Size: st.Size, MTimeNs: st.MTimeNs})
			if old != nil {
				delete(m.files, rel)
				m.dirty[serverOf(old)] = true
			}
			continue
		}
		if sk, ok := m.skips[rel]; ok && sk.Size == st.Size && sk.MTimeNs == st.MTimeNs && !st.Racy && sk.Reason != "outline_failed" {
			m.skip(rep, rel, sk)
			continue
		}
		unavailable := m.serverUnavailable(server)
		if unavailable && extractor == nil {
			m.skip(rep, rel, skipInfo{Reason: "server_unavailable", Size: st.Size, MTimeNs: st.MTimeNs})
			continue
		}
		rep.Files++
		p := pending{rel: rel, language: language, server: server, state: st, old: old, extract: extractor != nil, noOutline: unavailable}
		if unavailable {
			rep.Skipped = bump(rep.Skipped, "server_unavailable")
		}
		switch {
		case old == nil:
			rep.Added++
			todo = append(todo, p)
		case old.Server != server:
			// The definitions now route this file elsewhere: its outline came
			// from a server that no longer answers for it.
			rep.Changed++
			todo = append(todo, p)
		case !statFresh(old, st):
			todo = append(todo, p) // hash decides: touched or changed
		case lacks(old, need, server != "" && !unavailable, p.extract):
			todo = append(todo, p) // fresh, but a part the query needs is missing
		default:
			rep.Fresh++
		}
	}
	if len(todo) == 0 {
		return m.finish(ctx, snap, rep, t0, nil)
	}

	// Phase 1: read and hash the files that need it, and scan their imports.
	// No server is involved, so nothing here can be affected by one being
	// unready.
	results := m.inspect(ctx, todo, need)
	if err := ctx.Err(); err != nil {
		return nil, rep, err
	}

	// Phase 2: the outlines. A server that is still indexing must not be asked
	// to describe files it has not loaded (D6), and what it said would be
	// recorded as fact: so every server with work is waited for *before* any
	// outline is recorded, and one that is not ready fails the query with its
	// own error and records nothing that depended on it.
	var need2 []*built
	servers := map[string]string{} // server -> a sample file
	for _, b := range results {
		if b.needOutline {
			need2 = append(need2, b)
			if _, ok := servers[b.p.server]; !ok {
				servers[b.p.server] = b.p.rel
			}
		}
	}
	gone := map[string]bool{}
	names := make([]string, 0, len(servers))
	for s := range servers {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		version, err := m.opts.Backend.Ready(ctx, s, servers[s])
		var unavailable *ServerUnavailableError
		if errors.As(err, &unavailable) {
			m.unavailable[s] = unavail{Reason: unavailable.Error(), Key: m.keyOf(s)}
			gone[s] = true
			rep.Warnings = append(rep.Warnings, unavailable.Error()+"; its files are indexed for their imports only")
			continue
		}
		if err != nil {
			// Commit only what needed no server.
			m.applyResults(results, rep, false)
			m.saveDirty(rep)
			return nil, rep, err
		}
		if ps := m.parts[s]; ps != nil && ps.version != "" && version != "" && ps.version != version {
			// The part was built by another version of this server: none of its
			// outlines can be trusted, and the files that were fresh have to be
			// asked again. Start over with the part emptied.
			m.logf("index: %s is now %s, was %s: rebuilding its outlines", s, version, ps.version)
			for p, e := range m.files {
				if serverOf(e) == s {
					delete(m.files, p)
				}
			}
			delete(m.parts, s)
			m.dirty[s] = true
			note := fmt.Sprintf("server %s changed from version %s to %s; its outlines were rebuilt", s, ps.version, version)
			snap2, rep2, err2 := m.sync(ctx, need, sm)
			if rep2 != nil {
				rep2.Warnings = append([]string{note}, rep2.Warnings...)
			}
			return snap2, rep2, err2
		}
		ps := m.parts[s]
		if ps == nil {
			ps = &partState{}
			m.parts[s] = ps
		}
		ps.version = version
		delete(m.unavailable, s)
		rep.Servers = append(rep.Servers, s)
	}
	if len(gone) > 0 {
		kept := need2[:0]
		for _, b := range need2 {
			if gone[b.p.server] {
				b.needOutline, b.p.noOutline = false, true
				rep.Skipped = bump(rep.Skipped, "server_unavailable")
				continue
			}
			kept = append(kept, b)
		}
		need2 = kept
	}
	err = m.outlines(ctx, need2, rep)
	m.applyResults(results, rep, true)
	m.saveDirty(rep)
	if err != nil {
		return nil, rep, err
	}
	return m.finish(ctx, snap, rep, t0, nil)
}

func (m *Manager) finish(ctx context.Context, snap *Snapshot, rep *SyncReport, t0 time.Time, err error) (*Snapshot, *SyncReport, error) {
	rep.Elapsed = time.Since(t0)
	m.saveDirty(rep)
	if rep.Built > 0 {
		m.lastBuild = BuildInfo{At: time.Now(), Duration: rep.Elapsed, Files: rep.Built, Outlines: rep.Outlines}
	}
	if rep.Removed > 0 || rep.Built > 0 || rep.Touched > 0 {
		m.gen++
	}
	return snap, rep, err
}

func (m *Manager) skip(rep *SyncReport, rel string, sk skipInfo) {
	m.skips[rel] = sk
	if rep.Skipped == nil {
		rep.Skipped = map[string]int{}
	}
	rep.Skipped[sk.Reason]++
}

// statFresh reports whether an entry is current by its stat alone: same size
// and mtime, the file's mtime not within racyWindow of the moment the entry was
// recorded (a same-size edit within the clock's resolution would be invisible),
// and the scan's own racy hash, when it took one, equal to the entry's.
func statFresh(e *File, st FileState) bool {
	if e.Size != st.Size || e.MTimeNs != st.MTimeNs {
		return false
	}
	if st.Racy {
		return st.Hash != "" && st.Hash == e.Hash
	}
	return e.MTimeNs < e.RecordedNs-int64(racyWindow)
}

// lacks reports whether a fresh entry is missing a part of itself the query
// needs and that can be made.
func lacks(e *File, need Need, hasServer, extract bool) bool {
	if need&NeedOutline != 0 && hasServer && !e.HasOutline {
		return true
	}
	if extract && !e.HasImports {
		return true
	}
	return false
}

// inspect reads the pending files in parallel: hash, compare with the old
// entry, scan imports, and decide what each needs.
func (m *Manager) inspect(ctx context.Context, todo []pending, need Need) []*built {
	out := make([]*built, len(todo))
	var wg sync.WaitGroup
	sem := make(chan struct{}, m.opts.Parallelism)
	for i := range todo {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			out[i] = m.inspectOne(todo[i], need)
		}()
	}
	wg.Wait()
	res := out[:0]
	for _, b := range out {
		if b != nil {
			res = append(res, b)
		}
	}
	return res
}

func (m *Manager) inspectOne(p pending, need Need) *built {
	b := &built{p: p}
	abs := m.abs(p.rel)
	hash, content, err := hashFile(abs, m.opts.MaxFileBytes)
	if err != nil {
		b.skip = &skipInfo{Reason: "unreadable", Detail: err.Error(), Size: p.state.Size, MTimeNs: p.state.MTimeNs}
		return b
	}
	if int64(len(content)) > m.opts.MaxFileBytes {
		b.skip = &skipInfo{Reason: "too_large", Size: int64(len(content)), MTimeNs: p.state.MTimeNs}
		return b
	}
	if isBinary(content) {
		b.skip = &skipInfo{Reason: "binary", Size: p.state.Size, MTimeNs: p.state.MTimeNs}
		return b
	}
	b.content, b.hash = content, hash
	now := time.Now().UnixNano()
	base := File{Path: p.rel, Language: p.language, Server: p.server, Size: int64(len(content)),
		MTimeNs: p.state.MTimeNs, Hash: hash, RecordedNs: now}

	if p.old != nil && p.old.Hash == hash && p.old.Server == p.server {
		// The same content: everything learned from it stands. Re-stat, and
		// build only what the query needs that is missing.
		e := *p.old
		e.Size, e.MTimeNs, e.RecordedNs = base.Size, base.MTimeNs, now
		b.entry, b.kind = &e, "touched"
		if p.extract && !e.HasImports {
			e.Imports, e.HasImports = scanImports(p, content), true
			b.entry, b.kind = &e, "built"
		}
		b.needOutline = need&NeedOutline != 0 && p.server != "" && !p.noOutline && !e.HasOutline
		if b.kind == "touched" && b.needOutline {
			b.kind = "built"
		}
		return b
	}
	e := base
	if p.extract {
		e.Imports, e.HasImports = scanImports(p, content), true
	}
	b.entry, b.kind = &e, "built"
	b.needOutline = need&NeedOutline != 0 && p.server != "" && !p.noOutline
	return b
}

func scanImports(p pending, content []byte) []ImportRef {
	refs, _ := ExtractImports(p.language, p.rel, content)
	return refs
}

func isBinary(content []byte) bool {
	n := min(len(content), binarySniff)
	for _, c := range content[:n] {
		if c == 0 {
			return true
		}
	}
	return false
}

// outlines asks each file's server for its outline, in parallel, and fills the
// entries in. It stops at the first error that is not about one file; entries
// completed before it stay complete.
func (m *Manager) outlines(ctx context.Context, work []*built, rep *SyncReport) error {
	if len(work) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
		done     int
	)
	sem := make(chan struct{}, m.opts.Parallelism)
	for _, b := range work {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			rctx, rcancel := context.WithTimeout(ctx, outlineTimeout)
			syms, err := m.opts.Backend.Outline(rctx, b.p.server, b.p.rel, b.content)
			rcancel()
			if err != nil {
				var fe *FileError
				if errors.As(err, &fe) {
					b.skip = &skipInfo{Reason: "outline_failed", Detail: fe.Error(), Size: b.p.state.Size, MTimeNs: b.p.state.MTimeNs}
					return
				}
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				errMu.Unlock()
				return
			}
			e := *b.entry
			e.Symbols, e.HasOutline = buildSymbols(b.p.rel, b.p.language, b.content, syms), true
			b.entry, b.needOutline = &e, false
			// Publish as each outline completes, and write the cache every
			// checkpointEvery of them: a build that is killed resumes from its
			// last checkpoint instead of starting over. An entry is complete
			// when it is published, so a killed build leaves only true entries.
			m.applyMu.Lock()
			m.applyOne(b, rep, true)
			m.applyMu.Unlock()
			errMu.Lock()
			done++
			checkpoint := done%checkpointEvery == 0
			errMu.Unlock()
			if checkpoint {
				m.saveDirty(rep)
			}
		}()
	}
	wg.Wait()
	rep.Outlines += done
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

// applyResults publishes the entries built, under applyMu. With withOutlines
// false only entries that need no outline are published: the not-ready path.
func (m *Manager) applyResults(results []*built, rep *SyncReport, withOutlines bool) {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	for _, b := range results {
		m.applyOne(b, rep, withOutlines)
	}
}

// applyOne publishes one built file; applyMu is held.
func (m *Manager) applyOne(b *built, rep *SyncReport, withOutlines bool) {
	if b.applied {
		return
	}
	if b.skip != nil {
		if b.skip.Reason == "outline_failed" && !withOutlines {
			return
		}
		b.applied = true
		m.skip(rep, b.p.rel, *b.skip)
		if b.p.old != nil {
			delete(m.files, b.p.rel)
			m.dirty[serverOf(b.p.old)] = true
		}
		if b.skip.Reason == "outline_failed" && b.skip.Detail != "" {
			rep.Warnings = append(rep.Warnings, fmt.Sprintf("%s: not indexed: %s", b.p.rel, b.skip.Detail))
		}
		return
	}
	if b.needOutline {
		return // never publish an entry that lacks the outline the query needs
	}
	b.applied = true
	delete(m.skips, b.p.rel)
	e := b.entry
	switch b.kind {
	case "touched":
		rep.Touched++
	case "built":
		rep.Built++
		if b.p.old != nil && b.p.old.Hash != e.Hash {
			rep.Changed++
		}
	}
	if b.p.old != nil && b.p.old.Server != e.Server {
		m.dirty[serverOf(b.p.old)] = true
	}
	m.files[e.Path] = e
	m.dirty[serverOf(e)] = true
}

// saveDirty writes every part that changed. A failure to write is a warning:
// the index in memory is right and only the next process pays for it.
func (m *Manager) saveDirty(rep *SyncReport) {
	if m.opts.CacheDir == "" || len(m.dirty) == 0 {
		return
	}
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	for server := range m.dirty {
		var files []*File
		for _, f := range m.files {
			if serverOf(f) == server {
				files = append(files, f)
			}
		}
		if len(files) == 0 {
			// Nothing to keep: remove a stale part instead of writing an empty one.
			_ = os.Remove(pathJoin(m.opts.CacheDir, partName(server)))
			delete(m.parts, server)
			delete(m.dirty, server)
			continue
		}
		ps := m.parts[server]
		if ps == nil {
			ps = &partState{}
			m.parts[server] = ps
		}
		ps.key = partKey{Schema: SchemaVersion, Build: m.opts.Build, Server: server}
		if server != noServerPart {
			ps.key.ServerKey = m.opts.Backend.ServerKey(server)
		}
		m.mergeOnDisk(server, ps.key, &files)
		if _, err := writePart(m.opts.CacheDir, m.opts.Root, ps.key, ps.version, files, time.Now()); err != nil {
			rep.Warnings = append(rep.Warnings, "the index cache could not be written ("+err.Error()+"); it is kept in memory only")
			m.logf("index: writing part %s: %v", server, err)
			continue
		}
		delete(m.dirty, server)
	}
}

// mergeOnDisk adds to files the entries another process wrote to the same part
// since this one loaded it, for paths this one does not hold — the case of two
// builders covering different scopes. An entry it does hold is never replaced:
// this process revalidated its own against the disk, and the other's may be
// older.
func (m *Manager) mergeOnDisk(server string, key partKey, files *[]*File) {
	p, _ := readPart(pathJoin(m.opts.CacheDir, partName(server)), func(k partKey) (bool, string) {
		return k == key, "another key"
	})
	if p == nil || m.prevSnap == nil {
		return
	}
	held := map[string]bool{}
	for _, f := range *files {
		held[f.Path] = true
	}
	for _, f := range p.files {
		st, ok := m.prevSnap.State[f.Path]
		if held[f.Path] || !ok || m.files[f.Path] != nil || !statFresh(f, st) {
			continue
		}
		m.files[f.Path] = f
		*files = append(*files, f)
	}
}

func (m *Manager) abs(rel string) string {
	return pathJoin(m.opts.Root, rel)
}

// fileList are the entries in scope that have what need asks for, sorted.
func (m *Manager) fileList(sm *scopeMatcher, need Need) []*File {
	out := make([]*File, 0, len(m.files))
	for _, f := range m.files {
		if !sm.allows(f.Path, f.Language) {
			continue
		}
		if need&NeedOutline != 0 && !f.HasOutline {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// serverUnavailable reports whether a server is known to be unusable under the
// executable it has now: once its executable changes (it was installed, or
// upgraded) it is tried again.
func (m *Manager) serverUnavailable(server string) bool {
	if server == "" {
		return false
	}
	u, ok := m.unavailable[server]
	if !ok {
		return false
	}
	if u.Key != m.keyOf(server) {
		delete(m.unavailable, server)
		return false
	}
	return true
}

func bump(m map[string]int, key string) map[string]int {
	if m == nil {
		m = map[string]int{}
	}
	m[key]++
	return m
}

func (m *Manager) keyOf(server string) string {
	if k, ok := m.keys[server]; ok {
		return k
	}
	if m.keys == nil {
		m.keys = map[string]string{}
	}
	k := m.opts.Backend.ServerKey(server)
	m.keys[server] = k
	return k
}
