package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

// The whole-repo commands, against the text server of index_scenario_test.go:
// its outlines come from the files it is given, so what the index says can be
// checked against what is on disk, and an answer that is stale is visible.

// idxWorkspace is a workspace (a go.mod, a .git marker, the given files) with
// the working directory in it and a server that outlines from text.
func idxWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := tree(t, files)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	scenario{textOutline: true}.apply(t)
	return dir
}

var idxFiles = map[string]string{
	"a.go":     "package a\n\n// ParseConfig reads the configuration. It is strict.\nfunc ParseConfig() {}\n\nfunc parseConfigFile() {}\n\nfunc Other() {}\n",
	"b.go":     "package a\n\nfunc Parse() {}\n\nfunc Alpha() {}\n",
	"sub/c.go": "package sub\n\nfunc InSub() {}\n",
	"notes.md": "# notes\n",
}

func decodeSearch(t *testing.T, stdout string) (searchSymbolsData, rawEnvelope) {
	t.Helper()
	var d searchSymbolsData
	env := decodeData(t, stdout, &d)
	return d, env
}

func envWarns(env rawEnvelope, sub string) bool { return hasWarning(env.Warnings, sub) }

// decodeReport re-decodes a data.Index value (any, since D46 defaults it to a
// one-line reportSummary) as the full index.SyncReport a test wants to
// inspect field by field. The call under test must have passed --report.
func decodeReport(t *testing.T, v any) *index.SyncReport {
	t.Helper()
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshaling data.index: %v", err)
	}
	var rep index.SyncReport
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatalf("decoding data.index as index.SyncReport: %v (%s)", err, b)
	}
	return &rep
}

func TestSearchSymbolsRanksBuildsLazilyAndIsRepeatable(t *testing.T) {
	idxWorkspace(t, idxFiles)

	code, stdout, stderr := runMain("search_symbols", "ParseConfig", "--report")
	if code != ExitOK {
		t.Fatalf("exit %d\nstderr: %s\nstdout: %s", code, stderr, stdout)
	}
	d, env := decodeSearch(t, stdout)
	if len(d.Results) < 2 {
		t.Fatalf("results = %+v", d.Results)
	}
	if got := d.Results[0]; got.Name != "ParseConfig" || got.Match != "exact" || got.ID != "a.go::ParseConfig#function" || got.File != "a.go" || got.Line != 4 {
		t.Errorf("first result = %+v", got)
	}
	if got := d.Results[1]; got.Name != "parseConfigFile" || got.Match != "prefix" {
		t.Errorf("second result = %+v", got)
	}
	if d.Results[0].Doc != "ParseConfig reads the configuration." {
		t.Errorf("doc sentence = %q", d.Results[0].Doc)
	}
	rep := decodeReport(t, d.Index)
	if !envWarns(env, "built lazily") || rep == nil || rep.Built == 0 || rep.Outlines != 4 {
		t.Errorf("a cold query must say it built, and how much: %v / %+v", env.Warnings, rep)
	}

	// The second query builds nothing, and says nothing about it.
	code, stdout2, _ := runMain("search_symbols", "ParseConfig", "--report")
	if code != ExitOK {
		t.Fatalf("second exit %d", code)
	}
	d2, env2 := decodeSearch(t, stdout2)
	rep2 := decodeReport(t, d2.Index)
	if envWarns(env2, "built lazily") || rep2.Built != 0 || rep2.Fresh == 0 {
		t.Errorf("a warm query built: %v / %+v", env2.Warnings, rep2)
	}
	if !slices.EqualFunc(d.Results, d2.Results, func(a, b symbolHit) bool { return a.ID == b.ID && a.Match == b.Match }) {
		t.Errorf("the same query gave different results: %+v vs %+v", d.Results, d2.Results)
	}
}

func TestSearchSymbolIDsAreTheOnesOutlineAndSourceUse(t *testing.T) {
	idxWorkspace(t, idxFiles)

	_, out, _ := runMain("search_symbols", "ParseConfig", "--detail", "full", "--limit", "1")
	d, _ := decodeSearch(t, out)
	if len(d.Results) != 1 || d.Results[0].Source == nil {
		t.Fatalf("full detail must inline the source: %+v", d.Results)
	}
	hit := d.Results[0]

	// The id resolves in `source`, to the same text and the same hash.
	code, srcOut, stderr := runMain("source", hit.ID)
	if code != ExitOK {
		t.Fatalf("source %s: exit %d: %s%s", hit.ID, code, srcOut, stderr)
	}
	var sd sourceData
	decodeData(t, srcOut, &sd)
	if len(sd.Symbols) != 1 || sd.Symbols[0].Source != hit.Source.Source || sd.Symbols[0].Hash != hit.Source.Hash {
		t.Errorf("search --detail full and source disagree:\n%+v\n%+v", hit.Source, sd.Symbols)
	}
	if !strings.HasPrefix(hit.Source.Source, "// ParseConfig reads the configuration.") {
		t.Errorf("the doc comment is part of the symbol's source: %q", hit.Source.Source)
	}

	// And it is the id `outline` lists.
	_, outl, _ := runMain("outline", "a.go")
	var od outlineData
	decodeData(t, outl, &od)
	var ids []string
	var walk func(ns []*outlineNode)
	walk = func(ns []*outlineNode) {
		for _, n := range ns {
			ids = append(ids, n.ID)
			walk(n.Children)
		}
	}
	walk(od.Files[0].Symbols)
	if !slices.Contains(ids, hit.ID) {
		t.Errorf("outline ids %v do not include the search id %s", ids, hit.ID)
	}
}

func TestSearchSymbolsDetailLevels(t *testing.T) {
	idxWorkspace(t, idxFiles)

	_, out, _ := runMain("search_symbols", "ParseConfig", "--detail", "compact", "--limit", "1")
	var raw struct {
		Results []map[string]any `json:"results"`
	}
	decodeData(t, out, &raw)
	if len(raw.Results) != 1 {
		t.Fatalf("results = %v", raw.Results)
	}
	for _, k := range []string{"signature", "doc", "container", "source", "end_line"} {
		if _, ok := raw.Results[0][k]; ok {
			t.Errorf("compact result carries %q: %v", k, raw.Results[0])
		}
	}
	// The match quality is what a compact row is for reading, so it is there at
	// every level (D42).
	for _, k := range []string{"id", "name", "kind", "file", "line", "match", "score"} {
		if _, ok := raw.Results[0][k]; !ok {
			t.Errorf("compact result lacks %q: %v", k, raw.Results[0])
		}
	}

	_, out, _ = runMain("search_symbols", "ParseConfig", "--limit", "1")
	decodeData(t, out, &raw)
	for _, k := range []string{"signature", "doc", "end_line"} {
		if _, ok := raw.Results[0][k]; !ok {
			t.Errorf("standard result lacks %q: %v", k, raw.Results[0])
		}
	}
	if _, ok := raw.Results[0]["source"]; ok {
		t.Error("standard result carries source")
	}

	if code, out, _ := runMain("search_symbols", "ParseConfig", "--detail", "verbose"); code != ExitUsage {
		t.Errorf("--detail verbose: exit %d, want usage: %s", code, out)
	}
}

func TestSearchSymbolsFiltersAndTruncation(t *testing.T) {
	idxWorkspace(t, idxFiles)

	names := func(args ...string) ([]string, searchSymbolsData) {
		t.Helper()
		_, out, _ := runMain(append([]string{"search_symbols"}, args...)...)
		d, _ := decodeSearch(t, out)
		var n []string
		for _, r := range d.Results {
			n = append(n, r.Name)
		}
		return n, d
	}
	if got, _ := names("in", "--path", "sub"); !slices.Equal(got, []string{"InSub"}) {
		t.Errorf("--path sub: %v", got)
	}
	if got, _ := names("parse", "--glob", "b.go"); !slices.Equal(got, []string{"Parse"}) {
		t.Errorf("--glob b.go: %v", got)
	}
	if got, _ := names("parse", "--glob", "!b.go", "--kind", "function"); slices.Contains(got, "Parse") || len(got) == 0 {
		t.Errorf("--glob !b.go: %v", got)
	}
	if got, _ := names("parse", "--lang", "python"); len(got) != 0 {
		t.Errorf("--lang python found Go symbols: %v", got)
	}
	if got, _ := names("parse", "--kind", "struct"); len(got) != 0 {
		t.Errorf("--kind struct: %v", got)
	}
	got, d := names("parse", "--limit", "1")
	if len(got) != 1 || !d.Truncated || d.Total < 2 || d.Count != 1 {
		t.Errorf("--limit 1: %v truncated=%v total=%d", got, d.Truncated, d.Total)
	}
}

func TestSearchSymbolsEmptyIsAuthoritativeAndFuzzyIsFlagged(t *testing.T) {
	idxWorkspace(t, idxFiles)

	code, out, _ := runMain("search_symbols", "Alphx")
	if code != ExitProblems {
		t.Fatalf("exit %d, want 1 for an authoritative empty answer\n%s", code, out)
	}
	d, env := decodeSearch(t, out)
	if !env.OK || len(d.Results) != 0 || d.Fuzzy || !slices.Contains(d.Nearest, "Alpha") {
		t.Errorf("empty answer = %+v (nearest %v)", d, d.Nearest)
	}

	code, out, _ = runMain("search_symbols", "Alphx", "--fuzzy")
	if code != ExitOK {
		t.Fatalf("--fuzzy exit %d\n%s", code, out)
	}
	d, env = decodeSearch(t, out)
	if !d.Fuzzy || len(d.Results) == 0 || d.Results[0].Name != "Alpha" || d.Results[0].Match != "fuzzy" || !envWarns(env, "nearest names") {
		t.Errorf("fuzzy answer = %+v / %v", d, env.Warnings)
	}
}

func TestSearchSymbolsFuzzyCatchesATransposition(t *testing.T) {
	idxWorkspace(t, idxFiles)

	code, out, _ := runMain("search_symbols", "Alhpa", "--fuzzy")
	if code != ExitOK {
		t.Fatalf("--fuzzy exit %d\n%s", code, out)
	}
	if d, _ := decodeSearch(t, out); !d.Fuzzy || len(d.Results) == 0 || d.Results[0].Name != "Alpha" {
		t.Errorf("a transposed name must fuzzy-match: %+v", d)
	}
}

func TestSearchSymbolsRejectsAnUnknownKind(t *testing.T) {
	idxWorkspace(t, idxFiles)

	code, out, _ := runMain("search_symbols", "parse", "--kind", "bogus")
	if code != ExitUsage || !strings.Contains(out, "bogus") {
		t.Errorf("--kind bogus: exit %d, want usage\n%s", code, out)
	}
	if code, out, _ := runMain("search_symbols", "parse", "--kind", "function,Struct"); code == ExitUsage {
		t.Errorf("known kinds, in any case, are accepted: %s", out)
	}
}

func TestSearchSymbolsTextFormat(t *testing.T) {
	idxWorkspace(t, idxFiles)
	code, out, _ := runMain("search_symbols", "ParseConfig", "--format", "text", "--limit", "2")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if !strings.HasPrefix(lines[0], "a.go:4: function ParseConfig  ") {
		t.Errorf("first line = %q", lines[0])
	}
	if !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "# ") }) {
		t.Errorf("the notices (built lazily, truncation) are missing from text: %q", out)
	}
}

func TestIndexSeesEditsAddsDeletesAndRenamesBetweenInvocations(t *testing.T) {
	dir := idxWorkspace(t, idxFiles)
	if code, out, _ := runMain("index", "build"); code != ExitOK {
		t.Fatalf("index build: exit %d\n%s", code, out)
	}
	find := func(q string) []string {
		t.Helper()
		_, out, _ := runMain("search_symbols", q)
		d, _ := decodeSearch(t, out)
		var ids []string
		for _, r := range d.Results {
			if r.Match == "exact" {
				ids = append(ids, r.ID)
			}
		}
		return ids
	}
	if got := find("Alpha"); !slices.Equal(got, []string{"b.go::Alpha#function"}) {
		t.Fatalf("Alpha before: %v", got)
	}

	write(t, filepath.Join(dir, "b.go"), "package a\n\nfunc Parse() {}\n\nfunc Beta() {}\n\n// extra line\n")
	write(t, filepath.Join(dir, "d.go"), "package a\n\nfunc Delta() {}\n")
	if err := os.Remove(filepath.Join(dir, "a.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "sub", "c.go"), filepath.Join(dir, "sub", "renamed.go")); err != nil {
		t.Fatal(err)
	}

	for name, want := range map[string][]string{
		"Alpha":       nil,                                // removed from an edited file
		"Beta":        {"b.go::Beta#function"},            // added to an edited file
		"Delta":       {"d.go::Delta#function"},           // new file
		"ParseConfig": nil,                                // deleted file
		"InSub":       {"sub/renamed.go::InSub#function"}, // renamed file: new path, old one gone
	} {
		if got := find(name); !slices.Equal(got, want) {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

func TestCorruptIndexCacheIsDiscardedAndRebuilt(t *testing.T) {
	dir := idxWorkspace(t, idxFiles)
	if code, out, _ := runMain("index", "build"); code != ExitOK {
		t.Fatalf("index build: exit %d\n%s", code, out)
	}
	cache, err := daemon.IndexCacheDir(canonPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	parts, _ := filepath.Glob(filepath.Join(cache, "part-*.json"))
	if len(parts) == 0 {
		t.Fatalf("nothing was persisted in %s", cache)
	}
	for _, p := range parts {
		raw, _ := os.ReadFile(p)
		if err := os.WriteFile(p, raw[:len(raw)/2], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A fresh process reads it: --no-daemon is one, and the daemon of a
	// daemon-mode run would still have its own memory.
	retireDaemons(t)
	code, out, stderr := runMain("--no-daemon", "search_symbols", "Alpha")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	d, env := decodeSearch(t, out)
	if !envWarns(env, "discarded the cached index part") || len(d.Results) == 0 || d.Results[0].ID != "b.go::Alpha#function" {
		t.Errorf("results %+v, warnings %v", d.Results, env.Warnings)
	}
}

func TestNotReadyServerIsExit5AndRecordsNothing(t *testing.T) {
	dir := tree(t, idxFiles)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	scenario{textOutline: true, indexing: true}.apply(t)

	code, out, _ := runMain("search_symbols", "Alpha", "--timeout", "1s")
	if code != ExitNotReady {
		t.Fatalf("exit %d, want 5\n%s", code, out)
	}
	env := decodeData(t, out, nil)
	if env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Errorf("envelope = %+v", env)
	}
	cache, _ := daemon.IndexCacheDir(canonPath(dir))
	if parts, _ := filepath.Glob(filepath.Join(cache, "part-*.json")); len(parts) != 0 {
		t.Errorf("a server that was still indexing had its outlines recorded: %v", parts)
	}
	// Imports need no server: they answer while the server is not ready.
	if code, out, _ := runMain("imports", "a.go"); code != ExitOK {
		t.Errorf("imports while a server is indexing: exit %d\n%s", code, out)
	}
}

func TestIndexStatusBuildClear(t *testing.T) {
	idxWorkspace(t, idxFiles)

	code, out, _ := runMain("index", "status")
	if code != ExitOK {
		t.Fatalf("status exit %d\n%s", code, out)
	}
	var st index.Status
	decodeData(t, out, &st)
	if st.Warm || st.Indexed != 0 || st.Files != 5 && st.Files != 4 {
		t.Errorf("cold status = %+v", st)
	}

	code, out, _ = runMain("index", "build")
	if code != ExitOK {
		t.Fatalf("build exit %d\n%s", code, out)
	}
	var br index.BuildResult
	env := decodeData(t, out, &br)
	if br.Report.Outlines != 4 || br.Status.Symbols != 6 || !br.Status.Warm {
		t.Errorf("build result = %+v / %+v", br.Report, br.Status)
	}
	if !envWarns(env, "files no server and no import extractor covers") {
		t.Errorf("uncovered files must be said: %v", env.Warnings)
	}
	uncovered := map[string]int{}
	for _, u := range br.Status.Uncovered {
		uncovered[u.Language] = u.Files
	}
	if uncovered["markdown"] != 1 {
		t.Errorf("uncovered = %v", br.Status.Uncovered)
	}
	if len(br.Status.Servers) != 1 || br.Status.Servers[0].Indexed != 4 || br.Status.CacheBytes == 0 || !br.Status.CachePresent {
		t.Errorf("servers %+v cache %d", br.Status.Servers, br.Status.CacheBytes)
	}

	_, out, _ = runMain("index", "status", "--format", "text")
	if !strings.Contains(out, "warm") || !strings.Contains(out, "server:   fake") && !strings.Contains(out, "server:   gopls") {
		t.Errorf("status text:\n%s", out)
	}

	code, out, _ = runMain("index", "clear")
	if code != ExitOK {
		t.Fatalf("clear exit %d\n%s", code, out)
	}
	var cr index.ClearResult
	decodeData(t, out, &cr)
	if cr.Entries != 4 || cr.Parts == 0 {
		t.Errorf("clear = %+v", cr)
	}
	_, out, _ = runMain("index", "status")
	decodeData(t, out, &st)
	if st.Indexed != 0 || st.Warm {
		t.Errorf("status after clear = %+v", st)
	}

	if code, _, _ := runMain("index"); code != ExitUsage {
		t.Errorf("index with no subcommand: exit %d", code)
	}
	if code, _, _ := runMain("index", "frobnicate"); code != ExitUsage {
		t.Errorf("index frobnicate: exit %d", code)
	}
}

var impFiles = map[string]string{
	"main.go":      "package main\n\nimport (\n\t\"fmt\"\n\t// \"fixture/commented\"\n\t\"fixture/util\"\n)\n\nfunc main() { fmt.Println(util.X) }\n",
	"util/util.go": "package util\n\nvar X = 1\n",
	"a.py":         "import b\n",
	"b.py":         "import a\n# import c\n",
	"README.md":    "# readme\n",
}

func TestImportCommands(t *testing.T) {
	idxWorkspace(t, impFiles)

	code, out, stderr := runMain("imports", "main.go")
	if code != ExitOK {
		t.Fatalf("imports: exit %d\n%s%s", code, out, stderr)
	}
	var id importsData
	decodeData(t, out, &id)
	byspec := map[string]index.ResolvedImport{}
	for _, im := range id.Imports {
		byspec[im.Spec] = im
	}
	if len(id.Imports) != 2 || !byspec["fmt"].External || byspec["fmt"].Category != "stdlib" ||
		byspec["fixture/util"].External || !slices.Equal(byspec["fixture/util"].Targets, []string{"util"}) {
		t.Errorf("imports = %+v", id.Imports)
	}
	if _, ok := byspec["fixture/commented"]; ok {
		t.Error("an import inside a comment was reported")
	}

	for _, target := range []string{"util", "util/util.go", "fixture/util"} {
		code, out, _ = runMain("find_importers", target)
		if code != ExitOK {
			t.Fatalf("find_importers %s: exit %d\n%s", target, code, out)
		}
		var imp importersData
		decodeData(t, out, &imp)
		if len(imp.Importers) != 1 || imp.Importers[0].File != "main.go" || imp.Importers[0].Line != 6 || imp.Count != 1 {
			t.Errorf("find_importers %s = %+v", target, imp.Importers)
		}
	}
	// A file nothing imports: an authoritative empty answer.
	if code, out, _ := runMain("find_importers", "main.go"); code != ExitProblems {
		t.Errorf("find_importers of an unimported file: exit %d\n%s", code, out)
	}

	code, out, _ = runMain("dependency_graph", "main.go", "--depth", "2", "--external")
	if code != ExitOK {
		t.Fatalf("dependency_graph: exit %d\n%s", code, out)
	}
	var gd graphData
	decodeData(t, out, &gd)
	var edges []string
	for _, e := range gd.Edges {
		edges = append(edges, e.From+"->"+e.To)
	}
	if !slices.Contains(edges, "util->util") && !slices.Contains(edges, ".->util") {
		// main.go is in the root package; either spelling names the same edge.
		if !slices.ContainsFunc(edges, func(e string) bool { return strings.HasSuffix(e, "->util") }) {
			t.Errorf("edges = %v", edges)
		}
	}
	_, txt, _ := runMain("dependency_graph", "main.go", "--format", "text")
	if !strings.Contains(txt, "util") {
		t.Errorf("text tree:\n%s", txt)
	}

	code, out, _ = runMain("dependency_cycles")
	if code != ExitProblems {
		t.Fatalf("dependency_cycles: exit %d, want 1 for a cycle\n%s", code, out)
	}
	var cd cyclesData
	decodeData(t, out, &cd)
	if cd.Count != 1 || !slices.Equal(cd.Cycles[0], []string{"a.py", "b.py"}) {
		t.Errorf("cycles = %+v", cd.Cycles)
	}

	// A language no extractor reads is "not covered", never "no imports".
	code, out, _ = runMain("imports", "README.md")
	env := decodeData(t, out, nil)
	if code != ExitNoServer || env.OK || env.Error.Code != "not_covered" {
		t.Errorf("imports README.md: exit %d, %+v", code, env.Error)
	}
	if code, out, _ := runMain("find_importers", "nosuch/thing"); code == ExitOK {
		t.Errorf("find_importers of a node that is not there succeeded: %s", out)
	}
}

func TestIndexCommandsAreConfinedToTheWorkspace(t *testing.T) {
	dir := idxWorkspace(t, idxFiles)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "x.go"), "package x\n\nfunc Secret() {}\n")
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Skip("symlinks unavailable")
	}

	for _, args := range [][]string{
		{"imports", "../" + filepath.Base(outside) + "/x.go"},
		{"imports", filepath.Join(outside, "x.go")},
		{"imports", "link/x.go"},
		{"search_symbols", "Secret", "--path", outside},
		{"search_symbols", "Secret", "--path", ".."},
		{"search_symbols", "Secret", "--path", "link"},
		{"repo_map", "--path", "link"},
		{"find_importers", filepath.Join(outside, "x.go")},
		{"dependency_graph", outside},
	} {
		code, out, _ := runMain(args...)
		env := decodeData(t, out, nil)
		if code != ExitUsage || env.OK || env.Error == nil || env.Error.Code != "outside_workspace" {
			t.Errorf("%v: exit %d %+v\n%s", args, code, env.Error, out)
		}
	}

	// A symlinked file inside the workspace is not read, and its symbols are not indexed.
	if err := os.Symlink(filepath.Join(outside, "x.go"), filepath.Join(dir, "linked.go")); err != nil {
		t.Skip("symlinks unavailable")
	}
	_, out, _ := runMain("search_symbols", "Secret")
	d, _ := decodeSearch(t, out)
	if len(d.Results) != 0 {
		t.Errorf("a symlink out of the workspace was indexed: %+v", d.Results)
	}
}

func TestRepoMapIsTokenBudgetedAndRanked(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"main.go":      "package main\n\nimport \"fixture/util\"\n\nfunc main() {}\n",
		"other.go":     "package main\n\nimport \"fixture/util\"\n\nfunc Other() {}\n",
		"util/util.go": "package util\n\nfunc Helper() {}\n\nfunc More() {}\n",
	})
	code, out, stderr := runMain("repo_map", "--budget", "2000")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	var rd repoMapData
	env := decodeData(t, out, &rd)
	if len(rd.Files) != 3 || rd.Files[0].Path != "util/util.go" || rd.Truncated || !rd.Ranked {
		t.Errorf("map = %+v", rd.RepoMapResult)
	}
	if !envWarns(env, "built lazily") {
		t.Errorf("warnings = %v", env.Warnings)
	}

	code, out, _ = runMain("repo_map", "--budget", "20", "--format", "json")
	if code != ExitOK {
		t.Fatalf("small budget: exit %d\n%s", code, out)
	}
	decodeData(t, out, &rd)
	if !rd.Truncated || rd.TokensEstimated > 20 || rd.FilesListed >= rd.FilesTotal {
		t.Errorf("budget not respected: %+v", rd.RepoMapResult)
	}

	_, out, _ = runMain("repo_map", "--format", "text")
	if !strings.HasPrefix(out, "util/util.go:\n") || !strings.Contains(out, "  function Helper") {
		t.Errorf("text map:\n%s", out)
	}
	if code, _, _ := runMain("repo_map", "--budget", "0"); code != ExitUsage {
		t.Errorf("--budget 0: exit %d", code)
	}
}

func TestRepoOutlineCarriesSymbolCountsOnlyWhenWarm(t *testing.T) {
	idxWorkspace(t, idxFiles)

	_, out, _ := runMain("repo_outline")
	var cold map[string]any
	decodeData(t, out, &cold)
	if _, ok := cold["symbols"]; ok {
		t.Errorf("repo_outline forced or invented an index: %v", cold["symbols"])
	}
	// asking did not build anything
	_, out, _ = runMain("index", "status")
	var st index.Status
	decodeData(t, out, &st)
	if st.Indexed != 0 {
		t.Fatalf("repo_outline built the index: %+v", st)
	}

	if code, out, _ := runMain("index", "build"); code != ExitOK {
		t.Fatalf("build: %d %s", code, out)
	}
	_, out, _ = runMain("repo_outline", "--depth", "2")
	var warm repoOutlineData
	decodeData(t, out, &warm)
	if warm.Symbols == nil || warm.Symbols.Total != 6 || warm.Symbols.Files != 4 || warm.Symbols.ByKind["function"] != 6 {
		t.Fatalf("symbols = %+v", warm.Symbols)
	}
	dirs := map[string]int{}
	for _, d := range warm.Directories {
		dirs[d.Path] = d.Symbols
	}
	if dirs["."] != 6 || dirs["sub"] != 1 {
		t.Errorf("per-directory symbols = %v", dirs)
	}

	// A file that changes since is not counted, and the answer says so.
	write(t, filepath.Join(".", "b.go"), "package a\n\nfunc Parse() {}\n\nfunc Alpha() {}\n\nfunc Extra() {}\n")
	_, out, _ = runMain("repo_outline")
	decodeData(t, out, &warm)
	if warm.Symbols == nil || warm.Symbols.NotCurrent != 1 || warm.Symbols.Total != 4 {
		t.Errorf("after an edit: %+v", warm.Symbols)
	}
}

// --- through the daemon: the freshness the daemon adds ---

func TestExternalEditIsSeenByReferencesThroughAWarmDaemon(t *testing.T) {
	dir := idxWorkspace(t, map[string]string{
		"main.go":  "package p\n\nfunc Use() {}\n",
		"other.go": "package p\n\n// NEEDLE\nfunc A() {}\n",
	})
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)
	traceFile := filepath.Join(t.TempDir(), "trace.jsonl")
	t.Setenv(traceEnv, traceFile)

	refs := func() int {
		t.Helper()
		code, out, stderr := runMain("references", "main.go:3:6")
		if code != ExitOK {
			t.Fatalf("references: exit %d\n%s%s", code, out, stderr)
		}
		rs := decodeResults(t, out)
		if len(rs.Results) != 1 || !strings.HasSuffix(rs.Results[0].Path, "other.go") {
			t.Fatalf("results = %+v", rs.Results)
		}
		return rs.Results[0].Start.Line
	}
	if line := refs(); line != 3 {
		t.Fatalf("first answer: line %d, want 3", line)
	}

	// An editor (or git checkout) changes a file lightspeed never opened. The
	// server has it cached; only the daemon can tell it.
	write(t, filepath.Join(dir, "other.go"), "package p\n\n\n\n// NEEDLE\nfunc A() {}\n")
	if line := refs(); line != 5 {
		t.Fatalf("after an external edit the warm server still answers from its old reading: line %d, want 5", line)
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d servers were started; the second query was meant to be a warm one", n)
	}
	trace, _ := os.ReadFile(traceFile)
	if strings.Count(string(trace), "workspace/didChangeWatchedFiles") != 1 || !strings.Contains(string(trace), "other.go") {
		t.Errorf("the server should have been told of exactly the one changed file:\n%s", trace)
	}

	// Nothing changed: nothing is sent.
	_ = refs()
	trace, _ = os.ReadFile(traceFile)
	if strings.Count(string(trace), "workspace/didChangeWatchedFiles") != 1 {
		t.Errorf("an unchanged tree produced a notification:\n%s", trace)
	}

	// A file that is created and one that is deleted are told as well.
	write(t, filepath.Join(dir, "new.go"), "package p\n")
	if err := os.Remove(filepath.Join(dir, "main.go")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "main.go"), "package p\n\nfunc Use() {}\n\nfunc Use2() {}\n")
	_ = refs()
	trace, _ = os.ReadFile(traceFile)
	if !strings.Contains(string(trace), "new.go") {
		t.Errorf("a created file was not reported:\n%s", trace)
	}
}

func TestDaemonBuildsTheIndexOnceForManyCommands(t *testing.T) {
	idxWorkspace(t, idxFiles)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	for i := 0; i < 3; i++ {
		if code, out, _ := runMain("search_symbols", "Alpha"); code != ExitOK {
			t.Fatalf("search %d: exit %d\n%s", i, code, out)
		}
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started for three searches", n)
	}
	_, out, _ := runMain("search_symbols", "Alpha", "--report")
	d, env := decodeSearch(t, out)
	rep := decodeReport(t, d.Index)
	if envWarns(env, "built lazily") || rep.Built != 0 {
		t.Errorf("a warm daemon rebuilt: %+v %v", rep, env.Warnings)
	}
}

// --- MCP ---

func TestMCPIndexTools(t *testing.T) {
	dir := idxWorkspace(t, impFiles)
	write(t, filepath.Join(dir, "a.go"), "package main\n\nfunc ParseConfig() {}\n")
	cs := mcpSession(t, dir)

	call := func(name string, args map[string]any) (env rawEnvelope, exit int) {
		t.Helper()
		res := callTool(t, cs, name, args)
		return decodeData(t, resultText(t, res), nil), resultExit(t, res)
	}

	env, exit := call("index_status", nil)
	if !env.OK || exit != ExitOK {
		t.Errorf("index_status: %+v exit %d", env, exit)
	}
	env, exit = call("index_build", nil)
	if !env.OK || exit != ExitOK {
		t.Errorf("index_build: %+v exit %d", env, exit)
	}
	res := callTool(t, cs, "search_symbols", map[string]any{"query": "ParseConfig", "detail": "compact", "kind": []string{"function"}})
	var sd searchSymbolsData
	decodeData(t, resultText(t, res), &sd)
	if res.IsError || len(sd.Results) == 0 || sd.Results[0].ID != "a.go::ParseConfig#function" || sd.Detail != "compact" {
		t.Errorf("search_symbols: %+v", sd)
	}
	// no match: exit 1 in the result's meta, still not a protocol failure
	res = callTool(t, cs, "search_symbols", map[string]any{"query": "Nonexistentzzz"})
	if resultExit(t, res) != ExitProblems {
		t.Errorf("empty search exit %d", resultExit(t, res))
	}

	res = callTool(t, cs, "repo_map", map[string]any{"budget": 500})
	var rd repoMapData
	decodeData(t, resultText(t, res), &rd)
	if res.IsError || len(rd.Files) == 0 {
		t.Errorf("repo_map: %+v", rd)
	}
	res = callTool(t, cs, "imports", map[string]any{"file": "main.go"})
	var id importsData
	decodeData(t, resultText(t, res), &id)
	if res.IsError || len(id.Imports) != 2 {
		t.Errorf("imports: %+v", id)
	}
	res = callTool(t, cs, "find_importers", map[string]any{"target": "util"})
	var imp importersData
	decodeData(t, resultText(t, res), &imp)
	if res.IsError || len(imp.Importers) != 1 {
		t.Errorf("find_importers: %+v", imp)
	}
	res = callTool(t, cs, "dependency_graph", map[string]any{"path": "main.go", "direction": "out", "depth": 1})
	var gd graphData
	decodeData(t, resultText(t, res), &gd)
	if res.IsError || len(gd.Nodes) == 0 {
		t.Errorf("dependency_graph: %+v", gd)
	}
	res = callTool(t, cs, "dependency_cycles", nil)
	var cd cyclesData
	decodeData(t, resultText(t, res), &cd)
	if cd.Count != 1 || resultExit(t, res) != ExitProblems {
		t.Errorf("dependency_cycles: %+v exit %d", cd, resultExit(t, res))
	}
	// an error envelope is an isError result
	res = callTool(t, cs, "imports", map[string]any{"file": "README.md"})
	if !res.IsError || decodeData(t, resultText(t, res), nil).Error.Code != "not_covered" {
		t.Errorf("imports of an uncovered file: %+v", res)
	}
	res = callTool(t, cs, "index_clear", nil)
	if res.IsError {
		t.Errorf("index_clear: %s", resultText(t, res))
	}
}

func TestMCPSearchSymbolsNotReadyIsAnErrorResult(t *testing.T) {
	dir := tree(t, idxFiles)
	t.Chdir(dir)
	scenario{textOutline: true, indexing: true}.apply(t)
	cs := mcpSession(t, dir)
	res := callTool(t, cs, "search_symbols", map[string]any{"query": "Alpha", "timeout": "1s"})
	if !res.IsError || resultExit(t, res) != ExitNotReady {
		t.Errorf("a not-ready workspace: isError=%v exit=%d", res.IsError, resultExit(t, res))
	}
}

// A per-file error in an `outline` batch names the file the way its ids do —
// relative to the workspace — whatever directory the command ran in and however
// the argument was spelled (D31).
func TestOutlineBatchErrorUsesTheWorkspaceRelativePath(t *testing.T) {
	dir := idxWorkspace(t, idxFiles)
	t.Chdir(filepath.Join(dir, "sub"))

	code, out, _ := runMain("outline", "c.go", "../missing.go", filepath.Join(dir, "gone", "x.go"))
	if code != ExitProblems {
		t.Fatalf("exit %d, want 1 (one file could be outlined, others could not)\n%s", code, out)
	}
	var od outlineData
	decodeData(t, out, &od)
	var files []string
	for _, f := range od.Files {
		files = append(files, f.File)
		if f.Error != nil && (strings.HasPrefix(f.Error.Target, "..") || filepath.IsAbs(f.Error.Target)) {
			t.Errorf("error target %q is not workspace-relative", f.Error.Target)
		}
		if f.Error != nil && strings.Contains(f.Error.Message, dir) {
			t.Errorf("error message %q leaks the absolute path", f.Error.Message)
		}
	}
	want := []string{"sub/c.go", "missing.go", "gone/x.go"}
	if !slices.Equal(files, want) {
		t.Errorf("files = %v, want %v", files, want)
	}
	if !strings.Contains(out, "gone/x.go: no such file") || strings.Contains(out, dir) {
		t.Errorf("the message and the warning must name gone/x.go relative to the workspace, and no absolute path may appear:\n%s", out)
	}
}

// `index build` is the explicit warm-up; the "built lazily" notice is for a
// query that had to build, and would tell the caller to run what they just ran.
func TestExplicitIndexBuildDoesNotClaimALazyBuild(t *testing.T) {
	idxWorkspace(t, idxFiles)
	code, out, stderr := runMain("index", "build")
	if code != ExitOK {
		t.Fatalf("index build: exit %d\nstderr: %s\n%s", code, stderr, out)
	}
	var res struct {
		Report *struct {
			Built int `json:"built"`
		} `json:"report"`
	}
	env := decodeData(t, out, &res)
	if res.Report == nil || res.Report.Built == 0 {
		t.Fatalf("a cold build must have built something: %s", out)
	}
	if envWarns(env, "built lazily") {
		t.Errorf("an explicit build was reported as lazy: %v", env.Warnings)
	}
}

// A cut graph shows the nodes of the edges it lists and no others, whether or
// not the walk began at a named file: over the whole workspace every file is a
// start, and keeping them all left 29 nodes beside 2 edges.
func TestDependencyGraphLimitCutsNodesWithEdges(t *testing.T) {
	idxWorkspace(t, impFiles)
	for _, args := range [][]string{{"dependency_graph", "--limit", "1"}, {"dependency_graph", "--limit", "1", "--direction", "both"}} {
		code, out, _ := runMain(args...)
		if code != ExitOK {
			t.Fatalf("%v: exit %d\n%s", args, code, out)
		}
		var gd graphData
		decodeData(t, out, &gd)
		if !gd.Truncated || gd.Count != 1 || gd.Total < 2 || len(gd.Edges) != 1 {
			t.Fatalf("%v: %+v", args, gd)
		}
		want := []string{gd.Edges[0].From, gd.Edges[0].To}
		var got []string
		for _, n := range gd.Nodes {
			got = append(got, n.ID)
		}
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(want, got) {
			t.Errorf("%v: nodes %v do not match the edge's ends %v", args, got, want)
		}
	}
}

// TestIndexBackedReportsAreCompactByDefault pins D46's fix for every
// index-backed command besides search_symbols and index_status (which have
// their own coverage): repo_map, find_importers, imports, dependency_graph
// and dependency_cycles must default their `report` field to a one-line
// reportSummary and restore the full index.SyncReport only with --report.
func TestIndexBackedReportsAreCompactByDefault(t *testing.T) {
	idxWorkspace(t, impFiles)

	assertCompact := func(t *testing.T, v any) {
		t.Helper()
		m, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("report is not a compact object: %#v", v)
		}
		if _, ok := m["summary"]; !ok {
			t.Errorf("compact report has no summary: %#v", m)
		}
		if _, ok := m["stale"]; !ok {
			t.Errorf("compact report has no stale flag: %#v", m)
		}
		if _, ok := m["files"]; ok {
			t.Errorf("report is not compact by default, the full field 'files' leaked through: %#v", m)
		}
	}
	assertFull := func(t *testing.T, v any) {
		t.Helper()
		rep := decodeReport(t, v)
		if rep == nil || rep.Files == 0 {
			t.Fatalf("--report did not restore the full index.SyncReport: %+v", rep)
		}
	}

	t.Run("repo_map", func(t *testing.T) {
		_, out, stderr := runMain("repo_map")
		var d repoMapData
		decodeData(t, out, &d)
		assertCompact(t, d.Report)
		if _, out, _ = runMain("repo_map", "--report"); out == "" {
			t.Fatalf("repo_map --report: no output\n%s", stderr)
		}
		decodeData(t, out, &d)
		assertFull(t, d.Report)
	})
	t.Run("find_importers", func(t *testing.T) {
		_, out, _ := runMain("find_importers", "util/util.go")
		var d importersData
		decodeData(t, out, &d)
		assertCompact(t, d.Report)
		_, out, _ = runMain("find_importers", "util/util.go", "--report")
		decodeData(t, out, &d)
		assertFull(t, d.Report)
	})
	t.Run("imports", func(t *testing.T) {
		_, out, _ := runMain("imports", "main.go")
		var d importsData
		decodeData(t, out, &d)
		assertCompact(t, d.Report)
		_, out, _ = runMain("imports", "main.go", "--report")
		decodeData(t, out, &d)
		assertFull(t, d.Report)
	})
	t.Run("dependency_graph", func(t *testing.T) {
		_, out, _ := runMain("dependency_graph")
		var d graphData
		decodeData(t, out, &d)
		assertCompact(t, d.Report)
		_, out, _ = runMain("dependency_graph", "--report")
		decodeData(t, out, &d)
		assertFull(t, d.Report)
	})
	t.Run("dependency_cycles", func(t *testing.T) {
		_, out, _ := runMain("dependency_cycles")
		var d cyclesData
		decodeData(t, out, &d)
		assertCompact(t, d.Report)
		_, out, _ = runMain("dependency_cycles", "--report")
		decodeData(t, out, &d)
		assertFull(t, d.Report)
	})
}
