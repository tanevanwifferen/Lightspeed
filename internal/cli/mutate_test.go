package cli

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The M2 acceptance tests of PLAN §8, at the command layer.
//
// internal/edit already proves the transaction itself: an overlapping
// edit set is refused, a failed multi-file write rolls back, and the
// diff it renders is the bytes it would have written. What these tests
// prove is that the *commands* still have those properties once a real
// server, a real handshake and the CLI's own flag handling are in the
// way — because a command that stages an edit and then writes it by
// some other route would pass every test in internal/edit and still
// corrupt a tree.
//
// The three criteria, and where each is tested:
//
//	a 3-file rename fully applies or leaves the tree untouched
//	    TestRenameAppliesAllThreeFiles, TestRenameLeavesTreeUntouched
//	a malicious overlapping edit is rejected with nothing written
//	    TestRenameRefusesOverlappingEdits
//	`--format diff | git apply` reproduces `--apply` exactly
//	    TestRenameDiffReproducesApply

// The fixture: one declaration and three uses across three files, so
// that a rename has to touch all of them and a partial application is
// visible.
//
// The UTF-16 columns below are counted by hand from the source, not
// derived from the code under test.
//
//	a.go line 3: `func Old() int { return 1 }`             Old at 5-8
//	b.go line 3: `func useB() int { return Old() }`        Old at 25-28
//	c.go line 3: `func useC() int { return Old() + Old() }`Old at 25-28, 33-36
var fixtureFiles = map[string]string{
	"a.go": "package fixture\n\nfunc Old() int { return 1 }\n",
	"b.go": "package fixture\n\nfunc useB() int { return Old() }\n",
	"c.go": "package fixture\n\nfunc useC() int { return Old() + Old() }\n",
}

// renamedFiles is the fixture after Old becomes New.
var renamedFiles = map[string]string{
	"go.mod": "module fixture\n\ngo 1.27\n",
	"a.go":   "package fixture\n\nfunc New() int { return 1 }\n",
	"b.go":   "package fixture\n\nfunc useB() int { return New() }\n",
	"c.go":   "package fixture\n\nfunc useC() int { return New() + New() }\n",
}

// declLoc is the location of the declaration, in the CLI's 1-based
// byte columns: `func Old` puts O in column 6.
func declLoc(dir string) string { return filepath.Join(dir, "a.go") + ":3:6" }

// renameToNew is the WorkspaceEdit a well-behaved server answers with.
func renameToNew(dir string) map[string]any {
	return changesEdit(map[string][]any{
		filepath.Join(dir, "a.go"): {textEdit(2, 5, 8, "New")},
		filepath.Join(dir, "b.go"): {textEdit(2, 25, 28, "New")},
		filepath.Join(dir, "c.go"): {textEdit(2, 25, 28, "New"), textEdit(2, 33, 36, "New")},
	})
}

// renameScenario is the common setup: a server that can rename, and
// answers with edits.
func renameScenario(t *testing.T, dir string, edit any) scenario {
	t.Helper()
	s := scenario{
		capabilities: mutationServerCaps(nil),
		results: map[string]any{
			methodPrepareRename: map[string]any{
				"range":       map[string]any{"start": map[string]any{"line": 2, "character": 5}, "end": map[string]any{"line": 2, "character": 8}},
				"placeholder": "Old",
			},
			methodRename: edit,
		},
	}
	return s
}

// TestRenamePreviewsByDefault: no --apply, no --format, and the answer
// is a unified diff of every file that would change — with the tree
// exactly as it was.
func TestRenamePreviewsByDefault(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	s := renameScenario(t, dir, renameToNew(dir))
	s.apply(t)

	code, stdout, stderr := runMain("rename", declLoc(dir), "New")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	for _, want := range []string{
		"diff --git a/a.go b/a.go",
		"diff --git a/b.go b/b.go",
		"diff --git a/c.go b/c.go",
		"-func Old() int { return 1 }",
		"+func New() int { return 1 }",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("preview is missing %q:\n%s", want, stdout)
		}
	}
	assertUnchanged(t, dir, before)
}

// TestRenameAppliesAllThreeFiles is the first half of PLAN §8's M2
// criterion: a 3-file rename fully applies.
func TestRenameAppliesAllThreeFiles(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := renameScenario(t, dir, renameToNew(dir))
	s.apply(t)

	code, stdout, stderr := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	got := snapshot(t, dir)
	for name, want := range renamedFiles {
		if got[name] != want {
			t.Errorf("%s =\n%q\nwant\n%q", name, got[name], want)
		}
	}

	var payload struct {
		Changes []struct {
			Kind string `json:"kind"`
			Path string `json:"path"`
			Diff string `json:"diff"`
		} `json:"changes"`
		Count int `json:"count"`
		Edits int `json:"edits"`
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("data is not a change set: %v (%s)", err, raw)
	}
	if payload.Count != 3 || payload.Edits != 4 {
		t.Errorf("payload count=%d edits=%d, want 3 and 4 (%s)", payload.Count, payload.Edits, raw)
	}
	for i, c := range payload.Changes {
		if c.Kind != "modify" {
			t.Errorf("change %d kind = %q, want modify", i, c.Kind)
		}
		if filepath.IsAbs(c.Path) {
			t.Errorf("change %d path %q is absolute; paths are relative to the workspace root", i, c.Path)
		}
		if c.Diff == "" {
			t.Errorf("change %d carries no diff", i)
		}
	}
}

// TestRenameLeavesTreeUntouched is the other half: one bad edit in the
// third file and not a byte of the first two is written.
func TestRenameLeavesTreeUntouched(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	// The first two files are perfectly good; the third names a line
	// that does not exist.
	bad := changesEdit(map[string][]any{
		filepath.Join(dir, "a.go"): {textEdit(2, 5, 8, "New")},
		filepath.Join(dir, "b.go"): {textEdit(2, 25, 28, "New")},
		filepath.Join(dir, "c.go"): {textEdit(99, 0, 3, "New")},
	})
	s := renameScenario(t, dir, bad)
	s.apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty", "--format", "json")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "edit_conflict" {
		t.Errorf("envelope = %+v, want ok:false code edit_conflict", env)
	}
	assertUnchanged(t, dir, before)
}

// TestRenameRefusesOverlappingEdits is PLAN §8's second M2 criterion,
// at the command layer: a server that emits two edits fighting over the
// same bytes gets nothing written.
func TestRenameRefusesOverlappingEdits(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	hostile := changesEdit(map[string][]any{
		filepath.Join(dir, "a.go"): {textEdit(2, 5, 8, "New")},
		filepath.Join(dir, "b.go"): {
			textEdit(2, 25, 28, "New"),
			textEdit(2, 26, 29, "Evil"),
		},
	})
	s := renameScenario(t, dir, hostile)
	s.apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "edit_conflict" {
		t.Errorf("envelope = %+v, want ok:false code edit_conflict", env)
	}
	assertUnchanged(t, dir, before)

	// And the preview must refuse too: a diff of an edit set that
	// cannot be applied is worse than no diff at all.
	if code, _, _ := runMain("rename", declLoc(dir), "New"); code != ExitProblems {
		t.Errorf("preview exit code = %d, want %d", code, ExitProblems)
	}
	assertUnchanged(t, dir, before)
}

// TestRenameRefusesSnippetEdits: the edit shape that the generated LSP
// types would silently turn into a deletion has to be refused at the
// command layer too.
func TestRenameRefusesSnippetEdits(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	s := renameScenario(t, dir, map[string]any{
		"changes": map[string]any{
			uriOf(filepath.Join(dir, "a.go")): []any{
				map[string]any{
					"range":   map[string]any{"start": map[string]any{"line": 2, "character": 5}, "end": map[string]any{"line": 2, "character": 8}},
					"snippet": map[string]any{"value": "New$0"},
				},
			},
		},
	})
	s.apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	assertUnchanged(t, dir, before)
}

// TestRenameDiffReproducesApply is PLAN §8's third M2 criterion. Two
// identical trees: one is patched with `git apply` from the preview,
// the other written by --apply, and they must end up byte-identical.
func TestRenameDiffReproducesApply(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	applied := tree(t, fixtureFiles)
	patched := tree(t, fixtureFiles)
	s := renameScenario(t, applied, renameToNew(applied))
	s.apply(t)

	code, patch, stderr := runMain("rename", declLoc(applied), "New", "--format", "diff")
	if code != ExitOK {
		t.Fatalf("preview exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	patchFile := filepath.Join(t.TempDir(), "rename.patch")
	write(t, patchFile, patch)

	cmd := exec.Command(git, "apply", "-p1", "--whitespace=nowarn", patchFile)
	cmd.Dir = patched
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\npatch:\n%s", err, out, patch)
	}

	code, _, stderr = runMain("rename", declLoc(applied), "New", "--apply", "--allow-dirty")
	if code != ExitOK {
		t.Fatalf("apply exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}

	wantTree := snapshot(t, applied)
	gotTree := snapshot(t, patched)
	if len(wantTree) != len(gotTree) {
		t.Fatalf("git apply produced %d files, --apply produced %d", len(gotTree), len(wantTree))
	}
	for name, want := range wantTree {
		if gotTree[name] != want {
			t.Errorf("%s: git apply gave\n%q\n--apply gave\n%q", name, gotTree[name], want)
		}
	}
	// And the result is the rename we asked for, not two matching
	// no-ops.
	for name, want := range renamedFiles {
		if wantTree[name] != want {
			t.Errorf("%s = %q, want %q", name, wantTree[name], want)
		}
	}
}

// TestRenameChecksPrepareRenameFirst: a position the server will not
// rename must fail before textDocument/rename is ever sent, so that no
// edit set exists to be staged.
func TestRenameChecksPrepareRenameFirst(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	s := renameScenario(t, dir, renameToNew(dir))
	s.results[methodPrepareRename] = nil // "you cannot rename here"
	readTrace := s.traceTo(t)
	s.apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "not_found" {
		t.Errorf("envelope = %+v, want ok:false code not_found", env)
	}
	for _, msg := range readTrace() {
		if msg.Method == methodRename {
			t.Fatalf("textDocument/rename was sent after prepareRename refused")
		}
	}
	assertUnchanged(t, dir, before)
}

// TestRenameSkipsPrepareRenameWhenUnadvertised: PLAN §5.4 forbids
// calling a method the server never claimed, even a helpful one.
func TestRenameSkipsPrepareRenameWhenUnadvertised(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := renameScenario(t, dir, renameToNew(dir))
	s.capabilities = mutationServerCaps(map[string]any{"renameProvider": true})
	readTrace := s.traceTo(t)
	s.apply(t)

	if code, _, stderr := runMain("rename", declLoc(dir), "New"); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	for _, msg := range readTrace() {
		if msg.Method == methodPrepareRename {
			t.Fatal("prepareRename was called although the server never advertised it")
		}
	}
}

// TestRenameWithoutCapabilityFailsLoudly: exit 3, and the message says
// what the server can do instead.
func TestRenameWithoutCapabilityFailsLoudly(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	s := renameScenario(t, dir, renameToNew(dir))
	s.capabilities = mutationServerCaps(map[string]any{"renameProvider": nil})
	s.apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Errorf("envelope = %+v, want ok:false code unsupported_method", env)
	}
	assertUnchanged(t, dir, before)
}

// TestMutationsWhileIndexingExitNotReady: PLAN §5.2 does not stop at
// the read-only commands. An edit computed against a half-built index
// is the same lie as an empty reference list, with a write behind it.
func TestMutationsWhileIndexingExitNotReady(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"rename", []string{"rename", declLoc(dir), "New", "--apply", "--allow-dirty"}},
		{"format", []string{"format", filepath.Join(dir, "a.go"), "--apply", "--allow-dirty"}},
		{"codeaction", []string{"codeaction", declLoc(dir), "--index", "1", "--apply", "--allow-dirty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := scenario{
				indexing:     true,
				capabilities: mutationServerCaps(nil),
				results: map[string]any{
					methodPrepareRename: nil,
					methodRename:        map[string]any{},
					methodFormatting:    []any{},
					methodCodeAction:    []any{},
				},
			}
			s.apply(t)
			args := append(append([]string{}, tc.args...), "--timeout", "1s", "--settle", "20ms")
			code, stdout, _ := runMain(args...)
			if code != ExitNotReady {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNotReady, stdout)
			}
			env := decodeEnvelope(t, stdout)
			if env.OK || env.Error == nil || env.Error.Code != "not_ready" {
				t.Errorf("envelope = %+v, want ok:false code not_ready", env)
			}
			assertUnchanged(t, dir, before)
		})
	}
}

// TestMutationFormats: every mutation honours json, text and diff, and
// refuses sarif before starting a server.
func TestMutationFormats(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := renameScenario(t, dir, renameToNew(dir))
	s.apply(t)
	loc := declLoc(dir)

	code, stdout, _ := runMain("rename", loc, "New", "--format", "json")
	if code != ExitOK {
		t.Fatalf("json: exit code = %d", code)
	}
	if env := decodeEnvelope(t, stdout); !env.OK {
		t.Errorf("json: envelope ok = false: %+v", env.Error)
	}

	code, stdout, _ = runMain("rename", loc, "New", "--format", "text")
	if code != ExitOK {
		t.Fatalf("text: exit code = %d", code)
	}
	for _, want := range []string{"a.go:3:6: New", "b.go:3:26: New", "c.go:3:26: New", "c.go:3:34: New"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output is missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = runMain("rename", loc, "New", "--format", "diff")
	if code != ExitOK || !strings.HasPrefix(stdout, "diff --git") {
		t.Errorf("diff: exit code = %d, output starts %q", code, firstLine(stdout))
	}

	code, stdout, _ = runMain("rename", loc, "New", "--format", "sarif")
	if code != ExitUsage {
		t.Fatalf("sarif: exit code = %d, want %d", code, ExitUsage)
	}
	if env := decodeEnvelope(t, stdout); env.OK || env.Error == nil || env.Error.Code != "unsupported_format" {
		t.Errorf("sarif: envelope = %+v, want code unsupported_format", env)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// TestRenameUsage covers the argument checking that must not need a
// server.
func TestRenameUsage(t *testing.T) {
	dir := tree(t, fixtureFiles)
	for _, args := range [][]string{
		{"rename", declLoc(dir)},
		{"rename", declLoc(dir), "New", "extra"},
		{"rename", declLoc(dir), "  "},
		{"rename", filepath.Join(dir, "nope.go") + ":1:1", "New"},
	} {
		if code, _, _ := runMain(args...); code != ExitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitUsage)
		}
	}
}
