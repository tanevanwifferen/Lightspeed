package render

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/diff"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// legacySource and oldSource are the files the fixture deletes and
// renames; they exist so the diff shows real content leaving and
// arriving rather than just naming files.
const legacySource = `package store

// Deprecated: use UserRepository.
type LegacyRepo struct{}
`

const oldSource = `package store

func helper(r *UserRepo) {}
`

const createdSource = `package store

// UserRepository is the renamed UserRepo.
type UserRepository = UserRepo
`

// renameFixture is the change set `lightspeed rename UserRepo
// UserRepository` would preview: two files edited, one created, one
// deleted, one renamed-with-edits, and a non-ASCII file whose edits
// change the byte length of every following column.
func renameFixture(t *testing.T) ChangeSet {
	t.Helper()

	ascii := mapper("internal/store/user.go", asciiSource)
	asciiEdits := []protocol.TextEdit{
		{Range: rangeOf(t, ascii, "UserRepo", 1), NewText: "UserRepository"},
		{Range: rangeOf(t, ascii, "UserRepo", 2), NewText: "UserRepository"},
	}
	modify, err := NewChange(ascii, asciiEdits)
	if err != nil {
		t.Fatalf("NewChange(user.go): %v", err)
	}

	cjk := mapper("internal/fixture/fixture.go", cjkSource)
	cjkEdits := []protocol.TextEdit{
		{Range: rangeOf(t, cjk, "ユーザー名", 1), NewText: "利用者名"},
		{Range: rangeOf(t, cjk, "ユーザー名", 2), NewText: "利用者名"},
	}
	cjkModify, err := NewChange(cjk, cjkEdits)
	if err != nil {
		t.Fatalf("NewChange(fixture.go): %v", err)
	}

	legacy := mapper("internal/store/legacy.go", legacySource)
	del, err := NewDeleteChange(legacy)
	if err != nil {
		t.Fatalf("NewDeleteChange: %v", err)
	}

	old := mapper("internal/store/old.go", oldSource)
	renamed, err := NewRenameChange(old, protocol.URIFromPath("/w/internal/store/new.go"),
		[]protocol.TextEdit{{Range: rangeOf(t, old, "UserRepo", 0), NewText: "UserRepository"}})
	if err != nil {
		t.Fatalf("NewRenameChange: %v", err)
	}

	created := NewCreateChange(protocol.URIFromPath("/w/internal/store/repository.go"), createdSource)

	cs := ChangeSet{Changes: []Change{modify, cjkModify, del, renamed, created}}
	cs.Sort()
	return cs
}

func TestChangesDiff(t *testing.T) {
	cs := renameFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, cs, Options{Root: fixtureRoot})
	})
	golden(t, "rename_diff.txt", got)
}

func TestChangesJSON(t *testing.T) {
	cs := renameFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatJSON, cs, Options{Root: fixtureRoot, Indent: true})
	})
	golden(t, "rename_json.json", got)

	var env struct {
		Data changesData `json:"data"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if env.Data.Count != 5 || env.Data.Edits != 5 {
		t.Errorf("count = %d edits = %d, want 5 and 5", env.Data.Count, env.Data.Edits)
	}
	// Every change carries its own patch, so an agent that already
	// asked for JSON need not re-run with --format diff.
	for _, c := range env.Data.Changes {
		if c.Diff == "" {
			t.Errorf("change %s has no diff", c.Path)
		}
		if filepath.IsAbs(c.Path) {
			t.Errorf("change path %q is absolute despite Root", c.Path)
		}
	}
}

func TestChangesText(t *testing.T) {
	cs := renameFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatText, cs, Options{Root: fixtureRoot})
	})
	golden(t, "rename_text.txt", got)

	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		if strings.Count(line, ":") < 2 {
			t.Errorf("line %q is not file:line:col: text", line)
		}
	}
}

// TestChangesDiffAppliesHermetically verifies the patch with the
// vendored unified-diff applier: no git, no subprocess. The vendored
// applier only understands zero-context hunks, so this half is rendered
// with --diff-context 0; TestChangesDiffAppliesWithGit covers the
// default 3-context output.
func TestChangesDiffAppliesHermetically(t *testing.T) {
	for _, c := range renameFixture(t).Changes {
		if c.Kind == ChangeRename {
			// A rename renders as two file patches; ApplyUnified takes
			// one at a time, so git covers this case instead.
			continue
		}
		t.Run(string(c.Kind)+"-"+filepath.Base(c.Path), func(t *testing.T) {
			patch := mustRender(t, func(w *bytes.Buffer) error {
				return Changes(w, FormatDiff, ChangeSet{Changes: []Change{c}},
					Options{Root: fixtureRoot, DiffContext: DiffContextLines(0)})
			})
			got, err := diff.ApplyUnified(stripPatchHeaders(string(patch)), c.Before)
			if err != nil {
				t.Fatalf("the patch does not apply: %v\n%s", err, patch)
			}
			if got != c.After {
				t.Errorf("applying the patch produced\n%q\nwant\n%q", got, c.After)
			}
		})
	}
}

// TestChangesDiffAppliesWithGit is the PLAN §4 promise that --format
// diff is "feedable to git apply", checked against real git rather than
// by inspection. It skips when git is absent; the hermetic test above
// always runs.
func TestChangesDiffAppliesWithGit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	cs := renameFixture(t)
	patch := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, cs, Options{Root: fixtureRoot})
	})

	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		// A hermetic git: no user config, no system config, no hooks.
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_TERMINAL_PROMPT=0",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s\npatch:\n%s", strings.Join(args, " "), err, out, patch)
		}
	}

	// Lay down the "before" tree.
	for _, c := range cs.Changes {
		if c.Kind == ChangeCreate {
			continue
		}
		writeFixtureFile(t, dir, c.Path, c.Before)
	}
	if err := os.WriteFile(filepath.Join(dir, "patch.diff"), patch, 0o644); err != nil {
		t.Fatal(err)
	}

	run("init", "-q")
	// --check first: git's own verdict on whether the patch is
	// well-formed and applies cleanly.
	run("apply", "--check", "patch.diff")
	run("apply", "patch.diff")

	// The tree must now match every change's After exactly.
	assertApplied(t, dir, cs.Changes)
}

// assertApplied checks that a tree matches what the change set promised:
// every modified and created file holds its After, every deleted file
// and every rename source is gone.
func assertApplied(t *testing.T, dir string, changes []Change) {
	t.Helper()
	for _, c := range changes {
		switch c.Kind {
		case ChangeDelete:
			assertAbsent(t, dir, c.Path)
		case ChangeRename:
			assertAbsent(t, dir, c.Path)
			assertContent(t, dir, c.NewPath, c.After)
		default:
			assertContent(t, dir, c.Path, c.After)
		}
	}
}

// TestTruncatedDiffStillApplies makes sure the truncation notice cannot
// break the patch it warns about: `git apply` skips the preamble.
func TestTruncatedDiffStillApplies(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	cs := renameFixture(t)
	patch := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, cs, Options{Root: fixtureRoot, Limit: 2})
	})
	golden(t, "rename_truncated_diff.txt", patch)

	if !bytes.HasPrefix(patch, []byte("# changes truncated: showing 2 of 5 changes (--limit 2)\n")) {
		t.Fatalf("truncation was not announced up front:\n%s", patch)
	}

	dir := t.TempDir()
	kept := cs.Changes[:2]
	for _, c := range kept {
		writeFixtureFile(t, dir, c.Path, c.Before)
	}
	if err := os.WriteFile(filepath.Join(dir, "patch.diff"), patch, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "apply", "patch.diff")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\npatch:\n%s", err, out, patch)
	}
	assertApplied(t, dir, kept)
}

// TestDiffWithoutRootUsesAbsoluteLabels documents the other shape: with
// no workspace root there is no `a/`…`b/` prefix to strip, so the patch
// is a plain `diff -u` for `patch -p0`.
func TestDiffWithoutRootUsesAbsoluteLabels(t *testing.T) {
	cs := renameFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, ChangeSet{Changes: cs.Changes[:1]}, Options{})
	})
	golden(t, "rename_diff_absolute.txt", got)

	if bytes.Contains(got, []byte("diff --git")) {
		t.Error("a git header was emitted for an absolute-path patch")
	}
	if !bytes.Contains(got, []byte("--- /w/internal/fixture/fixture.go")) {
		t.Errorf("labels are not the absolute paths:\n%s", got)
	}
}

func TestChangesDiffContextIsHonoured(t *testing.T) {
	// A modify change: a pure delete has no unchanged lines for context
	// to widen.
	cs := ChangeSet{Changes: renameFixture(t).Changes[:1]}
	wide := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, cs, Options{Root: fixtureRoot, DiffContext: DiffContextLines(1)})
	})
	narrow := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, cs, Options{Root: fixtureRoot, DiffContext: DiffContextLines(0)})
	})
	if len(narrow) >= len(wide) {
		t.Errorf("--diff-context 0 produced %d bytes, --diff-context 1 produced %d", len(narrow), len(wide))
	}
}

func TestNoOpChangeProducesNoPatch(t *testing.T) {
	m := mapper("a.go", asciiSource)
	c, err := NewChange(m, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Changes(w, FormatDiff, ChangeSet{Changes: []Change{c}}, Options{Root: fixtureRoot})
	})
	if len(got) != 0 {
		t.Errorf("a change that changes nothing produced a patch:\n%s", got)
	}
}

// TestNewChangeRejectsOverlappingEdits is the render-layer half of PLAN
// §5.3: a hostile edit set must fail before it becomes a plausible
// diff, not after.
func TestNewChangeRejectsOverlappingEdits(t *testing.T) {
	m := mapper("a.go", asciiSource)
	first := rangeOf(t, m, "UserRepo struct", 0)
	second := rangeOf(t, m, "Repo struct {", 0)

	_, err := NewChange(m, []protocol.TextEdit{
		{Range: first, NewText: "X"},
		{Range: second, NewText: "Y"},
	})
	if err == nil {
		t.Fatal("overlapping edits were accepted")
	}
	if got := CodeForError(err); got != CodeEditConflict {
		t.Errorf("code = %q, want %q", got, CodeEditConflict)
	}
	if got := ExitCode(err); got != ExitProblems {
		t.Errorf("exit = %d, want %d", got, ExitProblems)
	}
}

func TestChangesRejectsSARIF(t *testing.T) {
	err := Changes(&bytes.Buffer{}, FormatSARIF, renameFixture(t), Options{})
	if got := CodeForError(err); got != CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q", got, CodeUnsupportedFormat)
	}
}

func TestChangeConstructorsRejectNilMapper(t *testing.T) {
	if _, err := NewChange(nil, nil); err == nil {
		t.Error("NewChange(nil) was accepted")
	}
	if _, err := NewDeleteChange(nil); err == nil {
		t.Error("NewDeleteChange(nil) was accepted")
	}
}

func TestUnknownChangeKindIsAnError(t *testing.T) {
	err := Changes(&bytes.Buffer{}, FormatDiff,
		ChangeSet{Changes: []Change{{Kind: "explode", Path: "/w/a.go"}}}, Options{Root: fixtureRoot})
	if got := CodeForError(err); got != CodeInternal {
		t.Errorf("code = %q, want %q", got, CodeInternal)
	}
}

// stripPatchHeaders removes the git file headers and `#` notices that
// the vendored ApplyUnified does not parse, leaving the ---/+++/@@ body.
func stripPatchHeaders(patch string) string {
	var out []string
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "),
			strings.HasPrefix(line, "new file mode "),
			strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "# "):
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// workspacePath turns a fixture's absolute path into the workspace-
// relative path that the patch labels use, which is where the file has
// to live for `git apply -p1` to find it.
func workspacePath(abs string) string {
	return filepath.FromSlash(Options{Root: fixtureRoot}.displayPath(abs))
}

func writeFixtureFile(t *testing.T, dir, abs, content string) {
	t.Helper()
	path := filepath.Join(dir, workspacePath(abs))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, dir, abs, want string) {
	t.Helper()
	rel := workspacePath(abs)
	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("%s: %v", rel, err)
	}
	if string(got) != want {
		t.Errorf("%s after applying:\n%q\nwant\n%q", rel, got, want)
	}
}

func assertAbsent(t *testing.T, dir, abs string) {
	t.Helper()
	rel := workspacePath(abs)
	if _, err := os.Stat(filepath.Join(dir, rel)); !os.IsNotExist(err) {
		t.Errorf("%s still exists after applying (stat error: %v)", rel, err)
	}
}
