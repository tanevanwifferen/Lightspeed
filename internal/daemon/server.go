package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
)

// Defaults for the socket server.
const (
	// DefaultListenTimeout is how long a daemon with no connected
	// clients waits before exiting — gopls's `-listen.timeout`, whose
	// autostarted default is one minute. Ours is much longer for the
	// reason in PLAN §3: the warm index this daemon exists to keep
	// costs 30–90 seconds to rebuild, so exiting eagerly defeats the
	// purpose. The per-server reaper
	// ([PoolOptions.SessionIdleTimeout]) is the finer-grained
	// mechanism, and it releases the memory long before this fires.
	DefaultListenTimeout = 30 * time.Minute

	// DefaultDrainTimeout bounds a graceful shutdown: how long
	// in-flight requests, and then the clients holding them, are
	// given to finish before the connections are closed underneath
	// them.
	DefaultDrainTimeout = 10 * time.Second

	// probeTimeout bounds the "is somebody already listening here?"
	// dial that precedes binding.
	probeTimeout = 250 * time.Millisecond

	// replyGrace is how long a shutdown waits for clients to read
	// their answers and disconnect, after every request in flight has
	// finished. It exists because the reply to the request that
	// caused the shutdown — `daemon stop` — is written just after the
	// handler returns, and closing the socket under it would turn a
	// successful stop into an I/O error.
	//
	// It is deliberately short and separate from DrainTimeout: work
	// in flight deserves the full drain, an idle connection does not.
	replyGrace = 500 * time.Millisecond
)

// ErrIdleTimeout is returned by [Server.Serve] when the daemon exits
// because no client connected for [ServerOptions.ListenTimeout]. It is
// a successful exit, not a failure: the CLI should exit 0 on it.
var ErrIdleTimeout = errors.New("daemon: exiting after idle timeout")

// ErrAlreadyRunning is returned by [Server.Listen] when a live daemon
// is already listening on the socket. Losing this race is also not a
// failure — the other daemon can serve the clients.
var ErrAlreadyRunning = errors.New("daemon: another daemon is already listening")

// ServerOptions configures a [Server].
type ServerOptions struct {
	// Service answers requests. Required.
	Service *Service

	// Socket is the unix socket to listen on; see [SocketPath].
	// Required.
	Socket string

	// ListenTimeout is the idle-exit deadline: with no client
	// connected for this long, Serve returns [ErrIdleTimeout]. Zero
	// means [DefaultListenTimeout]; negative means never exit.
	ListenTimeout time.Duration

	// DrainTimeout bounds a graceful shutdown; zero means
	// [DefaultDrainTimeout].
	DrainTimeout time.Duration

	// Logf logs daemon events. Nil discards them.
	Logf func(format string, args ...any)
}

func (o ServerOptions) withDefaults() ServerOptions {
	if o.ListenTimeout == 0 {
		o.ListenTimeout = DefaultListenTimeout
	}
	if o.DrainTimeout <= 0 {
		o.DrainTimeout = DefaultDrainTimeout
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// A Server is the daemon's socket end: it accepts clients on a unix
// socket, hands their requests to the [Service], exits when it has
// been idle for too long, and shuts down without dropping work that is
// in flight.
//
// The accept loop is gopls's jsonrpc2.Serve: one goroutine accepting,
// a connection counter, and a timer that runs only while the counter
// is zero. The differences are ours: the graceful drain, and the fact
// that outliving all clients does not mean outliving all servers — the
// pool's own reaper handles those independently.
type Server struct {
	opts    ServerOptions
	ln      net.Listener
	socket  string
	socketI uint64 // inode of the published socket, to recognise our own
	started time.Time

	mu       sync.Mutex
	cond     *sync.Cond
	conns    map[net.Conn]struct{}
	inflight int
	stopping bool

	stop       chan struct{} // closed to stop accepting
	done       chan struct{} // closed when Serve has returned
	closeOnce  sync.Once
	shutdownMu sync.Mutex
}

// NewServer returns a server for the service. It does not bind
// anything; call [Server.Listen] and then [Server.Serve], or
// [Server.ListenAndServe].
func NewServer(opts ServerOptions) (*Server, error) {
	if opts.Service == nil {
		return nil, errors.New("daemon: server needs a service")
	}
	if opts.Socket == "" {
		return nil, errors.New("daemon: server needs a socket path")
	}
	s := &Server{
		opts:  opts.withDefaults(),
		conns: map[net.Conn]struct{}{},
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)

	// Wire the service back to its transport: Status should report
	// where we listen and how many clients are attached, and
	// MethodStop has to trigger a graceful shutdown of *this*
	// server. The shutdown runs in its own goroutine so that the
	// reply to the stop request is written before the connection it
	// arrived on is taken away.
	svc := s.opts.Service
	svc.socket = s.opts.Socket
	svc.listenTimeout = s.opts.ListenTimeout
	svc.clients = s.clientCount
	svc.stop = func() {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.DrainTimeout)
			defer cancel()
			if err := s.Shutdown(ctx); err != nil {
				s.opts.Logf("shutdown: %v", err)
			}
		}()
	}
	return s, nil
}

// Addr reports the socket the server is listening on, empty before
// [Server.Listen] succeeds.
func (s *Server) Addr() string { return s.socket }

// Listen binds the socket. It handles the two ways a socket path can be
// occupied:
//
//   - A live daemon is listening: [ErrAlreadyRunning], and the caller
//     should simply use it.
//   - A stale socket file from a daemon that died: taken over.
//
// The takeover binds a temporary socket in the same directory and
// renames it onto the published path. Rename is atomic, so unlike
// remove-then-bind (what gopls's dialer does, with a TODO about the
// race) there is no window in which the address does not exist, and two
// daemons starting at once end with one of them addressable rather than
// both holding half of the address.
func (s *Server) Listen() error {
	path := s.opts.Socket
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("daemon: socket directory: %w", err)
	}
	if ours, err := ownedByUs(path); err == nil && !ours {
		return fmt.Errorf("daemon: socket %s is owned by a different user", path)
	}
	if c, err := net.DialTimeout("unix", path, probeTimeout); err == nil {
		c.Close()
		return fmt.Errorf("%w on %s", ErrAlreadyRunning, path)
	}

	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	_ = os.Remove(tmp)
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return fmt.Errorf("daemon: listening on %s: %w", tmp, err)
	}
	// The socket must not be reachable by other users even if the
	// runtime directory somehow is.
	if err := os.Chmod(tmp, 0o600); err != nil {
		ln.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: securing %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		ln.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("daemon: publishing %s: %w", path, err)
	}

	s.ln = ln
	s.socket = path
	s.socketI, _ = inode(path)
	s.started = time.Now()
	s.opts.Logf("listening on %s", path)
	return nil
}

// ListenAndServe binds the socket and serves until the daemon is idle,
// stopped, or ctx is done.
func (s *Server) ListenAndServe(ctx context.Context) error {
	if err := s.Listen(); err != nil {
		return err
	}
	return s.Serve(ctx)
}

// Serve accepts clients until [Server.Shutdown] is called, ctx is done,
// or no client has been connected for the listen timeout — in which
// case it returns [ErrIdleTimeout].
//
// Serve always leaves the pool shut down and the socket removed, so a
// daemon that exits on its idle timeout does not leave a stale socket
// for the next client to trip over.
func (s *Server) Serve(ctx context.Context) error {
	if s.ln == nil {
		return errors.New("daemon: Serve before Listen")
	}
	err := s.serve(ctx)

	// Whatever ended the loop, leave nothing behind: shut down the
	// language servers and unpublish the socket.
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.opts.DrainTimeout)
	defer cancel()
	if serr := s.Shutdown(shutCtx); serr != nil && err == nil {
		err = serr
	}
	return err
}

func (s *Server) serve(ctx context.Context) error {
	defer close(s.done)

	newConns := make(chan net.Conn)
	var acceptErr error
	go func() {
		defer close(newConns)
		for {
			nc, err := s.ln.Accept()
			if err != nil {
				acceptErr = err
				return
			}
			newConns <- nc
		}
	}()

	closed := make(chan struct{}, 1)
	idle := s.opts.ListenTimeout
	if idle < 0 {
		idle = time.Duration(math.MaxInt64) // ~290 years, as gopls puts it
	}
	timer := time.NewTimer(idle)
	defer timer.Stop()

	for {
		select {
		case nc, ok := <-newConns:
			if !ok {
				// The listener stopped. During shutdown that is us;
				// otherwise it is a real failure.
				if s.isStopping() {
					return nil
				}
				return fmt.Errorf("daemon: accept: %w", acceptErr)
			}
			if s.addConn(nc) == 1 {
				timer.Stop()
			}
			go func() {
				s.serveConn(ctx, nc)
				s.removeConn(nc)
				select {
				case closed <- struct{}{}:
				default:
				}
			}()

		case <-closed:
			if s.clientCount() == 0 {
				timer.Reset(idle)
			}

		case <-timer.C:
			if s.clientCount() > 0 {
				// A client connected while the timer was firing.
				continue
			}
			return ErrIdleTimeout

		case <-s.stop:
			return nil

		case <-ctx.Done():
			return nil
		}
	}
}

// serveConn drives one client connection until the client hangs up or
// the server takes the connection away.
//
// Requests on one connection are answered in order, because the
// handler runs on internal/client's read loop. That is the right
// trade-off for the CLI, which sends one request per invocation:
// concurrency between *clients* — the property PLAN §3 needs — comes
// from separate connections, each with its own goroutine and read
// loop. Pipelining several requests down one connection is deferred
// until there is a batch mode to want it.
func (s *Server) serveConn(ctx context.Context, nc net.Conn) {
	conn := client.NewConn(nc, nc)
	conn.SetRequestHandler(func(_ context.Context, method string, params json.RawMessage) (any, error) {
		return s.handle(ctx, method, params)
	})
	<-conn.Done()
	_ = nc.Close()
}

// handle answers one protocol request, counting it as in flight so a
// graceful shutdown waits for it.
func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	s.mu.Lock()
	s.inflight++
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.cond.Broadcast()
		s.mu.Unlock()
	}()

	result, err := s.opts.Service.dispatch(ctx, method, params)
	if err != nil {
		if errors.Is(err, client.ErrMethodNotFound) {
			return nil, err // answered as JSON-RPC MethodNotFound
		}
		return nil, toRPC(err)
	}
	return result, nil
}

func (s *Server) addConn(nc net.Conn) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.conns[nc] = struct{}{}
	return len(s.conns)
}

func (s *Server) removeConn(nc net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, nc)
	s.cond.Broadcast()
}

// clientCount reports how many clients are connected right now.
func (s *Server) clientCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func (s *Server) isStopping() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopping
}

// Shutdown stops the daemon gracefully, in this order: stop accepting
// new clients, unpublish the socket, let the requests in flight finish,
// let their clients read the answers, then close what is left and shut
// down every pooled language server.
//
// The order is the point. Unpublishing first means a client that
// arrives during the shutdown fails to dial and spawns a fresh daemon
// instead of connecting to one that is leaving. Draining before closing
// means an agent's `references` call in flight returns its answer
// rather than an I/O error.
//
// Shutdown is idempotent and safe to call concurrently.
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutdownMu.Lock()
	defer s.shutdownMu.Unlock()

	s.mu.Lock()
	already := s.stopping
	s.stopping = true
	s.mu.Unlock()

	var errs []error
	if !already {
		s.closeOnce.Do(func() { close(s.stop) })

		if s.ln != nil {
			if err := s.ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		s.unpublish()

		// Requests in flight get the whole drain budget; the clients
		// holding them get a brief grace afterwards to read their
		// answers and hang up, which is all a client that has been
		// answered needs.
		if err := s.waitFor(ctx, func() bool { return s.inflight == 0 }); err != nil {
			errs = append(errs, fmt.Errorf("daemon: requests still in flight at shutdown: %w", err))
		}
		graceCtx, graceCancel := context.WithTimeout(context.WithoutCancel(ctx), replyGrace)
		_ = s.waitFor(graceCtx, func() bool { return len(s.conns) == 0 })
		graceCancel()

		s.mu.Lock()
		for nc := range s.conns {
			_ = nc.Close()
		}
		s.mu.Unlock()

		if err := s.opts.Service.pool.Close(ctx); err != nil {
			errs = append(errs, err)
		}
		s.opts.Logf("stopped")
	}

	// Whoever called Shutdown wants the daemon actually down, so
	// wait for the accept loop to notice — but only if it is running,
	// and never past ctx.
	select {
	case <-s.done:
	case <-ctx.Done():
	case <-time.After(50 * time.Millisecond):
	}
	return errors.Join(errs...)
}

// waitFor blocks until cond holds or ctx is done. cond is evaluated
// under s.mu.
func (s *Server) waitFor(ctx context.Context, cond func() bool) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			s.mu.Lock()
			s.cond.Broadcast()
			s.mu.Unlock()
		case <-stop:
		}
	}()

	s.mu.Lock()
	defer s.mu.Unlock()
	for !cond() && ctx.Err() == nil {
		s.cond.Wait()
	}
	if !cond() {
		return ctx.Err()
	}
	return nil
}

// unpublish removes the socket file, but only if it is still the one we
// published: another daemon may have renamed its own socket over the
// path, and deleting that would strand it with clients unable to find
// it.
func (s *Server) unpublish() {
	if s.socket == "" {
		return
	}
	if s.socketI != 0 {
		if ino, err := inode(s.socket); err == nil && ino != s.socketI {
			s.opts.Logf("socket %s now belongs to another daemon; leaving it", s.socket)
			return
		}
	}
	if err := os.Remove(s.socket); err != nil && !os.IsNotExist(err) {
		s.opts.Logf("removing socket %s: %v", s.socket, err)
	}
}
