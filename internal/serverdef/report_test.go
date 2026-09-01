package serverdef

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// checkFor finds the check with the given id and subject.
func checkFor(t *testing.T, report *DoctorReport, id, subject string) Check {
	t.Helper()
	for _, c := range report.Checks {
		if c.ID == id && c.Subject == subject {
			return c
		}
	}
	var have []string
	for _, c := range report.Checks {
		have = append(have, c.ID+"/"+c.Subject)
	}
	t.Fatalf("no %s check for %q; have %v", id, subject, have)
	return Check{}
}

// TestServersReport is `lightspeed servers`: what is configured, what
// resolved, from which layer, installed or not.
func TestServersReport(t *testing.T) {
	e := newTestEnv(t)
	e.Binary("gopls")
	e.Binary("mise")
	e.Runner = fakeMise("2026.8.14", nil)
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 90\n")

	report, err := Servers(t.Context(), e.Options())
	if err != nil {
		t.Fatalf("Servers() = %v", err)
	}
	if got, want := len(report.Servers), len(BuiltinNames()); got != want {
		t.Fatalf("got %d servers, want %d", got, want)
	}
	if report.Installed() != 1 {
		t.Errorf("Installed() = %d, want 1 (only gopls is on the test PATH)", report.Installed())
	}
	if !report.Mise.Available {
		t.Errorf("mise = %+v, want available", report.Mise)
	}
	if report.Offline {
		t.Error("Offline = true, want false")
	}
	if len(report.Problems) != 0 {
		t.Errorf("problems = %v, want none", report.Problems)
	}

	var gopls, pyright ServerStatus
	for _, s := range report.Servers {
		switch s.Name {
		case "gopls":
			gopls = s
		case "pyright":
			pyright = s
		}
	}
	if !gopls.Installed || gopls.Binary.Source != BinaryPATH {
		t.Errorf("gopls = %+v, want installed from PATH", gopls.Binary)
	}
	if gopls.Origin.Layer != LayerUser {
		t.Errorf("gopls origin = %v, want the user layer", gopls.Origin)
	}
	if len(gopls.Shadowed) != 1 {
		t.Errorf("gopls shadowed = %v, want the built-in it overrode", gopls.Shadowed)
	}
	if gopls.Def.Activation.Priority != 90 {
		t.Errorf("gopls priority = %d, want 90", gopls.Def.Activation.Priority)
	}
	if pyright.Installed {
		t.Errorf("pyright = %+v, want not installed", pyright.Binary)
	}
	if got, want := pyright.InstallCommand, []string{"mise", "use", "-g", "npm:pyright"}; !slices.Equal(got, want) {
		t.Errorf("pyright install command = %v, want %v", got, want)
	}

	// The report survives the JSON envelope, which is how an agent will
	// actually read it.
	blob, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	for _, want := range []string{`"layer":2`, `"installed":true`, `"install_command"`, `"origin"`} {
		if !strings.Contains(string(blob), want) {
			t.Errorf("JSON does not contain %s: %s", want, blob)
		}
	}
}

// TestServersReportSurvivesBrokenConfig: the command whose job is to
// explain the current state must still explain it when the state is bad.
func TestServersReportSurvivesBrokenConfig(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	e.WriteUser("broken.toml", "schema_version = 1\nname = \"x\noops\n")

	report, err := Servers(t.Context(), e.Options())
	if err != nil {
		t.Fatalf("Servers() = %v, want a report rather than a failure", err)
	}
	if len(report.Problems) != 1 {
		t.Fatalf("problems = %v, want one", report.Problems)
	}
	if !strings.Contains(report.Problems[0].Message, "broken.toml") {
		t.Errorf("problem = %q, want it to name the file", report.Problems[0].Message)
	}
	if len(report.Servers) != len(BuiltinNames()) {
		t.Errorf("got %d servers, want the built-ins to still be reported", len(report.Servers))
	}
	// And Load, which the query path uses, refuses outright.
	if _, err := Load(e.Options()); err == nil {
		t.Error("Load() succeeded despite the broken file")
	}
}

// TestDoctorHealthy is the boring case, which still has to be reported:
// every server installed, mise there, nothing offline.
func TestDoctorHealthy(t *testing.T) {
	e := newTestEnv(t)
	for _, name := range []string{"gopls", "clangd", "lua-language-server", "pyright-langserver", "rust-analyzer", "vtsls", "mise"} {
		e.Binary(name)
	}
	e.Runner = fakeMise("2026.8.14", nil)

	report, err := Doctor(t.Context(), nil, e.Options())
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	if !report.OK() {
		for _, c := range report.Checks {
			if c.Severity.Worse(SeverityInfo) {
				t.Errorf("unexpected finding: %s", c)
			}
		}
	}
	if got := report.Worst(); got != SeverityOK && got != SeverityInfo {
		t.Errorf("Worst() = %q, want ok or info", got)
	}
	if c := checkFor(t, report, CheckOffline, ""); c.Severity != SeverityOK {
		t.Errorf("offline check = %s, want ok", c)
	}
	if c := checkFor(t, report, CheckMise, MiseName); c.Severity != SeverityOK {
		t.Errorf("mise check = %s, want ok", c)
	}
	for _, name := range BuiltinNames() {
		if c := checkFor(t, report, CheckServerBinary, name); c.Severity != SeverityOK {
			t.Errorf("%s check = %s, want ok", name, c)
		}
	}
}

// TestDoctorDiagnoses is the list the CLI's `doctor` exists to produce,
// all of it at once: no server for a path, a server on PATH that cannot
// run, a missing server, no mise, a layer override, a broken file and
// offline mode.
func TestDoctorDiagnoses(t *testing.T) {
	e := newTestEnv(t)
	e.UnexecutableBinary("gopls")
	e.Vars[EnvOffline] = "1"
	e.WriteUser("gopls.toml", "schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 90\n")
	e.WriteUser("broken.toml", "schema_version = 1\nname = \"x\noops\n")
	opts := e.Options()
	opts.PathCheck = func(path string) ([]string, error) {
		switch {
		case strings.HasSuffix(path, ".go"):
			return []string{"gopls"}, nil
		case strings.HasSuffix(path, ".bin"):
			return nil, nil
		default:
			return nil, fmt.Errorf("%s: unrecognised file type", path)
		}
	}

	report, err := Doctor(t.Context(), []string{"main.go", "blob.bin", "notes.unknown"}, opts)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	if report.OK() {
		t.Error("OK() = true, but this machine is a mess")
	}
	if report.Worst() != SeverityError {
		t.Errorf("Worst() = %q, want %q", report.Worst(), SeverityError)
	}

	if c := checkFor(t, report, CheckOffline, ""); c.Severity != SeverityInfo || !strings.Contains(c.Message, "offline") {
		t.Errorf("offline check = %s", c)
	}
	if c := checkFor(t, report, CheckMise, MiseName); c.Severity != SeverityWarn {
		t.Errorf("mise check = %s, want a warning that install cannot work", c)
	} else if !strings.Contains(c.Detail, "never downloads") {
		t.Errorf("mise detail = %q, want it to say lightspeed will not download anything itself", c.Detail)
	}

	// gopls: on PATH, not runnable. An error, not a "not installed".
	gopls := checkFor(t, report, CheckServerBinary, "gopls")
	if gopls.Severity != SeverityError {
		t.Errorf("gopls check = %s, want an error", gopls)
	}
	for _, want := range []string{"cannot be run", "not executable"} {
		if !strings.Contains(gopls.Message, want) {
			t.Errorf("gopls message = %q, want it to mention %q", gopls.Message, want)
		}
	}
	if len(gopls.Fix) == 0 {
		t.Error("gopls check carries no fix command")
	}

	// pyright: simply not installed. A warning with the exact command.
	pyright := checkFor(t, report, CheckServerBinary, "pyright")
	if pyright.Severity != SeverityWarn {
		t.Errorf("pyright check = %s, want a warning", pyright)
	}
	if got, want := pyright.Fix, []string{"mise", "use", "-g", "npm:pyright"}; !slices.Equal(got, want) {
		t.Errorf("pyright fix = %v, want %v", got, want)
	}

	// The layers: the override is visible, and the broken file is loud.
	override := checkFor(t, report, CheckLayerOverride, "gopls")
	if !strings.Contains(override.Message, KeyPriority) || !strings.Contains(override.Message, "built-in") {
		t.Errorf("override check = %s, want it to name the key and what it overrode", override)
	}
	var brokenFound bool
	for _, c := range report.Checks {
		if c.ID == CheckConfigFile && strings.Contains(c.Subject, "broken.toml") {
			brokenFound = true
			if c.Severity != SeverityError {
				t.Errorf("broken file check = %s, want an error", c)
			}
		}
	}
	if !brokenFound {
		t.Error("no check reported the broken configuration file")
	}

	// Paths: one handled, one claimed by nobody, one unrecognised.
	if c := checkFor(t, report, CheckPathRouting, "main.go"); c.Severity != SeverityOK {
		t.Errorf("main.go check = %s, want ok", c)
	}
	if c := checkFor(t, report, CheckPathRouting, "blob.bin"); c.Severity != SeverityError {
		t.Errorf("blob.bin check = %s, want an error: nothing handles it", c)
	}
	if c := checkFor(t, report, CheckPathRouting, "notes.unknown"); c.Severity != SeverityError ||
		!strings.Contains(c.Message, "unrecognised file type") {
		t.Errorf("notes.unknown check = %s, want the router's own error", c)
	}
}

func TestDoctorWithoutPathCheck(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	e.Binary("mise")

	report, err := Doctor(t.Context(), []string{"main.go"}, e.Options())
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	c := checkFor(t, report, CheckPathRouting, "main.go")
	if c.Severity != SeverityWarn || !strings.Contains(c.Message, "no router") {
		t.Errorf("check = %s, want a warning that routing was not checked", c)
	}
}

func TestDoctorNoServersConfigured(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	opts := e.Options()
	opts.SkipBuiltins = true
	opts.SkipUser = true

	report, err := Doctor(t.Context(), nil, opts)
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	c := checkFor(t, report, CheckServersConfigured, "")
	if c.Severity != SeverityError {
		t.Errorf("check = %s, want an error", c)
	}
}

func TestDoctorLayerChecks(t *testing.T) {
	e := newTestEnv(t)
	e.Runner = fakeMise("2026.8.14", nil)
	e.WriteWorkspace("schema_version = 1\nname = \"gopls\"\n[activation]\npriority = 90\n")

	report, err := Doctor(t.Context(), nil, e.Options())
	if err != nil {
		t.Fatalf("Doctor() = %v", err)
	}
	ws := checkFor(t, report, CheckConfigLayer, LayerWorkspace.String())
	if !strings.Contains(ws.Message, WorkspaceFile) {
		t.Errorf("workspace layer check = %s, want it to name the file it read", ws)
	}
	user := checkFor(t, report, CheckConfigLayer, LayerUser.String())
	if user.Severity != SeverityInfo || !strings.Contains(user.Message, ServersDir) {
		t.Errorf("user layer check = %s, want an informational note naming the directory", user)
	}
	builtin := checkFor(t, report, CheckConfigLayer, LayerBuiltin.String())
	if !strings.Contains(builtin.Message, "gopls") {
		t.Errorf("builtin layer check = %s, want it to list the defaults", builtin)
	}
}

func TestSeverityOrdering(t *testing.T) {
	order := []Severity{SeverityOK, SeverityInfo, SeverityWarn, SeverityError}
	for i, s := range order {
		for j, other := range order {
			if got, want := s.Worse(other), i > j; got != want {
				t.Errorf("%q.Worse(%q) = %t, want %t", s, other, got, want)
			}
		}
	}
	empty := &DoctorReport{}
	if empty.Worst() != SeverityOK || !empty.OK() {
		t.Errorf("an empty report is not ok: %q", empty.Worst())
	}
}

func TestCheckString(t *testing.T) {
	c := Check{ID: CheckServerBinary, Severity: SeverityWarn, Subject: "pyright", Message: "not installed", Fix: []string{"mise", "use", "-g", "npm:pyright"}}
	want := "[warn] server_binary pyright: not installed — run: mise use -g npm:pyright"
	if got := c.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestOriginAndLayerStrings(t *testing.T) {
	if got, want := (Origin{Layer: LayerBuiltin}).String(), "built-in default"; got != want {
		t.Errorf("builtin origin = %q, want %q", got, want)
	}
	if got := (Origin{Layer: LayerUser, File: "/x/gopls.toml"}).String(); !strings.Contains(got, "/x/gopls.toml") || !strings.Contains(got, "user") {
		t.Errorf("user origin = %q", got)
	}
	if got, want := LayerUnknown.String(), "unknown"; got != want {
		t.Errorf("LayerUnknown = %q, want %q", got, want)
	}
	if got := (Override{Origin: Origin{Layer: LayerBuiltin}, Whole: true}).String(); !strings.Contains(got, "whole definition") {
		t.Errorf("whole override = %q", got)
	}
	if got := (Override{Origin: Origin{Layer: LayerUser, File: "/x"}, Keys: []string{KeyPriority}}).String(); !strings.Contains(got, KeyPriority) {
		t.Errorf("partial override = %q", got)
	}
}

// TestErrorCodes pins the machine-readable codes and exit codes the
// render layer maps (PLAN §4, docs/DECISIONS.md D7).
func TestErrorCodes(t *testing.T) {
	tests := []struct {
		err      error
		code     string
		exit     int
		sentinel error
	}{
		{&ConfigError{Origin: Origin{Layer: LayerUser, File: "/x"}, Err: errors.New("bad")}, "invalid_config", 2, ErrInvalidConfig},
		{&ConflictError{Name: "gopls"}, "config_conflict", 2, ErrInvalidConfig},
		{&NotInstalledError{Name: "gopls"}, "server_not_installed", 3, ErrNotInstalled},
		{&OfflineError{Action: "install gopls"}, "offline", 3, ErrOffline},
		{&MiseUnavailableError{Action: "install gopls"}, "mise_unavailable", 3, ErrMiseUnavailable},
		{&NoSuchServerError{Name: "nope"}, "no_such_server", 2, ErrNoSuchServer},
		{&InstallFailedError{Name: "gopls"}, "install_failed", 4, ErrInstallFailed},
	}
	for _, tt := range tests {
		coder, ok := tt.err.(interface{ Code() string })
		if !ok {
			t.Errorf("%T carries no Code()", tt.err)
			continue
		}
		if got := coder.Code(); got != tt.code {
			t.Errorf("%T.Code() = %q, want %q", tt.err, got, tt.code)
		}
		if got := exitCodeOf(t, tt.err); got != tt.exit {
			t.Errorf("%T.ExitCode() = %d, want %d", tt.err, got, tt.exit)
		}
		if !errors.Is(tt.err, tt.sentinel) {
			t.Errorf("%T is not %v", tt.err, tt.sentinel)
		}
		if tt.err.Error() == "" {
			t.Errorf("%T has an empty message", tt.err)
		}
	}
}

// TestConfigErrorUnwrapsCause keeps errors.As working through the
// layering's wrapper, so a caller can still reach a parse error.
func TestConfigErrorUnwrapsCause(t *testing.T) {
	cause := errors.New("root cause")
	err := error(&ConfigError{Origin: Origin{Layer: LayerUser, File: "/x"}, Err: cause})
	if !errors.Is(err, cause) {
		t.Error("the cause is not reachable through the ConfigError")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Error("ErrInvalidConfig is not reachable through the ConfigError")
	}
}
