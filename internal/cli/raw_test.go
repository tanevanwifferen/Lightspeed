package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The raw end-to-end test needs a language server subprocess without
// touching the network or the host system: when this env var is set,
// the test binary re-execs as the fake server (PLAN §7 tests item b).
const fakeServerModeEnv = "LIGHTSPEED_TEST_FAKESERVER"

func TestMain(m *testing.M) {
	if os.Getenv(fakeServerModeEnv) == "1" {
		// runFakeServer (scenario_test.go) picks the script from the
		// environment; with no scenario set it is the M0 fixed one.
		os.Exit(runFakeServer())
	}
	os.Exit(m.Run())
}

// useFakeServer points the raw command's server resolution at this
// test binary in fake-server mode.
func useFakeServer(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverCommandEnv, exe)
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
	t.Setenv(serverCommandEnv, "lightspeed-no-such-server-binary")

	code, stdout, _ := runMain("raw", "x/y")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d", code, ExitNoServer)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "no_server" {
		t.Errorf("envelope = %+v, want ok:false code no_server", env)
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
