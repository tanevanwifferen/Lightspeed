package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
)

// The methods of the daemon protocol. They are namespaced so that a
// stray LSP client which dials the socket by accident gets a clean
// MethodNotFound rather than something surprising.
const (
	// MethodQuery runs one LSP request against the right pooled
	// server. Params are a [Request], the result is a [Response].
	MethodQuery = "lightspeed/query"
	// MethodStatus describes the daemon and its sessions. The result
	// is a [Status].
	MethodStatus = "lightspeed/status"
	// MethodStop asks the daemon to shut down gracefully. It answers
	// before it goes.
	MethodStop = "lightspeed/stop"
	// MethodHandshake exchanges identity, as gopls's
	// `gopls/handshake` does: it is how a client notices it is
	// talking to a daemon from a different build of the binary.
	MethodHandshake = "lightspeed/handshake"
)

// A Request is one query: which file it is about, and the LSP method
// to run.
//
// Positions are the caller's business. Params travel verbatim, and the
// byte-column to UTF-16 conversion of PLAN §5.1 happens in the CLI,
// which can do it with internal/docstore and no server at all. The
// daemon boundary is therefore at the LSP level, which keeps this
// contract small and keeps position mapping in one place.
type Request struct {
	// Path is the file the request concerns. It selects the server
	// and the workspace root; it is required even for methods whose
	// params do not mention a document, such as workspace/symbol.
	Path string `json:"path"`

	// LanguageID overrides language detection.
	LanguageID string `json:"language_id,omitempty"`

	// Server, if set, insists on that server definition by name
	// instead of the highest-priority one that claims Path.
	Server string `json:"server,omitempty"`

	// Method is the LSP method to call.
	Method string `json:"method"`

	// Params is the LSP params, passed to the server verbatim.
	Params json.RawMessage `json:"params,omitempty"`

	// Open asks for Path to be opened on the server (didOpen) before
	// the request, refreshing it from disk if the daemon's copy is
	// stale. Most servers answer nothing useful about a document
	// they were never told about (PLAN §5.4).
	Open bool `json:"open,omitempty"`

	// Raw skips the readiness gate and issues a single request. It
	// is for the `raw` escape hatch and for methods whose answer
	// cannot be mistaken for authoritative emptiness. The zero value
	// gates, because PLAN §5.2's failure mode — an empty answer from
	// a server that is still indexing — is the one an agent must
	// never be handed by accident.
	Raw bool `json:"raw,omitempty"`

	// Timeout bounds the request inside the daemon. Zero leaves it
	// to the readiness gate's own timeout.
	Timeout time.Duration `json:"timeout_ns,omitempty"`
}

// A Response is one query's answer, with the evidence for believing it.
type Response struct {
	// Server is the definition that answered, ServerName what the
	// process calls itself.
	Server     string `json:"server"`
	ServerName string `json:"server_name,omitempty"`
	// Root is the workspace root of the session that answered.
	Root string `json:"root"`

	// Result is the LSP result, verbatim.
	Result json.RawMessage `json:"result,omitempty"`

	// Ready is which of PLAN §5.2's rules established authority, and
	// Warnings are the envelope warnings for an authority that was
	// inferred rather than observed. Both are empty for a Raw
	// request, which asked for no such evidence.
	Ready    string   `json:"ready,omitempty"`
	Warnings []string `json:"warnings,omitempty"`

	// Attempts is how many times the request was issued, Waited how
	// long the readiness gate held it.
	Attempts int           `json:"attempts,omitempty"`
	Waited   time.Duration `json:"waited_ns,omitempty"`

	// Warm reports that an already-running server answered — the
	// daemon earning its keep. False means this request paid for the
	// spawn.
	Warm bool `json:"warm"`

	// Spawns is how many language servers the pool has started since
	// it came up. Together with Warm it is the timing-independent
	// way to see that a warm cache was used: two queries, one spawn.
	Spawns int64 `json:"spawns"`
}

// A Status describes a daemon and its pool. It is the answer to
// `lightspeed daemon status` (PLAN §4) and the generalization of
// gopls's `remote sessions`.
type Status struct {
	// PID is the daemon's process id, Executable the binary it runs.
	PID        int    `json:"pid"`
	Executable string `json:"executable,omitempty"`
	// Socket is the address it listens on, empty in --no-daemon mode.
	Socket string `json:"socket,omitempty"`
	// InProcess reports the --no-daemon path: there is no daemon,
	// the pool lives in the calling process.
	InProcess bool `json:"in_process"`
	// Workspace is the resolved workspace root this daemon is keyed
	// on (PLAN §3).
	Workspace string `json:"workspace,omitempty"`
	// Started is when the daemon came up, Uptime how long ago that
	// was.
	Started time.Time     `json:"started"`
	Uptime  time.Duration `json:"uptime_ns"`
	// Clients is how many clients are connected right now.
	Clients int `json:"clients"`
	// Requests is how many queries the daemon has served, Spawns how
	// many language servers it has started.
	Requests int64 `json:"requests"`
	Spawns   int64 `json:"spawns"`
	// ListenTimeout is the idle-exit deadline for the daemon,
	// SessionIdleTimeout the idle-reap deadline for one server.
	ListenTimeout      time.Duration `json:"listen_timeout_ns"`
	SessionIdleTimeout time.Duration `json:"session_idle_timeout_ns"`
	// Sessions are the live language servers.
	Sessions []SessionStatus `json:"sessions"`
}

// A Handshake is the identity exchange of [MethodHandshake].
type Handshake struct {
	// Executable is the binary the daemon is running.
	Executable string `json:"executable"`
	// PID is the daemon's process id.
	PID int `json:"pid"`
	// Workspace is the root the daemon is keyed on.
	Workspace string `json:"workspace,omitempty"`
	// Started is when the daemon came up.
	Started time.Time `json:"started"`
}

// A Service answers requests against a [Pool]. It is the only place
// where a query is turned into LSP traffic, and both modes use it: the
// daemon serves it over a socket, and --no-daemon calls it directly.
// That is what makes --no-daemon a debugging tool rather than a second
// implementation of the same behaviour.
type Service struct {
	pool      *Pool
	workspace string
	started   time.Time
	requests  atomic.Int64

	// socket and clients are set by the [Server] that publishes this
	// service, and are empty in the in-process mode.
	socket  string
	clients func() int

	// stop is what MethodStop triggers; the Server installs it. In
	// the in-process mode it closes the pool.
	stop func()

	// listenTimeout is reported by Status; the Server sets it.
	listenTimeout time.Duration
}

// NewService returns a service over pool for the given workspace root.
func NewService(pool *Pool, workspace string) *Service {
	return &Service{pool: pool, workspace: workspace, started: time.Now()}
}

// Pool returns the service's pool.
func (s *Service) Pool() *Pool { return s.pool }

// Remote reports whether this handle talks to another process. A
// Service never does.
func (s *Service) Remote() bool { return false }

// Query runs one request: resolve the file to a server, start or reuse
// that server, optionally open the document, and issue the LSP method —
// under the readiness gate unless the caller asked for a raw request.
func (s *Service) Query(ctx context.Context, req Request) (*Response, error) {
	if req.Method == "" {
		return nil, &Error{
			Code:    CodeUsage,
			Message: "daemon: request has no LSP method",
			Exit:    exitUsage,
		}
	}
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	lease, err := s.pool.Acquire(ctx, Target{
		Path:       req.Path,
		LanguageID: req.LanguageID,
		Server:     req.Server,
	})
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	s.requests.Add(1)

	resp := &Response{
		Server:     lease.Server().Name,
		ServerName: lease.Session().ServerName(),
		Root:       lease.Root(),
		Warm:       lease.Warm(),
		Spawns:     s.pool.Spawns(),
	}

	if req.Open {
		if err := openDocument(lease, req.Path); err != nil {
			return nil, err
		}
	}

	if req.Raw {
		result, err := lease.Session().Call(ctx, req.Method, req.Params)
		if err != nil {
			return nil, s.decorate(err, lease)
		}
		resp.Result = result
		return resp, nil
	}

	q, err := lease.Session().Query(ctx, req.Method, req.Params)
	if err != nil {
		return nil, s.decorate(err, lease)
	}
	resp.Result = q.Result
	resp.Ready = q.Ready
	resp.Warnings = q.Warnings
	resp.Attempts = q.Attempts
	resp.Waited = q.Waited
	return resp, nil
}

// decorate attaches the server and root to an error, so that a
// polyglot workspace's failures say which server failed.
func (s *Service) decorate(err error, lease *Lease) error {
	e := asError(err)
	if e.Server == "" {
		e.Server = lease.Server().Name
	}
	if e.Root == "" {
		e.Root = lease.Root()
	}
	return e
}

// openDocument tells the server about the file, re-reading it from disk
// each time. internal/docstore does the rest: a document that is
// already open with the same content costs nothing, and one whose
// content has changed is pushed as a didChange with a rebuilt Mapper.
//
// Re-reading is not optional in a warm daemon. The file on disk may
// well have changed since the last query — edit, ask, edit, ask is an
// agent's whole working style — and a server answering from the version
// we opened ten minutes ago would hand back positions into a file that
// no longer exists.
func openDocument(lease *Lease, path string) error {
	if _, err := lease.Docs().Open(path); err != nil {
		return asError(err)
	}
	return nil
}

// Status describes the service, its pool and its sessions.
func (s *Service) Status(ctx context.Context) (*Status, error) {
	exe, _ := os.Executable()
	st := &Status{
		PID:                os.Getpid(),
		Executable:         exe,
		Socket:             s.socket,
		InProcess:          s.socket == "",
		Workspace:          s.workspace,
		Started:            s.started,
		Uptime:             time.Since(s.started),
		Requests:           s.requests.Load(),
		Spawns:             s.pool.Spawns(),
		ListenTimeout:      s.listenTimeout,
		SessionIdleTimeout: s.pool.opts.SessionIdleTimeout,
		Sessions:           s.pool.Sessions(),
	}
	if s.clients != nil {
		st.Clients = s.clients()
	}
	return st, nil
}

// Stop shuts the service down: the daemon's graceful shutdown when it
// is being served over a socket, or just the pool in the in-process
// mode.
func (s *Service) Stop(ctx context.Context) error {
	if s.stop != nil {
		s.stop()
		return nil
	}
	return s.pool.Close(ctx)
}

// Close releases the service's resources. For the in-process mode this
// is the shutdown of every pooled server, and skipping it leaks
// language server processes for as long as the caller lives.
func (s *Service) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.pool.opts.ShutdownTimeout)
	defer cancel()
	return s.pool.Close(ctx)
}

// handshake answers MethodHandshake.
func (s *Service) handshake() *Handshake {
	exe, _ := os.Executable()
	return &Handshake{
		Executable: exe,
		PID:        os.Getpid(),
		Workspace:  s.workspace,
		Started:    s.started,
	}
}

// dispatch runs one protocol method, decoding params and encoding the
// result. It is the single place the wire protocol is interpreted, so
// the socket server stays a transport.
func (s *Service) dispatch(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case MethodQuery:
		var req Request
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return s.Query(ctx, req)
	case MethodStatus:
		return s.Status(ctx)
	case MethodStop:
		return map[string]any{"stopping": true}, s.Stop(ctx)
	case MethodHandshake:
		return s.handshake(), nil
	}
	return nil, fmt.Errorf("%w: %s", client.ErrMethodNotFound, method)
}

func decode(params json.RawMessage, v any) error {
	if len(params) == 0 {
		return &Error{Code: CodeUsage, Message: "daemon: request has no params", Exit: exitUsage}
	}
	if err := json.Unmarshal(params, v); err != nil {
		return &Error{
			Code:    CodeUsage,
			Message: fmt.Sprintf("daemon: malformed request: %v", err),
			Exit:    exitUsage,
		}
	}
	return nil
}
