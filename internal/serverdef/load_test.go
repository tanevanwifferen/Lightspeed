package serverdef

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// layerOf finds the reported layer of one server, so a test can say
// what it means in one line.
func resolvedOf(t *testing.T, res *Resolution, name string) *Resolved {
	t.Helper()
	got, ok := res.Server(name)
	if !ok {
		t.Fatalf("server %q is not in the resolution (have %v)", name, res.Names())
	}
	return got
}

func layerStatus(t *testing.T, res *Resolution, layer Layer) LayerStatus {
	t.Helper()
	for _, s := range res.Layers {
		if s.Layer == layer {
			return s
		}
	}
	t.Fatalf("no status reported for the %s layer", layer)
	return LayerStatus{}
}

// TestLoadBuiltinsOnly is the zero-config case of PLAN §6: no files
// anywhere, and every built-in server is configured and attributed.
func TestLoadBuiltinsOnly(t *testing.T) {
	e := newTestEnv(t)
	res, err := Load(e.Options())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got, want := res.Names(), BuiltinNames(); len(got) != len(want) {
		t.Fatalf("Names() = %v, want the %d built-ins", got, len(want))
	}
	// Sorted by name, so that every report over a resolution is stable.
	sorted := slices.Clone(res.Names())
	slices.Sort(sorted)
	if !slices.Equal(res.Names(), sorted) {
		t.Errorf("Names() = %v, want them sorted", res.Names())
	}

	gopls := resolvedOf(t, res, "gopls")
	if gopls.Origin.Layer != LayerBuiltin {
		t.Errorf("gopls origin = %v, want the builtin layer", gopls.Origin)
	}
	if gopls.Origin.File != "" {
		t.Errorf("gopls origin file = %q, want empty: built-ins are compiled in", gopls.Origin.File)
	}
	if len(gopls.Overrides) != 1 || !gopls.Overrides[0].Whole {
		t.Errorf("gopls overrides = %v, want one whole definition", gopls.Overrides)
	}
	if len(gopls.Shadowed()) != 0 {
		t.Errorf("gopls shadowed = %v, want nothing shadowed", gopls.Shadowed())
	}

	// The layers are reported strongest first, and each says what it
	// looked at even when it found nothing.
	if got := []Layer{res.Layers[0].Layer, res.Layers[1].Layer, res.Layers[2].Layer}; !slices.Equal(got, Layers()) {
		t.Errorf("layer order = %v, want %v", got, Layers())
	}
	ws := layerStatus(t, res, LayerWorkspace)
	if ws.Exists || ws.Path != filepath.Join(e.Workspace, WorkspaceFile) {
		t.Errorf("workspace status = %+v, want the path it looked for and Exists false", ws)
	}
	user := layerStatus(t, res, LayerUser)
	if want := filepath.Join(e.ConfigDir, ServersDir); user.Path != want {
		t.Errorf("user layer path = %q, want %q", user.Path, want)
	}
	if len(user.Files) != 0 {
		t.Errorf("user layer files = %v, want none", user.Files)
	}
	if b := layerStatus(t, res, LayerBuiltin); len(b.Servers) != len(BuiltinNames()) {
		t.Errorf("builtin layer servers = %v, want all %d", b.Servers, len(BuiltinNames()))
	}
}

// TestLoadUserPartialOverride is the case the fragment machinery exists
// for: a user file that changes one key keeps everything else.
func TestLoadUserPartialOverride(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("gopls.toml", `schema_version = 1
name = "gopls"

[activation]
priority = 90
`)
	res, err := Load(e.Options())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	gopls := resolvedOf(t, res, "gopls")

	if gopls.Def.Activation.Priority != 90 {
		t.Errorf("priority = %d, want 90 from the user layer", gopls.Def.Activation.Priority)
	}
	builtin, _ := Builtin("gopls")
	if !slices.Equal(gopls.Def.Server.Command, builtin.Server.Command) {
		t.Errorf("command = %v, want the built-in %v kept", gopls.Def.Server.Command, builtin.Server.Command)
	}
	if !slices.Equal(gopls.Def.Activation.RootMarkers, builtin.Activation.RootMarkers) {
		t.Errorf("root markers = %v, want the built-in %v kept", gopls.Def.Activation.RootMarkers, builtin.Activation.RootMarkers)
	}

	// Provenance: the user layer won, and it says exactly what it set.
	if gopls.Origin.Layer != LayerUser {
		t.Errorf("origin = %v, want the user layer", gopls.Origin)
	}
	if want := filepath.Join(e.ConfigDir, ServersDir, "gopls.toml"); gopls.Origin.File != want {
		t.Errorf("origin file = %q, want %q", gopls.Origin.File, want)
	}
	if got, want := gopls.Overrides[0].Keys, []string{KeyPriority}; !slices.Equal(got, want) {
		t.Errorf("winning override keys = %v, want %v", got, want)
	}
	if gopls.Overrides[0].Whole {
		t.Error("the winning override is marked whole, but it is a fragment")
	}
	shadowed := gopls.Shadowed()
	if len(shadowed) != 1 || shadowed[0].Origin.Layer != LayerBuiltin || !shadowed[0].Whole {
		t.Fatalf("shadowed = %v, want the whole built-in definition", shadowed)
	}
	// Nothing else was touched.
	pyright := resolvedOf(t, res, "pyright")
	if pyright.Origin.Layer != LayerBuiltin {
		t.Errorf("pyright origin = %v, want the builtin layer", pyright.Origin)
	}
}

// TestLoadWorkspaceBeatsUser is PLAN §6's ordering: the in-tree file is
// the strongest, and the layers it overrode stay visible.
func TestLoadWorkspaceBeatsUser(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 70\n[server]\ncommand = [\"gopls\", \"-rpc.trace\"]\n")
	e.WriteWorkspace("schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 90\n")

	res, err := Load(e.Options())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	gopls := resolvedOf(t, res, "gopls")
	if gopls.Def.Activation.Priority != 90 {
		t.Errorf("priority = %d, want the workspace's 90", gopls.Def.Activation.Priority)
	}
	// The user layer still wins over the built-in for the key the
	// workspace did not mention.
	if got, want := gopls.Def.Server.Command, []string{"gopls", "-rpc.trace"}; !slices.Equal(got, want) {
		t.Errorf("command = %v, want the user layer's %v", got, want)
	}
	if got := []Layer{gopls.Overrides[0].Origin.Layer, gopls.Overrides[1].Origin.Layer, gopls.Overrides[2].Origin.Layer}; !slices.Equal(got, Layers()) {
		t.Errorf("override layers = %v, want %v (strongest first)", got, Layers())
	}
	if gopls.Origin.Layer != LayerWorkspace {
		t.Errorf("origin = %v, want the workspace layer", gopls.Origin)
	}
}

// TestLoadWorkspaceMultiServer covers the polyglot repo: one file, two
// servers, in the [servers.<name>] form.
func TestLoadWorkspaceMultiServer(t *testing.T) {
	e := newTestEnv(t)
	e.WriteWorkspace(`schema_version = 1

[servers.gopls.activation]
priority = 80

[servers.zls]
[servers.zls.activation]
languages = ["zig"]
globs = ["**/*.zig"]
root_markers = ["build.zig", ".git"]

[servers.zls.server]
command = ["zls"]

[servers.zls.install]
mise = "zls"
`)
	res, err := Load(e.Options())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := resolvedOf(t, res, "gopls").Def.Activation.Priority; got != 80 {
		t.Errorf("gopls priority = %d, want 80", got)
	}
	zls := resolvedOf(t, res, "zls")
	if zls.Origin.Layer != LayerWorkspace {
		t.Errorf("zls origin = %v, want the workspace layer", zls.Origin)
	}
	if got, want := zls.Def.Server.Command, []string{"zls"}; !slices.Equal(got, want) {
		t.Errorf("zls command = %v, want %v", got, want)
	}
	if zls.Def.Activation.Priority != DefaultPriority {
		t.Errorf("zls priority = %d, want the default %d", zls.Def.Activation.Priority, DefaultPriority)
	}
	if len(res.Names()) != len(BuiltinNames())+1 {
		t.Errorf("names = %v, want the built-ins plus zls", res.Names())
	}
}

// TestLoadIncompleteNewServer refuses a fragment for a server no other
// layer defines: it cannot be started, and a definition that silently
// does nothing is what PLAN §6 forbids.
func TestLoadIncompleteNewServer(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("zls.toml", "schema_version = 1\nname = \"zls\"\n[activation]\nlanguages = [\"zig\"]\n")

	_, err := Load(e.Options())
	if err == nil {
		t.Fatal("Load() succeeded, want an error about the incomplete definition")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error %v is not an ErrInvalidConfig", err)
	}
	for _, want := range []string{"zls.toml", "server.command is required", "must describe it completely"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
	if got := exitCodeOf(t, err); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
}

// TestLoadSameLayerConflict: two files in one layer defining one server
// is an ambiguity with no rule to break it, so it is refused rather
// than resolved by filename luck.
func TestLoadSameLayerConflict(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("00-gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 10\n")
	e.WriteUser("99-gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 20\n")

	_, err := Load(e.Options())
	if err == nil {
		t.Fatal("Load() succeeded, want a conflict error")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("error %v is not an ErrInvalidConfig", err)
	}
	for _, want := range []string{"defined twice in the same configuration layer", "00-gopls.toml", "99-gopls.toml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// TestLoadCrossLayerIsNotAConflict is the other half of that rule.
func TestLoadCrossLayerIsNotAConflict(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 10\n")
	e.WriteWorkspace("schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 20\n")
	if _, err := Load(e.Options()); err != nil {
		t.Fatalf("Load() = %v, want the workspace layer to simply win", err)
	}
}

func TestLoadBrokenFileIsFatal(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("broken.toml", "schema_version = 1\nname = \"x\noops\n")

	_, err := Load(e.Options())
	if err == nil {
		t.Fatal("Load() succeeded, want a parse error")
	}
	for _, want := range []string{"broken.toml", "line 2", "unterminated string"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestLoadEmptyMentionIsAnError(t *testing.T) {
	e := newTestEnv(t)
	e.WriteWorkspace("schema_version = 1\n[servers.gopls]\n")
	_, err := Load(e.Options())
	if err == nil {
		t.Fatal("Load() succeeded, want an error about a server mentioned and not configured")
	}
	if !strings.Contains(err.Error(), "sets nothing") {
		t.Errorf("error = %q, want it to say the file sets nothing", err)
	}
}

// TestLoadUnreadableDirectoryAndFile covers the I/O failures, which must
// be reported and not skipped.
func TestLoadUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not deny anything")
	}
	e := newTestEnv(t)
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n")
	path := filepath.Join(e.ConfigDir, ServersDir, "gopls.toml")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	_, err := Load(e.Options())
	if err == nil {
		t.Fatal("Load() succeeded, want a permission error")
	}
	if !strings.Contains(err.Error(), "gopls.toml") {
		t.Errorf("error = %q, want it to name the file", err)
	}
}

func TestLoadSkipFlags(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 90\n")

	opts := e.Options()
	opts.SkipUser = true
	res, err := Load(opts)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := resolvedOf(t, res, "gopls").Def.Activation.Priority; got != DefaultPriority {
		t.Errorf("priority = %d, want the built-in default with the user layer skipped", got)
	}
	if s := layerStatus(t, res, LayerUser); s.Skipped == "" {
		t.Error("the user layer was skipped without saying so")
	}

	opts = e.Options()
	opts.SkipBuiltins = true
	if _, err := Load(opts); err == nil {
		t.Fatal("Load() with SkipBuiltins succeeded, but the user fragment is incomplete on its own")
	}

	opts.SkipUser = true
	res, err = Load(opts)
	if err != nil {
		t.Fatalf("Load() with everything skipped = %v", err)
	}
	if len(res.Servers) != 0 {
		t.Errorf("servers = %v, want none", res.Names())
	}
}

// TestUserConfigDir walks the whole XDG chain, including the case where
// there is no home at all — a daemon started from systemd, say.
func TestUserConfigDir(t *testing.T) {
	tests := []struct {
		name      string
		configDir string
		vars      map[string]string
		want      string
		wantWhy   string
	}{{
		name:      "explicit",
		configDir: "/opt/ls",
		vars:      map[string]string{EnvConfigDir: "/ignored", EnvConfigHome: "/ignored"},
		want:      "/opt/ls",
		wantWhy:   "set by the caller",
	}, {
		name:    "environment override",
		vars:    map[string]string{EnvConfigDir: "/opt/ls", EnvConfigHome: "/xdg"},
		want:    "/opt/ls",
		wantWhy: "from " + EnvConfigDir,
	}, {
		name:    "xdg",
		vars:    map[string]string{EnvConfigHome: "/xdg", "HOME": "/home/u"},
		want:    filepath.Join("/xdg", ConfigSubdir),
		wantWhy: "from " + EnvConfigHome,
	}, {
		name:    "home",
		vars:    map[string]string{"HOME": "/home/u"},
		want:    filepath.Join("/home/u", ".config", ConfigSubdir),
		wantWhy: "from $HOME",
	}, {
		name:    "nothing",
		vars:    map[string]string{},
		want:    "",
		wantWhy: "no " + EnvConfigHome + " and no $HOME",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := Options{ConfigDir: tt.configDir, Getenv: func(k string) string { return tt.vars[k] }}
			dir, why := opts.UserConfigDir()
			if dir != tt.want {
				t.Errorf("dir = %q, want %q", dir, tt.want)
			}
			if why != tt.wantWhy {
				t.Errorf("why = %q, want %q", why, tt.wantWhy)
			}
		})
	}
}

// TestUserLayerFromXDG proves the XDG path is really the one read, not
// just the one reported.
func TestUserLayerFromXDG(t *testing.T) {
	e := newTestEnv(t)
	xdg := filepath.Join(t.TempDir(), "xdg")
	dir := filepath.Join(xdg, ConfigSubdir, ServersDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	e.write(filepath.Join(dir, "gopls.toml"), "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 33\n")

	opts := e.Options()
	opts.ConfigDir = ""
	e.Vars[EnvConfigHome] = xdg

	res, err := Load(opts)
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if got := resolvedOf(t, res, "gopls").Def.Activation.Priority; got != 33 {
		t.Errorf("priority = %d, want 33 from $%s", got, EnvConfigHome)
	}
}

// TestLoadIgnoresNonTOML: servers.d holds *.toml, and nothing else is
// read — an editor backup file must not become configuration.
func TestLoadIgnoresNonTOML(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("gopls.toml.bak", "this is not toml at all")
	e.WriteUser("README", "notes")
	dir := filepath.Join(e.ConfigDir, ServersDir, "nested.toml")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := Load(e.Options())
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if s := layerStatus(t, res, LayerUser); len(s.Files) != 0 {
		t.Errorf("read %v, want nothing: only *.toml files, and only files", s.Files)
	}
}

func TestOfflineSwitch(t *testing.T) {
	tests := []struct {
		env  string
		flag bool
		want bool
	}{
		{env: "", flag: false, want: false},
		{env: "", flag: true, want: true},
		{env: "1", want: true},
		{env: "true", want: true},
		{env: "YES", want: true},
		{env: "on", want: true},
		{env: "yolo", want: true},
		{env: "0", want: false},
		{env: "false", want: false},
		{env: "no", want: false},
		{env: " off ", want: false},
	}
	for _, tt := range tests {
		opts := Options{Offline: tt.flag, Getenv: func(k string) string {
			if k == EnvOffline {
				return tt.env
			}
			return ""
		}}
		if got := opts.offline(); got != tt.want {
			t.Errorf("offline with %s=%q flag=%t = %t, want %t", EnvOffline, tt.env, tt.flag, got, tt.want)
		}
	}
}

func TestFindWorkspaceRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, WorkspaceFile), []byte("schema_version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FindWorkspaceRoot(deep)
	if err != nil {
		t.Fatalf("FindWorkspaceRoot() = %v", err)
	}
	if got != root {
		t.Errorf("FindWorkspaceRoot(%q) = %q, want %q", deep, got, root)
	}

	// From a file, not a directory.
	file := filepath.Join(deep, "main.go")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := FindWorkspaceRoot(file); got != root {
		t.Errorf("FindWorkspaceRoot(%q) = %q, want %q", file, got, root)
	}

	// No file anywhere is the normal zero-config case, not an error.
	other := t.TempDir()
	got, err = FindWorkspaceRoot(other)
	if err != nil {
		t.Fatalf("FindWorkspaceRoot() = %v", err)
	}
	if got != "" {
		t.Errorf("FindWorkspaceRoot(%q) = %q, want empty", other, got)
	}
}

// TestLoadIsDeterministic: two loads of the same tree describe it
// identically, which is what makes the reports diffable.
func TestLoadIsDeterministic(t *testing.T) {
	e := newTestEnv(t)
	e.WriteUser("a-gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 51\n")
	e.WriteUser("b-vtsls.toml", "schema_version = 1\nname = \"vtsls\"\n[activation]\npriority = 52\n")
	e.WriteWorkspace("schema_version = 1\n[servers.pyright.activation]\npriority = 53\n")

	first, err := Load(e.Options())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Load(e.Options())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(first.Names(), second.Names()) {
		t.Errorf("names differ between loads: %v vs %v", first.Names(), second.Names())
	}
	for i := range first.Servers {
		a, b := first.Servers[i], second.Servers[i]
		if a.Origin != b.Origin {
			t.Errorf("%s origin differs: %v vs %v", a.Name(), a.Origin, b.Origin)
		}
		if len(a.Overrides) != len(b.Overrides) {
			t.Errorf("%s override count differs: %d vs %d", a.Name(), len(a.Overrides), len(b.Overrides))
		}
	}
}

func TestRequire(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("gopls")
	opts := e.Options()
	opts.SkipMise = true

	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	// Before probing, Require refuses to guess.
	if _, err := res.Require("gopls"); err == nil || !strings.Contains(err.Error(), "has not been probed") {
		t.Errorf("Require before Probe = %v, want a refusal to guess", err)
	}

	res.Probe(t.Context(), opts)

	resolved, err := res.Require("gopls")
	if err != nil {
		t.Fatalf("Require(gopls) = %v", err)
	}
	if !resolved.Installed() {
		t.Error("gopls is on the test PATH but Installed() is false")
	}

	// A missing server is exit 3 with the exact command to run.
	_, err = res.Require("pyright")
	if err == nil {
		t.Fatal("Require(pyright) succeeded, but nothing is installed")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("error %v is not an ErrNotInstalled", err)
	}
	if got := exitCodeOf(t, err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	var notInstalled *NotInstalledError
	if !errors.As(err, &notInstalled) {
		t.Fatalf("error %v is not a *NotInstalledError", err)
	}
	if got, want := notInstalled.InstallCommand, []string{MiseName, "use", "-g", "npm:pyright"}; !slices.Equal(got, want) {
		t.Errorf("install command = %v, want %v", got, want)
	}
	if !strings.Contains(err.Error(), "mise use -g npm:pyright") {
		t.Errorf("error = %q, want it to quote the install command", err)
	}

	// An unknown name is a usage error, and lists what exists.
	_, err = res.Require("nope")
	if !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("Require(nope) = %v, want ErrNoSuchServer", err)
	}
	if got := exitCodeOf(t, err); got != 2 {
		t.Errorf("exit code = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "gopls") {
		t.Errorf("error = %q, want it to list the configured servers", err)
	}
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	var exiter interface{ ExitCode() int }
	if !errors.As(err, &exiter) {
		t.Fatalf("error %v carries no ExitCode()", err)
	}
	return exiter.ExitCode()
}
