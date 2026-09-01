package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWorkspaceIsStableAcrossSubdirectories is PLAN §3's requirement in
// one assertion: every path inside a repository resolves to the same
// workspace, so `cd internal/store` addresses the daemon that is
// already running instead of starting a second one and paying the
// indexing cost twice.
func TestWorkspaceIsStableAcrossSubdirectories(t *testing.T) {
	repo := t.TempDir()
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(repo, ".git"))
	// A nested Go module: one repository, so one daemon, even though
	// internal/router will resolve two different gopls roots inside
	// it.
	nested := filepath.Join(repo, "tools", "gen")
	mkdir(t, nested)
	write(t, filepath.Join(nested, "go.mod"), "module gen\n")
	write(t, filepath.Join(nested, "main.go"), "package main\n")
	write(t, filepath.Join(repo, "main.go"), "package main\n")

	for _, path := range []string{
		repo,
		filepath.Join(repo, "main.go"),
		nested,
		filepath.Join(nested, "main.go"),
		filepath.Join(repo, "does", "not", "exist.go"),
	} {
		got, err := Workspace(path)
		if err != nil {
			t.Fatalf("Workspace(%s): %v", path, err)
		}
		if got != repo {
			t.Errorf("Workspace(%s) = %q, want %q", path, got, repo)
		}
	}
}

// TestWorkspaceFallsBackToTheFilesOwnDirectory: a file outside any
// repository is a legitimate query and gets its own daemon rather than
// an error.
func TestWorkspaceFallsBackToTheFilesOwnDirectory(t *testing.T) {
	dir := t.TempDir()
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "lonely.go"), "package main\n")

	got, err := Workspace(filepath.Join(dir, "lonely.go"))
	if err != nil {
		t.Fatal(err)
	}
	// The temp directory has no marker, and neither should any of its
	// ancestors, so the fallback is the file's own directory.
	if got != dir {
		t.Errorf("Workspace = %q, want %q", got, dir)
	}
}

// TestWorkspaceNearestMarkerWins: a nested repository — a submodule, a
// vendored checkout — is its own workspace.
func TestWorkspaceNearestMarkerWins(t *testing.T) {
	outer := t.TempDir()
	outer, err := filepath.EvalSymlinks(outer)
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(outer, ".git"))
	inner := filepath.Join(outer, "vendor", "other")
	mkdir(t, filepath.Join(inner, ".git"))
	write(t, filepath.Join(inner, "lib.go"), "package other\n")

	got, err := Workspace(filepath.Join(inner, "lib.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got != inner {
		t.Errorf("Workspace = %q, want the nested repository %q", got, inner)
	}
}

// TestWorkspaceResolvesSymlinks: two spellings of one directory must
// hash to one socket, or a symlinked checkout would run two daemons
// over the same files.
func TestWorkspaceResolvesSymlinks(t *testing.T) {
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "real")
	mkdir(t, filepath.Join(real, ".git"))
	write(t, filepath.Join(real, "main.go"), "package main\n")
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	viaReal, err := Workspace(filepath.Join(real, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	viaLink, err := Workspace(filepath.Join(link, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if viaReal != viaLink {
		t.Errorf("symlinked spellings resolved differently: %q vs %q", viaReal, viaLink)
	}
	if WorkspaceHash(viaReal) != WorkspaceHash(viaLink) {
		t.Error("symlinked spellings hash to different sockets")
	}
}

func TestWorkspaceRejectsEmptyPath(t *testing.T) {
	if _, err := Workspace(""); err == nil {
		t.Fatal("Workspace(\"\") succeeded")
	}
}

// TestWorkspaceHash: short, stable, and different for different roots.
// Short matters — a unix socket path has about a hundred bytes to play
// with.
func TestWorkspaceHash(t *testing.T) {
	const root = "/home/user/src/repo"
	first := WorkspaceHash(root)
	if len(first) != 16 {
		t.Errorf("hash %q is %d characters, want 16", first, len(first))
	}
	if first != WorkspaceHash(root) {
		t.Error("the hash is not stable for one root")
	}
	if first == WorkspaceHash(root+"2") {
		t.Error("two roots hash the same")
	}
	if strings.Trim(first, "0123456789abcdef") != "" {
		t.Errorf("hash %q is not hex, so it is not safe in a filename", first)
	}
}

// TestSocketPathHonoursXDGRuntimeDir is the address of PLAN §3, spelled
// out: $XDG_RUNTIME_DIR/lightspeed/<workspace-hash>.sock.
func TestSocketPathHonoursXDGRuntimeDir(t *testing.T) {
	dir := socketDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)

	const root = "/home/user/src/repo"
	got, err := SocketPath(root)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, RuntimeDirName, WorkspaceHash(root)+".sock")
	if got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}

	info, err := os.Stat(filepath.Join(dir, RuntimeDirName))
	if err != nil {
		t.Fatalf("the runtime directory was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the runtime directory is mode %o, want 700: it holds sockets into this user's workspaces", perm)
	}
}

// TestSocketPathWithoutXDGRuntimeDir: containers, cron and macOS all
// manage to have no XDG_RUNTIME_DIR, and the daemon still has to work.
func TestSocketPathWithoutXDGRuntimeDir(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")

	got, err := SocketPath("/home/user/src/repo")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(got)
	t.Cleanup(func() { _ = os.Remove(dir) })
	if !strings.HasPrefix(dir, filepath.Clean(os.TempDir())) {
		t.Errorf("fallback socket directory %q is not under the temp directory", dir)
	}
	// Per user, so two users on one machine cannot collide in /tmp.
	if !strings.Contains(filepath.Base(dir), RuntimeDirName) {
		t.Errorf("fallback socket directory %q does not name lightspeed", dir)
	}
}

// TestSocketPathIsShortEnoughForAUnixSocket: the reason the name is a
// hash and not the path. A sun_path is about 108 bytes, and exceeding
// it fails at bind time with an error nobody can read.
func TestSocketPathIsShortEnoughForAUnixSocket(t *testing.T) {
	dir := socketDir(t)
	t.Setenv("XDG_RUNTIME_DIR", dir)

	deep := "/" + strings.Repeat("some-very-long-directory-name/", 12) + "project"
	got, err := SocketPath(deep)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > 100 {
		t.Errorf("socket path is %d bytes for a %d-byte workspace root: %q", len(got), len(deep), got)
	}
}

// TestOptionsAddressing checks the resolution [Open] does before it
// dials anything: the override wins, then the path, then the working
// directory.
func TestOptionsAddressing(t *testing.T) {
	dir := socketDir(t)
	repo := t.TempDir()
	repo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Join(repo, ".git"))
	write(t, filepath.Join(repo, "main.go"), "package main\n")

	fromPath, err := Options{Path: filepath.Join(repo, "main.go"), RuntimeDir: dir}.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := SocketPathIn(dir, repo); fromPath != want {
		t.Errorf("socket from path = %q, want %q", fromPath, want)
	}

	fromWorkspace, err := Options{Workspace: repo, RuntimeDir: dir}.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if fromWorkspace != fromPath {
		t.Errorf("the workspace override addresses %q, the path %q", fromWorkspace, fromPath)
	}

	explicit, err := Options{Socket: "/tmp/explicit.sock", Workspace: repo}.SocketPath()
	if err != nil {
		t.Fatal(err)
	}
	if explicit != "/tmp/explicit.sock" {
		t.Errorf("the socket override was ignored: %q", explicit)
	}

	// No path at all: the working directory decides, which is what a
	// bare `lightspeed daemon status` does.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := Options{RuntimeDir: dir}.WorkspaceRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := Workspace(cwd); root != want {
		t.Errorf("workspace from the working directory = %q, want %q", root, want)
	}
}

func TestOwnedByUs(t *testing.T) {
	dir := socketDir(t)
	ours, err := ownedByUs(dir)
	if err != nil {
		t.Fatalf("ownedByUs: %v", err)
	}
	if !ours {
		t.Error("a directory this test just created is not ours")
	}
	// A path that does not exist is nobody's: there is nothing to
	// hijack, and the caller is about to create it.
	ours, err = ownedByUs(filepath.Join(dir, "absent"))
	if err != nil || !ours {
		t.Errorf("ownedByUs on a missing path = %v, %v; want true, nil", ours, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
