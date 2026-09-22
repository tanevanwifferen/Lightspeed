package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// The raw end-to-end test needs a language server subprocess without
// touching the network or the host system: when this env var is set,
// the test binary re-execs as the fake server (PLAN §7 tests item b).
const fakeServerModeEnv = "LIGHTSPEED_TEST_FAKESERVER"

func TestMain(m *testing.M) {
	// The auto-spawned daemon is this binary re-executed as
	// `daemon serve ...` (daemon_test.go). It has to be recognised
	// before the fake-server mode below, because a test that set
	// fakeServerModeEnv passes it on to the daemon it starts, and the
	// language server the daemon starts in turn must still be the fake.
	if len(os.Args) > 1 && os.Args[1] == "daemon" {
		os.Exit(Main(os.Args[1:], os.Stdout, os.Stderr))
	}
	if os.Getenv(fakeServerModeEnv) == "1" {
		// runFakeServer (scenario_test.go) picks the script from the
		// environment; with no scenario set it is the M0 fixed one.
		os.Exit(runFakeServer())
	}
	quickRaceExit()
	os.Exit(runTests(m))
}

// quickRaceExit makes the processes this binary starts — the fake language
// server, the daemon — leave at once when built with -race. The race runtime
// sleeps atexit_sleep_ms (1s by default) before a process exits, so that its
// report can be flushed, and every command in this package waits for the server
// it started to exit: under -race that was a second per session, and
// `go test -race ./internal/cli` took five minutes to run a suite that takes
// twenty seconds without. The runtime read GORACE when this process started, so
// this reaches the children only; races are still reported.
func quickRaceExit() {
	os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
}

// useFakeServer points the raw command's server resolution at this
// test binary in fake-server mode.
func useFakeServer(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	useServerCommand(t, exe)
	t.Setenv(fakeServerModeEnv, "1")
}

// runMain runs the CLI in-process and returns exit code and streams.
//
// The streams are safeBuffers, not bytes.Buffers: the CLI hands its
// stderr to os/exec as the language server's stderr, and for a writer
// that is not an *os.File os/exec copies through a goroutine. A
// bytes.Buffer loses concurrent writes outright there — ReadFrom
// truncates the buffer before its blocking read and restores the
// length afterwards — so the CLI's own diagnostics would vanish.
func runMain(args ...string) (code int, stdout, stderr string) {
	var out, errOut safeBuffer
	code = Main(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func decodeEnvelope(t *testing.T, stdout string) render.Envelope {
	t.Helper()
	var env render.Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\nstdout: %q", err, stdout)
	}
	return env
}

// TestRawEcho proves `lightspeed raw` end to end: spawn server,
// initialize handshake, request, envelope on stdout. The params
// include a non-BMP character so UTF-8 payloads survive the trip.
func TestRawEcho(t *testing.T) {
	useFakeServer(t)

	params := `{"msg":"hello 𐐀 world","n":42}`
	code, stdout, stderr := runMain("raw", fakeserver.EchoMethod, "--params", params)
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}

	env := decodeEnvelope(t, stdout)
	if env.Version != render.EnvelopeVersion {
		t.Errorf("envelope version = %d, want %d", env.Version, render.EnvelopeVersion)
	}
	if !env.OK {
		t.Fatalf("envelope ok = false, error: %+v", env.Error)
	}

	data, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("data is not an object: %v (data: %s)", err, data)
	}
	if err := json.Unmarshal([]byte(params), &want); err != nil {
		t.Fatal(err)
	}
	if got["msg"] != want["msg"] || got["n"] != want["n"] {
		t.Errorf("echoed data = %v, want %v", got, want)
	}
}

// TestRawInitialize checks the special case: `raw initialize` returns
// the handshake's own result instead of initializing twice.
func TestRawInitialize(t *testing.T) {
	useFakeServer(t)

	code, stdout, stderr := runMain("raw", "initialize")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false, error: %+v", env.Error)
	}
	if !strings.Contains(stdout, "lightspeed-fakeserver") {
		t.Errorf("initialize result does not contain serverInfo: %s", stdout)
	}
}

// TestRawServerError checks that a server-reported JSON-RPC error
// becomes an ok:false envelope with a machine code, not a crash.
func TestRawServerError(t *testing.T) {
	useFakeServer(t)

	code, stdout, _ := runMain("raw", "no/such/method")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d", code, ExitProblems)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK {
		t.Fatal("envelope ok = true, want false")
	}
	if env.Error == nil || env.Error.Code != "server_error" {
		t.Errorf("error = %+v, want code server_error", env.Error)
	}
}

// TestRawNoServer checks the exit-3 path when the server binary does
// not exist.
func TestRawNoServer(t *testing.T) {
	useServerCommand(t, "lightspeed-no-such-server-binary")
	t.Setenv("PATH", t.TempDir()) // nothing to find, and no mise to ask

	code, stdout, _ := runMain("raw", "x/y")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d", code, ExitNoServer)
	}
	env := decodeEnvelope(t, stdout)
	// Since M3 raw routes like every other command, so a missing
	// binary is reported like every other command reports it.
	if env.OK || env.Error == nil || env.Error.Code != "server_not_installed" {
		t.Errorf("envelope = %+v, want ok:false code server_not_installed", env)
	}
}

// TestRawUsage checks the exit-2 path and its envelope.
func TestRawUsage(t *testing.T) {
	code, stdout, _ := runMain("raw")
	if code != ExitUsage {
		t.Fatalf("exit code = %d, want %d", code, ExitUsage)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "usage" {
		t.Errorf("envelope = %+v, want ok:false code usage", env)
	}

	if code, _, _ := runMain("raw", fakeserver.EchoMethod, "--params", "{not json"); code != ExitUsage {
		t.Errorf("invalid --params: exit code = %d, want %d", code, ExitUsage)
	}
}

// viaDaemonEnv makes the whole suite run through the auto-spawned
// daemon instead of in process: `LIGHTSPEED_TEST_VIA_DAEMON=1 go test
// ./internal/cli`. Every command test then doubles as a parity test
// between the two modes, which is what PLAN §8 M3's "byte-identical"
// claim needs and no hand-picked list of cases can give. It is opt-in
// because it starts a daemon per test workspace.
const viaDaemonEnv = "LIGHTSPEED_TEST_VIA_DAEMON"

// suiteRuntimeDir is the runtime directory the via-daemon suite keeps
// its sockets in; empty when the suite runs in process.
var suiteRuntimeDir string

// retireDaemons stops every daemon this test binary has started, and returns
// only when their *processes* have exited.
//
// A scenario is configured through the environment of a server
// *process*, so a warm server keeps answering from the scenario it was
// born under. That is exactly right for a daemon and exactly wrong for
// two subtests that share a workspace and apply different scenarios, so
// applying a scenario retires whatever the previous one started. It
// only ever touches suiteRuntimeDir: never a developer's own daemons.
//
// Waiting for the socket to disappear is not waiting for the daemon: it
// unpublishes the socket first and then, for a while longer, shuts its
// language servers down — which write to the spawn log and the trace in the
// test's temporary directory. A test that returned on the socket alone had
// t.TempDir's RemoveAll racing those writes, and lost with "directory not
// empty". A daemon that left on its own idle timeout has no socket to find
// at all, so the processes are waited for by asking the daemon package which
// ones this binary started.
func retireDaemons(t *testing.T) {
	t.Helper()
	if suiteRuntimeDir == "" {
		return
	}
	sockets, _ := filepath.Glob(filepath.Join(suiteRuntimeDir, daemon.RuntimeDirName, "*.sock"))
	for _, socket := range sockets {
		c, err := daemon.Dial(context.Background(), daemon.ClientOptions{Socket: socket, NoSpawn: true})
		if err != nil {
			continue
		}
		_ = c.Stop(context.Background())
		_ = c.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := daemon.WaitChildren(ctx); err != nil {
		t.Errorf("a daemon outlived its test: %v", err)
	}
}

// runTests sets the mode the suite runs in and runs it.
//
// By default every test runs in process, as they did before M3:
// hermetic, and nothing left running when the test binary exits. The
// daemon tests (daemon_test.go) opt back in explicitly.
func runTests(m *testing.M) int {
	// A developer's own servers.d, or an exported LIGHTSPEED_OFFLINE, must
	// not decide what a hermetic test resolves: point the user layer at an
	// empty directory, which each test then fills as it needs
	// (useServerCommand, useConfigDir).
	isolated, err := os.MkdirTemp("", "lsconf")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(isolated)
	os.Setenv(serverdef.EnvConfigDir, isolated)
	os.Unsetenv(serverdef.EnvOffline)
	// The workspace index persists under $XDG_CACHE_HOME; a test must not read
	// or write the developer's.
	cache, err := os.MkdirTemp("", "lscache")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(cache)
	os.Setenv("XDG_CACHE_HOME", cache)

	if os.Getenv(viaDaemonEnv) != "1" {
		os.Setenv(noDaemonEnv, "1")
		return m.Run()
	}
	// A short directory: a unix socket path is limited to ~100 bytes,
	// and t.TempDir() names are long.
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	suiteRuntimeDir = dir
	os.Setenv(noDaemonEnv, "0")
	os.Setenv("XDG_RUNTIME_DIR", dir)
	// Daemons started by tests that have finished should not linger.
	os.Setenv(daemonTimeoutEnv, "2s")
	code := m.Run()
	// A daemon that is still shutting down when the binary exits would be
	// writing into a directory the deferred RemoveAll above is deleting.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := daemon.WaitChildren(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}
