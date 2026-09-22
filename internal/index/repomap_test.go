package index

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func mapFile(path, lang string, syms ...Symbol) *File {
	for i := range syms {
		if syms[i].ID == "" {
			syms[i].ID = path + "::" + syms[i].Name + "#" + syms[i].Kind
		}
		if syms[i].Qualified == "" {
			syms[i].Qualified = syms[i].Name
		}
	}
	return &File{Path: path, Language: lang, HasOutline: true, Symbols: syms}
}

func msym(name, kind string, line int, sig string) Symbol {
	return Symbol{Name: name, Kind: kind, Line: line, EndLine: line + 1, Signature: sig}
}

func mapPaths(r RepoMapResult) []string {
	var out []string
	for _, f := range r.Files {
		out = append(out, f.Path)
	}
	return out
}

func TestRepoMapRankOrderAndFallback(t *testing.T) {
	files := []*File{
		mapFile("a.go", "go", msym("A", "function", 1, "func A()")),
		mapFile("b.go", "go", msym("B", "function", 1, "func B()"), msym("B2", "function", 5, "func B2()")),
		mapFile("c.go", "go", msym("C", "function", 1, "func C()")),
		{Path: "d.go", Language: "go"}, // no outline: not in the map
		mapFile("e.go", "go"),          // no symbols: not in the map
	}
	r, err := RepoMap(files, map[string]float64{"c.go": 0.5, "a.go": 0.2}, RepoMapOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := mapPaths(r); !reflect.DeepEqual(got, []string{"c.go", "a.go", "b.go"}) || !r.Ranked {
		t.Errorf("ranked order = %v ranked=%v", got, r.Ranked)
	}
	if r.FilesTotal != 3 || r.FilesListed != 3 || r.Truncated {
		t.Errorf("totals: %+v", r)
	}
	if r.Files[0].Rank != 0.5 {
		t.Errorf("rank not carried: %+v", r.Files[0])
	}

	r, _ = RepoMap(files, nil, RepoMapOptions{})
	if got := mapPaths(r); !reflect.DeepEqual(got, []string{"b.go", "a.go", "c.go"}) || r.Ranked {
		t.Errorf("unranked fallback (symbol count, then path) = %v ranked=%v", got, r.Ranked)
	}
}

func TestRepoMapBudgetIsARule(t *testing.T) {
	var files []*File
	rank := map[string]float64{}
	for i := 0; i < 80; i++ {
		p := fmt.Sprintf("pkg/file%02d.go", i)
		var syms []Symbol
		for j := 0; j < 12; j++ {
			syms = append(syms, msym(fmt.Sprintf("Func%02d_%02d", i, j), "function", j*4+1,
				fmt.Sprintf("func Func%02d_%02d(ctx context.Context, arg%d int) error", i, j, j)))
		}
		files = append(files, mapFile(p, "go", syms...))
		rank[p] = float64(100 - i)
	}
	for _, budget := range []int{10, 50, 200, 500, 1500, 100000} {
		r, err := RepoMap(files, rank, RepoMapOptions{BudgetTokens: budget})
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		RenderRepoMapText(&buf, r)
		body := buf.String()
		if i := strings.Index(body, "# listed"); i >= 0 {
			body = body[:i]
		}
		if EstimateTokens(body) > budget {
			t.Errorf("budget %d: rendered body is %d tokens", budget, EstimateTokens(body))
		}
		if r.TokensEstimated != EstimateTokens(body) {
			t.Errorf("budget %d: TokensEstimated %d, body %d", budget, r.TokensEstimated, EstimateTokens(body))
		}
		if r.Budget != budget {
			t.Errorf("budget echo %d", r.Budget)
		}
		if got := r.FilesListed < r.FilesTotal; got != r.Truncated && budget != 100000 {
			// a cut inside the last file also truncates
			if !r.Truncated {
				t.Errorf("budget %d: %d of %d files but Truncated=false", budget, r.FilesListed, r.FilesTotal)
			}
		}
		// Rank order is kept: the listed files are a prefix of the ranking.
		for i, f := range r.Files {
			if f.Path != fmt.Sprintf("pkg/file%02d.go", i) {
				t.Errorf("budget %d: file %d is %s", budget, i, f.Path)
			}
		}
	}
	r, _ := RepoMap(files, rank, RepoMapOptions{BudgetTokens: 100000})
	if r.Truncated || r.FilesListed != 80 {
		t.Errorf("a big budget lists everything: %+v", r.Truncated)
	}
	// 10 tokens = 40 bytes: not even one header line plus a symbol.
	r, _ = RepoMap(files, rank, RepoMapOptions{BudgetTokens: 3})
	if len(r.Files) != 0 || !r.Truncated {
		t.Errorf("a budget too small for one header returns no files: %+v", r)
	}
	if r.Files == nil {
		t.Error("Files must be an empty list, not null, in JSON")
	}
}

func TestRepoMapPerFileTiers(t *testing.T) {
	var files []*File
	rank := map[string]float64{}
	for i := 0; i < 25; i++ {
		p := fmt.Sprintf("f%02d.go", i)
		var syms []Symbol
		for j := 0; j < 10; j++ {
			syms = append(syms, msym(fmt.Sprintf("Sym%d", j), "function", j+1, "func()"))
		}
		files = append(files, mapFile(p, "go", syms...))
		rank[p] = float64(100 - i)
	}
	r, _ := RepoMap(files, rank, RepoMapOptions{BudgetTokens: 100000})
	for i, f := range r.Files {
		want := 8
		if i >= repoMapTopFiles {
			want = repoMapTailSymbols
		}
		if len(f.Symbols) != want || f.SymbolTotal != 10 {
			t.Errorf("file %d lists %d of %d symbols, want %d of 10", i, len(f.Symbols), f.SymbolTotal, want)
		}
	}
	r, _ = RepoMap(files, rank, RepoMapOptions{BudgetTokens: 100000, PerFile: 2})
	if len(r.Files[0].Symbols) != 2 || len(r.Files[24].Symbols) != 2 {
		t.Errorf("PerFile 2: %d and %d", len(r.Files[0].Symbols), len(r.Files[24].Symbols))
	}
}

func TestRepoMapSymbolChoiceAndText(t *testing.T) {
	typ := msym("Server", "struct", 3, "")
	m1 := msym("Handle", "method", 8, "func (s *Server) Handle(req Request) Response")
	m1.Qualified, m1.Parent = "Server.Handle", 2
	m2 := msym("stop", "method", 12, "func (s *Server) stop()")
	m2.Qualified, m2.Parent = "Server.stop", 2
	field := msym("addr", "field", 4, "string")
	field.Qualified, field.Parent = "Server.addr", 2
	local := msym("tmp", "variable", 9, "int")
	local.Qualified, local.Parent = "Handle.tmp", 4 // depth 2: never listed
	f := mapFile("srv.go", "go",
		msym("helper", "function", 1, "func helper()"),
		typ, field, m1, local, m2,
		msym("New", "function", 20, "func New(addr string) *Server"),
		msym("Run\nmulti", "function", 30, "func Run(\n\tctx context.Context,\n) error"))
	r, _ := RepoMap([]*File{f}, nil, RepoMapOptions{PerFile: 4})
	var texts []string
	for _, s := range r.Files[0].Symbols {
		texts = append(texts, s.Text)
	}
	// exported first, type before function before method; shown in document order.
	want := []string{
		"struct Server",
		"method Handle  func (s *Server) Handle(req Request) Response",
		"function New  func New(addr string) *Server",
		"function Run multi  func Run( ctx context.Context, ) error",
	}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("symbol lines:\n%q\nwant\n%q", texts, want)
	}
	for _, s := range r.Files[0].Symbols {
		if strings.ContainsAny(s.Text, "\n\r") {
			t.Errorf("newline in %q", s.Text)
		}
	}
}

func TestRepoMapLongSignatureClipped(t *testing.T) {
	long := msym("Big", "function", 1, "func Big("+strings.Repeat("argument int, ", 40)+") error")
	r, _ := RepoMap([]*File{mapFile("a.go", "go", long)}, nil, RepoMapOptions{})
	if got := r.Files[0].Symbols[0].Text; len(got) > repoMapTextBytes || !strings.HasSuffix(got, "...") {
		t.Errorf("len %d: %q", len(got), got)
	}
	multi := msym("N", "function", 1, strings.Repeat("名", 100))
	r, _ = RepoMap([]*File{mapFile("a.go", "go", multi)}, nil, RepoMapOptions{})
	if got := r.Files[0].Symbols[0].Text; len(got) > repoMapTextBytes || !strings.HasSuffix(got, "...") {
		t.Errorf("multibyte clip: len %d %q", len(got), got)
	}
}

func TestRepoMapScopeFilters(t *testing.T) {
	files := []*File{
		mapFile("internal/a.go", "go", msym("A", "function", 1, "")),
		mapFile("internal/gen/g.go", "go", msym("G", "function", 1, "")),
		mapFile("web/app.ts", "typescript", msym("app", "function", 1, "")),
		mapFile("cmd/main.go", "go", msym("main", "function", 1, "")),
	}
	paths := func(o RepoMapOptions) []string {
		r, err := RepoMap(files, nil, o)
		if err != nil {
			t.Fatal(err)
		}
		return mapPaths(r)
	}
	if got := paths(RepoMapOptions{Path: "internal"}); !reflect.DeepEqual(got, []string{"internal/a.go", "internal/gen/g.go"}) {
		t.Errorf("path: %v", got)
	}
	if got := paths(RepoMapOptions{Languages: []string{"TypeScript"}}); !reflect.DeepEqual(got, []string{"web/app.ts"}) {
		t.Errorf("language: %v", got)
	}
	if got := paths(RepoMapOptions{Globs: []string{"*.go", "!gen"}}); !reflect.DeepEqual(got, []string{"cmd/main.go", "internal/a.go"}) {
		t.Errorf("globs: %v", got)
	}
	if _, err := RepoMap(files, nil, RepoMapOptions{Globs: []string{"["}}); err == nil {
		t.Error("a bad glob was accepted")
	}
	r, _ := RepoMap(files, nil, RepoMapOptions{Path: "internal"})
	if r.FilesTotal != 2 {
		t.Errorf("FilesTotal counts the scope, not the repository: %d", r.FilesTotal)
	}
}

func TestRepoMapRenderText(t *testing.T) {
	files := []*File{
		mapFile("a.go", "go", msym("A", "function", 1, "func A()")),
		mapFile("b.go", "go", msym("B", "function", 1, "func B()")),
	}
	r, _ := RepoMap(files, map[string]float64{"a.go": 1}, RepoMapOptions{})
	var buf bytes.Buffer
	RenderRepoMapText(&buf, r)
	if want := "a.go:\n  function A  func A()\nb.go:\n  function B  func B()\n"; buf.String() != want {
		t.Errorf("text:\n%s", buf.String())
	}

	// A budget of exactly the first file: 4 tokens = 16 bytes; "a.go:\n" is 6,
	// "  function A  func A()\n" is 24 — the header fits and the symbol does not.
	r, _ = RepoMap(files, map[string]float64{"a.go": 1}, RepoMapOptions{BudgetTokens: 4})
	buf.Reset()
	RenderRepoMapText(&buf, r)
	if !strings.HasPrefix(buf.String(), "a.go:\n# listed 1 of 2 files within a budget of 4 tokens") || !r.Truncated {
		t.Errorf("truncated text: %q", buf.String())
	}
}

func TestEstimateTokens(t *testing.T) {
	for in, want := range map[string]int{"": 0, "a": 1, "abcd": 1, "abcde": 2, strings.Repeat("x", 400): 100} {
		if got := EstimateTokens(in); got != want {
			t.Errorf("EstimateTokens(%d bytes) = %d, want %d", len(in), got, want)
		}
	}
}

func TestRepoMapDeterministic(t *testing.T) {
	var files []*File
	for i := 0; i < 30; i++ {
		files = append(files, mapFile(fmt.Sprintf("p%02d.go", i), "go", msym("X", "function", 1, "func X()")))
	}
	render := func(fs []*File) string {
		r, _ := RepoMap(fs, nil, RepoMapOptions{})
		var b bytes.Buffer
		RenderRepoMapText(&b, r)
		return b.String()
	}
	want := render(files)
	rev := make([]*File, len(files))
	for i, f := range files {
		rev[len(files)-1-i] = f
	}
	if got := render(rev); got != want {
		t.Error("input order changed the map")
	}
}
