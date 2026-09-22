package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// fileResult decodes `file` output.
func fileResult(t *testing.T, stdout string) (fileData, rawEnvelope) {
	t.Helper()
	var data fileData
	env := decodeData(t, stdout, &data)
	return data, env
}

// TestFileRange: a line range of a file, byte for byte, with the numbers an
// agent needs to ask for the next one.
func TestFileRange(t *testing.T) {
	symWorkspace(t, map[string]string{"notes.txt": "one\ntwo\nthree\n", "empty.txt": ""})

	code, stdout, stderr := runMain("file", "sym.go", "--start", "13", "--end", "16")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	got, _ := fileResult(t, stdout)
	if got.Source != greetSource || got.StartLine != 13 || got.EndLine != 16 || got.TotalLines != 27 ||
		got.File != "sym.go" || got.Language != "go" || got.Truncated || got.Hash != sha(greetSource) {
		t.Errorf("file sym.go 13-16 = %+v", got)
	}

	// The whole file is the default, and an end past it is the end.
	whole := strings.TrimSuffix(symSource, "\n")
	for _, args := range [][]string{{"file", "sym.go"}, {"file", "sym.go", "--end", "999"}, {"file", "sym.go", "--start", "1"}} {
		_, stdout, _ := runMain(args...)
		got, _ := fileResult(t, stdout)
		if got.Source != whole || got.StartLine != 1 || got.EndLine != 27 || got.TotalLines != 27 {
			t.Errorf("%v: lines %d-%d of %d, source differs from the file", args, got.StartLine, got.EndLine, got.TotalLines)
		}
	}
	_, stdout, _ = runMain("file", "notes.txt", "--start", "3")
	if got, _ := fileResult(t, stdout); got.Source != "three" || got.Language != "" {
		t.Errorf("notes.txt from 3 = %+v", got)
	}
	code, stdout, _ = runMain("file", "empty.txt")
	if got, _ := fileResult(t, stdout); code != ExitOK || got.TotalLines != 0 || got.Source != "" || got.EndLine != 0 {
		t.Errorf("an empty file: exit %d, %+v", code, got)
	}

	// Ranges that make no sense are usage errors.
	for name, args := range map[string][]string{
		"start past the end": {"file", "sym.go", "--start", "28"},
		"end before start":   {"file", "sym.go", "--start", "5", "--end", "3"},
		"negative":           {"file", "sym.go", "--start", "-1"},
		"a directory":        {"file", "."},
	} {
		if code, _, _ := runMain(args...); code != ExitUsage {
			t.Errorf("%s: exit %d, want %d", name, code, ExitUsage)
		}
	}
	code, stdout, _ = runMain("file", "nope.txt")
	if got, _ := errorData(t, stdout); code != ExitUsage || got != "no_such_file" {
		t.Errorf("a missing file: exit %d, code %q", code, got)
	}

	code, stdout, _ = runMain("file", "sym.go", "--start", "13", "--end", "14", "--format", "text")
	if want := "sym.go:13: // Greet は挨拶を返す 👋\nsym.go:14: func (g *Greeter) Greet() string {\n"; code != ExitOK || stdout != want {
		t.Errorf("text format: exit %d\n%q\nwant\n%q", code, stdout, want)
	}
}

// TestFileCaps: --max-lines and --max-bytes cut, say so, and never split a
// character; the hash stays that of the range asked for.
func TestFileCaps(t *testing.T) {
	symWorkspace(t, nil)

	_, stdout, _ := runMain("file", "sym.go", "--start", "13", "--end", "16", "--max-lines", "2")
	got, env := fileResult(t, stdout)
	if !got.Truncated || got.EndLine != 14 || got.Hash != sha(greetSource) ||
		got.Source != strings.Join(strings.Split(greetSource, "\n")[:2], "\n") ||
		!strings.Contains(strings.Join(env.Warnings, "\n"), "lines 13-14 of the 4 requested") {
		t.Errorf("--max-lines 2: %+v, warnings %v", got, env.Warnings)
	}
	mid := strings.Index(greetSource, "👋") + 1
	_, stdout, _ = runMain("file", "sym.go", "--start", "13", "--end", "16", "--max-bytes", strconv.Itoa(mid))
	got, _ = fileResult(t, stdout)
	if !got.Truncated || !utf8.ValidString(got.Source) || got.Source != greetSource[:strings.Index(greetSource, "👋")] {
		t.Errorf("--max-bytes into an emoji: %q, truncated %v", got.Source, got.Truncated)
	}
}

// TestFileIsConfinedToTheWorkspace: a path that leaves the workspace — by
// `..`, by an absolute path, or by a symlink inside it — is refused before it
// is read.
func TestFileIsConfinedToTheWorkspace(t *testing.T) {
	dir, _ := symWorkspace(t, nil)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	write(t, secret, "do not read\n")
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(dir, secret)
	if err != nil {
		t.Fatal(err)
	}

	for name, arg := range map[string]string{
		"an absolute path": secret,
		"a parent path":    rel,
		"a symlink out":    "link.txt",
		// Refused as outside, not reported as missing: which paths exist
		// out there is not for a confined command to say.
		"a missing path outside": filepath.Join(outside, "nope.txt"),
		"a missing parent path":  "../../nowhere/at/all",
	} {
		code, stdout, _ := runMain("file", arg)
		got, _ := errorData(t, stdout)
		if code != ExitUsage || got != "outside_workspace" || strings.Contains(stdout, "do not read") {
			t.Errorf("%s: exit %d, code %q\n%s", name, code, got, stdout)
		}
	}
	// Inside, by an absolute path, is fine.
	if code, _, _ := runMain("file", filepath.Join(dir, "sym.go"), "--end", "1"); code != ExitOK {
		t.Errorf("an absolute path inside the workspace: exit %d", code)
	}
}

// TestFileRefusesBinary: a file with a NUL byte is not text to hand an agent.
func TestFileRefusesBinary(t *testing.T) {
	symWorkspace(t, map[string]string{"blob.bin": "ab\x00cd"})
	if code, stdout, _ := runMain("file", "blob.bin"); code != ExitUsage || !strings.Contains(stdout, "binary file") {
		t.Errorf("a binary file: exit %d\n%s", code, stdout)
	}
}
