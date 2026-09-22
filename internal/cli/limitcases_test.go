package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// TestEveryListCommandHonoursLimit is D42's guarantee, enforced over the command
// table: a command that returns a list has a `--limit` that bounds it, says
// `truncated:true` when it bit and reports the total it cut from — or it is
// listed with the reason it is not bounded. A command in the table with neither
// fails here, so adding a list command without a limit is a decision somebody
// has to write down (registerLimitCase / registerNoLimit, from the command's own
// test file).

// A limitCase drives one list-returning command over a fixture whose answer has
// at least four rows. TestEveryListCommandHonoursLimit runs Invoke twice, with
// no extra arguments and with `--limit 2`, and requires the second answer to
// hold exactly two rows, say truncated:true and report the total. A command that
// returns one object rather than a list is registered with registerNoLimit and
// the reason.
type limitCase struct {
	// Invoke runs the command (with a fixture of its own) and returns stdout.
	Invoke func(t *testing.T, extra ...string) string
	// Rows is the key of the row array in the envelope's data ("results" for a
	// result-set command). A dotted path descends through arrays and sums their
	// elements' rows: "files.symbols" is every file's symbols.
	Rows string
	// Total is the key of the count before the limit; "total" when empty. It and
	// `truncated` are read from the object that holds the rows (the top level
	// of data, or the nested object a dotted Rows names).
	Total string
}

var (
	limitCases   = map[string]limitCase{}
	noLimitCases = map[string]string{}
)

// registerLimitCase declares how to exercise command's --limit. Call it from an
// init in the command's own test file.
func registerLimitCase(command string, c limitCase) { limitCases[command] = c }

// registerNoLimit declares that command returns no list a --limit could bound,
// and says why.
func registerNoLimit(command, reason string) { noLimitCases[command] = reason }

// rowCount counts the rows at path in data (see limitCase.Rows). A path steps
// through objects by key and through arrays by summing what each element holds.
func rowCount(data any, path string) int {
	head, rest, more := strings.Cut(path, ".")
	obj, ok := data.(map[string]any)
	if !ok {
		return -1
	}
	v, ok := obj[head]
	if !ok {
		return -1
	}
	if !more {
		arr, ok := v.([]any)
		if !ok {
			return -1
		}
		return len(arr)
	}
	switch v := v.(type) {
	case map[string]any:
		return rowCount(v, rest)
	case []any:
		sum := 0
		for _, el := range v {
			if n := rowCount(el, rest); n > 0 {
				sum += n
			}
		}
		return sum
	}
	return -1
}

// rowHolder is the object that holds the rows array at path when the path goes
// through objects only: a nested list carries its own `total` and `truncated`
// beside its rows (`semantic.rows` with `semantic.total`). It is nil when the
// path passes through an array, and the answer's own top level speaks then.
func rowHolder(data any, path string) map[string]any {
	obj, ok := data.(map[string]any)
	if !ok {
		return nil
	}
	head, rest, more := strings.Cut(path, ".")
	if !more {
		return obj
	}
	next, ok := obj[head].(map[string]any)
	if !ok {
		return nil
	}
	return rowHolder(next, rest)
}

func TestEveryListCommandHonoursLimit(t *testing.T) {
	for _, c := range commands {
		_, hasCase := limitCases[c.Name]
		reason, hasNo := noLimitCases[c.Name]
		switch {
		case hasCase && hasNo:
			t.Errorf("%s is registered both as bounded and as not bounded", c.Name)
		case !hasCase && !hasNo:
			t.Errorf("%s has no limit case and no reason for lacking one: a command that returns a list honours --limit "+
				"(truncated:true and the total when it bites); register it with registerLimitCase, or with registerNoLimit and the reason", c.Name)
		case hasNo && strings.TrimSpace(reason) == "":
			t.Errorf("%s is registered as not bounded without a reason", c.Name)
		}
	}
	names := make([]string, 0, len(limitCases))
	for name := range limitCases {
		if lookupCommand(name) == nil {
			t.Errorf("limit case for %q, which is not a command", name)
			continue
		}
		names = append(names, name)
	}
	for name := range noLimitCases {
		if lookupCommand(name) == nil {
			t.Errorf("no-limit reason for %q, which is not a command", name)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		lc := limitCases[name]
		t.Run(name, func(t *testing.T) {
			totalKey := lc.Total
			if totalKey == "" {
				totalKey = "total"
			}
			full := limitData(t, lc.Invoke(t))
			if n := rowCount(full, lc.Rows); n < 4 {
				t.Fatalf("the fixture answers %d row(s) in %q; a limit case needs at least 4:\n%v", n, lc.Rows, full)
			}

			stdout := lc.Invoke(t, "--limit", "2")
			env := decodeEnvelope(t, stdout)
			cut := limitData(t, stdout)
			if n := rowCount(cut, lc.Rows); n != 2 {
				t.Errorf("--limit 2 listed %d row(s) in %q, want exactly 2", n, lc.Rows)
			}
			obj := cut.(map[string]any)
			if holder := rowHolder(cut, lc.Rows); holder != nil {
				obj = holder
			}
			if truncated, _ := obj["truncated"].(bool); !truncated {
				t.Errorf("--limit 2 cut the list but the answer does not say truncated:true")
			}
			if total, _ := obj[totalKey].(float64); total < 4 {
				t.Errorf("--limit 2 reports %s = %v, want the total before the cut (at least 4)", totalKey, obj[totalKey])
			}
			if !slices.ContainsFunc(env.Warnings, func(w string) bool {
				return strings.Contains(w, "truncated") || strings.Contains(w, "listed 2 of") || strings.Contains(w, "lists 2 of")
			}) {
				t.Errorf("--limit 2 cut the list without a warning that says so and how to widen it: %q", env.Warnings)
			}
		})
	}
}

// limitData decodes an envelope's data, failing on an error envelope.
func limitData(t *testing.T, stdout string) any {
	t.Helper()
	var env struct {
		OK    bool `json:"ok"`
		Data  any  `json:"data"`
		Error *struct {
			Code, Message string
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not an envelope: %v\n%s", err, stdout)
	}
	if !env.OK {
		t.Fatalf("the command failed: %+v\n%s", env.Error, stdout)
	}
	return env.Data
}

// --- fixtures ---

// limitWorkspace is a workspace with more than four of everything the whole-repo
// commands list: importers of util (five packages), imports of one file (five),
// graph edges, four import cycles, symbols named Item…, files and directories.
func limitWorkspace(t *testing.T) string {
	t.Helper()
	files := map[string]string{
		"util/util.go": "package util\n\nfunc Helper() {}\n",
		"all/all.go": "package all\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"sort\"\n\t\"strings\"\n\t\"fixture/util\"\n)\n\n" +
			"func ItemAll() { fmt.Println(os.Args, sort.Ints, strings.ToLower, util.Helper) }\n",
	}
	for _, p := range []string{"pa", "pb", "pc", "pd", "pe"} {
		files[p+"/x.go"] = fmt.Sprintf("package %s\n\nimport \"fixture/util\"\n\nfunc Item%s() { util.Helper() }\n", p, strings.ToUpper(p))
	}
	for _, c := range []string{"c1", "c2", "c3", "c4"} {
		files[c+"a/x.go"] = fmt.Sprintf("package %sa\n\nimport \"fixture/%sb\"\n\nvar _ = %sb.X\n", c, c, c)
		files[c+"b/x.go"] = fmt.Sprintf("package %sb\n\nimport \"fixture/%sa\"\n\nvar X = %sa.X\n", c, c, c)
	}
	return idxWorkspace(t, files)
}

// wholeRepoCase is a limit case for a command that runs against limitWorkspace.
func wholeRepoCase(rows, total string, args ...string) limitCase {
	return limitCase{Rows: rows, Total: total, Invoke: func(t *testing.T, extra ...string) string {
		t.Helper()
		limitWorkspace(t)
		_, stdout, stderr := runMain(append(slices.Clone(args), extra...)...)
		if stdout == "" {
			t.Fatalf("%v printed nothing; stderr: %s", args, stderr)
		}
		return stdout
	}}
}

// limitLSPFile is a Go file of six functions in a module, for the commands a
// scripted server answers.
func limitLSPFile(t *testing.T) (dir, file string) {
	t.Helper()
	dir = tree(t, map[string]string{
		"a.go": "package a\n\nfunc F1() {}\nfunc F2() {}\nfunc F3() {}\nfunc F4() {}\nfunc F5() {}\nfunc F6() {}\n",
	})
	return dir, filepath.Join(dir, "a.go")
}

// limitLocations are six locations, one per function name.
func limitLocations(file string) []any {
	var out []any
	for line := 2; line < 8; line++ {
		out = append(out, loc(file, line, 5, 7))
	}
	return out
}

func limitDocSymbols() []any {
	rng := func(line, from, to int) map[string]any {
		return map[string]any{
			"start": map[string]any{"line": line, "character": from},
			"end":   map[string]any{"line": line, "character": to},
		}
	}
	var out []any
	for i := 0; i < 6; i++ {
		out = append(out, map[string]any{
			"name": fmt.Sprintf("F%d", i+1), "kind": 12,
			"range": rng(i+2, 0, 13), "selectionRange": rng(i+2, 5, 7),
		})
	}
	return out
}

// lspCase is a limit case for a command a scripted server answers.
func lspCase(rows string, build func(t *testing.T) (scenario, []string)) limitCase {
	return limitCase{Rows: rows, Invoke: func(t *testing.T, extra ...string) string {
		t.Helper()
		sc, args := build(t)
		sc.apply(t)
		_, stdout, stderr := runMain(append(slices.Clone(args), extra...)...)
		if stdout == "" {
			t.Fatalf("%v printed nothing; stderr: %s", args, stderr)
		}
		return stdout
	}}
}

func init() {
	// Whole-repo commands, from the index and the working tree.
	registerLimitCase("search_symbols", wholeRepoCase("results", "total", "search_symbols", "Item"))
	registerLimitCase("repo_map", wholeRepoCase("files", "files_total", "repo_map"))
	registerLimitCase("find_importers", wholeRepoCase("importers", "total", "find_importers", "util"))
	registerLimitCase("imports", wholeRepoCase("imports", "total", "imports", "all/all.go"))
	registerLimitCase("dependency_graph", wholeRepoCase("edges", "total", "dependency_graph"))
	registerLimitCase("dependency_cycles", wholeRepoCase("cycles", "total", "dependency_cycles"))
	registerLimitCase("tree", wholeRepoCase("files", "total", "tree"))
	registerLimitCase("repo_outline", wholeRepoCase("directories", "total_directories", "repo_outline"))
	registerLimitCase("search_text", wholeRepoCase("matches", "total", "search_text", "package"))

	// Commands a language server answers.
	for name, method := range map[string]string{"definition": methodDefinition, "references": methodReferences, "implementation": methodImplementation} {
		registerLimitCase(name, lspCase("results", func(t *testing.T) (scenario, []string) {
			_, file := limitLSPFile(t)
			return scenario{results: map[string]any{method: limitLocations(file)}}, []string{name, file + ":3:6"}
		}))
	}
	registerLimitCase("symbols", lspCase("results", func(t *testing.T) (scenario, []string) {
		_, file := limitLSPFile(t)
		return scenario{results: map[string]any{methodDocumentSymbol: limitDocSymbols()}}, []string{"symbols", file}
	}))
	registerLimitCase("outline", lspCase("files.symbols", func(t *testing.T) (scenario, []string) {
		_, file := limitLSPFile(t)
		return scenario{results: map[string]any{methodDocumentSymbol: limitDocSymbols()}}, []string{"outline", file}
	}))
	registerLimitCase("workspace_symbol", lspCase("results", func(t *testing.T) (scenario, []string) {
		dir, file := limitLSPFile(t)
		var syms []any
		for line := 2; line < 8; line++ {
			syms = append(syms, map[string]any{
				"name": fmt.Sprintf("F%d", line-1), "kind": 12,
				"location": loc(file, line, 5, 7),
			})
		}
		return scenario{results: map[string]any{methodWorkspaceSymbol: syms}}, []string{"workspace_symbol", "F", "--path", dir}
	}))
	registerLimitCase("codeaction", lspCase("results", func(t *testing.T) (scenario, []string) {
		_, file := limitLSPFile(t)
		var actions []any
		for i := 1; i <= 6; i++ {
			actions = append(actions, map[string]any{"title": fmt.Sprintf("Action %d", i), "kind": "quickfix"})
		}
		return scenario{capabilities: mutationServerCaps(nil), results: map[string]any{methodCodeAction: actions}},
			[]string{"codeaction", file + ":3:6"}
	}))
	registerLimitCase("call_hierarchy", lspCase("results", func(t *testing.T) (scenario, []string) {
		_, file := limitLSPFile(t)
		var incoming []any
		for line := 3; line < 9; line++ {
			incoming = append(incoming, incomingCall(callItemJSON(fmt.Sprintf("caller%d", line), 12, file, line, 5, 7)))
		}
		return scenario{
			capabilities: m5Capabilities(nil),
			results:      map[string]any{methodPrepareCallHierarchy: []any{callItemJSON("F1", 12, file, 2, 5, 7)}},
			calls:        map[string]any{"F1": map[string]any{"incoming": incoming}},
		}, []string{"call_hierarchy", file + ":3:6", "--direction", "incoming", "--settle", "20ms"}
	}))
	registerLimitCase("check", lspCase("diagnostics", func(t *testing.T) (scenario, []string) {
		dir, main, _ := checkFixture(t)
		var diags []any
		for line := 0; line < 6; line++ {
			diags = append(diags, diagnostic(line, 0, 1, 2, fmt.Sprintf("warning %d", line), "compiler", "w"))
		}
		return scenario{capabilities: m5Capabilities(nil), diagnostics: map[string]any{main: diags}},
			[]string{"check", dir, "--settle", "20ms"}
	}))

	// Commands whose answer is not a list a limit could bound.
	registerNoLimit("hover", "answers one hover text, not a list")
	registerNoLimit("rename", "a preview must show every edit --apply would write; cutting it would misreport what --apply does")
	registerNoLimit("format", "a preview must show every edit --apply would write; cutting it would misreport what --apply does")
	registerNoLimit("file", "one line range of one file, bounded by --start/--end and --max-lines/--max-bytes")
	registerNoLimit("source", "returns the symbols the caller named, each bounded by --max-lines/--max-bytes; nothing is enumerated")
	registerNoLimit("context", "one symbol with its import block and hover, bounded by --max-lines/--max-bytes/--max-header-lines")
	registerNoLimit("index", "status, build and clear each answer with one report")
	registerNoLimit("servers", "bounded by the configuration, not the workspace: the handful of servers that are defined")
	registerNoLimit("doctor", "bounded by the configuration and by the paths given: one check per layer, server and path")
	registerNoLimit("daemon", "one status of one daemon (or its log tail, which has its own byte bound)")
	registerNoLimit("install", "one install plan for the one server named")
	registerNoLimit("batch", "one envelope per input line; the caller sizes the input and each line's command bounds itself")
	registerNoLimit("raw", "the escape hatch: prints the server's own result, uninterpreted")
	registerNoLimit("help", "a static list of the commands, not an answer about the workspace")
	registerNoLimit("mcp", "serves the tools over stdio; it lists nothing")
}
