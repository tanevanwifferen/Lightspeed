package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// docs/DECISIONS.md D28: a language server that dies while it starts is
// reported with its exit status and the tail of its stderr, in the
// envelope, whether or not a daemon started it. Before, the envelope said
// "initialize: jsonrpc: connection closed" and the reason was only in the
// daemon's log.

// brokenShim is the executable that motivated it: mise's shim for a tool
// with no version set, which complains on stderr and exits.
func brokenShim(t *testing.T) string {
	t.Helper()
	return shellScript(t, t.TempDir(), "gopls",
		"echo 'starting up' >&2\necho 'mise ERROR No version is set for shim: gopls' >&2\nexit 42\n")
}

func TestStartupDeathIsReportedWithExitStatusAndStderr(t *testing.T) {
	for _, mode := range []string{"in process", "daemon"} {
		t.Run(mode, func(t *testing.T) {
			dir, file := cjkFixture(t)
			useServerCommand(t, brokenShim(t))
			args := []string{"definition", file + ":3:5"}
			if mode == "in process" {
				args = append([]string{"--no-daemon"}, args...)
			} else {
				daemonEnv(t, "30s")
			}
			_ = dir

			code, stdout, stderr := runMain(args...)
			if code != ExitCrash {
				t.Fatalf("exit %d, want %d; stderr: %s\nstdout: %s", code, ExitCrash, stderr, stdout)
			}
			env := decodeData(t, stdout, nil)
			if env.OK || env.Error == nil || env.Error.Code != "spawn_failed" {
				t.Fatalf("envelope = %s, want ok:false spawn_failed", stdout)
			}
			for _, want := range []string{"exit status 42", "starting up", "No version is set for shim: gopls"} {
				if !strings.Contains(env.Error.Message, want) {
					t.Errorf("message %q lacks %q", env.Error.Message, want)
				}
			}
			if strings.Contains(env.Error.Message, "\n") {
				t.Errorf("message is not one line: %q", env.Error.Message)
			}
			data := env.Error.Data
			if data["exit_status"] != "exit status 42" || data["exit_code"] != float64(42) {
				t.Errorf("error.data exit = %v / %v, want exit status 42 / 42", data["exit_status"], data["exit_code"])
			}
			tail, _ := data["stderr_tail"].(string)
			if !strings.Contains(tail, "starting up") || !strings.Contains(tail, "No version is set for shim") {
				t.Errorf("error.data.stderr_tail = %q, want both lines the server wrote", tail)
			}
			if data["server"] != "gopls" {
				t.Errorf("error.data.server = %v, want gopls", data["server"])
			}
		})
	}
}

// The two modes must say the same thing, byte for byte: the daemon is
// not allowed to be a less informative way to run.
func TestStartupDeathEnvelopeIsTheSameInBothModes(t *testing.T) {
	dir, file := cjkFixture(t)
	_ = dir
	useServerCommand(t, brokenShim(t))
	daemonEnv(t, "30s")

	_, direct, _ := runMain("--no-daemon", "definition", file+":3:5")
	_, viaDaemon, _ := runMain("definition", file+":3:5")
	if direct != viaDaemon {
		t.Errorf("the envelope differs by mode:\n--no-daemon: %s\ndaemon:      %s", direct, viaDaemon)
	}
	var check map[string]any
	if err := json.Unmarshal([]byte(direct), &check); err != nil {
		t.Fatal(err)
	}
}

// A chatty server must not turn the envelope into its log.
func TestStartupDeathStderrTailIsBounded(t *testing.T) {
	dir, file := cjkFixture(t)
	_ = dir
	useServerCommand(t, shellScript(t, t.TempDir(), "gopls",
		"i=0\nwhile [ $i -lt 400 ]; do echo \"line $i of a very verbose startup log\" >&2; i=$((i+1)); done\nexit 3\n"))

	code, stdout, _ := runMain("--no-daemon", "definition", file+":3:5")
	if code != ExitCrash {
		t.Fatalf("exit %d, want %d\n%s", code, ExitCrash, stdout)
	}
	env := decodeData(t, stdout, nil)
	tail, _ := env.Error.Data["stderr_tail"].(string)
	if len(tail) == 0 || len(tail) > 2048 {
		t.Fatalf("stderr_tail is %d bytes, want 1..2048", len(tail))
	}
	if !strings.Contains(tail, "line 399 ") || strings.Contains(tail, "line 0 ") {
		t.Errorf("stderr_tail should be the end of the log, got %q…", tail[:min(len(tail), 60)])
	}
	if !strings.HasPrefix(tail, "line ") {
		t.Errorf("stderr_tail starts mid-line: %q", tail[:min(len(tail), 40)])
	}
	if env.Error.Data["stderr_truncated"] != true {
		t.Errorf("stderr_truncated = %v, want true", env.Error.Data["stderr_truncated"])
	}
	if len(env.Error.Message) > 600 {
		t.Errorf("the message is %d bytes; the summary must stay short", len(env.Error.Message))
	}
}
