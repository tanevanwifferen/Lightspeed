package router

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// The fixture tree. A repo with a nested Go module, a second Go
// module beside it, a Cargo project at the top, a Python project in a
// subdirectory, Python and Rust files that belong to none of them,
// and — outside the repo — a Go workspace, so that marker
// significance can be told apart from marker proximity.
//
//	base/
//	  repo/
//	    .git/                 <- repo-wide fallback marker
//	    Cargo.toml            <- rust root
//	    README.md
//	    data.bin
//	    src/lib.rs
//	    src/nested/deep.rs
//	    gomod/go.mod          <- nested Go module
//	    gomod/main.go
//	    gomod/sub/deep.go
//	    gomod/src/other.rs    <- inside the module, under the cargo root
//	    other/go.mod          <- a second Go module beside the first
//	    other/main.go
//	    python/pyproject.toml <- python root
//	    python/app.py
//	    python/pkg/mod.py
//	    loose/script.py       <- python file with no python root
//	    link -> gomod         <- symlinked view of the module
//	  work/go.work            <- outranks the go.mod below it
//	  work/mod/go.mod
//	  work/mod/m.go
var fixture = []string{
	"repo/.git/",
	"repo/Cargo.toml",
	"repo/README.md",
	"repo/data.bin",
	"repo/src/lib.rs",
	"repo/src/nested/deep.rs",
	"repo/gomod/go.mod",
	"repo/gomod/main.go",
	"repo/gomod/sub/deep.go",
	"repo/gomod/src/other.rs",
	"repo/other/go.mod",
	"repo/other/main.go",
	"repo/python/pyproject.toml",
	"repo/python/app.py",
	"repo/python/pkg/mod.py",
	"repo/loose/script.py",
	"work/go.work",
	"work/mod/go.mod",
	"work/mod/m.go",
}

// newFixture materialises the tree and returns its base directory,
// with symlinks already resolved so that expectations can be written
// as plain joins.
func newFixture(t *testing.T) string {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks(TempDir()) = %v", err)
	}
	for _, entry := range fixture {
		full := filepath.Join(base, entry)
		if strings.HasSuffix(entry, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(base, "repo", "gomod"), filepath.Join(base, "repo", "link")); err != nil {
		t.Fatal(err)
	}
	return base
}

// def builds a definition for tests, so that a test case can say what
// it is about instead of restating the whole schema.
func def(name string, priority int, languages, globs, markers []string) *serverdef.ServerDef {
	return &serverdef.ServerDef{
		SchemaVersion: serverdef.SchemaVersion,
		Name:          name,
		Activation: serverdef.Activation{
			Languages:   languages,
			Globs:       globs,
			RootMarkers: markers,
			Priority:    priority,
		},
		Server: serverdef.Server{Command: []string{name}},
	}
}

// builtinRouter routes with the shipped defaults only.
func builtinRouter(t *testing.T) *Router {
	t.Helper()
	r, err := New(serverdef.Builtins()...)
	if err != nil {
		t.Fatalf("New(Builtins()) = %v", err)
	}
	return r
}

type wantMatch struct {
	server string // definition name
	root   string // slash path relative to the fixture base
	marker string // root marker, "" for the fallback root
	glob   string // matched glob, "" if claimed by language alone
}

func checkMatches(t *testing.T, path string, got []Match, base string, want []wantMatch) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("Resolve(%s) returned %d matches (%s), want %d (%s)",
			path, len(got), summarise(got), len(want), summariseWant(want))
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Server.Name != w.server {
			t.Errorf("Resolve(%s) match %d server = %q, want %q", path, i, g.Server.Name, w.server)
		}
		if wantRoot := filepath.Join(base, filepath.FromSlash(w.root)); g.Root != wantRoot {
			t.Errorf("Resolve(%s) match %d root = %q, want %q", path, i, g.Root, wantRoot)
		}
		if g.RootMarker != w.marker {
			t.Errorf("Resolve(%s) match %d marker = %q, want %q", path, i, g.RootMarker, w.marker)
		}
		if g.MatchedGlob != w.glob {
			t.Errorf("Resolve(%s) match %d glob = %q, want %q", path, i, g.MatchedGlob, w.glob)
		}
		if got, want := g.Fallback(), w.marker == ""; got != want {
			t.Errorf("Resolve(%s) match %d Fallback() = %t, want %t", path, i, got, want)
		}
	}
}

func summarise(matches []Match) string {
	var parts []string
	for _, m := range matches {
		parts = append(parts, m.Server.Name+"@"+m.Root)
	}
	return strings.Join(parts, ", ")
}

func summariseWant(want []wantMatch) string {
	var parts []string
	for _, w := range want {
		parts = append(parts, w.server+"@"+w.root)
	}
	return strings.Join(parts, ", ")
}

// TestResolveBuiltins is the polyglot walk-through: every interesting
// file in the fixture, routed by the shipped defaults.
func TestResolveBuiltins(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	tests := []struct {
		name string
		path string
		want []wantMatch
	}{{
		// Nearest root wins: the module, not the repo's .git.
		name: "file in nested go module",
		path: "repo/gomod/main.go",
		want: []wantMatch{{"gopls", "repo/gomod", "go.mod", "**/*.go"}},
	}, {
		name: "file deeper in nested go module",
		path: "repo/gomod/sub/deep.go",
		want: []wantMatch{{"gopls", "repo/gomod", "go.mod", "**/*.go"}},
	}, {
		// The sibling module resolves to itself: a root marker in
		// one module is not visible from the other.
		name: "file in sibling go module",
		path: "repo/other/main.go",
		want: []wantMatch{{"gopls", "repo/other", "go.mod", "**/*.go"}},
	}, {
		name: "go.mod itself",
		path: "repo/gomod/go.mod",
		want: []wantMatch{{"gopls", "repo/gomod", "go.mod", "**/go.mod"}},
	}, {
		// Marker significance beats proximity: go.work outranks the
		// go.mod in the same tree.
		name: "file in go workspace member",
		path: "work/mod/m.go",
		want: []wantMatch{{"gopls", "work", "go.work", "**/*.go"}},
	}, {
		name: "rust file under cargo root",
		path: "repo/src/lib.rs",
		want: []wantMatch{{"rust-analyzer", "repo", "Cargo.toml", "**/*.rs"}},
	}, {
		name: "rust file nested under cargo root",
		path: "repo/src/nested/deep.rs",
		want: []wantMatch{{"rust-analyzer", "repo", "Cargo.toml", "**/*.rs"}},
	}, {
		// A Rust file inside the Go module still belongs to the
		// cargo root: roots are per server, not per directory.
		name: "rust file inside the go module",
		path: "repo/gomod/src/other.rs",
		want: []wantMatch{{"rust-analyzer", "repo", "Cargo.toml", "**/*.rs"}},
	}, {
		name: "cargo manifest",
		path: "repo/Cargo.toml",
		want: []wantMatch{{"rust-analyzer", "repo", "Cargo.toml", "**/Cargo.toml"}},
	}, {
		name: "python file under python root",
		path: "repo/python/app.py",
		want: []wantMatch{{"pyright", "repo/python", "pyproject.toml", "**/*.py"}},
	}, {
		name: "python file deeper under python root",
		path: "repo/python/pkg/mod.py",
		want: []wantMatch{{"pyright", "repo/python", "pyproject.toml", "**/*.py"}},
	}, {
		// The python root does not leak sideways: this file falls
		// back to the repo's .git, not to repo/python.
		name: "python file with no python root",
		path: "repo/loose/script.py",
		want: []wantMatch{{"pyright", "repo", ".git", "**/*.py"}},
	}, {
		// A file that does not exist yet still routes.
		name: "not-yet-created go file",
		path: "repo/gomod/brand_new.go",
		want: []wantMatch{{"gopls", "repo/gomod", "go.mod", "**/*.go"}},
	}, {
		// Symlinked paths collapse onto the real root, so the
		// daemon keys one session, not two.
		name: "go file reached through a symlink",
		path: "repo/link/main.go",
		want: []wantMatch{{"gopls", "repo/gomod", "go.mod", "**/*.go"}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(base, filepath.FromSlash(tt.path))
			got, err := r.Resolve(path)
			if err != nil {
				t.Fatalf("Resolve(%s) = %v", tt.path, err)
			}
			checkMatches(t, tt.path, got, base, tt.want)
		})
	}
}

// TestResolveNoServer covers PLAN §4's exit code 3: a file nobody
// claims must produce a clear, identifiable refusal rather than an
// empty result that reads like "no references found".
func TestResolveNoServer(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	tests := []struct {
		name     string
		path     string
		wantLang string
	}{
		{"known language, no server", "repo/README.md", "markdown"},
		{"unknown file type", "repo/data.bin", ""},
		{"directory", "repo/gomod", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(base, filepath.FromSlash(tt.path))
			got, err := r.Resolve(path)
			if err == nil {
				t.Fatalf("Resolve(%s) = %s, want a no-server error", tt.path, summarise(got))
			}
			if got != nil {
				t.Errorf("Resolve(%s) returned %s alongside an error, want no matches", tt.path, summarise(got))
			}
			if !errors.Is(err, ErrNoServer) {
				t.Errorf("Resolve(%s) error = %v, want it to wrap ErrNoServer", tt.path, err)
			}
			var noServer *NoServerError
			if !errors.As(err, &noServer) {
				t.Fatalf("Resolve(%s) error = %T, want *NoServerError", tt.path, err)
			}
			if noServer.Path != path {
				t.Errorf("NoServerError.Path = %q, want %q", noServer.Path, path)
			}
			if noServer.LanguageID != tt.wantLang {
				t.Errorf("NoServerError.LanguageID = %q, want %q", noServer.LanguageID, tt.wantLang)
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the path", err)
			}
		})
	}

	if CodeNoServer == "" {
		t.Error("CodeNoServer is empty; the CLI needs a machine code for exit 3")
	}
}

// TestResolvePriority checks the ordering contract: priority
// descending, then name, regardless of the order definitions were
// loaded in.
func TestResolvePriority(t *testing.T) {
	base := newFixture(t)
	goFile := filepath.Join(base, "repo", "gomod", "main.go")

	t.Run("priority breaks the tie", func(t *testing.T) {
		// Loaded lowest-priority first, on purpose.
		r, err := New(
			def("go-low", 10, []string{"go"}, nil, []string{"go.mod"}),
			def("go-high", 90, []string{"go"}, nil, []string{"go.mod"}),
			def("go-mid", 50, []string{"go"}, nil, []string{"go.mod"}),
		)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		got, err := r.Resolve(goFile)
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		checkMatches(t, goFile, got, base, []wantMatch{
			{"go-high", "repo/gomod", "go.mod", ""},
			{"go-mid", "repo/gomod", "go.mod", ""},
			{"go-low", "repo/gomod", "go.mod", ""},
		})
	})

	t.Run("equal priority falls back to name", func(t *testing.T) {
		r, err := New(
			def("zebra", 50, []string{"go"}, nil, []string{"go.mod"}),
			def("alpha", 50, []string{"go"}, nil, []string{"go.mod"}),
		)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		got, err := r.Resolve(goFile)
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		checkMatches(t, goFile, got, base, []wantMatch{
			{"alpha", "repo/gomod", "go.mod", ""},
			{"zebra", "repo/gomod", "go.mod", ""},
		})
	})

	t.Run("servers with different roots coexist", func(t *testing.T) {
		// The polyglot case that motivates carrying a root per
		// match: same file, two servers, two roots.
		r, err := New(
			def("modscoped", 60, []string{"go"}, nil, []string{"go.mod"}),
			def("repoescoped", 40, []string{"go"}, nil, []string{".git"}),
		)
		if err != nil {
			t.Fatalf("New() = %v", err)
		}
		got, err := r.Resolve(goFile)
		if err != nil {
			t.Fatalf("Resolve() = %v", err)
		}
		checkMatches(t, goFile, got, base, []wantMatch{
			{"modscoped", "repo/gomod", "go.mod", ""},
			{"repoescoped", "repo", ".git", ""},
		})
	})
}

// TestResolveGlobsDoNotCrossRoots pins the property that makes
// per-server roots safe: an anchored glob is anchored at that
// server's root, and cannot reach a like-named directory elsewhere.
func TestResolveGlobsDoNotCrossRoots(t *testing.T) {
	base := newFixture(t)
	r, err := New(def("srcrust", 50, nil, []string{"src/**/*.rs"}, []string{"Cargo.toml"}))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	// repo/src/lib.rs is src/lib.rs relative to the cargo root.
	inRoot := filepath.Join(base, "repo", "src", "lib.rs")
	got, err := r.Resolve(inRoot)
	if err != nil {
		t.Fatalf("Resolve(repo/src/lib.rs) = %v", err)
	}
	checkMatches(t, "repo/src/lib.rs", got, base, []wantMatch{
		{"srcrust", "repo", "Cargo.toml", "src/**/*.rs"},
	})

	// repo/gomod/src/other.rs is also under a directory called src,
	// but relative to the same cargo root it is gomod/src/other.rs.
	crossRoot := filepath.Join(base, "repo", "gomod", "src", "other.rs")
	if got, err := r.Resolve(crossRoot); err == nil {
		t.Errorf("Resolve(repo/gomod/src/other.rs) = %s, want no server: the glob is anchored at the cargo root", summarise(got))
	} else if !errors.Is(err, ErrNoServer) {
		t.Errorf("Resolve(repo/gomod/src/other.rs) error = %v, want ErrNoServer", err)
	}
}

// TestResolveAbsoluteGlob covers the one pattern form that is not
// root-relative.
func TestResolveAbsoluteGlob(t *testing.T) {
	base := newFixture(t)
	pattern := filepath.ToSlash(filepath.Join(base, "repo", "gomod", "**", "*.go"))
	r, err := New(def("abs", 50, nil, []string{pattern}, []string{"go.mod"}))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if _, err := r.Resolve(filepath.Join(base, "repo", "gomod", "sub", "deep.go")); err != nil {
		t.Errorf("Resolve(repo/gomod/sub/deep.go) = %v, want a match on the absolute glob", err)
	}
	if _, err := r.Resolve(filepath.Join(base, "repo", "other", "main.go")); !errors.Is(err, ErrNoServer) {
		t.Errorf("Resolve(repo/other/main.go) error = %v, want ErrNoServer", err)
	}
}

// TestResolveFallbackRoot covers the no-marker-anywhere case: still a
// match, rooted at the file's own directory and flagged as such.
func TestResolveFallbackRoot(t *testing.T) {
	base := newFixture(t)
	r, err := New(
		def("orphan", 50, []string{"go"}, nil, []string{"lightspeed-test-no-such-marker"}),
		def("markerless", 40, []string{"go"}, nil, nil),
	)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	path := filepath.Join(base, "repo", "gomod", "sub", "deep.go")
	got, err := r.Resolve(path)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	checkMatches(t, path, got, base, []wantMatch{
		{"orphan", "repo/gomod/sub", "", ""},
		{"markerless", "repo/gomod/sub", "", ""},
	})
}

// TestResolveAs covers the caller-supplied language id, which is both
// the --language escape hatch and the way a directory can be routed.
func TestResolveAs(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	t.Run("overrides detection", func(t *testing.T) {
		path := filepath.Join(base, "repo", "data.bin")
		got, err := r.ResolveAs(path, "go")
		if err != nil {
			t.Fatalf("ResolveAs(data.bin, go) = %v", err)
		}
		checkMatches(t, path, got, base, []wantMatch{{"gopls", "repo", ".git", ""}})
	})

	t.Run("directory starts the root walk at itself", func(t *testing.T) {
		path := filepath.Join(base, "repo", "gomod")
		got, err := r.ResolveAs(path, "go")
		if err != nil {
			t.Fatalf("ResolveAs(repo/gomod, go) = %v", err)
		}
		checkMatches(t, path, got, base, []wantMatch{{"gopls", "repo/gomod", "go.mod", ""}})
		if got[0].LanguageID != "go" {
			t.Errorf("LanguageID = %q, want %q", got[0].LanguageID, "go")
		}
	})
}

// TestGroup covers the batch shape the daemon needs: one bucket per
// session, across a deliberately multi-root, polyglot input.
func TestGroup(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	rel := []string{
		"repo/gomod/main.go",
		"repo/python/app.py",
		"repo/gomod/sub/deep.go",
		"repo/other/main.go",
		"repo/README.md",
		"repo/src/lib.rs",
		"repo/link/main.go", // the same file as repo/gomod/main.go
	}
	paths := make([]string, len(rel))
	for i, p := range rel {
		paths[i] = filepath.Join(base, filepath.FromSlash(p))
	}

	groups, unclaimed, err := r.Group(paths)
	if err != nil {
		t.Fatalf("Group() = %v", err)
	}

	type wantGroup struct {
		server string
		root   string
		paths  []string
	}
	// Equal priority throughout, so the order is by name, then root.
	want := []wantGroup{
		{"gopls", "repo/gomod", []string{"repo/gomod/main.go", "repo/gomod/sub/deep.go", "repo/link/main.go"}},
		{"gopls", "repo/other", []string{"repo/other/main.go"}},
		{"pyright", "repo/python", []string{"repo/python/app.py"}},
		{"rust-analyzer", "repo", []string{"repo/src/lib.rs"}},
	}
	if len(groups) != len(want) {
		t.Fatalf("Group() returned %d groups, want %d: %+v", len(groups), len(want), groups)
	}
	for i, w := range want {
		g := groups[i]
		if g.Server.Name != w.server {
			t.Errorf("group %d server = %q, want %q", i, g.Server.Name, w.server)
		}
		if wantRoot := filepath.Join(base, filepath.FromSlash(w.root)); g.Root != wantRoot {
			t.Errorf("group %d root = %q, want %q", i, g.Root, wantRoot)
		}
		if len(g.Paths) != len(w.paths) {
			t.Fatalf("group %d has %d paths (%v), want %d (%v)", i, len(g.Paths), g.Paths, len(w.paths), w.paths)
		}
		for j, wp := range w.paths {
			if want := filepath.Join(base, filepath.FromSlash(wp)); g.Paths[j] != want {
				t.Errorf("group %d path %d = %q, want %q", i, j, g.Paths[j], want)
			}
		}
	}

	// The unclaimed file is reported, as given, and does not fail the
	// batch.
	if len(unclaimed) != 1 || unclaimed[0] != filepath.Join(base, "repo", "README.md") {
		t.Errorf("unclaimed = %v, want just repo/README.md", unclaimed)
	}
}

func TestGroupEmpty(t *testing.T) {
	r := builtinRouter(t)
	groups, unclaimed, err := r.Group(nil)
	if err != nil {
		t.Fatalf("Group(nil) = %v", err)
	}
	if len(groups) != 0 || len(unclaimed) != 0 {
		t.Errorf("Group(nil) = %v, %v, want empty", groups, unclaimed)
	}
}

func TestNewErrors(t *testing.T) {
	tests := []struct {
		name string
		defs []*serverdef.ServerDef
		want string
	}{{
		name: "invalid definition",
		defs: []*serverdef.ServerDef{def("", 50, []string{"go"}, nil, nil)},
		want: "name is required",
	}, {
		name: "duplicate name",
		defs: []*serverdef.ServerDef{
			def("gopls", 50, []string{"go"}, nil, nil),
			def("gopls", 60, []string{"go"}, nil, nil),
		},
		want: `server "gopls" is defined twice`,
	}, {
		name: "bad glob",
		defs: []*serverdef.ServerDef{def("broken", 50, nil, []string{"**/*.[go"}, nil)},
		want: `server "broken": glob`,
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := New(tt.defs...)
			if err == nil {
				t.Fatalf("New() = %+v, want an error mentioning %q", r, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("New() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestNewEmptyAndServers(t *testing.T) {
	r, err := New()
	if err != nil {
		t.Fatalf("New() = %v", err)
	}
	if got := r.Servers(); len(got) != 0 {
		t.Errorf("Servers() = %v, want empty", got)
	}
	// With no definitions, every path is a no-server result rather
	// than a panic or an empty success.
	if _, err := r.Resolve("main.go"); !errors.Is(err, ErrNoServer) {
		t.Errorf("Resolve() error = %v, want ErrNoServer", err)
	}

	r = builtinRouter(t)
	if got, want := len(r.Servers()), len(serverdef.Builtins()); got != want {
		t.Errorf("len(Servers()) = %d, want %d", got, want)
	}
	if got := r.Servers()[0].Name; got != "gopls" {
		t.Errorf("Servers()[0] = %q, want the load order preserved (gopls first)", got)
	}
}

func TestResolveRelativePathAndErrors(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	// A relative path is resolved against the process working
	// directory, like every other path a CLI is handed.
	t.Chdir(filepath.Join(base, "repo", "gomod"))
	got, err := r.Resolve("sub/deep.go")
	if err != nil {
		t.Fatalf(`Resolve("sub/deep.go") = %v`, err)
	}
	checkMatches(t, "sub/deep.go", got, base, []wantMatch{
		{"gopls", "repo/gomod", "go.mod", "**/*.go"},
	})

	if _, err := r.Resolve(""); err == nil || errors.Is(err, ErrNoServer) {
		t.Errorf(`Resolve("") error = %v, want an argument error`, err)
	}
}

// TestResolveDeepNonexistentPath covers canonicalisation of a path
// several missing components deep, which is what `--symbol` on a file
// an agent is about to create looks like.
func TestResolveDeepNonexistentPath(t *testing.T) {
	base := newFixture(t)
	r := builtinRouter(t)

	path := filepath.Join(base, "repo", "gomod", "new", "deeper", "file.go")
	got, err := r.Resolve(path)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	// The root walk starts at the nearest directory that exists on
	// the way down, so the module is still found.
	checkMatches(t, path, got, base, []wantMatch{
		{"gopls", "repo/gomod", "go.mod", "**/*.go"},
	})
}

// TestRelativeTo covers the defensive branch: if a root ever failed to
// be an ancestor, matching must fall back to the absolute path rather
// than to a "../.." subject that no pattern anticipates.
func TestRelativeTo(t *testing.T) {
	tests := []struct{ root, abs, want string }{
		{"/a/b", "/a/b/c.go", "c.go"},
		{"/a/b", "/a/b/c/d.go", "c/d.go"},
		{"/a/b", "/a/b", "."},
		{"/a/b", "/a/c.go", "/a/c.go"},
		{"/a/b", "/x/y/c.go", "/x/y/c.go"},
	}
	for _, tt := range tests {
		if got := relativeTo(filepath.FromSlash(tt.root), filepath.FromSlash(tt.abs)); got != tt.want {
			t.Errorf("relativeTo(%q, %q) = %q, want %q", tt.root, tt.abs, got, tt.want)
		}
	}
}
