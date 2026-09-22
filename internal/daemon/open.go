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
	// Session starts or finds the server for a target and describes
	// what it advertised.
	Session(ctx context.Context, t Target) (*SessionInfo, error)
	// Open announces documents, with their content, to the server
	// for a target.
	Open(ctx context.Context, req OpenRequest) (*OpenResult, error)
	// CloseDocuments closes documents on the server for a target.
	CloseDocuments(ctx context.Context, req CloseRequest) error
	// FilesChanged tells the server for a target that files changed on
	// disk.
	FilesChanged(ctx context.Context, req ChangedRequest) error
	// Ready waits for the server to finish its initial work.
	Ready(ctx context.Context, req ReadyRequest) error
	// Diagnostics reports what the server has published about some
	// files.
	Diagnostics(ctx context.Context, req DiagnosticsRequest) (*DiagnosticsState, error)
	// Index runs one operation on the workspace's index, which the daemon (or
	// the in-process service) owns.
	Index(ctx context.Context, req IndexRequest) (*IndexResponse, error)
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

	// ConfigID identifies the server definitions this client resolved.
	// When it is set and the daemon it reaches was started with
	// different ones ([PoolOptions.ConfigID]), [Open] does not serve the
	// command from that daemon's warm sessions: a session started under
	// the old definitions would answer with a server, command or
	// settings the configuration no longer names. If this client is the
	// daemon's only one it is stopped and a fresh one started; if
	// another command is connected it is left alone and [Open] fails
	// with [CodeDaemonStale] (docs/DECISIONS.md D17). Empty disables the
	// check.
	ConfigID string

	// OnStale, if set, is told why a daemon is being restarted, before
	// it is. It is for a human-readable note on stderr.
	OnStale func(why string)

	// SkipBuildCheck serves the command from whatever daemon answers, even
	// one started by a different lightspeed build. By default [Open]
	// compares the daemon's [Build] with [SelfBuild] and replaces one that
	// differs — stopping it and starting this executable's own — because a
	// daemon from another binary would otherwise answer with that binary's
	// behaviour under this one's version (D29). A daemon that another
	// command is connected to is not replaced; [Open] fails with
	// [CodeDaemonStale] instead, as for a changed configuration. With
	// [Options.NoSpawn] the check is off as well: nothing that may not start
	// a daemon can replace one.
	SkipBuildCheck bool

	// OnReplaced, if set, is told that a daemon was replaced because it
	// was another build, and how it differed, after the replacement. It is
	// what the CLI puts in the envelope's warnings: a version mix-up that
	// was corrected is still something the caller should know happened.
	OnReplaced func(why string)

	// ExecutableCheck and ExecutableCurrent configure [Serve]'s watch on
	// its own executable; see [ServerOptions].
	ExecutableCheck   time.Duration
	ExecutableCurrent func() bool

	// IndexCacheDir overrides where the workspace's index is persisted; empty
	// means $XDG_CACHE_HOME/lightspeed/<workspace-hash> (IndexCacheDir).
	IndexCacheDir string

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
		svc := NewService(pool, root)
		if opts.IndexCacheDir != "" {
			svc.SetIndexCacheDir(opts.IndexCacheDir)
		}
		return svc, nil
	}

	socket, err := opts.SocketPath()
	if err != nil {
		return nil, err
	}
	co := ClientOptions{
		Socket:        socket,
		Spawn:         opts.Spawn,
		NoSpawn:       opts.NoSpawn,
		ListenTimeout: opts.ListenTimeout,
		DialTimeout:   opts.DialTimeout,
		SpawnTimeout:  opts.SpawnTimeout,
		Logf:          opts.Logf,
	}
	c, err := Dial(ctx, co)
	if err != nil {
		return nil, err
	}
	// A caller that may not start a daemon cannot replace one either: it
	// is looking at the daemon (`daemon status`, `daemon stop`), and must
	// see the stale one, not remove it.
	checkBuild := !opts.SkipBuildCheck && !opts.NoSpawn
	if opts.ConfigID == "" && !checkBuild {
		return c, nil
	}
	st, err := c.Status(ctx)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	// A daemon this call just started is this executable by construction
	// (or was configured not to be, by a test); only a pre-existing one can
	// be somebody else's.
	if checkBuild && !c.Spawned() {
		if why := SelfBuild().Differs(st.Build); why != "" {
			c, st, err = replaceBuild(ctx, c, st, why, opts, co)
			if err != nil {
				return nil, err
			}
		}
	}
	if opts.ConfigID == "" {
		return c, nil
	}
	return currentConfig(ctx, c, st, opts, co)
}

// replaceBuild stops a daemon that another lightspeed build started and
// returns a client of one started by this build, with its status.
//
// Idle daemons are replaced and busy ones refused, for D17's reason: the
// daemon is shared, and stopping it under a command that is mid-query
// turns that command's answer into a crash. The cost is that two builds in
// use at once — an installed binary and a checkout's, say — take turns
// replacing each other's daemon, discarding its warm servers each time, and
// each replacement is reported.
func replaceBuild(ctx context.Context, c *Client, st *Status, why string, opts Options, co ClientOptions) (*Client, *Status, error) {
	if st.Clients > 1 {
		_ = c.Close()
		return nil, nil, &Error{
			Code: CodeDaemonStale,
			Message: fmt.Sprintf("the daemon for %s (pid %d) was started by another lightspeed build (%s) and %d other command(s) are connected to it, so it was not replaced; run `lightspeed daemon stop` for this workspace and retry",
				st.Workspace, st.PID, why, st.Clients-1),
			Exit: exitUsage,
		}
	}
	fresh, err := restartDaemon(ctx, c, st, co)
	if err != nil {
		return nil, nil, err
	}
	fst, err := fresh.Status(ctx)
	if err != nil {
		_ = fresh.Close()
		return nil, nil, err
	}
	if opts.OnReplaced != nil {
		opts.OnReplaced(fmt.Sprintf("replaced the daemon for this workspace (pid %d), which was %s; warm language servers were discarded", st.PID, why))
	}
	return fresh, fst, nil
}

// restartDaemon stops the daemon c is connected to, waits for its process to
// go, and returns a client of a new one. It closes c.
func restartDaemon(ctx context.Context, c *Client, st *Status, co ClientOptions) (*Client, error) {
	stopErr := c.Stop(ctx)
	_ = c.Close()
	if stopErr != nil {
		return nil, stopErr
	}
	if err := waitGone(ctx, co.Socket, DefaultDrainTimeout+2*time.Second); err != nil {
		return nil, err
	}
	// The socket goes first; the old daemon is still shutting its servers
	// down for a while, and the new one must not overlap it.
	exitCtx, cancelExit := context.WithTimeout(ctx, DefaultDrainTimeout+2*time.Second)
	exitErr := WaitExit(exitCtx, st.PID)
	cancelExit()
	if exitErr != nil {
		return nil, &Error{Code: CodeInternal, Message: exitErr.Error(), Exit: exitCrash}
	}
	return Dial(ctx, co)
}

// currentConfig returns a client of a daemon that was started with the
// definitions opts.ConfigID names, restarting the one it was given if it
// was not and nobody else is using it.
//
// Restart-when-idle rather than always-refuse because the usual cause is
// somebody editing .lightspeed.toml and running the next command, and an
// error to be cleared by hand every time would teach callers to stop the
// daemon reflexively. Refuse-when-busy rather than always-restart because
// the daemon is shared: stopping it under a command that is mid-query
// turns that command's answer into a crash. A daemon that predates
// config tracking reports no id and so is stale by this test, which is
// the safe direction.
func currentConfig(ctx context.Context, c *Client, st *Status, opts Options, co ClientOptions) (Handle, error) {
	if st.ConfigID == opts.ConfigID {
		return c, nil
	}
	stale := func(why string) error {
		return &Error{
			Code: CodeDaemonStale,
			Message: fmt.Sprintf("the daemon for %s is running with different server definitions than this command resolved (%s); %s",
				st.Workspace, why, "run `lightspeed daemon stop` for this workspace and retry"),
			Exit: exitUsage,
		}
	}
	if c.Spawned() {
		// Restarting a daemon we started a moment ago would only
		// repeat the disagreement.
		_ = c.Close()
		return nil, stale("a daemon started by this command resolved something else, so the configuration changed while it started")
	}
	if st.Clients > 1 {
		_ = c.Close()
		return nil, stale(fmt.Sprintf("%d other command(s) are connected to it, so it was not restarted", st.Clients-1))
	}

	if opts.OnStale != nil {
		opts.OnStale("the server definitions changed since this workspace's daemon started; restarting it, which discards its warm language servers")
	}
	fresh, err := restartDaemon(ctx, c, st, co)
	if err != nil {
		return nil, err
	}
	st, err = fresh.Status(ctx)
	if err != nil {
		_ = fresh.Close()
		return nil, err
	}
	if st.ConfigID != opts.ConfigID {
		_ = fresh.Close()
		return nil, stale("a daemon restarted for this command resolved something else, so the configuration changed while it started")
	}
	return fresh, nil
}

// waitGone blocks until nothing answers on the socket, so that a daemon
// that has been told to stop is not raced by the one that replaces it.
func waitGone(ctx context.Context, socket string, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		nc, err := dialOnce(ctx, socket, 100*time.Millisecond)
		if err != nil {
			return nil
		}
		_ = nc.Close()
		if time.Now().After(deadline) {
			return &Error{
				Code:    CodeInternal,
				Message: fmt.Sprintf("daemon: the previous daemon on %s did not go away within %s", socket, budget),
				Exit:    exitCrash,
			}
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	if opts.IndexCacheDir != "" {
		service.SetIndexCacheDir(opts.IndexCacheDir)
	}

	server, err := NewServer(ServerOptions{
		Service:       service,
		Socket:        socket,
		ListenTimeout: opts.ListenTimeout,
		DrainTimeout:  opts.DrainTimeout,

		ExecutableCheck:   opts.ExecutableCheck,
		ExecutableCurrent: opts.ExecutableCurrent,
		Logf:              opts.Logf,
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
