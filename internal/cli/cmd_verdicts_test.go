package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The composed verdict commands (D38), against the scripted server. Its
// documentSymbol answers come from the files' `func` lines and its references
// are every NEEDLE in other.go (textServer), so a fixture names its function
// NEEDLE to have exactly the references it lists.

// verdictWorkspace is a workspace (a go.mod, a .git marker, the files) with the
// working directory in it and a server that can rename, outline from text and
// answer the read-only methods; extraCaps overrides capabilities (a nil value
// removes one) and results is built from the directory, for the canned answers
// that name files.
func verdictWorkspace(t *testing.T, files map[string]string, extraCaps map[string]any, results func(dir string) map[string]any) string {
	t.Helper()
	dir := tree(t, files)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	caps := mutationServerCaps(extraCaps)
	for k, v := range readOnlyCapabilities() {
		if _, ok := caps[k]; !ok {
			caps[k] = v
		}
	}
	for k, v := range extraCaps {
		if v == nil {
			delete(caps, k)
		}
	}
	var res map[string]any
	if results != nil {
		res = results(dir)
	}
	scenario{capabilities: caps, results: res, textOutline: true}.apply(t)
	return dir
}

// needleFiles: one declaration, four references in one file, and two text-only
// mentions (configuration and prose) that no reference accounts for.
var needleFiles = map[string]string{
	"a.go": "package fixture\n\nfunc NEEDLE() int { return 1 }\n\nfunc Other() {}\n",
	"other.go": "package fixture\n\nfunc user1() int { return NEEDLE() }\n\nfunc user2() int { return NEEDLE() + NEEDLE() }\n\n" +
		"func user3() int { return NEEDLE() }\n\nfunc user4() int { return NEEDLE() }\n",
	"conf.yaml": "handler: NEEDLE\n",
	"notes.md":  "see NEEDLE for details\n",
}

// symbolAnswer is a workspace/symbol answer naming a function.
func symbolAnswer(dir, file, name string, line int) map[string]any {
	return map[string]any{"name": name, "kind": 12, "location": map[string]any{
		"uri": uriOf(filepath.Join(dir, file)),
		"range": map[string]any{
			"start": map[string]any{"line": line, "character": 5},
			"end":   map[string]any{"line": line, "character": 5 + len(name)},
		},
	}}
}

func needleSymbols(dir string) map[string]any {
	return map[string]any{"workspace/symbol": []any{symbolAnswer(dir, "a.go", "NEEDLE", 2)}}
}

// runJSON runs a command and decodes its envelope's data.
func runJSON(t *testing.T, v any, args ...string) (code int, env rawEnvelope, stdout string) {
	t.Helper()
	code, stdout, stderr := runMain(append(args, "--format", "json")...)
	if stdout == "" {
		t.Fatalf("%v printed nothing (exit %d); stderr: %s", args, code, stderr)
	}
	return code, decodeData(t, stdout, v), stdout
}

// --- check_references ---

func TestCheckReferencesKeepsSemanticAndTextApart(t *testing.T) {
	dir := verdictWorkspace(t, needleFiles, nil, needleSymbols)

	// The same symbol named four ways gives the same answer.
	for _, args := range [][]string{
		{"check_references", filepath.Join(dir, "a.go") + ":3:6"},
		{"check_references", "--id", "a.go::NEEDLE#function"},
		{"check_references", "a.go::NEEDLE#function"},
		{"check_references", "NEEDLE"},
		{"check_references", "--symbol", "NEEDLE"},
	} {
		var d checkReferencesData
		code, env, stdout := runJSON(t, &d, args...)
		if code != ExitOK || !env.OK {
			t.Fatalf("%v: exit %d\n%s", args, code, stdout)
		}
		if d.Resolution.Kind != "symbol" || d.Identifier != "NEEDLE" || d.Verdict != verdictUsed {
			t.Errorf("%v: resolution %+v identifier %q verdict %q", args, d.Resolution, d.Identifier, d.Verdict)
		}
		if d.Symbol == nil || d.Symbol.ID != "a.go::NEEDLE#function" {
			t.Errorf("%v: symbol = %+v", args, d.Symbol)
		}
		if d.Semantic.Total != 4 || d.Semantic.Count != 4 || d.Semantic.Evidence != evidenceReferences {
			t.Errorf("%v: semantic = %+v", args, d.Semantic)
		}
		for _, r := range d.Semantic.Rows {
			if r.File != "other.go" || r.Evidence != evidenceReferences || r.In == "" {
				t.Errorf("%v: semantic row %+v", args, r)
			}
		}
		if got := d.Semantic.Rows[0].In; got != "other.go::user1#function" {
			t.Errorf("%v: first reference is in %q, want the enclosing symbol's id", args, got)
		}
		// The text-only list holds what the references do not: the config and
		// the prose, and neither the declaration line nor the referencing lines.
		var files []string
		for _, r := range d.TextOnly.Rows {
			files = append(files, r.File)
			if r.Evidence != evidenceSearchText {
				t.Errorf("%v: text row %+v", args, r)
			}
		}
		slices.Sort(files)
		if !slices.Equal(files, []string{"conf.yaml", "notes.md"}) || d.TextOnly.Total != 2 {
			t.Errorf("%v: text-only files = %v (total %d), want conf.yaml and notes.md", args, files, d.TextOnly.Total)
		}
		if !strings.HasPrefix(d.Rule, "used:") {
			t.Errorf("rule = %q", d.Rule)
		}
	}
}

func TestCheckReferencesTextFormat(t *testing.T) {
	dir := verdictWorkspace(t, needleFiles, nil, needleSymbols)
	code, stdout, _ := runMain("check_references", filepath.Join(dir, "a.go")+":3:6", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	for _, want := range []string{
		"# semantic references (evidence: references): 4 listed of 4 in 1 file(s)",
		"other.go:3:", "[in other.go::user1#function]",
		"# text-only mentions (evidence: search_text): 2 listed of 2",
		"conf.yaml:1:", "notes.md:1:",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output lacks %q:\n%s", want, stdout)
		}
	}
}

func TestCheckReferencesTruncatesEachListAndSaysSo(t *testing.T) {
	verdictWorkspace(t, needleFiles, nil, needleSymbols)
	var d checkReferencesData
	code, env, stdout := runJSON(t, &d, "check_references", "NEEDLE", "--limit", "1")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Semantic.Count != 1 || d.Semantic.Total != 4 || !d.Semantic.Truncated ||
		d.TextOnly.Count != 1 || d.TextOnly.Total != 2 || !d.TextOnly.Truncated || !d.Truncated || d.Total != 6 {
		t.Errorf("semantic %+v\ntext %+v\ntop %v/%d", d.Semantic, d.TextOnly, d.Truncated, d.Total)
	}
	if !hasWarning(env.Warnings, "listed 1 of 4") || !hasWarning(env.Warnings, "listed 1 of 2") {
		t.Errorf("warnings = %v", env.Warnings)
	}
}

func TestCheckReferencesAmbiguousNameIsItsOwnVerdictWithCandidateCounts(t *testing.T) {
	verdictWorkspace(t, needleFiles, nil, func(dir string) map[string]any {
		return map[string]any{"workspace/symbol": []any{
			symbolAnswer(dir, "a.go", "NEEDLE", 2), symbolAnswer(dir, "other.go", "NEEDLE", 2)}}
	})
	var d checkReferencesData
	code, _, stdout := runJSON(t, &d, "check_references", "NEEDLE")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Resolution.Kind != "ambiguous" || len(d.Resolution.Candidates) != 2 || d.Symbol != nil {
		t.Errorf("resolution = %+v symbol = %+v", d.Resolution, d.Symbol)
	}
	// The headline must not say "only in text" about a name whose candidates may
	// well have semantic references: it is its own verdict.
	if d.Semantic.Total != 0 || d.TextOnly.Total < 6 || d.Verdict != verdictAmbiguous {
		t.Errorf("semantic %d, text %d, verdict %q", d.Semantic.Total, d.TextOnly.Total, d.Verdict)
	}
	if !strings.Contains(d.Rule, "NOT known") || !strings.Contains(d.Rule, "per candidate") {
		t.Errorf("rule = %q", d.Rule)
	}
	for _, c := range d.Resolution.Candidates {
		if c.References == nil {
			t.Errorf("candidate %s was not counted", c.Symbol)
		}
	}
}

func TestCheckReferencesUnknownNameIsUnusedAndExit1(t *testing.T) {
	verdictWorkspace(t, needleFiles, nil, func(string) map[string]any {
		return map[string]any{"workspace/symbol": []any{}}
	})
	var d checkReferencesData
	code, _, stdout := runJSON(t, &d, "check_references", "Zzzz")
	if code != ExitProblems {
		t.Fatalf("exit %d, want %d\n%s", code, ExitProblems, stdout)
	}
	if d.Resolution.Kind != "not_found" || d.Verdict != verdictUnused || d.Semantic.Total+d.TextOnly.Total != 0 {
		t.Errorf("data = %+v", d)
	}
}

func TestCheckReferencesUsedOnlyInText(t *testing.T) {
	dir := verdictWorkspace(t, map[string]string{
		"a.go":      "package fixture\n\nfunc Lonely() {}\n",
		"conf.yaml": "hook: Lonely\n",
	}, nil, nil)
	var d checkReferencesData
	code, _, stdout := runJSON(t, &d, "check_references", filepath.Join(dir, "a.go")+":3:6")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Resolution.Kind != "symbol" || d.Semantic.Total != 0 || d.TextOnly.Total != 1 || d.Verdict != verdictUsedOnlyInText {
		t.Errorf("data = %+v", d)
	}
}

// A server with no references at all still gets a text answer, and it says why.
func TestCheckReferencesWithoutReferencesProviderDegradesToText(t *testing.T) {
	dir := verdictWorkspace(t, needleFiles, map[string]any{"referencesProvider": nil}, nil)
	var d checkReferencesData
	code, _, stdout := runJSON(t, &d, "check_references", filepath.Join(dir, "a.go")+":3:6")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Resolution.Kind != "unavailable" || d.Semantic.Total != 0 || d.TextOnly.Total == 0 {
		t.Errorf("data = %+v", d)
	}
}

// --- type_hierarchy ---

func typeItem(dir, file, name string, line int) map[string]any {
	return map[string]any{"name": name, "kind": 5, "uri": uriOf(filepath.Join(dir, file)),
		"range":          map[string]any{"start": map[string]any{"line": line, "character": 0}, "end": map[string]any{"line": line, "character": 20}},
		"selectionRange": map[string]any{"start": map[string]any{"line": line, "character": 5}, "end": map[string]any{"line": line, "character": 5 + len(name)}}}
}

var typeFiles = map[string]string{
	"a.go": "package fixture\n\ntype Dog struct{}\n",
	"b.go": "package fixture\n\ntype Animal interface{}\n\ntype Walker interface{}\n",
	"c.go": "package fixture\n\ntype Puppy struct{}\n\ntype Hound struct{}\n",
}

func typeWorkspace(t *testing.T, native bool) string {
	t.Helper()
	extra := map[string]any{"typeHierarchyProvider": true}
	if !native {
		extra = nil
	}
	return verdictWorkspace(t, typeFiles, extra, func(dir string) map[string]any {
		res := map[string]any{}
		if native {
			res["textDocument/prepareTypeHierarchy"] = []any{typeItem(dir, "a.go", "Dog", 2)}
			res["typeHierarchy/supertypes"] = []any{typeItem(dir, "b.go", "Animal", 2), typeItem(dir, "b.go", "Walker", 4)}
			res["typeHierarchy/subtypes"] = []any{typeItem(dir, "c.go", "Puppy", 2), typeItem(dir, "c.go", "Hound", 4)}
		} else {
			res["textDocument/implementation"] = []any{
				map[string]any{"uri": uriOf(filepath.Join(dir, "c.go")), "range": map[string]any{"start": map[string]any{"line": 2, "character": 5}, "end": map[string]any{"line": 2, "character": 10}}},
				map[string]any{"uri": uriOf(filepath.Join(dir, "c.go")), "range": map[string]any{"start": map[string]any{"line": 4, "character": 5}, "end": map[string]any{"line": 4, "character": 10}}},
			}
		}
		return res
	})
}

func TestTypeHierarchyNative(t *testing.T) {
	dir := typeWorkspace(t, true)
	loc := filepath.Join(dir, "a.go") + ":3:6"
	var d typeHierarchyData
	code, _, stdout := runJSON(t, &d, "type_hierarchy", loc)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Source != evidenceTypeHierarchy || d.Count != 4 || d.Truncated {
		t.Fatalf("data = %+v", d)
	}
	var got []string
	for _, r := range d.Rows {
		got = append(got, r.Direction+":"+r.Name)
		if r.Evidence != evidenceTypeHierarchy || r.Depth != 1 || r.File == "" || r.Line == 0 {
			t.Errorf("row = %+v", r)
		}
	}
	if !slices.Equal(got, []string{"supertype:Animal", "supertype:Walker", "subtype:Puppy", "subtype:Hound"}) {
		t.Errorf("rows = %v", got)
	}

	// --direction narrows, and the text form draws the tree.
	code, _, _ = runJSON(t, &d, "type_hierarchy", loc, "--direction", "supertypes")
	if code != ExitOK || d.Count != 2 || d.Rows[0].Direction != "supertype" {
		t.Errorf("supertypes only: exit %d data %+v", code, d)
	}
	_, text, _ := runMain("type_hierarchy", loc, "--format", "text")
	for _, want := range []string{"^ Animal", "v Puppy", "[type_hierarchy]"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestTypeHierarchyDepthCyclesAndLimit(t *testing.T) {
	dir := typeWorkspace(t, true)
	loc := filepath.Join(dir, "a.go") + ":3:6"

	// The scripted server answers every expansion the same, so a deeper walk
	// leads back to a type already shown, and says so instead of looping.
	var d typeHierarchyData
	code, env, stdout := runJSON(t, &d, "type_hierarchy", loc, "--depth", "3", "--direction", "subtypes")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Cycles == 0 || !hasWarning(env.Warnings, "lead back to a type already shown") {
		t.Errorf("cycles = %d warnings = %v", d.Cycles, env.Warnings)
	}
	if code, _, _ := runMain("type_hierarchy", loc, "--depth", "9"); code != ExitUsage {
		t.Errorf("--depth 9: exit %d, want %d", code, ExitUsage)
	}

	code, env, _ = runJSON(t, &d, "type_hierarchy", loc, "--limit", "2")
	if code != ExitOK || d.Count != 2 || d.Total != 4 || !d.Truncated || !hasWarning(env.Warnings, "listed 2 of 4") {
		t.Errorf("--limit 2: exit %d count %d total %d truncated %v warnings %v", code, d.Count, d.Total, d.Truncated, env.Warnings)
	}
}

func TestTypeHierarchyFallsBackToImplementationAndSaysSo(t *testing.T) {
	dir := typeWorkspace(t, false)
	loc := filepath.Join(dir, "a.go") + ":3:6"
	var d typeHierarchyData
	code, env, stdout := runJSON(t, &d, "type_hierarchy", loc)
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Source != evidenceImplementation || d.Count != 2 {
		t.Fatalf("data = %+v", d)
	}
	for i, want := range []string{"Puppy", "Hound"} {
		r := d.Rows[i]
		if r.Name != want || r.Direction != "subtype" || r.Evidence != evidenceImplementation {
			t.Errorf("row %d = %+v", i, r)
		}
	}
	if !hasWarning(env.Warnings, "no typeHierarchyProvider") || !hasWarning(env.Warnings, "supertypes are not available") {
		t.Errorf("warnings = %v", env.Warnings)
	}

	// Supertypes have no fallback: exit 3 naming the capability.
	code, stdout, _ = runMain("type_hierarchy", loc, "--direction", "supertypes", "--format", "json")
	if code != ExitNoServer {
		t.Fatalf("supertypes only: exit %d\n%s", code, stdout)
	}
	env2 := decodeData(t, stdout, nil)
	if env2.Error == nil || env2.Error.Code != string(render.CodeUnsupportedMethod) || !strings.Contains(env2.Error.Message, "typeHierarchyProvider") {
		t.Errorf("error = %+v", env2.Error)
	}
}

func TestTypeHierarchyNeedsSomeCapability(t *testing.T) {
	dir := verdictWorkspace(t, typeFiles, map[string]any{"implementationProvider": nil}, nil)
	code, stdout, _ := runMain("type_hierarchy", filepath.Join(dir, "a.go")+":3:6", "--format", "json")
	if code != ExitNoServer {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if env := decodeData(t, stdout, nil); env.Error == nil || env.Error.Code != string(render.CodeUnsupportedMethod) {
		t.Errorf("error = %+v", env.Error)
	}
}

// --- rename_check ---

// renameCheckWorkspace is the rename fixture of the M2 tests, with a README that
// mentions Old on lines a rename does not touch, and a server that outlines.
func renameCheckWorkspace(t *testing.T, mentions int, extraCaps map[string]any, prepare any) string {
	t.Helper()
	files := map[string]string{}
	for k, v := range fixtureFiles {
		files[k] = v
	}
	files["README.md"] = strings.Repeat("call Old() first\n", mentions)
	return verdictWorkspace(t, files, extraCaps, func(dir string) map[string]any {
		return map[string]any{
			methodPrepareRename: prepare,
			methodRename:        renameToNew(dir),
		}
	})
}

var oldPrepare = map[string]any{
	"range":       map[string]any{"start": map[string]any{"line": 2, "character": 5}, "end": map[string]any{"line": 2, "character": 8}},
	"placeholder": "Old",
}

func TestRenameCheckOKSummarizesEditsAndWritesNothing(t *testing.T) {
	dir := renameCheckWorkspace(t, 1, nil, oldPrepare)
	before := snapshot(t, dir)
	var d renameCheckData
	code, _, stdout := runJSON(t, &d, "rename_check", declLoc(dir), "New")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Verdict != renameOK || d.OldName != "Old" || d.NewName != "New" || d.Symbol.ID != "a.go::Old#function" {
		t.Errorf("verdict %q old %q new %q symbol %+v", d.Verdict, d.OldName, d.NewName, d.Symbol)
	}
	if d.Prepare.Status != "accepted" || d.Prepare.Placeholder != "Old" || d.Prepare.Evidence != evidencePrepareRename {
		t.Errorf("prepare = %+v", d.Prepare)
	}
	if d.Edits.Status != "accepted" || d.Edits.Files != 3 || d.Edits.Edits != 4 || len(d.Edits.PerFile) != 3 || d.Edits.Evidence != evidenceRename {
		t.Errorf("edits = %+v", d.Edits)
	}
	if !d.Collisions.Checked || d.Collisions.Count != 0 || d.Collisions.Evidence != evidenceIndex {
		t.Errorf("collisions = %+v", d.Collisions)
	}
	if d.Unrenamed.Total != 1 || d.Unrenamed.Rows[0].File != "README.md" || d.Unrenamed.Rows[0].Evidence != evidenceSearchText {
		t.Errorf("unrenamed = %+v", d.Unrenamed)
	}
	assertUnchanged(t, dir, before)

	_, text, _ := runMain("rename_check", declLoc(dir), "New", "--format", "text")
	for _, want := range []string{"# rename_check Old -> New: ok", "3 file(s), 4 edit(s)", "README.md:1:"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestRenameCheckFindsACollisionThroughTheIndex(t *testing.T) {
	dir := renameCheckWorkspace(t, 1, nil, oldPrepare)
	before := snapshot(t, dir)
	var d renameCheckData
	// useB is declared in b.go, in the package of a.go.
	code, _, stdout := runJSON(t, &d, "rename_check", declLoc(dir), "useB")
	if code != ExitProblems {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Verdict != renameCollision || d.Collisions.Count != 1 {
		t.Fatalf("verdict %q collisions %+v", d.Verdict, d.Collisions)
	}
	c := d.Collisions.Rows[0]
	if c.ID != "b.go::useB#function" || c.Scope != "same_directory" || c.Evidence != evidenceIndex {
		t.Errorf("collision = %+v", c)
	}
	assertUnchanged(t, dir, before)
}

func TestRenameCheckRefusedNoopAndUnverified(t *testing.T) {
	// The server will not rename here.
	dir := renameCheckWorkspace(t, 1, nil, nil)
	var d renameCheckData
	code, _, stdout := runJSON(t, &d, "rename_check", declLoc(dir), "New")
	if code != ExitProblems || d.Verdict != renameRefused || d.Prepare.Status != "refused" || d.Edits.Status != "not_asked" {
		t.Errorf("refused: exit %d verdict %q prepare %+v edits %+v\n%s", code, d.Verdict, d.Prepare, d.Edits.renameStep, stdout)
	}

	// The same name changes nothing.
	dir = renameCheckWorkspace(t, 1, nil, oldPrepare)
	code, _, _ = runJSON(t, &d, "rename_check", declLoc(dir), "Old")
	if code != ExitProblems || d.Verdict != renameNoop {
		t.Errorf("noop: exit %d verdict %q", code, d.Verdict)
	}

	// A server that cannot rename cannot preview: only the index speaks.
	dir = renameCheckWorkspace(t, 1, map[string]any{"renameProvider": nil}, oldPrepare)
	code, _, _ = runJSON(t, &d, "rename_check", declLoc(dir), "New")
	if code != ExitProblems || d.Verdict != renameUnverified || d.Prepare.Status != "not_supported" || d.Edits.Status != "not_supported" || !d.Collisions.Checked {
		t.Errorf("unverified: exit %d verdict %q prepare %+v edits %+v collisions %+v", code, d.Verdict, d.Prepare, d.Edits.renameStep, d.Collisions)
	}
}

func TestRenameCheckRejectsAnEmptyOrSpacedName(t *testing.T) {
	dir := renameCheckWorkspace(t, 1, nil, oldPrepare)
	for _, name := range []string{"", "two words"} {
		if code, _, _ := runMain("rename_check", declLoc(dir), name); code != ExitUsage {
			t.Errorf("new name %q: exit %d, want %d", name, code, ExitUsage)
		}
	}
}

// --- delete_check ---

func TestDeleteCheckInUse(t *testing.T) {
	dir := verdictWorkspace(t, needleFiles, nil, nil)
	before := snapshot(t, dir)
	var d deleteCheckData
	code, _, stdout := runJSON(t, &d, "delete_check", filepath.Join(dir, "a.go")+":3:6")
	if code != ExitProblems {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Verdict != deleteInUse || d.References.NonTest != 4 || d.References.Total != 4 || d.Symbol.ID != "a.go::NEEDLE#function" {
		t.Errorf("verdict %q references %+v symbol %+v", d.Verdict, d.References, d.Symbol)
	}
	if d.Exported != "exported" || !strings.Contains(d.ExportedBasis, "upper-case") ||
		!slices.ContainsFunc(d.Caveats, func(c string) bool { return strings.Contains(c, "exported") }) {
		t.Errorf("exported %q (%s) caveats %v", d.Exported, d.ExportedBasis, d.Caveats)
	}
	for _, r := range d.References.Rows {
		if r.Evidence != evidenceReferences || r.In == "" {
			t.Errorf("row = %+v", r)
		}
	}
	assertUnchanged(t, dir, before)

	_, text, _ := runMain("delete_check", filepath.Join(dir, "a.go")+":3:6", "--format", "text")
	for _, want := range []string{"# delete_check NEEDLE: in_use", "# exported: exported", "other.go:3:"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
}

func TestDeleteCheckIgnoresReferencesInsideItsOwnDeclaration(t *testing.T) {
	// The server also returns the declaration itself and a recursive call, both
	// on the declaration's own line: neither is a use.
	dir := verdictWorkspace(t, map[string]string{
		"other.go": "package fixture\n\nfunc NEEDLE() int { return NEEDLE() }\n\nfunc Keep() {}\n",
	}, nil, nil)
	var d deleteCheckData
	code, _, stdout := runJSON(t, &d, "delete_check", filepath.Join(dir, "other.go")+":3:6")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Verdict != deleteUnused || d.References.Total != 0 || d.References.InsideDeclaration == 0 ||
		d.Last.OfFile || !d.Last.Checked {
		t.Errorf("verdict %q references %+v last %+v", d.Verdict, d.References, d.Last)
	}
	if !slices.ContainsFunc(d.Caveats, func(c string) bool { return strings.Contains(c, "inside the symbol's own declaration") }) {
		t.Errorf("caveats = %v", d.Caveats)
	}
}

func TestDeleteCheckUnexportedAndUnused(t *testing.T) {
	dir := verdictWorkspace(t, map[string]string{
		"a.go": "package fixture\n\nfunc lonely() {}\n\nfunc Keep() {}\n",
	}, nil, nil)
	var d deleteCheckData
	code, _, stdout := runJSON(t, &d, "delete_check", filepath.Join(dir, "a.go")+":3:6")
	if code != ExitOK || d.Verdict != deleteUnused || d.Exported != "unexported" || len(d.Caveats) != 0 {
		t.Errorf("exit %d verdict %q exported %q caveats %v\n%s", code, d.Verdict, d.Exported, d.Caveats, stdout)
	}
}

func TestDeleteCheckLastSymbolOfPackageListsItsImporters(t *testing.T) {
	dir := verdictWorkspace(t, map[string]string{
		"solo/x.go": "package solo\n\nfunc lonely() {}\n",
		"use.go":    "package fixture\n\nimport _ \"fixture/solo\"\n\nfunc main() {}\n",
	}, nil, nil)
	var d deleteCheckData
	code, _, stdout := runJSON(t, &d, "delete_check", filepath.Join(dir, "solo", "x.go")+":3:6")
	if code != ExitProblems {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	if d.Verdict != deleteDangling || !d.Last.OfFile || !d.Last.OfPackage || d.Last.RemainingInPackage != 0 {
		t.Errorf("verdict %q last %+v", d.Verdict, d.Last)
	}
	if d.Importers == nil || d.ImportersTarget != "solo" || d.Importers.Total != 1 ||
		d.Importers.Rows[0].File != "use.go" || d.Importers.Rows[0].Evidence != evidenceImporters {
		t.Errorf("importers %q %+v", d.ImportersTarget, d.Importers)
	}
}

func TestDeleteCheckTestsOnly(t *testing.T) {
	dir := verdictWorkspace(t, map[string]string{
		"tests/a.go":     "package tests\n\nfunc NEEDLE() {}\n\nfunc Keep() {}\n",
		"tests/other.go": "package tests\n\nfunc user() { NEEDLE() }\n",
	}, nil, nil)
	var d deleteCheckData
	code, _, stdout := runJSON(t, &d, "delete_check", filepath.Join(dir, "tests", "a.go")+":3:6")
	if code != ExitProblems || d.Verdict != deleteTestsOnly || d.References.NonTest != 0 || d.References.InTests != 1 {
		t.Errorf("exit %d verdict %q references %+v\n%s", code, d.Verdict, d.References, stdout)
	}
}

func TestExportednessRules(t *testing.T) {
	for _, tc := range []struct {
		lang, name, decl, file, rel string
		nested                      bool
		want                        string
	}{
		{"go", "Foo", "func Foo() {}", "", "a.go", false, "exported"},
		{"go", "foo", "func foo() {}", "", "a.go", false, "unexported"},
		{"python", "_helper", "def _helper():", "", "a.py", false, "unexported"},
		{"python", "helper", "def helper():", "", "a.py", false, "exported"},
		{"typescript", "f", "export function f() {}", "", "a.ts", false, "exported"},
		{"typescript", "f", "function f() {}", "export { f };", "a.ts", false, "exported"},
		{"typescript", "f", "function f() {}", "const x = 1;", "a.ts", false, "unexported"},
		{"typescript", "m", "  m() {}", "", "a.ts", true, "unknown"},
		{"rust", "f", "pub fn f() {}", "", "a.rs", false, "exported"},
		{"rust", "f", "pub(crate) fn f() {}", "", "a.rs", false, "unknown"},
		{"rust", "f", "fn f() {}", "", "a.rs", false, "unexported"},
		{"java", "f", "public void f() {}", "", "A.java", true, "exported"},
		{"java", "f", "void f() {}", "", "A.java", true, "unknown"},
		{"lua", "f", "local function f() end", "", "a.lua", false, "unexported"},
		{"c", "f", "static int f() {", "", "a.c", false, "unexported"},
		{"c", "f", "int f() {", "", "a.c", false, "unknown"},
		{"cobol", "F", "", "", "a.cbl", false, "unknown"},
	} {
		if got, basis := languageExportState(tc.lang, tc.name, tc.nested, tc.decl, tc.file, tc.rel); got != tc.want || basis == "" {
			t.Errorf("%s %q %q: %q (%s), want %q", tc.lang, tc.name, tc.decl, got, basis, tc.want)
		}
	}
}

// --- MCP ---

func TestMCPVerdictTools(t *testing.T) {
	dir := verdictWorkspace(t, needleFiles, nil, needleSymbols)
	cs := mcpSession(t, dir)

	res := callTool(t, cs, "check_references", map[string]any{"name": "a.go:3:6"})
	if res.IsError || resultExit(t, res) != ExitOK {
		t.Fatalf("check_references: %s", resultText(t, res))
	}
	var d checkReferencesData
	decodeData(t, resultText(t, res), &d)
	if d.Verdict != verdictUsed || d.Semantic.Total != 4 || d.TextOnly.Total != 2 {
		t.Errorf("check_references data = %+v", d)
	}

	res = callTool(t, cs, "check_references", map[string]any{"name": "NEEDLE"})
	if res.IsError {
		t.Fatalf("check_references by name: %s", resultText(t, res))
	}

	res = callTool(t, cs, "delete_check", map[string]any{"id": "a.go::NEEDLE#function"})
	if res.IsError || resultExit(t, res) != ExitProblems {
		t.Fatalf("delete_check: %s", resultText(t, res))
	}
	var dd deleteCheckData
	decodeData(t, resultText(t, res), &dd)
	if dd.Verdict != deleteInUse {
		t.Errorf("delete_check verdict = %q", dd.Verdict)
	}
}

func TestMCPRenameCheckAndTypeHierarchy(t *testing.T) {
	dir := renameCheckWorkspace(t, 1, nil, oldPrepare)
	cs := mcpSession(t, dir)
	res := callTool(t, cs, "rename_check", map[string]any{"location": "a.go:3:6", "new_name": "New"})
	if res.IsError || resultExit(t, res) != ExitOK {
		t.Fatalf("rename_check: %s", resultText(t, res))
	}
	var d renameCheckData
	decodeData(t, resultText(t, res), &d)
	if d.Verdict != renameOK || d.Edits.Edits != 4 {
		t.Errorf("rename_check data = %+v", d)
	}

	dir = typeWorkspace(t, true)
	cs = mcpSession(t, dir)
	res = callTool(t, cs, "type_hierarchy", map[string]any{"location": "a.go:3:6", "direction": "subtypes"})
	if res.IsError {
		t.Fatalf("type_hierarchy: %s", resultText(t, res))
	}
	var th typeHierarchyData
	decodeData(t, resultText(t, res), &th)
	if th.Count != 2 || th.Rows[0].Direction != "subtype" {
		t.Errorf("type_hierarchy data = %+v", th)
	}
}

// --- --limit ---

func init() {
	invoke := func(setup func(t *testing.T) []string) func(t *testing.T, extra ...string) string {
		return func(t *testing.T, extra ...string) string {
			t.Helper()
			args := setup(t)
			_, stdout, stderr := runMain(append(append(args, "--format", "json"), extra...)...)
			if stdout == "" {
				t.Fatalf("%v printed nothing; stderr: %s", args, stderr)
			}
			return stdout
		}
	}
	registerLimitCase("type_hierarchy", limitCase{Rows: "rows", Invoke: invoke(func(t *testing.T) []string {
		dir := typeWorkspace(t, true)
		return []string{"type_hierarchy", filepath.Join(dir, "a.go") + ":3:6"}
	})})
	registerLimitCase("check_references", limitCase{Rows: "semantic.rows", Invoke: invoke(func(t *testing.T) []string {
		verdictWorkspace(t, needleFiles, nil, needleSymbols)
		return []string{"check_references", "NEEDLE"}
	})})
	registerLimitCase("delete_check", limitCase{Rows: "references.rows", Invoke: invoke(func(t *testing.T) []string {
		dir := verdictWorkspace(t, needleFiles, nil, nil)
		return []string{"delete_check", filepath.Join(dir, "a.go") + ":3:6"}
	})})
	registerLimitCase("rename_check", limitCase{Rows: "unrenamed_mentions.rows", Invoke: invoke(func(t *testing.T) []string {
		dir := renameCheckWorkspace(t, 5, nil, oldPrepare)
		return []string{"rename_check", declLoc(dir), "New"}
	})})
}
