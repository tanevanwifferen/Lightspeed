package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runBatch runs `lightspeed batch` with a scripted input stream and
// returns the exit code and the streams. It is the only test here that
// needs MainWithStdin: everything else the CLI does ignores stdin.
func runBatch(input string, args ...string) (code int, stdout, stderr string) {
	var out, errOut safeBuffer
	code = MainWithStdin(append([]string{"batch"}, args...), strings.NewReader(input), &out, &errOut)
	return code, out.String(), errOut.String()
}

// batchLine is one output line: an ordinary envelope plus the query
// field that says which input line it answers.
type batchLine struct {
	Version  int             `json:"version"`
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Query *struct {
		Index   int      `json:"index"`
		Command string   `json:"command"`
		Argv    []string `json:"argv"`
		Exit    int      `json:"exit"`
	} `json:"query"`
}

// decodeBatch parses the JSON-lines output, asserting that every line
// is a complete envelope on its own. That property is the contract:
// a consumer reads one line and has one answer.
func decodeBatch(t *testing.T, stdout string) []batchLine {
	t.Helper()
	var out []batchLine
	for i, line := range strings.Split(strings.TrimSuffix(stdout, "\n"), "\n") {
		if line == "" {
			continue
		}
		var parsed batchLine
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("output line %d is not a JSON envelope: %v\nline: %s", i+1, err, line)
		}
		if parsed.Version != 1 {
			t.Errorf("output line %d has version %d, want 1", i+1, parsed.Version)
		}
		out = append(out, parsed)
	}
	return out
}

// TestBatchMixedOutcomes is the case the exit-code contract exists
// for: one query fails, the others succeed, every answer is still
// printed, and the batch's exit code reports the worst thing that
// happened rather than the first or the last.
func TestBatchMixedOutcomes(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodReferences: []any{loc(file, 2, 4, 6)},
			methodDefinition: []any{loc(file, 4, 5, 7)},
		},
	}.apply(t)

	input := strings.Join([]string{
		"# a comment, and the blank line below, are skipped",
		"",
		fmt.Sprintf("references %s:3:5 --settle 20ms", file),
		// A location naming a file that is not there: exit 2, and no
		// server is started for it.
		"definition no-such-file.go:1:1",
		fmt.Sprintf("definition %s:6:9 --settle 20ms", file),
	}, "\n")

	code, stdout, stderr := runBatch(input)
	lines := decodeBatch(t, stdout)
	if len(lines) != 3 {
		t.Fatalf("got %d output lines, want 3 (one per query, none for the comment):\n%s", len(lines), stdout)
	}
	// The failing query does not stop the ones after it.
	for i, want := range []struct {
		index   int
		command string
		exit    int
		ok      bool
	}{
		{1, "references", ExitOK, true},
		{2, "definition", ExitUsage, false},
		{3, "definition", ExitOK, true},
	} {
		got := lines[i]
		if got.Query == nil {
			t.Fatalf("line %d carries no query field: %+v", i+1, got)
		}
		if got.Query.Index != want.index || got.Query.Command != want.command || got.Query.Exit != want.exit {
			t.Errorf("line %d query = %+v, want index %d command %q exit %d",
				i+1, *got.Query, want.index, want.command, want.exit)
		}
		if got.OK != want.ok {
			t.Errorf("line %d ok = %v, want %v", i+1, got.OK, want.ok)
		}
	}
	if lines[1].Error == nil || lines[1].Error.Code != "no_such_file" {
		t.Errorf("the failing line's error = %+v, want code no_such_file", lines[1].Error)
	}
	// Exit 2 outranks the two successes: a malformed query means the
	// caller's own input is wrong, and that is worth more than a
	// summary of "mostly fine".
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitUsage, stderr)
	}
}

// TestBatchExitCodeIsTheMostSevere checks the ranking directly: a
// query that found problems must not hide a query that crashed or one
// whose authority could not be established.
func TestBatchExitCodeIsTheMostSevere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		exits []int
		want  int
	}{
		{"all fine", []int{ExitOK, ExitOK}, ExitOK},
		{"problems only", []int{ExitOK, ExitProblems}, ExitProblems},
		{"not ready outranks problems", []int{ExitProblems, ExitNotReady}, ExitNotReady},
		{"no server outranks problems", []int{ExitProblems, ExitNoServer}, ExitNoServer},
		{"not ready outranks no server", []int{ExitNoServer, ExitNotReady}, ExitNotReady},
		{"usage outranks not ready", []int{ExitNotReady, ExitUsage}, ExitUsage},
		{"crash outranks everything", []int{ExitUsage, ExitCrash, ExitProblems}, ExitCrash},
		{"an unknown code is treated as a crash", []int{ExitOK, 99}, 99},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchExit(tc.exits); got != tc.want {
				t.Errorf("batchExit(%v) = %d, want %d", tc.exits, got, tc.want)
			}
		})
	}
}

// TestBatchNotReadyIsPerQuery: the readiness gate does not become
// weaker in a batch. A query whose workspace is still indexing is
// exit 5 on its own line, and the batch reports it.
func TestBatchNotReadyIsPerQuery(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		indexing:     true,
		results:      map[string]any{methodReferences: []any{}},
	}.apply(t)

	code, stdout, _ := runBatch(fmt.Sprintf("references %s:3:5 --timeout 300ms\n", file))
	if code != ExitNotReady {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNotReady, stdout)
	}
	lines := decodeBatch(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	if lines[0].Error == nil || lines[0].Error.Code != "not_ready" {
		t.Errorf("error = %+v, want code not_ready", lines[0].Error)
	}
	if lines[0].OK {
		t.Error("an unready query reported ok:true inside a batch")
	}
}

// TestBatchNonJSONOutputIsWrapped: a query that asked for text, diff
// or sarif has no envelope of its own, and a line that is not JSON
// would break every consumer of every other line. Its bytes are
// carried as a string instead.
func TestBatchNonJSONOutputIsWrapped(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	code, stdout, _ := runBatch(fmt.Sprintf("references %s:3:5 --format text --settle 20ms\n", file))
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitOK, stdout)
	}
	lines := decodeBatch(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	var data struct {
		Format string `json:"format"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(lines[0].Data, &data); err != nil {
		t.Fatalf("data is not the raw-output wrapper: %v (%s)", err, lines[0].Data)
	}
	if data.Format != "raw" {
		t.Errorf("format = %q, want raw", data.Format)
	}
	if !strings.Contains(data.Output, "変数") || !strings.HasSuffix(data.Output, "\n") {
		t.Errorf("output = %q, want the text answer verbatim", data.Output)
	}
}

// TestBatchQuoting: a query line is tokenized like a shell's argument
// vector and nothing more. A quoted argument with spaces in it has to
// survive, because --params and --symbol both need one.
func TestBatchQuoting(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodWorkspaceSymbol: []any{wsSymbol("変数", "my pkg", 13, file, 2, 4, 6)},
			methodHover: map[string]any{
				"contents": map[string]any{"kind": "plaintext", "value": "hi"},
			},
		},
	}.apply(t)

	dir := filepath.Dir(file)
	code, stdout, stderr := runBatch(fmt.Sprintf(
		"hover --symbol 'my pkg.変数' --path %q --settle 20ms\n", dir))
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	lines := decodeBatch(t, stdout)
	if len(lines) != 1 || lines[0].Query == nil {
		t.Fatalf("got %d lines: %s", len(lines), stdout)
	}
	if want := "my pkg.変数"; lines[0].Query.Argv[2] != want {
		t.Errorf("argv = %q, want the quoted argument to be one token %q", lines[0].Query.Argv, want)
	}
}

// TestSplitCommandLine covers the tokenizer on its own, including the
// inputs it must refuse rather than guess at.
func TestSplitCommandLine(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want []string
		bad  bool
	}{
		{in: "references a.go:1:1", want: []string{"references", "a.go:1:1"}},
		{in: "  spaced   out  ", want: []string{"spaced", "out"}},
		{in: `raw x/y --params '{"a":1}'`, want: []string{"raw", "x/y", "--params", `{"a":1}`}},
		{in: `hover --symbol "pkg.Type Method"`, want: []string{"hover", "--symbol", "pkg.Type Method"}},
		{in: `a "b\"c"`, want: []string{"a", `b"c`}},
		{in: `a b\ c`, want: []string{"a", "b c"}},
		{in: `empty "" quoted`, want: []string{"empty", "", "quoted"}},
		{in: `unterminated 'quote`, bad: true},
		{in: `unterminated "quote`, bad: true},
		{in: `trailing backslash \`, bad: true},
	} {
		got, err := splitCommandLine(tc.in)
		if tc.bad {
			if err == nil {
				t.Errorf("splitCommandLine(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("splitCommandLine(%q): %v", tc.in, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("splitCommandLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBatchFailFast stops at the first query that did not exit 0, so
// that a batch whose first line reveals a broken assumption does not
// spend a minute proving it again.
func TestBatchFailFast(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	input := strings.Join([]string{
		"definition no-such-file.go:1:1",
		fmt.Sprintf("references %s:3:5 --settle 20ms", file),
		"",
	}, "\n")

	code, stdout, _ := runBatch(input, "--fail-fast")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
	}
	if lines := decodeBatch(t, stdout); len(lines) != 1 {
		t.Errorf("got %d lines, want 1 (the batch stopped): %s", len(lines), stdout)
	}
}

// TestBatchSummary is the opt-in final line. It is opt-in because the
// default contract is one envelope per query and nothing else, and a
// consumer that reads the last line expecting an answer should not get
// a count instead.
func TestBatchSummary(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	input := fmt.Sprintf("references %s:3:5 --settle 20ms\ndefinition no-such-file.go:1:1\n", file)
	code, stdout, _ := runBatch(input, "--summary")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
	}
	lines := decodeBatch(t, stdout)
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (two answers and a summary): %s", len(lines), stdout)
	}
	summary := lines[2]
	if summary.Query != nil {
		t.Error("the summary line carries a query field; it is not an answer")
	}
	var data batchSummaryData
	if err := json.Unmarshal(summary.Data, &data); err != nil {
		t.Fatalf("the summary is not a summary: %v (%s)", err, summary.Data)
	}
	if data.Queries != 2 || data.OK != 1 || data.Failed != 1 || data.Exit != ExitUsage {
		t.Errorf("summary = %+v, want 2 queries, 1 ok, 1 failed, exit 2", data)
	}
	if data.ByExit["2"] != 1 || data.ByExit["0"] != 1 {
		t.Errorf("by_exit = %v, want one query per code", data.ByExit)
	}
}

// TestBatchFromFile: the same queries from a file rather than the
// stream, which is how a batch gets checked into a repository.
func TestBatchFromFile(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	queries := filepath.Join(t.TempDir(), "queries.txt")
	write(t, queries, fmt.Sprintf("references %s:3:5 --settle 20ms\n", file))

	var out, errOut safeBuffer
	code := Main([]string{"batch", "--file", queries}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, errOut.String(), out.String())
	}
	if lines := decodeBatch(t, out.String()); len(lines) != 1 {
		t.Errorf("got %d lines, want 1: %s", len(lines), out.String())
	}
}

// TestBatchRefusals covers the input that never becomes a query.
func TestBatchRefusals(t *testing.T) {
	_, file := cjkFixture(t)

	t.Run("a batch cannot contain a batch", func(t *testing.T) {
		scenario{capabilities: m5Capabilities(nil)}.apply(t)
		code, stdout, _ := runBatch("batch\n")
		if code != ExitUsage {
			t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
		}
		lines := decodeBatch(t, stdout)
		if len(lines) != 1 || lines[0].Error == nil {
			t.Fatalf("got %+v, want one failed line", lines)
		}
		if !strings.Contains(lines[0].Error.Message, "cannot contain a batch") {
			t.Errorf("message = %q, want the refusal", lines[0].Error.Message)
		}
	})

	t.Run("an unknown subcommand", func(t *testing.T) {
		scenario{capabilities: m5Capabilities(nil)}.apply(t)
		code, stdout, _ := runBatch("refs a.go:1:1\n")
		if code != ExitUsage {
			t.Fatalf("exit code = %d, want %d", code, ExitUsage)
		}
		lines := decodeBatch(t, stdout)
		if len(lines) != 1 || lines[0].Error == nil || lines[0].Error.Code != "usage" {
			t.Fatalf("got %+v, want one usage failure", lines)
		}
	})

	t.Run("an empty input", func(t *testing.T) {
		code, stdout, _ := runBatch("\n# only comments\n\n")
		if code != ExitUsage {
			t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
		}
		env := decodeEnvelope(t, stdout)
		if env.OK || env.Error == nil || env.Error.Code != "usage" {
			t.Errorf("envelope = %+v, want ok:false code usage", env)
		}
	})

	t.Run("a missing query file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.txt")
		var out, errOut safeBuffer
		code := Main([]string{"batch", "--file", missing}, &out, &errOut)
		if code != ExitUsage {
			t.Fatalf("exit code = %d, want %d", code, ExitUsage)
		}
		if env := decodeEnvelope(t, out.String()); env.Error == nil || env.Error.Code != "no_such_file" {
			t.Errorf("error = %+v, want code no_such_file", env.Error)
		}
	})

	t.Run("arguments instead of input", func(t *testing.T) {
		code, stdout, _ := runBatch("", "references", file+":3:5")
		if code != ExitUsage {
			t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
		}
	})
}

// TestBatchStreamsAsItGoes: the answers are written per query rather
// than collected, so a batch killed halfway leaves the answers it did
// produce. The proof is that the output for the first query is already
// complete and parseable while the second is still to come, which the
// file-descriptor version of this test can observe directly: the
// second query is one that never finishes in time.
func TestBatchStreamsAsItGoes(t *testing.T) {
	_, file := cjkFixture(t)
	// A pipe, so the test can read the first line before the batch has
	// finished writing the second.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })

	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	input := fmt.Sprintf("references %s:3:5 --settle 20ms\nreferences %s:3:5 --settle 20ms\n", file, file)
	done := make(chan int, 1)
	var errOut safeBuffer
	go func() {
		code := MainWithStdin([]string{"batch"}, strings.NewReader(input), w, &errOut)
		_ = w.Close()
		done <- code
	}()

	// One line, read before the batch has exited.
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 512)
	for !strings.Contains(string(buf), "\n") {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			t.Fatalf("reading the batch output: %v (so far: %q)", err, buf)
		}
	}
	first := strings.SplitN(string(buf), "\n", 2)[0]
	var parsed batchLine
	if err := json.Unmarshal([]byte(first), &parsed); err != nil {
		t.Fatalf("the first line is not a complete envelope: %v\nline: %s", err, first)
	}
	if parsed.Query == nil || parsed.Query.Index != 1 {
		t.Errorf("first line = %+v, want the answer to query 1", parsed)
	}

	// Drain the rest so the writer cannot block, then collect the code.
	go func() {
		_, _ = r.Read(make([]byte, 64*1024))
	}()
	if code := <-done; code != ExitOK {
		t.Errorf("exit code = %d, want %d; stderr: %s", code, ExitOK, errOut.String())
	}
}
