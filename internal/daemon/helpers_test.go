package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// This file holds the hermetic scaffolding the daemon tests share: a
// [Launcher] that starts internal/fakeserver in-process over a pipe
// instead of executing a language server, plus workspace and router
// fixtures. Nothing here touches the network or a real server, and
// every started fake is accounted for at the end of the test.

// fakeFleet is a Launcher plus the bookkeeping that makes the M3
// acceptance criterion testable. PLAN §8 states it as a latency claim
// ("second references returns in <200ms"), which cannot be checked
// hermetically without a real rust-analyzer. What can be checked — and
// is the actual mechanism behind the claim — is that the second query
// does not start a server: spawns stays at one. That assertion does not
// depend on timing at all.
type fakeFleet struct {
	// startDelay is how long the fake takes to answer `initialize`,
	// standing in for rust-analyzer's cold start.
	startDelay time.Duration

	// scenario builds the fakeserver script for a server definition
	// and root. Nil means [defaultScenario].
	scenario func(def *serverdef.ServerDef, root string) fakeserver.Options

	mu        sync.Mutex
	spawns    int
	instances []*fakeInstance
}

// fakeInstance is one running fake server.
type fakeInstance struct {
	def  string
	root string

	// conn is the client end of the connection.
	conn *client.Conn

	// script is the fake server's own side, for asserting on what
	// the client sent.
	script *fakeserver.Conn

	// exited is closed when fakeserver.Run returned, which happens
	// when the client sends `exit` or the pipes are closed. It is
	// how the tests prove a session was shut down and not merely
	// forgotten.
	exited  chan struct{}
	runErr  error
	closers []io.Closer
}

// kill severs the connection without an LSP exit, imitating a server
// that crashed.
func (i *fakeInstance) kill() {
	for _, c := range i.closers {
		_ = c.Close()
	}
}

func (f *fakeFleet) launcher() Launcher {
	return func(ctx context.Context, def *serverdef.ServerDef, root string) (*Instance, error) {
		// Two pipes: one per direction. The fake server runs in a
		// goroutine in this process, so a test never depends on a
		// binary being installed.
		clientRead, serverWrite := io.Pipe()
		serverRead, clientWrite := io.Pipe()

		inst := &fakeInstance{
			def:     def.Name,
			root:    root,
			exited:  make(chan struct{}),
			closers: []io.Closer{clientWrite, serverWrite, clientRead, serverRead},
		}

		scenario := f.scenario
		if scenario == nil {
			scenario = defaultScenario
		}
		opts := scenario(def, root)
		if f.startDelay > 0 {
			opts = withStartDelay(opts, f.startDelay)
		}
		captured := opts.OnStart
		opts.OnStart = func(c *fakeserver.Conn) {
			inst.script = c
			if captured != nil {
				captured(c)
			}
		}

		go func() {
			defer close(inst.exited)
			inst.runErr = fakeserver.Run(serverRead, serverWrite, opts)
		}()

		inst.conn = client.NewConn(clientRead, clientWrite)

		f.mu.Lock()
		f.spawns++
		f.instances = append(f.instances, inst)
		f.mu.Unlock()

		return &Instance{
			Conn: inst.conn,
			Wait: func(ctx context.Context) error {
				select {
				case <-inst.exited:
				case <-ctx.Done():
					// A real launcher kills the process here; the
					// fake just stops listening to it.
					inst.kill()
					return fmt.Errorf("fake server for %s did not exit: %w", root, ctx.Err())
				}
				inst.kill()
				return nil
			},
		}, nil
	}
}

func (f *fakeFleet) spawnCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.spawns
}

func (f *fakeFleet) all() []*fakeInstance {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeInstance(nil), f.instances...)
}

// assertAllExited fails unless every fake server this fleet started has
// left. It is the leak check: a daemon that forgets a session leaves a
// language server running, which on a real machine is a gigabyte of
// rust-analyzer nobody will ever reap.
func (f *fakeFleet) assertAllExited(t *testing.T) {
	t.Helper()
	for _, inst := range f.all() {
		select {
		case <-inst.exited:
		case <-time.After(2 * time.Second):
			t.Errorf("fake server %s for %s never exited", inst.def, inst.root)
		}
	}
}

// defaultScenario is a server that advertises the capabilities the
// tests query, never reports progress, and echoes.
func defaultScenario(def *serverdef.ServerDef, root string) fakeserver.Options {
	return fakeserver.Options{
		ServerName: "fake-" + def.Name,
		Capabilities: map[string]any{
			"referencesProvider": true,
			"definitionProvider": true,
			"hoverProvider":      true,
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
				return []any{map[string]any{
					"uri": "file://" + filepath.Join(root, "main.go"),
					"range": map[string]any{
						"start": map[string]any{"line": 1, "character": 2},
						"end":   map[string]any{"line": 1, "character": 5},
					},
				}}, nil
			},
		},
	}
}

// withStartDelay makes the fake slow to answer `initialize`, which is
// where a real server's cold start shows up: rust-analyzer answers the
// handshake only after it has looked at the workspace.
func withStartDelay(opts fakeserver.Options, d time.Duration) fakeserver.Options {
	inner := opts.Methods["initialize"]
	if opts.Methods == nil {
		opts.Methods = map[string]fakeserver.Method{}
	}
	opts.Methods["initialize"] = func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
		time.Sleep(d)
		if inner != nil {
			return inner(c, params)
		}
		caps := opts.Capabilities
		if caps == nil {
			caps = map[string]any{}
		}
		name := opts.ServerName
		if name == "" {
			name = "lightspeed-fakeserver"
		}
		return map[string]any{
			"capabilities": caps,
			"serverInfo":   map[string]any{"name": name, "version": "0.0.1"},
		}, nil
	}
	return opts
}

// indexingScenario is a server that begins a progress token and never
// ends it, and answers every query with an empty list: PLAN §5.2's
// dangerous case. Queries against it must exit 5, through the socket
// as well as in process.
func indexingScenario(def *serverdef.ServerDef, root string) fakeserver.Options {
	opts := defaultScenario(def, root)
	opts.Methods["textDocument/references"] = func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
		return []any{}, nil
	}
	opts.AfterInitialized = func(c *fakeserver.Conn) {
		_ = c.ProgressBegin("indexing", "Indexing")
		_ = c.ProgressReport("indexing", "crates", 10)
	}
	return opts
}

// fakeDef is a server definition that activates on one language and
// roots at go.mod. Its command is never executed: the tests replace the
// launcher.
func fakeDef(name, language, ext string, priority int) *serverdef.ServerDef {
	return &serverdef.ServerDef{
		SchemaVersion: serverdef.SchemaVersion,
		Name:          name,
		Activation: serverdef.Activation{
			Languages:   []string{language},
			Globs:       []string{"**/*" + ext},
			RootMarkers: []string{"go.mod"},
			Priority:    priority,
		},
		Server: serverdef.Server{Command: []string{"lightspeed-never-executed-" + name}},
	}
}

// goDef is the fixture used by most tests: one server for Go files.
func goDef() *serverdef.ServerDef { return fakeDef("fake-go", "go", ".go", 50) }

func testRouter(t *testing.T, defs ...*serverdef.ServerDef) *router.Router {
	t.Helper()
	if len(defs) == 0 {
		defs = []*serverdef.ServerDef{goDef()}
	}
	r, err := router.New(defs...)
	if err != nil {
		t.Fatalf("router.New: %v", err)
	}
	return r
}

// workspace creates a module-shaped directory with the given files and
// returns its canonical path. Canonical matters: on macOS /var is a
// symlink to /private/var, and a test that compares roots with the
// daemon's would otherwise fail for reasons that have nothing to do
// with the daemon.
func workspace(t *testing.T, files ...string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		files = []string{"main.go"}
	}
	for _, name := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("package fixture\n\nfunc Fixture() {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// fastGate keeps the readiness gate's windows short enough that a test
// suite stays fast. The gate's own tests cover its rules; here it only
// has to run.
func fastGate() client.GateOptions {
	return client.GateOptions{
		Settle:          20 * time.Millisecond,
		NoProgressGrace: 10 * time.Millisecond,
		PollInterval:    5 * time.Millisecond,
		Timeout:         2 * time.Second,
	}
}

// newTestPool builds a pool over the fleet and closes it when the test
// ends, so no test can leave a fake server running.
func newTestPool(t *testing.T, fleet *fakeFleet, opts PoolOptions) *Pool {
	t.Helper()
	if opts.Router == nil {
		opts.Router = testRouter(t)
	}
	opts.Launcher = fleet.launcher()
	if opts.Gate == (client.GateOptions{}) {
		opts.Gate = fastGate()
	}
	if opts.ShutdownTimeout == 0 {
		opts.ShutdownTimeout = 2 * time.Second
	}
	opts.Logf = t.Logf
	pool, err := NewPool(opts)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := pool.Close(ctx); err != nil {
			t.Errorf("pool.Close: %v", err)
		}
	})
	return pool
}

// waitFor polls until cond holds, and fails the test if it never does.
// Used for facts that are asynchronous by nature (a watcher noticing a
// dead server), never for facts a test could assert directly.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// exitCodeOf reports the PLAN §4 exit code an error carries, or -1 if
// it carries none. It is the same type assertion internal/render makes,
// spelled out here so the tests check the contract and not our helper.
func exitCodeOf(err error) int {
	var exiter interface{ ExitCode() int }
	if errors.As(err, &exiter) {
		return exiter.ExitCode()
	}
	return -1
}

func codeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}
