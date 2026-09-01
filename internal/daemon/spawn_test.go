package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// This file covers the auto-spawn path, which is the one part of the
// design that cannot be tested inside a single process: a client that
// finds no daemon re-executes lightspeed's own binary and dials the
// result.
//
// The binary it re-executes here is the test binary itself, in a mode
// TestMain intercepts. So the spawn, the socket, the handshake and the
// shutdown are all real, and the only fake left is the language server
// at the far end.

// Environment the child daemon is configured with. Configuration goes
// through the environment rather than flags because the flags belong to
// internal/cli, which another agent owns this round.
const (
	envChild      = "LIGHTSPEED_TEST_DAEMON_CHILD"
	envSocket     = "LIGHTSPEED_TEST_DAEMON_SOCKET"
	envRoot       = "LIGHTSPEED_TEST_DAEMON_ROOT"
	envListen     = "LIGHTSPEED_TEST_DAEMON_LISTEN_TIMEOUT"
	envStartDelay = "LIGHTSPEED_TEST_DAEMON_START_DELAY"
)

func TestMain(m *testing.M) {
	if os.Getenv(envChild) == "1" {
		os.Exit(runChildDaemon())
	}
	os.Exit(m.Run())
}

// runChildDaemon is the daemon side of the auto-spawn test: the same
// [Serve] entry point internal/cli will call, with a fake fleet in
// place of real language servers.
func runChildDaemon() int {
	socket := os.Getenv(envSocket)
	root := os.Getenv(envRoot)
	if socket == "" || root == "" {
		fmt.Fprintln(os.Stderr, "child daemon: missing socket or root")
		return 2
	}
	listen, err := time.ParseDuration(orDefault(os.Getenv(envListen), "15s"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child daemon: bad listen timeout: %v\n", err)
		return 2
	}
	delay, err := time.ParseDuration(orDefault(os.Getenv(envStartDelay), "0s"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "child daemon: bad start delay: %v\n", err)
		return 2
	}

	defs, err := childRouter()
	if err != nil {
		fmt.Fprintf(os.Stderr, "child daemon: %v\n", err)
		return 2
	}
	fleet := &fakeFleet{startDelay: delay}

	err = Serve(context.Background(), Options{
		Pool: PoolOptions{
			Router:          defs,
			Launcher:        fleet.launcher(),
			Gate:            fastGate(),
			ShutdownTimeout: 2 * time.Second,
		},
		Workspace:     root,
		Socket:        socket,
		ListenTimeout: listen,
		DrainTimeout:  3 * time.Second,
		Logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "daemon: "+format+"\n", args...)
		},
	})
	switch {
	case err == nil, errors.Is(err, ErrIdleTimeout), errors.Is(err, ErrAlreadyRunning):
		return 0
	default:
		fmt.Fprintf(os.Stderr, "child daemon: %v\n", err)
		return 1
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// childRouter builds the router the child daemon uses. It cannot use
// the test helper, because there is no *testing.T in the child.
func childRouter() (*router.Router, error) { return router.New(goDef()) }

// TestAutoSpawnAndReuse is the whole M3 loop from the client's side:
// the first command starts a daemon, the second finds it, and the
// language server is started exactly once for both.
func TestAutoSpawnAndReuse(t *testing.T) {
	root := workspace(t)
	dir := socketDir(t)
	socket := SocketPathIn(dir, root)
	logfile := filepath.Join(dir, "daemon.log")

	opts := Options{
		Workspace:     root,
		RuntimeDir:    dir,
		ListenTimeout: 15 * time.Second,
		DialTimeout:   2 * time.Second,
		Logf:          t.Logf,
		Spawn: SpawnConfig{
			Executable: os.Args[0],
			// No arguments: the child is configured through the
			// environment, and the test binary would reject flags it
			// does not know.
			Args:    func(string) []string { return nil },
			Logfile: logfile,
			Env: append(os.Environ(),
				envChild+"=1",
				envSocket+"="+socket,
				envRoot+"="+root,
				envListen+"=15s",
				// A slow cold start, so that a second spawn would be
				// obvious in the timings as well as in the counter.
				envStartDelay+"=300ms",
			),
		},
	}
	t.Cleanup(func() { stopDaemon(t, socket) })

	// First invocation: no daemon yet.
	first, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open (first invocation): %v\n%s", err, readLog(logfile))
	}
	if !first.Remote() {
		t.Error("the handle is not remote, so nothing was spawned")
	}
	if c, ok := first.(*Client); ok && !c.Spawned() {
		t.Error("the first invocation found a daemon it should have had to start")
	}
	cold, err := first.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("first query: %v\n%s", err, readLog(logfile))
	}
	if cold.Warm {
		t.Error("the first query reported a warm session")
	}
	if cold.Spawns != 1 {
		t.Errorf("the daemon spawned %d servers, want 1", cold.Spawns)
	}
	if err := first.Close(); err != nil {
		t.Errorf("closing the first client: %v", err)
	}

	// Second invocation, a fresh process's worth of state: it must
	// find the running daemon and its warm server.
	second, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open (second invocation): %v\n%s", err, readLog(logfile))
	}
	defer second.Close()
	if c, ok := second.(*Client); ok && c.Spawned() {
		t.Error("the second invocation started a second daemon")
	}
	warm, err := second.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("second query: %v\n%s", err, readLog(logfile))
	}
	if !warm.Warm {
		t.Error("the second invocation got a cold session")
	}
	if warm.Spawns != 1 {
		t.Errorf("the daemon spawned %d servers across two invocations, want 1", warm.Spawns)
	}
	if string(warm.Result) != string(cold.Result) {
		t.Errorf("the warm answer differs from the cold one:\n%s\n%s", cold.Result, warm.Result)
	}

	st, err := second.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.PID == os.Getpid() {
		t.Error("the daemon is running in the test process")
	}
	if st.Socket != socket {
		t.Errorf("the daemon reports socket %q, want %q", st.Socket, socket)
	}
	if st.Requests != 2 {
		t.Errorf("the daemon reports %d requests, want 2", st.Requests)
	}
	if st.Workspace != root {
		t.Errorf("the daemon is keyed on %q, want %q", st.Workspace, root)
	}
	if st.ListenTimeout != 15*time.Second {
		t.Errorf("the daemon's listen timeout is %s, want 15s", st.ListenTimeout)
	}

	// And it can be told to go, leaving nothing behind.
	if err := second.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	waitFor(t, "the daemon to remove its socket", func() bool {
		_, err := os.Stat(socket)
		return os.IsNotExist(err)
	})
	if _, err := Dial(context.Background(), ClientOptions{Socket: socket, NoSpawn: true}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("after a stop, dialling returned %v, want ErrNotRunning", err)
	}
	waitFor(t, "the daemon to log a clean stop", func() bool {
		return strings.Contains(readLog(logfile), "stopped")
	})
}

// TestAutoSpawnRecoversFromStaleSocket: the previous daemon was killed
// and its socket file is still there. The next command must start a
// daemon anyway — a SIGKILL must not leave a workspace permanently
// broken.
func TestAutoSpawnRecoversFromStaleSocket(t *testing.T) {
	root := workspace(t)
	dir := socketDir(t)
	socket := SocketPathIn(dir, root)
	logfile := filepath.Join(dir, "daemon.log")

	// A file where the socket belongs, with nothing behind it. This
	// is the shape of the problem gopls's dialer removes-and-rebinds
	// around; ours renames over it.
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := Options{
		Workspace:     root,
		RuntimeDir:    dir,
		ListenTimeout: 15 * time.Second,
		DialTimeout:   2 * time.Second,
		Logf:          t.Logf,
		Spawn: SpawnConfig{
			Executable: os.Args[0],
			Args:       func(string) []string { return nil },
			Logfile:    logfile,
			Env: append(os.Environ(),
				envChild+"=1",
				envSocket+"="+socket,
				envRoot+"="+root,
				envListen+"=15s",
			),
		},
	}
	t.Cleanup(func() { stopDaemon(t, socket) })

	handle, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatalf("Open over a stale socket: %v\n%s", err, readLog(logfile))
	}
	defer handle.Close()
	if _, err := handle.Query(context.Background(), refsRequest(root)); err != nil {
		t.Fatalf("query over a recovered socket: %v\n%s", err, readLog(logfile))
	}
}

// TestNoDaemonModeSpawnsNothing: --no-daemon must not touch the socket
// directory at all, which is what makes it usable in CI.
func TestNoDaemonModeSpawnsNothing(t *testing.T) {
	root := workspace(t)
	dir := socketDir(t)
	fleet := &fakeFleet{}

	handle, err := Open(context.Background(), Options{
		NoDaemon:   true,
		Workspace:  root,
		RuntimeDir: dir,
		Pool: PoolOptions{
			Router:          testRouter(t),
			Launcher:        fleet.launcher(),
			Gate:            fastGate(),
			ShutdownTimeout: 2 * time.Second,
			Logf:            t.Logf,
		},
		Spawn: SpawnConfig{
			// If the in-process mode ever spawned anything, this
			// would run and fail the test.
			Executable: filepath.Join(dir, "must-not-be-executed"),
		},
	})
	if err != nil {
		t.Fatalf("Open --no-daemon: %v", err)
	}
	if handle.Remote() {
		t.Error("--no-daemon produced a remote handle")
	}

	resp, err := handle.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if resp.Warm {
		t.Error("the first in-process query reported a warm session")
	}
	warm, err := handle.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if !warm.Warm || warm.Spawns != 1 {
		t.Errorf("the in-process pool does not reuse sessions: warm=%v spawns=%d", warm.Warm, warm.Spawns)
	}

	if err := handle.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	fleet.assertAllExited(t)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("--no-daemon left %q in the socket directory", e.Name())
	}
}

// stopDaemon makes sure no daemon outlives the test, whatever went
// wrong in it.
func stopDaemon(t *testing.T, socket string) {
	t.Helper()
	if _, err := os.Stat(socket); err != nil {
		return
	}
	c, err := Dial(context.Background(), ClientOptions{Socket: socket, NoSpawn: true})
	if err != nil {
		return
	}
	defer c.Close()
	if err := c.Stop(context.Background()); err != nil {
		t.Logf("stopping the leftover daemon: %v", err)
	}
}

func readLog(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return "(no daemon log: " + err.Error() + ")"
	}
	return string(b)
}
