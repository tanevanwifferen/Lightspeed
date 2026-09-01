package serverdef

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"
)

// errorsAs is errors.As, named so the intent reads at the call site.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// m4Servers is PLAN §8's definition of done for M4, together with the
// executable each definition must actually find on PATH. The binary
// names are not the server names, which is the point: a table that
// looked right and searched for "lua-ls" would find nothing.
var m4Servers = map[string]string{
	"gopls":         "gopls",
	"rust-analyzer": "rust-analyzer",
	"pyright":       "pyright-langserver",
	"vtsls":         "vtsls",
	"clangd":        "clangd",
	"lua-ls":        "lua-language-server",
}

// TestM4ZeroConfigResolvesAllSix is the milestone criterion, made
// hermetic: on a machine with the six binaries on PATH and no
// configuration file anywhere, every one of the six resolves to a
// runnable executable with nothing hand-written.
//
// The real criterion — all six answering `references` — needs the real
// servers and cannot be a unit test. This is the half of it that can
// be: everything up to the spawn.
func TestM4ZeroConfigResolvesAllSix(t *testing.T) {
	e := newTestEnv(t)
	for _, binary := range m4Servers {
		e.Binary(binary)
	}
	opts := e.Options()
	opts.SkipMise = true // no installer anywhere: PATH alone must be enough

	res, err := Resolve(t.Context(), opts)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}

	// No file was read: this is items 3 and 4 of PLAN §6 and nothing else.
	for _, layer := range []Layer{LayerWorkspace, LayerUser} {
		if s := layerStatus(t, res, layer); len(s.Files) != 0 {
			t.Errorf("the %s layer read %v, but the test wrote no configuration", layer, s.Files)
		}
	}

	names := slices.Sorted(slices.Values(res.Names()))
	want := slices.Sorted(slices.Values(slices.Collect(maps.Keys(m4Servers))))
	if !slices.Equal(names, want) {
		t.Fatalf("resolved %v, want exactly %v", names, want)
	}

	for name, binary := range m4Servers {
		t.Run(name, func(t *testing.T) {
			resolved, err := res.Require(name)
			if err != nil {
				t.Fatalf("Require(%q) = %v", name, err)
			}
			if resolved.Binary.Source != BinaryPATH {
				t.Errorf("source = %v, want %v: this is the zero-config path", resolved.Binary.Source, BinaryPATH)
			}
			if resolved.Binary.Name != binary {
				t.Errorf("looked for %q, want %q", resolved.Binary.Name, binary)
			}
			if resolved.Origin.Layer != LayerBuiltin {
				t.Errorf("origin = %v, want the built-in layer", resolved.Origin)
			}
			if len(resolved.Shadowed()) != 0 {
				t.Errorf("shadowed = %v, want nothing: no other layer exists", resolved.Shadowed())
			}
			// The definition is complete enough to start a server with.
			if err := resolved.Def.Validate(); err != nil {
				t.Errorf("effective definition does not validate: %v", err)
			}
			if len(resolved.Def.Activation.RootMarkers) == 0 {
				t.Error("no root markers, so the router could not find a workspace root")
			}
		})
	}

	// And doctor says so, without a single error or warning.
	report, err := Doctor(t.Context(), nil, opts)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	for _, c := range report.Checks {
		// mise is deliberately absent here, which doctor reports as
		// info because SkipMise is a choice, not a fault.
		if c.Severity == SeverityWarn || c.Severity == SeverityError {
			t.Errorf("doctor on a fully-installed machine reports %v", c)
		}
	}
	if report.Provenance.License != "Apache-2.0" || report.Provenance.Commit == "" {
		t.Errorf("doctor provenance = %+v, want the corpus attributed", report.Provenance)
	}
}

// TestM4EveryLayerAtOnce exercises all four items of PLAN §6 together —
// the case each single-layer test cannot reach — and checks that the
// winner of every key is inspectable rather than merely correct.
func TestM4EveryLayerAtOnce(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("gopls")
	// Built-in: languages, globs, root markers, command, install spec.
	// User layer: a different command and a priority.
	e.WriteUser("gopls.toml", `schema_version = 1
name = "gopls"

[activation]
priority = 70

[server]
command = ["gopls"]
settings = { gopls = { "ui.diagnostic.staticcheck" = true } }
`)
	// Workspace layer: overrides the priority only.
	e.WriteWorkspace(`schema_version = 1
name = "gopls"

[activation]
priority = 90

[server]
settings = { gopls = { buildFlags = ["-tags=integration"] } }
`)
	opts := e.Options()
	opts.SkipMise = true

	res, err := Resolve(t.Context(), opts)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	gopls := resolvedOf(t, res, "gopls")

	// Every key came from the strongest layer that mentioned it.
	if gopls.Def.Activation.Priority != 90 {
		t.Errorf("priority = %d, want the workspace's 90", gopls.Def.Activation.Priority)
	}
	builtin, _ := Builtin("gopls")
	if !slices.Equal(gopls.Def.Activation.RootMarkers, builtin.Activation.RootMarkers) {
		t.Errorf("root markers = %v, want the built-in's %v", gopls.Def.Activation.RootMarkers, builtin.Activation.RootMarkers)
	}
	if gopls.Def.Install.Mise != builtin.Install.Mise {
		t.Errorf("install spec = %q, want the built-in's %q", gopls.Def.Install.Mise, builtin.Install.Mise)
	}
	// The two settings tables merged rather than replaced, which is the
	// one merge rule that is not "the stronger layer wins outright".
	settings, ok := gopls.Def.Server.Settings["gopls"].(map[string]any)
	if !ok {
		t.Fatalf("settings = %#v, want a gopls table", gopls.Def.Server.Settings)
	}
	if settings["ui.diagnostic.staticcheck"] != true {
		t.Errorf("settings = %#v, want the user layer's staticcheck kept", settings)
	}
	if _, ok := settings["buildFlags"]; !ok {
		t.Errorf("settings = %#v, want the workspace's buildFlags added", settings)
	}

	// PATH sniffing is the fourth item, and it is not a layer.
	if gopls.Binary.Source != BinaryPATH {
		t.Errorf("binary source = %v, want %v", gopls.Binary.Source, BinaryPATH)
	}

	// Nothing was shadowed silently: all three layers are on the record,
	// strongest first, each with the keys it set.
	if got := len(gopls.Overrides); got != 3 {
		t.Fatalf("overrides = %d, want one per layer: %v", got, gopls.Overrides)
	}
	wantKeys := [][]string{
		{KeyPriority, KeySettings},
		{KeyPriority, KeyCommand, KeySettings},
		nil, // the built-in sets everything
	}
	for i, override := range gopls.Overrides {
		if override.Origin.Layer != Layers()[i] {
			t.Errorf("override %d is from %v, want %v", i, override.Origin.Layer, Layers()[i])
		}
		if wantKeys[i] != nil && !slices.Equal(override.Keys, wantKeys[i]) {
			t.Errorf("override %d keys = %v, want %v", i, override.Keys, wantKeys[i])
		}
	}
	if !gopls.Overrides[2].Whole {
		t.Error("the built-in contribution is not marked whole")
	}

	// doctor explains the override rather than leaving it implicit.
	report, err := Doctor(t.Context(), nil, opts)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	var explained string
	for _, c := range report.Checks {
		if c.ID == CheckLayerOverride && c.Subject == "gopls" {
			explained = c.Message
		}
	}
	if explained == "" {
		t.Fatal("doctor reported no layer_override check for gopls")
	}
	for _, want := range []string{WorkspaceFile, ServersDir, "built-in default", KeyPriority} {
		if !strings.Contains(explained, want) {
			t.Errorf("override check %q does not mention %q", explained, want)
		}
	}
}

// TestM4ProbingNeverInstalls pins PLAN §6's "nothing downloads
// implicitly" at the level it actually has to hold: the commands
// lightspeed runs while merely answering "where is this server?".
//
// The assertion is on the child's environment, not on mise's behaviour.
// mise as shipped does not install from `which`, but that is mise's
// choice to change and this guarantee is ours to keep.
func TestM4ProbingNeverInstalls(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	var ran [][]string
	base := fakeMise("2026.8.14", nil)
	e.Runner = func(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
		ran = append(ran, args)
		return base(ctx, env, name, args...)
	}

	opts := e.Options()
	res, err := Resolve(t.Context(), opts)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if len(ran) == 0 {
		t.Fatal("mise was never consulted, so this test proves nothing")
	}
	// Only questions were asked.
	for _, args := range ran {
		if len(args) == 0 || (args[0] != "--version" && args[0] != "which") {
			t.Errorf("probing ran `mise %s`, want only --version and which", strings.Join(args, " "))
		}
	}
	// And every one of them was told not to install.
	if len(e.Env) != len(ran) {
		t.Fatalf("recorded %d environments for %d commands", len(e.Env), len(ran))
	}
	for i, env := range e.Env {
		for _, want := range miseProbeEnv {
			if !slices.Contains(env, want) {
				t.Errorf("`mise %s` ran without %s (env %v)", strings.Join(ran[i], " "), want, env)
			}
		}
	}

	// Nothing resolved, and every server says so with exit 3 and the
	// command that would fix it — never by fixing it itself.
	for name := range m4Servers {
		resolved := resolvedOf(t, res, name)
		if resolved.Installed() {
			t.Errorf("%s reports installed, but nothing is on PATH", name)
		}
		err := resolved.NotInstalledError(false)
		if got := exitCodeOf(t, err); got != 3 {
			t.Errorf("%s exit code = %d, want 3", name, got)
		}
		if !strings.Contains(err.Error(), "mise use -g ") {
			t.Errorf("%s error = %q, want the exact install command", name, err)
		}
	}
}

// TestM4InstallIsTheOnlyDownload is the other side of that rule: the one
// call allowed to fetch runs with the ambient environment, because
// forcing MISE_OFFLINE on it would break the only operation the user
// explicitly asked to be online for.
func TestM4InstallIsTheOnlyDownload(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	var useEnv []string
	base := fakeMise("2026.8.14", nil)
	e.Runner = func(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
		if len(args) > 0 && args[0] == "use" {
			useEnv = append([]string{}, env...)
		}
		return base(ctx, env, name, args...)
	}

	if _, err := InstallServer(t.Context(), InstallRequest{Name: "gopls"}, e.Options()); err != nil {
		t.Fatalf("InstallServer() = %v", err)
	}
	for _, forbidden := range miseProbeEnv {
		if slices.Contains(useEnv, forbidden) {
			t.Errorf("`mise use` ran with %s, which would stop the install it was asked to do", forbidden)
		}
	}
}

// TestM4OfflineRefusesTheInstallAndNothingElse: the kill switch stops
// the one operation that reaches the network and leaves resolution —
// which never does — working, because a CLI that went blind offline
// would be useless on a plane.
func TestM4OfflineRefusesTheInstallAndNothingElse(t *testing.T) {
	for _, tt := range []struct {
		name string
		set  func(*Options)
	}{
		{"flag", func(o *Options) { o.Offline = true }},
		{"env", func(o *Options) { /* set below */ }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestEnv(t)
			e.Binary("mise")
			e.Binary("gopls")
			e.Runner = fakeMise("2026.8.14", nil)
			if tt.name == "env" {
				e.Vars[EnvOffline] = "1"
			}
			opts := e.Options()
			tt.set(&opts)

			res, err := Resolve(t.Context(), opts)
			if err != nil {
				t.Fatalf("Resolve() offline = %v, want resolution to keep working", err)
			}
			if !res.Offline {
				t.Fatal("the resolution does not report the kill switch")
			}
			if _, err := res.Require("gopls"); err != nil {
				t.Errorf("Require(gopls) offline = %v, want the PATH binary to still be usable", err)
			}

			// A dry run is a question, so it is still answered.
			result, err := res.Install(t.Context(), InstallRequest{Name: "pyright", DryRun: true}, opts)
			if err != nil {
				t.Fatalf("dry-run install offline = %v, want the plan", err)
			}
			if result.Ran {
				t.Error("a dry run ran mise")
			}

			// The real thing is refused, with the command to run later.
			_, err = res.Install(t.Context(), InstallRequest{Name: "pyright"}, opts)
			var offline *OfflineError
			if !errorsAs(err, &offline) {
				t.Fatalf("install offline = %v, want an *OfflineError", err)
			}
			if got := exitCodeOf(t, err); got != 3 {
				t.Errorf("exit code = %d, want 3", got)
			}
			if !strings.Contains(err.Error(), "mise use -g npm:pyright") {
				t.Errorf("error = %q, want the command to run when online", err)
			}
			if !strings.Contains(err.Error(), EnvOffline) {
				t.Errorf("error = %q, want it to name the switch that caused it", err)
			}
		})
	}
}

// TestM4PathFallbackWhenMiseIsAbsent: mise is not a dependency, it is a
// delegate. Without it, PATH still answers, and only installing breaks.
func TestM4PathFallbackWhenMiseIsAbsent(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("gopls") // no mise on the test PATH at all
	opts := e.Options()

	res, err := Resolve(t.Context(), opts)
	if err != nil {
		t.Fatalf("Resolve() = %v", err)
	}
	if res.Mise.Available {
		t.Fatalf("mise = %+v, want unavailable", res.Mise)
	}
	if _, err := res.Require("gopls"); err != nil {
		t.Errorf("Require(gopls) without mise = %v, want the PATH binary", err)
	}

	_, err = res.Install(t.Context(), InstallRequest{Name: "pyright"}, opts)
	var unavailable *MiseUnavailableError
	if !errorsAs(err, &unavailable) {
		t.Fatalf("install without mise = %v, want a *MiseUnavailableError", err)
	}
	if got := exitCodeOf(t, err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	// It says what to do instead, since lightspeed will not download.
	for _, want := range []string{"install mise", "pyright-langserver"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	// doctor grades the same situation as a warning, not a failure.
	report, err := Doctor(t.Context(), nil, opts)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	for _, c := range report.Checks {
		if c.ID == CheckMise && c.Severity != SeverityWarn {
			t.Errorf("mise check = %v, want a warning", c)
		}
	}
}
