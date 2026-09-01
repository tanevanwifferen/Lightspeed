package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cjkSource is the fixture PLAN §8's M1 definition of done names: a
// file whose identifiers are outside the BMP-free ASCII range, so that
// every UTF-16 code unit the server counts differs from the byte
// column the command line uses.
//
// The coordinates below are computed by hand and asserted literally
// rather than derived from the same Mapper the code under test uses —
// a test that recomputes the answer with the implementation cannot
// fail when the implementation is wrong.
//
//	line 3: `var 変数 = 1`            変数 at bytes 5-11,  UTF-16 4-6
//	line 6: `\treturn 変数 + 変数`     変数 at bytes 9-15,  UTF-16 8-10
//	                                  and at bytes 18-24, UTF-16 13-15
const cjkSource = "package cjk\n\nvar 変数 = 1\n\nfunc 関数() int {\n\treturn 変数 + 変数\n}\n"

// cjkFixture writes the fixture into a temporary Go module and
// returns the module root and the file.
func cjkFixture(t *testing.T) (dir, file string) {
	t.Helper()
	dir = t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module cjk\n\ngo 1.27\n")
	file = filepath.Join(dir, "cjk.go")
	write(t, file, cjkSource)
	return dir, file
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// loc builds an LSP location literal for the fixture, in the UTF-16
// coordinates a real server would send.
func loc(file string, line, startChar, endChar int) map[string]any {
	return map[string]any{
		"uri": "file://" + file,
		"range": map[string]any{
			"start": map[string]any{"line": line, "character": startChar},
			"end":   map[string]any{"line": line, "character": endChar},
		},
	}
}

// resultsData mirrors the payload internal/render writes for a result
// set, so tests can assert on the numbers an agent would branch on.
type resultsPayload struct {
	Kind    string `json:"kind"`
	Results []struct {
		Path   string `json:"path"`
		Kind   string `json:"kind"`
		Label  string `json:"label"`
		Detail string `json:"detail"`
		Text   string `json:"text"`
		Start  struct {
			Line, Column, Offset int
		} `json:"start"`
		End struct {
			Line, Column, Offset int
		} `json:"end"`
		Before []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"before"`
		After []struct {
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"after"`
	} `json:"results"`
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit"`
}

func decodeResults(t *testing.T, stdout string) resultsPayload {
	t.Helper()
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false, error: %+v", env.Error)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload resultsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("data is not a result set: %v (%s)", err, raw)
	}
	return payload
}

// TestReferencesOnCJKFixtureIsByteExact is half of PLAN §8's M1
// definition of done: the columns lightspeed prints are byte columns,
// exactly, for a file where UTF-16 and bytes disagree everywhere.
func TestReferencesOnCJKFixtureIsByteExact(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{
		// Deliberately out of source order: the renderer sorts.
		methodReferences: []any{
			loc(file, 5, 13, 15),
			loc(file, 2, 4, 6),
			loc(file, 5, 8, 10),
		},
	}}.apply(t)

	code, stdout, stderr := runMain("references", file+":3:5", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	want := strings.Join([]string{
		fmt.Sprintf("%s:3:5: var 変数 = 1", file),
		fmt.Sprintf("%s:6:9: \treturn 変数 + 変数", file),
		fmt.Sprintf("%s:6:18: \treturn 変数 + 変数", file),
		"",
	}, "\n")
	if stdout != want {
		t.Errorf("text output mismatch\n got: %q\nwant: %q", stdout, want)
	}

	// The JSON payload must agree, down to the byte offsets.
	code, stdout, stderr = runMain("references", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	got := decodeResults(t, stdout)
	if got.Kind != "references" || got.Count != 3 || got.Total != 3 || got.Truncated {
		t.Errorf("payload = %+v, want kind references, count 3, total 3, not truncated", got)
	}
	type point struct{ line, col, offset int }
	wantPoints := []struct{ start, end point }{
		{point{3, 5, 17}, point{3, 11, 23}},
		{point{6, 9, 57}, point{6, 15, 63}},
		{point{6, 18, 66}, point{6, 24, 72}},
	}
	for i, w := range wantPoints {
		r := got.Results[i]
		gotStart := point{r.Start.Line, r.Start.Column, r.Start.Offset}
		gotEnd := point{r.End.Line, r.End.Column, r.End.Offset}
		if gotStart != w.start || gotEnd != w.end {
			t.Errorf("result %d = %v-%v, want %v-%v", i, gotStart, gotEnd, w.start, w.end)
		}
		if r.Path != file {
			t.Errorf("result %d path = %q, want %q", i, r.Path, file)
		}
	}
}

// TestLocationRoundTripsThroughUTF16 checks the input half of the same
// property: the byte column typed on the command line must reach the
// server as the matching UTF-16 position, so echoing it back yields
// the byte column we started from.
func TestLocationRoundTripsThroughUTF16(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{
		methodDefinition: json.RawMessage(positionEcho),
	}}.apply(t)

	for _, arg := range []string{
		file + ":6:18", // byte column: the second 変数 on the line
		file + ":#66",  // the same place, as a byte offset
	} {
		code, stdout, stderr := runMain("definition", arg)
		if code != ExitOK {
			t.Fatalf("%s: exit code = %d, want %d; stderr: %s", arg, code, ExitOK, stderr)
		}
		got := decodeResults(t, stdout)
		if got.Count != 1 {
			t.Fatalf("%s: count = %d, want 1", arg, got.Count)
		}
		r := got.Results[0]
		if r.Start.Line != 6 || r.Start.Column != 18 || r.Start.Offset != 66 {
			t.Errorf("%s: echoed position = %d:%d (offset %d), want 6:18 (offset 66)",
				arg, r.Start.Line, r.Start.Column, r.Start.Offset)
		}
	}
}

// TestQueryWhileIndexingExitsNotReady is the other half of PLAN §8's
// M1 definition of done, and the failure mode PLAN §5.2 exists to
// prevent: a server that answers "no references" while it is still
// indexing must produce exit 5, never an empty list an agent would
// believe.
func TestQueryWhileIndexingExitsNotReady(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		indexing: true,
		results:  map[string]any{methodReferences: []any{}},
	}.apply(t)

	code, stdout, stderr := runMain("references", file+":3:5", "--timeout", "400ms")
	if code != ExitNotReady {
		t.Fatalf("exit code = %d, want %d (not ready); stderr: %s\nstdout: %s",
			code, ExitNotReady, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK {
		t.Fatalf("envelope ok = true; an unready workspace must not answer: %s", stdout)
	}
	if env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("error = %+v, want code not_ready", env.Error)
	}
	if strings.Contains(stdout, `"results"`) {
		t.Errorf("stdout carries a result set while not ready: %s", stdout)
	}
	if !strings.Contains(env.Error.Message, "indexing") {
		t.Errorf("message %q does not say the server was indexing", env.Error.Message)
	}
}

// TestDefinitionAcceptsEveryLocationShape checks the three shapes the
// protocol allows for a definition answer. A client that understands
// only one of them reports "nothing found" for the others, which is
// the same lie the readiness gate exists to prevent.
func TestDefinitionAcceptsEveryLocationShape(t *testing.T) {
	_, file := cjkFixture(t)
	uri := "file://" + file
	rng := map[string]any{
		"start": map[string]any{"line": 2, "character": 4},
		"end":   map[string]any{"line": 2, "character": 6},
	}
	shapes := map[string]any{
		"single Location": map[string]any{"uri": uri, "range": rng},
		"Location array":  []any{map[string]any{"uri": uri, "range": rng}},
		"LocationLink":    []any{map[string]any{"targetUri": uri, "targetRange": rng, "targetSelectionRange": rng}},
		"bare LocationLink": map[string]any{
			"targetUri": uri, "targetRange": rng,
		},
	}
	for name, result := range shapes {
		t.Run(name, func(t *testing.T) {
			scenario{results: map[string]any{methodDefinition: result}}.apply(t)
			code, stdout, stderr := runMain("definition", file+":6:9")
			if code != ExitOK {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
			}
			got := decodeResults(t, stdout)
			if got.Count != 1 {
				t.Fatalf("count = %d, want 1 (%s)", got.Count, stdout)
			}
			if r := got.Results[0]; r.Start.Line != 3 || r.Start.Column != 5 {
				t.Errorf("position = %d:%d, want 3:5", r.Start.Line, r.Start.Column)
			}
		})
	}
}

// TestEmptyAnswerExitsProblems: an authoritative "nothing found" is
// still rendered — an agent gets a well-formed empty result set — but
// the exit code is 1, grep's convention, and never 5.
func TestEmptyAnswerExitsProblems(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{methodImplementation: []any{}}}.apply(t)

	code, stdout, stderr := runMain("implementation", file+":3:5", "--settle", "10ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitProblems, stderr, stdout)
	}
	got := decodeResults(t, stdout)
	if got.Count != 0 || got.Truncated {
		t.Errorf("payload = %+v, want an empty, untruncated result set", got)
	}
}

// TestHover renders the server's markup as the result's label, so that
// text output stays one line per result and JSON keeps the markdown.
func TestHover(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{
		methodHover: map[string]any{
			"contents": map[string]any{"kind": "markdown", "value": "var 変数 int\n\nthe variable"},
			"range": map[string]any{
				"start": map[string]any{"line": 2, "character": 4},
				"end":   map[string]any{"line": 2, "character": 6},
			},
		},
	}}.apply(t)

	code, stdout, stderr := runMain("hover", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	got := decodeResults(t, stdout)
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1", got.Count)
	}
	if want := "var 変数 int\n\nthe variable"; got.Results[0].Label != want {
		t.Errorf("label = %q, want %q", got.Results[0].Label, want)
	}
	if got.Results[0].Start.Column != 5 {
		t.Errorf("column = %d, want 5", got.Results[0].Start.Column)
	}

	// Text output collapses the newlines so one result stays one line.
	code, stdout, _ = runMain("hover", file+":3:5", "--format", "text")
	if code != ExitOK {
		t.Fatalf("text: exit code = %d, want %d", code, ExitOK)
	}
	if lines := strings.Count(strings.TrimSuffix(stdout, "\n"), "\n"); lines != 0 {
		t.Errorf("text output is %d lines, want 1: %q", lines+1, stdout)
	}
	if !strings.Contains(stdout, `var 変数 int\n\nthe variable`) {
		t.Errorf("text output lost the hover body: %q", stdout)
	}
}

// TestSymbolsFlattensHierarchy checks that a hierarchical
// DocumentSymbol answer keeps document order and gains qualified
// names, and that positions point at the name rather than the whole
// declaration.
func TestSymbolsFlattensHierarchy(t *testing.T) {
	_, file := cjkFixture(t)
	nameRange := func(line, from, to int) map[string]any {
		return map[string]any{
			"start": map[string]any{"line": line, "character": from},
			"end":   map[string]any{"line": line, "character": to},
		}
	}
	scenario{results: map[string]any{
		methodDocumentSymbol: []any{
			map[string]any{
				"name": "変数", "kind": 13,
				"range": nameRange(2, 0, 11), "selectionRange": nameRange(2, 4, 6),
			},
			map[string]any{
				"name": "関数", "kind": 12, "detail": "func() int",
				"range": nameRange(4, 0, 16), "selectionRange": nameRange(4, 5, 7),
				"children": []any{
					map[string]any{
						"name": "inner", "kind": 13,
						"range": nameRange(5, 1, 7), "selectionRange": nameRange(5, 8, 10),
					},
				},
			},
		},
	}}.apply(t)

	code, stdout, stderr := runMain("symbols", file)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	got := decodeResults(t, stdout)
	want := []struct {
		label string
		kind  string
		line  int
		col   int
	}{
		{"変数", "variable", 3, 5},
		{"関数", "function", 5, 6},
		{"関数.inner", "variable", 6, 9},
	}
	if got.Count != len(want) {
		t.Fatalf("count = %d, want %d (%s)", got.Count, len(want), stdout)
	}
	for i, w := range want {
		r := got.Results[i]
		if r.Label != w.label || r.Kind != w.kind || r.Start.Line != w.line || r.Start.Column != w.col {
			t.Errorf("symbol %d = %q %s at %d:%d, want %q %s at %d:%d",
				i, r.Label, r.Kind, r.Start.Line, r.Start.Column, w.label, w.kind, w.line, w.col)
		}
	}
}

// TestWorkspaceSymbol covers the flat shape, the container-qualified
// name, and the WorkspaceSymbol variant whose location carries a uri
// and no range — which must be reported as such rather than silently
// pointing at line 1.
func TestWorkspaceSymbol(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{
		methodWorkspaceSymbol: []any{
			map[string]any{
				"name": "変数", "kind": 13, "containerName": "cjk",
				"location": map[string]any{
					"uri": "file://" + file,
					"range": map[string]any{
						"start": map[string]any{"line": 2, "character": 4},
						"end":   map[string]any{"line": 2, "character": 6},
					},
				},
			},
			map[string]any{
				"name": "関数", "kind": 12,
				"location": map[string]any{"uri": "file://" + file},
			},
		},
	}}.apply(t)

	code, stdout, stderr := runMain("workspace_symbol", "変", "--path", dir)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	got := decodeResults(t, stdout)
	if got.Count != 2 {
		t.Fatalf("count = %d, want 2 (%s)", got.Count, stdout)
	}
	if got.Results[0].Label != "cjk.変数" {
		t.Errorf("label = %q, want %q", got.Results[0].Label, "cjk.変数")
	}
	if got.Results[0].Start.Column != 5 {
		t.Errorf("column = %d, want 5", got.Results[0].Start.Column)
	}
	if got.Results[1].Start.Line != 1 || got.Results[1].Start.Column != 1 {
		t.Errorf("uri-only symbol at %d:%d, want 1:1",
			got.Results[1].Start.Line, got.Results[1].Start.Column)
	}
	if !hasWarning(env.Warnings, "no range") {
		t.Errorf("warnings = %q, want one about the missing range", env.Warnings)
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// TestLimitAndContext exercises PLAN §4's token discipline: the
// matched line only by default, more on request, and truncation that
// announces itself.
func TestLimitAndContext(t *testing.T) {
	_, file := cjkFixture(t)
	results := map[string]any{methodReferences: []any{
		loc(file, 2, 4, 6),
		loc(file, 5, 8, 10),
		loc(file, 5, 13, 15),
	}}

	t.Run("limit", func(t *testing.T) {
		scenario{results: results}.apply(t)
		code, stdout, _ := runMain("references", file+":3:5", "--limit", "2")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d", code, ExitOK)
		}
		env := decodeEnvelope(t, stdout)
		got := decodeResults(t, stdout)
		if got.Count != 2 || got.Total != 3 || !got.Truncated || got.Limit != 2 {
			t.Errorf("payload = %+v, want 2 of 3 with truncated:true and limit 2", got)
		}
		if !hasWarning(env.Warnings, "truncated") {
			t.Errorf("warnings = %q, want a truncation warning", env.Warnings)
		}
	})

	t.Run("context", func(t *testing.T) {
		scenario{results: results}.apply(t)
		code, stdout, _ := runMain("references", file+":3:5", "--context", "1", "--limit", "1")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d", code, ExitOK)
		}
		got := decodeResults(t, stdout)
		r := got.Results[0]
		if len(r.Before) != 1 || r.Before[0].Line != 2 || r.Before[0].Text != "" {
			t.Errorf("before = %+v, want the empty line 2", r.Before)
		}
		if len(r.After) != 1 || r.After[0].Line != 4 || r.After[0].Text != "" {
			t.Errorf("after = %+v, want the empty line 4", r.After)
		}
	})

	t.Run("no context by default", func(t *testing.T) {
		scenario{results: results}.apply(t)
		code, stdout, _ := runMain("references", file+":3:5", "--limit", "1")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d", code, ExitOK)
		}
		if r := decodeResults(t, stdout).Results[0]; len(r.Before) != 0 || len(r.After) != 0 {
			t.Errorf("default output carries context: before=%+v after=%+v", r.Before, r.After)
		}
	})
}

// TestUnsupportedMethodNamesWhatWorks: a server that never advertised
// referencesProvider must produce exit 3 with a message that says what
// it can do instead — PLAN §4's requirement that the command surface
// never lies.
func TestUnsupportedMethodNamesWhatWorks(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{capabilities: map[string]any{
		"definitionProvider": true,
		"hoverProvider":      true,
	}}.apply(t)

	code, stdout, _ := runMain("references", file+":3:5")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Fatalf("error = %+v, want code unsupported_method", env.Error)
	}
	for _, want := range []string{"definition", "hover"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message %q does not offer %q", env.Error.Message, want)
		}
	}
	if strings.Contains(env.Error.Message, "implementation") {
		t.Errorf("message %q offers a command this server cannot answer", env.Error.Message)
	}
}

// TestHelpSurfaceComesFromCapabilities checks the runtime command
// surface: `help <file>` starts the server that handles the file and
// reports what it actually advertises.
func TestHelpSurfaceComesFromCapabilities(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{capabilities: map[string]any{
		"referencesProvider": true,
		"hoverProvider":      true,
	}}.apply(t)

	code, stdout, stderr := runMain("help", file)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var surface surfaceData
	if err := json.Unmarshal(raw, &surface); err != nil {
		t.Fatalf("data is not a surface: %v (%s)", err, raw)
	}
	if surface.ServerName != "fake-langserver" {
		t.Errorf("server name = %q, want fake-langserver", surface.ServerName)
	}
	available := names(surface.Available)
	unavailable := names(surface.Unavailable)
	for _, want := range []string{"references", "hover", "raw", "help"} {
		if !contains(available, want) {
			t.Errorf("%q missing from the available surface %v", want, available)
		}
	}
	for _, want := range []string{"definition", "implementation", "symbols", "workspace_symbol"} {
		if !contains(unavailable, want) {
			t.Errorf("%q missing from the unavailable surface %v", want, unavailable)
		}
	}
	for _, info := range surface.Unavailable {
		if info.Capability == "" {
			t.Errorf("unavailable command %q does not say which capability it needs", info.Name)
		}
	}
	if !strings.Contains(stderr, "fake-langserver") {
		t.Errorf("human help does not name the server: %s", stderr)
	}
}

func names(infos []commandInfo) []string {
	out := make([]string, len(infos))
	for i, info := range infos {
		out[i] = info.Name
	}
	return out
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestUsageErrors covers the exit-2 surface: bad locations, missing
// files, unknown subcommands and formats that mean nothing here.
func TestUsageErrors(t *testing.T) {
	_, file := cjkFixture(t)

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"unknown subcommand", []string{"refs", file + ":1:1"}, ExitUsage, "usage"},
		{"missing argument", []string{"references"}, ExitUsage, "usage"},
		{"too many arguments", []string{"definition", file + ":1:1", file + ":2:2"}, ExitUsage, "usage"},
		{"no such file", []string{"definition", filepath.Join(t.TempDir(), "gone.go") + ":1:1"}, ExitUsage, "no_such_file"},
		{"directory", []string{"definition", t.TempDir() + ":1:1"}, ExitUsage, "usage"},
		{"column past end of line", []string{"definition", file + ":3:400"}, ExitUsage, "invalid_position"},
		{"offset past end of file", []string{"definition", file + ":#9999"}, ExitUsage, "invalid_position"},
		{"unknown format", []string{"definition", file + ":3:5", "--format", "yaml"}, ExitUsage, "invalid_format"},
		{"diff format", []string{"references", file + ":3:5", "--format", "diff"}, ExitUsage, "unsupported_format"},
		{"sarif format", []string{"symbols", file, "--format", "sarif"}, ExitUsage, "unsupported_format"},
		{"negative limit", []string{"references", file + ":3:5", "--limit", "-1"}, ExitUsage, "usage"},
		{"zero timeout", []string{"references", file + ":3:5", "--timeout", "0"}, ExitUsage, "usage"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scenario{results: map[string]any{methodDefinition: []any{}}}.apply(t)
			code, stdout, _ := runMain(tc.args...)
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, tc.code, stdout)
			}
			env := decodeEnvelope(t, stdout)
			if env.OK || env.Error == nil || string(env.Error.Code) != tc.want {
				t.Errorf("envelope = %+v, want ok:false code %s", env, tc.want)
			}
		})
	}
}

// TestNoServerForUnknownLanguage: a file no definition claims exits 3,
// and says so rather than pretending the answer is empty.
func TestNoServerForUnknownLanguage(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "notes.wat")
	write(t, file, "nothing here\n")

	code, stdout, _ := runMain("definition", file+":1:1")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "no_server" {
		t.Errorf("error = %+v, want code no_server", env.Error)
	}
}

// TestServerNotInstalled: a matched server whose binary is absent is
// exit 3 with the command that would fix it (PLAN §6: nothing installs
// implicitly).
func TestServerNotInstalled(t *testing.T) {
	_, file := cjkFixture(t)
	t.Setenv(serverCommandEnv, "lightspeed-definitely-not-installed")

	code, stdout, _ := runMain("definition", file+":3:5")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "server_not_installed" {
		t.Fatalf("error = %+v, want code server_not_installed", env.Error)
	}
	if !strings.Contains(env.Error.Message, "mise use -g") {
		t.Errorf("message %q does not carry the install command", env.Error.Message)
	}
}

// TestReferencesIncludeDeclarationFlag checks that -d reaches the
// server. Asserting on the request rather than on the answer is the
// only way to tell a flag that works from a flag that is ignored.
func TestReferencesIncludeDeclarationFlag(t *testing.T) {
	_, file := cjkFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, false},
		{"short", []string{"-d"}, true},
		{"long", []string{"--declaration"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := scenario{results: map[string]any{
				methodReferences: []any{loc(file, 2, 4, 6)},
			}}
			readTrace := sc.traceTo(t)
			sc.apply(t)

			args := append([]string{"references", file + ":3:5"}, tc.args...)
			code, stdout, stderr := runMain(args...)
			if code != ExitOK {
				t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
			}
			params := requestParams(t, readTrace(), methodReferences)
			var got struct {
				Context struct {
					IncludeDeclaration bool `json:"includeDeclaration"`
				} `json:"context"`
			}
			if err := json.Unmarshal(params, &got); err != nil {
				t.Fatal(err)
			}
			if got.Context.IncludeDeclaration != tc.want {
				t.Errorf("includeDeclaration = %v, want %v (params: %s)",
					got.Context.IncludeDeclaration, tc.want, params)
			}
		})
	}
}

// TestDocumentIsOpenedBeforeTheQuery: most servers answer nothing
// about a file they were never told about (PLAN §5.4), so the didOpen
// has to precede the request and carry the file's real content.
func TestDocumentIsOpenedBeforeTheQuery(t *testing.T) {
	_, file := cjkFixture(t)
	sc := scenario{results: map[string]any{
		methodDefinition: []any{loc(file, 2, 4, 6)},
	}}
	readTrace := sc.traceTo(t)
	sc.apply(t)

	code, stdout, stderr := runMain("definition", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}

	trace := readTrace()
	openAt, queryAt, closeAt := -1, -1, -1
	for i, msg := range trace {
		switch msg.Method {
		case "textDocument/didOpen":
			openAt = i
		case methodDefinition:
			queryAt = i
		case "textDocument/didClose":
			closeAt = i
		}
	}
	if openAt < 0 || queryAt < 0 || openAt > queryAt {
		t.Fatalf("didOpen at %d, query at %d; want the open first (trace: %+v)", openAt, queryAt, trace)
	}
	if closeAt < 0 || closeAt < queryAt {
		t.Errorf("didClose at %d, query at %d; want the close last", closeAt, queryAt)
	}

	var opened struct {
		TextDocument struct {
			URI        string `json:"uri"`
			LanguageID string `json:"languageId"`
			Text       string `json:"text"`
			Version    int    `json:"version"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(trace[openAt].Params, &opened); err != nil {
		t.Fatal(err)
	}
	if opened.TextDocument.Text != cjkSource {
		t.Errorf("didOpen text = %q, want the file's content", opened.TextDocument.Text)
	}
	if opened.TextDocument.LanguageID != "go" {
		t.Errorf("didOpen languageId = %q, want go", opened.TextDocument.LanguageID)
	}
	if opened.TextDocument.Version != 1 {
		t.Errorf("didOpen version = %d, want 1", opened.TextDocument.Version)
	}
}

// requestParams returns the parameters of the first traced message for
// a method.
func requestParams(t *testing.T, trace []traced, method string) json.RawMessage {
	t.Helper()
	for _, msg := range trace {
		if msg.Method == method {
			return msg.Params
		}
	}
	t.Fatalf("%s was never sent (trace: %+v)", method, trace)
	return nil
}

// TestFlagsAfterPositional: Go's flag package stops at the first
// non-flag argument, which would silently ignore every flag an agent
// writes after the location. It must not.
func TestFlagsAfterPositional(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{
		methodReferences: []any{loc(file, 2, 4, 6), loc(file, 5, 8, 10)},
	}}.apply(t)

	code, stdout, stderr := runMain("references", file+":3:5", "--limit", "1")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	if got := decodeResults(t, stdout); got.Count != 1 || !got.Truncated {
		t.Errorf("payload = %+v, want the trailing --limit to have applied", got)
	}
}

// TestServerErrorIsAResult: a JSON-RPC error response means the server
// answered, so it is exit 1 and not a crash.
func TestServerErrorIsAResult(t *testing.T) {
	_, file := cjkFixture(t)
	// No canned result for the method: the scripted server answers
	// MethodNotFound while still advertising the capability.
	scenario{}.apply(t)

	code, stdout, _ := runMain("definition", file+":3:5")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "server_error" {
		t.Errorf("error = %+v, want code server_error", env.Error)
	}
}

// TestTopLevelHelpListsCapabilityRequirements: without a file there is
// no server to ask, so the static surface must at least say which
// capability each command depends on.
func TestTopLevelHelpListsCapabilityRequirements(t *testing.T) {
	code, stdout, stderr := runMain("help")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	if stdout != "" {
		t.Errorf("help wrote to stdout: %q", stdout)
	}
	// The M5 surface is in here too now: check, call_hierarchy, batch
	// and --symbol are implemented, so the usage text has to say so.
	for _, want := range []string{
		"definition", "references", "implementation", "hover", "symbols",
		"workspace_symbol", "rename", "codeaction", "format",
		"check", "call_hierarchy", "batch", "--symbol", "sarif",
		"needs referencesProvider", "needs renameProvider", "--apply",
		"needs callHierarchyProvider",
		"file.go:#1234", "exit codes",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("usage text is missing %q:\n%s", want, stderr)
		}
	}
}

// TestLocationSyntaxes checks that every span form PLAN §4 promises
// reaches the server as the same position. The range forms use their
// start, which is what a point-valued LSP request needs.
func TestLocationSyntaxes(t *testing.T) {
	_, file := cjkFixture(t)
	for _, tc := range []struct {
		arg  string
		line int
		col  int
	}{
		{file, 1, 1},               // no position: the start of the file
		{file + ":3", 3, 1},        // line only
		{file + ":3:5", 3, 5},      // line and byte column
		{file + ":#17", 3, 5},      // byte offset
		{file + ":6:9-6:15", 6, 9}, // a range on one line
		{file + ":3:5-6:15", 3, 5}, // a range across lines
		{file + ":#57-#63", 6, 9},  // a range of byte offsets
		{file + ":6:18", 6, 18},    // past a CJK identifier
	} {
		t.Run(filepath.Base(tc.arg), func(t *testing.T) {
			scenario{results: map[string]any{
				methodDefinition: json.RawMessage(positionEcho),
			}}.apply(t)
			code, stdout, stderr := runMain("definition", tc.arg)
			if code != ExitOK {
				t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
			}
			r := decodeResults(t, stdout).Results[0]
			if r.Start.Line != tc.line || r.Start.Column != tc.col {
				t.Errorf("%s resolved to %d:%d, want %d:%d", tc.arg, r.Start.Line, r.Start.Column, tc.line, tc.col)
			}
		})
	}
}

// TestUnreadableResultIsWarnedAbout: a location in a file the store
// cannot read must not abort the answer and must not vanish from it.
func TestUnreadableResultIsWarnedAbout(t *testing.T) {
	dir, file := cjkFixture(t)
	missing := filepath.Join(dir, "deleted.go")
	scenario{results: map[string]any{
		methodReferences: []any{
			loc(file, 2, 4, 6),
			loc(missing, 0, 0, 1),
		},
	}}.apply(t)

	code, stdout, stderr := runMain("references", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if got := decodeResults(t, stdout); got.Count != 1 {
		t.Errorf("count = %d, want 1 (the readable result)", got.Count)
	}
	if !hasWarning(env.Warnings, "deleted.go") {
		t.Errorf("warnings = %q, want one naming the unreadable file", env.Warnings)
	}
}
