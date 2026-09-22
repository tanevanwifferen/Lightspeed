package index

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// sym builds a symbol whose id is derived from the file, name and kind.
func sym(path, name, kind string, line int) Symbol {
	return Symbol{
		ID: path + "::" + name + "#" + kind, Name: name, Qualified: name, Kind: kind,
		Line: line, EndLine: line + 2,
	}
}

func file(path, lang string, syms ...Symbol) *File {
	return &File{Path: path, Language: lang, HasOutline: true, Symbols: syms}
}

func names(r SearchResult) []string {
	var out []string
	for _, h := range r.Hits {
		out = append(out, h.Symbol.Name)
	}
	return out
}

func search(t *testing.T, files []*File, q SearchQuery) SearchResult {
	t.Helper()
	r, err := NewSearchIndex(files).Search(q)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSearchTierOrder: exact > prefix > token, whatever BM25 says. The token
// match is built to have a far larger BM25 score than the exact one (its name,
// container, signature and doc all repeat the query token).
func TestSearchTierOrder(t *testing.T) {
	loud := sym("a.go", "configLoader", "function", 1)
	loud.Qualified = "config.configLoader"
	loud.Container = "config"
	loud.Signature = "func configLoader(config config) config"
	loud.Doc = "config config config"
	files := []*File{
		file("a.go", "go", loud, sym("a.go", "config", "function", 10), sym("a.go", "configure", "function", 20)),
		file("b.go", "go", sym("b.go", "parseConfig", "function", 1), sym("b.go", "unrelated", "function", 5)),
	}
	r := search(t, files, SearchQuery{Text: "config"})
	want := []string{"config", "configLoader", "configure", "parseConfig"}
	if got := names(r); !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
	wantMatch := []string{MatchExact, MatchPrefix, MatchPrefix, MatchToken}
	// configLoader and configure are both prefix matches, ordered by score
	// inside the tier (the loud one first); parseConfig is the token match.
	for i, h := range r.Hits {
		if h.Match != wantMatch[i] {
			t.Errorf("hit %d (%s): match %q, want %q", i, h.Symbol.Name, h.Match, wantMatch[i])
		}
	}
	for i := 1; i < len(r.Hits); i++ {
		if r.Hits[i].Score > r.Hits[i-1].Score {
			t.Errorf("Score not monotone with the order: %v then %v", r.Hits[i-1].Score, r.Hits[i].Score)
		}
	}
}

func TestSearchAdversarialBM25CannotBeatExact(t *testing.T) {
	spam := sym("a.go", "helperThing", "function", 1)
	spam.Qualified = "handler.helperThing"
	spam.Signature = strings.Repeat("handler ", 30)
	spam.Doc = strings.Repeat("handler ", 30)
	files := []*File{file("a.go", "go", spam, sym("a.go", "Handler", "type", 40))}
	r := search(t, files, SearchQuery{Text: "handler"})
	if got := names(r); !reflect.DeepEqual(got, []string{"Handler", "helperThing"}) {
		t.Fatalf("got %v", got)
	}
	if r.Hits[0].Match != MatchExact || r.Hits[1].Match != MatchToken {
		t.Errorf("matches: %s, %s", r.Hits[0].Match, r.Hits[1].Match)
	}
}

func TestSearchCaseSensitiveExactFirst(t *testing.T) {
	files := []*File{file("a.go", "go",
		sym("a.go", "server", "variable", 1), sym("a.go", "Server", "type", 5), sym("a.go", "SERVER", "constant", 9))}
	r := search(t, files, SearchQuery{Text: "Server"})
	if len(r.Hits) != 3 || r.Hits[0].Symbol.Name != "Server" {
		t.Fatalf("hits: %v", names(r))
	}
	for _, h := range r.Hits {
		if h.Match != MatchExact {
			t.Errorf("%s is %s, want exact (case and separators are ignored)", h.Symbol.Name, h.Match)
		}
	}
}

func TestSearchCamelAndSnakeQueries(t *testing.T) {
	files := []*File{file("a.go", "go",
		sym("a.go", "ParseConfigFile", "function", 1),
		sym("a.go", "parse_config", "function", 5),
		sym("a.go", "configParser", "function", 9),
		sym("a.go", "Unrelated", "function", 13))}
	for _, q := range []string{"parseConfig", "parse_config", "parse config", "PARSE CONFIG"} {
		r := search(t, files, SearchQuery{Text: q})
		got := names(r)
		if len(got) < 2 {
			t.Errorf("%q: %v", q, got)
		}
		joined := strings.Join(got, ",")
		if !strings.Contains(joined, "ParseConfigFile") || !strings.Contains(joined, "parse_config") {
			t.Errorf("%q missed a name: %v", q, got)
		}
		if strings.Contains(joined, "Unrelated") {
			t.Errorf("%q matched an unrelated symbol: %v", q, got)
		}
	}
	// exact: the normalised query equals parse_config, and ParseConfigFile is a
	// prefix match, so exact comes first.
	r := search(t, files, SearchQuery{Text: "parseConfig"})
	if r.Hits[0].Symbol.Name != "parse_config" || r.Hits[0].Match != MatchExact {
		t.Errorf("first: %+v", r.Hits[0])
	}
	if r.Hits[1].Symbol.Name != "ParseConfigFile" || r.Hits[1].Match != MatchPrefix {
		t.Errorf("second: %+v", r.Hits[1])
	}
}

func TestSearchMultiTokenCoverage(t *testing.T) {
	files := []*File{file("a.go", "go",
		sym("a.go", "loadUserConfig", "function", 1),
		sym("a.go", "loadUser", "function", 5),
		sym("a.go", "saveConfig", "function", 9))}
	r := search(t, files, SearchQuery{Text: "user config"})
	got := names(r)
	if got[0] != "loadUserConfig" || len(got) != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestSearchQualifiedAndDocFields(t *testing.T) {
	m := sym("a.go", "Handle", "method", 3)
	m.Qualified, m.Container, m.Parent = "Server.Handle", "Server", 1
	d := sym("a.go", "helper", "function", 20)
	d.Doc = "Retries the upload until the quota resets."
	files := []*File{file("a.go", "go", sym("a.go", "Server", "struct", 1), m, d)}

	r := search(t, files, SearchQuery{Text: "Server.Handle"})
	if r.Hits[0].Symbol.ID != "a.go::Handle#method" || r.Hits[0].Match != MatchExact {
		t.Errorf("qualified query: %+v", r.Hits[0])
	}
	r = search(t, files, SearchQuery{Text: "quota"})
	if len(r.Hits) != 1 || r.Hits[0].Symbol.Name != "helper" || r.Hits[0].Match != MatchToken {
		t.Errorf("doc query: %+v", r.Hits)
	}
}

func TestSearchFilters(t *testing.T) {
	files := []*File{
		file("internal/a.go", "go", sym("internal/a.go", "Run", "function", 1), sym("internal/a.go", "Run", "method", 9)),
		file("cmd/b.go", "go", sym("cmd/b.go", "Run", "function", 1)),
		file("web/run.ts", "typescript", sym("web/run.ts", "run", "function", 1)),
		file("internal/gen/g.go", "go", sym("internal/gen/g.go", "Run", "function", 1)),
	}
	count := func(q SearchQuery) []string {
		q.Text = "run"
		var out []string
		for _, h := range search(t, files, q).Hits {
			out = append(out, h.File+":"+h.Symbol.Kind)
		}
		return out
	}
	if got := count(SearchQuery{}); len(got) != 5 {
		t.Errorf("unfiltered: %v", got)
	}
	if got := count(SearchQuery{Kinds: []string{"method"}}); !reflect.DeepEqual(got, []string{"internal/a.go:method"}) {
		t.Errorf("kind: %v", got)
	}
	if got := count(SearchQuery{Languages: []string{"TypeScript"}}); !reflect.DeepEqual(got, []string{"web/run.ts:function"}) {
		t.Errorf("language: %v", got)
	}
	if got := count(SearchQuery{Path: "internal"}); len(got) != 3 {
		t.Errorf("path: %v", got)
	}
	if got := count(SearchQuery{Path: "internal/"}); len(got) != 3 {
		t.Errorf("path with slash: %v", got)
	}
	if got := count(SearchQuery{Path: "internal/a.go"}); len(got) != 2 {
		t.Errorf("path to a file: %v", got)
	}
	if got := count(SearchQuery{Path: "inter"}); len(got) != 0 {
		t.Errorf("a path is a directory prefix, not a string prefix: %v", got)
	}
	if got := count(SearchQuery{Globs: []string{"*.go", "!gen"}}); len(got) != 3 {
		t.Errorf("globs: %v", got)
	}
	r := search(t, files, SearchQuery{Text: "run", Kinds: []string{"function"}, Path: "internal"})
	if r.Candidates != 2 || len(r.Hits) != 2 {
		t.Errorf("candidates %d hits %d, want 2 and 2", r.Candidates, len(r.Hits))
	}
	if _, err := NewSearchIndex(files).Search(SearchQuery{Text: "run", Globs: []string{"["}}); err == nil {
		t.Error("a bad glob was accepted")
	}
}

func TestSearchLimitTotalTruncated(t *testing.T) {
	var syms []Symbol
	for i := 0; i < 30; i++ {
		syms = append(syms, sym("a.go", fmt.Sprintf("item%02d", i), "function", i*3+1))
	}
	files := []*File{file("a.go", "go", syms...)}
	r := search(t, files, SearchQuery{Text: "item", Limit: 5})
	if len(r.Hits) != 5 || r.Total != 30 || !r.Truncated {
		t.Errorf("hits %d total %d truncated %v", len(r.Hits), r.Total, r.Truncated)
	}
	r = search(t, files, SearchQuery{Text: "item"})
	if len(r.Hits) != 30 || r.Truncated {
		t.Errorf("no limit: hits %d truncated %v", len(r.Hits), r.Truncated)
	}
	r = search(t, files, SearchQuery{Text: "item", Limit: 30})
	if r.Truncated {
		t.Error("a limit equal to the total is not truncation")
	}
}

// A typo inside a camelCase query still token-matches its other words, and that
// is an answer: serverNmae finds serverName and serverPort by "server", and
// fuzzy is not consulted. Fuzzy is for when nothing matches at all.
func TestSearchTypoWithOneGoodTokenIsNotFuzzy(t *testing.T) {
	files := []*File{file("a.go", "go", sym("a.go", "serverName", "function", 1))}
	r := search(t, files, SearchQuery{Text: "serverNmae", Fuzzy: true})
	if r.UsedFuzzy || len(r.Hits) != 1 || r.Hits[0].Match != MatchToken {
		t.Errorf("%+v", r)
	}
}

func TestSearchEmptyGivesNearestNotGuesses(t *testing.T) {
	files := []*File{file("a.go", "go",
		sym("a.go", "serverName", "function", 1),
		sym("a.go", "serverPort", "function", 5),
		sym("a.go", "Zebra", "type", 9))}
	r := search(t, files, SearchQuery{Text: "servernmae"})
	if len(r.Hits) != 0 || r.UsedFuzzy || r.Total != 0 {
		t.Fatalf("hits without --fuzzy: %+v", r)
	}
	if len(r.Nearest) == 0 || r.Nearest[0] != "serverName" {
		t.Errorf("nearest = %v", r.Nearest)
	}
	if len(r.Nearest) > nearestLimit {
		t.Errorf("nearest has %d names", len(r.Nearest))
	}
	if r.Candidates != 3 {
		t.Errorf("candidates = %d", r.Candidates)
	}
}

func TestSearchFuzzyFlagged(t *testing.T) {
	files := []*File{file("a.go", "go",
		sym("a.go", "serverName", "function", 1), sym("a.go", "Zebra", "type", 9))}
	r := search(t, files, SearchQuery{Text: "servernmae", Fuzzy: true})
	if !r.UsedFuzzy || len(r.Hits) != 1 || r.Hits[0].Symbol.Name != "serverName" || r.Hits[0].Match != MatchFuzzy {
		t.Fatalf("%+v", r)
	}
	if len(r.Nearest) != 0 {
		t.Errorf("nearest alongside hits: %v", r.Nearest)
	}
	if r.Hits[0].Score < 1 || r.Hits[0].Score >= 2 {
		t.Errorf("fuzzy score %v is outside tier 1", r.Hits[0].Score)
	}
	// Fuzzy is a fallback, never mixed into a real answer.
	files[0].Symbols = append(files[0].Symbols, sym("a.go", "serverNam", "function", 20))
	r = search(t, files, SearchQuery{Text: "server", Fuzzy: true})
	if r.UsedFuzzy {
		t.Error("fuzzy used although there were real matches")
	}
	// Nothing near: fuzzy finds nothing and the answer is the empty one.
	r = search(t, files, SearchQuery{Text: "qqqqqqqq", Fuzzy: true})
	if r.UsedFuzzy || len(r.Hits) != 0 || len(r.Nearest) != 0 {
		t.Errorf("hopeless query: %+v", r)
	}
	// Near but not matching: authoritative empty, with the names listed.
	r = search(t, files, SearchQuery{Text: "servernam2x"})
	if len(r.Hits) != 0 && r.UsedFuzzy {
		t.Errorf("fuzzy off but used: %+v", r)
	}
}

func TestSearchEmptyQueryAndIndex(t *testing.T) {
	r := search(t, nil, SearchQuery{Text: "x"})
	if len(r.Hits) != 0 || r.Nearest != nil {
		t.Errorf("empty index: %+v", r)
	}
	r = search(t, []*File{file("a.go", "go", sym("a.go", "A", "function", 1))}, SearchQuery{Text: "  _ "})
	if len(r.Hits) != 0 {
		t.Errorf("a query with no tokens: %+v", r)
	}
	// files whose outline was never built have no symbols and are ignored
	r = search(t, []*File{{Path: "x.go"}}, SearchQuery{Text: "x"})
	if len(r.Hits) != 0 {
		t.Errorf("%+v", r)
	}
}

// TestSearchDeterministic: the same answer on every run and whatever order the
// files arrive in, ties included.
func TestSearchDeterministic(t *testing.T) {
	var files []*File
	for i := 0; i < 12; i++ {
		p := fmt.Sprintf("pkg%02d/f.go", i%4)
		files = append(files, file(p, "go",
			sym(p, "Handler", "function", i+1), sym(p, fmt.Sprintf("handler%d", i), "function", i+40),
			sym(p, "newHandler", "function", i+80)))
	}
	// distinct paths for the duplicates so ids differ
	for i, f := range files {
		f.Path = fmt.Sprintf("pkg%02d/f%02d.go", i%4, i)
		for j := range f.Symbols {
			f.Symbols[j].ID = f.Path + "::" + f.Symbols[j].Name + "#function"
		}
	}
	key := func(fs []*File) string {
		r := search(t, fs, SearchQuery{Text: "handler"})
		var b strings.Builder
		for _, h := range r.Hits {
			fmt.Fprintf(&b, "%s %s %s %.9f\n", h.Match, h.File, h.Symbol.ID, h.Score)
		}
		return b.String()
	}
	want := key(files)
	if want != key(files) {
		t.Fatal("two runs differ")
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20; i++ {
		shuffled := append([]*File(nil), files...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := key(shuffled); got != want {
			t.Fatalf("file order changed the answer:\n%s\nvs\n%s", got, want)
		}
	}
}

func TestSearchShallowerNestingWinsTies(t *testing.T) {
	inner := sym("a.go", "run", "method", 5)
	inner.Parent = 1
	files := []*File{file("a.go", "go", sym("a.go", "T", "struct", 1), inner, sym("b.go", "run", "function", 1))}
	r := search(t, files, SearchQuery{Text: "run"})
	if len(r.Hits) != 2 || r.Hits[0].Symbol.Kind != "function" {
		t.Errorf("hits: %+v", r.Hits)
	}
}

func benchFiles(n int) []*File {
	var files []*File
	for i := 0; i < n/30; i++ {
		f := &File{Path: fmt.Sprintf("pkg%03d/file%03d.go", i%50, i), Language: "go", HasOutline: true}
		for j := 0; j < 30; j++ {
			name := fmt.Sprintf("Handle%sThing%d", []string{"Read", "Write", "Parse", "Load", "Save"}[j%5], j)
			f.Symbols = append(f.Symbols, Symbol{
				ID: f.Path + "::" + name + "#function", Name: name, Qualified: name, Kind: "function",
				Line: j*5 + 1, EndLine: j*5 + 4, Signature: "func " + name + "(ctx context.Context, in Input) (Output, error)",
				Doc: "Handles the thing for the request.",
			})
		}
		files = append(files, f)
	}
	return files
}

func BenchmarkNewSearchIndex100k(b *testing.B) {
	files := benchFiles(100000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		NewSearchIndex(files)
	}
}

func BenchmarkSearch3k(b *testing.B) {
	idx := NewSearchIndex(benchFiles(3000))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := idx.Search(SearchQuery{Text: "parseThing", Limit: 20}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestSearchSpeed is a guard, not a benchmark, and loose enough for -race
// (about 5x slower): unraced, the build of 100k symbols takes ~0.45 s and a
// query over 3k ~1.6 ms (the benchmarks above); the guard fails at 5 s and 50 ms.
func TestSearchSpeed(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	big := benchFiles(100000)
	res := testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			NewSearchIndex(big)
		}
	})
	if d := res.NsPerOp(); d > 5e9 {
		t.Errorf("building a 100k-symbol index took %.2fs", float64(d)/1e9)
	}
	small := NewSearchIndex(benchFiles(3000))
	res = testing.Benchmark(func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			small.Search(SearchQuery{Text: "parseThing", Limit: 20})
		}
	})
	if d := res.NsPerOp(); d > 50e6 {
		t.Errorf("a query over 3k symbols took %.2fms", float64(d)/1e6)
	}
}
