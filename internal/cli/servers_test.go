package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// PLAN §8 M4, wired: the layers of PLAN §6 reach the command line. These
// tests are hermetic — a fake mise and fake servers on a PATH of their
// own, a configuration directory of their own, workspaces in t.TempDir —
// and each one names the layer or the flag it is about.

type originJSON struct {
	Layer string `json:"layer"`
	File  string `json:"file"`
}

type serverJSON struct {
	Name           string                `json:"name"`
	Origin         originJSON            `json:"origin"`
	Overrides      []json.RawMessage     `json:"overrides"`
	Shadowed       []originedJSON        `json:"shadowed"`
	KeyOrigins     map[string]originJSON `json:"key_origins"`
	Installed      bool                  `json:"installed"`
	InstallCommand []string              `json:"install_command"`
	Binary         struct {
		Name       string   `json:"name"`
		Path       string   `json:"path"`
		Source     string   `json:"source"`
		Problem    string   `json:"problem"`
		Note       string   `json:"note"`
		Fix        []string `json:"fix"`
		StartProbe string   `json:"start_probe"`
	} `json:"binary"`
	Definition serverdef.ServerDef `json:"definition"`
}

type originedJSON struct {
	Origin originJSON `json:"origin"`
	Whole  bool       `json:"whole"`
}

type serversJSON struct {
	Workspace string       `json:"workspace"`
	Installed int          `json:"installed"`
	Servers   []serverJSON `json:"servers"`
	Layers    []struct {
		Layer   string `json:"layer"`
		Exists  bool   `json:"exists"`
		Skipped string `json:"skipped"`
	} `json:"layers"`
	Offline  bool `json:"offline"`
	Problems []struct {
		Origin  originJSON `json:"origin"`
		Message string     `json:"message"`
	} `json:"problems"`
}

func (s serversJSON) server(t *testing.T, name string) serverJSON {
	t.Helper()
	for _, srv := range s.Servers {
		if srv.Name == name {
			return srv
		}
	}
	t.Fatalf("servers lists no %q: %+v", name, s.Servers)
	return serverJSON{}
}

// emptyPath is a PATH with nothing on it: no server, and no mise.
func emptyPath(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

func canonicalDir(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func runServers(t *testing.T, args ...string) (int, serversJSON) {
	t.Helper()
	code, stdout, stderr := runMain(append([]string{"servers"}, args...)...)
	var data serversJSON
	env := decodeData(t, stdout, &data)
	if !env.OK {
		t.Fatalf("servers: not ok (exit %d): %s\nstderr: %s", code, stdout, stderr)
	}
	return code, data
}

// TestServersShowsEachLayerWinning is PLAN §6's ordering, key by key:
// the workspace file beats the user's servers.d beats the generated
// defaults, and the output says which layer every setting came from.
func TestServersShowsEachLayerWinning(t *testing.T) {
	emptyPath(t)
	cfg := useConfigDir(t)
	ws := t.TempDir()
	root := canonicalDir(t, ws)
	builtin, ok := serverdef.Builtin("gopls")
	if !ok {
		t.Fatal("no built-in gopls")
	}

	t.Run("generated defaults only", func(t *testing.T) {
		code, data := runServers(t, "--path", ws)
		if code != ExitOK {
			t.Fatalf("exit %d, want %d", code, ExitOK)
		}
		if data.Workspace != root {
			t.Errorf("workspace = %q, want %q", data.Workspace, root)
		}
		if len(data.Servers) != len(serverdef.BuiltinNames()) {
			t.Errorf("%d servers, want the %d built-in ones", len(data.Servers), len(serverdef.BuiltinNames()))
		}
		gopls := data.server(t, "gopls")
		if gopls.Origin.Layer != "builtin" {
			t.Errorf("origin = %+v, want the builtin layer", gopls.Origin)
		}
		for key, origin := range gopls.KeyOrigins {
			if origin.Layer != "builtin" {
				t.Errorf("key %s comes from %+v, want builtin", key, origin)
			}
		}
		if gopls.Installed {
			t.Error("gopls is reported installed on an empty PATH")
		}
		if want := []string{"mise", "use", "-g", builtin.Install.Mise}; !slices.Equal(gopls.InstallCommand, want) {
			t.Errorf("install command = %v, want %v", gopls.InstallCommand, want)
		}
	})

	t.Run("the user layer beats the generated defaults", func(t *testing.T) {
		file := writeUserServer(t, cfg, "gopls.toml", "schema_version = 1\nname = \"gopls\"\n\n[activation]\npriority = 90\n")
		_, data := runServers(t, "--path", ws)
		gopls := data.server(t, "gopls")
		if gopls.Origin.Layer != "user" || gopls.Origin.File != file {
			t.Errorf("origin = %+v, want the user file %s", gopls.Origin, file)
		}
		if got := gopls.KeyOrigins[serverdef.KeyPriority]; got.Layer != "user" {
			t.Errorf("%s comes from %+v, want user", serverdef.KeyPriority, got)
		}
		if got := gopls.KeyOrigins[serverdef.KeyCommand]; got.Layer != "builtin" {
			t.Errorf("%s comes from %+v, want builtin: a partial override keeps the rest", serverdef.KeyCommand, got)
		}
		if gopls.Definition.Activation.Priority != 90 {
			t.Errorf("priority = %d, want 90", gopls.Definition.Activation.Priority)
		}
	})

	t.Run("the workspace layer beats both", func(t *testing.T) {
		file := filepath.Join(ws, serverdef.WorkspaceFile)
		write(t, file, "schema_version = 1\nname = \"gopls\"\n\n[activation]\npriority = 95\n\n[server]\ncommand = [\"gopls-in-tree\"]\n")
		_, data := runServers(t, "--path", ws)
		gopls := data.server(t, "gopls")
		if gopls.Origin.Layer != "workspace" || gopls.Origin.File != filepath.Join(root, serverdef.WorkspaceFile) {
			t.Errorf("origin = %+v, want the workspace file", gopls.Origin)
		}
		if got := gopls.KeyOrigins[serverdef.KeyPriority]; got.Layer != "workspace" {
			t.Errorf("%s comes from %+v, want workspace", serverdef.KeyPriority, got)
		}
		if got := gopls.KeyOrigins[serverdef.KeyCommand]; got.Layer != "workspace" {
			t.Errorf("%s comes from %+v, want workspace", serverdef.KeyCommand, got)
		}
		if got := gopls.KeyOrigins[serverdef.KeyLanguages]; got.Layer != "builtin" {
			t.Errorf("%s comes from %+v, want builtin", serverdef.KeyLanguages, got)
		}
		if gopls.Definition.Activation.Priority != 95 || !slices.Equal(gopls.Definition.Server.Command, []string{"gopls-in-tree"}) {
			t.Errorf("definition = %+v, want the workspace's priority and command", gopls.Definition)
		}
		if len(gopls.Overrides) != 3 || len(gopls.Shadowed) != 2 {
			t.Errorf("%d contributions, %d shadowed; want 3 and 2 (user and builtin were overridden)", len(gopls.Overrides), len(gopls.Shadowed))
		}
		if pyright := data.server(t, "pyright"); pyright.Origin.Layer != "builtin" {
			t.Errorf("pyright origin = %+v, want builtin: nothing overrode it", pyright.Origin)
		}
	})

	t.Run("text", func(t *testing.T) {
		code, stdout, stderr := runMain("servers", "--path", ws, "--format", "text")
		if code != ExitOK {
			t.Fatalf("exit %d; stderr: %s", code, stderr)
		}
		for _, want := range []string{"gopls", "missing", "workspace", serverdef.KeyLanguages, "mise use -g"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("text output lacks %q:\n%s", want, stdout)
			}
		}
	})
}

// TestServersFindsExecutablesOnPATH is layer 4 of PLAN §6: with nothing
// configured, a server on PATH is used, and `servers` says where it is.
func TestServersFindsExecutablesOnPATH(t *testing.T) {
	useConfigDir(t)
	bin := t.TempDir()
	gopls := shellScript(t, bin, "gopls", "exit 0\n")
	t.Setenv("PATH", bin)

	_, data := runServers(t, "--path", t.TempDir())
	got := data.server(t, "gopls")
	if !got.Installed || got.Binary.Path != gopls || got.Binary.Source != "path" {
		t.Errorf("gopls = %+v, want installed at %s, found on PATH", got.Binary, gopls)
	}
	if data.Installed != 1 {
		t.Errorf("%d servers installed, want 1", data.Installed)
	}
	if other := data.server(t, "pyright"); other.Installed {
		t.Errorf("pyright is reported installed, but only gopls is on PATH")
	}
}

// TestBrokenConfigIsReportedNotSkipped: a file that cannot be used is
// exit 2 for a command that needs the definitions, and is *listed* by the
// commands that exist to explain it. Nothing may quietly do nothing.
func TestBrokenConfigIsReportedNotSkipped(t *testing.T) {
	emptyPath(t)
	useConfigDir(t)
	dir, file := cjkFixture(t)
	broken := filepath.Join(dir, serverdef.WorkspaceFile)
	write(t, broken, "schema_version = 1\nname = \n")

	code, stdout, _ := runMain("definition", file+":3:5")
	if code != ExitUsage {
		t.Fatalf("definition: exit %d, want %d; stdout: %s", code, ExitUsage, stdout)
	}
	env := decodeData(t, stdout, nil)
	if env.Error == nil || env.Error.Code != "invalid_config" || !strings.Contains(env.Error.Message, serverdef.WorkspaceFile) {
		t.Errorf("error = %+v, want invalid_config naming %s", env.Error, serverdef.WorkspaceFile)
	}

	code, stdout, _ = runMain("servers", "--path", dir)
	if code != ExitProblems {
		t.Errorf("servers: exit %d, want %d", code, ExitProblems)
	}
	var data serversJSON
	if env := decodeData(t, stdout, &data); !env.OK || len(data.Problems) != 1 || data.Problems[0].Origin.File != canonicalDir(t, dir)+"/"+serverdef.WorkspaceFile {
		t.Errorf("servers did not list the broken file as a problem: %s", stdout)
	}
	if len(data.Servers) != len(serverdef.BuiltinNames()) {
		t.Errorf("servers listed %d servers, want the built-in ones all the same", len(data.Servers))
	}

	code, stdout, _ = runMain("doctor", dir)
	var report struct {
		Worst  string `json:"worst"`
		Checks []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"checks"`
	}
	decodeData(t, stdout, &report)
	if code != ExitProblems || report.Worst != "error" {
		t.Errorf("doctor: exit %d, worst %q, want %d and error", code, report.Worst, ExitProblems)
	}
	found := false
	for _, c := range report.Checks {
		found = found || (c.ID == serverdef.CheckConfigFile && c.Severity == "error")
	}
	if !found {
		t.Errorf("doctor has no config_file error for the broken file: %s", stdout)
	}
}

// TestEachLayerWinsForARealCommand goes one step past `servers`: it is the
// server that is *started* that has to come from the winning layer. Three
// executables, each recording its tag when launched, stand in for the same
// language server as PATH would supply it, as the user layer would
// override it, and as the workspace would.
func TestEachLayerWinsForARealCommand(t *testing.T) {
	setup := func(t *testing.T) (dir, file, log string, bin string) {
		t.Helper()
		useConfigDir(t)
		dir, file = cjkFixture(t)
		scenario{
			ownServers: true,
			results:    map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}},
		}.apply(t)
		log = filepath.Join(t.TempDir(), "started.log")
		bin = filepath.Join(t.TempDir(), "bin")
		t.Setenv("PATH", bin)
		return dir, file, log, bin
	}
	started := func(t *testing.T, log string) []string {
		t.Helper()
		data, _ := os.ReadFile(log)
		return strings.Fields(string(data))
	}
	define := func(t *testing.T, file string) {
		t.Helper()
		code, stdout, stderr := runMain("definition", file+":3:5")
		if code != ExitOK {
			t.Fatalf("definition: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
		}
	}

	t.Run("the generated definition, found by PATH sniffing", func(t *testing.T) {
		_, file, log, bin := setup(t)
		fakeServerScript(t, bin, "gopls", "path", log)
		define(t, file)
		if got := started(t, log); !slices.Equal(got, []string{"path"}) {
			t.Errorf("started %v, want the gopls found on PATH", got)
		}
	})

	t.Run("the user layer overrides the command", func(t *testing.T) {
		_, file, log, bin := setup(t)
		fakeServerScript(t, bin, "gopls", "path", log)
		user := fakeServerScript(t, t.TempDir(), "user-gopls", "user", log)
		writeUserServer(t, os.Getenv(serverdef.EnvConfigDir), "gopls.toml", commandOverride("gopls", user))
		define(t, file)
		if got := started(t, log); !slices.Equal(got, []string{"user"}) {
			t.Errorf("started %v, want the user layer's command", got)
		}
	})

	t.Run("the workspace layer beats the user layer", func(t *testing.T) {
		dir, file, log, bin := setup(t)
		fakeServerScript(t, bin, "gopls", "path", log)
		user := fakeServerScript(t, t.TempDir(), "user-gopls", "user", log)
		writeUserServer(t, os.Getenv(serverdef.EnvConfigDir), "gopls.toml", commandOverride("gopls", user))
		tree := fakeServerScript(t, t.TempDir(), "tree-gopls", "workspace", log)
		write(t, filepath.Join(dir, serverdef.WorkspaceFile), commandOverride("gopls", tree))
		define(t, file)
		if got := started(t, log); !slices.Equal(got, []string{"workspace"}) {
			t.Errorf("started %v, want the workspace file's command", got)
		}
	})
}

// TestWorkspaceFileDefinesANewServer: PLAN §6's "the only file we ask
// users to write". A server no other layer knows, for a file type nothing
// else claims, answers a real query.
func TestWorkspaceFileDefinesANewServer(t *testing.T) {
	useConfigDir(t)
	emptyPath(t)
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.wat")
	write(t, notes, "(module)\n")
	scenario{ownServers: true, results: map[string]any{methodDefinition: json.RawMessage(positionEcho)}}.apply(t)

	code, stdout, _ := runMain("definition", notes+":1:2")
	if code != ExitNoServer {
		t.Fatalf("before the config exists: exit %d, want %d; %s", code, ExitNoServer, stdout)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, serverdef.WorkspaceFile), `schema_version = 1
name = "wat-ls"

[activation]
globs = ["**/*.wat"]
root_markers = [".lightspeed.toml"]

[server]
command = ["`+exe+`"]
`)
	code, stdout, stderr := runMain("definition", notes+":1:2")
	if code != ExitOK {
		t.Fatalf("with the config: exit %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	if !strings.Contains(stdout, "notes.wat") {
		t.Errorf("no location in the answer: %s", stdout)
	}
}

// TestMissingServerExitsThreeWithTheInstallCommand: PLAN §6, the security
// posture. Nothing downloads; the envelope carries what to run.
func TestMissingServerExitsThreeWithTheInstallCommand(t *testing.T) {
	useConfigDir(t)
	emptyPath(t)
	_, file := cjkFixture(t)
	builtin, _ := serverdef.Builtin("gopls")
	want := "mise use -g " + builtin.Install.Mise

	code, stdout, _ := runMain("definition", file+":3:5")
	if code != ExitNoServer {
		t.Fatalf("exit %d, want %d; %s", code, ExitNoServer, stdout)
	}
	env := decodeData(t, stdout, nil)
	if env.Error == nil || env.Error.Code != "server_not_installed" {
		t.Fatalf("error = %+v, want server_not_installed", env.Error)
	}
	if !strings.Contains(env.Error.Message, want) || env.Error.Data["install"] != want || env.Error.Data["server"] != "gopls" {
		t.Errorf("error = %+v, want the exact command %q in message and data", env.Error, want)
	}
	if _, offline := env.Error.Data["offline"]; offline {
		t.Error("the envelope claims offline mode without --offline")
	}

	// --offline changes what the caller may do about it, and says so.
	code, stdout, _ = runMain("--offline", "definition", file+":3:5")
	env = decodeData(t, stdout, nil)
	if code != ExitNoServer || env.Error == nil || env.Error.Data["offline"] != true || !strings.Contains(env.Error.Message, "offline") {
		t.Errorf("--offline: exit %d, error %+v; want exit 3 naming offline mode", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, want) {
		t.Errorf("--offline hides the install command: %s", env.Error.Message)
	}
}

// TestMissingServerWithNoInstallSpec: a workspace-defined server with no
// install.mise has no command to quote, and says what to do instead
// rather than inventing a package name.
func TestMissingServerWithNoInstallSpec(t *testing.T) {
	useConfigDir(t)
	emptyPath(t)
	dir := t.TempDir()
	notes := filepath.Join(dir, "notes.wat")
	write(t, notes, "(module)\n")
	write(t, filepath.Join(dir, serverdef.WorkspaceFile), `schema_version = 1
name = "wat-ls"

[activation]
globs = ["**/*.wat"]
root_markers = [".lightspeed.toml"]

[server]
command = ["wat-language-server"]
`)
	code, stdout, _ := runMain("definition", notes+":1:2")
	env := decodeData(t, stdout, nil)
	if code != ExitNoServer || env.Error == nil || env.Error.Code != "server_not_installed" {
		t.Fatalf("exit %d, error %+v; want exit 3 server_not_installed", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "wat-language-server") || !strings.Contains(env.Error.Message, serverdef.WorkspaceFile) {
		t.Errorf("message %q should say to put the binary on PATH or set server.command", env.Error.Message)
	}
	if _, has := env.Error.Data["install"]; has {
		t.Errorf("the envelope invents an install command: %v", env.Error.Data)
	}
}

// TestOfflineDoesNotBlockQueries: --offline is a kill switch over
// downloads. Nothing a query does can touch the network, so a query works.
func TestOfflineDoesNotBlockQueries(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	for _, args := range [][]string{
		{"--offline", "definition", file + ":3:5"},
		{"definition", file + ":3:5", "--offline"},
	} {
		if code, stdout, stderr := runMain(args...); code != ExitOK {
			t.Errorf("%v: exit %d; stderr: %s\nstdout: %s", args, code, stderr, stdout)
		}
	}
}

// TestOfflineIsReportedByEveryReport: the flag and the environment both
// show up, and neither can be argued out of the other.
func TestOfflineIsReportedByEveryReport(t *testing.T) {
	emptyPath(t)
	useConfigDir(t)
	ws := t.TempDir()
	for name, run := range map[string]func(t *testing.T){
		"flag before the subcommand": func(t *testing.T) {
			_, data := runServers2(t, "--offline", "servers", "--path", ws)
			if !data.Offline {
				t.Error("servers does not report offline")
			}
		},
		"flag after the subcommand": func(t *testing.T) {
			_, data := runServers2(t, "servers", "--path", ws, "--offline")
			if !data.Offline {
				t.Error("servers does not report offline")
			}
		},
		"environment": func(t *testing.T) {
			t.Setenv(serverdef.EnvOffline, "1")
			_, data := runServers2(t, "servers", "--path", ws)
			if !data.Offline {
				t.Error("servers does not report offline")
			}
		},
		"neither": func(t *testing.T) {
			_, data := runServers2(t, "servers", "--path", ws)
			if data.Offline {
				t.Error("servers reports offline with nothing asking for it")
			}
		},
	} {
		t.Run(name, run)
	}

	t.Run("doctor says so", func(t *testing.T) {
		code, stdout, _ := runMain("--offline", "doctor")
		var report struct {
			Offline bool `json:"offline"`
		}
		decodeData(t, stdout, &report)
		if !report.Offline || !strings.Contains(stdout, `"`+serverdef.CheckOffline+`"`) {
			t.Errorf("doctor (exit %d) does not report the offline switch: %s", code, stdout)
		}
	})
}

func runServers2(t *testing.T, args ...string) (int, serversJSON) {
	t.Helper()
	code, stdout, stderr := runMain(args...)
	var data serversJSON
	if env := decodeData(t, stdout, &data); !env.OK {
		t.Fatalf("%v: not ok (exit %d): %s\nstderr: %s", args, code, stdout, stderr)
	}
	return code, data
}

type installJSON struct {
	Plan struct {
		Name   string   `json:"name"`
		Binary string   `json:"binary"`
		Spec   string   `json:"spec"`
		Use    []string `json:"use"`
		Which  []string `json:"which"`
	} `json:"plan"`
	Ran    bool   `json:"ran"`
	Path   string `json:"path"`
	Output string `json:"output"`
}

// TestInstallPlansByDefault: `install` prints the exact command and runs
// nothing. Nothing may download because an agent tried a command to see
// what it does (PLAN §6).
func TestInstallPlansByDefault(t *testing.T) {
	useConfigDir(t)
	calls := fakeMise(t, 0)
	builtin, _ := serverdef.Builtin("gopls")

	code, stdout, stderr := runMain("install", "gopls")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data installJSON
	env := decodeData(t, stdout, &data)
	if want := []string{"mise", "use", "-g", builtin.Install.Mise}; !slices.Equal(data.Plan.Use, want) {
		t.Errorf("plan = %v, want %v", data.Plan.Use, want)
	}
	if data.Ran || data.Path != "" {
		t.Errorf("the default is a plan, but ran=%v path=%q", data.Ran, data.Path)
	}
	if len(env.Warnings) == 0 || !strings.Contains(env.Warnings[0], "install gopls --run") {
		t.Errorf("warnings = %v, want the plan flagged as unexecuted and naming --run", env.Warnings)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("planning invoked mise: %v", got)
	}

	// The plan is available with --offline: a question, not a download.
	if code, _, _ := runMain("--offline", "install", "gopls"); code != ExitOK {
		t.Errorf("--offline install (plan): exit %d, want %d", code, ExitOK)
	}

	code, stdout, _ = runMain("install", "gopls", "--version", "0.99.0")
	decodeData(t, stdout, &data)
	if code != ExitOK || !strings.HasSuffix(data.Plan.Spec, "@0.99.0") {
		t.Errorf("--version: exit %d, spec %q, want the requested version", code, data.Plan.Spec)
	}

	code, stdout, _ = runMain("install", "gopls", "--format", "text")
	if code != ExitOK || !strings.Contains(stdout, "would run: mise use -g "+builtin.Install.Mise) || !strings.Contains(stdout, "nothing was installed") {
		t.Errorf("text plan: exit %d\n%s", code, stdout)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("planning invoked mise: %v", got)
	}
}

// TestInstallRunsOnlyWhenAsked: --run hands the one command to mise, and
// then asks mise where the binary went.
func TestInstallRunsOnlyWhenAsked(t *testing.T) {
	useConfigDir(t)
	calls := fakeMise(t, 0)
	builtin, _ := serverdef.Builtin("gopls")

	code, stdout, stderr := runMain("install", "gopls", "--run")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var data installJSON
	env := decodeData(t, stdout, &data)
	if !data.Ran || data.Path != "/fake/mise/bin/gopls" || !strings.Contains(data.Output, "installed") {
		t.Errorf("result = %+v, want ran with mise's output and path", data)
	}
	for _, w := range env.Warnings {
		if strings.Contains(w, "nothing was installed") {
			t.Errorf("a run is reported as a plan: %v", env.Warnings)
		}
	}
	want := []string{"--version", "use -g " + builtin.Install.Mise, "which gopls"}
	if got := calls(); !slices.Equal(got, want) {
		t.Errorf("mise was called with %q, want %q", got, want)
	}
}

// TestInstallRefusesOffline: the kill switch, from the flag, from the
// environment, and from a batch that inherited it. mise is never run.
func TestInstallRefusesOffline(t *testing.T) {
	useConfigDir(t)
	calls := fakeMise(t, 0)

	refused := func(t *testing.T, code int, stdout string) {
		t.Helper()
		env := decodeData(t, stdout, nil)
		if code != ExitNoServer || env.Error == nil || env.Error.Code != "offline" {
			t.Errorf("exit %d, error %+v; want exit 3 offline", code, env.Error)
		}
		if env.Error != nil && !strings.Contains(env.Error.Message, "mise use -g") {
			t.Errorf("the refusal does not say what to run yourself: %s", env.Error.Message)
		}
	}
	t.Run("flag", func(t *testing.T) {
		code, stdout, _ := runMain("--offline", "install", "gopls", "--run")
		refused(t, code, stdout)
	})
	t.Run("flag after the subcommand", func(t *testing.T) {
		code, stdout, _ := runMain("install", "gopls", "--run", "--offline")
		refused(t, code, stdout)
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv(serverdef.EnvOffline, "1")
		code, stdout, _ := runMain("install", "gopls", "--run")
		refused(t, code, stdout)
	})
	t.Run("a batch's queries inherit it", func(t *testing.T) {
		var out, errOut safeBuffer
		code := MainWithStdin([]string{"--offline", "batch"}, strings.NewReader("install gopls --run\n"), &out, &errOut)
		line := strings.SplitN(out.String(), "\n", 2)[0]
		var query struct {
			Query struct {
				Exit int `json:"exit"`
			} `json:"query"`
		}
		if err := json.Unmarshal([]byte(line), &query); err != nil {
			t.Fatalf("batch line %q: %v", line, err)
		}
		if query.Query.Exit != ExitNoServer || code != ExitNoServer || !strings.Contains(line, `"offline"`) {
			t.Errorf("batch exit %d, query exit %d: %s", code, query.Query.Exit, line)
		}
	})
	if got := calls(); len(got) != 0 {
		t.Errorf("offline mode still ran mise: %v", got)
	}
}

// TestInstallFailures: each way an install can not happen has a code and
// an exit status of its own.
func TestInstallFailures(t *testing.T) {
	t.Run("no such server", func(t *testing.T) {
		useConfigDir(t)
		fakeMise(t, 0)
		code, stdout, _ := runMain("install", "nonesuch")
		env := decodeData(t, stdout, nil)
		if code != ExitUsage || env.Error == nil || env.Error.Code != "no_such_server" || !strings.Contains(env.Error.Message, "gopls") {
			t.Errorf("exit %d, error %+v; want exit 2 no_such_server listing the known servers", code, env.Error)
		}
	})
	t.Run("no mise", func(t *testing.T) {
		useConfigDir(t)
		emptyPath(t)
		code, stdout, _ := runMain("install", "gopls", "--run")
		env := decodeData(t, stdout, nil)
		if code != ExitNoServer || env.Error == nil || env.Error.Code != "mise_unavailable" {
			t.Errorf("exit %d, error %+v; want exit 3 mise_unavailable", code, env.Error)
		}
	})
	t.Run("mise fails", func(t *testing.T) {
		useConfigDir(t)
		fakeMise(t, 1)
		code, stdout, _ := runMain("install", "gopls", "--run")
		env := decodeData(t, stdout, nil)
		if code != ExitCrash || env.Error == nil || env.Error.Code != "install_failed" {
			t.Errorf("exit %d, error %+v; want exit 4 install_failed", code, env.Error)
		}
	})
	t.Run("a definition with no install spec", func(t *testing.T) {
		useConfigDir(t)
		fakeMise(t, 0)
		ws := t.TempDir()
		write(t, filepath.Join(ws, serverdef.WorkspaceFile), "schema_version = 1\nname = \"wat-ls\"\n\n[activation]\nglobs = [\"**/*.wat\"]\n\n[server]\ncommand = [\"wat-ls\"]\n")
		code, stdout, _ := runMain("install", "wat-ls", "--path", ws)
		env := decodeData(t, stdout, nil)
		if code != ExitNoServer || env.Error == nil || env.Error.Code != "server_not_installed" {
			t.Errorf("exit %d, error %+v; want exit 3 and no invented package name", code, env.Error)
		}
	})
}

// TestDoctorReportsRoutingAndConfig: the existing serverdef.Doctor, at
// the command line, with the router of the path's own workspace behind
// PathCheck.
func TestDoctorReportsRoutingAndConfig(t *testing.T) {
	emptyPath(t)
	useConfigDir(t)
	dir, goFile := cjkFixture(t)
	wat := filepath.Join(dir, "notes.wat")
	write(t, wat, "(module)\n")

	code, stdout, stderr := runMain("doctor", goFile, wat)
	var report struct {
		Workspace string `json:"workspace"`
		Worst     string `json:"worst"`
		Checks    []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
			Subject  string `json:"subject"`
			Message  string `json:"message"`
		} `json:"checks"`
		Servers []struct {
			Name string `json:"name"`
		} `json:"servers"`
	}
	decodeData(t, stdout, &report)
	byPath := map[string]string{}
	severities := map[string]string{}
	for _, c := range report.Checks {
		if c.ID == serverdef.CheckPathRouting {
			byPath[c.Subject] = c.Message
			severities[c.Subject] = c.Severity
		}
	}
	if got := byPath[goFile]; got != "handled by gopls" || severities[goFile] != "ok" {
		t.Errorf("routing of %s = %q (%s), want handled by gopls", goFile, got, severities[goFile])
	}
	if got := byPath[wat]; !strings.Contains(got, "no server") || severities[wat] != "error" {
		t.Errorf("routing of %s = %q (%s), want an error: no server", wat, got, severities[wat])
	}
	if report.Worst != "error" || code != ExitProblems {
		t.Errorf("worst %q, exit %d (stderr %s); want error and exit %d", report.Worst, code, stderr, ExitProblems)
	}
	if len(report.Servers) != len(serverdef.BuiltinNames()) {
		t.Errorf("doctor lists %d servers, want %d", len(report.Servers), len(serverdef.BuiltinNames()))
	}

	// Without the unclaimed file nothing is an error: a server that is
	// merely missing is a warning, because lightspeed works for the
	// others.
	code, stdout, _ = runMain("doctor", goFile)
	decodeData(t, stdout, &report)
	if code != ExitOK || report.Worst == "error" {
		t.Errorf("doctor %s: exit %d, worst %q; want no error", goFile, code, report.Worst)
	}

	code, stdout, _ = runMain("doctor", goFile, "--format", "text")
	if code != ExitOK || !strings.Contains(stdout, "handled by gopls") || !strings.Contains(stdout, "result:") {
		t.Errorf("text doctor: exit %d\n%s", code, stdout)
	}
}

// TestServerdefCodesAreRenderCodes keeps the two taxonomies one: every
// error internal/serverdef can return has to come out of the renderer with
// the code and exit status serverdef gave it, and that code has to be one
// internal/render knows.
func TestServerdefCodesAreRenderCodes(t *testing.T) {
	origin := serverdef.Origin{Layer: serverdef.LayerUser, File: "/x.toml"}
	for _, err := range []error{
		&serverdef.ConfigError{Origin: origin, Err: os.ErrInvalid},
		&serverdef.ConflictError{Name: "gopls", First: origin, Second: origin},
		&serverdef.NotInstalledError{Name: "gopls", Binary: "gopls"},
		&serverdef.OfflineError{Action: "install gopls"},
		&serverdef.MiseUnavailableError{Action: "install gopls", Binary: "gopls"},
		&serverdef.NoSuchServerError{Name: "x"},
		&serverdef.InstallFailedError{Name: "gopls", Err: os.ErrInvalid},
	} {
		wrapped := serverdefFailure(err)
		coded := err.(interface {
			Code() string
			ExitCode() int
		})
		if got := string(render.CodeForError(wrapped)); got != coded.Code() {
			t.Errorf("%T: envelope code %q, want serverdef's %q", err, got, coded.Code())
		}
		if got := render.ExitCode(wrapped); got != coded.ExitCode() {
			t.Errorf("%T: exit %d, want serverdef's %d", err, got, coded.ExitCode())
		}
		if got := render.ExitCodeForCode(render.Code(coded.Code())); got != coded.ExitCode() {
			t.Errorf("%T: render's table maps %q to %d, serverdef says %d", err, coded.Code(), got, coded.ExitCode())
		}
	}
}

// TestDaemonStatusOmitsStartedWithoutADaemon: a zero time.Time is not
// "absent" to encoding/json, and 0001-01-01T00:00:00Z reads as data.
func TestDaemonStatusOmitsStartedWithoutADaemon(t *testing.T) {
	daemonEnv(t, "30s")
	dir := t.TempDir()
	code, stdout, stderr := runMain("daemon", "status", "--path", dir)
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s", code, stderr)
	}
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data["running"] != false {
		t.Fatalf("a daemon is running for a fresh directory: %s", stdout)
	}
	if started, present := env.Data["started"]; present {
		t.Errorf("started = %v with no daemon running, want the key absent", started)
	}
	if strings.Contains(stdout, "0001-01-01") {
		t.Errorf("a zero time leaked into the envelope: %s", stdout)
	}
}
