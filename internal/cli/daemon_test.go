package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
)

// These tests are the M3 wiring, end to end: a real daemon process (this
// test binary re-executed as `daemon serve`, see TestMain), a real unix
// socket, and the scripted fake language server behind it. Nothing else
// in the suite starts a daemon unless LIGHTSPEED_TEST_VIA_DAEMON is set.
//
// What they count is *processes*. A daemon's own report of how many
// servers it started is evidence about the daemon; the spawn log the
// fake server writes when it starts is evidence about the machine.

// daemonEnv puts the test in daemon mode with sockets in a directory of
// its own, and retires whatever daemon the test started when it ends. The
// directory is short because a unix socket path is limited to about a
// hundred bytes and t.TempDir() names are not.
func daemonEnv(t *testing.T, idle string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ls")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv(noDaemonEnv, "0")
	t.Setenv("XDG_RUNTIME_DIR", dir)
	t.Setenv(daemonTimeoutEnv, idle)
	prev := suiteRuntimeDir
	suiteRuntimeDir = dir
	t.Cleanup(func() {
		retireDaemons(t)
		suiteRuntimeDir = prev
	})
	return dir
}

// spawnLog makes the fake server log its own starts and exits, and
// returns readers for the two counts.
func spawnLog(t *testing.T) (spawns, exits func() int) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "spawns.log")
	t.Setenv(spawnLogEnv, path)
	count := func(word string) func() int {
		return func() int {
			data, _ := os.ReadFile(path)
			n := 0
			for _, line := range strings.Split(string(data), "\n") {
				if line == word {
					n++
				}
			}
			return n
		}
	}
	return count("spawn"), count("exit")
}

// eventually polls for a fact that is asynchronous by nature — a daemon
// leaving — and never for one a test could assert directly.
func eventually(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// statusOf runs `daemon status` and decodes it.
func statusOf(t *testing.T, args ...string) daemonStatusData {
	t.Helper()
	code, stdout, stderr := runMain(append([]string{"daemon", "status"}, args...)...)
	if code != ExitOK {
		t.Fatalf("daemon status: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	var env struct {
		OK   bool             `json:"ok"`
		Data daemonStatusData `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil || !env.OK {
		t.Fatalf("daemon status is not an ok envelope: %v\n%s", err, stdout)
	}
	return env.Data
}

func definitionScenario(t *testing.T) (dir, file string) {
	t.Helper()
	dir, file = cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	return dir, file
}

// TestDaemonSharesOneServerAcrossInvocations is PLAN §8 M3's criterion
// as far as it can be checked without a real rust-analyzer: the second
// query does not pay for a server, because there is only one, and it was
// started by the first. Latency is a consequence of that, and cannot be
// asserted hermetically; the process count can.
func TestDaemonSharesOneServerAcrossInvocations(t *testing.T) {
	dir, file := definitionScenario(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	code1, out1, err1 := runMain("definition", file+":3:5")
	code2, out2, err2 := runMain("definition", file+":3:5")
	if code1 != ExitOK || code2 != ExitOK {
		t.Fatalf("exit codes %d and %d, want both %d\nstderr: %s | %s\nstdout: %s | %s",
			code1, code2, ExitOK, err1, err2, out1, out2)
	}
	if out1 != out2 {
		t.Errorf("the warm answer differs from the cold one:\n%s\n%s", out1, out2)
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started for two invocations, want 1", n)
	}

	st := statusOf(t, "--path", dir)
	if !st.Running {
		t.Fatal("no daemon is running after two commands")
	}
	if st.Spawns != 1 || len(st.Servers) != 1 {
		t.Fatalf("daemon reports %d spawns and %d servers, want 1 and 1: %+v", st.Spawns, len(st.Servers), st)
	}
	if got := st.Servers[0]; got.Server != "gopls" || got.State != "ready" || got.Requests < 2 {
		t.Errorf("server = %+v, want gopls, ready, at least 2 requests", got)
	}
	if st.PID == os.Getpid() {
		t.Error("the daemon is running in the test process, so nothing was spawned")
	}
}

// TestDaemonHandlesEveryCommand sends each server-backed command
// through the daemon against one warm server. The parity of their
// output with --no-daemon is TestModesAreByteIdentical's job and, over
// the whole suite, LIGHTSPEED_TEST_VIA_DAEMON's; this asserts the
// property the wiring is for: they all reach the *same* server.
func TestDaemonHandlesEveryCommand(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{
		results: map[string]any{
			methodDefinition:     []any{loc(file, 2, 4, 6)},
			methodReferences:     []any{loc(file, 2, 4, 6)},
			methodImplementation: []any{loc(file, 2, 4, 6)},
			methodHover:          map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "var 変数 int"}},
			methodDocumentSymbol: []any{},
			methodWorkspaceSymbol: []any{map[string]any{
				"name": "変数", "kind": 13, "containerName": "cjk", "location": loc(file, 2, 4, 6),
			}},
		},
	}.apply(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	for _, args := range [][]string{
		{"definition", file + ":3:5"},
		{"references", file + ":3:5"},
		{"implementation", file + ":3:5"},
		{"hover", file + ":3:5"},
		{"symbols", file},
		{"workspace_symbol", "変数", "--path", dir},
		{"references", "--symbol", "cjk.変数", "--path", dir},
		{"help", file},
		{"raw", "workspace/symbol", "--params", `{"query":"変数"}`, "--path", dir},
	} {
		code, stdout, stderr := runMain(args...)
		// `symbols` is scripted to an authoritative empty answer, which
		// is exit 1; the point here is that it was answered at all.
		if code != ExitOK && !(args[0] == "symbols" && code == ExitProblems) {
			t.Errorf("%v: exit %d; stderr: %s\nstdout: %s", args, code, stderr, stdout)
		}
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started for nine commands, want 1", n)
	}
}

// TestDaemonBatchSharesOneServer: a batch is a stream of independent
// commands, and each of them finds the daemon.
func TestDaemonBatchSharesOneServer(t *testing.T) {
	_, file := definitionScenario(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	code, stdout, stderr := runBatch("definition " + file + ":3:5\ndefinition " + file + ":3:5\ndefinition " + file + ":3:5\n")
	if code != ExitOK {
		t.Fatalf("batch: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	if lines := decodeBatch(t, stdout); len(lines) != 3 {
		t.Fatalf("batch answered %d lines, want 3", len(lines))
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started for a three-line batch, want 1", n)
	}
}

// TestNoDaemonSpawnsPerCall: --no-daemon, in every spelling, is today's
// behaviour — a fresh server per command, and no daemon left behind.
func TestNoDaemonSpawnsPerCall(t *testing.T) {
	dir, file := definitionScenario(t)
	runtime := daemonEnv(t, "30s")
	spawns, exits := spawnLog(t)

	// The flag before the subcommand, the flag after it, and the
	// environment variable: three spellings, three servers.
	if code, stdout, stderr := runMain("--no-daemon", "definition", file+":3:5"); code != ExitOK {
		t.Fatalf("--no-daemon before the subcommand: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	if code, stdout, stderr := runMain("definition", file+":3:5", "--no-daemon"); code != ExitOK {
		t.Fatalf("--no-daemon after the subcommand: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	t.Setenv(noDaemonEnv, "1")
	if code, stdout, stderr := runMain("definition", file+":3:5"); code != ExitOK {
		t.Fatalf("%s=1: exit %d; stderr: %s\nstdout: %s", noDaemonEnv, code, stderr, stdout)
	}

	if n := spawns(); n != 3 {
		t.Errorf("%d language servers were started for three --no-daemon invocations, want 3", n)
	}
	// Every one of them was shut down by the command that started it.
	eventually(t, "the in-process servers to exit", 5*time.Second, func() bool { return exits() == 3 })

	t.Setenv(noDaemonEnv, "0")
	if st := statusOf(t, "--path", dir); st.Running {
		t.Errorf("a daemon is running after --no-daemon invocations: %+v", st)
	}
	if entries, _ := filepath.Glob(filepath.Join(runtime, "lightspeed", "*")); len(entries) != 0 {
		t.Errorf("--no-daemon left %v in the runtime directory", entries)
	}
}

// TestNoDaemonWithDaemonSubcommandIsUsage: there is no daemon to ask.
func TestNoDaemonWithDaemonSubcommandIsUsage(t *testing.T) {
	daemonEnv(t, "30s")
	for _, sub := range []string{"status", "stop", "logs"} {
		if code, stdout, _ := runMain("--no-daemon", "daemon", sub); code != ExitUsage {
			t.Errorf("daemon %s --no-daemon: exit %d, want %d\n%s", sub, code, ExitUsage, stdout)
		}
	}
	if code, _, _ := runMain("daemon"); code != ExitUsage {
		t.Errorf("daemon with no subcommand: exit %d, want %d", code, ExitUsage)
	}
	if code, _, _ := runMain("daemon", "explode"); code != ExitUsage {
		t.Errorf("daemon explode: exit %d, want %d", code, ExitUsage)
	}
}

// TestDaemonStatusStopLogs is the management surface: status describes
// the daemon and its servers, logs shows what it said, stop leaves
// nothing behind, and each says the truthful thing when there is no
// daemon.
func TestDaemonStatusStopLogs(t *testing.T) {
	dir, file := definitionScenario(t)
	runtime := daemonEnv(t, "30s")
	spawns, exits := spawnLog(t)

	// Nothing has run yet. That is an answer, not an error.
	if st := statusOf(t, "--path", dir); st.Running || st.Workspace == "" || st.Socket == "" {
		t.Errorf("status before any command = %+v, want not running with the workspace and socket named", st)
	}
	code, stdout, _ := runMain("daemon", "stop", "--path", dir)
	if code != ExitOK || !strings.Contains(stdout, `"stopped":false`) {
		t.Errorf("stop with nothing running: exit %d, %s; want exit 0 and stopped:false", code, stdout)
	}
	if code, stdout, _ := runMain("daemon", "logs", "--path", dir); code != ExitOK || !strings.Contains(stdout, `"exists":false`) {
		t.Errorf("logs before any daemon: exit %d, %s; want exit 0 and exists:false", code, stdout)
	}

	if code, stdout, stderr := runMain("definition", file+":3:5"); code != ExitOK {
		t.Fatalf("definition: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}

	st := statusOf(t, "--path", dir)
	want, _ := filepath.EvalSymlinks(dir)
	switch {
	case !st.Running:
		t.Fatal("status says no daemon is running")
	case st.Workspace != want:
		t.Errorf("workspace = %q, want %q", st.Workspace, want)
	case !strings.HasPrefix(st.Socket, filepath.Join(runtime, "lightspeed")) || !strings.HasSuffix(st.Socket, ".sock"):
		t.Errorf("socket = %q, want a .sock under %s/lightspeed", st.Socket, runtime)
	case st.PID <= 0 || st.PID == os.Getpid():
		t.Errorf("pid = %d, want the daemon's own", st.PID)
	case st.IdleTimeoutSeconds != 30:
		t.Errorf("idle timeout = %vs, want 30s", st.IdleTimeoutSeconds)
	case st.SessionIdleTimeoutSeconds <= 0:
		t.Errorf("session idle timeout = %vs, want a positive one", st.SessionIdleTimeoutSeconds)
	case len(st.Servers) != 1 || st.Servers[0].Root != want || st.Servers[0].State != "ready":
		t.Errorf("servers = %+v, want one ready gopls rooted at %s", st.Servers, want)
	}

	if code, stdout, _ := runMain("daemon", "status", "--path", dir, "--format", "text"); code != ExitOK ||
		!strings.Contains(stdout, "gopls") || !strings.Contains(stdout, "[ready]") {
		t.Errorf("status --format text: exit %d\n%s", code, stdout)
	}

	// The log shows the server being started, and works while the
	// daemon is up.
	code, stdout, _ = runMain("daemon", "logs", "--path", dir, "--lines", "100")
	var logs struct {
		Data daemonLogsData `json:"data"`
	}
	if err := json.Unmarshal([]byte(stdout), &logs); err != nil || code != ExitOK {
		t.Fatalf("logs: exit %d: %v\n%s", code, err, stdout)
	}
	if !logs.Data.Exists || !strings.Contains(strings.Join(logs.Data.Lines, "\n"), "spawning gopls") {
		t.Errorf("the log does not show the server being spawned: %+v", logs.Data)
	}

	code, stdout, stderr := runMain("daemon", "stop", "--path", dir)
	if code != ExitOK || !strings.Contains(stdout, `"stopped":true`) {
		t.Fatalf("stop: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	// Stop returns when the daemon has gone, not when it has started
	// going: the socket is already removed, and so is the server.
	if _, err := os.Stat(st.Socket); !os.IsNotExist(err) {
		t.Errorf("the socket is still there after stop (stat: %v)", err)
	}
	// ...and so is the *process*, not just the socket: the daemon unpublishes
	// its socket first and shuts its language servers down afterwards, so a
	// stop that returned on the socket would hand the caller a daemon that is
	// still writing. There is no waiting here (no eventually): what stop
	// returned is what is asserted.
	if exits() != spawns() {
		t.Errorf("stop returned with %d language server(s) started and %d exited", spawns(), exits())
	}
	gone, cancelGone := context.WithTimeout(context.Background(), time.Millisecond)
	if err := daemon.WaitExit(gone, st.PID); err != nil {
		t.Errorf("stop returned while the daemon (pid %d) was still running: %v", st.PID, err)
	}
	cancelGone()
	if st := statusOf(t, "--path", dir); st.Running {
		t.Errorf("status after stop says the daemon is running: %+v", st)
	}
	if code, stdout, _ := runMain("daemon", "stop", "--path", dir); code != ExitOK || !strings.Contains(stdout, `"stopped":false`) {
		t.Errorf("a second stop: exit %d, %s; want exit 0 and stopped:false", code, stdout)
	}

	// The next command starts a fresh daemon, which is what "stop" is
	// for and not a failure.
	if code, stdout, stderr := runMain("definition", file+":3:5"); code != ExitOK {
		t.Fatalf("definition after stop: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	if n := spawns(); n != 2 {
		t.Errorf("%d language servers were started across a stop, want 2", n)
	}
}

// TestDaemonIsKeyedOnTheWorkspaceNotTheDirectory: `cd` into a
// subdirectory reaches the daemon (and the warm server) that the
// repository root already has, instead of starting a second one — PLAN
// §3's "socket keyed on resolved workspace root so `cd` into a subdir
// reuses it".
func TestDaemonIsKeyedOnTheWorkspaceNotTheDirectory(t *testing.T) {
	dir, file := cjkFixture(t)
	// The marker that makes `dir` a repository: without one, the
	// directory of the file would be its own workspace.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "internal", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "inner.go"), cjkSource)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	if code, stdout, stderr := runMain("definition", file+":3:5"); code != ExitOK {
		t.Fatalf("definition from the root: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	atRoot := statusOf(t, "--path", dir)

	t.Chdir(sub)
	// No --path: the working directory is the address.
	fromSub := statusOf(t)
	if !fromSub.Running || fromSub.Socket != atRoot.Socket || fromSub.PID != atRoot.PID {
		t.Errorf("from a subdirectory: %+v\nfrom the root: %+v\nwant the same daemon", fromSub, atRoot)
	}
	// A query about a file under the subdirectory, by a relative path,
	// is answered by the same warm server.
	if code, stdout, stderr := runMain("definition", "inner.go:3:5"); code != ExitOK {
		t.Fatalf("definition from the subdirectory: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started across the root and a subdirectory, want 1", n)
	}
	if code, stdout, _ := runMain("daemon", "stop"); code != ExitOK || !strings.Contains(stdout, `"stopped":true`) {
		t.Errorf("stop from the subdirectory: exit %d, %s; want it to stop the root's daemon", code, stdout)
	}
}

// TestDaemonExitsWhenIdle: with no client connected for the listen
// timeout the daemon leaves, taking its language servers and its socket
// with it — also when the client that started it was in a subdirectory.
func TestDaemonExitsWhenIdle(t *testing.T) {
	dir, file := cjkFixture(t)
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(sub, "inner.go"), cjkSource)
	_ = file
	scenario{results: map[string]any{methodDefinition: []any{loc(filepath.Join(sub, "inner.go"), 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "400ms")
	spawns, exits := spawnLog(t)

	t.Chdir(sub)
	if code, stdout, stderr := runMain("definition", "inner.go:3:5"); code != ExitOK {
		t.Fatalf("definition: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	st := statusOf(t)
	if !st.Running || st.IdleTimeoutSeconds != 0.4 {
		t.Fatalf("status = %+v, want a running daemon with a 0.4s idle timeout", st)
	}
	// statusOf connected, so the clock restarts; then nobody does.
	eventually(t, "the idle daemon to remove its socket", 10*time.Second, func() bool {
		_, err := os.Stat(st.Socket)
		return os.IsNotExist(err)
	})
	eventually(t, "the idle daemon to shut its language server down", 5*time.Second, func() bool {
		return exits() == spawns() && spawns() == 1
	})
	if st := statusOf(t); st.Running {
		t.Errorf("status after the idle exit says the daemon is running: %+v", st)
	}
}

// TestDaemonWhileIndexingStillExitsNotReady is PLAN §5.2 across the
// socket. The daemon holds a warm server that is still indexing and
// answers every query with an empty list; the exit code must be 5 — for
// the first query and for the tenth, with the daemon and without it,
// byte for byte — and never an authoritative-looking empty answer. If
// the distinction were lost in transit, the daemon would be quietly less
// safe than no daemon, which is the one thing it must not be.
func TestDaemonWhileIndexingStillExitsNotReady(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{
		indexing: true,
		results:  map[string]any{methodReferences: []any{}},
	}.apply(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	args := []string{"references", file + ":3:5", "--timeout", "400ms", "--settle", "50ms"}
	var viaDaemon []string
	for i := 0; i < 2; i++ {
		code, stdout, stderr := runMain(args...)
		if code != ExitNotReady {
			t.Fatalf("query %d through the daemon: exit %d, want %d (not ready); stderr: %s\nstdout: %s",
				i+1, code, ExitNotReady, stderr, stdout)
		}
		env := decodeEnvelope(t, stdout)
		if env.OK || env.Error == nil || env.Error.Code != "not_ready" {
			t.Fatalf("query %d: envelope = %s, want ok:false code not_ready", i+1, stdout)
		}
		if strings.Contains(stdout, `"results"`) {
			t.Errorf("query %d: an unready workspace produced a result set: %s", i+1, stdout)
		}
		viaDaemon = append(viaDaemon, stdout)
	}
	if n := spawns(); n != 1 {
		t.Errorf("%d language servers were started for two queries, want 1 (the second must have hit the same indexing server)", n)
	}

	// `daemon status` says so too: this is the state in which an empty
	// answer could not be believed.
	st := statusOf(t, "--path", dir)
	if len(st.Servers) != 1 || st.Servers[0].State != "indexing" || len(st.Servers[0].Progress) == 0 {
		t.Errorf("servers = %+v, want one server in state indexing with its progress token named", st.Servers)
	}

	// And the envelope is the one the in-process mode prints.
	code, direct, _ := runMain(append([]string{"--no-daemon"}, args...)...)
	if code != ExitNotReady {
		t.Fatalf("--no-daemon: exit %d, want %d", code, ExitNotReady)
	}
	for i, got := range viaDaemon {
		if normalizeElapsed(got) != normalizeElapsed(direct) {
			t.Errorf("query %d through the daemon differs from --no-daemon:\n%s\n%s", i+1, got, direct)
		}
	}
}

// gaveUp matches the two numbers that legitimately differ from run to
// run in a not-ready message: how long the gate waited before it gave
// up, and how many times it asked in that time.
var gaveUp = regexp.MustCompile(`gave up after \S+ and \d+ attempt\(s\)`)

func normalizeElapsed(s string) string { return gaveUp.ReplaceAllString(s, "gave up") }

// TestModesAreByteIdentical: the same command against the same script
// prints the same bytes whether it goes through the daemon or not.
func TestModesAreByteIdentical(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{
		results: map[string]any{
			methodDefinition: []any{loc(file, 2, 4, 6)},
			methodReferences: []any{loc(file, 2, 4, 6), loc(file, 5, 8, 10)},
			methodHover:      map[string]any{"contents": map[string]any{"kind": "markdown", "value": "```go\nvar 変数 int\n```"}},
			methodWorkspaceSymbol: []any{map[string]any{
				"name": "変数", "kind": 13, "containerName": "cjk", "location": loc(file, 2, 4, 6),
			}},
		},
	}.apply(t)
	daemonEnv(t, "30s")

	for _, args := range [][]string{
		{"definition", file + ":3:5"},
		{"references", file + ":3:5", "--context", "1", "--format", "text"},
		{"references", file + ":3:5", "--limit", "1", "--indent"},
		{"hover", file + ":3:5"},
		{"workspace_symbol", "変数", "--path", dir},
		{"references", "--symbol", "cjk.変数", "--path", dir},
		// Failures are part of the contract too.
		{"definition", file + ":99:1"},
		{"implementation", file + ":3:5"},
		{"definition", filepath.Join(dir, "missing.go") + ":1:1"},
	} {
		codeD, outD, _ := runMain(args...)
		codeP, outP, _ := runMain(append([]string{"--no-daemon"}, args...)...)
		if codeD != codeP || outD != outP {
			t.Errorf("%v differs between modes:\n daemon    (exit %d): %s\n in process (exit %d): %s",
				args, codeD, outD, codeP, outP)
		}
	}
}

// TestApplyTellsTheWarmServerWhichFilesChanged: a daemon's server
// outlives the command that rewrote three files, and nothing else is
// watching the tree. The command has to say what it wrote, or the next
// query is answered from what the server last read.
func TestApplyTellsTheWarmServerWhichFilesChanged(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := renameScenario(t, dir, renameToNew(dir))
	readTrace := s.traceTo(t)
	s.apply(t)
	daemonEnv(t, "30s")

	code, stdout, stderr := runMain("rename", declLoc(dir), "New", "--apply", "--allow-dirty", "--format", "json")
	if code != ExitOK {
		t.Fatalf("rename --apply: exit %d; stderr: %s\nstdout: %s", code, stderr, stdout)
	}

	var told []string
	eventually(t, "the server to be told about the writes", 5*time.Second, func() bool {
		told = nil
		for _, msg := range readTrace() {
			if msg.Method != "workspace/didChangeWatchedFiles" {
				continue
			}
			var p struct {
				Changes []struct {
					URI  string `json:"uri"`
					Type int    `json:"type"`
				} `json:"changes"`
			}
			if err := json.Unmarshal(msg.Params, &p); err != nil {
				t.Fatalf("bad didChangeWatchedFiles params: %v", err)
			}
			for _, c := range p.Changes {
				told = append(told, filepath.Base(c.URI)+":"+string(rune('0'+c.Type)))
			}
		}
		return len(told) == 3
	})
	if got := strings.Join(told, ","); got != "a.go:2,b.go:2,c.go:2" {
		t.Errorf("the server was told %q, want the three rewritten files as changed", got)
	}
	// And the documents the command opened were closed again, so the
	// server reads the new text from disk instead of its own copy.
	var opened, closed int
	for _, msg := range readTrace() {
		switch msg.Method {
		case "textDocument/didOpen":
			opened++
		case "textDocument/didClose":
			closed++
		}
	}
	if opened == 0 || opened != closed {
		t.Errorf("%d documents opened and %d closed, want every one closed", opened, closed)
	}
}

// TestCheckOnAWarmServerReportsOnlyWhatItWasAskedAbout: a daemon's
// session has been told about every file an earlier command opened. The
// next `check` on another file must not report those — they were
// published for content the server no longer has open, and a fresh
// server (--no-daemon) would never have mentioned them. Run twice per
// file so the second answer comes from a session that has already
// published about it.
func TestCheckOnAWarmServerReportsOnlyWhatItWasAskedAbout(t *testing.T) {
	_, main, helper := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main:   []any{diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName")},
			helper: []any{},
		},
	}.apply(t)
	daemonEnv(t, "30s")
	spawns, _ := spawnLog(t)

	for _, path := range []string{main, helper, main, helper} {
		args := []string{"check", path, "--settle", "20ms"}
		codeD, outD, errD := runMain(args...)
		codeP, outP, _ := runMain(append([]string{"--no-daemon"}, args...)...)
		if codeD != codeP || outD != outP {
			t.Errorf("check %s differs between modes:\n daemon    (exit %d): %s\n in process (exit %d): %s",
				filepath.Base(path), codeD, outD, codeP, outP)
		}
		if path == helper {
			if codeD != ExitOK || errD != "" {
				t.Errorf("check helper.go through a warm daemon: exit %d, want %d (cjk.go's error is not helper.go's); stdout: %s",
					codeD, ExitOK, outD)
			}
			if strings.Contains(outD, "cjk.go") {
				t.Errorf("check helper.go reported a diagnostic in cjk.go: %s", outD)
			}
		}
	}
	// The daemon really was warm: one server for its four runs (the
	// --no-daemon runs are the other four spawns).
	if n := spawns(); n != 1+4 {
		t.Errorf("%d language servers started, want 5 (one daemon server + four in-process)", n)
	}
}
