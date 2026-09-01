package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// wsSymbol builds one SymbolInformation, the flat shape
// workspace/symbol answers with, in the UTF-16 coordinates a server
// sends.
func wsSymbol(name, container string, kind int, file string, line, startChar, endChar int) map[string]any {
	sym := map[string]any{
		"name": name,
		"kind": kind,
		"location": map[string]any{
			"uri": uriOf(file),
			"range": map[string]any{
				"start": map[string]any{"line": line, "character": startChar},
				"end":   map[string]any{"line": line, "character": endChar},
			},
		},
	}
	if container != "" {
		sym["containerName"] = container
	}
	return sym
}

// TestSymbolResolvesToAByteColumn is the point of --symbol: an agent
// that cannot compute a UTF-16 column, or a byte one, still gets the
// right identifier — and is told which location it got, in the syntax
// it could have typed itself.
func TestSymbolResolvesToAByteColumn(t *testing.T) {
	dir, file := cjkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodWorkspaceSymbol: []any{wsSymbol("変数", "cjk", 13, file, 2, 4, 6)},
			methodDefinition:      json.RawMessage(positionEcho),
		},
	}
	readTrace := sc.traceTo(t)
	sc.apply(t)

	code, stdout, stderr := runMain("definition", "--symbol", "cjk.変数", "--path", dir, "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	// The echoed position proves the whole round trip: the symbol's
	// UTF-16 range became a byte column, the byte column became a
	// location argument, and the location argument became the same
	// UTF-16 position on the wire again.
	got := decodeResults(t, stdout)
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1 (%s)", got.Count, stdout)
	}
	if r := got.Results[0]; r.Start.Line != 3 || r.Start.Column != 5 {
		t.Errorf("resolved to %d:%d, want 3:5", r.Start.Line, r.Start.Column)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, ":3:5") {
		t.Errorf("warnings = %q, want one reporting the location the symbol resolved to", env.Warnings)
	}

	// The query sent to the server is the *name*, not the path:
	// workspace/symbol matches names, and the path is applied here.
	params := requestParams(t, readTrace(), methodWorkspaceSymbol)
	var req struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		t.Fatal(err)
	}
	if req.Query != "変数" {
		t.Errorf("workspace/symbol query = %q, want the last segment %q", req.Query, "変数")
	}
}

// TestSymbolAmbiguityIsRefused is the rule that matters. Two symbols
// that answer to the same name are two different pieces of code, and
// picking one is how an agent renames the wrong Handle. The refusal
// carries both locations so the retry is a copy-paste.
func TestSymbolAmbiguityIsRefused(t *testing.T) {
	dir, main, helper := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodWorkspaceSymbol: []any{
				wsSymbol("helper", "cjk", 12, main, 2, 4, 6),
				wsSymbol("helper", "cjk", 12, helper, 2, 5, 11),
			},
		},
	}.apply(t)

	code, stdout, _ := runMain("references", "--symbol", "cjk.helper", "--path", dir, "--settle", "20ms")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d (the invocation named two things); stdout: %s", code, ExitUsage, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "usage" {
		t.Fatalf("envelope = %+v, want ok:false code usage", env)
	}
	if !strings.Contains(env.Error.Message, "matches 2 symbols") {
		t.Errorf("message %q does not say how many symbols matched", env.Error.Message)
	}
	for _, want := range []string{"cjk.go:3:5", "helper.go:3:6"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("message %q does not offer the location %q", env.Error.Message, want)
		}
	}
	// The candidates are also machine-readable, so an agent does not
	// have to parse the sentence.
	data, err := json.Marshal(env.Error.Data)
	if err != nil {
		t.Fatal(err)
	}
	var details struct {
		Symbol     string `json:"symbol"`
		Candidates []struct {
			Symbol   string `json:"symbol"`
			Location string `json:"location"`
		} `json:"candidates"`
	}
	if err := json.Unmarshal(data, &details); err != nil {
		t.Fatalf("error.data is not the candidate list: %v (%s)", err, data)
	}
	if details.Symbol != "cjk.helper" || len(details.Candidates) != 2 {
		t.Errorf("details = %+v, want the query and two candidates", details)
	}
	for _, c := range details.Candidates {
		if !strings.Contains(c.Location, ":3:") {
			t.Errorf("candidate %+v has no usable location", c)
		}
	}
}

// TestSymbolMatching pins the matching rule down, because a caller has
// to be able to predict it.
func TestSymbolMatching(t *testing.T) {
	dir, main, helper := checkFixture(t)
	// Every case answers with the same candidate set and differs only
	// in the query.
	candidates := []any{
		wsSymbol("Handle", "pkg.Server", 6, main, 2, 4, 6),   // pkg.Server.Handle
		wsSymbol("Handle", "pkg.Client", 6, helper, 2, 5, 7), // pkg.Client.Handle
		wsSymbol("Server", "", 23, main, 4, 5, 7),            // Server
		wsSymbol("Server", "pkg.Server", 6, helper, 2, 5, 7), // pkg.Server.Server
	}
	for _, tc := range []struct {
		name  string
		query string
		code  int
		want  string // a substring of the resolved location, or of the error
	}{
		{"a container-qualified suffix", "Server.Handle", ExitOK, "cjk.go:3:5"},
		{"the full path", "pkg.Client.Handle", ExitOK, "helper.go:3:6"},
		{"a bare name that is unique", "Client.Handle", ExitOK, "helper.go:3:6"},
		{"a bare name that is not", "Handle", ExitUsage, "matches 2 symbols"},
		{"an exact match beats a suffix", "Server", ExitOK, "cjk.go:5:6"},
		{"a wrong container", "other.Handle", ExitProblems, "matched no symbol"},
		{"a name nothing has", "Missing", ExitProblems, "matched no symbol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario{
				capabilities: m5Capabilities(nil),
				results: map[string]any{
					methodWorkspaceSymbol: candidates,
					methodHover: map[string]any{
						"contents": map[string]any{"kind": "plaintext", "value": "resolved"},
					},
				},
			}.apply(t)

			code, stdout, _ := runMain("hover", "--symbol", tc.query, "--path", dir, "--settle", "20ms")
			if code != tc.code {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, tc.code, stdout)
			}
			env := decodeEnvelope(t, stdout)
			haystack := strings.Join(env.Warnings, " ")
			if env.Error != nil {
				haystack = env.Error.Message
			}
			if !strings.Contains(haystack, tc.want) {
				t.Errorf("%q: got %q, want it to mention %q", tc.query, haystack, tc.want)
			}
		})
	}
}

// TestSymbolWithoutARange: a WorkspaceSymbol may name a file and no
// position at all. Resolving that to line 1 column 1 would be a
// location nobody asked about, so it is refused instead.
func TestSymbolWithoutARange(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodWorkspaceSymbol: []any{
				map[string]any{
					"name": "変数", "kind": 13, "containerName": "cjk",
					"location": map[string]any{"uri": uriOf(file)},
				},
			},
		},
	}.apply(t)

	code, stdout, _ := runMain("definition", "--symbol", "cjk.変数", "--path", dir, "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "not_found" {
		t.Fatalf("error = %+v, want code not_found", env.Error)
	}
	if !strings.Contains(env.Error.Message, "not where") {
		t.Errorf("message %q does not explain that the server gave no position", env.Error.Message)
	}
}

// TestSymbolWithRename: --symbol shifts the positional arguments, so
// the command that takes two of them has to keep working.
func TestSymbolWithRename(t *testing.T) {
	dir := tree(t, map[string]string{"a.go": "package a\n\nfunc Old() {}\n"})
	file := dir + "/a.go"
	scenario{
		capabilities: mutationServerCaps(map[string]any{
			"workspaceSymbolProvider": true,
			"renameProvider":          map[string]any{"prepareProvider": false},
		}),
		results: map[string]any{
			methodWorkspaceSymbol: []any{wsSymbol("Old", "a", 12, file, 2, 5, 8)},
			methodRename: changesEdit(map[string][]any{
				file: {textEdit(2, 5, 8, "New")},
			}),
		},
	}.apply(t)

	code, stdout, stderr := runMain("rename", "--symbol", "a.Old", "New",
		"--path", dir, "--format", "json", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	if !strings.Contains(stdout, "New") {
		t.Errorf("the rename preview does not carry the new name: %s", stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, "a.Old") {
		t.Errorf("warnings = %q, want one naming the symbol that was resolved", env.Warnings)
	}
}

// TestSymbolUsage: --symbol and a location are two ways of saying the
// same thing, so saying both is a mistake worth reporting rather than
// a precedence rule nobody will remember.
func TestSymbolUsage(t *testing.T) {
	dir, file := cjkFixture(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"both a location and a symbol", []string{"definition", file + ":3:5", "--symbol", "cjk.変数", "--path", dir}},
		{"an empty segment", []string{"definition", "--symbol", "cjk..変数", "--path", dir}},
		{"nothing at all", []string{"definition", "--path", dir}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario{capabilities: m5Capabilities(nil)}.apply(t)
			code, stdout, _ := runMain(tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
			}
			if env := decodeEnvelope(t, stdout); env.OK || env.Error == nil {
				t.Errorf("envelope = %+v, want a failure", env)
			}
		})
	}
}

// TestSymbolWithoutWorkspaceSymbolProvider: --symbol is a
// workspace/symbol query, so a server that never advertised it cannot
// answer — exit 3, not a guess at a position.
func TestSymbolWithoutWorkspaceSymbolProvider(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{capabilities: m5Capabilities(map[string]any{"workspaceSymbolProvider": nil})}.apply(t)

	code, stdout, _ := runMain("definition", "--symbol", "cjk.変数", "--path", dir, "--settle", "20ms")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Errorf("error = %+v, want code unsupported_method", env.Error)
	}
}
