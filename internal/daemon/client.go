package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
)

// Defaults for the client half.
const (
	// DefaultDialTimeout bounds one attempt to reach the daemon.
	// gopls uses one second, and the socket is local: if it takes
	// longer than this, the daemon is not there.
	DefaultDialTimeout = 1 * time.Second

	// DefaultSpawnTimeout is how long a client keeps trying to reach
	// a daemon it has just started before giving up.
	DefaultSpawnTimeout = 10 * time.Second

	// spawnPollInitial and spawnPollMax bound the backoff between
	// those attempts.
	//
	// This is the one place the port deliberately departs from
	// gopls, which retries five times and paces the retries at the
	// full dial timeout — so a cold start there costs a second or
	// more even when the daemon has bound its socket in twenty
	// milliseconds. Since M3's whole subject is latency, we poll
	// with a short doubling backoff inside one budget instead.
	spawnPollInitial = 20 * time.Millisecond
	spawnPollMax     = 250 * time.Millisecond
)

// ErrNotRunning reports that no daemon is listening on the socket and
// the client was told not to start one. `daemon status` and `daemon
// stop` want to say "nothing is running" rather than fail.
var ErrNotRunning = errors.New("daemon: no daemon is running")

// A SpawnConfig says how to start a daemon that is not running. The
// binary is lightspeed itself: the daemon is not a separate program,
// it is this program in another mode, exactly as gopls re-executes
// itself with `serve -listen=…`.
type SpawnConfig struct {
	// Executable is the binary to run; empty means os.Executable().
	Executable string

	// Args builds the daemon's argument list for a socket path.
	// Empty means [DefaultSpawnArgs] with the client's listen
	// timeout. It is a hook rather than a constant because the
	// command names belong to internal/cli, and a test points it at
	// itself.
	Args func(socket string) []string

	// Env is the daemon's environment; nil inherits the client's.
	// The daemon runs language servers, which read PATH, GOFLAGS,
	// CARGO_HOME and much else, so inheriting is the sane default.
	Env []string

	// Dir is the daemon's working directory; empty inherits.
	Dir string

	// Logfile, if set, receives the daemon's own stderr — including
	// every language server's stderr. Without it that output goes to
	// /dev/null, because it must never reach the client's stdout,
	// where the JSON envelope lives.
	Logfile string
}

// DefaultSpawnArgs is the command line a client uses to start the
// daemon. The flag names mirror gopls's (`-listen`, `-listen.timeout`)
// so that anyone who has debugged a gopls daemon recognises them.
//
// internal/cli owns the actual commands; this is the shape they are
// expected to take, and the one thing both sides have to agree on.
func DefaultSpawnArgs(socket string, listenTimeout time.Duration) []string {
	args := []string{"daemon", "serve", "--listen", socket}
	if listenTimeout != 0 {
		args = append(args, "--listen.timeout", listenTimeout.String())
	}
	return args
}

// ClientOptions configures [Dial].
type ClientOptions struct {
	// Socket is the daemon's address. Required.
	Socket string

	// Spawn describes how to start a daemon when none is listening.
	Spawn SpawnConfig

	// NoSpawn forbids starting one: [Dial] then reports
	// [ErrNotRunning] instead. `daemon status` uses this.
	NoSpawn bool

	// ListenTimeout is passed to a spawned daemon through
	// [DefaultSpawnArgs].
	ListenTimeout time.Duration

	// DialTimeout bounds one dial attempt; zero means
	// [DefaultDialTimeout].
	DialTimeout time.Duration

	// SpawnTimeout bounds the wait for a daemon this client started
	// to become reachable; zero means [DefaultSpawnTimeout].
	SpawnTimeout time.Duration

	// Logf logs client events. Nil discards them.
	Logf func(format string, args ...any)
}

// A Client is the thin forwarding end: it holds one connection to the
// daemon and turns [Handle] calls into requests on it. The CLI process
// does no LSP work of its own, which is what makes a warm query cheap —
// all it pays is a socket round trip.
type Client struct {
	conn    *client.Conn
	nc      net.Conn
	socket  string
	spawned bool
}

// Dial connects to the daemon on opts.Socket, starting one if none is
// listening and spawning is allowed.
//
// This is gopls's autoDialer.dialNet: verify we own the socket, try
// once in case a daemon is already up, otherwise start one and retry
// with a bounded number of attempts, because a freshly started daemon
// takes a moment to bind.
func Dial(ctx context.Context, opts ClientOptions) (*Client, error) {
	if opts.Socket == "" {
		return nil, errors.New("daemon: dial needs a socket path")
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	timeout := opts.DialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}

	// Fail closed on a socket someone else owns: on the other end of
	// it would be another user's process, with this workspace's file
	// contents flowing through it.
	if ours, err := ownedByUs(opts.Socket); err != nil {
		logf("cannot check socket ownership, continuing: %v", err)
	} else if !ours {
		return nil, &Error{
			Code:    CodeInternal,
			Message: fmt.Sprintf("daemon: socket %s is owned by a different user", opts.Socket),
			Exit:    exitCrash,
		}
	}

	if nc, err := dialOnce(ctx, opts.Socket, timeout); err == nil {
		return newClient(nc, opts.Socket, false), nil
	}
	if opts.NoSpawn {
		return nil, fmt.Errorf("%w on %s", ErrNotRunning, opts.Socket)
	}

	if err := spawnDaemon(opts); err != nil {
		return nil, err
	}
	logf("started daemon for %s", opts.Socket)

	budget := opts.SpawnTimeout
	if budget <= 0 {
		budget = DefaultSpawnTimeout
	}
	deadline := time.Now().Add(budget)
	backoff := spawnPollInitial
	var lastErr error
	for {
		nc, err := dialOnce(ctx, opts.Socket, timeout)
		if err == nil {
			return newClient(nc, opts.Socket, true), nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		wait := min(backoff, time.Until(deadline))
		if wait <= 0 {
			break
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff = min(2*backoff, spawnPollMax)
	}
	return nil, &Error{
		Code:    CodeSpawnFailed,
		Message: fmt.Sprintf("daemon: started a daemon but could not reach %s within %s: %v", opts.Socket, budget, lastErr),
		Exit:    exitCrash,
	}
}

func dialOnce(ctx context.Context, socket string, timeout time.Duration) (net.Conn, error) {
	d := net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "unix", socket)
}

func newClient(nc net.Conn, socket string, spawned bool) *Client {
	return &Client{conn: client.NewConn(nc, nc), nc: nc, socket: socket, spawned: spawned}
}

// spawnDaemon starts the daemon process, detached from the client's
// session so that the shell that happened to run the first query does
// not own the daemon's lifetime.
func spawnDaemon(opts ClientOptions) error {
	exe := opts.Spawn.Executable
	if exe == "" {
		found, err := os.Executable()
		if err != nil {
			return &Error{
				Code:    CodeSpawnFailed,
				Message: fmt.Sprintf("daemon: cannot find own executable: %v", err),
				Exit:    exitCrash,
			}
		}
		exe = found
	}
	args := opts.Spawn.Args
	if args == nil {
		listen := opts.ListenTimeout
		args = func(socket string) []string { return DefaultSpawnArgs(socket, listen) }
	}

	cmd := exec.Command(exe, args(opts.Socket)...)
	cmd.Env = opts.Spawn.Env
	cmd.Dir = opts.Spawn.Dir
	if opts.Spawn.Logfile != "" {
		f, err := os.OpenFile(opts.Spawn.Logfile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return &Error{
				Code:    CodeSpawnFailed,
				Message: fmt.Sprintf("daemon: opening logfile %s: %v", opts.Spawn.Logfile, err),
				Exit:    exitCrash,
			}
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	daemonize(cmd)
	if err := cmd.Start(); err != nil {
		return &Error{
			Code:    CodeSpawnFailed,
			Message: fmt.Sprintf("daemon: starting %s: %v", exe, err),
			Exit:    exitCrash,
		}
	}
	// The daemon outlives us; do not keep it as a child we will
	// never wait for.
	_ = cmd.Process.Release()
	return nil
}

// Socket reports the address this client is connected to.
func (c *Client) Socket() string { return c.socket }

// Spawned reports whether this client had to start the daemon — the
// invocation that paid for the language server's cold start.
func (c *Client) Spawned() bool { return c.spawned }

// Remote reports that the work happens in another process.
func (c *Client) Remote() bool { return true }

// Query runs one request on the daemon.
func (c *Client) Query(ctx context.Context, req Request) (*Response, error) {
	var resp Response
	if err := c.call(ctx, MethodQuery, req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Status describes the daemon and its pooled servers.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	var st Status
	if err := c.call(ctx, MethodStatus, struct{}{}, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// Handshake exchanges identity with the daemon. A daemon whose
// executable differs from ours is a daemon from another build: it will
// answer requests perfectly well, but its behaviour is that of the
// binary it was started from, which is worth knowing during
// development. gopls does the same check and only logs about it.
func (c *Client) Handshake(ctx context.Context) (*Handshake, error) {
	var hs Handshake
	if err := c.call(ctx, MethodHandshake, struct{}{}, &hs); err != nil {
		return nil, err
	}
	return &hs, nil
}

// Stop asks the daemon to shut down gracefully. The daemon answers
// before it goes, so a nil error means the shutdown started, not that
// it has finished.
func (c *Client) Stop(ctx context.Context) error {
	return c.call(ctx, MethodStop, struct{}{}, nil)
}

// Close closes the connection to the daemon, which keeps running.
func (c *Client) Close() error {
	if c.nc == nil {
		return nil
	}
	return c.nc.Close()
}

// call issues one request and decodes the reply, restoring the
// classification of a daemon-side error so the caller cannot tell
// whether the work happened here or over there.
func (c *Client) call(ctx context.Context, method string, params, result any) error {
	raw, err := c.conn.Call(ctx, method, params)
	if err != nil {
		if errors.Is(err, client.ErrConnClosed) {
			return &Error{
				Code:    CodeDaemonClosed,
				Message: fmt.Sprintf("daemon: connection to %s closed while calling %s", c.socket, method),
				Exit:    exitCrash,
			}
		}
		return fromRPC(err)
	}
	if result == nil {
		return nil
	}
	if err := json.Unmarshal(raw, result); err != nil {
		return &Error{
			Code:    CodeInternal,
			Message: fmt.Sprintf("daemon: malformed %s reply: %v", method, err),
			Exit:    exitCrash,
		}
	}
	return nil
}
