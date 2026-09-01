package serverdef

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A testEnv is a hermetic machine: its own workspace, its own
// configuration directory, its own PATH of fake binaries, and its own
// environment. Nothing in it touches the real filesystem outside
// t.TempDir, the real PATH or a real language server, which is what
// lets the layering be tested exhaustively.
type testEnv struct {
	t         *testing.T
	Workspace string
	ConfigDir string
	BinDir    string
	Vars      map[string]string

	// Runner, if set, replaces the process runner. Nil means no
	// command can be run at all, which is the right default: a test
	// that unexpectedly shells out should fail, not succeed slowly.
	Runner Runner

	// Env records the environment of every command the test ran, in
	// order, so that a test can assert what the child process was
	// given and not only what it was called with.
	Env [][]string
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	base := t.TempDir()
	e := &testEnv{
		t:         t,
		Workspace: filepath.Join(base, "workspace"),
		ConfigDir: filepath.Join(base, "config", "lightspeed"),
		BinDir:    filepath.Join(base, "bin"),
		Vars:      map[string]string{},
	}
	for _, dir := range []string{e.Workspace, e.ConfigDir, e.BinDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e.Vars["PATH"] = e.BinDir
	e.Vars["HOME"] = filepath.Join(base, "home")
	return e
}

// WriteWorkspace writes the workspace's .lightspeed.toml.
func (e *testEnv) WriteWorkspace(content string) {
	e.t.Helper()
	e.write(filepath.Join(e.Workspace, WorkspaceFile), content)
}

// WriteUser writes one file into the user layer's servers.d.
func (e *testEnv) WriteUser(name, content string) {
	e.t.Helper()
	dir := filepath.Join(e.ConfigDir, ServersDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		e.t.Fatal(err)
	}
	e.write(filepath.Join(dir, name), content)
}

func (e *testEnv) write(path, content string) {
	e.t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// Binary puts a runnable fake executable on the test PATH. Its contents
// are never executed by these tests; only its existence and mode are.
func (e *testEnv) Binary(name string) string {
	e.t.Helper()
	path := filepath.Join(e.BinDir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// UnexecutableBinary puts a file with the right name and no executable
// bit on PATH: what an extracted archive or a bad install leaves behind.
func (e *testEnv) UnexecutableBinary(name string) string {
	e.t.Helper()
	path := filepath.Join(e.BinDir, name)
	if err := os.WriteFile(path, []byte("not executable"), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// DirectoryBinary puts a directory where a binary should be.
func (e *testEnv) DirectoryBinary(name string) string {
	e.t.Helper()
	path := filepath.Join(e.BinDir, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// BrokenShim puts a dangling symlink on PATH: exactly what a version
// manager leaves when its shim points at a tool version that is not
// installed. An exec lookup cannot see it at all, which is why
// diagnosing it needs Lstat.
func (e *testEnv) BrokenShim(name string) string {
	e.t.Helper()
	path := filepath.Join(e.BinDir, name)
	if err := os.Symlink(filepath.Join(e.BinDir, "missing-target"), path); err != nil {
		e.t.Fatal(err)
	}
	return path
}

// Options builds the Options for this environment.
func (e *testEnv) Options() Options {
	return Options{
		WorkspaceRoot: e.Workspace,
		ConfigDir:     e.ConfigDir,
		Getenv:        e.getenv,
		LookPath:      e.lookPath,
		Run:           e.run,
	}
}

func (e *testEnv) getenv(key string) string { return e.Vars[key] }

// lookPath mirrors exec.LookPath's semantics — a regular, executable
// file on PATH — over the test's own PATH.
func (e *testEnv) lookPath(name string) (string, error) {
	if filepath.IsAbs(name) {
		return name, nil
	}
	for _, dir := range filepath.SplitList(e.Vars["PATH"]) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		fi, err := os.Stat(candidate)
		if err != nil || fi.IsDir() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate, nil
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

func (e *testEnv) run(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	if e.Runner == nil {
		e.t.Fatalf("test ran a command it did not expect: %s %v", name, args)
	}
	e.Env = append(e.Env, env)
	return e.Runner(ctx, env, name, args...)
}

// fakeMise is a Runner that answers the two mise calls this package
// makes. version is reported by `--version`; which maps a binary name
// to the path `mise which` returns.
func fakeMise(version string, which map[string]string) Runner {
	return func(_ context.Context, _ []string, name string, args ...string) (string, string, error) {
		if len(args) == 0 {
			return "", "", exec.ErrNotFound
		}
		switch args[0] {
		case "--version":
			return version + "\n", "", nil
		case "which":
			if len(args) < 2 {
				return "", "usage\n", &exec.ExitError{}
			}
			if path, ok := which[args[1]]; ok {
				return path + "\n", "", nil
			}
			return "", "mise ERROR tool not found\n", &exitStatus{code: 1}
		case "use":
			return "mise installed " + args[len(args)-1] + "\n", "", nil
		default:
			return "", "unexpected: " + args[0], &exitStatus{code: 2}
		}
	}
}

// exitStatus is a stand-in for *exec.ExitError, which cannot be built
// with a chosen status outside os/exec.
type exitStatus struct{ code int }

func (e *exitStatus) Error() string { return "exit status " + itoa(e.code) }

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
