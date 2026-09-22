package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// A broken mise shim used to be reported `installed ... (path)`, severity
// ok, while every query failed: gopls 0.23.0 was installed in mise, no
// version was active for the directory, and the shim on PATH printed
// `mise ERROR No version is set for shim: gopls` and exited. These tests
// build that machine — a shim, and a fake mise that answers what the real
// one answered — and go through the command line.

const goplsMiseTool = "go:golang.org/x/tools/gopls"

// shimMachine is a PATH with a mise shim for gopls and a fake mise.
// installed says whether mise has gopls 0.23.0 on disk (not active); its
// binary is a fake language server that logs "real" when it is started. It
// returns the workspace file, that binary and the log.
func shimMachine(t *testing.T, installed bool) (file, real, log string) {
	t.Helper()
	useConfigDir(t)
	_, file = cjkFixture(t)
	scenario{
		ownServers: true,
		results:    map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}},
	}.apply(t)

	root := t.TempDir()
	log = filepath.Join(t.TempDir(), "started.log")
	real = fakeServerScript(t, filepath.Join(root, "installs", "gopls", "0.23.0", "bin"), "gopls", "real", log)
	shims := filepath.Join(root, "mise", "shims")
	shellScript(t, shims, "gopls", "echo 'mise ERROR No version is set for shim: gopls' >&2\nexit 1\n")

	list := "[]"
	if installed {
		list = `[{"version":"0.23.0","installed":true,"active":false}]`
	}
	bin := filepath.Join(root, "bin")
	shellScript(t, bin, "mise", `case "$1" in
  --version) echo "2026.9.7 linux-x64" ;;
  -C) shift 2
      echo "mise ERROR $2 is a mise bin however it is not currently active. Use `+"`mise use`"+` to activate it in this directory." >&2
      exit 1 ;;
  ls) echo '`+list+`' ;;
  which) if [ "$2" = "--tool" ]; then echo "`+real+`"; else exit 1; fi ;;
  *) exit 2 ;;
esac
`)
	t.Setenv("PATH", shims+string(os.PathListSeparator)+bin)
	return file, real, log
}

type doctorChecksJSON struct {
	Worst  string `json:"worst"`
	Checks []struct {
		ID       string   `json:"id"`
		Severity string   `json:"severity"`
		Subject  string   `json:"subject"`
		Message  string   `json:"message"`
		Detail   string   `json:"detail"`
		Fix      []string `json:"fix"`
	} `json:"checks"`
}

func (d doctorChecksJSON) binaryCheck(t *testing.T, server string) (severity, message, detail string, fix []string) {
	t.Helper()
	for _, c := range d.Checks {
		if c.ID == serverdef.CheckServerBinary && c.Subject == server {
			return c.Severity, c.Message, c.Detail, c.Fix
		}
	}
	t.Fatalf("doctor has no server_binary check for %s: %+v", server, d.Checks)
	return
}

// TestBrokenMiseShimLaunchesTheInstalledVersion: the shim does not
// resolve, mise has the version, so that binary is what runs, and every
// report says so instead of "installed at the shim".
func TestBrokenMiseShimLaunchesTheInstalledVersion(t *testing.T) {
	file, real, log := shimMachine(t, true)

	code, stdout, stderr := runMain("definition", file+":3:5")
	if code != ExitOK {
		t.Fatalf("definition: exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if data, _ := os.ReadFile(log); strings.TrimSpace(string(data)) != "real" {
		t.Errorf("started %q, want mise's installed gopls (%s)", data, real)
	}

	_, servers := runServers(t, "--path", filepath.Dir(file))
	gopls := servers.server(t, "gopls")
	if !gopls.Installed || gopls.Binary.Path != real || gopls.Binary.Source != "mise" {
		t.Fatalf("servers: gopls = %+v, want installed at %s from mise", gopls.Binary, real)
	}
	if !strings.Contains(gopls.Binary.Note, "mise shim") || !strings.Contains(gopls.Binary.Note, "no active version") {
		t.Errorf("provenance note = %q, want it to say the shim has no active version", gopls.Binary.Note)
	}
	if want := "mise use -g " + goplsMiseTool + "@0.23.0"; strings.Join(gopls.InstallCommand, " ") != want {
		t.Errorf("install command = %v, want %q", gopls.InstallCommand, want)
	}
	_, text, _ := runMain("servers", "--path", filepath.Dir(file), "--format", "text")
	if !strings.Contains(text, "note: the mise shim") {
		t.Errorf("text output does not carry the provenance:\n%s", text)
	}

	code, stdout, _ = runMain("doctor", file)
	var report doctorChecksJSON
	decodeData(t, stdout, &report)
	severity, message, detail, fix := report.binaryCheck(t, "gopls")
	if severity != "warn" || !strings.Contains(message, real) || !strings.Contains(detail, "no active version") ||
		strings.Join(fix, " ") != "mise use -g "+goplsMiseTool+"@0.23.0" {
		t.Errorf("doctor gopls = %s %q / %q / %v, want a warning naming the launched binary and the fix", severity, message, detail, fix)
	}
}

// TestBrokenMiseShimWithNothingInstalledExitsThree: neither active nor
// installed is a server that cannot be used — exit 3 with the exact
// command, no server process started, and `servers` and `doctor` agree.
func TestBrokenMiseShimWithNothingInstalledExitsThree(t *testing.T) {
	file, _, log := shimMachine(t, false)
	want := "mise use -g " + goplsMiseTool + "@v0.23.0"

	code, stdout, _ := runMain("definition", file+":3:5")
	if code != ExitNoServer {
		t.Fatalf("definition: exit %d, want %d\n%s", code, ExitNoServer, stdout)
	}
	env := decodeData(t, stdout, nil)
	if env.Error == nil || env.Error.Code != "server_not_installed" {
		t.Fatalf("error = %+v, want server_not_installed", env.Error)
	}
	if !strings.Contains(env.Error.Message, "mise shim") || !strings.Contains(env.Error.Message, want) || env.Error.Data["install"] != want {
		t.Errorf("error = %+v, want the shim named and the exact command %q", env.Error, want)
	}
	if data, _ := os.ReadFile(log); len(data) != 0 {
		t.Errorf("a server was started (%q) for a shim that cannot run", data)
	}

	_, servers := runServers(t, "--path", filepath.Dir(file))
	if gopls := servers.server(t, "gopls"); gopls.Installed || gopls.Binary.Source != "unusable" || !strings.Contains(gopls.Binary.Problem, "mise shim") {
		t.Errorf("servers: gopls = %+v, want unusable", gopls)
	}
	_, text, _ := runMain("servers", "--path", filepath.Dir(file), "--format", "text")
	if !strings.Contains(text, "unusable") || !strings.Contains(text, want) {
		t.Errorf("text output:\n%s", text)
	}

	code, stdout, _ = runMain("doctor", file)
	var report doctorChecksJSON
	decodeData(t, stdout, &report)
	severity, _, _, fix := report.binaryCheck(t, "gopls")
	if severity != "error" || report.Worst != "error" || code != ExitProblems || strings.Join(fix, " ") != want {
		t.Errorf("doctor: gopls %s, worst %s, exit %d, fix %v; want an error with %q", severity, report.Worst, code, fix, want)
	}
}

// TestDoctorStartsWhatItSniffs is the generic rule behind the shim fix: a
// binary that is on PATH and executable is not thereby usable. A launcher
// that dies for a missing library is an error in `doctor` and not
// "installed" in `servers`; one that merely rejects --version, as gopls
// does, is fine.
func TestDoctorStartsWhatItSniffs(t *testing.T) {
	useConfigDir(t)
	work := t.TempDir()
	broken := t.TempDir()
	shellScript(t, broken, "gopls", "echo 'gopls: error while loading shared libraries: libc.so.9' >&2\nexit 127\n")
	strict := t.TempDir()
	shellScript(t, strict, "rust-analyzer", "echo 'flag provided but not defined: -version' >&2\nexit 2\n")
	t.Setenv("PATH", broken+string(os.PathListSeparator)+strict)

	_, servers := runServers(t, "--path", work)
	if g := servers.server(t, "gopls"); g.Installed || g.Binary.StartProbe != "failed" || !strings.Contains(g.Binary.Problem, "shared libraries") {
		t.Errorf("gopls = %+v, want it reported as unable to start", g)
	}
	if ra := servers.server(t, "rust-analyzer"); !ra.Installed || ra.Binary.StartProbe != "started" {
		t.Errorf("rust-analyzer = %+v, want installed: it ran, it just has no --version", ra)
	}

	goFile := filepath.Join(work, "x.go")
	write(t, filepath.Join(work, "go.mod"), "module x\n")
	write(t, goFile, "package x\n")
	code, stdout, _ := runMain("doctor", goFile)
	var report doctorChecksJSON
	decodeData(t, stdout, &report)
	if severity, _, _, _ := report.binaryCheck(t, "gopls"); severity != "error" || code != ExitProblems {
		t.Errorf("doctor: gopls %s, exit %d; want an error", severity, code)
	}
	if severity, _, _, _ := report.binaryCheck(t, "rust-analyzer"); severity != "ok" {
		t.Errorf("doctor: rust-analyzer %s, want ok", severity)
	}
}
