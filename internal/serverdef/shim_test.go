package serverdef

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A shimMise is a fake mise that knows what the real one printed on the
// machine this was written for: `mise which gopls` fails with "not
// currently active" while `mise which --tool <tool>@<version> gopls`
// answers, and `mise ls --json --installed <tool>` lists what is on disk.
type shimMise struct {
	mu sync.Mutex
	// active is what `mise which <name>` answers, per binary name. A name
	// that is absent is a tool with no active version.
	active map[string]string
	// installed lists installed versions per tool.
	installed map[string][]string
	// real is the path `mise which --tool tool@version name` answers.
	real map[string]string
	// calls records every argv, and dirs the -C directory of each.
	calls [][]string
	dirs  []string
}

func (m *shimMise) run(_ context.Context, env []string, _ string, args ...string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, args)
	dir := ""
	if len(args) >= 2 && args[0] == "-C" {
		dir, args = args[1], args[2:]
	}
	m.dirs = append(m.dirs, dir)
	switch {
	case len(args) == 1 && args[0] == "--version":
		return "2026.9.7 linux-x64\n", "", nil
	case len(args) == 2 && args[0] == "which":
		if p, ok := m.active[args[1]]; ok {
			return p + "\n", "", nil
		}
		return "", "mise ERROR " + args[1] + " is a mise bin however it is not currently active. Use `mise use` to activate it in this directory.\n", &exitStatus{code: 1}
	case len(args) == 4 && args[0] == "which" && args[1] == "--tool":
		if p, ok := m.real[args[2]+" "+args[3]]; ok {
			return p + "\n", "", nil
		}
		return "", "mise ERROR not installed\n", &exitStatus{code: 1}
	case len(args) == 4 && args[0] == "ls":
		type entry struct {
			Version   string `json:"version"`
			Installed bool   `json:"installed"`
			Active    bool   `json:"active"`
		}
		out := []entry{}
		for _, v := range m.installed[args[3]] {
			out = append(out, entry{Version: v, Installed: true})
		}
		b, _ := json.Marshal(out)
		return string(b) + "\n", "", nil
	}
	return "", "unexpected: " + strings.Join(args, " ") + "\n", &exitStatus{code: 2}
}

// shimEnv is a machine whose PATH has a mise shim for gopls: a file in a
// `.../mise/shims` directory that does what the real one does with no
// active version — prints mise's error and exits 1 — plus a mise on PATH.
func shimEnv(t *testing.T) (*testEnv, string) {
	t.Helper()
	e := newTestEnv(t)
	shims := filepath.Join(filepath.Dir(e.BinDir), "mise", "shims")
	if err := os.MkdirAll(shims, 0o755); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(shims, "gopls")
	script := "#!/bin/sh\necho 'mise ERROR No version is set for shim: gopls' >&2\nexit 1\n"
	if err := os.WriteFile(shim, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Binary("mise")
	e.Vars["PATH"] = shims + string(os.PathListSeparator) + e.BinDir
	return e, shim
}

// realGopls is a stand-in for the version mise has installed but not
// activated.
func realGopls(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "installs", "go-golang-org-x-tools-gopls", "0.23.0", "bin", "gopls")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

const goplsTool = "go:golang.org/x/tools/gopls"

func TestIsMiseShim(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "somewhere", "shims", "gopls")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/usr/bin/mise", link); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(dir, "elsewhere", "shims", "gopls")
	for _, tc := range []struct {
		name string
		path string
		data string
		want bool
	}{
		{"under a mise directory", "/home/u/.local/share/mise/shims/gopls", "", true},
		{"a link to mise", link, "", true},
		{"MISE_DATA_DIR", "/data/shims/gopls", "/data", true},
		{"another manager's shims", "/home/u/.asdf/shims/gopls", "", false},
		{"not a shims directory", "/home/u/.local/share/mise/installs/gopls", "", false},
		{"shims with no mise", plain, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Getenv: func(k string) string {
				if k == "MISE_DATA_DIR" {
					return tc.data
				}
				return ""
			}}
			if got := isMiseShim(tc.path, opts); got != tc.want {
				t.Errorf("isMiseShim(%s) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestShimThatResolvesStaysInstalled: a shim mise can resolve in the
// workspace is the server it always was, and mise was asked in that
// workspace, because a shim's answer depends on the directory.
func TestShimThatResolvesStaysInstalled(t *testing.T) {
	e, shim := shimEnv(t)
	m := &shimMise{active: map[string]string{"gopls": realGopls(t)}}
	e.Runner = m.run

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != shim || got.Source != BinaryPATH || got.Note != "" {
		t.Fatalf("binary = %+v, want the shim itself, runnable, no note", got)
	}
	asked := false
	for i, dir := range m.dirs {
		if dir != "" {
			asked = true
			if dir != e.Workspace || strings.Join(m.calls[i][2:], " ") != "which gopls" {
				t.Errorf("mise ran %v, want `which gopls` in the workspace %q", m.calls[i], e.Workspace)
			}
		}
	}
	if !asked {
		t.Errorf("mise was never asked in the workspace: %v", m.calls)
	}
}

// TestShimWithNoActiveVersionLaunchesTheInstalledOne is the reported
// failure: gopls 0.23.0 is installed in mise, no version is active for
// the directory, and the shim on PATH is useless. The installed binary is
// launched and provenance says why. The definition pins `v0.23.0` while
// mise lists `0.23.0`, which must still match.
func TestShimWithNoActiveVersionLaunchesTheInstalledOne(t *testing.T) {
	e, shim := shimEnv(t)
	real := realGopls(t)
	m := &shimMise{
		installed: map[string][]string{goplsTool: {"0.22.1", "0.23.0"}},
		real:      map[string]string{goplsTool + "@0.23.0 gopls": real},
	}
	e.Runner = m.run

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != real || got.Source != BinaryMise {
		t.Fatalf("binary = %+v, want mise's %s launched", got, real)
	}
	for _, want := range []string{shim, "no active version", goplsTool + "@0.23.0", "mise use -g " + goplsTool + "@0.23.0"} {
		if !strings.Contains(got.Note, want) {
			t.Errorf("note = %q, want it to mention %q", got.Note, want)
		}
	}
	if strings.Join(got.Fix, " ") != "mise use -g "+goplsTool+"@0.23.0" {
		t.Errorf("fix = %v", got.Fix)
	}
}

// TestShimPicksTheHighestInstalledWhenThePinIsNotThere: the pin is the
// preference, not a requirement, since the alternative is no server.
func TestShimPicksTheHighestInstalledWhenThePinIsNotThere(t *testing.T) {
	e, _ := shimEnv(t)
	real := realGopls(t)
	m := &shimMise{
		installed: map[string][]string{goplsTool: {"0.9.0", "0.10.2", "0.10.0"}},
		real:      map[string]string{goplsTool + "@0.10.2 gopls": real},
	}
	e.Runner = m.run

	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != real || !strings.Contains(got.Note, "@0.10.2") {
		t.Fatalf("binary = %+v, want 0.10.2 (numeric order, not string order)", got)
	}
}

// TestShimWithNothingInstalledIsNotUsable: no active version and nothing
// installed is a server that cannot be used, with the command to run, and
// never an "installed" one.
func TestShimWithNothingInstalledIsNotUsable(t *testing.T) {
	e, shim := shimEnv(t)
	e.Runner = (&shimMise{}).run

	got := probe(t, e, "gopls")
	if got.Runnable || got.Source != BinaryUnusable || got.Path != shim {
		t.Fatalf("binary = %+v, want the shim, unusable", got)
	}
	if !strings.Contains(got.Problem, "mise shim") || !strings.Contains(got.Problem, "no version of gopls is active") {
		t.Errorf("problem = %q", got.Problem)
	}
	if strings.Join(got.Fix, " ") != "mise use -g "+goplsTool+"@v0.23.0" {
		t.Errorf("fix = %v, want the definition's install command", got.Fix)
	}
}

func TestShimOfADefinitionWithoutAnInstallSpec(t *testing.T) {
	e, shim := shimEnv(t)
	e.Runner = (&shimMise{}).run
	def := &ServerDef{Name: "x", Server: Server{Command: []string{"gopls"}}}
	base := Binary{Name: "gopls", Path: shim, Source: BinaryPATH, Runnable: true, Probed: true}

	got := verifyShim(t.Context(), def, base, e.Options(), MiseStatus{Available: true, Path: "mise"})
	if got.Runnable || got.Fix != nil {
		t.Errorf("binary = %+v, want unusable with no invented fix", got)
	}
}

// TestShimWithoutMiseIsLeftToTheStartProbe: with nobody to ask, the hot
// path keeps the shim and `servers`/`doctor` run it to find out.
func TestShimWithoutMiseIsLeftToTheStartProbe(t *testing.T) {
	e, shim := shimEnv(t)
	e.Vars["PATH"] = filepath.Dir(shim) // no mise on it
	got := probe(t, e, "gopls")
	if !got.Runnable || got.Path != shim {
		t.Fatalf("binary = %+v", got)
	}
	e.Starter = func(context.Context, string, []string, string, ...string) (string, int, error) {
		return "mise ERROR No version is set for shim: gopls\n", 1, nil
	}
	rep, err := Servers(t.Context(), e.Options())
	if err != nil {
		t.Fatal(err)
	}
	s := serverNamed(t, rep, "gopls")
	if s.Installed || s.Binary.StartProbe != "failed" || !strings.Contains(s.Binary.Problem, "No version is set for shim") {
		t.Errorf("gopls = %+v, want failed by the start probe", s.Binary)
	}
}

func serverNamed(t *testing.T, rep *ServersReport, name string) ServerStatus {
	t.Helper()
	for _, s := range rep.Servers {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no server %q in the report", name)
	return ServerStatus{}
}

func doctorCheck(t *testing.T, rep *DoctorReport, id, subject string) Check {
	t.Helper()
	for _, c := range rep.Checks {
		if c.ID == id && c.Subject == subject {
			return c
		}
	}
	t.Fatalf("no %s check for %q in %v", id, subject, rep.Checks)
	return Check{}
}

// TestDoctorAndServersOnABrokenShim: what was reported as `installed`,
// severity ok. It is now an error with the command to run when nothing is
// installed, and a warning (it works, a shell would not) when mise has a
// version to launch.
func TestDoctorAndServersOnABrokenShim(t *testing.T) {
	t.Run("nothing installed", func(t *testing.T) {
		e, _ := shimEnv(t)
		e.Runner = (&shimMise{}).run
		rep, err := Doctor(t.Context(), nil, e.Options())
		if err != nil {
			t.Fatal(err)
		}
		c := doctorCheck(t, rep, CheckServerBinary, "gopls")
		if c.Severity != SeverityError || strings.Join(c.Fix, " ") != "mise use -g "+goplsTool+"@v0.23.0" {
			t.Errorf("check = %+v, want an error carrying the install command", c)
		}
		if rep.OK() {
			t.Error("doctor says OK with an unusable server")
		}
		servers, _ := Servers(t.Context(), e.Options())
		if s := serverNamed(t, servers, "gopls"); s.Installed {
			t.Errorf("servers lists gopls as installed: %+v", s.Binary)
		}
	})
	t.Run("an installed version is launched", func(t *testing.T) {
		e, _ := shimEnv(t)
		real := realGopls(t)
		e.Runner = (&shimMise{
			installed: map[string][]string{goplsTool: {"0.23.0"}},
			real:      map[string]string{goplsTool + "@0.23.0 gopls": real},
		}).run
		rep, err := Doctor(t.Context(), nil, e.Options())
		if err != nil {
			t.Fatal(err)
		}
		c := doctorCheck(t, rep, CheckServerBinary, "gopls")
		if c.Severity != SeverityWarn || !strings.Contains(c.Message, real) || !strings.Contains(c.Detail, "no active version") ||
			strings.Join(c.Fix, " ") != "mise use -g "+goplsTool+"@0.23.0" {
			t.Errorf("check = %+v, want a warning that names the launched binary and the fix", c)
		}
		servers, _ := Servers(t.Context(), e.Options())
		s := serverNamed(t, servers, "gopls")
		if !s.Installed || s.Binary.Path != real || s.Binary.Note == "" {
			t.Errorf("servers: %+v", s.Binary)
		}
	})
}

// TestStartProbeDecidesOnHowTheProcessEnded is the generic rule: a file
// that exists is not evidence the server starts. What counts as failing is
// narrow, because `--version` is not universal.
func TestStartProbeDecidesOnHowTheProcessEnded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		out     string
		exit    int
		err     error
		want    string
		problem string
	}{
		{name: "exits 0", want: "ok"},
		{name: "gopls: flag error, exit 2", out: "flag provided but not defined: -version\n", exit: 2, want: "started"},
		{name: "cannot exec", err: os.ErrPermission, want: "failed", problem: "permission denied"},
		{name: "shell cannot find it", exit: 127, want: "failed", problem: "exit status 127"},
		{name: "missing library", out: "x: error while loading shared libraries: libfoo.so\n", exit: 127, want: "failed", problem: "shared libraries"},
		{name: "mise error", out: "mise ERROR No version is set for shim: gopls\n", exit: 1, want: "failed", problem: "No version is set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.Binary("gopls")
			e.Runner = fakeMise("2026.8.14", nil)
			var gotDir string
			var gotArgs []string
			e.Starter = func(_ context.Context, dir string, env []string, path string, args ...string) (string, int, error) {
				gotDir, gotArgs = dir, args
				if !strings.Contains(strings.Join(env, " "), "MISE_NOT_FOUND_AUTO_INSTALL=0") {
					t.Errorf("start probe env %v may let a shim install things", env)
				}
				return tc.out, tc.exit, tc.err
			}
			rep, err := Servers(t.Context(), e.Options())
			if err != nil {
				t.Fatal(err)
			}
			s := serverNamed(t, rep, "gopls")
			if s.Binary.StartProbe != tc.want || s.Installed != (tc.want != "failed") {
				t.Errorf("binary = %+v, want start probe %q", s.Binary, tc.want)
			}
			if tc.problem != "" && !strings.Contains(s.Binary.Problem, tc.problem) {
				t.Errorf("problem = %q, want %q", s.Binary.Problem, tc.problem)
			}
			if gotDir != e.Workspace || strings.Join(gotArgs, " ") != "--version" {
				t.Errorf("probe ran %v in %q, want --version in the workspace", gotArgs, gotDir)
			}
			doc, _ := Doctor(t.Context(), nil, e.Options())
			c := doctorCheck(t, doc, CheckServerBinary, "gopls")
			if (tc.want == "failed") != (c.Severity == SeverityError) {
				t.Errorf("doctor severity = %s for a %s probe", c.Severity, tc.want)
			}
		})
	}
}

// TestStartProbeOnlyRunsWhatCanRun: a binary that is not there is not
// started to be told so, and neither `Probe` nor `ProbeServer` — the path
// before every query — ever starts anything.
func TestStartProbeOnlyRunsWhatCanRun(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	e.Binary("gopls")
	started := 0
	e.Starter = func(context.Context, string, []string, string, ...string) (string, int, error) {
		started++
		return "", 0, nil
	}
	opts := e.Options()
	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	res.Probe(t.Context(), opts)
	if _, err := res.ProbeServer(t.Context(), "gopls", opts); err != nil {
		t.Fatal(err)
	}
	if started != 0 {
		t.Fatalf("Probe/ProbeServer started %d process(es); queries must not pay for it", started)
	}
	res.StartProbe(t.Context(), opts)
	if started != 1 {
		t.Errorf("StartProbe started %d process(es), want 1: only gopls is on PATH", started)
	}
}

// TestExecStartAgainstRealProcesses runs the production runner on scripts,
// so that the classification is checked against what a process really
// does: its exit status, its output, and a hang that must be cut short.
func TestExecStartAgainstRealProcesses(t *testing.T) {
	old := startProbeTimeout
	startProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { startProbeTimeout = old })

	script := func(body string) string {
		p := filepath.Join(t.TempDir(), "srv")
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dir := t.TempDir()
	for _, tc := range []struct {
		name, body, want string
		problem          string
	}{
		{"ok", "echo v1", "ok", ""},
		{"non-zero usage", "echo 'flag provided but not defined' >&2; exit 2", "started", ""},
		{"a shim with no version", "echo 'mise ERROR No version is set for shim: gopls' >&2; exit 1", "failed", "No version is set"},
		{"a hang is a server that started", "exec sleep 30", "started", ""},
		{"runs in the workspace", `[ "$(pwd -P)" = "` + dir + `" ] || exit 1`, "ok", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got := startProbe(t.Context(), Binary{Name: "srv", Path: script(tc.body), Runnable: true, Probed: true},
				Options{WorkspaceRoot: dir})
			if got.StartProbe != tc.want {
				t.Errorf("start probe = %q (%s), want %q", got.StartProbe, got.Problem, tc.want)
			}
			if tc.problem != "" && !strings.Contains(got.Problem, tc.problem) {
				t.Errorf("problem = %q, want %q", got.Problem, tc.problem)
			}
			if time.Since(start) > 5*time.Second {
				t.Errorf("took %v: the probe is not bounded", time.Since(start))
			}
		})
	}
	t.Run("not executable", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "srv")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := startProbe(t.Context(), Binary{Path: p, Runnable: true}, Options{}); got.StartProbe != "failed" || got.Runnable {
			t.Errorf("binary = %+v", got)
		}
	})
}
