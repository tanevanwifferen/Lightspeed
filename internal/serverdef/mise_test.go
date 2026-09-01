package serverdef

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSplitSpecAndWithVersion(t *testing.T) {
	tests := []struct {
		spec        string
		tool        string
		version     string
		withVersion string
	}{
		{"rust-analyzer", "rust-analyzer", "", "rust-analyzer@1.2.3"},
		{"npm:pyright", "npm:pyright", "", "npm:pyright@1.2.3"},
		{"go:golang.org/x/tools/gopls@v0.23.0", "go:golang.org/x/tools/gopls", "v0.23.0", "go:golang.org/x/tools/gopls@1.2.3"},
		// The leading @ of a scoped npm package is not a version.
		{"npm:@vtsls/language-server", "npm:@vtsls/language-server", "", "npm:@vtsls/language-server@1.2.3"},
		{"npm:@vtsls/language-server@0.2.9", "npm:@vtsls/language-server", "0.2.9", "npm:@vtsls/language-server@1.2.3"},
		{"ubi:clangd/clangd", "ubi:clangd/clangd", "", "ubi:clangd/clangd@1.2.3"},
		{"ubi:clangd/clangd@20.1.0", "ubi:clangd/clangd", "20.1.0", "ubi:clangd/clangd@1.2.3"},
	}
	for _, tt := range tests {
		tool, version := splitSpec(tt.spec)
		if tool != tt.tool || version != tt.version {
			t.Errorf("splitSpec(%q) = (%q, %q), want (%q, %q)", tt.spec, tool, version, tt.tool, tt.version)
		}
		if got := withVersion(tt.spec, "1.2.3"); got != tt.withVersion {
			t.Errorf("withVersion(%q) = %q, want %q", tt.spec, got, tt.withVersion)
		}
	}
}

func TestDetectMise(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14 linux-x64 (2026-08-26)", nil)

	// Not on PATH.
	got := DetectMise(t.Context(), e.Options())
	if got.Available || !strings.Contains(got.Problem, "not on PATH") {
		t.Errorf("status = %+v, want unavailable because it is not on PATH", got)
	}

	path := e.Binary("mise")
	got = DetectMise(t.Context(), e.Options())
	if !got.Available {
		t.Fatalf("status = %+v, want available", got)
	}
	if got.Path != path {
		t.Errorf("path = %q, want %q", got.Path, path)
	}
	if got.Version != "2026.8.14 linux-x64 (2026-08-26)" {
		t.Errorf("version = %q, want the first line of `mise --version`", got.Version)
	}
	if !strings.Contains(got.String(), "2026.8.14") {
		t.Errorf("String() = %q", got.String())
	}
}

func TestDetectMiseThatFails(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	e.Runner = func(context.Context, string, ...string) (string, string, error) {
		return "", "mise: broken install\n", &exitStatus{code: 1}
	}
	got := DetectMise(t.Context(), e.Options())
	if got.Available {
		t.Fatalf("status = %+v, want unavailable", got)
	}
	if !strings.Contains(got.Problem, "broken install") {
		t.Errorf("problem = %q, want mise's own words", got.Problem)
	}
}

func TestPlanInstall(t *testing.T) {
	e := newTestEnv(t)
	res, err := Load(e.Options())
	if err != nil {
		t.Fatal(err)
	}

	plan, err := res.PlanInstall("gopls", "")
	if err != nil {
		t.Fatalf("PlanInstall() = %v", err)
	}
	if got, want := plan.Use, []string{"mise", "use", "-g", "go:golang.org/x/tools/gopls@v0.23.0"}; !slices.Equal(got, want) {
		t.Errorf("Use = %v, want %v", got, want)
	}
	if got, want := plan.Which, []string{"mise", "which", "gopls"}; !slices.Equal(got, want) {
		t.Errorf("Which = %v, want %v", got, want)
	}
	if plan.String() != strings.Join(plan.Use, " ") {
		t.Errorf("String() = %q", plan.String())
	}

	plan, err = res.PlanInstall("gopls", "v0.24.1")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Spec != "go:golang.org/x/tools/gopls@v0.24.1" {
		t.Errorf("Spec = %q, want the requested version", plan.Spec)
	}

	if _, err := res.PlanInstall("nope", ""); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("PlanInstall(nope) = %v, want ErrNoSuchServer", err)
	}
}

func TestPlanInstallWithoutSpec(t *testing.T) {
	e := newTestEnv(t)
	e.WriteWorkspace(`schema_version = 1

[servers.homegrown]
[servers.homegrown.activation]
languages = ["fictional"]

[servers.homegrown.server]
command = ["homegrown-ls"]
`)
	res, err := Load(e.Options())
	if err != nil {
		t.Fatal(err)
	}
	_, err = res.PlanInstall("homegrown", "")
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("PlanInstall() = %v, want ErrNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "no install.mise spec") {
		t.Errorf("error = %q, want it to say there is nothing to delegate", err)
	}
	// And it does not invent a package name.
	if strings.Contains(err.Error(), "mise use") {
		t.Errorf("error = %q, want no guessed install command", err)
	}
}

func TestInstallDryRun(t *testing.T) {
	e := newTestEnv(t)
	// No runner: a dry run must not execute anything, not even mise
	// detection's --version.
	opts := e.Options()
	opts.SkipMise = true
	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	result, err := res.Install(t.Context(), InstallRequest{Name: "pyright", DryRun: true}, opts)
	if err != nil {
		t.Fatalf("Install(dry run) = %v", err)
	}
	if result.Ran {
		t.Error("Ran = true for a dry run")
	}
	if got, want := result.Plan.Use, []string{"mise", "use", "-g", "npm:pyright"}; !slices.Equal(got, want) {
		t.Errorf("plan = %v, want %v", got, want)
	}
}

// TestInstallOffline is PLAN §6's kill switch: refused before anything
// runs, and the message still says what to run when online.
func TestInstallOffline(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	e.Runner = fakeMise("2026.8.14", nil)
	e.Vars[EnvOffline] = "1"

	opts := e.Options()
	res, err := Resolve(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Offline {
		t.Error("resolution does not report offline mode")
	}

	_, err = res.Install(t.Context(), InstallRequest{Name: "pyright"}, opts)
	if !errors.Is(err, ErrOffline) {
		t.Fatalf("Install() = %v, want ErrOffline", err)
	}
	if got := exitCodeOf(t, err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	for _, want := range []string{"mise use -g npm:pyright", EnvOffline} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}

	// The flag alone is enough, without the environment variable.
	e.Vars[EnvOffline] = ""
	opts = e.Options()
	opts.Offline = true
	if _, err := InstallServer(t.Context(), InstallRequest{Name: "pyright"}, opts); !errors.Is(err, ErrOffline) {
		t.Errorf("InstallServer() with --offline = %v, want ErrOffline", err)
	}
}

func TestInstallWithoutMise(t *testing.T) {
	e := newTestEnv(t)
	opts := e.Options()
	_, err := InstallServer(t.Context(), InstallRequest{Name: "pyright"}, opts)
	if !errors.Is(err, ErrMiseUnavailable) {
		t.Fatalf("InstallServer() = %v, want ErrMiseUnavailable", err)
	}
	if got := exitCodeOf(t, err); got != 3 {
		t.Errorf("exit code = %d, want 3", got)
	}
	for _, want := range []string{"mise", "pyright-langserver"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

func TestInstallDelegatesToMise(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	installed := filepath.Join(t.TempDir(), "pyright-langserver")
	if err := os.WriteFile(installed, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var ran [][]string
	base := fakeMise("2026.8.14", map[string]string{"pyright-langserver": installed})
	e.Runner = func(ctx context.Context, name string, args ...string) (string, string, error) {
		ran = append(ran, append([]string{filepath.Base(name)}, args...))
		return base(ctx, name, args...)
	}

	opts := e.Options()
	result, err := InstallServer(t.Context(), InstallRequest{Name: "pyright"}, opts)
	if err != nil {
		t.Fatalf("InstallServer() = %v", err)
	}
	if !result.Ran {
		t.Error("Ran = false")
	}
	if result.Path != installed {
		t.Errorf("Path = %q, want %q from `mise which`", result.Path, installed)
	}
	if len(result.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", result.Warnings)
	}
	if !strings.Contains(result.Output, "npm:pyright") {
		t.Errorf("Output = %q, want mise's own words", result.Output)
	}

	// Exactly the two invocations PLAN §1 describes, plus the version
	// probe: nothing else, and nothing that downloads on its own.
	want := [][]string{
		{"mise", "--version"},
		{"mise", "use", "-g", "npm:pyright"},
		{"mise", "which", "pyright-langserver"},
	}
	if len(ran) != len(want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
	for i := range want {
		if !slices.Equal(ran[i], want[i]) {
			t.Errorf("invocation %d = %v, want %v", i, ran[i], want[i])
		}
	}
}

func TestInstallFailureCarriesMiseOutput(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	e.Runner = func(_ context.Context, _ string, args ...string) (string, string, error) {
		if args[0] == "--version" {
			return "2026.8.14\n", "", nil
		}
		return "", "mise ERROR no such tool: npm:pyright\n", &exitStatus{code: 1}
	}

	_, err := InstallServer(t.Context(), InstallRequest{Name: "pyright"}, e.Options())
	if !errors.Is(err, ErrInstallFailed) {
		t.Fatalf("InstallServer() = %v, want ErrInstallFailed", err)
	}
	if got := exitCodeOf(t, err); got != 4 {
		t.Errorf("exit code = %d, want 4", got)
	}
	if !strings.Contains(err.Error(), "no such tool") {
		t.Errorf("error = %q, want mise's own diagnosis", err)
	}
}

func TestInstallWarnsWhenWhichFails(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	e.Runner = fakeMise("2026.8.14", nil) // `which` finds nothing

	result, err := InstallServer(t.Context(), InstallRequest{Name: "pyright"}, e.Options())
	if err != nil {
		t.Fatalf("InstallServer() = %v", err)
	}
	if result.Path != "" {
		t.Errorf("Path = %q, want empty", result.Path)
	}
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "which") {
		t.Errorf("warnings = %v, want one about `mise which`", result.Warnings)
	}
}

// TestExecRunner covers the production runner, using the Go toolchain's
// own binary as a command that certainly exists.
func TestExecRunner(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh available")
	}
	stdout, stderr, err := execRunner(t.Context(), sh, "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("execRunner() = %v", err)
	}
	if strings.TrimSpace(stdout) != "out" || strings.TrimSpace(stderr) != "err" {
		t.Errorf("stdout = %q, stderr = %q", stdout, stderr)
	}
	if _, _, err := execRunner(t.Context(), sh, "-c", "exit 3"); err == nil {
		t.Error("execRunner() reported success for a failing command")
	}
	if got := trimOutput(stdout, stderr); got != "out\nerr" {
		t.Errorf("trimOutput() = %q, want %q", got, "out\nerr")
	}
}

// TestProbeServerIsTargeted: the hot path asks mise about one server,
// not about all six.
func TestProbeServerIsTargeted(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("mise")
	var ran [][]string
	base := fakeMise("2026.8.14", nil)
	e.Runner = func(ctx context.Context, name string, args ...string) (string, string, error) {
		ran = append(ran, args)
		return base(ctx, name, args...)
	}
	opts := e.Options()
	res, err := Load(opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := res.ProbeServer(t.Context(), "gopls", opts); err != nil {
		t.Fatalf("ProbeServer() = %v", err)
	}
	want := [][]string{{"--version"}, {"which", "gopls"}}
	if len(ran) != len(want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
	// The others are still unprobed, and Require says so rather than
	// reporting them as missing.
	if _, err := res.Require("pyright"); err == nil || !strings.Contains(err.Error(), "has not been probed") {
		t.Errorf("Require(pyright) = %v, want a refusal to guess", err)
	}
	if _, err := res.ProbeServer(t.Context(), "nope", opts); !errors.Is(err, ErrNoSuchServer) {
		t.Errorf("ProbeServer(nope) = %v, want ErrNoSuchServer", err)
	}
}
