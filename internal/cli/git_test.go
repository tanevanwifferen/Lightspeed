package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// PLAN §9.4's answer, tested. See git.go for why the answer is "yes,
// refuse, with --allow-dirty".
//
// These are the only tests in the package that run a real subprocess
// other than the fake server. They are still hermetic — a repository
// inside the test's own temporary directory, no network, no host
// configuration — and they skip rather than fail where git is absent.

// initRepo turns dir into a git repository with everything committed.
// The config is passed per-invocation and the ambient config files are
// pointed at /dev/null, so a developer's global gitconfig (a signing
// key, a template, a hook path) cannot change the outcome.
func initRepo(t *testing.T, dir string) {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=lightspeed", "GIT_AUTHOR_EMAIL=lightspeed@example.invalid",
			"GIT_COMMITTER_NAME=lightspeed", "GIT_COMMITTER_EMAIL=lightspeed@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("-c", "commit.gpgsign=false", "commit", "-qm", "fixture")
}

// TestApplyRefusesDirtyWorktree is the refusal itself: uncommitted work
// in the tree means `git checkout` is no longer an undo, so nothing is
// written.
func TestApplyRefusesDirtyWorktree(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	// The agent's own uncommitted work, in a file the rename does not
	// even touch.
	write(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.27\n// edited\n")
	before := snapshot(t, dir)
	renameScenario(t, dir, renameToNew(dir)).apply(t)

	code, stdout, _ := runMain("rename", declLoc(dir), "New", "--apply")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "dirty_worktree" {
		t.Fatalf("envelope = %+v, want ok:false code dirty_worktree", env)
	}
	for _, want := range []string{"go.mod", "--allow-dirty"} {
		if !strings.Contains(env.Error.Message, want) {
			t.Errorf("the refusal does not mention %q: %s", want, env.Error.Message)
		}
	}
	assertUnchanged(t, dir, before)

	// The escape hatch works, and is the only thing that makes it work.
	code, _, stderr := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty")
	if code != ExitOK {
		t.Fatalf("--allow-dirty: exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	if got := snapshot(t, dir)["a.go"]; got != renamedFiles["a.go"] {
		t.Errorf("a.go = %q, want %q", got, renamedFiles["a.go"])
	}
}

// TestApplyRefusesBeforeStartingAServer: the check is a precondition,
// so it must cost nothing. A rust-analyzer workspace takes a minute to
// load, and finding out afterwards would make the refusal useless.
func TestApplyRefusesBeforeStartingAServer(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	write(t, filepath.Join(dir, "a.go"), fixtureFiles["a.go"]+"// dirt\n")
	s := renameScenario(t, dir, renameToNew(dir))
	readTrace := s.traceTo(t)
	s.apply(t)

	if code, _, _ := runMain("rename", declLoc(dir), "New", "--apply"); code != ExitProblems {
		t.Fatalf("exit code = %d, want %d", code, ExitProblems)
	}
	if msgs := readTrace(); len(msgs) > 0 {
		t.Errorf("a server was consulted before the worktree check: %+v", msgs)
	}
}

// TestApplyAllowsCleanWorktree: the common case is that the check is
// invisible.
func TestApplyAllowsCleanWorktree(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	renameScenario(t, dir, renameToNew(dir)).apply(t)

	code, stdout, stderr := runMain("rename", declLoc(dir), "New", "--apply", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	if len(env.Warnings) != 0 {
		t.Errorf("a clean repository produced warnings: %v", env.Warnings)
	}
	if got := snapshot(t, dir)["c.go"]; got != renamedFiles["c.go"] {
		t.Errorf("c.go = %q, want %q", got, renamedFiles["c.go"])
	}
}

// TestUntrackedFilesAreNotDirt: `git checkout` does not remove
// untracked files, so their presence does not cost the caller an undo.
// Refusing over a stray build artefact would teach callers to pass
// --allow-dirty reflexively, which is worse than not checking at all.
func TestUntrackedFilesAreNotDirt(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	write(t, filepath.Join(dir, "scratch.txt"), "notes\n")
	renameScenario(t, dir, renameToNew(dir)).apply(t)

	code, stdout, stderr := runMain("rename", declLoc(dir), "New", "--apply")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	if got := snapshot(t, dir)["a.go"]; got != renamedFiles["a.go"] {
		t.Errorf("a.go = %q, want %q", got, renamedFiles["a.go"])
	}
}

// TestApplyOutsideARepositoryWarns: lightspeed is not a git tool, so a
// tree that is not a repository is written — but the caller is told
// that there is no undo, in the envelope rather than a log.
func TestApplyOutsideARepositoryWarns(t *testing.T) {
	dir := tree(t, fixtureFiles)
	renameScenario(t, dir, renameToNew(dir)).apply(t)

	code, stdout, stderr := runMain("rename", declLoc(dir), "New", "--apply", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "no undo") {
		t.Errorf("warnings do not mention the missing undo: %v", env.Warnings)
	}
	if got := snapshot(t, dir)["b.go"]; got != renamedFiles["b.go"] {
		t.Errorf("b.go = %q, want %q", got, renamedFiles["b.go"])
	}
}

// TestPreviewNeverChecksTheWorktree: a preview writes nothing, so it
// has nothing to protect and must not refuse.
func TestPreviewNeverChecksTheWorktree(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	write(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.27\n// edited\n")
	before := snapshot(t, dir)
	renameScenario(t, dir, renameToNew(dir)).apply(t)

	if code, _, stderr := runMain("rename", declLoc(dir), "New"); code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	assertUnchanged(t, dir, before)
}

// TestPorcelainPaths checks the `git status -z` parser directly,
// because a rename entry carries two NUL-separated paths and reading
// the second as a change of its own would report a path that did not
// change.
func TestPorcelainPaths(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want []string
	}{
		{"clean", "", nil},
		{"one modification", " M a.go\x00", []string{"a.go"}},
		{"staged and unstaged", "M  a.go\x00 D b.go\x00", []string{"a.go", "b.go"}},
		{"rename", "R  new.go\x00old.go\x00 M c.go\x00", []string{"new.go", "c.go"}},
		{"path with a space", " M dir/a b.go\x00", []string{"dir/a b.go"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := porcelainPaths([]byte(tc.out)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("porcelainPaths(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// TestWorktreeStatusOutsideARepository: the graceful half of the
// "handle it gracefully" requirement.
func TestWorktreeStatusOutsideARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	if _, _, err := worktreeStatus(dir); err == nil {
		t.Fatal("worktreeStatus reported a repository where there is none")
	} else if !strings.Contains(err.Error(), "not inside a git repository") {
		t.Errorf("error = %v, want it to name the missing repository", err)
	}
	warnings, err := checkWorktrees([]string{filepath.Join(dir, "a.go")}, false)
	if err != nil {
		t.Fatalf("checkWorktrees refused outside a repository: %v", err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "no undo") {
		t.Errorf("warnings = %v, want one about the missing undo", warnings)
	}
}
