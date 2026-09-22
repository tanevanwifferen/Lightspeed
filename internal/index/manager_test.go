package index

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// fakeBackend is a language server as the index sees it: a table from file
// extension to server, an outline that reads `func Name` lines, and a record of
// everything it was asked.
type fakeBackend struct {
	mu       sync.Mutex
	servers  map[string]string // extension -> server name
	key      string
	version  string
	notReady error
	outlined []string // rel of every Outline call
	readies  int
}

func newFake() *fakeBackend {
	return &fakeBackend{servers: map[string]string{".go": "gopls"}, key: "k1", version: "v1"}
}

func (f *fakeBackend) Server(rel string) string { return f.servers[filepath.Ext(rel)] }
func (f *fakeBackend) ServerKey(server string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key
}
func (f *fakeBackend) Ready(_ context.Context, _, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readies++
	return f.version, f.notReady
}

var funcLine = regexp.MustCompile(`^func (\w+)`)

func (f *fakeBackend) Outline(_ context.Context, _, rel string, content []byte) ([]symbols.Symbol, error) {
	f.mu.Lock()
	f.outlined = append(f.outlined, rel)
	f.mu.Unlock()
	var out []symbols.Symbol
	for i, line := range strings.Split(string(content), "\n") {
		if m := funcLine.FindStringSubmatch(line); m != nil {
			out = append(out, symbols.Symbol{
				Name: m[1], Qualified: m[1], Kind: "function",
				Range:    protocol.Range{Start: protocol.Position{Line: uint32(i), Character: 5}, End: protocol.Position{Line: uint32(i), Character: uint32(5 + len(m[1]))}},
				Full:     protocol.Range{Start: protocol.Position{Line: uint32(i)}, End: protocol.Position{Line: uint32(i), Character: uint32(len(line))}},
				HasRange: true,
			})
		}
	}
	return out, nil
}

func (f *fakeBackend) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := append([]string(nil), f.outlined...)
	sort.Strings(c)
	return c
}

func (f *fakeBackend) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outlined = nil
}

// put writes a file whose mtime is old enough not to be racy.
func put(t *testing.T, root, rel, content string) {
	t.Helper()
	putAged(t, root, rel, content, time.Hour)
}

func putAged(t *testing.T, root, rel, content string, age time.Duration) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
}

func newRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func newManager(t *testing.T, root, cache, build string, b Backend) *Manager {
	t.Helper()
	return New(Options{Root: root, CacheDir: cache, Build: build, Backend: b, Parallelism: 4})
}

func sampleTree(t *testing.T, root string) {
	put(t, root, "a.go", "package a\n\nfunc Alpha() {}\n\nfunc Beta() {}\n")
	put(t, root, "b.go", "package a\n\nfunc Gamma() {}\n")
	put(t, root, "c.py", "import os\n\ndef c():\n    pass\n")
	put(t, root, "README.md", "# hi\n")
}

func hitNames(hits []Hit) []string {
	var out []string
	for _, h := range hits {
		out = append(out, h.Symbol.Name)
	}
	return out
}

func TestColdBuildThenWarmBuildBuildsNothing(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)

	res, err := m.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Report; got.Built != 3 || got.Outlines != 2 || got.Fresh != 0 {
		t.Fatalf("cold report = %+v", got)
	}
	if got := fb.calls(); len(got) != 2 {
		t.Fatalf("outline calls = %v", got)
	}
	if res.Report.Uncovered["markdown"] != 1 {
		t.Fatalf("README.md must be reported uncovered, got %v", res.Report.Uncovered)
	}
	if !res.Status.Warm || res.Status.Symbols != 3 || res.Status.Outlined != 2 {
		t.Fatalf("status after build = %+v", res.Status)
	}

	fb.reset()
	res, err = m.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Report; got.Built != 0 || got.Fresh != 3 || res.Report.Notice() != "" {
		t.Fatalf("warm report = %+v", got)
	}
	if len(fb.calls()) != 0 {
		t.Fatalf("a warm build asked the server: %v", fb.calls())
	}

	// A fresh process loads it from disk and builds nothing either.
	fb2 := newFake()
	m2 := newManager(t, root, cache, "b1", fb2)
	res, err = m2.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Built != 0 || res.Report.Fresh != 3 || len(fb2.calls()) != 0 || len(res.Report.Discarded) != 0 {
		t.Fatalf("reload report = %+v, calls %v", res.Report, fb2.calls())
	}
}

func TestSearchSeesEditAddDeleteRename(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	ctx := context.Background()
	if _, err := m.Build(ctx); err != nil {
		t.Fatal(err)
	}
	search := func(q string) []string {
		t.Helper()
		out, err := m.Search(ctx, SearchQuery{Text: q})
		if err != nil {
			t.Fatal(err)
		}
		return hitNames(out.Hits)
	}
	if got := search("Gamma"); len(got) != 1 {
		t.Fatalf("Gamma = %v", got)
	}

	fb.reset()
	// edit (different length), add, delete, rename.
	put(t, root, "b.go", "package a\n\nfunc Delta() {}\n\nfunc Epsilon() {}\n")
	put(t, root, "d.go", "package a\n\nfunc Zeta() {}\n")
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "c.py"), filepath.Join(root, "e.py")); err != nil {
		t.Fatal(err)
	}

	if got := search("Gamma"); len(got) != 0 {
		t.Fatalf("the old symbol of an edited file is served: %v", got)
	}
	if got := search("Alpha"); len(got) != 0 {
		t.Fatalf("a deleted file's symbol is served: %v", got)
	}
	if got := search("Delta"); len(got) != 1 {
		t.Fatalf("Delta = %v", got)
	}
	if got := search("Zeta"); len(got) != 1 {
		t.Fatalf("Zeta = %v", got)
	}
	// Only the files that changed were asked about again.
	if got := fb.calls(); strings.Join(got, ",") != "b.go,d.go" {
		t.Fatalf("outline calls after the edits = %v", got)
	}

	// The rename shows in the import side too: e.py is indexed, c.py is not.
	g, err := m.Imports(ctx, "e.py")
	if err != nil || !g.Covered {
		t.Fatalf("imports of the renamed file: %+v, %v", g, err)
	}
	if _, err := m.Imports(ctx, "c.py"); err == nil {
		t.Fatal("imports of a file that no longer exists must fail")
	}
}

func TestSameSizeEditInTheSameTickIsSeen(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	put(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	ctx := context.Background()
	// A fresh file: its mtime is within the racy window of the moment it is
	// indexed, so its stat proves nothing.
	putAged(t, root, "a.go", "package a\n\nfunc Alpha() {}\n", 0)
	if _, err := m.Build(ctx); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	// Same length, same mtime, different content.
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc Omega() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(root, "a.go"), fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	out, err := m.Search(ctx, SearchQuery{Text: "Omega"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 1 {
		t.Fatalf("a same-size, same-mtime edit was not seen: %v", hitNames(out.Hits))
	}
	if old, _ := m.Search(ctx, SearchQuery{Text: "Alpha"}); len(old.Hits) != 0 {
		t.Fatalf("the old symbol is still served: %v", hitNames(old.Hits))
	}
}

func TestTouchedFileIsNotRebuilt(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	ctx := context.Background()
	if _, err := m.Build(ctx); err != nil {
		t.Fatal(err)
	}
	fb.reset()
	later := time.Now().Add(-30 * time.Minute)
	if err := os.Chtimes(filepath.Join(root, "a.go"), later, later); err != nil {
		t.Fatal(err)
	}
	res, err := m.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Report.Touched != 1 || res.Report.Built != 0 || len(fb.calls()) != 0 {
		t.Fatalf("touch: %+v calls %v", res.Report, fb.calls())
	}
}

func TestCorruptCacheIsDiscardedNotTrusted(t *testing.T) {
	for name, damage := range map[string]func(path string, raw []byte) []byte{
		"truncated":      func(_ string, raw []byte) []byte { return raw[:len(raw)/2] },
		"garbage":        func(_ string, _ []byte) []byte { return []byte("not json at all") },
		"edited entries": func(_ string, raw []byte) []byte { return []byte(strings.Replace(string(raw), "Alpha", "Alphx", 1)) },
		"empty":          func(_ string, _ []byte) []byte { return nil },
		"foreign json":   func(_ string, _ []byte) []byte { return []byte(`{"hello":"world"}`) },
	} {
		t.Run(name, func(t *testing.T) {
			root, cache := newRoot(t), t.TempDir()
			sampleTree(t, root)
			m := newManager(t, root, cache, "b1", newFake())
			if _, err := m.Build(context.Background()); err != nil {
				t.Fatal(err)
			}
			part := filepath.Join(cache, partName("gopls"))
			raw, err := os.ReadFile(part)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(part, damage(part, raw), 0o644); err != nil {
				t.Fatal(err)
			}

			fb := newFake()
			m2 := newManager(t, root, cache, "b1", fb)
			res, err := m2.Build(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Report.Discarded) != 1 || !strings.HasPrefix(res.Report.Discarded[0].Name, "part-gopls") {
				t.Fatalf("discarded = %+v", res.Report.Discarded)
			}
			if got := fb.calls(); len(got) != 2 {
				t.Fatalf("the Go files were not rebuilt from the server: %v", got)
			}
			out, _ := m2.Search(context.Background(), SearchQuery{Text: "Alpha"})
			if len(out.Hits) != 1 || out.Hits[0].Symbol.Name != "Alpha" {
				t.Fatalf("Alpha after the rebuild: %v", hitNames(out.Hits))
			}
		})
	}
}

func TestSchemaBuildAndServerKeyMismatchesRebuildThatPart(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	if _, err := newManager(t, root, cache, "b1", newFake()).Build(context.Background()); err != nil {
		t.Fatal(err)
	}

	t.Run("build id", func(t *testing.T) {
		fb := newFake()
		res, err := newManager(t, root, cache, "b2", fb).Build(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if res.Report.Built != 3 || len(fb.calls()) != 2 || len(res.Report.Discarded) != 2 {
			t.Fatalf("a new build id must rebuild everything: %+v calls %v", res.Report, fb.calls())
		}
	})

	// The previous subtest rewrote the cache as build b2.
	t.Run("server key rebuilds only that server's part", func(t *testing.T) {
		fb := newFake()
		fb.key = "k2"
		res, err := newManager(t, root, cache, "b2", fb).Build(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got := fb.calls(); len(got) != 2 {
			t.Fatalf("gopls part not rebuilt after its executable changed: %v", got)
		}
		if res.Report.Built != 2 || res.Report.Fresh != 1 {
			t.Fatalf("the server-less part (imports) must be kept: %+v", res.Report)
		}
	})

	t.Run("schema version", func(t *testing.T) {
		raw, err := os.ReadFile(filepath.Join(cache, partName("gopls")))
		if err != nil {
			t.Fatal(err)
		}
		bumped := strings.Replace(string(raw), fmt.Sprintf(`"schema":%d`, SchemaVersion), fmt.Sprintf(`"schema":%d`, SchemaVersion+1), 1)
		if bumped == string(raw) {
			t.Fatal("schema field not found")
		}
		if err := os.WriteFile(filepath.Join(cache, partName("gopls")), []byte(bumped), 0o644); err != nil {
			t.Fatal(err)
		}
		fb := newFake()
		fb.key = "k2"
		res, err := newManager(t, root, cache, "b2", fb).Build(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(fb.calls()) != 2 || len(res.Report.Discarded) != 1 {
			t.Fatalf("schema bump: calls %v discarded %+v", fb.calls(), res.Report.Discarded)
		}
	})
}

func TestServerVersionChangeRebuildsItsOutlines(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	if _, err := newManager(t, root, cache, "b1", newFake()).Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Nothing changed on disk except one file, and the server is now v2: the
	// version check happens when the server is needed, and then every outline
	// of the part is asked again.
	put(t, root, "b.go", "package a\n\nfunc Gamma2() {}\n")
	fb := newFake()
	fb.version = "v2"
	m := newManager(t, root, cache, "b1", fb)
	res, err := m.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := fb.calls(); len(got) != 2 {
		t.Fatalf("outline calls = %v, want both Go files", got)
	}
	found := false
	for _, w := range res.Report.Warnings {
		found = found || strings.Contains(w, "v1 to v2") || strings.Contains(w, "from version v1")
	}
	if !found {
		t.Fatalf("no warning about the version change: %v", res.Report.Warnings)
	}
}

type notReadyErr struct{}

func (notReadyErr) Error() string { return "server is still indexing" }

func TestNotReadyServerRecordsNothing(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	put(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	put(t, root, "b.go", "package a\n")
	fb := newFake()
	fb.notReady = notReadyErr{}
	m := newManager(t, root, cache, "b1", fb)
	_, err := m.Search(context.Background(), SearchQuery{Text: "Alpha"})
	var nr notReadyErr
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want the not-ready error", err)
	}
	if len(fb.calls()) != 0 {
		t.Fatalf("a server that is not ready was asked for outlines: %v", fb.calls())
	}
	entries, _ := os.ReadDir(cache)
	if len(entries) != 0 {
		t.Fatalf("something was recorded: %v", entries)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Indexed != 0 || st.Missing != 2 || st.Warm {
		t.Fatalf("status = %+v", st)
	}

	// Once it is ready the same manager builds.
	fb.mu.Lock()
	fb.notReady = nil
	fb.mu.Unlock()
	out, err := m.Search(context.Background(), SearchQuery{Text: "Alpha"})
	if err != nil || len(out.Hits) != 1 {
		t.Fatalf("after ready: %v %v", err, out)
	}
}

func TestImportsOnlyNeedsNoServer(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	fb := newFake()
	fb.notReady = notReadyErr{}
	m := newManager(t, root, cache, "b1", fb)
	// c.py has no server and a Python extractor; a.go's server is not ready but
	// an import query does not need it.
	out, err := m.Imports(context.Background(), "c.py")
	if err != nil {
		t.Fatal(err)
	}
	if !out.Covered || len(out.Imports) != 1 || out.Imports[0].Spec != "os" {
		t.Fatalf("imports = %+v", out.ImportsResult)
	}
	if fb.readies != 0 {
		t.Fatal("an imports query waited for a server")
	}
	// A language nothing covers is "not covered", not "no imports".
	un, err := m.Imports(context.Background(), "README.md")
	if err != nil {
		t.Fatal(err)
	}
	if un.Covered {
		t.Fatalf("README.md reported as covered: %+v", un.ImportsResult)
	}
}

func TestTwoConcurrentBuildersLeaveAValidCache(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	for i := 0; i < 40; i++ {
		put(t, root, fmt.Sprintf("pkg%d/f%d.go", i%5, i), fmt.Sprintf("package p\n\nfunc F%d() {}\n", i))
	}
	for round := 0; round < 3; round++ {
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := newManager(t, root, cache, "b1", newFake()).Build(context.Background())
				errs <- err
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		fb := newFake()
		res, err := newManager(t, root, cache, "b1", fb).Build(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Report.Discarded) != 0 || res.Report.Built != 0 || res.Report.Fresh != 40 || len(fb.calls()) != 0 {
			t.Fatalf("round %d: the cache two builders wrote is not complete and valid: %+v calls %d", round, res.Report, len(fb.calls()))
		}
		// Start the next round cold.
		if _, err := newManager(t, root, cache, "b1", newFake()).Clear(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	entries, _ := os.ReadDir(cache)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Fatalf("a temporary file was left behind: %s", e.Name())
		}
	}
}

func TestScopeBuildsOnlyWhatTheQueryCanReach(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	put(t, root, "one/a.go", "package a\n\nfunc InOne() {}\n")
	put(t, root, "two/b.go", "package b\n\nfunc InTwo() {}\n")
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	out, err := m.Search(context.Background(), SearchQuery{Text: "In", Path: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fb.calls(); strings.Join(got, ",") != "one/a.go" {
		t.Fatalf("a search under one/ asked about %v", got)
	}
	if len(out.Hits) != 1 || out.Hits[0].File != "one/a.go" {
		t.Fatalf("hits = %v", hitNames(out.Hits))
	}
	if !strings.Contains(out.Report.Notice(), "built lazily") {
		t.Fatalf("no lazy-build notice: %q", out.Report.Notice())
	}
}

func TestStaleEntryOutsideTheScopeCannotReachTheAnswer(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	put(t, root, "one/a.go", "package a\n\nfunc OldName() {}\n")
	put(t, root, "two/b.go", "package b\n\nfunc Other() {}\n")
	m := newManager(t, root, cache, "b1", newFake())
	if _, err := m.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	put(t, root, "one/a.go", "package a\n\nfunc Renamed() {}\n\nfunc More() {}\n")
	// Query scoped to two/: one/a.go is not revalidated, and must not be
	// reported, whatever the query text.
	out, err := m.Search(context.Background(), SearchQuery{Text: "OldName", Path: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Hits) != 0 {
		t.Fatalf("a stale, unrevalidated entry reached the answer: %v", hitNames(out.Hits))
	}
	out, err = m.Search(context.Background(), SearchQuery{Text: "OldName"})
	if err != nil || len(out.Hits) != 0 {
		t.Fatalf("after full revalidation: %v %v", err, hitNames(out.Hits))
	}
}

func TestSymlinkOutOfTheWorkspaceIsNeverRead(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.go"), []byte("package s\n\nfunc Secret() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	if err := os.Symlink(filepath.Join(outside, "secret.go"), filepath.Join(root, "link.go")); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	if _, err := m.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := fb.calls(); strings.Join(got, ",") != "a.go" {
		t.Fatalf("a symlink was outlined: %v", got)
	}
	out, _ := m.Search(context.Background(), SearchQuery{Text: "Secret"})
	if len(out.Hits) != 0 {
		t.Fatalf("a symlinked file's symbols are indexed: %v", hitNames(out.Hits))
	}
	st, _ := m.Status(context.Background())
	if len(st.Skipped) != 1 || st.Skipped[0].Reason != "symlink" {
		t.Fatalf("skipped = %+v", st.Skipped)
	}
}

func TestLargeAndBinaryFilesAreSkippedWithReasons(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	put(t, root, "a.go", "package a\n\nfunc Alpha() {}\n")
	put(t, root, "big.go", "package a\n"+strings.Repeat("// padding\n", 200))
	put(t, root, "bin.go", "package a\x00\x01\x02")
	fb := newFake()
	m := New(Options{Root: root, CacheDir: cache, Build: "b1", Backend: fb, MaxFileBytes: 1024, Parallelism: 2})
	if _, err := m.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	st, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, g := range st.Skipped {
		got[g.Reason] = g.Count
	}
	if got["too_large"] != 1 || got["binary"] != 1 || st.Indexed != 1 {
		t.Fatalf("skipped %v, indexed %d", got, st.Indexed)
	}
	if fb.calls()[0] != "a.go" || len(fb.calls()) != 1 {
		t.Fatalf("outline calls = %v", fb.calls())
	}
}

func TestClearForgetsMemoryAndDisk(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	m := newManager(t, root, cache, "b1", newFake())
	if _, err := m.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := m.Clear(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Entries != 3 || res.Parts != 2 || res.Bytes == 0 {
		t.Fatalf("clear = %+v", res)
	}
	if _, err := os.Stat(cache); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache directory still there: %v", err)
	}
	st, _ := m.Status(context.Background())
	if st.Indexed != 0 || st.Missing != 3 {
		t.Fatalf("status after clear = %+v", st)
	}
}

func TestCountsAreWarmOnlyAndNeverBuild(t *testing.T) {
	root, cache := newRoot(t), t.TempDir()
	sampleTree(t, root)
	fb := newFake()
	m := newManager(t, root, cache, "b1", fb)
	c, err := m.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if c.Warm || len(fb.calls()) != 0 {
		t.Fatalf("counts on a cold index: %+v", c)
	}
	if _, err := m.Build(context.Background()); err != nil {
		t.Fatal(err)
	}
	put(t, root, "b.go", "package a\n\nfunc Changed() {}\n\nfunc Again() {}\n")
	c, err = m.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !c.Warm || c.Files["a.go"].Symbols != 2 || c.NotCurrent != 1 {
		t.Fatalf("counts = %+v", c)
	}
	if _, ok := c.Files["b.go"]; ok {
		t.Fatal("a changed file's stale counts were reported")
	}
}

func TestBuildSymbolsIDsDocsAndParents(t *testing.T) {
	src := "package a\n\n// Server serves requests. It is fast.\ntype Server struct{}\n\n// Handle handles one.\n//go:noinline\nfunc (s *Server) Handle(x int) error {\n\treturn nil\n}\n"
	rng := func(l1, l2 uint32) protocol.Range {
		return protocol.Range{Start: protocol.Position{Line: l1}, End: protocol.Position{Line: l2, Character: 1}}
	}
	syms := []symbols.Symbol{
		{Name: "(*Server).Handle", Qualified: "(*Server).Handle", Kind: "method", Range: rng(7, 7), Full: rng(7, 9), Container: "Server", HasRange: true},
		{Name: "Server", Qualified: "Server", Kind: "struct", Range: rng(3, 3), Full: rng(3, 3), HasRange: true},
	}
	got := buildSymbols("a.go", "go", []byte(src), syms)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	if got[0].ID != "a.go::Server#struct" || got[0].Doc != "Server serves requests." || got[0].Line != 4 {
		t.Fatalf("type: %+v", got[0])
	}
	h := got[1]
	if h.ID != "a.go::Server.Handle#method" || h.Name != "Handle" || h.Container != "Server" || h.Doc != "Handle handles one." || h.Line != 8 || h.EndLine != 10 {
		t.Fatalf("method: %+v", h)
	}
	if !strings.HasPrefix(h.Signature, "func (s *Server) Handle(x int) error") {
		t.Fatalf("signature = %q", h.Signature)
	}
}

func TestTrackerReusesTheListUntilSomethingMoves(t *testing.T) {
	root := newRoot(t)
	put(t, root, "a.go", "package a\n")
	put(t, root, "sub/b.go", "package b\n")
	// A directory modified within the last two seconds is "young" and always
	// listed again; age these so the fast path is what is being tested.
	touchDir(t, root, ".")
	touchDir(t, root, "sub")
	tr := NewTracker(root)
	ctx := context.Background()
	s1, err := tr.Scan(ctx)
	if err != nil || !s1.Listed || len(s1.Files) != 2 {
		t.Fatalf("first scan: %+v %v", s1, err)
	}
	s2, _ := tr.Scan(ctx)
	if s2.Listed {
		t.Fatal("an unchanged tree was listed again")
	}
	if d := Diff(s1, s2); len(d) != 0 {
		t.Fatalf("diff of nothing = %v", d)
	}

	// New file: the directory's mtime moves and the list is made again.
	put(t, root, "sub/c.go", "package c\n")
	touchDir(t, root, "sub")
	s3, _ := tr.Scan(ctx)
	if !s3.Listed || len(s3.Files) != 3 {
		t.Fatalf("new file not seen: %+v", s3)
	}
	if d := Diff(s2, s3); len(d) != 1 || d[0].Path != "sub/c.go" || d[0].Kind != Created {
		t.Fatalf("diff = %v", d)
	}

	// Edit.
	put(t, root, "a.go", "package a // edited\n")
	s4, _ := tr.Scan(ctx)
	if d := Diff(s3, s4); len(d) != 1 || d[0].Path != "a.go" || d[0].Kind != Changed {
		t.Fatalf("diff = %v", d)
	}
	// Delete.
	if err := os.Remove(filepath.Join(root, "sub", "b.go")); err != nil {
		t.Fatal(err)
	}
	s5, _ := tr.Scan(ctx)
	if d := Diff(s4, s5); len(d) != 1 || d[0].Path != "sub/b.go" || d[0].Kind != Deleted {
		t.Fatalf("diff = %v", d)
	}
}

// touchDir moves a directory's mtime the way creating a file in it does, well away
// from the racy window so the test does not depend on the young-directory rule.
func touchDir(t *testing.T, root, rel string) {
	t.Helper()
	mt := time.Now().Add(-time.Minute)
	if err := os.Chtimes(filepath.Join(root, rel), mt, mt); err != nil {
		t.Fatal(err)
	}
}

func TestTrackerDetectsARacySameSizeEdit(t *testing.T) {
	root := newRoot(t)
	putAged(t, root, "a.go", "package a // 1\n", 0)
	tr := NewTracker(root)
	ctx := context.Background()
	s1, _ := tr.Scan(ctx)
	fi, _ := os.Stat(filepath.Join(root, "a.go"))
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a // 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(filepath.Join(root, "a.go"), fi.ModTime(), fi.ModTime())
	s2, _ := tr.Scan(ctx)
	if d := Diff(s1, s2); len(d) != 1 || d[0].Kind != Changed {
		t.Fatalf("a racy same-size edit was missed: %v", d)
	}
}

func TestPersistRoundTripAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	key := partKey{Schema: SchemaVersion, Build: "b", Server: "gopls", ServerKey: "k"}
	files := []*File{{Path: "b.go", Hash: "h2"}, {Path: "a.go", Hash: "h1", HasOutline: true, Symbols: []Symbol{{ID: "a.go::X#function", Name: "X", Doc: "<x> & y  "}}}}
	if _, err := writePart(dir, "/r", key, "v1", files, time.Now()); err != nil {
		t.Fatal(err)
	}
	p, why := readPart(filepath.Join(dir, partName("gopls")), func(k partKey) (bool, string) { return k == key, "key" })
	if p == nil {
		t.Fatalf("round trip failed: %s", why)
	}
	if len(p.files) != 2 || p.files[0].Path != "a.go" || p.files[0].Symbols[0].Doc != "<x> & y  " || p.serverVersion != "v1" {
		t.Fatalf("read back %+v", p.files)
	}
	// No temp file is left.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("directory = %v", entries)
	}
	// A different key is refused.
	if p, _ := readPart(filepath.Join(dir, partName("gopls")), func(partKey) (bool, string) { return false, "nope" }); p != nil {
		t.Fatal("a part with the wrong key was accepted")
	}
}
