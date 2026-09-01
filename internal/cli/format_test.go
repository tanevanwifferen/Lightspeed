package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// The format fixture. x.go has a double space where one belongs;
// short.go is one line long, so the same edit is out of range in it —
// which is how one bad file among several is scripted without needing
// per-file scripted answers.
//
//	x.go line 3: `func  X() {}`   the extra space is UTF-16 4-6
var formatFiles = map[string]string{
	"x.go":     "package fixture\n\nfunc  X() {}\n",
	"short.go": "package fixture\n",
}

// squeezeSpace is the formatting answer: turn the double space into one.
func squeezeSpace() []any { return []any{textEdit(2, 4, 6, " ")} }

func formatScenario(result any) scenario {
	return scenario{
		capabilities: mutationServerCaps(nil),
		results:      map[string]any{methodFormatting: result},
	}
}

// TestFormatPreviewsByDefault: like rename, format shows a diff and
// writes nothing until asked.
func TestFormatPreviewsByDefault(t *testing.T) {
	dir := tree(t, map[string]string{"x.go": formatFiles["x.go"]})
	before := snapshot(t, dir)
	formatScenario(squeezeSpace()).apply(t)

	code, stdout, stderr := runMain("format", filepath.Join(dir, "x.go"))
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	for _, want := range []string{"diff --git a/x.go b/x.go", "-func  X() {}", "+func X() {}"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("preview is missing %q:\n%s", want, stdout)
		}
	}
	assertUnchanged(t, dir, before)
}

// TestFormatAppliesEveryFileInOneTransaction: several files, one
// commit. Both are written or neither is.
func TestFormatAppliesEveryFileInOneTransaction(t *testing.T) {
	dir := tree(t, map[string]string{
		"x.go": formatFiles["x.go"],
		"y.go": "package fixture\n\nfunc  Y() {}\n",
	})
	formatScenario(squeezeSpace()).apply(t)

	x, y := filepath.Join(dir, "x.go"), filepath.Join(dir, "y.go")
	code, stdout, stderr := runMain("format", x, y, "--apply", "--allow-dirty", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	got := snapshot(t, dir)
	if got["x.go"] != "package fixture\n\nfunc X() {}\n" {
		t.Errorf("x.go = %q", got["x.go"])
	}
	if got["y.go"] != "package fixture\n\nfunc Y() {}\n" {
		t.Errorf("y.go = %q", got["y.go"])
	}
}

// TestFormatWritesNothingWhenOneFileFails: the second file's edit does
// not fit, and the first file — whose edit was perfectly good — is left
// alone. This is the same all-or-nothing guarantee as the 3-file
// rename, reached by a different command.
func TestFormatWritesNothingWhenOneFileFails(t *testing.T) {
	dir := tree(t, formatFiles)
	before := snapshot(t, dir)
	formatScenario(squeezeSpace()).apply(t)

	code, stdout, _ := runMain("format",
		filepath.Join(dir, "x.go"), filepath.Join(dir, "short.go"),
		"--apply", "--allow-dirty")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "edit_conflict" {
		t.Errorf("envelope = %+v, want ok:false code edit_conflict", env)
	}
	assertUnchanged(t, dir, before)
}

// TestFormatAlreadyFormattedSucceeds: no edits is this command's
// success, not its failure — unlike rename, where no edits means
// nothing was renamed.
func TestFormatAlreadyFormattedSucceeds(t *testing.T) {
	dir := tree(t, map[string]string{"x.go": "package fixture\n"})
	before := snapshot(t, dir)
	formatScenario([]any{}).apply(t)

	code, stdout, stderr := runMain("format", filepath.Join(dir, "x.go"),
		"--apply", "--allow-dirty", "--format", "json", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	assertUnchanged(t, dir, before)
}

// TestFormatRefusesTwoWorkspaces: a transaction has one root, so two
// roots is a usage error naming both rather than two half-applied
// commits.
func TestFormatRefusesTwoWorkspaces(t *testing.T) {
	one := tree(t, map[string]string{"x.go": formatFiles["x.go"]})
	two := tree(t, map[string]string{"x.go": formatFiles["x.go"]})
	before := snapshot(t, one)
	formatScenario(squeezeSpace()).apply(t)

	code, stdout, _ := runMain("format",
		filepath.Join(one, "x.go"), filepath.Join(two, "x.go"), "--apply", "--allow-dirty")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
	}
	if env := decodeEnvelope(t, stdout); env.OK || env.Error == nil || env.Error.Code != "usage" {
		t.Errorf("envelope = %+v, want ok:false code usage", env)
	}
	assertUnchanged(t, one, before)
}

// TestFormatUsage: the argument checking that costs no server.
func TestFormatUsage(t *testing.T) {
	dir := tree(t, formatFiles)
	for _, args := range [][]string{
		{"format"},
		{"format", filepath.Join(dir, "nope.go")},
		{"format", dir},
		{"format", filepath.Join(dir, "x.go"), "--tab-size", "0"},
	} {
		if code, _, _ := runMain(args...); code != ExitUsage {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitUsage)
		}
	}
}

// TestFormatDeduplicatesRepeatedPaths: naming a file twice must not
// send its edits twice, which the applier would refuse as overlapping —
// an error about the server for a mistake on the command line.
func TestFormatDeduplicatesRepeatedPaths(t *testing.T) {
	dir := tree(t, map[string]string{"x.go": formatFiles["x.go"]})
	formatScenario(squeezeSpace()).apply(t)

	x := filepath.Join(dir, "x.go")
	code, stdout, stderr := runMain("format", x, x, "--apply", "--allow-dirty")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	if got := snapshot(t, dir)["x.go"]; got != "package fixture\n\nfunc X() {}\n" {
		t.Errorf("x.go = %q", got)
	}
}
