package serverdef

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probe is the shortest path from an environment to one server's binary
// status, so each case below reads as one sentence.
func probe(t *testing.T, e *testEnv, name string) Binary {
	t.Helper()
	opts := e.Options()
	res, err := Load(opts)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	res.Probe(t.Context(), opts)
	return resolvedOf(t, res, name).Binary
}

// TestProbeOnPATH is PLAN §6 item 4: the binary is simply on PATH, and
// that is the whole configuration needed.
func TestProbeOnPATH(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	want := e.Binary("gopls")

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != want {
		t.Fatalf("binary = %+v, want runnable at %s", got, want)
	}
	if got.Source != BinaryPATH {
		t.Errorf("source = %v, want %v", got.Source, BinaryPATH)
	}
	if got.Problem != "" {
		t.Errorf("problem = %q, want none", got.Problem)
	}
	if !got.Probed {
		t.Error("Probed = false after probing")
	}
}

func TestProbeNotFound(t *testing.T) {
	e := newTestEnv(t)
	opts := e.Options()
	opts.SkipMise = true
	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	res.Probe(t.Context(), opts)

	got := resolvedOf(t, res, "gopls").Binary
	if got.Runnable || got.Path != "" {
		t.Fatalf("binary = %+v, want nothing found", got)
	}
	if got.Source != BinaryNotFound {
		t.Errorf("source = %v, want %v", got.Source, BinaryNotFound)
	}
	if !strings.Contains(got.Problem, "not found on PATH") {
		t.Errorf("problem = %q, want it to say what was looked for", got.Problem)
	}
}

// TestProbeUnusable covers the three ways a name can be on PATH and
// still not be a language server. Telling these apart from "not
// installed" is the point: the fix is different, and `doctor` has to
// say which.
func TestProbeUnusable(t *testing.T) {
	tests := []struct {
		name        string
		place       func(e *testEnv, name string) string
		wantProblem string
	}{{
		name:        "no executable bit",
		place:       (*testEnv).UnexecutableBinary,
		wantProblem: "is not executable",
	}, {
		name:        "a directory",
		place:       (*testEnv).DirectoryBinary,
		wantProblem: "is a directory",
	}, {
		name:        "a dangling shim",
		place:       (*testEnv).BrokenShim,
		wantProblem: "symlink target is missing",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.Runner = fakeMise("2026.8.14", nil)
			path := tt.place(e, "gopls")

			got := probe(t, e, "gopls")
			if got.Runnable {
				t.Fatalf("binary = %+v, want unusable", got)
			}
			if got.Source != BinaryUnusable {
				t.Errorf("source = %v, want %v", got.Source, BinaryUnusable)
			}
			if got.Path != path {
				t.Errorf("path = %q, want %q: the diagnosis must name what it found", got.Path, path)
			}
			if !strings.Contains(got.Problem, tt.wantProblem) {
				t.Errorf("problem = %q, want it to mention %q", got.Problem, tt.wantProblem)
			}
		})
	}
}

// TestProbeCommandPath: a definition whose command is a path is used
// verbatim, since the user has said exactly where the server is.
func TestProbeCommandPath(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	elsewhere := filepath.Join(t.TempDir(), "custom-gopls")
	if err := os.WriteFile(elsewhere, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.WriteWorkspace("schema_version = 1\nname = \"gopls\"\n[server]\ncommand = [\"" + elsewhere + "\"]\n")

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != elsewhere {
		t.Fatalf("binary = %+v, want runnable at %s", got, elsewhere)
	}
	if got.Source != BinaryCommandPath {
		t.Errorf("source = %v, want %v", got.Source, BinaryCommandPath)
	}
}

func TestProbeCommandPathMissing(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	missing := filepath.Join(t.TempDir(), "nope", "gopls")
	e.WriteWorkspace("schema_version = 1\nname = \"gopls\"\n[server]\ncommand = [\"" + missing + "\"]\n")

	got := probe(t, e, "gopls")
	if got.Runnable {
		t.Fatalf("binary = %+v, want not runnable", got)
	}
	if got.Source != BinaryCommandPath || got.Path != missing {
		t.Errorf("binary = %+v, want the command path reported back", got)
	}
	if !strings.Contains(got.Problem, "does not exist") {
		t.Errorf("problem = %q, want it to say the path does not exist", got.Problem)
	}
}

// TestProbeFallsBackToMise: a tool mise installed but whose shims are
// not on this shell's PATH is still found, and reported as mise's.
func TestProbeFallsBackToMise(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	installed := filepath.Join(t.TempDir(), "installs", "gopls")
	if err := os.MkdirAll(filepath.Dir(installed), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Runner = fakeMise("2026.8.14", map[string]string{"gopls": installed})

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != installed {
		t.Fatalf("binary = %+v, want runnable at %s", got, installed)
	}
	if got.Source != BinaryMise {
		t.Errorf("source = %v, want %v", got.Source, BinaryMise)
	}
}

// TestProbePrefersPATHOverMise: PATH is the cheap answer and the one
// the user's shell would have used, so it wins when both work.
func TestProbePrefersPATHOverMise(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	onPath := e.Binary("gopls")
	e.Runner = fakeMise("2026.8.14", map[string]string{"gopls": "/somewhere/else/gopls"})

	got := probe(t, e, "gopls")
	if got.Path != onPath || got.Source != BinaryPATH {
		t.Errorf("binary = %+v, want the PATH copy at %s", got, onPath)
	}
}

// TestProbeBrokenShimWithMiseBehindIt: a dangling shim on PATH does not
// stop mise's real binary from being found. This is the common shape of
// a half-activated version manager.
func TestProbeBrokenShimWithMiseBehindIt(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	e.BrokenShim("gopls")
	behind := filepath.Join(t.TempDir(), "gopls")
	if err := os.WriteFile(behind, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Runner = fakeMise("2026.8.14", map[string]string{"gopls": behind})

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != behind || got.Source != BinaryMise {
		t.Errorf("binary = %+v, want mise's %s", got, behind)
	}
}

// TestProbeSkipMise: with mise disabled, only PATH is consulted — and
// no process is run at all, which the test enforces by leaving the
// runner nil.
func TestProbeSkipMise(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	opts := e.Options()
	opts.SkipMise = true

	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	res.Probe(t.Context(), opts)

	if res.Mise.Available {
		t.Errorf("mise = %+v, want unavailable when skipped", res.Mise)
	}
	if !res.Mise.Skipped {
		t.Error("mise status does not report that it was skipped")
	}
	if got := resolvedOf(t, res, "gopls").Binary; got.Source != BinaryNotFound {
		t.Errorf("binary = %+v, want not found", got)
	}
}

// TestProbeUsesRealLookPath exercises the production lookup — no
// injected LookPath, no injected runner — against a temporary PATH.
func TestProbeUsesRealLookPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gopls"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	// ConfigDir is pinned to an empty temporary directory: the real
	// environment must not leak a servers.d into a test.
	opts := Options{SkipMise: true, ConfigDir: t.TempDir()}
	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	res.Probe(t.Context(), opts)

	got := resolvedOf(t, res, "gopls").Binary
	if !got.Runnable || got.Path != filepath.Join(dir, "gopls") {
		t.Fatalf("binary = %+v, want the one on the temporary PATH", got)
	}
	if got := resolvedOf(t, res, "clangd").Binary; got.Runnable {
		t.Errorf("clangd = %+v, want nothing found on a PATH with only gopls on it", got)
	}
}

func TestScanPathForName(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "gopls"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	pathEnv := strings.Join([]string{"", dir, other}, string(filepath.ListSeparator))

	path, problem, found := scanPathForName("gopls", pathEnv)
	if !found {
		t.Fatal("scanPathForName did not find the unexecutable file")
	}
	if path != filepath.Join(other, "gopls") {
		t.Errorf("path = %q, want the file in the second PATH entry", path)
	}
	if !strings.Contains(problem, "not executable") {
		t.Errorf("problem = %q", problem)
	}
	if _, _, found := scanPathForName("nothing-here", pathEnv); found {
		t.Error("scanPathForName found a name that is not there")
	}
}

func TestBinaryString(t *testing.T) {
	tests := []struct {
		binary Binary
		want   string
	}{
		{Binary{Name: "gopls"}, "gopls: not looked for"},
		{Binary{Name: "gopls", Probed: true, Runnable: true, Path: "/usr/bin/gopls", Source: BinaryPATH}, "gopls: /usr/bin/gopls (path)"},
		{Binary{Name: "gopls", Probed: true, Path: "/usr/bin/gopls", Problem: "is a directory"}, "gopls: /usr/bin/gopls is not runnable (is a directory)"},
		{Binary{Name: "gopls", Probed: true, Problem: "not found on PATH"}, "gopls: not found (not found on PATH)"},
	}
	for _, tt := range tests {
		if got := tt.binary.String(); got != tt.want {
			t.Errorf("String() = %q, want %q", got, tt.want)
		}
	}
}
