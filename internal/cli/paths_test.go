package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Paths in output are relative to the workspace root, in text and JSON alike,
// and --absolute opts out (docs/DECISIONS.md D30).

// pathsFixture is the CJK workspace with a references answer that also names a
// file outside it.
func pathsFixture(t *testing.T) (dir, file, elsewhere string) {
	t.Helper()
	dir, file = cjkFixture(t)
	elsewhere = filepath.Join(t.TempDir(), "other.go")
	write(t, elsewhere, "package other\n\nvar X = 1\n")
	scenario{results: map[string]any{
		methodReferences: []any{loc(file, 2, 4, 6), loc(elsewhere, 2, 4, 5)},
	}}.apply(t)
	return dir, file, elsewhere
}

func TestTextPathsAreRelativeToTheWorkspaceRoot(t *testing.T) {
	dir, file, elsewhere := pathsFixture(t)

	code, stdout, stderr := runMain("references", file+":3:5", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "cjk.go:3:5: ") {
		t.Fatalf("text output:\n%s\nwant cjk.go:3:5: first, relative", stdout)
	}
	// A file outside the root cannot be relative to it, and stays absolute.
	if !strings.HasPrefix(lines[1], elsewhere+":3:5: ") {
		t.Errorf("a file outside the workspace: %q, want its absolute path %s", lines[1], elsewhere)
	}
	// Run from elsewhere, the paths need to be told what they are relative to.
	if want := "# paths are relative to " + dir; lines[2] != want {
		t.Errorf("notice = %q, want %q", lines[2], want)
	}

	// Run in the root itself, they are valid as they stand and there is nothing to say.
	t.Chdir(dir)
	_, stdout, _ = runMain("references", file+":3:5", "--format", "text")
	if strings.Contains(stdout, "# paths are relative") || !strings.HasPrefix(stdout, "cjk.go:3:5: ") {
		t.Errorf("text output from the root:\n%s\nwant relative paths and no notice", stdout)
	}
}

func TestAbsoluteKeepsFullPaths(t *testing.T) {
	_, file, _ := pathsFixture(t)

	_, stdout, _ := runMain("references", file+":3:5", "--format", "text", "--absolute")
	if !strings.HasPrefix(stdout, file+":3:5: ") || strings.Contains(stdout, "# paths are relative") {
		t.Errorf("--absolute text output:\n%s\nwant absolute paths and no notice", stdout)
	}

	_, stdout, _ = runMain("references", file+":3:5", "--absolute")
	got := decodeResults(t, stdout)
	if got.Results[0].Path != file || got.Root != "" {
		t.Errorf("--absolute JSON: path %q root %q, want %q and no root", got.Results[0].Path, got.Root, file)
	}
}

func TestJSONPathsAreRelativeWithTheirRoot(t *testing.T) {
	dir, file, elsewhere := pathsFixture(t)

	_, stdout, _ := runMain("references", file+":3:5")
	got := decodeResults(t, stdout)
	if got.Root != dir || got.Results[0].Path != "cjk.go" || got.Results[1].Path != elsewhere {
		t.Errorf("JSON: root %q, paths %q and %q; want %q, cjk.go and %q", got.Root, got.Results[0].Path, got.Results[1].Path, dir, elsewhere)
	}
}

// The MCP tools answer in JSON, so they are relative too, and `absolute` is
// theirs to ask for.
func TestMCPPathsAreRelativeUnlessAbsolute(t *testing.T) {
	dir, file, _ := pathsFixture(t)
	cs := mcpSession(t, t.TempDir())

	res := callTool(t, cs, "references", map[string]any{"workspace": dir, "location": "cjk.go:3:5"})
	if got := decodeResults(t, resultText(t, res)); got.Root != dir || got.Results[0].Path != "cjk.go" {
		t.Errorf("MCP references: root %q path %q", got.Root, got.Results[0].Path)
	}
	res = callTool(t, cs, "references", map[string]any{"workspace": dir, "location": "cjk.go:3:5", "absolute": true})
	if got := decodeResults(t, resultText(t, res)); got.Results[0].Path != file {
		t.Errorf("MCP references with absolute: path %q, want %q", got.Results[0].Path, file)
	}
}

// A batch line is a command line, so it takes --absolute like one.
func TestBatchLinesTakeAbsolute(t *testing.T) {
	_, file, _ := pathsFixture(t)
	in := "references " + file + ":3:5 --absolute --format text\nreferences " + file + ":3:5 --format text\n"
	var out, errOut safeBuffer
	code := Main([]string{"batch", "--file", writeTemp(t, in)}, &out, &errOut)
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\n%s", code, errOut.String(), out.String())
	}
	if s := out.String(); !strings.Contains(s, file+":3:5: ") || !strings.Contains(s, `cjk.go:3:5: `) {
		t.Errorf("batch output:\n%s\nwant one absolute and one relative", s)
	}
}

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "queries.txt")
	write(t, p, content)
	return p
}
