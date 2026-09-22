package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

// --- blast_radius ---

// blastFiles is a small Go workspace: Target is called by Mid (same file) and by
// Use (package b) and TestUse (b's test), and package c imports b.
//
//	a/a.go        line 2 Target, line 3 Mid (calls Target)
//	b/b.go        imports a; line 4 Use (calls a.Target)
//	b/b_test.go   line 2 TestUse (calls a.Target)
//	c/c.go        imports b; line 4 C (calls b.Use)
var blastFiles = map[string]string{
	"a/a.go":      "package a\n\nfunc Target() {}\nfunc Mid() { Target() }\n",
	"b/b.go":      "package b\n\nimport \"fixture/a\"\n\nfunc Use() { a.Target() }\n",
	"b/b_test.go": "package b\n\nfunc TestUse() { a.Target() }\n",
	"c/c.go":      "package c\n\nimport \"fixture/b\"\n\nfunc C() { b.Use() }\n",
}

// blastCallScript is the canned call hierarchy for Target: Mid and Use call it
// directly, C calls Use.
func blastCallScript(dir string) func(*scenario) {
	a, b, c := filepath.Join(dir, "a/a.go"), filepath.Join(dir, "b/b.go"), filepath.Join(dir, "c/c.go")
	return func(sc *scenario) {
		sc.results = map[string]any{methodPrepareCallHierarchy: []any{callItemJSON("Target", 12, a, 2, 5, 11)}}
		sc.calls = map[string]any{
			"Target": map[string]any{"incoming": []any{
				incomingCall(callItemJSON("Mid", 12, a, 3, 5, 8), callRange(3, 13, 19)),
				incomingCall(callItemJSON("Use", 12, b, 4, 5, 8), callRange(4, 15, 21)),
			}},
			"Use": map[string]any{"incoming": []any{
				incomingCall(callItemJSON("C", 12, c, 4, 5, 6), callRange(4, 13, 16)),
			}},
		}
	}
}

// blastWorkspace builds the fixture. The call hierarchy needs the directory in
// its canned paths, so the workspace is made twice: once to learn where it is.
func blastWorkspace(t *testing.T) string {
	t.Helper()
	dir := tree(t, blastFiles)
	impactWorkspaceAt(t, dir, wordSpec{}, blastCallScript(dir))
	return dir
}

// impactWorkspaceAt is impactWorkspace over a directory that already exists.
func impactWorkspaceAt(t *testing.T, dir string, spec wordSpec, extra func(*scenario)) {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	sc := scenario{capabilities: impactCapabilities()}
	if extra != nil {
		extra(&sc)
	}
	sc.apply(t)
	raw, _ := json.Marshal(spec)
	t.Setenv(wordServerEnv, string(raw))
}

func decodeBlast(t *testing.T, stdout string) (blastRadiusData, rawEnvelope) {
	t.Helper()
	var d blastRadiusData
	env := decodeData(t, stdout, &d)
	return d, env
}

func rowsOf(d blastRadiusData, evidence string) []blastRow {
	var out []blastRow
	for _, r := range d.Rows {
		if r.Evidence == evidence {
			out = append(out, r)
		}
	}
	return out
}

func TestBlastRadiusOfASymbol(t *testing.T) {
	blastWorkspace(t)
	code, out, stderr := runMain("blast_radius", "--id", "a/a.go::Target#function", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, env := decodeBlast(t, out)
	if d.Kind != "symbol" || d.Subject.ID != "a/a.go::Target#function" || d.Subject.File != "a/a.go" {
		t.Errorf("subject = %+v (kind %s)", d.Subject, d.Kind)
	}

	// The summary counts everything, once.
	s := d.Summary
	if s.References != 3 || s.ReferencingSymbols != 3 {
		t.Errorf("references = %d in %d symbols, want 3 in 3: %+v", s.References, s.ReferencingSymbols, d.Rows)
	}
	if s.Callers != 3 || s.CallersTransitive != 1 {
		t.Errorf("callers = %d (%d transitive), want 3 (1)", s.Callers, s.CallersTransitive)
	}
	if s.Importers != 1 || s.ImportersTransitive != 1 {
		t.Errorf("importers = %d (+%d transitive), want 1 (+1): %+v", s.Importers, s.ImportersTransitive, rowsOf(d, blastImports))
	}
	if s.Files != 4 || s.Packages != 3 || s.TestFiles != 1 {
		t.Errorf("files/packages/tests = %d/%d/%d, want 4/3/1", s.Files, s.Packages, s.TestFiles)
	}
	if s.Rows != len(d.Rows) || d.Total != s.Rows || d.Truncated {
		t.Errorf("rows: summary %d, listed %d, total %d, truncated %v", s.Rows, len(d.Rows), d.Total, d.Truncated)
	}

	// Rows say where they came from, and references are grouped by the symbol
	// that contains them.
	refs := rowsOf(d, blastRefs)
	byID := map[string]blastRow{}
	for _, r := range refs {
		byID[r.ID] = r
	}
	for _, id := range []string{"a/a.go::Mid#function", "b/b.go::Use#function", "b/b_test.go::TestUse#function"} {
		if _, ok := byID[id]; !ok {
			t.Errorf("no reference row for %s in %+v", id, refs)
		}
	}
	if !byID["b/b_test.go::TestUse#function"].Test || byID["b/b.go::Use#function"].Test {
		t.Errorf("test flags wrong: %+v", refs)
	}
	calls := rowsOf(d, blastCalls)
	if len(calls) != 3 || calls[0].Depth != 1 || calls[2].Depth != 2 || calls[2].Name != "C" || calls[2].File != "c/c.go" {
		t.Errorf("call rows = %+v", calls)
	}
	imports := rowsOf(d, blastImports)
	if len(imports) != 2 || imports[0].File != "b/b.go" || imports[0].Line != 3 || imports[1].File != "c" || imports[1].Depth != 2 {
		t.Errorf("import rows = %+v", imports)
	}

	// Every source says how it fared, and a depth that could have cut is said.
	status := map[string]string{}
	for _, ev := range d.Evidence {
		status[ev.Source] = ev.Status
	}
	if status[blastRefs] != blastOK || status[blastCalls] != blastOK || status[blastImports] != blastOK {
		t.Errorf("evidence = %+v", d.Evidence)
	}
	if !envWarns(env, "depth limit") {
		t.Errorf("callers at the depth limit were not reported: %v", env.Warnings)
	}
}

func TestBlastRadiusBoundsAndLimitAreReported(t *testing.T) {
	blastWorkspace(t)
	// --depth 1 stops at the direct callers and direct importers, and says so.
	code, out, stderr := runMain("blast_radius", "--id", "a/a.go::Target#function", "--depth", "1", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, env := decodeBlast(t, out)
	if d.Summary.Callers != 2 || d.Summary.CallersTransitive != 0 || d.Summary.ImportersTransitive != 0 {
		t.Errorf("depth 1 summary = %+v", d.Summary)
	}
	if d.Bounds.CallDepth != 1 || d.Bounds.CallsAtLimit != 2 || !envWarns(env, "depth limit") {
		t.Errorf("bounds = %+v, warnings %v", d.Bounds, env.Warnings)
	}

	// --limit cuts the rows and not the summary, and shares the room.
	code, out, _ = runMain("blast_radius", "--id", "a/a.go::Target#function", "--limit", "3", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("limit: exit %d\n%s", code, out)
	}
	d, env = decodeBlast(t, out)
	if !d.Truncated || d.Count != 3 || len(d.Rows) != 3 || d.Total != 8 || d.Summary.Rows != 8 || d.Summary.References != 3 {
		t.Errorf("limited answer: count %d total %d truncated %v summary %+v", d.Count, d.Total, d.Truncated, d.Summary)
	}
	sources := map[string]bool{}
	for _, r := range d.Rows {
		sources[r.Evidence] = true
	}
	if len(sources) != 3 {
		t.Errorf("a limit of 3 shows only %v: one source starved the others", sources)
	}
	if !envWarns(env, "listed 3 of 8 rows") {
		t.Errorf("truncation not reported: %v", env.Warnings)
	}
}

// TestBlastRadiusOfATypeSurvivesTheCallHierarchyError: real gopls answers
// prepareCallHierarchy on a type with a JSON-RPC error, not an empty list. The
// references and importers must still come back, with the call hierarchy said
// to be not applicable.
func TestBlastRadiusOfATypeSurvivesTheCallHierarchyError(t *testing.T) {
	dir := tree(t, map[string]string{
		"g/g.go": "package g\n\ntype Gate struct{}\n\nfunc New() *Gate { return nil }\n",
		"h/h.go": "package h\n\nimport \"fixture/g\"\n\nfunc Use(x g.Gate) {}\n",
	})
	impactWorkspaceAt(t, dir, wordSpec{NotCallable: true}, nil)
	code, out, stderr := runMain("blast_radius", "--id", "g/g.go::Gate#struct", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, env := decodeBlast(t, out)
	if !env.OK {
		t.Fatalf("not ok: %s", out)
	}
	if d.Summary.References != 2 || len(rowsOf(d, blastRefs)) != 2 {
		t.Errorf("references lost: %+v rows %+v", d.Summary, d.Rows)
	}
	if len(rowsOf(d, blastImports)) == 0 {
		t.Errorf("importers lost: %+v", d.Rows)
	}
	var calls *blastEvidence
	for i := range d.Evidence {
		if d.Evidence[i].Source == blastCalls {
			calls = &d.Evidence[i]
		}
	}
	if calls == nil || calls.Status != blastNotApplicable || d.Summary.Callers != 0 {
		t.Errorf("call evidence = %+v, callers %d", calls, d.Summary.Callers)
	}
	if !envWarns(env, "not a function") {
		t.Errorf("the server's error is not reported: %v", env.Warnings)
	}
}

func TestBlastRadiusWithoutCallHierarchyIsSaidNotSilent(t *testing.T) {
	dir := tree(t, blastFiles)
	impactWorkspaceAt(t, dir, wordSpec{}, func(sc *scenario) {
		sc.capabilities = m5Capabilities(map[string]any{"callHierarchyProvider": nil})
	})
	code, out, stderr := runMain("blast_radius", "--id", "a/a.go::Target#function", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, env := decodeBlast(t, out)
	if d.Summary.References != 3 || d.Summary.Callers != 0 {
		t.Errorf("summary = %+v", d.Summary)
	}
	var calls blastEvidence
	for _, ev := range d.Evidence {
		if ev.Source == blastCalls {
			calls = ev
		}
	}
	if calls.Status != blastUnavailable || !envWarns(env, "call_hierarchy unavailable") {
		t.Errorf("call hierarchy evidence = %+v, warnings %v", calls, env.Warnings)
	}
}

func TestBlastRadiusOfAFile(t *testing.T) {
	blastWorkspace(t)
	code, out, stderr := runMain("blast_radius", "a/a.go", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, env := decodeBlast(t, out)
	if d.Kind != "file" || d.Subject.File != "a/a.go" || d.Summary.Importers != 1 || d.Summary.ImportersTransitive != 1 {
		t.Errorf("file blast radius = %+v", d)
	}
	notAnalysed := 0
	for _, ev := range d.Evidence {
		if ev.Status == blastNotAnalysed {
			notAnalysed++
		}
	}
	if notAnalysed != 2 || !envWarns(env, "not analysed") {
		t.Errorf("evidence = %+v, warnings %v", d.Evidence, env.Warnings)
	}
	// A path outside the workspace is refused, not read.
	if code, out, _ := runMain("blast_radius", "/etc/hostname"); code == ExitOK {
		t.Errorf("a file outside the workspace was analysed:\n%s", out)
	}
}

func TestBlastRadiusTextAndUsage(t *testing.T) {
	blastWorkspace(t)
	code, out, _ := runMain("blast_radius", "--id", "a/a.go::Target#function", "--format", "text", "--settle", "20ms")
	if code != ExitOK || !strings.Contains(out, "summary: 3 references in 3 symbols; 3 callers (1 beyond direct)") ||
		!strings.Contains(out, "b/b_test.go:3: references function TestUse [test]") {
		t.Errorf("text (exit %d):\n%s", code, out)
	}
	for _, args := range [][]string{
		{"blast_radius"},
		{"blast_radius", "a/a.go", "--depth", "0"},
		{"blast_radius", "a/a.go", "--import-depth", "9"},
	} {
		if code, out, _ := runMain(args...); code != ExitUsage {
			t.Errorf("%v: exit %d, want usage\n%s", args, code, out)
		}
	}
}

func TestBlastRadiusWhileIndexingIsNotReady(t *testing.T) {
	dir := tree(t, blastFiles)
	impactWorkspaceAt(t, dir, wordSpec{}, func(sc *scenario) { sc.indexing = true })
	code, out, _ := runMain("blast_radius", "--id", "a/a.go::Target#function", "--timeout", "1s", "--settle", "20ms")
	if code != ExitNotReady {
		t.Errorf("exit %d, want %d (an empty answer from a server that is still indexing is not an answer)\n%s", code, ExitNotReady, out)
	}
}

func TestBlastFairCapSharesTheRoom(t *testing.T) {
	var rows []blastRow
	for i := 0; i < 10; i++ {
		rows = append(rows, blastRow{Evidence: blastRefs, File: "r"})
	}
	rows = append(rows, blastRow{Evidence: blastCalls, File: "c"}, blastRow{Evidence: blastImports, File: "i"})
	got := blastFairCap(rows, 4)
	count := map[string]int{}
	for _, r := range got {
		count[r.Evidence]++
	}
	if len(got) != 4 || count[blastCalls] != 1 || count[blastImports] != 1 || count[blastRefs] != 2 {
		t.Errorf("fair cap = %v", count)
	}
	if len(blastFairCap(rows, 0)) != 12 || len(blastFairCap(rows, 12)) != 12 {
		t.Error("no cut expected at limit 0 or at the size")
	}
	if blastLabelDepth("<- x") != 1 || blastLabelDepth("    <- x") != 3 {
		t.Error("label depth")
	}
}

// --- dead_code ---

var deadFiles = map[string]string{
	"main.go": "package main\n\nfunc main() {}\nfunc init() {}\n",
	"lib/lib.go": "package lib\n\n" +
		"func Used() { usedPriv() }\n" + // 3
		"func usedPriv() {}\n" + // 4
		"func dead1() {}\n" + // 5
		"func dead2() {}\n" + // 6
		"func dead3() {}\n" + // 7
		"type box struct{}\n" + // 8
		"func (b *box) draw() {}\n" + // 9
		"func (b *box) unusedM() {}\n" + // 10
		"func (b *box) String() string { return \"\" }\n", // 11
	"lib/lib_test.go": "package lib\n\nfunc TestDead1() { dead1() }\n",
}

func deadWorkspace(t *testing.T, extra func(*scenario)) string {
	t.Helper()
	dir := tree(t, deadFiles)
	impactWorkspaceAt(t, dir, wordSpec{Implements: []string{"draw"}}, extra)
	return dir
}

func decodeDead(t *testing.T, stdout string) (deadCodeData, rawEnvelope) {
	t.Helper()
	var d deadCodeData
	env := decodeData(t, stdout, &d)
	return d, env
}

func candidateNames(d deadCodeData) []string {
	var names []string
	for _, c := range d.Candidates {
		names = append(names, c.Name)
	}
	return names
}

func TestDeadCodeFindsCandidatesAndSparesTheRest(t *testing.T) {
	deadWorkspace(t, nil)
	code, out, stderr := runMain("dead_code", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit %d, want %d (candidates found)\n%s\n%s", code, ExitProblems, out, stderr)
	}
	d, env := decodeDead(t, out)
	// dead2, dead3 and unusedM have no reference at all. dead1 is used by a
	// test, usedPriv by Used, box by its methods, draw implements an interface;
	// main and init are entry points, String is on the ignore list, Used is
	// exported and TestDead1 is a test.
	if got := candidateNames(d); !slices.Equal(got, []string{"dead2", "dead3", "box.unusedM"}) {
		t.Errorf("candidates = %v", got)
	}
	want := map[string]int{deadTestFile: 1, deadEntryPoint: 2, deadIgnored: 1, deadExported: 1, deadImplements: 1}
	for reason, n := range want {
		if d.Excluded[reason] != n {
			t.Errorf("excluded[%s] = %d, want %d (all: %v)", reason, d.Excluded[reason], n, d.Excluded)
		}
	}
	if d.Eligible != 7 || d.Examined != 7 || d.Used != 3 || !d.Complete || d.NextCursor != "" || d.Symbols != 12 {
		t.Errorf("counts: eligible %d examined %d used %d complete %v cursor %q symbols %d",
			d.Eligible, d.Examined, d.Used, d.Complete, d.NextCursor, d.Symbols)
	}
	if d.Queries.References != 7 || d.Queries.Implementation != 2 {
		t.Errorf("queries = %+v, want 7 reference queries and an implementation query for each method with none", d.Queries)
	}
	byName := map[string]deadCandidate{}
	for _, c := range d.Candidates {
		byName[c.Name] = c
	}
	if c := byName["dead2"]; c.Confidence != "high" || c.Exported != "no" || c.ID != "lib/lib.go::dead2#function" || c.Line != 6 || len(c.Reasons) != 0 {
		t.Errorf("dead2 = %+v", c)
	}
	if c := byName["box.unusedM"]; c.Confidence != "medium" || c.Kind != "method" || len(c.Reasons) != 1 || !strings.Contains(c.Reasons[0], "interface") {
		t.Errorf("unusedM = %+v", c)
	}
	if !envWarns(env, "candidates, not verdicts") {
		t.Errorf("the standing warning is missing: %v", env.Warnings)
	}
}

func TestDeadCodeBudgetAndCursorResume(t *testing.T) {
	deadWorkspace(t, nil)
	var (
		seen   []string
		cursor string
		rounds int
	)
	for {
		rounds++
		args := []string{"dead_code", "--budget", "2", "--settle", "20ms"}
		if cursor != "" {
			args = append(args, "--cursor", cursor)
		}
		_, out, stderr := runMain(args...)
		d, env := decodeDead(t, out)
		if !env.OK {
			t.Fatalf("round %d: %s\n%s", rounds, out, stderr)
		}
		if d.Queries.References > 2 {
			t.Errorf("round %d made %d reference queries on a budget of 2", rounds, d.Queries.References)
		}
		seen = append(seen, candidateNames(d)...)
		if d.Complete {
			if d.NextCursor != "" {
				t.Errorf("a complete answer carries a cursor %q", d.NextCursor)
			}
			break
		}
		if d.NextCursor == "" || !envWarns(env, "--cursor") || d.Examined != 2 {
			t.Fatalf("round %d: examined %d, cursor %q, warnings %v", rounds, d.Examined, d.NextCursor, env.Warnings)
		}
		cursor = d.NextCursor
		if rounds > 10 {
			t.Fatal("the cursor does not advance")
		}
	}
	if rounds != 4 || !slices.Equal(seen, []string{"dead2", "dead3", "box.unusedM"}) {
		t.Errorf("%d rounds found %v; want the same three candidates, once each, in 4 slices of 2 out of 7", rounds, seen)
	}

	for _, bad := range []string{"nonsense", "x|lib/lib.go::a#function", "5|noseparator"} {
		if code, _, _ := runMain("dead_code", "--cursor", bad); code != ExitUsage {
			t.Errorf("--cursor %q: exit %d, want usage", bad, code)
		}
	}
}

func TestDeadCodeIncludeExportedScopeKindAndLimit(t *testing.T) {
	deadWorkspace(t, nil)
	_, out, _ := runMain("dead_code", "--include-exported", "--kind", "function", "--settle", "20ms")
	d, _ := decodeDead(t, out)
	var used deadCandidate
	for _, c := range d.Candidates {
		if c.Name == "Used" {
			used = c
		}
	}
	if used.Exported != "yes" || used.Confidence != "low" || !strings.Contains(strings.Join(used.Reasons, ";"), "public API") {
		t.Errorf("exported candidate = %+v (all %v)", used, candidateNames(d))
	}
	if slices.Contains(candidateNames(d), "box.unusedM") {
		t.Error("--kind function still examined a method")
	}

	// A scope: the positional path and --path are the same thing.
	_, out, _ = runMain("dead_code", "main.go", "--settle", "20ms")
	if d, _ := decodeDead(t, out); d.Scope != "main.go" || d.Symbols != 2 || len(d.Candidates) != 0 {
		t.Errorf("scoped answer = %+v", d)
	}
	if code, _, _ := runMain("dead_code", "lib", "--path", "main.go"); code != ExitUsage {
		t.Errorf("two different scopes: exit %d, want usage", code)
	}
	if code, _, _ := runMain("dead_code", "../elsewhere"); code == ExitOK || code == ExitProblems {
		t.Errorf("a scope outside the workspace: exit %d", code)
	}

	// --limit cuts the rows, and the total says how many there were.
	_, out, _ = runMain("dead_code", "--limit", "1", "--settle", "20ms")
	d, env := decodeDead(t, out)
	if d.Count != 1 || d.Total != 3 || !d.Truncated || !envWarns(env, "listed 1 of 3 candidates") {
		t.Errorf("limited answer: count %d total %d truncated %v warnings %v", d.Count, d.Total, d.Truncated, env.Warnings)
	}
	if code, _, _ := runMain("dead_code", "--kind", "nonsense"); code != ExitUsage {
		t.Errorf("unknown kind: exit %d, want usage", code)
	}
}

func TestDeadCodeIgnoreListFromTheWorkspaceConfig(t *testing.T) {
	dir := deadWorkspace(t, nil)
	write(t, filepath.Join(dir, ".lightspeed.toml"),
		"schema_version = 1\n\n[dead_code]\nignore_names = [\"dead2\", \"*.unusedM\"]\nignore_paths = [\"nothing/**\"]\n")
	code, out, stderr := runMain("dead_code", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit %d\n%s\n%s", code, out, stderr)
	}
	d, _ := decodeDead(t, out)
	if got := candidateNames(d); !slices.Equal(got, []string{"dead3"}) || d.Excluded[deadIgnored] != 3 {
		t.Errorf("candidates %v, excluded %v; want dead2 and unusedM ignored by the config, String by the defaults", got, d.Excluded)
	}

	// A path glob ignores a whole file.
	write(t, filepath.Join(dir, ".lightspeed.toml"), "schema_version = 1\n[dead_code]\nignore_paths = [\"lib/**\"]\n")
	_, out, _ = runMain("dead_code", "--settle", "20ms")
	if d, _ := decodeDead(t, out); len(d.Candidates) != 0 || d.Excluded[deadIgnored] < 8 {
		t.Errorf("ignore_paths: %v, excluded %v", candidateNames(d), d.Excluded)
	}

	// A typo is an error, not an ignore that does nothing.
	write(t, filepath.Join(dir, ".lightspeed.toml"), "schema_version = 1\n[dead_code]\nignore_name = [\"x\"]\n")
	code, out, _ = runMain("dead_code", "--settle", "20ms")
	env := decodeEnvelope(t, out)
	if code == ExitOK || code == ExitProblems || env.OK {
		t.Errorf("a malformed [dead_code] table: exit %d, %s", code, out)
	}
}

func TestDeadCodeWhileIndexingIsNotReady(t *testing.T) {
	deadWorkspace(t, func(sc *scenario) { sc.indexing = true })
	code, out, _ := runMain("dead_code", "--timeout", "1s", "--settle", "20ms")
	if code != ExitNotReady {
		t.Errorf("exit %d, want %d: an empty candidate list from a server still indexing would send someone to delete live code\n%s", code, ExitNotReady, out)
	}
}

func TestDeadCodeAndBlastRadiusTextFormat(t *testing.T) {
	deadWorkspace(t, nil)
	code, out, _ := runMain("dead_code", "--format", "text", "--settle", "20ms")
	if code != ExitProblems || !strings.Contains(out, "lib/lib.go:6: function dead2 [high]") ||
		!strings.Contains(out, "lib/lib.go:10: method box.unusedM [medium]") || !strings.Contains(out, "# candidates, not verdicts") {
		t.Errorf("text (exit %d):\n%s", code, out)
	}
}

// --- the MCP tools ---

func TestMCPImpactTools(t *testing.T) {
	dir := blastWorkspaceForMCP(t)
	cs := mcpSession(t, dir)

	res := callTool(t, cs, "blast_radius", map[string]any{"id": "a/a.go::Target#function", "depth": 1})
	var b blastRadiusData
	decodeData(t, resultText(t, res), &b)
	if res.IsError || b.Summary.References != 3 || b.Summary.Callers != 2 {
		t.Errorf("blast_radius: %+v", b.Summary)
	}
	res = callTool(t, cs, "blast_radius", map[string]any{"location": "a/a.go"})
	var bf blastRadiusData
	decodeData(t, resultText(t, res), &bf)
	if res.IsError || bf.Kind != "file" || bf.Summary.Importers != 1 {
		t.Errorf("blast_radius of a file: %+v", bf)
	}

	// dead_code needs its own fixture.
	dir = deadWorkspace(t, nil)
	cs = mcpSession(t, dir)
	res = callTool(t, cs, "dead_code", map[string]any{"budget": 3, "kind": []string{"function"}})
	var d deadCodeData
	decodeData(t, resultText(t, res), &d)
	if res.IsError || resultExit(t, res) != ExitProblems || d.Examined != 3 || d.Complete || d.NextCursor == "" {
		t.Errorf("dead_code: exit %d, %+v", resultExit(t, res), d)
	}
	res = callTool(t, cs, "dead_code", map[string]any{"dir": "main.go"})
	var ds deadCodeData
	decodeData(t, resultText(t, res), &ds)
	if res.IsError || ds.Scope != "main.go" {
		t.Errorf("dead_code scope: %+v", ds)
	}
}

// blastWorkspaceForMCP is blastWorkspace for a test that will also build a
// second workspace.
func blastWorkspaceForMCP(t *testing.T) string { return blastWorkspace(t) }

// --- unit tests of the rules ---

// isym is an index symbol as the rules see one.
func isym(name, kind string) index.Symbol {
	return index.Symbol{Name: name, Qualified: name, Kind: kind}
}

func TestDeadExportRules(t *testing.T) {
	sym := isym
	for _, tc := range []struct {
		lang, decl string
		sym        index.Symbol
		want       string
	}{
		{"go", "", sym("Handle", "function"), "yes"},
		{"go", "", sym("handle", "function"), "no"},
		{"go", "", sym("(*T).Handle", "method"), "yes"},
		{"python", "", sym("_helper", "function"), "no"},
		{"python", "", sym("helper", "function"), "unknown"},
		{"typescript", "export function f() {}", sym("f", "function"), "yes"},
		{"typescript", "function f() {}", sym("f", "function"), "no"},
		{"typescript", "  private g() {}", sym("g", "method"), "no"},
		{"typescript", "  g() {}", sym("g", "method"), "unknown"},
		{"rust", "pub fn f() {}", sym("f", "function"), "yes"},
		{"rust", "pub(crate) fn f() {}", sym("f", "function"), "no"},
		{"rust", "fn f() {}", sym("f", "function"), "no"},
		{"c", "static int f(void)", sym("f", "function"), "no"},
		{"c", "int f(void)", sym("f", "function"), "unknown"},
		{"lua", "local function f()", sym("f", "function"), "no"},
		{"java", "public void f()", sym("f", "method"), "unknown"},
	} {
		if got := deadExport(tc.lang, tc.sym, tc.decl); got != tc.want {
			t.Errorf("deadExport(%s, %q, %q) = %s, want %s", tc.lang, tc.sym.Qualified, tc.decl, got, tc.want)
		}
	}
}

func TestDeadEntryPointsAndBuildSensitivity(t *testing.T) {
	for _, tc := range []struct {
		lang, name, kind string
		want             bool
	}{
		{"go", "main", "function", true}, {"go", "init", "function", true}, {"go", "TestX", "function", true},
		{"go", "TestMain", "function", true}, {"go", "BenchmarkX", "function", true}, {"go", "ExampleX", "function", true},
		{"go", "FuzzX", "function", true}, {"go", "Testing", "function", false}, {"go", "Testify", "function", false},
		{"go", "helper", "function", false}, {"python", "__init__", "method", true}, {"python", "init_all", "function", false},
	} {
		if got := isDeadEntryPoint(tc.lang, isym(tc.name, tc.kind)); got != tc.want {
			t.Errorf("isDeadEntryPoint(%s, %s) = %v, want %v", tc.lang, tc.name, got, tc.want)
		}
	}
	for _, tc := range []struct {
		rel, text string
		want      bool
	}{
		{"a.go", "package a\n", false},
		{"a_linux.go", "package a\n", true},
		{"a_test.go", "package a\n", false},
		{"a.go", "//go:build windows\n\npackage a\n", true},
		{"a.go", "// +build ignore\n\npackage a\n", true},
		{"a.go", "package a\n\n// go:build in a comment after package\n", false},
		{"a.go", "package a\n\nimport \"C\"\n", true},
	} {
		if got := goBuildSensitive(tc.rel, tc.text); got != tc.want {
			t.Errorf("goBuildSensitive(%s, %q) = %v, want %v", tc.rel, tc.text, got, tc.want)
		}
	}
}

// --- --limit, for the table-driven enforcement test ---

func init() {
	registerLimitCase("blast_radius", limitCase{
		Rows: "rows",
		Invoke: func(t *testing.T, extra ...string) string {
			blastWorkspace(t)
			args := append([]string{"blast_radius", "--id", "a/a.go::Target#function", "--settle", "20ms"}, extra...)
			_, out, _ := runMain(args...)
			return out
		},
	})
	registerLimitCase("dead_code", limitCase{
		Rows: "candidates",
		Invoke: func(t *testing.T, extra ...string) string {
			dir := tree(t, map[string]string{
				"lib/lib.go": "package lib\n\nfunc a() {}\nfunc b() {}\nfunc c() {}\nfunc d() {}\nfunc e() {}\n",
			})
			impactWorkspaceAt(t, dir, wordSpec{}, nil)
			args := append([]string{"dead_code", "--settle", "20ms"}, extra...)
			_, out, _ := runMain(args...)
			return out
		},
	})
}
