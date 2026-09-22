package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// task_context, hermetically: the text server outlines the fixture files from
// their text (so the index is what is on disk), the scripted call graph answers
// the call hierarchy, and every expectation below is a property of the rule in
// docs/DECISIONS.md D41, not of a ranking that happened to come out.

var taskFixture = map[string]string{
	"importers.go": "package a\n\n// FindImporters lists the files that import a package.\nfunc FindImporters() {\n\tcaller()\n}\n\nfunc findImportersCache() {}\n\nfunc Unrelated() {}\n",
	"caller.go":    "package a\n\nfunc Caller() {\n\tFindImporters()\n}\n",
	"limit.go":     "package a\n\nfunc ApplyLimit() {}\n\nfunc LimitRows() {}\n\nfunc limit() {}\n",
	"sub/graph.go": "package sub\n\nfunc BuildGraph() {}\n\nfunc WalkGraph() {}\n",
	"a_test.go":    "package a\n\nfunc TestFindImporters() {}\n",
}

type taskOut struct {
	code int
	data taskContextData
	env  rawEnvelope
	raw  string
}

func runTask(t *testing.T, args ...string) taskOut {
	t.Helper()
	code, stdout, stderr := runMain(append([]string{"task_context"}, args...)...)
	out := taskOut{code: code, raw: stdout}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		out.env = decodeData(t, stdout, nil)
		if out.env.OK {
			decodeData(t, stdout, &out.data)
		}
	} else if code == 0 || code == 1 {
		t.Fatalf("no envelope: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return out
}

func taskWorkspace(t *testing.T) string {
	t.Helper()
	dir := idxWorkspace(t, taskFixture)
	// The call graph of FindImporters: Caller calls it, it calls caller().
	file := filepath.Join(dir, "importers.go")
	scenario{
		capabilities: m5Capabilities(nil),
		textOutline:  true,
		results:      map[string]any{methodPrepareCallHierarchy: []any{callItemJSON("FindImporters", 12, file, 3, 5, 18)}},
		calls: map[string]any{
			"FindImporters": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("Caller", 12, filepath.Join(dir, "caller.go"), 2, 5, 11), callRange(3, 1, 14))},
				"outgoing": []any{outgoingCall(callItemJSON("helper", 12, filepath.Join(dir, "limit.go"), 6, 5, 11), callRange(4, 1, 7))},
			},
		},
	}.apply(t)
	return dir
}

func TestTaskContextNamingASymbolIsHighWithSourceAndNeighbours(t *testing.T) {
	taskWorkspace(t)
	// --rule and --with-source restore what the default JSON answer trims
	// (the verdict sentence, full related-row detail, inlined source; D46) —
	// this test exercises all three.
	out := runTask(t, "callers of FindImporters are wrong", "--rule", "--with-source", "1")
	if out.code != ExitOK {
		t.Fatalf("exit %d\n%s", out.code, out.raw)
	}
	d := out.data
	if d.Confidence != confHigh {
		t.Fatalf("confidence = %s (%s), want high", d.Confidence, d.Reason)
	}
	if d.Rule == "" || d.Reason == "" || !strings.HasPrefix(d.Verdict, "found:") {
		t.Errorf("a confidence must come with its rule and reason: %+v", d)
	}
	if len(d.Symbols) == 0 {
		t.Fatalf("no symbols: %+v", d)
	}
	top := d.Symbols[0]
	if top.ID != "importers.go::FindImporters#function" || top.File != "importers.go" || top.Line != 4 {
		t.Errorf("top symbol = %+v", top)
	}
	if !slices.Contains(top.Evidence, "match:exact:FindImporters") {
		t.Errorf("evidence = %v", top.Evidence)
	}
	if top.Source == nil || !strings.Contains(top.Source.Source, "func FindImporters()") ||
		!strings.HasPrefix(top.Source.Source, "// FindImporters lists") {
		t.Errorf("the best symbol's source (doc comment included) must be in the capsule: %+v", top.Source)
	}
	// Neighbours, each with its evidence.
	var caller *taskRelated
	for i := range d.Related {
		if d.Related[i].Relation == relCaller && d.Related[i].Of == top.ID && caller == nil {
			caller = &d.Related[i]
		}
	}
	if caller == nil || caller.Name != "Caller" || caller.File != "caller.go" || caller.Line != 3 ||
		caller.ID != "caller.go::Caller#function" || caller.Evidence != "call_hierarchy" || caller.Of != top.ID {
		t.Fatalf("the caller row = %+v (all: %+v)", caller, d.Related)
	}
	if top.CallersTotal == nil || *top.CallersTotal != 1 || top.CalleesTotal == nil || *top.CalleesTotal != 1 {
		t.Errorf("call totals = %v/%v", top.CallersTotal, top.CalleesTotal)
	}
	if !slices.ContainsFunc(d.Related, func(r taskRelated) bool { return r.Relation == relSibling && r.Evidence == "outline" }) {
		t.Errorf("no same-file sibling: %+v", d.Related)
	}
	fileWhy := map[string][]string{}
	for _, f := range d.Files {
		fileWhy[f.File] = f.Why
	}
	if !slices.Contains(fileWhy["importers.go"], "symbol") || !slices.Contains(fileWhy["caller.go"], relCaller) {
		t.Errorf("files = %+v", d.Files)
	}
	// The term shows what became of it.
	if d.Terms[0].Term != "FindImporters" || d.Terms[0].Best != "exact" || d.Terms[0].Matches == 0 {
		t.Errorf("terms = %+v", d.Terms)
	}
	if d.Budget.Limit != defaultTaskBudget || d.Budget.Used == 0 || d.Budget.Used > d.Budget.Limit || d.Budget.Truncated {
		t.Errorf("budget = %+v", d.Budget)
	}
}

// Two symbols named the same way is not a decisive answer.
func TestTaskContextTwoExactMatchesIsNotHigh(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"a.go": "package a\n\nfunc Render() {}\n",
		"b.go": "package b\n\nfunc Render() {}\n",
	})
	out := runTask(t, "`Render` is slow", "--expand", "0")
	if out.data.Confidence != confMedium || len(out.data.Symbols) < 2 {
		t.Fatalf("confidence = %s, symbols = %+v; want medium with both", out.data.Confidence, out.data.Symbols)
	}
	if !strings.Contains(out.data.Reason, "not clearly ahead") {
		t.Errorf("reason = %q", out.data.Reason)
	}
}

// A plain word that happens to be a symbol's name is weak evidence.
func TestTaskContextAnExactPlainWordIsMedium(t *testing.T) {
	taskWorkspace(t)
	out := runTask(t, "how does the limit work", "--expand", "0", "--rule")
	d := out.data
	if d.Confidence != confMedium || out.code != ExitOK {
		t.Fatalf("confidence = %s (%s), exit %d; want medium", d.Confidence, d.Reason, out.code)
	}
	if d.Symbols[0].Name != "limit" || !strings.HasPrefix(d.Verdict, "candidates found") {
		t.Errorf("symbols = %+v, verdict = %q", d.Symbols, d.Verdict)
	}
	if len(d.Related) != 0 {
		t.Errorf("--expand 0 must expand nothing: %+v", d.Related)
	}
}

// A generic word matching an unrelated symbol does not carry a task whose
// distinctive term matches nothing: the shape of "oauth token refresh" against a
// codebase with a `token` type, a tokenizer and no OAuth.
func TestTaskContextUnmatchedDistinctiveTermIsLow(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"lex.go":     "package a\n\ntype token struct{}\n\ntype tokenKind int\n\nfunc tokenKey() string { return \"\" }\n",
		"tokens.go":  "package a\n\nfunc Tokenize() {}\n\nconst MatchToken = 1\n",
		"refresh.go": "package a\n\nfunc refreshView() {}\n",
	})
	for _, task := range []string{"oauth token refresh", "how does the limit handling work"} {
		out := runTask(t, task, "--rule")
		d := out.data
		if out.code != ExitProblems || d.Confidence != confLow || len(d.Symbols) != 0 {
			t.Errorf("%q: exit %d, confidence %s (%s), %d symbols; want 1, low, none", task, out.code, d.Confidence, d.Reason, len(d.Symbols))
		}
		if !strings.Contains(d.Verdict, "probably not implemented here") {
			t.Errorf("%q: verdict = %q", task, d.Verdict)
		}
	}
	out := runTask(t, "oauth token refresh")
	if !strings.Contains(out.data.Reason, "oauth matches no symbol") {
		t.Errorf("the reason names the term that matched nothing: %q", out.data.Reason)
	}
}

// A plain-English task that names a feature which exists is high: the type
// matches both words and the accessor that shares its name is not a rival.
func TestTaskContextPlainWordsCoveredByOneSymbolAreHigh(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"gate.go":    "package a\n\n// Gate is the readiness gate.\nfunc Gate() {}\n\nfunc NewGate() {}\n",
		"session.go": "package a\n\n// Gate returns the readiness gate.\nfunc (s *Session) Gate() {}\n",
		"other.go":   "package a\n\nfunc gateKey() {}\n",
	})
	out := runTask(t, "readiness gate", "--expand", "0")
	d := out.data
	if d.Confidence != confHigh || out.code != ExitOK {
		t.Fatalf("confidence = %s (%s), exit %d; want high", d.Confidence, d.Reason, out.code)
	}
	if len(d.Symbols) == 0 || d.Symbols[0].ID != "gate.go::Gate#function" {
		t.Errorf("the declaration must lead its accessor: %+v", d.Symbols)
	}
}

// Low makes an agent write the feature anew, so a feature that exists must not
// come out low because it was described in words. A compound name holds two of
// the task's words (ReapIdle = reap + idle) while the generic ones
// ("language", "servers") match nothing or something unrelated; the symbol must
// be found, lead its own test, and the unmatched part must be named.
func TestTaskContextCompoundNameIsNotLow(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"pool.go":      "package a\n\n// ReapIdle stops what nobody has used for a while.\nfunc ReapIdle() {}\n",
		"proc.go":      "package a\n\nfunc reap() {}\n",
		"report.go":    "package a\n\nfunc Servers() {}\n",
		"pool_test.go": "package a\n\nfunc TestPoolReapIdleLeavesOtherServersAlone() {}\n",
	})
	out := runTask(t, "reap idle language servers", "--expand", "0", "--rule")
	d := out.data
	if d.Confidence != confMedium || out.code != ExitOK {
		t.Fatalf("confidence = %s (%s), exit %d; want medium", d.Confidence, d.Reason, out.code)
	}
	if len(d.Symbols) == 0 || d.Symbols[0].ID != "pool.go::ReapIdle#function" {
		t.Fatalf("the compound name must lead, ahead of its test and of the one-word matches: %+v", d.Symbols)
	}
	if ev := d.Symbols[0].Evidence; !slices.Contains(ev, "match:prefix:reap") || !slices.Contains(ev, "match:name_token:idle") {
		t.Errorf("evidence = %v", ev)
	}
	if !strings.Contains(d.Reason, "ReapIdle matches 2 of the task's 4 terms") || !strings.Contains(d.Reason, "language matches no symbol") {
		t.Errorf("the reason names the compound hit and the term that matched nothing: %q", d.Reason)
	}
	// A word of the name and a word of the doc comment are a compound hit too.
	taskWorkspace(t)
	if out := runTask(t, "improve the importers list", "--expand", "0", "--rule"); out.data.Confidence != confMedium ||
		out.data.Symbols[0].Name != "FindImporters" {
		t.Errorf("confidence = %s (%s), symbols = %+v; want medium led by FindImporters", out.data.Confidence, out.data.Reason, out.data.Symbols)
	}
}

// One symbol named exactly like one of a task's two terms accounts for half of
// it: that is a candidate to read, not an absence ("test-only importers" against
// a function Importers).
func TestTaskContextHalfCoveredByOneSymbolIsNotLow(t *testing.T) {
	idxWorkspace(t, map[string]string{
		"queries.go": "package a\n\nfunc Importers() {}\n",
		"other.go":   "package a\n\nfunc Unrelated() {}\n",
	})
	out := runTask(t, "test-only importers", "--expand", "0")
	d := out.data
	if d.Confidence != confMedium || out.code != ExitOK {
		t.Fatalf("confidence = %s (%s), exit %d; want medium", d.Confidence, d.Reason, out.code)
	}
	if len(d.Symbols) == 0 || d.Symbols[0].ID != "queries.go::Importers#function" {
		t.Errorf("symbols = %+v", d.Symbols)
	}
	if !strings.Contains(d.Reason, "50%") || !strings.Contains(d.Reason, "test matches no symbol") {
		t.Errorf("reason = %q", d.Reason)
	}
}

// The honest answer to a task about something that is not there is not a list
// of the least bad matches.
func TestTaskContextAbsentIsLowAndPadsNothing(t *testing.T) {
	taskWorkspace(t)
	for _, task := range []string{
		"add a kubernetes operator",
		// One term is a word of two symbols' names and the other matches
		// nothing: under half of the task, and no symbol speaks for two terms.
		"improve the importers dashboard",
	} {
		out := runTask(t, task, "--rule")
		d := out.data
		if out.code != ExitProblems || d.Confidence != confLow {
			t.Errorf("%q: exit %d, confidence %s (%s); want 1 and low", task, out.code, d.Confidence, d.Reason)
		}
		if !strings.Contains(d.Verdict, "probably not implemented here") {
			t.Errorf("%q: verdict = %q", task, d.Verdict)
		}
		if len(d.Symbols) != 0 || d.Count != 0 || len(d.Related) != 0 || len(d.Files) != 0 || slices.ContainsFunc(d.Symbols, func(taskSymbol) bool { return true }) {
			t.Errorf("%q: a low-confidence answer padded with %+v", task, d)
		}
		if d.Rule == "" {
			t.Errorf("%q: no rule", task)
		}
	}
	out := runTask(t, "improve the importers dashboard")
	if out.data.Withheld == 0 || !slices.Contains(out.data.Nearest, "FindImporters") {
		t.Errorf("the weak matches are counted and named as a hint: withheld %d, nearest %v", out.data.Withheld, out.data.Nearest)
	}
	// The text form says it in words too.
	code, text, _ := runMain("task_context", "add a kubernetes operator", "--format", "text")
	if code != ExitProblems || !strings.Contains(text, "probably not implemented here") || !strings.Contains(text, "rule:") {
		t.Errorf("text form: exit %d\n%s", code, text)
	}
}

func TestTaskContextBudgetBitesAndSaysSo(t *testing.T) {
	files := map[string]string{}
	for _, n := range []string{"A", "B", "C", "D", "E", "F", "G", "H"} {
		files[strings.ToLower(n)+".go"] = "package a\n\n// FindThing" + n + " does one of eight things and its comment is long enough to cost tokens.\nfunc FindThing" + n + "() {\n\t// body\n}\n"
	}
	idxWorkspace(t, files)
	full := runTask(t, "FindThing", "--expand", "0")
	if full.data.Budget.Truncated || len(full.data.Symbols) != 8 {
		t.Fatalf("with the default budget everything fits: %+v", full.data.Budget)
	}
	out := runTask(t, "FindThing", "--expand", "0", "--budget", "650")
	d := out.data
	if !d.Budget.Truncated || d.Budget.Dropped["symbols"] == 0 || len(d.Symbols) >= 8 || !d.Truncated {
		t.Errorf("a small budget must drop symbols and say so: %+v, %d symbols", d.Budget, len(d.Symbols))
	}
	if d.Total != 8 || !envWarns(out.env, "budget bit") {
		t.Errorf("total %d, warnings %v", d.Total, out.env.Warnings)
	}
	// Source is the first thing to be cut to fit, and is cut, not silently
	// shortened.
	// (The scripted server draws a symbol on one line, so the long body is a
	// long parameter list; the signature is clipped and the source is not.)
	long := strings.Repeat("aVeryLongParameterName int, ", 250)
	idxWorkspace(t, map[string]string{"big.go": "package a\n\nfunc BigThing(" + long + ") {}\n"})
	out = runTask(t, "BigThing", "--expand", "0", "--with-source", "1", "--budget", "1200")
	src := out.data.Symbols[0].Source
	if src == nil || !src.Truncated || !out.data.Budget.Truncated || out.data.Budget.Dropped["source_bytes"] != 1 || out.data.Budget.Used > 1300 {
		t.Errorf("a long source must be cut to the budget and marked: %+v / %+v", src, out.data.Budget)
	}
	if sig := out.data.Symbols[0].Signature; len(sig) > maxSignatureBytes+len("…") {
		t.Errorf("signature not clipped: %d bytes", len(sig))
	}
}

func TestTaskContextFileTermsBoostAndList(t *testing.T) {
	taskWorkspace(t)
	out := runTask(t, "something is off in sub/graph.go", "--expand", "0", "--rule")
	d := out.data
	if d.Confidence != confMedium || len(d.Symbols) != 2 {
		t.Fatalf("confidence %s, symbols %+v", d.Confidence, d.Symbols)
	}
	for _, s := range d.Symbols {
		if s.File != "sub/graph.go" || !slices.Contains(s.Evidence, "in_file:sub/graph.go") {
			t.Errorf("symbol %+v", s)
		}
	}
	if !strings.Contains(d.Reason, "in a file the task names") {
		t.Errorf("reason = %q", d.Reason)
	}
	// A file that is not there is said, not guessed at.
	out = runTask(t, "look at nowhere/missing.go", "--expand", "0")
	if out.data.Confidence != confLow || !envWarns(out.env, "is not in the workspace") {
		t.Errorf("missing file: %s, warnings %v", out.data.Confidence, out.env.Warnings)
	}
	// A bare file name resolves by suffix, and the name of a file boosts the
	// names in it: BuildGraph is in graph.go and matches the word `graph`.
	out = runTask(t, "graph.go BuildGraph", "--expand", "0", "--rule")
	if out.data.Symbols[0].Name != "BuildGraph" || out.data.Confidence != confHigh {
		t.Errorf("suffix path: %s %+v", out.data.Confidence, out.data.Symbols)
	}
}

func TestTaskContextTestFilesAreDemotedUnlessTheTaskIsAboutTests(t *testing.T) {
	taskWorkspace(t)
	out := runTask(t, "TestFindImporters", "--expand", "0", "--rule")
	if out.data.Symbols[0].File != "a_test.go" {
		t.Errorf("an exact match on a test name is still the best match: %+v", out.data.Symbols)
	}
	out = runTask(t, "FindImporters", "--expand", "0", "--rule")
	pos := func(file string) int {
		return slices.IndexFunc(out.data.Symbols, func(s taskSymbol) bool { return s.File == file })
	}
	if out.data.Symbols[0].File != "importers.go" || (pos("a_test.go") >= 0 && pos("a_test.go") < pos("importers.go")) {
		t.Errorf("symbols = %+v", out.data.Symbols)
	}
}

func TestTaskContextUsageAndReadiness(t *testing.T) {
	idxWorkspace(t, idxFiles)
	if code, out, _ := runMain("task_context", "   "); code != ExitUsage {
		t.Errorf("an empty task: exit %d\n%s", code, out)
	}
	if code, _, _ := runMain("task_context"); code != ExitUsage {
		t.Errorf("no task: exit %d", code)
	}
	if code, out, _ := runMain("task_context", "x", "--budget", "0"); code != ExitUsage {
		t.Errorf("--budget 0: exit %d\n%s", code, out)
	}
	// Only stopwords: an answer, and a low one.
	out := runTask(t, "please add a new one")
	if out.data.Confidence != confLow || len(out.data.Terms) != 0 {
		t.Errorf("no terms: %+v", out.data)
	}

	dir := tree(t, idxFiles)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	scenario{textOutline: true, indexing: true}.apply(t)
	code, stdout, _ := runMain("task_context", "ParseConfig", "--timeout", "1s")
	if code != ExitNotReady {
		t.Errorf("a server that is still indexing: exit %d, want 5\n%s", code, stdout)
	}
	if env := decodeData(t, stdout, nil); env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestMCPTaskContext(t *testing.T) {
	dir := taskWorkspace(t)
	cs := mcpSession(t, dir)
	res := callTool(t, cs, "task_context", map[string]any{"task": "callers of FindImporters are wrong", "budget": 3000, "with_source": 1})
	var d taskContextData
	decodeData(t, resultText(t, res), &d)
	if res.IsError || d.Confidence != confHigh || len(d.Symbols) == 0 || d.Symbols[0].Source == nil || d.Budget.Limit != 3000 {
		t.Errorf("task_context over MCP: isError=%v %+v", res.IsError, d)
	}
	res = callTool(t, cs, "task_context", map[string]any{"task": "add a kubernetes operator"})
	if resultExit(t, res) != ExitProblems {
		t.Errorf("low confidence over MCP: exit %d", resultExit(t, res))
	}
}

func TestExtractTerms(t *testing.T) {
	type want struct {
		term, kind string
		minWeight  float64
	}
	cases := []struct {
		name, text string
		want       []want
		absent     []string
	}{
		{"camel and snake", "make find_importers respect parseConfig", []want{
			{"find_importers", termIdentifier, 3}, {"parseConfig", termIdentifier, 3},
			{"importers", termWord, 0.8}, {"respect", termWord, 1}}, []string{"make"}},
		{"pascal splits into words", "the FindImporters helper", []want{
			{"FindImporters", termIdentifier, 3}, {"find", termWord, 0.8}, {"importers", termWord, 0.8}, {"helper", termWord, 1}}, []string{"the"}},
		{"backticks weigh more and keep plain words", "why does `limit` ignore it", []want{
			{"limit", termIdentifier, 3.5}, {"ignore", termWord, 1}}, []string{"why", "does", "it"}},
		{"a quoted phrase is prose", "the error \"file not found\" shows", []want{
			{"found", termWord, 1}, {"error", termWord, 1}, {"shows", termWord, 1}}, []string{"file"}},
		{"dotted path", "rename pkg.Server.Handle now", []want{
			{"Handle", termIdentifier, 3}, {"Server", termIdentifier, 1.5}, {"pkg", termIdentifier, 1.5}}, []string{"now"}},
		{"paths and their stem", "look at internal/cli/search_text.go and index.go", []want{
			{"internal/cli/search_text.go", termPath, 2.5}, {"index.go", termPath, 2.5}, {"search_text", termWord, 1.2}}, nil},
		{"flags", "--max-lines is ignored", []want{{"max_lines", termFlag, 2}, {"ignored", termWord, 1}}, []string{"is"}},
		{"call syntax", "walk() never returns", []want{{"walk", termIdentifier, 3}, {"never", termWord, 1}, {"returns", termWord, 1}}, nil},
		{"stopwords and short words vanish", "add a fix to it and go on", nil, []string{"add", "fix", "go", "on"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			terms, dropped := extractTerms(tc.text)
			if dropped != 0 {
				t.Errorf("dropped %d", dropped)
			}
			byTerm := map[string]taskTerm{}
			for _, tm := range terms {
				byTerm[tm.Term] = tm
			}
			for _, w := range tc.want {
				got, ok := byTerm[w.term]
				if !ok || got.Kind != w.kind || got.Weight < w.minWeight {
					t.Errorf("term %q: got %+v, want kind %s weight >= %v (terms: %+v)", w.term, got, w.kind, w.minWeight, terms)
				}
			}
			for _, a := range tc.absent {
				if _, ok := byTerm[a]; ok {
					t.Errorf("%q must not be a term: %+v", a, terms)
				}
			}
			for i := 1; i < len(terms); i++ {
				if terms[i].Weight > terms[i-1].Weight {
					t.Errorf("terms are heaviest first: %+v", terms)
				}
			}
		})
	}

	// The cap drops the lightest, and says how many.
	var words []string
	for i := 0; i < 20; i++ {
		words = append(words, "wordnumber"+strings.Repeat("x", i))
	}
	words = append(words, "keepThisName")
	terms, dropped := extractTerms(strings.Join(words, " "))
	if len(terms) != maxTaskTerms || dropped < 9 || terms[0].Term != "keepThisName" {
		t.Errorf("cap: %d terms, dropped %d, first %+v", len(terms), dropped, terms[0])
	}
}

func init() {
	registerLimitCase("task_context", limitCase{
		Rows: "symbols",
		Invoke: func(t *testing.T, extra ...string) string {
			files := map[string]string{}
			for _, n := range []string{"a", "b", "c", "d", "e"} {
				files[n+".go"] = "package a\n\nfunc ParseThing" + strings.ToUpper(n) + "() {}\n"
			}
			idxWorkspace(t, files)
			_, out, _ := runMain(append([]string{"task_context", "ParseThing", "--expand", "0"}, extra...)...)
			return out
		},
	})
}

// The budget is honoured, not merely reported: with a budget the fixed header
// cannot fit, the diagnostics are shed (and said to be), the answer lands
// inside the budget, and a budget below the smallest answer is flagged Over
// instead of claiming it fits.
func TestTaskContextBudgetIsHonouredAndTheRuleIsOneLine(t *testing.T) {
	taskWorkspace(t)
	full := runTask(t, "callers of FindImporters are wrong")
	if full.code != ExitOK || full.data.Budget.Truncated {
		t.Fatalf("the default budget must not bite: %+v", full.data.Budget)
	}
	if full.data.Rule != confidenceRuleName || len(full.data.Rule) > 40 {
		t.Errorf("the default JSON rule must be the bare rule name, got %d bytes: %q", len(full.data.Rule), full.data.Rule)
	}
	if strings.Contains(full.raw, "(b) in a task of plain words") {
		t.Errorf("the full rule paragraph is repeated in every answer")
	}
	if strings.Contains(full.raw, confidenceRuleShort) {
		t.Errorf("the one-line rule paragraph is repeated in every JSON answer; only text output should carry it")
	}
	if withRule := runTask(t, "callers of FindImporters are wrong", "--rule"); withRule.data.Rule != confidenceRule {
		t.Errorf("--rule must print the full rule")
	}

	const budget = 400
	out := runTask(t, "callers of FindImporters are wrong", "--budget", "400")
	d := out.data
	if d.Budget.Used > budget || d.Budget.Over || !d.Budget.Truncated {
		t.Errorf("budget = %+v; the answer must fit %d tokens and say it was cut", d.Budget, budget)
	}
	if len(d.Symbols) == 0 || d.Symbols[0].ID != "importers.go::FindImporters#function" {
		t.Errorf("the best symbol must survive a tight budget: %+v", d.Symbols)
	}

	tiny := runTask(t, "callers of FindImporters are wrong", "--budget", "10")
	if !tiny.data.Budget.Over || tiny.data.Budget.Used <= 10 || !envWarns(tiny.env, "below the smallest answer") {
		t.Errorf("an impossible budget must be said to be impossible: %+v warnings %v", tiny.data.Budget, tiny.env.Warnings)
	}
}

// TestTaskContextJSONIsCompactByDefault pins the D46 default: no terms array,
// no verdict sentence, symbols carry only id/line/signature/score (name,
// kind, file, end_line, doc and evidence are the id and --rule's job), and
// at most defaultTaskRelated related rows with only their identity. --rule
// restores all of it, and nothing is lost — everything is still reachable.
func TestTaskContextJSONIsCompactByDefault(t *testing.T) {
	taskWorkspace(t)
	out := runTask(t, "callers of FindImporters are wrong")
	if out.code != ExitOK {
		t.Fatalf("exit %d\n%s", out.code, out.raw)
	}
	for _, absent := range []string{`"terms"`, `"verdict"`, `"doc"`, `"evidence"`, `"end_line"`} {
		if strings.Contains(out.raw, absent) {
			t.Errorf("default answer contains %s, want it left out by default:\n%s", absent, out.raw)
		}
	}
	if len(out.data.Symbols) == 0 || out.data.Symbols[0].ID == "" || out.data.Symbols[0].Signature == "" {
		t.Errorf("id and signature must survive compaction: %+v", out.data.Symbols)
	}
	for _, s := range out.data.Symbols {
		if s.Name != "" || s.Kind != "" || s.File != "" || s.Doc != "" {
			t.Errorf("a default symbol must not carry name/kind/file/doc (the id already has name/kind/file): %+v", s)
		}
	}
	if len(out.data.Related) > defaultTaskRelated {
		t.Errorf("related rows = %d, want at most %d by default", len(out.data.Related), defaultTaskRelated)
	}
	for _, r := range out.data.Related {
		if r.Of != "" {
			t.Errorf("compact related row must not carry the full 'of' id: %+v", r)
		}
	}
	if !strings.Contains(out.raw, `"of_index"`) {
		t.Errorf("compact related row must say which ranked symbol it neighbours, cheaply: %s", out.raw)
	}

	full := runTask(t, "callers of FindImporters are wrong", "--rule")
	if len(full.data.Terms) == 0 {
		t.Errorf("--rule must restore terms: %+v", full.data)
	}
	if full.data.Symbols[0].Name == "" || full.data.Symbols[0].Kind == "" || full.data.Symbols[0].File == "" {
		t.Errorf("--rule must restore name/kind/file on symbols: %+v", full.data.Symbols[0])
	}
	if !strings.HasPrefix(full.data.Verdict, "found:") {
		t.Errorf("--rule must restore the verdict sentence: %q", full.data.Verdict)
	}
	for _, r := range full.data.Related {
		if r.Of == "" {
			t.Errorf("--rule must restore the full related row, Of included: %+v", r)
		}
	}
}

// TestTaskContextReportDefaultsToASummary pins the D46 default for the index
// revalidation report: a one-line summary and a stale flag, full with
// --report.
func TestTaskContextReportDefaultsToASummary(t *testing.T) {
	idxWorkspace(t, idxFiles)
	code, out, _ := runMain("task_context", "ParseConfig")
	if code != ExitOK && code != ExitProblems {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if strings.Contains(out, `"scan_ns"`) || strings.Contains(out, `"elapsed_ns"`) {
		t.Errorf("default answer carries the full index report: %s", out)
	}
	if !strings.Contains(out, `"summary"`) || !strings.Contains(out, `"stale"`) {
		t.Errorf("default answer must carry the one-line index summary and a stale flag: %s", out)
	}

	code, out, _ = runMain("task_context", "ParseConfig", "--report")
	if code != ExitOK && code != ExitProblems {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out, `"scan_ns"`) {
		t.Errorf("--report must restore the full index report: %s", out)
	}
}
