package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// paths lists a tree's files.
func treePaths(d treeData) []string {
	out := make([]string, len(d.Files))
	for i, f := range d.Files {
		out[i] = f.Path
	}
	return out
}

// repoWithFiles makes a git repository of the files, and works in it.
func repoWithFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := tree(t, files)
	initRepo(t, dir)
	t.Chdir(dir)
	return dir
}

// TestTreeIsGitignoreAware: inside a repository the listing is git's — tracked
// files and untracked ones that are not ignored — so ignored files, a deleted
// tracked file and .git itself are not in it.
func TestTreeIsGitignoreAware(t *testing.T) {
	dir := repoWithFiles(t, map[string]string{
		".gitignore":   "build/\n*.log\n",
		"a.go":         "package a\n",
		"pkg/b.py":     "x = 1\n",
		"README.md":    "# hi\n",
		"build/out.go": "package out\n",
		"debug.log":    "x\n",
		"gone.go":      "package gone\n",
	})
	write(t, filepath.Join(dir, "new.go"), "package n\n")            // untracked, not ignored
	write(t, filepath.Join(dir, "later.log"), "x")                   // untracked and ignored
	write(t, filepath.Join(dir, "build/more.go"), "x")               // untracked and ignored
	if err := os.Remove(filepath.Join(dir, "gone.go")); err != nil { // tracked, deleted
		t.Fatal(err)
	}

	code, stdout, stderr := runMain("tree")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data treeData
	okData(t, stdout, &data)
	want := []string{".gitignore", "README.md", "a.go", "go.mod", "new.go", "pkg/b.py"}
	if got := treePaths(data); !slices.Equal(got, want) {
		t.Errorf("files = %q\nwant    %q", got, want)
	}
	if data.Source != "git" || data.Total != 6 || data.Count != 6 || data.Truncated {
		t.Errorf("source %q, count %d, total %d, truncated %v", data.Source, data.Count, data.Total, data.Truncated)
	}
	byPath := map[string]treeFile{}
	for _, f := range data.Files {
		byPath[f.Path] = f
	}
	if f := byPath["a.go"]; f.Language != "go" || f.Server != "gopls" {
		t.Errorf("a.go = %+v, want go handled by gopls", f)
	}
	if f := byPath["pkg/b.py"]; f.Language != "python" || f.Server != "pyright" {
		t.Errorf("pkg/b.py = %+v, want python handled by pyright", f)
	}
	if f := byPath["README.md"]; f.Server != "" {
		t.Errorf("README.md = %+v, want no server", f)
	}

	// A subdirectory keeps paths relative to the workspace, and says which
	// directory it is.
	code, stdout, _ = runMain("tree", "pkg")
	data = treeData{}
	okData(t, stdout, &data)
	if got := treePaths(data); code != ExitOK || !slices.Equal(got, []string{"pkg/b.py"}) || data.Dir != "pkg" {
		t.Errorf("tree pkg: exit %d, files %q, dir %q", code, got, data.Dir)
	}

	code, stdout, _ = runMain("tree", "--format", "text")
	if code != ExitOK || !strings.Contains(stdout, "a.go  (go, gopls)\n") || strings.Contains(stdout, "debug.log") {
		t.Errorf("text tree: exit %d\n%s", code, stdout)
	}
}

// TestTreeWithoutGitWalks: outside a repository the directory is walked,
// hidden and build directories are skipped, and the answer says that .gitignore
// was not applied.
func TestTreeWithoutGitWalks(t *testing.T) {
	dir := tree(t, map[string]string{
		"src/a.go":                "package a\n",
		"node_modules/x/index.js": "x\n",
		".hidden/z.go":            "package z\n",
		"vendor/v.go":             "package v\n",
	})
	if err := os.Mkdir(filepath.Join(dir, ".lightspeed-root"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	code, stdout, stderr := runMain("tree")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data treeData
	env := okData(t, stdout, &data)
	if got, want := treePaths(data), []string{"go.mod", "src/a.go"}; !slices.Equal(got, want) {
		t.Errorf("files = %q, want %q", got, want)
	}
	if data.Source != "walk" || !strings.Contains(strings.Join(env.Warnings, "\n"), ".gitignore was not applied") {
		t.Errorf("source %q, warnings %v", data.Source, env.Warnings)
	}
}

// TestTreePrefixAndCap: --prefix filters on the workspace-relative path, and
// --max-files cuts with truncated:true and a warning.
func TestTreePrefixAndCap(t *testing.T) {
	repoWithFiles(t, map[string]string{
		"pkg/a.go": "package a\n", "pkg/b.go": "package b\n", "pkg/c.go": "package c\n",
		"pkg/d.go": "package d\n", "pkg/e.go": "package e\n", "cmd/main.go": "package main\n",
	})

	code, stdout, _ := runMain("tree", "--prefix", "pkg/", "--max-files", "2")
	var data treeData
	env := okData(t, stdout, &data)
	if got := treePaths(data); code != ExitOK || !slices.Equal(got, []string{"pkg/a.go", "pkg/b.go"}) {
		t.Errorf("files = %q", got)
	}
	if data.Count != 2 || data.Total != 5 || !data.Truncated || data.Prefix != "pkg/" {
		t.Errorf("count %d, total %d, truncated %v, prefix %q; want 2, 5, true, pkg/", data.Count, data.Total, data.Truncated, data.Prefix)
	}
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "2 of 5 files") {
		t.Errorf("the cut is not in the warnings: %v", env.Warnings)
	}
	if code, _, _ := runMain("tree", "--max-files", "0"); code != ExitUsage {
		t.Errorf("--max-files 0: exit %d, want %d", code, ExitUsage)
	}
	if code, stdout, _ := runMain("tree", "nope"); code != ExitUsage || !strings.Contains(stdout, "no_such_file") {
		t.Errorf("tree of a missing directory: exit %d\n%s", code, stdout)
	}
	if code, _, _ := runMain("tree", "cmd/main.go"); code != ExitUsage {
		t.Errorf("tree of a file: exit %d, want %d", code, ExitUsage)
	}
}

// TestRepoOutline: directories with recursive file counts, the language
// breakdown, and the servers the languages resolve to with whether each can be
// run — here gopls is on PATH and pyright is not.
func TestRepoOutline(t *testing.T) {
	repoWithFiles(t, map[string]string{
		"a.go": "package a\n", "pkg/x.go": "package x\n", "pkg/y.go": "package y\n",
		"pkg/sub/z.go": "package z\n", "web/app.py": "x = 1\n", "docs/README.md": "# hi\n",
	})
	// A PATH of our own: a gopls, and git so the listing is git's. pyright is
	// missing, whatever the developer's machine has.
	bin := t.TempDir()
	shellScript(t, bin, "gopls", "exit 0\n")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	if err := os.Symlink(git, filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)

	code, stdout, stderr := runMain("repo_outline")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data repoOutlineData
	okData(t, stdout, &data)
	if data.Files != 7 || data.Source != "git" || data.Truncated {
		t.Errorf("files %d, source %q, truncated %v; want 7 files from git", data.Files, data.Source, data.Truncated)
	}
	dirs := map[string]repoDir{}
	for _, d := range data.Directories {
		dirs[d.Path] = d
	}
	for path, files := range map[string]int{".": 7, "docs": 1, "pkg": 3, "pkg/sub": 1, "web": 1} {
		if got := dirs[path].Files; got != files {
			t.Errorf("directory %q has %d files, want %d (counts are recursive)", path, got, files)
		}
	}
	if dirs["pkg"].Languages["go"] != 3 {
		t.Errorf("pkg languages = %v, want 3 go files", dirs["pkg"].Languages)
	}
	langs := map[string]repoLanguage{}
	for _, l := range data.Languages {
		langs[l.Language] = l
	}
	if l := langs["go"]; l.Files != 4 || l.Server != "gopls" {
		t.Errorf("go = %+v, want 4 files handled by gopls", l)
	}
	if l := langs["python"]; l.Files != 1 || l.Server != "pyright" {
		t.Errorf("python = %+v, want 1 file handled by pyright", l)
	}
	servers := map[string]repoServer{}
	for _, s := range data.Servers {
		servers[s.Name] = s
	}
	if s := servers["gopls"]; !s.Installed || s.Path != filepath.Join(bin, "gopls") || s.Install != "" {
		t.Errorf("gopls = %+v, want installed at %s", s, filepath.Join(bin, "gopls"))
	}
	if s := servers["pyright"]; s.Installed || !strings.HasPrefix(s.Install, "mise use -g ") {
		t.Errorf("pyright = %+v, want missing, with the mise command that installs it", s)
	}

	// --depth 1 lists only the top level; --max-dirs cuts and says so.
	_, stdout, _ = runMain("repo_outline", "--depth", "1")
	data = repoOutlineData{}
	okData(t, stdout, &data)
	for _, d := range data.Directories {
		if strings.Count(d.Path, "/") > 0 {
			t.Errorf("--depth 1 listed %q", d.Path)
		}
	}
	_, stdout, _ = runMain("repo_outline", "--max-dirs", "2")
	data = repoOutlineData{}
	env := okData(t, stdout, &data)
	if len(data.Directories) != 2 || !data.Truncated || data.TotalDirectories != 5 ||
		!strings.Contains(strings.Join(env.Warnings, "\n"), "2 of 5 directories") {
		t.Errorf("--max-dirs 2: %d listed of %d, truncated %v, warnings %v", len(data.Directories), data.TotalDirectories, data.Truncated, env.Warnings)
	}
	if code, stdout, _ := runMain("repo_outline", "--format", "text"); code != ExitOK ||
		!strings.Contains(stdout, "server pyright") || !strings.Contains(stdout, "missing") {
		t.Errorf("text repo_outline: exit %d\n%s", code, stdout)
	}
}

// TestTreeAndRepoOutlineNeedNoServer: neither starts a language server or a
// daemon, and neither needs one to be installed.
func TestTreeAndRepoOutlineNeedNoServer(t *testing.T) {
	repoWithFiles(t, map[string]string{"a.go": "package a\n"})
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)
	t.Setenv("PATH", t.TempDir()) // no gopls, no git: the walk, and nothing to run

	for _, args := range [][]string{{"tree"}, {"repo_outline"}, {"file", "a.go"}} {
		if code, stdout, stderr := runMain(args...); code != ExitOK {
			t.Errorf("%v: exit %d; stderr: %s\nstdout: %s", args, code, stderr, stdout)
		}
	}
	if n := spawns(); n != 0 {
		t.Errorf("%d language server(s) were started", n)
	}
	if st := statusOf(t); st.Running {
		t.Errorf("a daemon was started: %+v", st)
	}
}

// outsideWorkspace makes a workspace with a directory beside it that holds a
// file the workspace must never list, and a symlink in the workspace that
// points at that directory. The working directory is the workspace.
func outsideWorkspace(t *testing.T) (ws, outside string) {
	t.Helper()
	ws = tree(t, map[string]string{"a.go": "package a\n", "pkg/b.go": "package b\n"})
	outside = t.TempDir()
	write(t, filepath.Join(outside, "secret.txt"), "do not list\n")
	if err := os.Symlink(outside, filepath.Join(ws, "link")); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	return ws, outside
}

// outsideDirs are the directory arguments that leave the workspace, by every
// route: `..`, an absolute path, a symlink inside it, and paths that do not
// exist out there — refused as outside, not reported as missing, since which
// directories exist beyond the workspace is not for a confined command to say.
func outsideDirs(t *testing.T, ws, outside string) map[string]string {
	t.Helper()
	rel, err := filepath.Rel(ws, outside)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{
		"the parent directory":     "..",
		"a relative path out":      rel,
		"an absolute path":         outside,
		"an absolute system path":  string(filepath.Separator),
		"a symlink out":            "link",
		"a path through a symlink": "link/sub",
		"a missing path outside":   filepath.Join(outside, "nope"),
		"a missing parent path":    "../../nowhere/at/all",
	}
}

// TestTreeAndRepoOutlineAreConfinedToTheWorkspace: a directory that leaves the
// workspace — by `..`, by an absolute path or by a symlink inside it — is
// refused as outside_workspace before anything is listed, as `file` refuses a
// path (docs/DECISIONS.md D23). The workspace is the working directory's or
// --path's; it is never taken from the directory being asked about.
func TestTreeAndRepoOutlineAreConfinedToTheWorkspace(t *testing.T) {
	ws, outside := outsideWorkspace(t)
	for _, cmd := range []string{"tree", "repo_outline"} {
		for name, dir := range outsideDirs(t, ws, outside) {
			code, stdout, _ := runMain(cmd, dir)
			got, _ := errorData(t, stdout)
			if code != ExitUsage || got != "outside_workspace" || strings.Contains(stdout, "secret.txt") {
				t.Errorf("%s %s: exit %d, code %q\n%s", cmd, name, code, got, stdout)
			}
		}
		// The workspace itself and a directory of it, by relative and by
		// absolute path, are fine; a symlink is followed and judged where it
		// leads, so a link to a directory inside is fine too.
		if err := os.Symlink(filepath.Join(ws, "pkg"), filepath.Join(ws, "inlink")); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{".", "pkg", filepath.Join(ws, "pkg"), "inlink"} {
			if code, stdout, stderr := runMain(cmd, dir); code != ExitOK {
				t.Errorf("%s %s: exit %d; stderr: %s\n%s", cmd, dir, code, stderr, stdout)
			}
		}
		os.Remove(filepath.Join(ws, "inlink"))
	}

	// --path names the workspace, exactly as for `file`: from a working
	// directory that is elsewhere, a directory of the workspace is fine and
	// one outside it is not — whatever the working directory is.
	t.Chdir(outside)
	for _, cmd := range []string{"tree", "repo_outline"} {
		if code, stdout, stderr := runMain(cmd, "--path", ws, filepath.Join(ws, "pkg")); code != ExitOK {
			t.Errorf("%s --path: exit %d; stderr: %s\n%s", cmd, code, stderr, stdout)
		}
		code, stdout, _ := runMain(cmd, "--path", ws, outside)
		if got, _ := errorData(t, stdout); code != ExitUsage || got != "outside_workspace" {
			t.Errorf("%s --path %s: exit %d, code %q\n%s", cmd, ws, code, got, stdout)
		}
	}
}

// TestMCPTreeAndRepoOutlineAreConfinedToTheWorkspace: the same over MCP, where
// the workspace is the call's `workspace` and `dir` is what an agent controls.
func TestMCPTreeAndRepoOutlineAreConfinedToTheWorkspace(t *testing.T) {
	ws, outside := outsideWorkspace(t)
	// The server's own working directory is elsewhere, so that the workspace
	// can only come from the call.
	cs := mcpSession(t, t.TempDir())
	for _, tool := range []string{"tree", "repo_outline"} {
		for name, dir := range outsideDirs(t, ws, outside) {
			res := callTool(t, cs, tool, map[string]any{"workspace": ws, "dir": dir})
			text := resultText(t, res)
			env := mcpEnvelope(t, text)
			if !res.IsError || env.OK || env.Error == nil || env.Error.Code != "outside_workspace" ||
				resultExit(t, res) != ExitUsage || strings.Contains(text, "secret.txt") {
				t.Errorf("%s %s: isError=%v exit %d\n%s", tool, name, res.IsError, resultExit(t, res), text)
			}
		}
		// Without a dir, the workspace; with one inside, that directory.
		for _, args := range []map[string]any{
			{"workspace": ws},
			{"workspace": ws, "dir": "pkg"},
			{"workspace": ws, "dir": filepath.Join(ws, "pkg")},
		} {
			if res := callTool(t, cs, tool, args); res.IsError {
				t.Errorf("%s %v: %s", tool, args, resultText(t, res))
			}
		}
	}
}
