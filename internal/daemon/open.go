package daemon

import (
	"context"
	"fmt"
	"os"
	"time"
)

// A Handle is the API a lightspeed command uses to reach a language
// server, whether the work happens in a shared daemon or in this
// process. [Open] returns one; [Client] and [Service] implement it.
//
// The interface is deliberately coarse. One Query is one round trip
// carrying an LSP method and its params, so a warm command pays a
// socket round trip and nothing else — no handshake, no capability
// negotiation, no document synchronisation from the client side.
type Handle interface {
	// Query runs one LSP request against the server that handles
	// req.Path, starting it if needed.
	Query(ctx context.Context, req Request) (*Response, error)
	// Status describes the daemon (or the in-process pool) and its
	// servers.
	Status(ctx context.Context) (*Status, error)
	// Stop shuts the daemon down gracefully. In the in-process mode
	// it shuts down the pool.
	Stop(ctx context.Context) error
	// Close releases the handle. For a client that closes the
	// connection and leaves the daemon running; for the in-process
	// mode it shuts every language server down, and skipping it
	// leaks processes.
	Close() error
	// Remote reports whether the work happens in another process.
	Remote() bool
}

var (
	_ Handle = (*Client)(nil)
	_ Handle = (*Service)(nil)
)

// Options configures [Open] and [Serve]: how to find the daemon, and
// how the pool behind it should behave.
type Options struct {
	// Pool configures the language server pool. Pool.Router is
	// required in the in-process mode and in [Serve]; a plain client
	// does not need it, because the daemon does the routing.
	Pool PoolOptions

	// Path is the file or directory the command is about. It decides
	// which workspace — and therefore which daemon — is addressed.
	// Empty means the working directory.
	Path string

	// Workspace overrides the resolved workspace root, i.e. the
	// daemon's identity. Empty means [Workspace] of Path.
	Workspace string

	// Socket overrides the socket path outright. Empty means
	// [SocketPathIn] of RuntimeDir and the workspace root.
	Socket string

	// RuntimeDir overrides where sockets live; empty means
	// [RuntimeDir], i.e. $XDG_RUNTIME_DIR/lightspeed (PLAN §3).
	RuntimeDir string

	// NoDaemon runs the pool in this process instead of talking to a
	// daemon — PLAN §3's `--no-daemon`, for CI and debugging.
	// Nothing is spawned, dialled or listened on.
	NoDaemon bool

	// NoSpawn connects to a running daemon but never starts one.
	NoSpawn bool

	// Spawn describes how to start a daemon.
	Spawn SpawnConfig

	// ListenTimeout is the daemon's idle-exit deadline: passed to a
	// daemon this client starts, and used by [Serve].
	ListenTimeout time.Duration

	// DrainTimeout bounds [Serve]'s graceful shutdown.
	DrainTimeout time.Duration

	// DialTimeout bounds one dial attempt.
	DialTimeout time.Duration

	// SpawnTimeout bounds the wait for a daemon this process started
	// to become reachable.
	SpawnTimeout time.Duration

	// Logf logs daemon and client events. Nil discards them.
	Logf func(format string, args ...any)
}

// WorkspaceRoot resolves the workspace root these options address: the
// override if there is one, otherwise the workspace of Path, otherwise
// of the working directory.
func (o Options) WorkspaceRoot() (string, error) {
	if o.Workspace != "" {
		return canonical(o.Workspace)
	}
	path := o.Path
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", fmt.Errorf("daemon: %w", err)
		}
		path = cwd
	}
	return Workspace(path)
}

// SocketPath resolves the socket these options address.
func (o Options) SocketPath() (string, error) {
	if o.Socket != "" {
		return o.Socket, nil
	}
	root, err := o.WorkspaceRoot()
	if err != nil {
		return "", err
	}
	dir := o.RuntimeDir
	if dir == "" {
		d, err := RuntimeDir()
		if err != nil {
			return "", err
		}
		dir = d
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("daemon: runtime directory %s: %w", dir, err)
	}
	return SocketPathIn(dir, root), nil
}

// Open returns the handle a command should use: a [Client] connected to
// the shared daemon — started on the spot if it was not running — or,
// with [Options.NoDaemon], a [Service] whose pool lives in this
// process.
//
// Both modes run the same [Service] code and produce the same errors
// with the same exit codes. That is the point of routing them through
// one API: `--no-daemon` is a way to see the same behaviour without the
// socket, not a second implementation to keep in sync.
func Open(ctx context.Context, opts Options) (Handle, error) {
	if opts.NoDaemon {
		root, err := opts.WorkspaceRoot()
		if err != nil {
			return nil, err
		}
		pool, err := NewPool(opts.Pool)
		if err != nil {
			return nil, err
		}
		return NewService(pool, root), nil
	}

	socket, err := opts.SocketPath()
	if err != nil {
		return nil, err
	}
	return Dial(ctx, ClientOptions{
		Socket:        socket,
		Spawn:         opts.Spawn,
		NoSpawn:       opts.NoSpawn,
		ListenTimeout: opts.ListenTimeout,
		DialTimeout:   opts.DialTimeout,
		SpawnTimeout:  opts.SpawnTimeout,
		Logf:          opts.Logf,
	})
}

// Serve runs the daemon itself: build the pool, publish the socket, and
// serve until the daemon is idle, stopped, or ctx is done. It is what
// `lightspeed daemon serve` should call.
//
// [ErrIdleTimeout] and [ErrAlreadyRunning] are returned as errors but
// are both successful outcomes — the first means the daemon did its job
// and left, the second that another daemon is already doing it — so the
// command should exit 0 on either.
func Serve(ctx context.Context, opts Options) error {
	root, err := opts.WorkspaceRoot()
	if err != nil {
		return err
	}
	socket, err := opts.SocketPath()
	if err != nil {
		return err
	}

	poolOpts := opts.Pool
	if poolOpts.Logf == nil {
		poolOpts.Logf = opts.Logf
	}
	pool, err := NewPool(poolOpts)
	if err != nil {
		return err
	}
	service := NewService(pool, root)

	server, err := NewServer(ServerOptions{
		Service:       service,
		Socket:        socket,
		ListenTimeout: opts.ListenTimeout,
		DrainTimeout:  opts.DrainTimeout,
		Logf:          opts.Logf,
	})
	if err != nil {
		closeCtx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
		defer cancel()
		_ = pool.Close(closeCtx)
		return err
	}

	err = server.ListenAndServe(ctx)
	if server.Addr() == "" {
		// Binding failed — the socket was taken by a live daemon, or
		// the directory is not writable — so Serve never ran and
		// never cleaned up. The pool exists either way; leave
		// nothing behind.
		closeCtx, cancel := context.WithTimeout(context.Background(), DefaultShutdownTimeout)
		defer cancel()
		_ = pool.Close(closeCtx)
	}
	return err
}
