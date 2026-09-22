package index

import (
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// The import graph.
//
// Node model. A Go node is a PACKAGE, identified by its workspace-relative
// directory ("." for the root): Go imports name packages, never files, and a
// file-level graph would put an edge from each importing file to every file of
// the imported package and invent cycles between files of one package. Every
// other language's node is a FILE. A Go file maps to its directory's node.
//
// Edges out of `_test.go` files are left out of the graph (their imports are
// still listed by ImportsOf): an external-test package imports the package it
// tests, and what that package's dependencies import in turn can close a cycle
// that does not exist in the build.
//
// Only what the lexers and the resolver can see is in the graph. A language with
// no extractor contributes files with no edges, and [Graph.Coverage] says so, so
// that "nothing imports this" is never mistaken for "this language was not read".

// ErrNoSuchNode is returned for a target the graph does not contain.
var ErrNoSuchNode = errors.New("not in the import graph")

// Direction is which way a walk follows edges.
type Direction string

const (
	DirIn   Direction = "in"   // who imports the root
	DirOut  Direction = "out"  // what the root imports
	DirBoth Direction = "both" // both
)

// ParseDirection reads "in", "out" or "both".
func ParseDirection(s string) (Direction, error) {
	switch Direction(s) {
	case DirIn, DirOut, DirBoth:
		return Direction(s), nil
	case "":
		return DirOut, nil
	}
	return "", fmt.Errorf("direction %q: want in, out or both", s)
}

// Node kinds in a [WalkResult].
const (
	NodeFile     = "file"
	NodePackage  = "package"
	NodeExternal = "external"
)

// maxWalkNodes bounds one walk; past it the result says it is truncated.
const maxWalkNodes = 5000

// A ResolvedImport is one import of a file with its resolution.
type ResolvedImport struct {
	Spec  string   `json:"spec"`
	Names []string `json:"names,omitempty"`
	Line  int      `json:"line"`
	Kind  string   `json:"kind,omitempty"`
	// Targets are node ids (package directories for Go, files otherwise).
	Targets  []string `json:"targets,omitempty"`
	External bool     `json:"external,omitempty"`
	Category string   `json:"category"`
}

// ImportsResult is what a file imports.
type ImportsResult struct {
	File     string `json:"file"`
	Language string `json:"language,omitempty"`
	// Covered is false when the file's language has no extractor (or the file
	// was never scanned): Imports is then unknown, not empty.
	Covered bool `json:"covered"`
	// Test marks a Go test file, whose imports are not graph edges.
	Test    bool             `json:"test,omitempty"`
	Imports []ResolvedImport `json:"imports"`
}

// An Importer is one place that imports a target.
type Importer struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Spec string `json:"spec"`
	// Test marks a test file. Only the test-only importers of a result carry
	// it: a Go `_test.go` file's imports are not edges of the graph (see the
	// top of this file), so they are reported beside the importers, never in
	// them, unless the caller merges them.
	Test bool `json:"test,omitempty"`
}

// ImportersResult is who imports a target.
type ImportersResult struct {
	// Target is the node the query resolved to.
	Target string `json:"target"`
	// Covered is false when the target's language has no extractor, so that an
	// empty importer list cannot be trusted.
	Covered   bool       `json:"covered"`
	Importers []Importer `json:"importers"`
	// TestOnly are the Go test files that import the target from a package whose
	// non-test files do not: the edges the graph leaves out, and the only ones
	// through which that package depends on the target. A test file whose
	// package also imports the target from non-test code, and a package's own
	// tests importing the package, are not listed: they change nothing the
	// importers do not already say.
	TestOnly []Importer `json:"test_only_importers,omitempty"`
}

// A Node is one node of a [WalkResult].
type Node struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Depth int    `json:"depth"`
}

// An Edge is one dependency: From imports To.
type Edge struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Spec     string `json:"spec,omitempty"`
	External bool   `json:"external,omitempty"`
}

// A WalkResult is a piece of the graph.
type WalkResult struct {
	Root      string    `json:"root,omitempty"`
	Depth     int       `json:"depth"`
	Direction Direction `json:"direction"`
	Nodes     []Node    `json:"nodes"`
	Edges     []Edge    `json:"edges"`
	// Cycles are the import cycles that touch the walk, each starting at its
	// smallest node.
	Cycles    [][]string `json:"cycles"`
	Truncated bool       `json:"truncated"`
}

// LangCoverage says whether a language present in the workspace has an import
// extractor.
type LangCoverage struct {
	Language string `json:"language"`
	Files    int    `json:"files"`
	Covered  bool   `json:"covered"`
}

type gEdge struct {
	from, to string
	file     string
	line     int
	spec     string
}

type extEdge struct {
	spec, category string
}

// A Graph is the resolved import graph of a set of files.
type Graph struct {
	files    []*File
	byPath   map[string]*File
	res      *Resolver
	nodeKind map[string]string
	members  map[string][]string
	out      map[string][]gEdge
	in       map[string][]gEdge
	// testIn are the edges out of Go test files, which the graph proper leaves
	// out; they exist so that importers can say what they left out.
	testIn   map[string][]gEdge
	succ     map[string][]string
	pred     map[string][]string
	ext      map[string][]extEdge
	resolved map[string][]ResolvedImport
}

func isGoTest(f *File) bool { return f.Language == "go" && strings.HasSuffix(f.Path, "_test.go") }

func nodeOfFile(f *File) string {
	if f.Language == "go" {
		return path.Dir(f.Path)
	}
	return f.Path
}

// BuildGraph resolves every scanned file's imports against ws. files need not
// be sorted; the graph never depends on their order.
func BuildGraph(files []*File, ws Workspace) *Graph {
	g := &Graph{
		files:    append([]*File(nil), files...),
		byPath:   map[string]*File{},
		res:      NewResolver(ws),
		nodeKind: map[string]string{},
		members:  map[string][]string{},
		out:      map[string][]gEdge{},
		in:       map[string][]gEdge{},
		testIn:   map[string][]gEdge{},
		succ:     map[string][]string{},
		pred:     map[string][]string{},
		ext:      map[string][]extEdge{},
		resolved: map[string][]ResolvedImport{},
	}
	sort.Slice(g.files, func(i, j int) bool { return g.files[i].Path < g.files[j].Path })
	for _, f := range g.files {
		g.byPath[f.Path] = f
		n := nodeOfFile(f)
		if f.Language == "go" {
			g.nodeKind[n] = NodePackage
		} else if g.nodeKind[n] == "" {
			g.nodeKind[n] = NodeFile
		}
		g.members[n] = append(g.members[n], f.Path)
	}

	for _, f := range g.files {
		if !f.HasImports {
			continue
		}
		from := nodeOfFile(f)
		list := make([]ResolvedImport, 0, len(f.Imports))
		for _, ref := range f.Imports {
			r := g.res.Resolve(f, ref)
			list = append(list, ResolvedImport{
				Spec: ref.Spec, Names: ref.Names, Line: ref.Line, Kind: ref.Kind,
				Targets: r.Targets, External: r.External, Category: r.Category,
			})
		}
		g.resolved[f.Path] = list
		if isGoTest(f) {
			for _, ri := range list {
				for _, t := range ri.Targets {
					g.testIn[t] = append(g.testIn[t], gEdge{from: from, to: t, file: f.Path, line: ri.Line, spec: ri.Spec})
				}
			}
			continue
		}
		for _, ri := range list {
			if ri.External {
				g.ext[from] = append(g.ext[from], extEdge{spec: ri.Spec, category: ri.Category})
			}
			for _, t := range ri.Targets {
				if g.nodeKind[t] == "" {
					if f.Language == "go" {
						g.nodeKind[t] = NodePackage
					} else {
						g.nodeKind[t] = NodeFile
					}
				}
				e := gEdge{from: from, to: t, file: f.Path, line: ri.Line, spec: ri.Spec}
				g.out[from] = append(g.out[from], e)
				g.in[t] = append(g.in[t], e)
			}
		}
	}

	less := func(a, b gEdge) bool {
		switch {
		case a.from != b.from:
			return a.from < b.from
		case a.to != b.to:
			return a.to < b.to
		case a.file != b.file:
			return a.file < b.file
		case a.line != b.line:
			return a.line < b.line
		}
		return a.spec < b.spec
	}
	for n, es := range g.out {
		sort.Slice(es, func(i, j int) bool { return less(es[i], es[j]) })
		g.out[n] = es
		for _, e := range es {
			if s := g.succ[n]; len(s) == 0 || s[len(s)-1] != e.to {
				g.succ[n] = append(g.succ[n], e.to)
			}
		}
	}
	for n, es := range g.in {
		sort.Slice(es, func(i, j int) bool { return less(es[i], es[j]) })
		g.in[n] = es
		for _, e := range es {
			if p := g.pred[n]; len(p) == 0 || p[len(p)-1] != e.from {
				g.pred[n] = append(g.pred[n], e.from)
			}
		}
	}
	for n := range g.ext {
		es := g.ext[n]
		sort.Slice(es, func(i, j int) bool { return es[i].spec < es[j].spec })
		g.ext[n] = dedupeExt(es)
	}
	for n := range g.members {
		sort.Strings(g.members[n])
	}
	return g
}

func dedupeExt(es []extEdge) []extEdge {
	out := es[:0]
	for i, e := range es {
		if i == 0 || e != es[i-1] {
			out = append(out, e)
		}
	}
	return out
}

func cleanTarget(target string) string {
	t := filepath.ToSlash(strings.TrimSpace(target))
	if t == "" {
		return ""
	}
	return strings.TrimSuffix(path.Clean(t), "/")
}

// NodeOf resolves what a user typed — a file, a package directory, or the
// import path of a workspace Go module's package — to a node id.
func (g *Graph) NodeOf(target string) (string, bool) {
	t := cleanTarget(target)
	if t == "" {
		return "", false
	}
	if f, ok := g.byPath[t]; ok {
		return nodeOfFile(f), true
	}
	if _, ok := g.nodeKind[t]; ok {
		return t, true
	}
	if dir, ok := g.res.goPackageDir(strings.TrimSpace(target)); ok {
		if _, known := g.nodeKind[dir]; known || len(g.res.goDirs[dir]) > 0 {
			return dir, true
		}
	}
	return "", false
}

// ImportsOf lists what one file imports, resolved.
func (g *Graph) ImportsOf(p string) ImportsResult {
	p = cleanTarget(p)
	res := ImportsResult{File: p, Imports: []ResolvedImport{}}
	f := g.byPath[p]
	if f == nil {
		return res
	}
	res.Language = f.Language
	res.Covered = f.HasImports
	res.Test = isGoTest(f)
	res.Imports = append(res.Imports, g.resolved[p]...)
	return res
}

// ImportersOf lists the files that import a file or package.
func (g *Graph) ImportersOf(target string) (ImportersResult, error) {
	node, ok := g.NodeOf(target)
	if !ok {
		return ImportersResult{}, fmt.Errorf("%s: %w", target, ErrNoSuchNode)
	}
	res := ImportersResult{Target: node, Importers: []Importer{}}
	seen := map[Importer]bool{}
	for _, e := range g.in[node] {
		imp := Importer{File: e.file, Line: e.line, Spec: e.spec}
		if !seen[imp] {
			seen[imp] = true
			res.Importers = append(res.Importers, imp)
		}
	}
	byLocation := func(list []Importer) {
		sort.Slice(list, func(i, j int) bool {
			a, b := list[i], list[j]
			if a.File != b.File {
				return a.File < b.File
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.Spec < b.Spec
		})
	}
	byLocation(res.Importers)
	prod := map[string]bool{}
	for _, e := range g.in[node] {
		prod[e.from] = true
	}
	seenTest := map[Importer]bool{}
	for _, e := range g.testIn[node] {
		if e.from == node || prod[e.from] {
			continue
		}
		imp := Importer{File: e.file, Line: e.line, Spec: e.spec, Test: true}
		if !seenTest[imp] {
			seenTest[imp] = true
			res.TestOnly = append(res.TestOnly, imp)
		}
	}
	byLocation(res.TestOnly)
	if m := g.members[node]; len(m) > 0 {
		res.Covered = ExtractorFor(g.byPath[m[0]].Language) != nil
	}
	if len(res.Importers) > 0 {
		res.Covered = true
	}
	return res, nil
}

// roots resolves a walk's root: a node, or a directory holding several.
func (g *Graph) roots(root string) ([]string, error) {
	if n, ok := g.NodeOf(root); ok {
		return []string{n}, nil
	}
	t := cleanTarget(root)
	var out []string
	if t != "" {
		prefix := t + "/"
		if t == "." {
			prefix = ""
		}
		for n := range g.nodeKind {
			if prefix == "" || strings.HasPrefix(n, prefix) {
				out = append(out, n)
			}
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: %w", root, ErrNoSuchNode)
	}
	sort.Strings(out)
	return out, nil
}

func (g *Graph) kindOf(id string) string {
	if k := g.nodeKind[id]; k != "" {
		return k
	}
	return NodeFile
}

// Walk returns the part of the graph within depth edges of root, following
// edges in direction dir. An empty root is the whole graph (depth does not
// apply). A depth below 1 is 1. External imports are edges to `external` nodes
// only when includeExternal is set.
func (g *Graph) Walk(root string, depth int, dir Direction, includeExternal bool) (WalkResult, error) {
	if dir == "" {
		dir = DirOut
	}
	if depth < 1 {
		depth = 1
	}
	res := WalkResult{Root: root, Direction: dir, Nodes: []Node{}, Edges: []Edge{}, Cycles: [][]string{}}

	type key struct{ from, to string }
	edges := map[key]Edge{}
	record := func(e Edge) {
		k := key{e.From, e.To}
		if old, ok := edges[k]; !ok || e.Spec < old.Spec {
			edges[k] = e
		}
	}
	depthOf := map[string]int{}
	kinds := map[string]string{}

	if root == "" {
		var ids []string
		for n := range g.nodeKind {
			ids = append(ids, n)
		}
		sort.Strings(ids)
		if len(ids) > maxWalkNodes {
			ids, res.Truncated = ids[:maxWalkNodes], true
		}
		keep := map[string]bool{}
		for _, n := range ids {
			keep[n] = true
			depthOf[n] = 0
			kinds[n] = g.nodeKind[n]
		}
		for from, es := range g.out {
			for _, e := range es {
				if keep[from] && keep[e.to] {
					record(Edge{From: from, To: e.to, Spec: e.spec})
				}
			}
		}
		if includeExternal {
			for from, xs := range g.ext {
				if !keep[from] {
					continue
				}
				for _, x := range xs {
					record(Edge{From: from, To: x.spec, Spec: x.spec, External: true})
					kinds[x.spec] = NodeExternal
					depthOf[x.spec] = 0
				}
			}
		}
		res.Depth = 0
	} else {
		roots, err := g.roots(root)
		if err != nil {
			return res, err
		}
		res.Depth = depth
		type item struct {
			id string
			d  int
		}
		var queue []item
		visit := func(id string, d int, kind string) bool {
			if _, ok := depthOf[id]; ok {
				return false
			}
			if len(depthOf) >= maxWalkNodes {
				res.Truncated = true
				return false
			}
			depthOf[id] = d
			kinds[id] = kind
			return kind != NodeExternal
		}
		for _, r := range roots {
			if visit(r, 0, g.kindOf(r)) {
				queue = append(queue, item{r, 0})
			}
		}
		for len(queue) > 0 {
			it := queue[0]
			queue = queue[1:]
			if it.d >= depth {
				continue
			}
			if dir == DirOut || dir == DirBoth {
				for _, e := range g.out[it.id] {
					if _, known := depthOf[e.to]; !known && len(depthOf) >= maxWalkNodes {
						res.Truncated = true
						continue
					}
					record(Edge{From: it.id, To: e.to, Spec: e.spec})
					if visit(e.to, it.d+1, g.kindOf(e.to)) {
						queue = append(queue, item{e.to, it.d + 1})
					}
				}
				if includeExternal {
					for _, x := range g.ext[it.id] {
						if _, known := depthOf[x.spec]; !known && len(depthOf) >= maxWalkNodes {
							res.Truncated = true
							continue
						}
						record(Edge{From: it.id, To: x.spec, Spec: x.spec, External: true})
						visit(x.spec, it.d+1, NodeExternal)
					}
				}
			}
			if dir == DirIn || dir == DirBoth {
				for _, e := range g.in[it.id] {
					if _, known := depthOf[e.from]; !known && len(depthOf) >= maxWalkNodes {
						res.Truncated = true
						continue
					}
					record(Edge{From: e.from, To: it.id, Spec: e.spec})
					if visit(e.from, it.d+1, g.kindOf(e.from)) {
						queue = append(queue, item{e.from, it.d + 1})
					}
				}
			}
		}
	}

	for id, d := range depthOf {
		res.Nodes = append(res.Nodes, Node{ID: id, Kind: kinds[id], Depth: d})
	}
	sort.Slice(res.Nodes, func(i, j int) bool {
		a, b := res.Nodes[i], res.Nodes[j]
		if a.Depth != b.Depth {
			return a.Depth < b.Depth
		}
		return a.ID < b.ID
	})
	for _, e := range edges {
		res.Edges = append(res.Edges, e)
	}
	sort.Slice(res.Edges, func(i, j int) bool {
		a, b := res.Edges[i], res.Edges[j]
		if a.From != b.From {
			return a.From < b.From
		}
		if a.To != b.To {
			return a.To < b.To
		}
		return a.Spec < b.Spec
	})
	for _, c := range g.Cycles() {
		for _, n := range c {
			if _, ok := depthOf[n]; ok {
				res.Cycles = append(res.Cycles, c)
				break
			}
		}
	}
	return res, nil
}

// RenderWalkTree prints a walk as an indented tree. A node that is already on
// the path above it is marked "(cycle)" and not expanded; one printed elsewhere
// is marked "(seen)". The whole graph is printed as an adjacency list.
func RenderWalkTree(w io.Writer, r WalkResult) {
	out := map[string][]string{}
	in := map[string][]string{}
	kind := map[string]string{}
	for _, n := range r.Nodes {
		kind[n.ID] = n.Kind
	}
	for _, e := range r.Edges {
		out[e.From] = append(out[e.From], e.To)
		in[e.To] = append(in[e.To], e.From)
	}
	label := func(id string) string {
		if kind[id] == NodeExternal {
			return id + " (external)"
		}
		return id
	}

	var walk func(id string, adj map[string][]string, depth int, onPath map[string]bool, seen map[string]bool)
	walk = func(id string, adj map[string][]string, depth int, onPath map[string]bool, seen map[string]bool) {
		onPath[id] = true
		for _, next := range adj[id] {
			indent := strings.Repeat("  ", depth)
			switch {
			case onPath[next]:
				fmt.Fprintf(w, "%s%s (cycle)\n", indent, label(next))
			case seen[next]:
				fmt.Fprintf(w, "%s%s (seen)\n", indent, label(next))
			default:
				seen[next] = true
				fmt.Fprintf(w, "%s%s\n", indent, label(next))
				walk(next, adj, depth+1, onPath, seen)
			}
		}
		delete(onPath, id)
	}

	if r.Root == "" {
		for _, n := range r.Nodes {
			if len(out[n.ID]) == 0 {
				continue
			}
			fmt.Fprintln(w, n.ID)
			for _, to := range out[n.ID] {
				fmt.Fprintf(w, "  %s\n", label(to))
			}
		}
	} else {
		var roots []string
		for _, n := range r.Nodes {
			if n.Depth == 0 {
				roots = append(roots, n.ID)
			}
		}
		for _, root := range roots {
			fmt.Fprintln(w, root)
			switch r.Direction {
			case DirBoth:
				fmt.Fprintln(w, "  imports:")
				walk(root, out, 2, map[string]bool{}, map[string]bool{root: true})
				fmt.Fprintln(w, "  imported by:")
				walk(root, in, 2, map[string]bool{}, map[string]bool{root: true})
			case DirIn:
				walk(root, in, 1, map[string]bool{}, map[string]bool{root: true})
			default:
				walk(root, out, 1, map[string]bool{}, map[string]bool{root: true})
			}
		}
	}
	if len(r.Cycles) > 0 {
		fmt.Fprintln(w, "cycles:")
		for _, c := range r.Cycles {
			fmt.Fprintf(w, "  %s -> %s\n", strings.Join(c, " -> "), c[0])
		}
	}
	if r.Truncated {
		fmt.Fprintln(w, "# truncated")
	}
}

// Cycles are the strongly connected components of the graph that hold a cycle:
// more than one node, or one node that imports itself. Each is listed in
// breadth-first order from its smallest node (which is the cycle's own order
// for a simple cycle), and the list is sorted.
func (g *Graph) Cycles() [][]string {
	var ids []string
	for n := range g.nodeKind {
		ids = append(ids, n)
	}
	sort.Strings(ids)

	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	next := 0
	var comps [][]string

	var strong func(v string)
	strong = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range g.succ[v] {
			if _, seen := index[w]; !seen {
				strong(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] == index[v] {
			var comp []string
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				comp = append(comp, w)
				if w == v {
					break
				}
			}
			comps = append(comps, comp)
		}
	}
	for _, v := range ids {
		if _, seen := index[v]; !seen {
			strong(v)
		}
	}

	out := [][]string{}
	for _, comp := range comps {
		if len(comp) == 1 {
			self := false
			for _, s := range g.succ[comp[0]] {
				self = self || s == comp[0]
			}
			if !self {
				continue
			}
			out = append(out, comp)
			continue
		}
		in := map[string]bool{}
		for _, n := range comp {
			in[n] = true
		}
		sort.Strings(comp)
		order := []string{comp[0]}
		seen := map[string]bool{comp[0]: true}
		for i := 0; i < len(order); i++ {
			for _, s := range g.succ[order[i]] {
				if in[s] && !seen[s] {
					seen[s] = true
					order = append(order, s)
				}
			}
		}
		out = append(out, order)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i][0] != out[j][0] {
			return out[i][0] < out[j][0]
		}
		return len(out[i]) < len(out[j])
	})
	return out
}

const (
	pageRankDamping = 0.85
	pageRankMaxIter = 100
	pageRankEpsilon = 1e-10
)

// PageRank scores every scanned file by the centrality of its node: the score of
// a Go package is spread evenly over its non-test files (over all of them when it
// has only tests), and the scores of the files present sum to 1. Dangling nodes
// spread their mass uniformly, the iteration is capped and the order fixed, so
// the same graph always gives the same numbers. Files of languages with no
// extractor are absent: they say nothing about the graph.
func (g *Graph) PageRank() map[string]float64 {
	inSet := map[string]bool{}
	for n := range g.out {
		inSet[n] = true
	}
	for n := range g.in {
		inSet[n] = true
	}
	for n, ms := range g.members {
		for _, m := range ms {
			if g.byPath[m].HasImports {
				inSet[n] = true
				break
			}
		}
	}
	nodes := make([]string, 0, len(inSet))
	for n := range inSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	res := map[string]float64{}
	n := len(nodes)
	if n == 0 {
		return res
	}
	idx := make(map[string]int, n)
	for i, id := range nodes {
		idx[id] = i
	}
	rank := make([]float64, n)
	for i := range rank {
		rank[i] = 1 / float64(n)
	}
	for iter := 0; iter < pageRankMaxIter; iter++ {
		next := make([]float64, n)
		dangling := 0.0
		for i, id := range nodes {
			s := g.succ[id]
			if len(s) == 0 {
				dangling += rank[i]
				continue
			}
			share := rank[i] / float64(len(s))
			for _, t := range s {
				next[idx[t]] += share
			}
		}
		delta := 0.0
		for i := range next {
			next[i] = (1-pageRankDamping)/float64(n) + pageRankDamping*(next[i]+dangling/float64(n))
			delta += abs(next[i] - rank[i])
		}
		rank = next
		if delta < pageRankEpsilon {
			break
		}
	}

	total := 0.0
	for i, id := range nodes {
		ms := g.members[id]
		if len(ms) == 0 {
			continue
		}
		var live []string
		for _, m := range ms {
			if !isGoTest(g.byPath[m]) {
				live = append(live, m)
			}
		}
		if len(live) == 0 {
			live = ms
		}
		for _, m := range live {
			res[m] = rank[i] / float64(len(live))
			total += res[m]
		}
	}
	if total > 0 {
		for m := range res {
			res[m] /= total
		}
	}
	return res
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// Coverage lists the languages present in the file set and whether each has an
// import extractor. A file of an uncovered language is in the graph as an
// isolated node; this is what says its edges are unknown.
func (g *Graph) Coverage() []LangCoverage {
	counts := map[string]int{}
	for _, f := range g.files {
		if f.Language != "" {
			counts[f.Language]++
		}
	}
	out := make([]LangCoverage, 0, len(counts))
	for l, n := range counts {
		out = append(out, LangCoverage{Language: l, Files: n, Covered: ExtractorFor(l) != nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out
}
