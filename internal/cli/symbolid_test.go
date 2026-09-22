package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// The fixture of symbol-addressed retrieval: one Go file whose symbols a fake
// server lists by script, chosen to be hard for the obvious implementation — CJK
// identifiers, emoji in comments and strings (so UTF-16 and byte columns
// disagree), two `init`s (a duplicate id), a signature that spans two lines,
// and two declarations on one line.
const symSource = "package sym\n" + // 0
	"\n" + // 1
	"import (\n" + // 2
	"\t\"fmt\"\n" + // 3
	"\t\"strings\"\n" + // 4
	")\n" + // 5
	"\n" + // 6
	"// Greeter says hello. 挨拶 🎉\n" + // 7
	"type Greeter struct {\n" + // 8
	"\tName string\n" + // 9
	"}\n" + // 10
	"\n" + // 11
	"// Greet は挨拶を返す 👋\n" + // 12
	"func (g *Greeter) Greet() string {\n" + // 13
	"\treturn fmt.Sprintf(\"こんにちは, %s 🎉\", strings.ToUpper(g.Name))\n" + // 14
	"}\n" + // 15
	"\n" + // 16
	"func init() {}\n" + // 17
	"\n" + // 18
	"func init() {}\n" + // 19
	"\n" + // 20
	"func Long(a int,\n" + // 21
	"\tb int) int {\n" + // 22
	"\treturn a + b\n" + // 23
	"}\n" + // 24
	"\n" + // 25
	"var 変数 = 1; var 名前 = \"😀\"\n" // 26

// The wanted text of the Greet method, doc comment included, byte for byte.
const greetSource = "// Greet は挨拶を返す 👋\n" +
	"func (g *Greeter) Greet() string {\n" +
	"\treturn fmt.Sprintf(\"こんにちは, %s 🎉\", strings.ToUpper(g.Name))\n" +
	"}"

// wantSymIDs are the ids of the fixture's symbols, in the server's order.
var wantSymIDs = []string{
	"sym.go::Greeter#struct",
	"sym.go::Greeter.Name#field",
	"sym.go::Greeter.Greet#method",
	"sym.go::init#function",
	"sym.go::init#function~2",
	"sym.go::Long#function",
	"sym.go::変数#variable",
	"sym.go::名前#variable",
}

// u16 is a string's length in UTF-16 code units, which is what an LSP column
// counts.
func u16(s string) int { return len(utf16.Encode([]rune(s))) }

// lspRange builds a range in UTF-16 coordinates.
func lspRange(l0, c0, l1, c1 int) map[string]any {
	return map[string]any{
		"start": map[string]any{"line": l0, "character": c0},
		"end":   map[string]any{"line": l1, "character": c1},
	}
}

// nameRange is the range of the last sub on a line of the fixture, in UTF-16.
// The last, so that `Greet` is the method's name and not the start of the
// receiver type `*Greeter` before it.
func nameRange(line int, sub string) map[string]any {
	text := strings.Split(symSource, "\n")[line]
	i := strings.LastIndex(text, sub)
	if i < 0 {
		panic("no " + sub + " on line " + text)
	}
	c := u16(text[:i])
	return lspRange(line, c, line, c+u16(sub))
}

// lineEnd is the UTF-16 length of a fixture line.
func lineEnd(line int) int { return u16(strings.Split(symSource, "\n")[line]) }

func dsym(name string, kind int, detail string, rng, sel map[string]any, children ...any) map[string]any {
	s := map[string]any{"name": name, "kind": kind, "range": rng, "selectionRange": sel}
	if detail != "" {
		s["detail"] = detail
	}
	if len(children) > 0 {
		s["children"] = children
	}
	return s
}

// symHierarchical is textDocument/documentSymbol as gopls answers it: a tree,
// with the receiver spelled into the method's name.
func symHierarchical() []any {
	second := strings.Index(strings.Split(symSource, "\n")[26], "var 名前")
	secondStart := u16(strings.Split(symSource, "\n")[26][:second])
	return []any{
		dsym("Greeter", 23, "struct{...}", lspRange(8, 0, 10, 1), nameRange(8, "Greeter"),
			dsym("Name", 8, "string", lspRange(9, 1, 9, 12), nameRange(9, "Name"))),
		dsym("(*Greeter).Greet", 6, "func() string", lspRange(13, 0, 15, 1), nameRange(13, "Greet")),
		dsym("init", 12, "func()", lspRange(17, 0, 17, 13), nameRange(17, "init")),
		dsym("init", 12, "func()", lspRange(19, 0, 19, 13), nameRange(19, "init")),
		dsym("Long", 12, "", lspRange(21, 0, 24, 1), nameRange(21, "Long")),
		dsym("変数", 13, "", lspRange(26, 0, 26, u16("var 変数 = 1")), nameRange(26, "変数")),
		dsym("名前", 13, "", lspRange(26, secondStart, 26, lineEnd(26)), nameRange(26, "名前")),
	}
}

// symFlat is the same file as a server that only knows SymbolInformation
// lists it: no hierarchy, the container in containerName, and the location's
// range the whole declaration.
func symFlat(file string) []any {
	info := func(name string, kind int, container string, rng map[string]any) map[string]any {
		s := map[string]any{"name": name, "kind": kind, "location": map[string]any{"uri": "file://" + file, "range": rng}}
		if container != "" {
			s["containerName"] = container
		}
		return s
	}
	second := strings.Index(strings.Split(symSource, "\n")[26], "var 名前")
	secondStart := u16(strings.Split(symSource, "\n")[26][:second])
	return []any{
		info("Greeter", 23, "", lspRange(8, 0, 10, 1)),
		info("Name", 8, "Greeter", lspRange(9, 1, 9, 12)),
		info("Greet", 6, "Greeter", lspRange(13, 0, 15, 1)),
		info("init", 12, "", lspRange(17, 0, 17, 13)),
		info("init", 12, "", lspRange(19, 0, 19, 13)),
		info("Long", 12, "", lspRange(21, 0, 24, 1)),
		info("変数", 13, "", lspRange(26, 0, 26, u16("var 変数 = 1"))),
		info("名前", 13, "", lspRange(26, secondStart, 26, lineEnd(26))),
	}
}

// symWorkspace is a workspace with .git (the marker that makes it one), the
// fixture file and the given other files, with the working directory in it.
func symWorkspace(t *testing.T, others map[string]string) (dir, file string) {
	t.Helper()
	dir = tree(t, others)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	file = filepath.Join(dir, "sym.go")
	write(t, file, symSource)
	t.Chdir(dir)
	return dir, file
}

// symScenario answers documentSymbol with the given result and hover with a
// fixed text, and applies it.
func symScenario(t *testing.T, documentSymbol any, extra map[string]any) {
	t.Helper()
	results := map[string]any{
		methodDocumentSymbol: documentSymbol,
		methodHover:          map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "func (*Greeter) Greet() string"}},
	}
	for k, v := range extra {
		results[k] = v
	}
	scenario{results: results}.apply(t)
}

// envData decodes an envelope's data into v, failing unless ok.
func okData(t *testing.T, stdout string, v any) rawEnvelope {
	t.Helper()
	env := decodeData(t, stdout, v)
	if !env.OK {
		t.Fatalf("envelope is not ok: %+v\n%s", env.Error, stdout)
	}
	return env
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestSymbolsCarryIDs: `symbols` puts the id of every symbol in its output,
// with the receiver spelled as the type, a duplicate told apart by ~N, and the
// path relative to the workspace.
func TestSymbolsCarryIDs(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)

	code, stdout, stderr := runMain("symbols", "sym.go")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	got := decodeResults(t, stdout)
	var ids []string
	for _, r := range got.Results {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, wantSymIDs) {
		t.Errorf("ids =\n%q\nwant\n%q", ids, wantSymIDs)
	}
}

// TestSymbolIDsDoNotDependOnTheServersShape: a server that lists the file as
// flat SymbolInformation, with the container in containerName, gives the same
// ids as one that answers with a tree.
func TestSymbolIDsDoNotDependOnTheServersShape(t *testing.T) {
	_, file := symWorkspace(t, nil)
	symScenario(t, symFlat(file), nil)

	code, stdout, stderr := runMain("symbols", "sym.go")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var ids []string
	for _, r := range decodeResults(t, stdout).Results {
		ids = append(ids, r.ID)
	}
	if !slices.Equal(ids, wantSymIDs) {
		t.Errorf("flat ids =\n%q\nwant\n%q", ids, wantSymIDs)
	}

	// And the outline nests them by containment, since the server did not.
	code, stdout, stderr = runMain("outline", "sym.go")
	if code != ExitOK {
		t.Fatalf("outline: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data outlineData
	okData(t, stdout, &data)
	greeter := data.Files[0].Symbols[0]
	if greeter.Name != "Greeter" || len(greeter.Children) != 2 ||
		greeter.Children[0].Name != "Name" || greeter.Children[1].Name != "Greet" {
		t.Errorf("Greeter's children = %+v, want Name and Greet nested by containment", greeter.Children)
	}
}

// TestSymbolsOutsideTheWorkspaceHaveNoIDs: an id's path is relative to the
// workspace, so a file outside it is listed without ids, not with an invented
// one.
func TestSymbolsOutsideTheWorkspaceHaveNoIDs(t *testing.T) {
	_, file := symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)
	elsewhere := t.TempDir()
	t.Chdir(elsewhere)

	code, stdout, stderr := runMain("symbols", file)
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	for _, r := range decodeResults(t, stdout).Results {
		if r.ID != "" {
			t.Errorf("%s has id %q, but the file is outside the workspace", r.Label, r.ID)
		}
	}
}

// TestOutline: a tree with ids, kinds, signatures and line ranges; a limit
// that says it cut; two files in one call served by one language server.
func TestOutline(t *testing.T) {
	dir, _ := symWorkspace(t, map[string]string{"sym2.go": symSource})
	symScenario(t, symHierarchical(), nil)
	spawns, _ := spawnLog(t)

	code, stdout, stderr := runMain("outline", "sym.go", filepath.Join(dir, "sym2.go"))
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data outlineData
	okData(t, stdout, &data)
	if len(data.Files) != 2 || data.Files[0].File != "sym.go" || data.Files[1].File != "sym2.go" || data.Files[0].Language != "go" {
		t.Fatalf("files = %+v, want sym.go and sym2.go, in the order given", data.Files)
	}
	if data.Count != 16 || data.Total != 16 || data.Truncated {
		t.Errorf("count/total/truncated = %d/%d/%v, want 16/16/false", data.Count, data.Total, data.Truncated)
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers for two files of one language, want 1", n)
	}

	top := data.Files[0].Symbols
	names := []string{}
	for _, n := range top {
		names = append(names, n.Name)
	}
	if want := []string{"Greeter", "(*Greeter).Greet", "init", "init", "Long", "変数", "名前"}; !slices.Equal(names, want) {
		t.Fatalf("top-level symbols = %q, want %q", names, want)
	}
	greeter, greet, long := top[0], top[1], top[4]
	if greeter.ID != "sym.go::Greeter#struct" || greeter.Kind != "struct" || greeter.Line != 9 || greeter.EndLine != 11 {
		t.Errorf("Greeter = %+v, want id sym.go::Greeter#struct, struct, lines 9-11", greeter)
	}
	if len(greeter.Children) != 1 || greeter.Children[0].ID != "sym.go::Greeter.Name#field" || greeter.Children[0].Line != 10 {
		t.Errorf("Greeter's children = %+v, want the Name field on line 10", greeter.Children)
	}
	// The server's own detail is the signature when it has one...
	if greet.Signature != "func() string" || greet.ID != "sym.go::Greeter.Greet#method" {
		t.Errorf("Greet = %+v, want the server's detail and the receiver spelled as the type", greet)
	}
	// ...and the declaration as written, joined over its lines, when it has not.
	if long.Signature != "func Long(a int, b int) int" || long.Line != 22 || long.EndLine != 25 {
		t.Errorf("Long = %+v, want the two-line signature joined, lines 22-25", long)
	}
	if got := data.Files[1].Symbols[0].ID; got != "sym2.go::Greeter#struct" {
		t.Errorf("the second file's ids = %q, want them relative to the workspace too", got)
	}

	// --limit cuts in document order and says so.
	code, stdout, stderr = runMain("outline", "sym.go", "--limit", "3")
	if code != ExitOK {
		t.Fatalf("--limit: exit %d; stderr: %s", code, stderr)
	}
	env := okData(t, stdout, &data)
	if data.Count != 3 || data.Total != 8 || !data.Truncated {
		t.Errorf("--limit 3: count/total/truncated = %d/%d/%v, want 3/8/true", data.Count, data.Total, data.Truncated)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "3 of 8 symbols") {
		t.Errorf("the cut is not in the warnings: %v", env.Warnings)
	}

	code, stdout, _ = runMain("outline", "sym.go", "--format", "text")
	if code != ExitOK || !strings.Contains(stdout, "sym.go:9: struct Greeter") || !strings.Contains(stdout, "sym.go:10:   field Name") {
		t.Errorf("text outline: exit %d\n%s", code, stdout)
	}

	// One file that does not exist is that call's error, before a server is
	// asked.
	if code, stdout, _ := runMain("outline", "gone.go"); code != ExitUsage || !strings.Contains(stdout, "no_such_file") {
		t.Errorf("outline of a missing file: exit %d\n%s", code, stdout)
	}
	// Among others it is that file's error: the rest are outlined, the exit
	// status says there was a problem, and the order is the one given (D31).
	code, stdout, stderr = runMain("outline", "sym.go", "gone.go", filepath.Join(dir, "sym2.go"))
	if code != ExitProblems {
		t.Fatalf("outline with a missing file: exit %d, want %d; stderr: %s\n%s", code, ExitProblems, stderr, stdout)
	}
	data = outlineData{}
	env = okData(t, stdout, &data)
	if len(data.Files) != 3 || data.Files[0].Count != 8 || data.Files[2].Count != 8 {
		t.Fatalf("files = %+v, want sym.go and sym2.go outlined around the failure", data.Files)
	}
	if e := data.Files[1].Error; e == nil || e.Code != "no_such_file" || e.Target != "gone.go" || len(data.Files[1].Symbols) != 0 {
		t.Errorf("the missing file's entry = %+v, want a no_such_file error and no symbols", data.Files[1])
	}
	if data.Files[0].Error != nil || data.Files[2].Error != nil {
		t.Errorf("the files that exist carry an error: %+v", data.Files)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "gone.go") {
		t.Errorf("the failure is not in the warnings: %v", env.Warnings)
	}
	// Nothing outlined at all is the error itself, with every file's in its data.
	if code, stdout, _ := runMain("outline", "gone.go", "gone2.go"); code != ExitUsage || !strings.Contains(stdout, "no_such_file") || !strings.Contains(stdout, "gone2.go") {
		t.Errorf("outline of only missing files: exit %d\n%s", code, stdout)
	}
	code, stdout, _ = runMain("outline", "sym.go", "gone.go", "--format", "text")
	if code != ExitProblems || !strings.Contains(stdout, "sym.go:9: struct Greeter") || !strings.Contains(stdout, "# gone.go: ") {
		t.Errorf("text outline with a missing file: exit %d\n%s", code, stdout)
	}
}

// symbolSourceData decodes `source` output.
func symbolSourceData(t *testing.T, stdout string) (sourceData, rawEnvelope) {
	t.Helper()
	var data sourceData
	env := decodeData(t, stdout, &data)
	return data, env
}

// TestSourceIsByteExact: the text of a symbol, including its doc comment, is
// what is in the file to the byte, however many CJK characters and emoji the
// UTF-16 columns of its range count differently.
func TestSourceIsByteExact(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)

	code, stdout, stderr := runMain("source", "sym.go::Greeter.Greet#method")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	data, env := symbolSourceData(t, stdout)
	if !env.OK || data.Count != 1 {
		t.Fatalf("envelope = %+v, count %d", env, data.Count)
	}
	got := data.Symbols[0]
	if got.Source != greetSource {
		t.Errorf("source =\n%q\nwant\n%q", got.Source, greetSource)
	}
	if got.ID != "sym.go::Greeter.Greet#method" || got.File != "sym.go" || got.Kind != "method" {
		t.Errorf("identity = %q %q %q", got.ID, got.File, got.Kind)
	}
	if got.Line != 13 || got.EndLine != 16 || got.TotalLines != 4 || got.Truncated {
		t.Errorf("lines %d-%d of %d, truncated %v; want 13-16 of 4, false (the doc comment line is included)",
			got.Line, got.EndLine, got.TotalLines, got.Truncated)
	}
	if got.Hash != sha(greetSource) {
		t.Errorf("hash = %s, want the sha256 of the symbol's text", got.Hash)
	}
	if !utf8.ValidString(got.Source) {
		t.Errorf("the source is not valid UTF-8")
	}

	// A struct's doc comment is a comment too, and its body is all of it.
	code, stdout, _ = runMain("source", "sym.go::Greeter#struct")
	data, _ = symbolSourceData(t, stdout)
	if want := "// Greeter says hello. 挨拶 🎉\ntype Greeter struct {\n\tName string\n}"; code != ExitOK || data.Symbols[0].Source != want {
		t.Errorf("Greeter source = %q, want %q", data.Symbols[0].Source, want)
	}
}

// TestSourceContextAndCaps: --context adds surrounding lines without changing
// the hash; --max-lines and --max-bytes cut, say so, and never split a
// character.
func TestSourceContextAndCaps(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)
	const id = "sym.go::Greeter.Greet#method"

	_, stdout, _ := runMain("source", id, "--context", "1")
	data, _ := symbolSourceData(t, stdout)
	got := data.Symbols[0]
	// Line and EndLine keep describing the symbol; the context is in Source,
	// which says where it starts and how much of it there is.
	if want := "\n" + greetSource + "\n"; got.Source != want || got.Line != 13 || got.EndLine != 16 ||
		got.SourceLine != 12 || got.ReturnedLines != 6 || got.ReturnedBytes != len(want) {
		t.Errorf("--context 1: source %q lines %d-%d (source from %d, %d lines, %d bytes), want %q lines 13-16 from 12, 6 lines",
			got.Source, got.Line, got.EndLine, got.SourceLine, got.ReturnedLines, got.ReturnedBytes, want)
	}
	if got.Hash != sha(greetSource) {
		t.Errorf("the hash changed with --context: it must be of the symbol alone")
	}

	code, stdout, _ := runMain("source", id, "--max-lines", "2")
	data, env := symbolSourceData(t, stdout)
	got = data.Symbols[0]
	if code != ExitOK || !got.Truncated || got.Source != strings.Join(strings.Split(greetSource, "\n")[:2], "\n") ||
		got.Line != 13 || got.EndLine != 16 || got.ReturnedLines != 2 || got.ReturnedBytes != len(got.Source) ||
		got.TotalLines != 4 || got.Hash != sha(greetSource) {
		t.Errorf("--max-lines 2: exit %d, %+v; line/end_line must still describe the whole symbol (13-16) and returned_lines the slice (2)", code, got)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "2 of its 4 lines returned") {
		t.Errorf("the cut is not in the warnings: %v", env.Warnings)
	}
	// A cap that takes only the --context lines leaves the symbol whole, and
	// the warning does not say it was cut ("2 of 2 lines" was the old wording).
	code, stdout, _ = runMain("source", id, "--context", "1", "--max-lines", "5")
	data, env = symbolSourceData(t, stdout)
	got = data.Symbols[0]
	if code != ExitOK || !got.Truncated || got.ReturnedLines != 5 || got.Line != 13 || got.EndLine != 16 ||
		!strings.Contains(strings.Join(env.Warnings, "\n"), "the --context lines were cut at the cap; the symbol's 4 lines are complete") {
		t.Errorf("--context 1 --max-lines 5: exit %d, %+v, warnings %v", code, got, env.Warnings)
	}

	// The cap lands in the middle of the emoji: it is dropped, not split.
	mid := strings.Index(greetSource, "👋") + 2
	_, stdout, _ = runMain("source", id, "--max-bytes", strconv.Itoa(mid))
	data, _ = symbolSourceData(t, stdout)
	got = data.Symbols[0]
	if !got.Truncated || !utf8.ValidString(got.Source) || got.Source != greetSource[:strings.Index(greetSource, "👋")] {
		t.Errorf("--max-bytes into an emoji: %q (valid %v, truncated %v)", got.Source, utf8.ValidString(got.Source), got.Truncated)
	}
	// A cap the symbol fits in is not a truncation.
	_, stdout, _ = runMain("source", id, "--max-bytes", "10000", "--max-lines", "100")
	if data, _ = symbolSourceData(t, stdout); data.Symbols[0].Truncated || data.Symbols[0].Source != greetSource {
		t.Errorf("a cap that was not reached truncated: %+v", data.Symbols[0])
	}
	if code, _, _ := runMain("source", id, "--max-lines", "-1"); code != ExitUsage {
		t.Errorf("a negative cap: exit %d, want %d", code, ExitUsage)
	}
}

// TestSourceSeveralAndByLocation: several ids in one call, a location naming
// the symbol that contains it (through the UTF-16 conversion), and a batch
// where one target has gone stale still returns the others.
func TestSourceSeveralAndByLocation(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)

	// Line 27, byte column 21: the name of the second variable, which is
	// after a CJK identifier, so its byte and UTF-16 columns differ.
	code, stdout, stderr := runMain("source", "sym.go::Long#function", "sym.go:27:21", "sym.go::init#function~2")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	data, _ := symbolSourceData(t, stdout)
	var ids []string
	for _, s := range data.Symbols {
		ids = append(ids, s.ID)
	}
	if want := []string{"sym.go::Long#function", "sym.go::名前#variable", "sym.go::init#function~2"}; !slices.Equal(ids, want) {
		t.Errorf("ids = %q, want %q, in the order asked", ids, want)
	}
	if want := "func Long(a int,\n\tb int) int {\n\treturn a + b\n}"; data.Symbols[0].Source != want {
		t.Errorf("Long = %q, want %q", data.Symbols[0].Source, want)
	}

	// One stale id among good ones: the good ones are returned, the stale one
	// is reported, and the exit code says there was a problem.
	code, stdout, _ = runMain("source", "sym.go::Long#function", "sym.go::Gone#function")
	data, env := symbolSourceData(t, stdout)
	if code != ExitProblems || !env.OK || data.Count != 1 || len(data.Errors) != 1 ||
		data.Errors[0].Code != "stale_id" || data.Errors[0].Target != "sym.go::Gone#function" {
		t.Errorf("partial batch: exit %d, count %d, errors %+v", code, data.Count, data.Errors)
	}

	// All stale: the envelope fails, with the error of the one target.
	code, stdout, _ = runMain("source", "sym.go::Gone#function")
	env2 := decodeEnvelope(t, stdout)
	if code != ExitProblems || env2.OK || env2.Error == nil || env2.Error.Code != "stale_id" {
		t.Errorf("a single stale id: exit %d, %+v", code, env2.Error)
	}

	// A location with no symbol at it is not_found, not a guess.
	code, stdout, _ = runMain("source", "sym.go:2:1")
	if env := decodeEnvelope(t, stdout); code != ExitProblems || env.OK || env.Error.Code != "not_found" {
		t.Errorf("a location inside no symbol: exit %d\n%s", code, stdout)
	}
}

// TestContext: one call for the symbol, its file's import block and the
// server's hover.
func TestContext(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)

	code, stdout, stderr := runMain("context", "sym.go::Greeter.Greet#method")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data contextData
	okData(t, stdout, &data)
	if data.Source != greetSource || data.ID != "sym.go::Greeter.Greet#method" || data.Hash != sha(greetSource) {
		t.Errorf("the symbol part = %+v", data.sourceItem)
	}
	if data.Header == nil || data.Header.Line != 1 || data.Header.EndLine != 6 ||
		data.Header.Source != "package sym\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)" || data.Header.Truncated {
		t.Errorf("header = %+v, want lines 1-6: the package clause and the import block", data.Header)
	}
	if data.Hover != "func (*Greeter) Greet() string" || data.Signature != "func() string" {
		t.Errorf("hover %q, signature %q", data.Hover, data.Signature)
	}

	_, stdout, _ = runMain("context", "sym.go::Long#function", "--max-header-lines", "2")
	env := okData(t, stdout, &data)
	if data.Header == nil || !data.Header.Truncated || data.Header.EndLine != 2 || data.Header.Source != "package sym\n" {
		t.Errorf("--max-header-lines 2: header = %+v", data.Header)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "header was cut") {
		t.Errorf("the cut header is not in the warnings: %v", env.Warnings)
	}
	// A signature the server did not give is sliced from the file.
	if data.Signature != "func Long(a int, b int) int" {
		t.Errorf("signature = %q", data.Signature)
	}

	// A server without hover still answers; it says so.
	scenario{
		capabilities: map[string]any{"documentSymbolProvider": true, "textDocumentSync": 1},
		results:      map[string]any{methodDocumentSymbol: symHierarchical()},
	}.apply(t)
	code, stdout, _ = runMain("context", "sym.go::Long#function")
	data = contextData{}
	env = okData(t, stdout, &data)
	if code != ExitOK || data.Hover != "" || !strings.Contains(strings.Join(env.Warnings, "\n"), "does not advertise hover") {
		t.Errorf("a server without hover: exit %d, hover %q, warnings %v", code, data.Hover, env.Warnings)
	}
}

// TestContextHoverDedupesAPlainSignature: a hover that is nothing but the
// declaration's own signature (an undocumented symbol, most servers) is
// omitted — it is already `signature` and source's first line — but a hover
// that says more survives whole (docs/DECISIONS.md D47).
func TestContextHoverDedupesAPlainSignature(t *testing.T) {
	symWorkspace(t, nil)
	scenario{results: map[string]any{
		methodDocumentSymbol: symHierarchical(),
		methodHover:          map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nfunc Long(a int, b int) int\n```"}},
	}}.apply(t)

	code, stdout, stderr := runMain("context", "sym.go::Long#function")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}
	var data contextData
	env := okData(t, stdout, &data)
	if data.Hover != "" {
		t.Errorf("hover identical to the signature must be omitted, got %q", data.Hover)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "hover omitted") {
		t.Errorf("the omission must be warned about: %v", env.Warnings)
	}

	// A hover that says more than the signature is kept, whole.
	scenario{results: map[string]any{
		methodDocumentSymbol: symHierarchical(),
		methodHover:          map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nfunc Long(a int, b int) int\n```\n\nLong does more."}},
	}}.apply(t)
	code, stdout, _ = runMain("context", "sym.go::Long#function")
	data = contextData{}
	okData(t, stdout, &data)
	if !strings.Contains(data.Hover, "Long does more.") {
		t.Errorf("a hover with real content must survive: %q", data.Hover)
	}
}

// errorData is an error envelope's data, as a map.
func errorData(t *testing.T, stdout string) (code string, data map[string]any) {
	t.Helper()
	var env rawEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("not an envelope: %v\n%s", err, stdout)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("envelope is ok: %s", stdout)
	}
	return env.Error.Code, env.Error.Data
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

func candidateIDs(data map[string]any) []string {
	var out []string
	list, _ := data["candidates"].([]any)
	for _, c := range list {
		m, _ := c.(map[string]any)
		if id, ok := m["id"].(string); ok {
			out = append(out, id)
		} else if f, ok := m["file"].(string); ok {
			out = append(out, f)
		}
	}
	return out
}

// TestStaleIDsAreErrorsWithCandidates: an id that no longer names anything is
// refused with the nearest candidates and never resolved to one of them.
func TestStaleIDsAreErrorsWithCandidates(t *testing.T) {
	symWorkspace(t, map[string]string{"other/sym.go": "package other\n"})
	symScenario(t, symHierarchical(), nil)

	for _, tc := range []struct {
		name string
		id   string
		want string // a candidate that must be named first
	}{
		{"a typo", "sym.go::Greeter.Greeet#method", "sym.go::Greeter.Greet#method"},
		{"the wrong kind", "sym.go::Greeter.Greet#function", "sym.go::Greeter.Greet#method"},
		{"a duplicate that is not there", "sym.go::init#function~3", "sym.go::init#function"},
		{"a symbol that was deleted", "sym.go::Long2#function", "sym.go::Long#function"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, cmd := range [][]string{{"source", tc.id}, {"context", tc.id}, {"references", "--id", tc.id}} {
				code, stdout, _ := runMain(cmd...)
				got, data := errorData(t, stdout)
				if code != ExitProblems || got != "stale_id" {
					t.Fatalf("%v: exit %d, code %q\n%s", cmd, code, got, stdout)
				}
				if data["id"] != tc.id {
					t.Errorf("%v: error data id = %v, want %q", cmd, data["id"], tc.id)
				}
				cands := candidateIDs(data)
				if len(cands) == 0 || cands[0] != tc.want {
					t.Errorf("%v: candidates = %q, want %q first", cmd, cands, tc.want)
				}
				if !strings.Contains(stdout, tc.want) {
					t.Errorf("%v: the message does not name %q", cmd, tc.want)
				}
			}
		})
	}
	// A file that moved: the candidates are the files of the same name.
	for _, cmd := range [][]string{{"source", "gone/sym.go::Greeter#struct"}, {"references", "--id", "gone/sym.go::Greeter#struct"}} {
		code, stdout, _ := runMain(cmd...)
		got, data := errorData(t, stdout)
		if cands := candidateIDs(data); code != ExitProblems || got != "stale_id" ||
			!slices.Equal(sortedCopy(cands), []string{"other/sym.go", "sym.go"}) {
			t.Errorf("%v: exit %d, code %q, candidates %q, want both files called sym.go", cmd, code, got, cands)
		}
	}
}

// TestMalformedAndEscapingIDs are refused before anything is read.
func TestMalformedAndEscapingIDs(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)
	spawns, _ := spawnLog(t)

	for _, tc := range []struct {
		id       string
		wantCode string
		wantExit int
	}{
		{"sym.go::Greeter", "usage", ExitUsage},
		{"sym.go::#struct", "usage", ExitUsage},
		{"sym.go::init#function~1", "usage", ExitUsage},
		{"../sym.go::Greeter#struct", "outside_workspace", ExitUsage},
		{"a/../../sym.go::Greeter#struct", "outside_workspace", ExitUsage},
		{"/etc/passwd::root#variable", "outside_workspace", ExitUsage},
	} {
		code, stdout, _ := runMain("references", "--id", tc.id)
		got, _ := errorData(t, stdout)
		if code != tc.wantExit || got != tc.wantCode {
			t.Errorf("--id %q: exit %d code %q, want %d %q", tc.id, code, got, tc.wantExit, tc.wantCode)
		}
	}
	if n := spawns(); n != 0 {
		t.Errorf("%d language server(s) were started for ids that are refused on sight", n)
	}
}

// TestEveryLocationCommandTakesAnID: --id is accepted wherever a location is,
// and rejected alongside one.
func TestEveryLocationCommandTakesAnID(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)
	const stale = "gone.go::X#function"

	for _, c := range commands {
		if !slices.Contains([]string{methodDefinition, methodReferences, methodImplementation, methodHover,
			methodRename, methodCodeAction, methodPrepareCallHierarchy}, c.Method) {
			continue
		}
		// dead_code asks a question of a directory, not of a position; it
		// needs the references capability and has no location to name.
		if c.Name == "dead_code" {
			continue
		}
		args := []string{c.Name, "--id", stale}
		if c.Name == "rename" {
			args = append(args, "New")
		}
		// Reaching the id's own resolution (and its own error) is what
		// proves the flag is understood: an unknown flag would be a usage
		// error, and a missing location one too.
		code, stdout, _ := runMain(args...)
		if got, _ := errorData(t, stdout); got != "stale_id" || code != ExitProblems {
			t.Errorf("%s --id: exit %d, code %q, want stale_id", c.Name, code, got)
		}

		both := []string{c.Name, "--id", stale, "sym.go:1:1"}
		if c.Name == "rename" {
			both = append(both, "New")
		}
		if code, stdout, _ := runMain(both...); code != ExitUsage || !strings.Contains(stdout, "both name a position") {
			t.Errorf("%s with an id and a location: exit %d\n%s", c.Name, code, stdout)
		}
		if code, _, _ := runMain(c.Name, "--id", stale, "--symbol", "X"); code != ExitUsage {
			t.Errorf("%s with an id and a symbol: exit %d, want %d", c.Name, code, ExitUsage)
		}
	}
}

// TestIDResolvesToTheSymbolsPosition: --id is a position, exactly as if the
// caller had typed the location of the symbol's name.
func TestIDResolvesToTheSymbolsPosition(t *testing.T) {
	symWorkspace(t, nil)
	symScenario(t, symHierarchical(), map[string]any{methodDefinition: json.RawMessage(positionEcho)})

	// Greet's name is on line 14 (1-based), after `func (g *Greeter) `.
	col := strings.LastIndex(strings.Split(symSource, "\n")[13], "Greet") + 1
	code, stdout, stderr := runMain("definition", "--id", "sym.go::Greeter.Greet#method")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	got := decodeResults(t, stdout).Results[0]
	if got.Start.Line != 14 || got.Start.Column != col {
		t.Errorf("the server was asked about %d:%d, want 14:%d (the symbol's name)", got.Start.Line, got.Start.Column, col)
	}
	// The second duplicate is the second one, by position.
	_, stdout, _ = runMain("definition", "--id", "sym.go::init#function~2")
	if got := decodeResults(t, stdout).Results[0]; got.Start.Line != 20 {
		t.Errorf("init~2 is at line %d, want 20", got.Start.Line)
	}
	// The name after a CJK identifier is a byte column, not a UTF-16 one.
	_, stdout, _ = runMain("definition", "--id", "sym.go::名前#variable")
	if got := decodeResults(t, stdout).Results[0]; got.Start.Line != 27 || got.Start.Column != 21 {
		t.Errorf("名前 is at %d:%d, want 27:21", got.Start.Line, got.Start.Column)
	}
}

// TestFlatIDResolvesToTheSymbolsName: a server that answers documentSymbol with
// flat SymbolInformation — as real gopls does for a client that does not
// advertise hierarchical support — puts the start of the whole declaration (the
// `func` keyword, column 1) in the location's range and has no selectionRange.
// --id must still hand the server the position of the symbol's name, or
// definition, references, hover and call_hierarchy find no identifier.
func TestFlatIDResolvesToTheSymbolsName(t *testing.T) {
	_, file := symWorkspace(t, nil)
	symScenario(t, symFlat(file), map[string]any{methodDefinition: json.RawMessage(positionEcho)})

	lines := strings.Split(symSource, "\n")
	col := func(line int, sub string) int { return strings.LastIndex(lines[line], sub) + 1 }
	for _, want := range []struct {
		id         string
		line, byte int
	}{
		// After the receiver, not on it: `func (g *Greeter) Greet`.
		{"sym.go::Greeter.Greet#method", 14, col(13, "Greet")},
		{"sym.go::Greeter#struct", 9, col(8, "Greeter")},
		{"sym.go::Greeter.Name#field", 10, col(9, "Name")},
		{"sym.go::init#function", 18, col(17, "init")},
		{"sym.go::init#function~2", 20, col(19, "init")},
		{"sym.go::Long#function", 22, col(21, "Long")},
		// Byte columns after UTF-16 ones: two CJK identifiers on one line.
		{"sym.go::変数#variable", 27, col(26, "変数")},
		{"sym.go::名前#variable", 27, col(26, "名前")},
	} {
		code, stdout, stderr := runMain("definition", "--id", want.id)
		if code != ExitOK {
			t.Errorf("%s: exit %d; stderr: %s\nstdout: %s", want.id, code, stderr, stdout)
			continue
		}
		got := decodeResults(t, stdout).Results[0]
		if got.Start.Line != want.line || got.Start.Column != want.byte {
			t.Errorf("%s: the server was asked about %d:%d, want %d:%d (the name, not the declaration's start)",
				want.id, got.Start.Line, got.Start.Column, want.line, want.byte)
		}
	}

	// And `symbols` reports the same position for a flat server, so that the
	// location it prints is one that can be fed back.
	_, stdout, _ := runMain("symbols", "sym.go")
	for _, r := range decodeResults(t, stdout).Results {
		if r.ID == "sym.go::Greeter.Greet#method" && (r.Start.Line != 14 || r.Start.Column != col(13, "Greet")) {
			t.Errorf("symbols puts Greet at %d:%d, want 14:%d", r.Start.Line, r.Start.Column, col(13, "Greet"))
		}
	}
}

// TestPinpointNames: the name is found after a receiver that spells it, and a
// name that is not in its declaration is counted rather than guessed.
func TestPinpointNames(t *testing.T) {
	src := "func (Apply *Stage) Apply() {}\nfunc F[T any](T T) {}\n"
	m := protocol.NewMapper("file:///x.go", []byte(src))
	syms := []symbol{
		{Name: "(*Stage).Apply", Flat: true, Full: protocol.Range{End: protocol.Position{Character: 30}}},
		{Name: "T", Flat: true, Full: protocol.Range{Start: protocol.Position{Line: 1}, End: protocol.Position{Line: 1, Character: 21}}},
		{Name: "gone", Flat: true, Full: protocol.Range{End: protocol.Position{Character: 30}}},
		{Name: "Apply", Full: protocol.Range{End: protocol.Position{Character: 30}}}, // hierarchical: untouched
	}
	for i := range syms {
		syms[i].Range = syms[i].Full
	}
	warnings := pinpointNames(m, syms)
	if got := syms[0].Range.Start.Character; got != 20 {
		t.Errorf("Apply is at column %d, want 20 (the method, not the receiver named Apply)", got)
	}
	// `T` is a type parameter and a parameter's name: the first whole word
	// outside parentheses is the one after `F[`.
	if got := syms[1].Range.Start; got.Line != 1 || got.Character != 7 {
		t.Errorf("T is at %d:%d, want 1:7", got.Line, got.Character)
	}
	if syms[2].Range != syms[2].Full {
		t.Errorf("a name that is not in its declaration moved to %+v", syms[2].Range)
	}
	if syms[3].Range != syms[3].Full {
		t.Errorf("a hierarchical symbol moved to %+v", syms[3].Range)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "1 symbols' names could not be located") {
		t.Errorf("warnings = %q, want one saying 1 name was not located", warnings)
	}
}

// TestWorkspaceSymbolAndCallHierarchyCarryIDs: symbols that arrive without
// their file's outline still get the id that outline would give them — the
// second `init` is `~2`, which nothing but the outline of its file can say.
func TestWorkspaceSymbolAndCallHierarchyCarryIDs(t *testing.T) {
	dir, file := symWorkspace(t, nil)
	greetSel, initSel, longSel := nameRange(13, "Greet"), nameRange(19, "init"), nameRange(21, "Long")
	at := func(name string, kind int, sel map[string]any) map[string]any {
		return map[string]any{"name": name, "kind": kind, "location": map[string]any{"uri": "file://" + file, "range": sel}}
	}
	item := func(name string, kind int, sel map[string]any) map[string]any {
		return map[string]any{"name": name, "kind": kind, "uri": "file://" + file, "range": sel, "selectionRange": sel}
	}
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodDocumentSymbol:  symHierarchical(),
			methodWorkspaceSymbol: []any{at("init", 12, initSel), at("Greet", 6, greetSel)},
			methodPrepareCallHierarchy: []any{
				item("Greet", 6, greetSel),
			},
		},
		calls: map[string]any{
			"Greet": map[string]any{
				"incoming": []any{incomingCall(item("Long", 12, longSel), callRange(21, 5, 9))},
				"outgoing": []any{outgoingCall(item("init", 12, initSel), callRange(14, 5, 9))},
			},
		},
	}.apply(t)

	code, stdout, stderr := runMain("workspace_symbol", "i", "--path", dir)
	if code != ExitOK {
		t.Fatalf("workspace_symbol: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var ids []string
	for _, r := range decodeResults(t, stdout).Results {
		ids = append(ids, r.ID)
	}
	if want := []string{"sym.go::init#function~2", "sym.go::Greeter.Greet#method"}; !slices.Equal(ids, want) {
		t.Errorf("workspace_symbol ids = %q, want %q", ids, want)
	}

	code, stdout, stderr = runMain("call_hierarchy", "--id", "sym.go::Greeter.Greet#method", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("call_hierarchy: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	ids = nil
	for _, r := range decodeResults(t, stdout).Results {
		ids = append(ids, r.ID)
	}
	if want := []string{"sym.go::Long#function", "sym.go::init#function~2"}; !slices.Equal(ids, want) {
		t.Errorf("call_hierarchy ids = %q, want %q", ids, want)
	}
}
