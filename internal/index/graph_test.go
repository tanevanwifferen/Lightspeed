package index

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func impGoWorkspace() ([]*File, *impWS) {
	return impFiles(map[string]string{
		"cmd/app/main.go":            "package main\nimport (\"example.com/app/internal/core\"; \"example.com/app/internal/util\"; \"fmt\")",
		"internal/core/core.go":      "package core\nimport \"example.com/app/internal/util\"",
		"internal/core/more.go":      "package core\nimport \"example.com/app/internal/util\"",
		"internal/core/core_test.go": "package core_test\nimport \"example.com/app/internal/testonly\"",
		"internal/util/util.go":      "package util\nimport \"os\"",
		"internal/testonly/t.go":     "package testonly\nimport \"example.com/app/internal/core\"",
	}, map[string]string{"go.mod": "module example.com/app\n"})
}

func TestGraphGoPackagesAndImporters(t *testing.T) {
	files, ws := impGoWorkspace()
	g := BuildGraph(files, ws)

	res, err := g.ImportersOf("internal/util")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, im := range res.Importers {
		got = append(got, im.File)
	}
	impEqual(t, "importers of util", got, []string{"cmd/app/main.go", "internal/core/core.go", "internal/core/more.go"})
	if !res.Covered || res.Target != "internal/util" {
		t.Errorf("result = %+v", res)
	}

	// The same node by file, by directory and by import path.
	for _, target := range []string{"internal/util/util.go", "internal/util", "./internal/util/", "example.com/app/internal/util"} {
		r, err := g.ImportersOf(target)
		if err != nil || r.Target != "internal/util" || len(r.Importers) != 3 {
			t.Errorf("ImportersOf(%q) = %+v, %v", target, r, err)
		}
	}
	if _, err := g.ImportersOf("nope/nothing.go"); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("unknown target: err = %v", err)
	}

	// ImportsOf keeps test files' imports even though they are not edges.
	imp := g.ImportsOf("internal/core/core_test.go")
	if !imp.Covered || !imp.Test || len(imp.Imports) != 1 || imp.Imports[0].Targets[0] != "internal/testonly" {
		t.Errorf("test file imports = %+v", imp)
	}
	res, _ = g.ImportersOf("internal/testonly")
	if len(res.Importers) != 0 {
		t.Errorf("a _test.go import must not be an edge: %+v", res.Importers)
	}
	main := g.ImportsOf("cmd/app/main.go")
	var cats []string
	for _, ri := range main.Imports {
		cats = append(cats, ri.Spec+"="+ri.Category)
	}
	impEqual(t, "main imports", cats, []string{
		"example.com/app/internal/core=workspace", "example.com/app/internal/util=workspace", "fmt=stdlib",
	})
}

func TestGraphNoCyclesFromGoTestPackages(t *testing.T) {
	files, ws := impGoWorkspace()
	// core_test imports testonly, which imports core: a cycle only if the test
	// file's edge counted.
	if c := BuildGraph(files, ws).Cycles(); len(c) != 0 {
		t.Errorf("cycles = %v", c)
	}
}

func TestGraphCyclesNonGo(t *testing.T) {
	files, ws := impFiles(map[string]string{
		"a.py": "import b\n",
		"b.py": "import a\n",
		"c.py": "import d\n",
		"d.py": "import e\n",
		"e.py": "import c\nimport e\n",
		"f.py": "import a\n",
	}, nil)
	g := BuildGraph(files, ws)
	got := g.Cycles()
	want := [][]string{{"a.py", "b.py"}, {"c.py", "d.py", "e.py"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cycles = %v, want %v", got, want)
	}

	// A file that resolves to itself names an item of its own module (Rust
	// `use self::Item`, Python `from . import name` in __init__.py): no edge, so
	// no false cycle of one.
	files, ws = impFiles(map[string]string{
		"s.py":            "import s\n",
		"t.py":            "import s\n",
		"pkg/__init__.py": "from . import thing_name\n",
		"Cargo.toml":      "",
		"src/lib.rs":      "mod util;\nuse crate::Thing;\n",
		"src/util.rs":     "use self::nothere::x;\nuse self::Enum::*;\n",
	}, nil)
	g = BuildGraph(files, ws)
	if got := g.Cycles(); len(got) != 0 {
		t.Errorf("self resolutions reported as cycles: %v", got)
	}
	for _, f := range []string{"s.py", "pkg/__init__.py", "src/util.rs"} {
		for _, ri := range g.ImportsOf(f).Imports {
			if ri.Category != CategorySelf || len(ri.Targets) != 0 {
				t.Errorf("%s: %s = %+v, want a target-less %q import", f, ri.Spec, ri, CategorySelf)
			}
		}
	}

	// Walk reports the cycle and renders it.
	walk, err := BuildGraph(mustFiles(impFiles(map[string]string{"a.py": "import b\n", "b.py": "import a\n"}, nil))).Walk("a.py", 5, DirOut, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(walk.Cycles, [][]string{{"a.py", "b.py"}}) {
		t.Errorf("walk cycles = %v", walk.Cycles)
	}
	var buf bytes.Buffer
	RenderWalkTree(&buf, walk)
	want2 := "a.py\n  b.py\n    a.py (cycle)\ncycles:\n  a.py -> b.py -> a.py\n"
	if buf.String() != want2 {
		t.Errorf("tree:\n%s\nwant:\n%s", buf.String(), want2)
	}
}

func mustFiles(f []*File, ws *impWS) ([]*File, Workspace) { return f, ws }

func TestGraphWalk(t *testing.T) {
	files, ws := impFiles(map[string]string{
		"a.py": "import b\nimport c\nimport os\n",
		"b.py": "import d\n",
		"c.py": "import d\n",
		"d.py": "import e\n",
		"e.py": "\n",
		"x.py": "import a\n",
	}, nil)
	g := BuildGraph(files, ws)

	ids := func(r WalkResult) []string {
		var out []string
		for _, n := range r.Nodes {
			out = append(out, n.ID)
		}
		return out
	}
	edgeStrs := func(r WalkResult) []string {
		var out []string
		for _, e := range r.Edges {
			out = append(out, e.From+">"+e.To)
		}
		return out
	}

	r, err := g.Walk("a.py", 1, DirOut, false)
	if err != nil {
		t.Fatal(err)
	}
	impEqual(t, "out depth 1", ids(r), []string{"a.py", "b.py", "c.py"})

	r, _ = g.Walk("a.py", 2, DirOut, false)
	impEqual(t, "out depth 2", ids(r), []string{"a.py", "b.py", "c.py", "d.py"})
	impEqual(t, "out depth 2 edges", edgeStrs(r), []string{"a.py>b.py", "a.py>c.py", "b.py>d.py", "c.py>d.py"})
	if r.Nodes[3].Depth != 2 {
		t.Errorf("depth of d = %d", r.Nodes[3].Depth)
	}

	r, _ = g.Walk("d.py", 2, DirIn, false)
	impEqual(t, "in depth 2", ids(r), []string{"d.py", "b.py", "c.py", "a.py"})

	r, _ = g.Walk("b.py", 1, DirBoth, false)
	impEqual(t, "both", ids(r), []string{"b.py", "a.py", "d.py"})

	r, _ = g.Walk("a.py", 1, DirOut, true)
	impEqual(t, "external", edgeStrs(r), []string{"a.py>b.py", "a.py>c.py", "a.py>os"})
	if last := r.Nodes[len(r.Nodes)-1]; last.ID != "os" || last.Kind != NodeExternal {
		t.Errorf("external node = %+v", last)
	}

	// A depth below 1 is 1.
	r, _ = g.Walk("a.py", 0, DirOut, false)
	impEqual(t, "depth 0", ids(r), []string{"a.py", "b.py", "c.py"})

	// The whole graph.
	r, _ = g.Walk("", 0, DirOut, false)
	if len(r.Nodes) != 6 || len(r.Edges) != 6 {
		t.Errorf("whole graph: %d nodes, %d edges", len(r.Nodes), len(r.Edges))
	}

	if _, err := g.Walk("missing.py", 1, DirOut, false); !errors.Is(err, ErrNoSuchNode) {
		t.Errorf("err = %v", err)
	}

	var buf bytes.Buffer
	r, _ = g.Walk("a.py", 3, DirOut, false)
	RenderWalkTree(&buf, r)
	want := "a.py\n  b.py\n    d.py\n      e.py\n  c.py\n    d.py (seen)\n"
	if buf.String() != want {
		t.Errorf("tree:\n%s\nwant:\n%s", buf.String(), want)
	}
	buf.Reset()
	r, _ = g.Walk("b.py", 1, DirBoth, false)
	RenderWalkTree(&buf, r)
	if !strings.Contains(buf.String(), "  imports:\n    d.py\n  imported by:\n    a.py\n") {
		t.Errorf("both tree:\n%s", buf.String())
	}
}

func TestGraphWalkDirectoryRoot(t *testing.T) {
	files, ws := impFiles(map[string]string{
		"pkg/a.py": "import pkg.b\n", "pkg/b.py": "\n", "other.py": "import pkg.a\n",
	}, nil)
	g := BuildGraph(files, ws)
	r, err := g.Walk("pkg", 1, DirOut, false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, n := range r.Nodes {
		ids = append(ids, n.ID)
	}
	impEqual(t, "directory root", ids, []string{"pkg/a.py", "pkg/b.py"})
}

func TestGraphDeterministic(t *testing.T) {
	build := func() (*Graph, WalkResult, map[string]float64) {
		files, ws := impGoWorkspace()
		g := BuildGraph(files, ws)
		r, _ := g.Walk("", 0, DirOut, true)
		return g, r, g.PageRank()
	}
	_, r1, p1 := build()
	_, r2, p2 := build()
	if !reflect.DeepEqual(r1, r2) || !reflect.DeepEqual(p1, p2) {
		t.Error("two builds of the same input differ")
	}
}

func TestGraphPageRank(t *testing.T) {
	files, ws := impFiles(map[string]string{
		"hub.py":  "\n",
		"a.py":    "import hub\n",
		"b.py":    "import hub\n",
		"c.py":    "import hub\nimport a\n",
		"d.py":    "import hub\n",
		"lone.py": "\n",
	}, nil)
	g := BuildGraph(files, ws)
	pr := g.PageRank()
	sum := 0.0
	best, bestScore := "", 0.0
	for f, s := range pr {
		sum += s
		if s > bestScore {
			best, bestScore = f, s
		}
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("sum = %v", sum)
	}
	if best != "hub.py" {
		t.Errorf("highest = %s (%v), want hub.py; all: %v", best, bestScore, pr)
	}
	if pr["a.py"] <= pr["b.py"] {
		t.Errorf("a.py (imported by c) should outrank b.py: %v vs %v", pr["a.py"], pr["b.py"])
	}

	// A Go package's score is spread over its non-test files.
	gfiles, gws := impGoWorkspace()
	gpr := BuildGraph(gfiles, gws).PageRank()
	if gpr["internal/core/core.go"] != gpr["internal/core/more.go"] {
		t.Errorf("package score not spread evenly: %v", gpr)
	}
	if _, ok := gpr["internal/core/core_test.go"]; ok {
		t.Error("a test file received part of a package's score")
	}
	if gpr["internal/util/util.go"] <= gpr["cmd/app/main.go"] {
		t.Errorf("util (imported by 3 files) should outrank main: %v", gpr)
	}
	sum = 0
	for _, s := range gpr {
		sum += s
	}
	if math.Abs(sum-1) > 1e-9 {
		t.Errorf("go sum = %v", sum)
	}
}

func TestGraphCoverage(t *testing.T) {
	files, ws := impFiles(map[string]string{
		"a.go":  "package a\n",
		"b.zig": "const std = @import(\"std\");\n",
		"c.zig": "\n",
		"d.lua": "require('x')\n",
	}, map[string]string{"go.mod": "module m\n"})
	g := BuildGraph(files, ws)
	got := g.Coverage()
	want := []LangCoverage{
		{Language: "go", Files: 1, Covered: true},
		{Language: "lua", Files: 1, Covered: true},
		{Language: "zig", Files: 2, Covered: false},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("coverage = %+v, want %+v", got, want)
	}

	// An uncovered file is "not read", not "imports nothing".
	imp := g.ImportsOf("b.zig")
	if imp.Covered || len(imp.Imports) != 0 {
		t.Errorf("zig imports = %+v", imp)
	}
	res, err := g.ImportersOf("b.zig")
	if err != nil || res.Covered {
		t.Errorf("importers of an uncovered file must not claim coverage: %+v, %v", res, err)
	}
	if _, ok := g.PageRank()["b.zig"]; ok {
		t.Error("an uncovered file must not be ranked by the import graph")
	}

	// A Go-only workspace reports no python at all.
	goOnly, gows := impFiles(map[string]string{"a.go": "package a\n"}, nil)
	for _, c := range BuildGraph(goOnly, gows).Coverage() {
		if c.Language == "python" {
			t.Errorf("python listed for a Go-only workspace: %+v", c)
		}
	}
}

func TestGraphImportsOfUnknownFile(t *testing.T) {
	g := BuildGraph(nil, newImpWS(nil))
	r := g.ImportsOf("ghost.go")
	if r.Covered || r.Imports == nil || len(r.Imports) != 0 {
		t.Errorf("r = %+v", r)
	}
}
