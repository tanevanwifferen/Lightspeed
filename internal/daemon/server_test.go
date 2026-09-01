package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// socketDir returns a short-lived directory for sockets. It is not
// t.TempDir because a unix socket path is limited to about a hundred
// bytes and a long test name would blow that budget in a way that looks
// like a daemon bug.
func socketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lsd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// testServer is a running daemon in this process, with its socket.
type testServer struct {
	*Server
	socket   string
	serveErr chan error
	fleet    *fakeFleet
}

// newTestServer starts a daemon on a socket in a temp directory. The
// language servers it pools are fakes; the socket, the protocol and the
// lifecycle are the real thing.
func newTestServer(t *testing.T, fleet *fakeFleet, pool PoolOptions, opts ServerOptions) *testServer {
	t.Helper()
	if fleet == nil {
		fleet = &fakeFleet{}
	}
	p := newTestPool(t, fleet, pool)
	if opts.Socket == "" {
		opts.Socket = filepath.Join(socketDir(t), "test.sock")
	}
	if opts.DrainTimeout == 0 {
		opts.DrainTimeout = 2 * time.Second
	}
	opts.Service = NewService(p, "workspace")
	opts.Logf = t.Logf

	srv, err := NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ts := &testServer{Server: srv, socket: opts.Socket, serveErr: make(chan error, 1), fleet: fleet}
	go func() { ts.serveErr <- srv.Serve(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	return ts
}

// dial connects a client to the test daemon, never spawning one.
func (ts *testServer) dial(t *testing.T) *Client {
	t.Helper()
	c, err := Dial(context.Background(), ClientOptions{
		Socket:  ts.socket,
		NoSpawn: true,
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// waitServe waits for the accept loop to return and reports its error.
func (ts *testServer) waitServe(t *testing.T) error {
	t.Helper()
	select {
	case err := <-ts.serveErr:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon never stopped serving")
		return nil
	}
}

func refsRequest(root string) Request {
	file := filepath.Join(root, "main.go")
	return Request{
		Path:   file,
		Method: "textDocument/references",
		Params: json.RawMessage(`{"textDocument":{"uri":"file://` + file + `"}}`),
		Open:   true,
	}
}

// TestServerWarmSessionAcrossClients is the daemon's reason to exist,
// over the socket: two separate client connections — two `lightspeed`
// invocations — share one language server. The fake is slow to start,
// so a second spawn would also show up as latency; the assertion is on
// the spawn count, which does not depend on the machine's mood.
func TestServerWarmSessionAcrossClients(t *testing.T) {
	root := workspace(t)
	ts := newTestServer(t, &fakeFleet{startDelay: 300 * time.Millisecond}, PoolOptions{}, ServerOptions{})

	first := ts.dial(t)
	cold, err := first.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	if cold.Warm {
		t.Error("the first query reported a warm session")
	}
	if err := first.Close(); err != nil {
		t.Fatalf("closing the first client: %v", err)
	}

	second := ts.dial(t)
	warm, err := second.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("second query: %v", err)
	}
	if !warm.Warm {
		t.Error("the second client got a cold session")
	}
	if warm.Spawns != 1 {
		t.Errorf("the daemon spawned %d servers for two queries, want 1", warm.Spawns)
	}
	if ts.fleet.spawnCount() != 1 {
		t.Errorf("the fleet started %d servers, want 1", ts.fleet.spawnCount())
	}
	if string(cold.Result) != string(warm.Result) {
		t.Errorf("warm and cold answers differ:\n%s\n%s", cold.Result, warm.Result)
	}

	st, err := second.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.InProcess {
		t.Error("a daemon reported itself as in-process")
	}
	if st.Socket != ts.socket {
		t.Errorf("status reports socket %q, want %q", st.Socket, ts.socket)
	}
	if st.Requests != 2 {
		t.Errorf("status reports %d requests, want 2", st.Requests)
	}
	if st.Spawns != 1 {
		t.Errorf("status reports %d spawns, want 1", st.Spawns)
	}
	if st.Clients != 1 {
		t.Errorf("status reports %d connected clients, want 1", st.Clients)
	}
	if len(st.Sessions) != 1 || st.Sessions[0].Requests != 2 {
		t.Errorf("status sessions: %+v", st.Sessions)
	}
	if st.Sessions[0].OpenDocuments != 1 {
		t.Errorf("the daemon holds %d open documents, want 1", st.Sessions[0].OpenDocuments)
	}
}

// TestServerConcurrentClients: many clients at once on one daemon, all
// answered, still one language server.
func TestServerConcurrentClients(t *testing.T) {
	root := workspace(t)
	ts := newTestServer(t, &fakeFleet{startDelay: 150 * time.Millisecond}, PoolOptions{}, ServerOptions{})

	const clients = 6
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		errs  []error
		warm  int
		ready = make(chan struct{})
	)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := Dial(context.Background(), ClientOptions{Socket: ts.socket, NoSpawn: true})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer c.Close()
			<-ready // make the requests overlap
			resp, err := c.Query(context.Background(), refsRequest(root))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if resp.Warm {
				warm++
			}
		}()
	}
	close(ready)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent clients: %v", errs)
	}
	if ts.fleet.spawnCount() != 1 {
		t.Errorf("%d concurrent clients caused %d spawns, want 1", clients, ts.fleet.spawnCount())
	}
	if warm != clients-1 {
		t.Errorf("%d of %d clients got a warm session, want %d", warm, clients, clients-1)
	}
}

// TestServerErrorsKeepTheirExitCodes is the property that makes the
// daemon safe: an error must mean the same thing through a socket as it
// does in process. If "still indexing" arrived as a generic failure,
// running with a daemon would be quietly less safe than running
// without one.
func TestServerErrorsKeepTheirExitCodes(t *testing.T) {
	root := workspace(t, "main.go", "notes.md")
	ts := newTestServer(t, &fakeFleet{scenario: indexingScenario}, PoolOptions{
		Gate: client.GateOptions{
			Settle:          20 * time.Millisecond,
			NoProgressGrace: 10 * time.Millisecond,
			PollInterval:    5 * time.Millisecond,
			Timeout:         200 * time.Millisecond,
		},
	}, ServerOptions{})
	c := ts.dial(t)

	_, err := c.Query(context.Background(), refsRequest(root))
	if err == nil {
		t.Fatal("a query against an indexing server returned a result")
	}
	if got := exitCodeOf(err); got != 5 {
		t.Errorf("exit code %d, want 5 (%v)", got, err)
	}
	if got := codeOf(err); got != "not_ready" {
		t.Errorf("code %q, want not_ready", got)
	}
	var derr *Error
	if !errors.As(err, &derr) {
		t.Fatalf("error is %T, want a *daemon.Error", err)
	}
	if derr.Server != "fake-go" || derr.Root != root {
		t.Errorf("error does not say which server failed: %+v", derr)
	}

	// A file no server claims: exit 3 on both sides of the socket.
	_, err = c.Query(context.Background(), Request{
		Path:   filepath.Join(root, "notes.md"),
		Method: "textDocument/references",
	})
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("unclaimed file: exit code %d, want 3 (%v)", got, err)
	}

	// A malformed request: exit 2.
	_, err = c.Query(context.Background(), Request{Path: filepath.Join(root, "main.go")})
	if got := exitCodeOf(err); got != 2 {
		t.Errorf("no method: exit code %d, want 2 (%v)", got, err)
	}
}

// TestServerHandshake: a client can find out what it is talking to,
// which is how a stale daemon from another build gets noticed.
func TestServerHandshake(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{})
	hs, err := ts.dial(t).Handshake(context.Background())
	if err != nil {
		t.Fatalf("Handshake: %v", err)
	}
	if hs.PID != os.Getpid() {
		t.Errorf("handshake pid %d, want %d", hs.PID, os.Getpid())
	}
	if hs.Workspace != "workspace" {
		t.Errorf("handshake workspace %q", hs.Workspace)
	}
	if hs.Executable == "" {
		t.Error("handshake does not name the daemon's executable")
	}
}

// TestServerUnknownMethod: a stray client gets a clean error, not a
// hang and not a crash.
func TestServerUnknownMethod(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{})
	c := ts.dial(t)
	if _, err := c.conn.Call(context.Background(), "textDocument/definition", nil); err == nil {
		t.Fatal("the daemon answered an LSP method as if it were a daemon method")
	}
	// Still alive afterwards.
	if _, err := c.Status(context.Background()); err != nil {
		t.Errorf("Status after an unknown method: %v", err)
	}
}

// TestServerStopIsGraceful: `daemon stop` must answer, then shut down
// the language servers, then remove the socket, so that the next
// command starts a fresh daemon instead of dialling a corpse.
func TestServerStopIsGraceful(t *testing.T) {
	root := workspace(t)
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{})
	c := ts.dial(t)

	if _, err := c.Query(context.Background(), refsRequest(root)); err != nil {
		t.Fatalf("query: %v", err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = c.Close()

	if err := ts.waitServe(t); err != nil {
		t.Errorf("Serve returned %v after a stop, want nil", err)
	}
	ts.fleet.assertAllExited(t)
	if _, err := os.Stat(ts.socket); !os.IsNotExist(err) {
		t.Errorf("the socket outlived the daemon: %v", err)
	}
	if _, err := net.DialTimeout("unix", ts.socket, 100*time.Millisecond); err == nil {
		t.Error("something is still listening on the socket")
	}
}

// TestServerDrainsRequestInFlight: a shutdown that arrives while a
// query is running must let the query answer. An agent's `references`
// call disappearing into an I/O error because a reaper fired is exactly
// the kind of flakiness that makes a tool untrustworthy.
func TestServerDrainsRequestInFlight(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{scenario: slowScenario(300 * time.Millisecond)}
	ts := newTestServer(t, fleet, PoolOptions{}, ServerOptions{})
	c := ts.dial(t)

	// Warm the session up first, so the slow part of the second query
	// is the request itself and not the spawn.
	if _, err := c.Query(context.Background(), refsRequest(root)); err != nil {
		t.Fatalf("warm-up query: %v", err)
	}

	type result struct {
		resp *Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := c.Query(context.Background(), refsRequest(root))
		_ = c.Close() // a real CLI exits as soon as it has its answer
		done <- result{resp, err}
	}()

	// Give the request time to reach the server, then shut down under
	// it.
	time.Sleep(50 * time.Millisecond)
	shutdownDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		shutdownDone <- ts.Shutdown(ctx)
	}()

	got := <-done
	if got.err != nil {
		t.Fatalf("the in-flight query failed during shutdown: %v", got.err)
	}
	if client.IsEmptyResult(got.resp.Result) {
		t.Errorf("the in-flight query returned nothing: %s", got.resp.Result)
	}
	if err := <-shutdownDone; err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	ts.fleet.assertAllExited(t)
}

// TestServerIdleTimeout is gopls's -listen.timeout: a daemon nobody
// talks to exits, and takes its language servers with it.
func TestServerIdleTimeout(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{ListenTimeout: 120 * time.Millisecond})

	err := ts.waitServe(t)
	if !errors.Is(err, ErrIdleTimeout) {
		t.Fatalf("Serve returned %v, want ErrIdleTimeout", err)
	}
	if _, err := os.Stat(ts.socket); !os.IsNotExist(err) {
		t.Errorf("an idle-exiting daemon left its socket behind: %v", err)
	}
}

// TestServerIdleTimeoutWaitsForClients: the timer must only run while
// nobody is connected, or a client sitting on a slow query would be
// disconnected by the idle reaper.
func TestServerIdleTimeoutWaitsForClients(t *testing.T) {
	root := workspace(t)
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{ListenTimeout: 100 * time.Millisecond})
	c := ts.dial(t)

	// Hold the connection open for several idle timeouts, using it
	// occasionally, and the daemon must stay up.
	for range 4 {
		time.Sleep(80 * time.Millisecond)
		if _, err := c.Query(context.Background(), refsRequest(root)); err != nil {
			t.Fatalf("query while holding the connection: %v", err)
		}
	}
	select {
	case err := <-ts.serveErr:
		t.Fatalf("the daemon exited while a client was connected: %v", err)
	default:
	}

	// Once the client leaves, the timer starts and the daemon exits.
	_ = c.Close()
	if err := ts.waitServe(t); !errors.Is(err, ErrIdleTimeout) {
		t.Errorf("Serve returned %v after the last client left, want ErrIdleTimeout", err)
	}
}

// TestServerRecoversStaleSocket: a daemon that was killed leaves a
// socket file that nothing is listening on. The next daemon must take
// the address over rather than refuse to start — otherwise one kill -9
// breaks the workspace until somebody deletes a file in
// $XDG_RUNTIME_DIR.
func TestServerRecoversStaleSocket(t *testing.T) {
	root := workspace(t)
	dir := socketDir(t)
	socket := filepath.Join(dir, "stale.sock")

	// A socket file with nobody behind it: bind and close the
	// listener without removing the file, which is what a SIGKILLed
	// daemon leaves.
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("the stale socket file is not there: %v", err)
	}

	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{Socket: socket})
	if _, err := ts.dial(t).Query(context.Background(), refsRequest(root)); err != nil {
		t.Fatalf("query after taking over a stale socket: %v", err)
	}
}

// TestServerRefusesToStealALiveSocket is the other half: a live daemon
// must not be displaced. Losing this race is a success — the caller
// should use the daemon that is already there.
func TestServerRefusesToStealALiveSocket(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{})

	pool := newTestPool(t, &fakeFleet{}, PoolOptions{})
	second, err := NewServer(ServerOptions{
		Service: NewService(pool, "workspace"),
		Socket:  ts.socket,
		Logf:    t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Listen(); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("Listen on a live socket returned %v, want ErrAlreadyRunning", err)
	}

	// And the first daemon is untouched.
	if _, err := ts.dial(t).Status(context.Background()); err != nil {
		t.Errorf("the original daemon stopped answering: %v", err)
	}
}

// TestServerNeedsServiceAndSocket: constructing a daemon without the
// two things it cannot invent must fail loudly.
func TestServerNeedsServiceAndSocket(t *testing.T) {
	if _, err := NewServer(ServerOptions{Socket: "/tmp/x.sock"}); err == nil {
		t.Error("NewServer without a service succeeded")
	}
	pool := newTestPool(t, &fakeFleet{}, PoolOptions{})
	if _, err := NewServer(ServerOptions{Service: NewService(pool, "")}); err == nil {
		t.Error("NewServer without a socket succeeded")
	}
}

// TestDialNoSpawnReportsNotRunning: `daemon status` on a workspace with
// no daemon must say so, not start one.
func TestDialNoSpawnReportsNotRunning(t *testing.T) {
	socket := filepath.Join(socketDir(t), "absent.sock")
	_, err := Dial(context.Background(), ClientOptions{Socket: socket, NoSpawn: true})
	if !errors.Is(err, ErrNotRunning) {
		t.Fatalf("Dial returned %v, want ErrNotRunning", err)
	}
	if _, statErr := os.Stat(socket); !os.IsNotExist(statErr) {
		t.Error("a dial with NoSpawn created something")
	}
}

// TestServeStopsWithContext: cancelling the daemon's context is the
// urgent path (a signal handler), and it must still leave no servers
// and no socket behind.
func TestServeStopsWithContext(t *testing.T) {
	root := workspace(t)
	dir := socketDir(t)
	fleet := &fakeFleet{}
	ctx, cancel := context.WithCancel(context.Background())

	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Options{
			Pool: PoolOptions{
				Router:          testRouter(t),
				Launcher:        fleet.launcher(),
				Gate:            fastGate(),
				ShutdownTimeout: 2 * time.Second,
			},
			Workspace:     root,
			RuntimeDir:    dir,
			ListenTimeout: 10 * time.Second,
			DrainTimeout:  2 * time.Second,
			Logf:          t.Logf,
		})
	}()

	socket := SocketPathIn(dir, root)
	waitFor(t, "the daemon to publish its socket", func() bool {
		_, err := os.Stat(socket)
		return err == nil
	})

	c, err := Dial(ctx, ClientOptions{Socket: socket, NoSpawn: true})
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := c.Query(ctx, refsRequest(root)); err != nil {
		t.Fatalf("query: %v", err)
	}
	_ = c.Close()

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve returned %v after its context was cancelled, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after its context was cancelled")
	}
	fleet.assertAllExited(t)
	if _, err := os.Stat(socket); !os.IsNotExist(err) {
		t.Errorf("the socket outlived the daemon: %v", err)
	}
}

// slowScenario is a fake server whose references method takes a while,
// so a test can have a request genuinely in flight.
func slowScenario(d time.Duration) func(*serverdef.ServerDef, string) fakeserver.Options {
	return func(def *serverdef.ServerDef, root string) fakeserver.Options {
		opts := defaultScenario(def, root)
		inner := opts.Methods["textDocument/references"]
		opts.Methods["textDocument/references"] = func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
			time.Sleep(d)
			return inner(c, params)
		}
		return opts
	}
}

// TestServerReapsIdleServersWithoutExiting is the M3 requirement in the
// clearest form available: an idle language server is shut down while
// the daemon that pooled it keeps running and keeps answering. gopls's
// daemon cannot make this distinction — its server is itself.
func TestServerReapsIdleServersWithoutExiting(t *testing.T) {
	root := workspace(t)
	ts := newTestServer(t, nil, PoolOptions{SessionIdleTimeout: 60 * time.Millisecond},
		ServerOptions{ListenTimeout: -1}) // never idle-exit, so only the reaper can act
	c := ts.dial(t)

	if _, err := c.Query(context.Background(), refsRequest(root)); err != nil {
		t.Fatalf("query: %v", err)
	}
	waitFor(t, "the daemon to reap its idle server", func() bool {
		st, err := c.Status(context.Background())
		return err == nil && len(st.Sessions) == 0
	})
	ts.fleet.assertAllExited(t)

	// The daemon is alive...
	select {
	case err := <-ts.serveErr:
		t.Fatalf("the daemon exited when its server was reaped: %v", err)
	default:
	}
	// ...and serves the next query by starting a fresh server.
	resp, err := c.Query(context.Background(), refsRequest(root))
	if err != nil {
		t.Fatalf("query after a reap: %v", err)
	}
	if resp.Warm {
		t.Error("the query after a reap reported a warm session")
	}
	if resp.Spawns != 2 {
		t.Errorf("the daemon reports %d spawns, want 2", resp.Spawns)
	}
}
