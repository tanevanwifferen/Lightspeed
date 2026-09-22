package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// The hermetic machine the M4 tests run on: its own user configuration
// directory, its own PATH of fake executables, its own workspace files.
// There is deliberately no environment variable that overrides the server
// command (that scaffolding was removed, docs/DECISIONS.md D18); a test that
// wants the fake language server says so the way a user would, in a
// configuration layer.

// useConfigDir points the user layer at a fresh, empty directory and
// returns it.
func useConfigDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(serverdef.EnvConfigDir, dir)
	return dir
}

// writeUserServer writes one file of the user layer, $CONFIG/servers.d.
func writeUserServer(t *testing.T, configDir, file, content string) string {
	t.Helper()
	dir := filepath.Join(configDir, serverdef.ServersDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, file)
	write(t, path, content)
	return path
}

// commandOverride is the TOML fragment that changes only a definition's
// command: everything else — activation, install spec — stays the
// generated default's, which is the point of key-by-key layering.
func commandOverride(name string, argv ...string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = strconv.Quote(a)
	}
	return fmt.Sprintf("schema_version = 1\nname = %q\n\n[server]\ncommand = [%s]\n", name, strings.Join(quoted, ", "))
}

// useServerCommand runs every built-in server as argv, through the user
// layer. It is the successor of the M0 LIGHTSPEED_SERVER_CMD override:
// same effect for a test, but through the mechanism a user has.
func useServerCommand(t *testing.T, argv ...string) string {
	t.Helper()
	dir := useConfigDir(t)
	for _, name := range serverdef.BuiltinNames() {
		writeUserServer(t, dir, name+".toml", commandOverride(name, argv...))
	}
	return dir
}

// shellScript writes an executable POSIX shell script and returns its
// path. The M4 tests that need an executable which is not the test binary
// itself — a fake mise, a launcher that records who started it — use one.
func shellScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX shell")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeServerScript is a language-server "executable" that records its
// tag in log and then becomes the scripted fake server. Which tag ended up
// in the log is which definition won.
func fakeServerScript(t *testing.T, dir, name, tag, log string) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return shellScript(t, dir, name,
		fmt.Sprintf("echo %s >> %s\nexec %s \"$@\"\n", tag, strconv.Quote(log), strconv.Quote(exe)))
}

// fakeMise puts a `mise` on PATH — and nothing else does — that records
// every invocation in the returned log, and answers the three calls
// lightspeed makes. useExit is what `mise use` exits with.
func fakeMise(t *testing.T, useExit int) (calls func() []string) {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(t.TempDir(), "mise.log")
	shellScript(t, bin, "mise", fmt.Sprintf(`echo "$@" >> %s
case "$1" in
  --version) echo "2026.1.0 linux-x64" ;;
  which) echo "/fake/mise/bin/$2" ;;
  use) echo "mise: installed $3 $4"; exit %d ;;
esac
`, strconv.Quote(log), useExit))
	t.Setenv("PATH", bin)
	return func() []string {
		data, err := os.ReadFile(log)
		if err != nil {
			return nil
		}
		return strings.Split(strings.TrimSpace(string(data)), "\n")
	}
}

// decodeData decodes an envelope's data into v.
func decodeData(t *testing.T, stdout string, v any) rawEnvelope {
	t.Helper()
	var env rawEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("stdout is not a JSON envelope: %v\n%s", err, stdout)
	}
	if v != nil {
		if err := json.Unmarshal(env.Data, v); err != nil {
			t.Fatalf("decoding data: %v\n%s", err, stdout)
		}
	}
	return env
}

// rawEnvelope is the envelope with its data left raw, for tests that
// decode the payload into their own shape.
type rawEnvelope struct {
	Version  int             `json:"version"`
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data"`
	Warnings []string        `json:"warnings"`
	Error    *struct {
		Code    string         `json:"code"`
		Message string         `json:"message"`
		Data    map[string]any `json:"data"`
	} `json:"error"`
}
