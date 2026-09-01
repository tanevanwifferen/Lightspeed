package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// Defaults for the pool. These are not gopls's numbers, and the reason
// is PLAN §3: rust-analyzer needs 30–90 seconds to index and jdtls
// minutes, so throwing a warm session away is far more expensive here
// than it is for gopls, whose autostarted daemon defaults to a
// one-minute listen timeout.
const (
	// DefaultSessionIdleTimeout is how long an unused language
	// server is kept alive. Ten minutes is long enough to survive a
	// coffee break and short enough that six abandoned rust-analyzers
	// do not sit on the machine's memory all afternoon.
	DefaultSessionIdleTimeout = 10 * time.Minute
	// DefaultStartTimeout bounds one server's spawn and initialize
	// handshake. It bounds the *handshake*, not indexing: a server
	// that has answered `initialize` is started, however unready it
	// still is. That distinction is the readiness gate's job.
	DefaultStartTimeout = 60 * time.Second
	// DefaultShutdownTimeout bounds the polite LSP exit of one
	// server before it is killed.
	DefaultShutdownTimeout = 5 * time.Second
)

// PoolOptions configures a [Pool].
type PoolOptions struct {
	// Router resolves paths to servers and roots. Required.
	Router *router.Router

	// Launcher starts a server process; nil means [ExecLauncher]
	// over Stderr.
	Launcher Launcher

	// SessionIdleTimeout is how long an idle server is kept before
	// the reaper shuts it down; zero means
	// [DefaultSessionIdleTimeout], negative means never reap.
	SessionIdleTimeout time.Duration

	// StartTimeout bounds spawn plus handshake for one server.
	StartTimeout time.Duration

	// ShutdownTimeout bounds one server's polite exit.
	ShutdownTimeout time.Duration

	// Gate configures the readiness gate of every session
	// (PLAN §5.2).
	Gate client.GateOptions

	// Docstore configures the per-session open-document store.
	Docstore docstore.Options

	// Stderr receives the language servers' stderr. Nil discards it.
	// It must never be the stdout carrying the JSON envelope.
	Stderr io.Writer

	// Logf logs pool events (spawns, reaps, deaths). Nil discards.
	Logf func(format string, args ...any)
}

func (o PoolOptions) withDefaults() PoolOptions {
	if o.SessionIdleTimeout == 0 {
		o.SessionIdleTimeout = DefaultSessionIdleTimeout
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = DefaultStartTimeout
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = DefaultShutdownTimeout
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	if o.Logf == nil {
		o.Logf = func(string, ...any) {}
	}
	return o
}

// A Pool is the daemon's set of live language servers, keyed by
// (server definition, resolved workspace root). It is PLAN §1 "build
// ourselves" item 2: gopls's daemon has one server, this has N
// heterogeneous ones, each with its own lifecycle, its own readiness
// gate, its own document store and its own idle deadline.
//
// A Pool is safe for concurrent use. Two clients asking for the same
// session while it is still starting wait for the one spawn rather
// than starting two servers — which is the single most important
// property in the package, because that spawn is the 30–90 seconds
// PLAN §3 exists to avoid.
type Pool struct {
	opts PoolOptions

	// spawns counts every server this pool has started, ever. It is
	// the honest, timing-independent way to see whether a warm
	// session was reused: two queries and one spawn.
	spawns atomic.Int64

	mu       sync.Mutex
	cond     *sync.Cond // signalled when inflight drops or an entry finishes starting
	entries  map[sessionKey]*poolEntry
	inflight int
	closed   bool

	reaperStop chan struct{}
	reaperDone chan struct{}
}

// sessionKey identifies one language server session. The root is part
// of the key: two Go modules in one repository are two gopls sessions,
// and PLAN §3's server pool is exactly this map.
type sessionKey struct {
	server string
	root   string
}

// poolEntry is one session, possibly still starting.
type poolEntry struct {
	key        sessionKey
	def        *serverdef.ServerDef
	root       string
	rootMarker string

	// ready is closed when the start attempt has finished. The
	// fields below it are written before that close and only read
	// after it, so the channel is their memory barrier.
	ready chan struct{}
	err   error
	inst  *Instance
	sess  *client.Session
	docs  *docstore.Store

	// Guarded by Pool.mu.
	started  time.Time
	lastUse  time.Time
	inflight int
	requests int64
	dead     bool
	retired  bool
}

// NewPool returns a pool over the given router and starts its idle
// reaper. Close shuts down every session it holds.
func NewPool(opts PoolOptions) (*Pool, error) {
	if opts.Router == nil {
		return nil, errors.New("daemon: pool needs a router")
	}
	p := &Pool{
		opts:    opts.withDefaults(),
		entries: map[sessionKey]*poolEntry{},
	}
	p.cond = sync.NewCond(&p.mu)
	p.startReaper()
	return p, nil
}

// Spawns reports how many language servers this pool has started since
// it was created.
func (p *Pool) Spawns() int64 { return p.spawns.Load() }

// A Target says which session a request needs: the file it is about,
// and optionally the language id to assume and the server definition
// to insist on.
type Target struct {
	// Path is the file or directory the request concerns.
	Path string
	// LanguageID overrides language detection; "" means detect.
	LanguageID string
	// Server, if set, is the server definition name to use, rather
	// than the highest-priority one that claims the path.
	Server string
}

// A Lease is a borrowed session. It exists so that a request in flight
// cannot have its server reaped or shut down underneath it: the idle
// reaper skips leased sessions, and a graceful shutdown waits for
// every lease to be released. Release exactly once, and do not use the
// session afterwards.
type Lease struct {
	pool  *Pool
	entry *poolEntry
	warm  bool
	once  sync.Once
}

// Session returns the LSP session: capability-checked calls, gated
// queries, progress (internal/client).
func (l *Lease) Session() *client.Session { return l.entry.sess }

// Docs returns the session's open-document store. Documents stay open
// across requests, which is half of what makes the second query fast.
func (l *Lease) Docs() *docstore.Store { return l.entry.docs }

// Server returns the definition serving this lease.
func (l *Lease) Server() *serverdef.ServerDef { return l.entry.def }

// Root returns the workspace root the session was initialized with.
func (l *Lease) Root() string { return l.entry.root }

// Warm reports whether the session already existed when the lease was
// taken — false only for the caller whose request caused the spawn.
func (l *Lease) Warm() bool { return l.warm }

// Release returns the lease. It is idempotent.
func (l *Lease) Release() {
	l.once.Do(func() {
		p := l.pool
		p.mu.Lock()
		l.entry.inflight--
		p.inflight--
		l.entry.lastUse = time.Now()
		p.cond.Broadcast()
		p.mu.Unlock()
	})
}

// Acquire resolves a target to a session, starting the server if it is
// not already running, and returns a lease on it.
//
// Concurrent acquisitions of the same session share one spawn: the
// first caller starts the server, the others wait for it. A caller
// whose context expires while waiting gives up without cancelling the
// spawn, because the next caller — or the one already waiting — still
// wants it.
func (p *Pool) Acquire(ctx context.Context, t Target) (*Lease, error) {
	m, err := p.resolve(t)
	if err != nil {
		return nil, err
	}

	// The retry loop exists for one case: a session that died (its
	// server crashed) between being found in the map and being
	// leased. Then it is evicted and started again. Two retries is
	// plenty; a server that dies twice in a row is broken, and
	// looping on it would hide that.
	for attempt := 0; attempt < 3; attempt++ {
		e, mine, err := p.entryFor(m)
		if err != nil {
			return nil, err
		}
		if mine {
			go p.start(e)
		}
		select {
		case <-e.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if e.err != nil {
			return nil, e.err
		}
		if lease, ok := p.lease(e); ok {
			return lease, nil
		}
		p.evict(e, "session was gone when it was leased")
	}
	return nil, &Error{
		Code:    CodeServerCrash,
		Message: fmt.Sprintf("server %q for %s keeps dying at startup", m.Server.Name, m.Root),
		Exit:    exitCrash,
		Server:  m.Server.Name,
		Root:    m.Root,
	}
}

// resolve asks the router which server handles the target.
func (p *Pool) resolve(t Target) (router.Match, error) {
	if t.Path == "" {
		return router.Match{}, &Error{
			Code:    CodeUsage,
			Message: "daemon: request has no path, so no server can be selected",
			Exit:    exitUsage,
		}
	}
	matches, err := p.opts.Router.ResolveAs(t.Path, t.LanguageID)
	if err != nil {
		return router.Match{}, err
	}
	if t.Server == "" {
		return matches[0], nil
	}
	for _, m := range matches {
		if m.Server.Name == t.Server {
			return m, nil
		}
	}
	return router.Match{}, &Error{
		Code:    CodeNoServer,
		Message: fmt.Sprintf("server %q does not handle %s", t.Server, t.Path),
		Exit:    exitNoServer,
		Server:  t.Server,
	}
}

// entryFor finds or creates the entry for a match. The second result
// says whether the caller is the one that must start it.
func (p *Pool) entryFor(m router.Match) (*poolEntry, bool, error) {
	key := sessionKey{server: m.Server.Name, root: m.Root}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, ErrDaemonClosed
	}
	if e, ok := p.entries[key]; ok && !e.dead {
		return e, false, nil
	}
	e := &poolEntry{
		key:        key,
		def:        m.Server,
		root:       m.Root,
		rootMarker: m.RootMarker,
		ready:      make(chan struct{}),
	}
	p.entries[key] = e
	return e, true, nil
}

// start spawns and initializes one server. It runs in its own
// goroutine on a context of its own, so that the client which happened
// to trigger the spawn cannot cancel it out from under the clients
// waiting behind it.
func (p *Pool) start(e *poolEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), p.opts.StartTimeout)
	defer cancel()

	defer close(e.ready)

	p.opts.Logf("spawning %s for %s", e.def.Name, e.root)

	launch := launcherFor(p.opts.Launcher, p.opts.Stderr)
	inst, err := launch(ctx, e.def, e.root)
	if err != nil {
		e.err = classifyLaunchError(e.def, err)
		p.forget(e)
		return
	}

	sess, err := client.Connect(ctx, inst.Conn, client.SessionOptions{
		RootDir:               e.root,
		InitializationOptions: initOptions(e.def),
		Settings:              settings(e.def),
		Gate:                  p.opts.Gate,
	})
	if err != nil {
		// The process is up but useless; reap it before giving up,
		// or the daemon leaks one server per failed handshake.
		reapCtx, reapCancel := context.WithTimeout(context.Background(), p.opts.ShutdownTimeout)
		defer reapCancel()
		if inst.Wait != nil {
			_ = inst.Wait(reapCtx)
		}
		e.err = &Error{
			Code:    CodeSpawnFailed,
			Message: fmt.Sprintf("server %q: initialize failed: %v", e.def.Name, err),
			Exit:    exitCrash,
			Server:  e.def.Name,
			Root:    e.root,
		}
		p.forget(e)
		return
	}

	e.inst = inst
	e.sess = sess
	e.docs = docstore.New(sess, p.opts.Docstore)
	p.spawns.Add(1)

	now := time.Now()
	p.mu.Lock()
	e.started, e.lastUse = now, now
	orphaned := p.closed || e.dead || p.entries[e.key] != e
	p.mu.Unlock()

	if orphaned {
		// The pool was closed, or this entry evicted, while the
		// server was starting. Nobody will ever be handed it, so shut
		// it down here rather than leave a language server running
		// with no owner. shutdownEntry waits for the same handshake
		// we just finished, so whichever of the two paths gets there
		// first does the work and the other is a no-op.
		p.opts.Logf("%s for %s finished starting after the pool let go of it", e.def.Name, e.root)
		go p.shutdownEntry(context.Background(), e)
		return
	}
	go p.watch(e)
}

// watch notices a server that died on its own — crashed, was killed,
// closed its stdout — and evicts it, so the next request starts a
// fresh one instead of talking into a dead pipe. It does not touch the
// other sessions: one server crashing is not a reason to stop the
// daemon.
func (p *Pool) watch(e *poolEntry) {
	<-e.inst.Conn.Done()
	p.mu.Lock()
	alreadyGone := e.retired
	p.mu.Unlock()
	if alreadyGone {
		return // we shut it down ourselves
	}
	p.evict(e, "server exited")
}

// lease marks an entry as in use, unless it is dead or its connection
// has gone away.
func (p *Pool) lease(e *poolEntry) (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || e.dead || e.retired || e.sess == nil {
		return nil, false
	}
	select {
	case <-e.inst.Conn.Done():
		return nil, false
	default:
	}
	warm := e.requests > 0
	e.requests++
	e.inflight++
	p.inflight++
	e.lastUse = time.Now()
	return &Lease{pool: p, entry: e, warm: warm}, true
}

// forget drops a failed entry from the map so the next request may try
// again. The entry's error stays with the callers already waiting on
// it.
func (p *Pool) forget(e *poolEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e.dead = true
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
}

// evict removes a session from the pool and shuts it down in the
// background. Requests already leased on it keep their lease and fail
// on their own terms; the map no longer hands it out.
func (p *Pool) evict(e *poolEntry, why string) {
	p.mu.Lock()
	if e.dead {
		p.mu.Unlock()
		return
	}
	e.dead = true
	if p.entries[e.key] == e {
		delete(p.entries, e.key)
	}
	p.mu.Unlock()
	p.opts.Logf("dropping %s for %s: %s", e.def.Name, e.root, why)
	go p.shutdownEntry(context.Background(), e)
}

// ReapIdle shuts down every session that has been unused for at least
// idle, and returns how many it shut down. A session with a request in
// flight is never reaped, however long its last completed request was
// ago, and a session that is still starting is never reaped at all.
//
// The daemon's background reaper is this function on a timer. It is
// exported and takes the threshold as an argument because that makes
// the interesting behaviour testable without waiting for wall-clock
// time — the alternative being a test suite that sleeps for minutes.
func (p *Pool) ReapIdle(ctx context.Context, idle time.Duration) int {
	if idle < 0 {
		return 0
	}
	now := time.Now()

	var victims []*poolEntry
	p.mu.Lock()
	for key, e := range p.entries {
		if !e.startFinished() || e.dead || e.inflight > 0 {
			continue
		}
		if now.Sub(e.lastUse) < idle {
			continue
		}
		e.dead = true
		delete(p.entries, key)
		victims = append(victims, e)
	}
	p.mu.Unlock()

	for _, e := range victims {
		p.opts.Logf("reaping idle %s for %s", e.def.Name, e.root)
		p.shutdownEntry(ctx, e)
	}
	return len(victims)
}

func (e *poolEntry) startFinished() bool {
	select {
	case <-e.ready:
		return true
	default:
		return false
	}
}

// startReaper runs ReapIdle on a timer. The cadence is a quarter of
// the idle timeout, clamped, which makes a session live up to 25%
// longer than its nominal deadline — an acceptable price for not
// keeping one timer per session alive.
func (p *Pool) startReaper() {
	idle := p.opts.SessionIdleTimeout
	if idle <= 0 {
		return // reaping disabled
	}
	interval := idle / 4
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	p.reaperStop = make(chan struct{})
	p.reaperDone = make(chan struct{})
	go func() {
		defer close(p.reaperDone)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-p.reaperStop:
				return
			case <-ticker.C:
				p.ReapIdle(context.Background(), idle)
			}
		}
	}()
}

// Drain waits until no request holds a lease, so that a caller may
// stop accepting new work and let the work in flight finish. It
// reports ctx's error if the requests outlast it.
func (p *Pool) Drain(ctx context.Context) error {
	stop := make(chan struct{})
	defer close(stop)
	// sync.Cond has no context support; a waker turns ctx into a
	// broadcast.
	go func() {
		select {
		case <-ctx.Done():
			p.mu.Lock()
			p.cond.Broadcast()
			p.mu.Unlock()
		case <-stop:
		}
	}()

	p.mu.Lock()
	defer p.mu.Unlock()
	for p.inflight > 0 && ctx.Err() == nil {
		p.cond.Wait()
	}
	if p.inflight > 0 {
		return fmt.Errorf("daemon: %d request(s) still in flight: %w", p.inflight, ctx.Err())
	}
	return nil
}

// Close drains in-flight requests and shuts every session down
// politely: LSP `shutdown`, `exit`, then reap the process. Sessions are
// closed concurrently, because a pool of six servers should not take
// six shutdown timeouts to leave.
//
// Close is idempotent, and refuses new acquisitions from the moment it
// starts.
func (p *Pool) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()

	if p.reaperStop != nil {
		close(p.reaperStop)
		<-p.reaperDone
		p.reaperStop = nil
	}

	drainErr := p.Drain(ctx)

	p.mu.Lock()
	victims := make([]*poolEntry, 0, len(p.entries))
	for key, e := range p.entries {
		e.dead = true
		delete(p.entries, key)
		victims = append(victims, e)
	}
	p.mu.Unlock()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, e := range victims {
		wg.Add(1)
		go func(e *poolEntry) {
			defer wg.Done()
			if err := p.shutdownEntry(ctx, e); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(e)
	}
	wg.Wait()

	if drainErr != nil {
		errs = append(errs, drainErr)
	}
	return errors.Join(errs...)
}

// shutdownEntry ends one session: the polite LSP exit sequence, then
// reaping whatever the launcher started. Both halves are bounded — a
// server that will not leave is killed, because a daemon that cannot
// exit is worse than one that kills a stuck subprocess.
func (p *Pool) shutdownEntry(ctx context.Context, e *poolEntry) error {
	// A session that is still starting cannot be shut down yet: its
	// process may not exist, and by the time it does this function
	// would be gone. Waiting for the handshake to finish — or fail —
	// is what keeps a shutdown during a cold start from leaking the
	// server it was in the middle of spawning.
	select {
	case <-e.ready:
	case <-ctx.Done():
		// Out of budget. start's own orphan check is the backstop.
		return fmt.Errorf("daemon: %s for %s was still starting at shutdown: %w", e.def.Name, e.root, ctx.Err())
	}

	p.mu.Lock()
	if e.retired {
		p.mu.Unlock()
		return nil
	}
	e.retired = true
	p.mu.Unlock()

	if e.sess == nil {
		return nil
	}

	// A detached context: shutting a server down must not be skipped
	// because the client that asked for it went away.
	base := context.WithoutCancel(ctx)
	closeCtx, cancel := context.WithTimeout(base, p.opts.ShutdownTimeout)
	defer cancel()
	closeErr := e.sess.Close(closeCtx)

	if e.inst.Wait != nil {
		waitCtx, waitCancel := context.WithTimeout(base, p.opts.ShutdownTimeout)
		defer waitCancel()
		if err := e.inst.Wait(waitCtx); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	if closeErr != nil {
		return fmt.Errorf("daemon: shutting down %s for %s: %w", e.def.Name, e.root, closeErr)
	}
	return nil
}

// SessionStatus describes one live session for [Status].
type SessionStatus struct {
	// Server is the definition's name; ServerName is what the
	// process called itself in its InitializeResult, which is how
	// you notice you are talking to something unexpected.
	Server     string `json:"server"`
	ServerName string `json:"server_name,omitempty"`
	// Root is the workspace root the session was initialized with,
	// and RootMarker the marker that resolved it.
	Root       string `json:"root"`
	RootMarker string `json:"root_marker,omitempty"`
	// Started is when the handshake completed, LastUsed when the
	// last request finished.
	Started  time.Time `json:"started"`
	LastUsed time.Time `json:"last_used"`
	// Idle is how long the session has been unused.
	Idle time.Duration `json:"idle_ns"`
	// Requests is how many requests this session has served,
	// InFlight how many are running right now.
	Requests int64 `json:"requests"`
	InFlight int   `json:"in_flight"`
	// OpenDocuments is how many documents are open on the session.
	OpenDocuments int `json:"open_documents"`
	// Indexing reports that the server still has unfinished
	// $/progress work, and Progress lists those tokens (PLAN §5.2).
	Indexing bool     `json:"indexing"`
	Progress []string `json:"progress,omitempty"`
	// Starting reports a session whose handshake has not finished.
	Starting bool `json:"starting,omitempty"`
}

// Sessions describes the pool's live sessions, newest activity last.
func (p *Pool) Sessions() []SessionStatus {
	now := time.Now()
	p.mu.Lock()
	entries := make([]*poolEntry, 0, len(p.entries))
	for _, e := range p.entries {
		entries = append(entries, e)
	}
	out := make([]SessionStatus, 0, len(entries))
	for _, e := range entries {
		s := SessionStatus{
			Server:     e.def.Name,
			Root:       e.root,
			RootMarker: e.rootMarker,
			Started:    e.started,
			LastUsed:   e.lastUse,
			Requests:   e.requests,
			InFlight:   e.inflight,
			Starting:   !e.startFinished(),
		}
		if !e.lastUse.IsZero() {
			s.Idle = now.Sub(e.lastUse)
		}
		out = append(out, s)
	}
	p.mu.Unlock()

	// The session's own accessors are not guarded by p.mu and must
	// not be called under it: they take the session's locks.
	for i := range out {
		e := entries[i]
		if !e.startFinished() || e.sess == nil {
			continue
		}
		out[i].ServerName = e.sess.ServerName()
		out[i].OpenDocuments = len(e.docs.OpenURIs())
		// A server that never speaks the progress protocol is not
		// "indexing forever": Drained is false for it because there
		// was nothing to drain (PLAN §5.2's fallback case).
		if progress := e.sess.Progress(); progress != nil && progress.Seen() {
			out[i].Indexing = !progress.Drained()
			out[i].Progress = progress.Active()
		}
	}
	sortSessions(out)
	return out
}

// initOptions and settings exist so a nil map is sent as absent
// rather than as an empty object: some servers treat `{}` as "the user
// configured nothing on purpose" and others as a config error, and
// omitting the key is what an editor would do.
func initOptions(def *serverdef.ServerDef) any {
	if len(def.Server.InitializationOptions) == 0 {
		return nil
	}
	return def.Server.InitializationOptions
}

func settings(def *serverdef.ServerDef) any {
	if len(def.Server.Settings) == 0 {
		return nil
	}
	return def.Server.Settings
}

// sortSessions gives Status a stable order: by server name, then root.
// Map iteration order would make `daemon status` output shuffle
// between invocations for no reason.
func sortSessions(s []SessionStatus) {
	slices.SortFunc(s, func(a, b SessionStatus) int {
		if c := strings.Compare(a.Server, b.Server); c != 0 {
			return c
		}
		return strings.Compare(a.Root, b.Root)
	})
}
