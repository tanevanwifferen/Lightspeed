package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
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
	// MethodSession starts (or finds) the server for a target and
	// describes it: what it advertised, and whether it was warm. The
	// params are a [Target], the result a [SessionInfo].
	MethodSession = "lightspeed/session"
	// MethodOpen announces documents to a target's server. The
	// params are an [OpenRequest], the result an [OpenResult].
	MethodOpen = "lightspeed/open"
	// MethodReady waits for a target's server to finish its initial
	// work, under the readiness gate. The params are a
	// [ReadyRequest].
	MethodReady = "lightspeed/ready"
	// MethodDiagnostics reports what a target's server has published.
	// The params are a [DiagnosticsRequest], the result a
	// [DiagnosticsState].
	MethodDiagnostics = "lightspeed/diagnostics"
	// MethodClose closes documents on a target's server. The params
	// are a [CloseRequest].
	MethodClose = "lightspeed/close"
	// MethodChanged tells a target's server that files changed on disk
	// behind its back. The params are a [ChangedRequest].
	MethodChanged = "lightspeed/changed"
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

	// GateTimeout and Settle are this request's own readiness-gate
	// options (PLAN §4's --timeout and --settle); zero means the
	// pool's. They are per request because a session outlives the
	// command that started it: see client.Gate.With.
	GateTimeout time.Duration `json:"gate_timeout_ns,omitempty"`
	Settle      time.Duration `json:"settle_ns,omitempty"`

	// CollectEdits records the workspace/applyEdit requests the
	// server sends while this request runs, and returns them in
	// [Response.Pushed]. It is for workspace/executeCommand: a code
	// action that is a command has no edit until the server has run it
	// (docs/DECISIONS.md D9). Outside such a request the session
	// refuses applyEdit.
	CollectEdits bool `json:"collect_edits,omitempty"`
}

// A Target and a list of documents make an [OpenRequest].
type OpenRequest struct {
	Target
	Documents []DocumentSpec `json:"documents"`
}

// A DocumentSpec is one document to announce, *with its content*.
//
// The bytes travel because the client has already built its position
// Mapper from them (PLAN §5.1): if the daemon re-read the file, a
// change between the two reads would put every position one edit off
// from the text the server is looking at. The server is told exactly
// what the Mapper was built from.
type DocumentSpec struct {
	// Path is the absolute path of the file.
	Path string `json:"path"`
	// LanguageID is the language id sent in didOpen.
	LanguageID string `json:"language_id"`
	// Content is the file's bytes.
	Content []byte `json:"content"`
}

// OpenResult is the answer to an [OpenRequest].
type OpenResult struct {
	// Versions holds, per requested document and in order, the
	// version the server now has. It is the daemon's number, not the
	// caller's: a warm session's documents were versioned by earlier
	// commands, and a versioned edit can only be checked against the
	// version the server actually saw.
	Versions []int32 `json:"versions"`
}

// A CloseRequest closes documents on a target's server.
type CloseRequest struct {
	Target
	// Paths are the absolute paths of the documents to close.
	Paths []string `json:"paths"`
}

// A ChangedRequest tells a server that files changed on disk.
type ChangedRequest struct {
	Target
	Files []FileChange `json:"files"`
}

// A FileChange is one file that changed on disk. Type is the LSP
// FileChangeType: 1 created, 2 changed, 3 deleted.
type FileChange struct {
	Path string `json:"path"`
	Type int    `json:"type"`
}

// A ReadyRequest waits for a server to finish its initial work.
type ReadyRequest struct {
	Target
	// GateTimeout and Settle are the gate options for this wait.
	GateTimeout time.Duration `json:"gate_timeout_ns,omitempty"`
	Settle      time.Duration `json:"settle_ns,omitempty"`
}

// SessionInfo describes the server behind a target.
type SessionInfo struct {
	// Server is the definition that answered, ServerName what the
	// process calls itself.
	Server     string `json:"server"`
	ServerName string `json:"server_name,omitempty"`
	// Root is the workspace root the session was initialized with.
	Root string `json:"root"`
	// Initialize is the server's InitializeResult, verbatim. The
	// client parses its capabilities itself, so that a method the
	// server never advertised is refused before it crosses the
	// socket and with the same error as in process.
	Initialize json.RawMessage `json:"initialize"`
	// Warm reports that the server was already running: false means
	// this request paid for the spawn.
	Warm bool `json:"warm"`
	// Spawns is how many servers the pool has started since it came
	// up.
	Spawns int64 `json:"spawns"`
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

	// Pushed are the workspace edits the server sent while a
	// [Request.CollectEdits] request ran.
	Pushed []PushedEdit `json:"pushed,omitempty"`
}

// A Status describes a daemon and its pool. It is the answer to
// `lightspeed daemon status` (PLAN §4) and the generalization of
// gopls's `remote sessions`.
type Status struct {
	// PID is the daemon's process id, Executable the binary it runs.
	PID        int    `json:"pid"`
	Executable string `json:"executable,omitempty"`
	// Build is the identity of that executable and the protocol it speaks.
	// A client compares it with its own (Options.CheckBuild): a daemon left
	// running by another binary is not one to serve this command.
	Build Build `json:"build"`
	// Socket is the address it listens on, empty in --no-daemon mode.
	Socket string `json:"socket,omitempty"`
	// InProcess reports the --no-daemon path: there is no daemon,
	// the pool lives in the calling process.
	InProcess bool `json:"in_process"`
	// Workspace is the resolved workspace root this daemon is keyed
	// on (PLAN §3).
	Workspace string `json:"workspace,omitempty"`
	// ConfigID identifies the server definitions the daemon was
	// started with; see [PoolOptions.ConfigID].
	ConfigID string `json:"config_id,omitempty"`
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
	// Build is the daemon's build identity, as in [Status].
	Build Build `json:"build"`
	// Workspace is the root the daemon is keyed on.
	Workspace string `json:"workspace,omitempty"`
	// ConfigID identifies the server definitions the daemon was
	// started with; see [PoolOptions.ConfigID].
	ConfigID string `json:"config_id,omitempty"`
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

	// docMu guards the two maps below, and is held across the store
	// call that follows a change to them, so that "the last holder
	// closed it" and "a new holder opened it" cannot interleave.
	docMu sync.Mutex
	// holders counts, per pooled document, how many client
	// connections have it open. The document is closed on the server
	// only when the count returns to zero: two commands working on
	// one file share the document, and the first to finish must not
	// close it under the other.
	holders map[heldDoc]int
	// held is what each connection holds, so that a client which dies
	// without closing (kill, crash) does not keep a document open in
	// the warm server for good: [Service.dropConn] gives its holds
	// back when the connection goes.
	held map[uint64]map[heldDoc]int

	// idxState is the workspace's index and the reconciliation of the servers
	// with the disk (index.go).
	idxState
}

// A heldDoc names a document in one pooled session. The store pointer
// identifies the session, so a document in a server that has since
// been replaced is not confused with the same path in its successor.
type heldDoc struct {
	docs *docstore.Store
	path string
}

// A connKey is the context key under which the [Server] passes the id
// of the connection a request arrived on. Requests that come in
// without one — the in-process mode — are all connection 0, which
// never goes away, and balance through Open and CloseDocuments alone.
type connKey struct{}

func connID(ctx context.Context) uint64 {
	id, _ := ctx.Value(connKey{}).(uint64)
	return id
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

	// Before anything is asked, the servers are told what changed on disk
	// since they were last told (D33).
	s.reconcile(ctx)
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

	if req.CollectEdits {
		collect := lease.hooks().arm()
		defer func() { resp.Pushed = collect() }()
	}

	if req.Raw {
		// The connection, not Session.Call: a raw request is the
		// escape hatch for methods no capability covers, and for
		// asking a server something it did not advertise. Callers
		// that want the capability check make it themselves, against
		// the InitializeResult [Service.Session] returned.
		result, err := lease.Session().Conn().Call(ctx, req.Method, req.Params)
		if err != nil {
			return nil, s.decorate(err, lease)
		}
		resp.Result = result
		return resp, nil
	}

	gate := client.GateOptions{Timeout: req.GateTimeout, Settle: req.Settle}
	q, err := lease.Session().QueryWith(ctx, req.Method, req.Params, gate)
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
		Build:              SelfBuild(),
		Socket:             s.socket,
		InProcess:          s.socket == "",
		Workspace:          s.workspace,
		ConfigID:           s.pool.opts.ConfigID,
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
		Build:      SelfBuild(),
		Workspace:  s.workspace,
		ConfigID:   s.pool.opts.ConfigID,
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
	case MethodSession:
		var t Target
		if err := decode(params, &t); err != nil {
			return nil, err
		}
		return s.Session(ctx, t)
	case MethodOpen:
		var req OpenRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return s.Open(ctx, req)
	case MethodReady:
		var req ReadyRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return map[string]any{}, s.Ready(ctx, req)
	case MethodChanged:
		var req ChangedRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return map[string]any{}, s.FilesChanged(ctx, req)
	case MethodClose:
		var req CloseRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return map[string]any{}, s.CloseDocuments(ctx, req)
	case MethodDiagnostics:
		var req DiagnosticsRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return s.Diagnostics(ctx, req)
	case MethodIndex:
		var req IndexRequest
		if err := decode(params, &req); err != nil {
			return nil, err
		}
		return s.Index(ctx, req)
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

// acquire is Pool.Acquire behind the checks every session-level
// request shares.
func (s *Service) acquire(ctx context.Context, t Target) (*Lease, error) {
	s.reconcile(ctx)
	lease, err := s.pool.Acquire(ctx, t)
	if err != nil {
		return nil, err
	}
	s.requests.Add(1)
	return lease, nil
}

// Session starts — or finds — the server for a target and describes
// it. A command calls this first: what the server advertised decides
// which methods it may send at all.
func (s *Service) Session(ctx context.Context, t Target) (*SessionInfo, error) {
	lease, err := s.acquire(ctx, t)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return &SessionInfo{
		Server:     lease.Server().Name,
		ServerName: lease.Session().ServerName(),
		Root:       lease.Root(),
		Initialize: lease.Session().Capabilities().Raw(),
		Warm:       lease.Warm(),
		Spawns:     s.pool.Spawns(),
	}, nil
}

// Open announces documents to the target's server, with the content
// the caller built its positions from.
//
// A document that is already open with identical content costs
// nothing, and one whose content changed is pushed as a didChange
// (internal/docstore). Either way the file's recorded diagnostics are
// dropped first when the content is new to the server — before the
// notification is sent, because the server may publish the moment it
// receives it, and dropping afterwards could discard the very answer
// being waited for.
func (s *Service) Open(ctx context.Context, req OpenRequest) (*OpenResult, error) {
	lease, err := s.acquire(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	defer lease.Release()

	out := &OpenResult{Versions: make([]int32, 0, len(req.Documents))}
	for _, spec := range req.Documents {
		if !filepath.IsAbs(spec.Path) {
			return nil, &Error{
				Code:    CodeUsage,
				Message: fmt.Sprintf("daemon: document path %q is not absolute; the daemon's working directory is not the caller's", spec.Path),
				Exit:    exitUsage,
			}
		}
		doc, err := s.openHeld(ctx, lease, spec)
		if err != nil {
			return nil, err
		}
		out.Versions = append(out.Versions, doc.Version)
	}
	return out, nil
}

// openHeld opens one document and records the calling connection as a
// holder of it.
func (s *Service) openHeld(ctx context.Context, lease *Lease, spec DocumentSpec) (*docstore.Document, error) {
	key := heldDoc{docs: lease.Docs(), path: filepath.Clean(spec.Path)}
	s.docMu.Lock()
	defer s.docMu.Unlock()

	prev, had := key.docs.Get(spec.Path)
	unchanged := had && prev.Open && bytes.Equal(prev.Content, spec.Content)
	if !unchanged {
		lease.hooks().forget(protocol.URIFromPath(spec.Path))
	}
	doc, err := key.docs.OpenContent(spec.Path, spec.LanguageID, spec.Content)
	if err != nil {
		return nil, s.decorate(err, lease)
	}
	if s.holders == nil {
		s.holders = map[heldDoc]int{}
		s.held = map[uint64]map[heldDoc]int{}
	}
	id := connID(ctx)
	if s.held[id] == nil {
		s.held[id] = map[heldDoc]int{}
	}
	s.held[id][key]++
	s.holders[key]++
	return doc, nil
}

// dropConn gives back everything a connection held, closing the
// documents nobody else holds. The [Server] calls it when a connection
// ends, whether the client said goodbye or was killed.
func (s *Service) dropConn(id uint64) {
	s.docMu.Lock()
	defer s.docMu.Unlock()
	for key, n := range s.held[id] {
		if s.holders[key] -= n; s.holders[key] <= 0 {
			delete(s.holders, key)
			_ = key.docs.Close(key.path) // the session may be gone already
		}
	}
	delete(s.held, id)
}

// CloseDocuments closes documents on the target's server, so that a
// warm session is left with the document set it started with.
//
// A command opens what it needs with the bytes it read, and the server
// then answers about *those* bytes, not about the disk, for as long as
// the document stays open. Left open, the next command — or an editor,
// or `git checkout` — would change the file under a server that keeps
// answering from the copy it was given: the stale-authority failure of
// PLAN §5.2 arriving by a different road. Each command therefore closes
// what it opened, and the server goes back to the disk. The first
// failure is returned once every path has been tried.
//
// A document is closed when its last holder lets go, not the first: a
// second command that opened the same file meanwhile keeps its copy
// until it, too, closes or its connection drops (see [Service.dropConn]).
func (s *Service) CloseDocuments(ctx context.Context, req CloseRequest) error {
	lease, err := s.acquire(ctx, req.Target)
	if err != nil {
		return err
	}
	defer lease.Release()
	var first error
	s.docMu.Lock()
	defer s.docMu.Unlock()
	id := connID(ctx)
	for _, path := range req.Paths {
		key := heldDoc{docs: lease.Docs(), path: filepath.Clean(path)}
		if s.held[id][key] > 0 {
			if s.held[id][key]--; s.held[id][key] == 0 {
				delete(s.held[id], key)
			}
			s.holders[key]--
		}
		if s.holders[key] > 0 {
			continue // another command still has it open
		}
		delete(s.holders, key)
		if err := key.docs.Close(path); err != nil && first == nil {
			first = s.decorate(err, lease)
		}
	}
	return first
}

// FilesChanged tells the target's server that files changed on disk
// with workspace/didChangeWatchedFiles.
//
// In an editor the *editor* watches the tree and sends these. Nothing
// watches it here, and a server that outlives a command would otherwise
// keep answering from what it last read of a file that `rename --apply`
// has since rewritten — a stale answer that looks authoritative, which
// is the failure PLAN §5.2 exists to prevent, arriving by writing
// instead of by indexing. So a command that writes says so. It is a
// notification: a server that ignores it costs nothing.
func (s *Service) FilesChanged(ctx context.Context, req ChangedRequest) error {
	// The files this request names are told to the servers by this request,
	// with the type the caller knows; the reconciliation (D33) tells them
	// about everything else that changed.
	told := make(map[string]bool, len(req.Files))
	for _, f := range req.Files {
		if rel, err := filepath.Rel(s.workspace, f.Path); err == nil {
			told[filepath.ToSlash(rel)] = true
		}
	}
	s.reconcileExcept(ctx, told)
	lease, err := s.pool.Acquire(ctx, req.Target)
	if err != nil {
		return err
	}
	s.requests.Add(1)
	defer lease.Release()
	changes := make([]map[string]any, 0, len(req.Files))
	for _, f := range req.Files {
		changes = append(changes, map[string]any{"uri": string(protocol.URIFromPath(f.Path)), "type": f.Type})
	}
	if err := lease.Session().Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": changes}); err != nil {
		return s.decorate(err, lease)
	}
	return nil
}

// Ready waits for the target's server to finish its initial work, or
// reports a not-ready error (exit 5) if it does not in time.
func (s *Service) Ready(ctx context.Context, req ReadyRequest) error {
	lease, err := s.acquire(ctx, req.Target)
	if err != nil {
		return err
	}
	defer lease.Release()
	gate := client.GateOptions{Timeout: req.GateTimeout, Settle: req.Settle}
	if err := lease.Session().AwaitReadyWith(ctx, gate); err != nil {
		return s.decorate(err, lease)
	}
	return nil
}

// Diagnostics reports what the target's server has published about
// some files.
func (s *Service) Diagnostics(ctx context.Context, req DiagnosticsRequest) (*DiagnosticsState, error) {
	lease, err := s.acquire(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	return lease.hooks().state(req.URIs, req.Snapshot), nil
}
