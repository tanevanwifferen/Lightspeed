package client

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// SessionOptions configures the LSP handshake and the readiness gate.
type SessionOptions struct {
	// RootDir is the workspace root, as a filesystem path.
	RootDir string
	// Capabilities overrides the advertised client capabilities.
	// Leave nil for DefaultClientCapabilities.
	Capabilities map[string]any
	// InitializationOptions is the server-specific
	// `initializationOptions` blob from the server definition.
	InitializationOptions any
	// Settings, if non-nil, is pushed with
	// workspace/didChangeConfiguration right after `initialized`.
	// Some servers only wake up once they have it (PLAN §5.4).
	Settings any
	// Gate configures the readiness gate (PLAN §5.2).
	Gate GateOptions
	// OnNotification, if set, sees every server notification after
	// the progress tracker has. It runs on the read loop and must
	// not block.
	OnNotification NotificationHandler
	// OnRequest, if set, handles server-to-client requests that the
	// session does not answer itself. Returning ErrMethodNotFound
	// declines one.
	OnRequest RequestHandler
}

// DefaultClientCapabilities are the capabilities lightspeed
// advertises. window.workDoneProgress is the load-bearing one: a
// server may only create progress tokens for a client that asked for
// them, and without progress the readiness gate of PLAN §5.2 has
// nothing to observe. We advertise only what we actually implement —
// claiming workspace.configuration, for instance, would earn us
// requests we cannot answer.
func DefaultClientCapabilities() map[string]any {
	return map[string]any{
		"general": map[string]any{
			// Positions are UTF-16 throughout; the vendored gopls
			// Mapper is what converts them (PLAN §5.1).
			"positionEncodings": []string{"utf-16"},
		},
		"window": map[string]any{
			"workDoneProgress": true,
		},
		"workspace": map[string]any{
			"workspaceFolders": true,
			"applyEdit":        false,
		},
		"textDocument": map[string]any{
			"synchronization": map[string]any{
				"dynamicRegistration": false,
				"willSave":            false,
				"didSave":             false,
			},
		},
	}
}

// Session is an initialized LSP connection: it knows what the server
// advertised (so it can refuse uncapabilitied methods, PLAN §5.4),
// what work the server has in flight, and whether an answer may be
// believed (PLAN §5.2).
type Session struct {
	conn     *Conn
	caps     *Capabilities
	progress *ProgressTracker
	gate     *Gate
	opts     SessionOptions
}

// Connect performs the initialize handshake on conn, records the
// server's capabilities and installs the progress handlers. The
// handlers are installed *before* `initialize` is sent, because a
// server may create progress tokens while it is still answering it.
func Connect(ctx context.Context, conn *Conn, opts SessionOptions) (*Session, error) {
	gateOpts := opts.Gate.withDefaults()
	s := &Session{
		conn:     conn,
		progress: NewProgressTracker(gateOpts.Clock),
		opts:     opts,
	}
	conn.SetNotificationHandler(s.handleNotification)
	conn.SetRequestHandler(s.handleRequest)

	raw, err := initialize(ctx, conn, opts)
	if err != nil {
		return nil, err
	}
	caps, err := ParseInitializeResult(raw)
	if err != nil {
		return nil, fmt.Errorf("initialize: malformed result: %w", err)
	}
	s.caps = caps

	if opts.Settings != nil {
		if err := conn.Notify("workspace/didChangeConfiguration",
			map[string]any{"settings": opts.Settings}); err != nil {
			return nil, fmt.Errorf("didChangeConfiguration: %w", err)
		}
	}

	// The gate's clock starts here: readiness is measured from the
	// end of the handshake, not from process spawn.
	s.gate = NewGate(s.progress, gateOpts)
	return s, nil
}

// initialize sends `initialize` + `initialized` and returns the raw
// InitializeResult.
func initialize(ctx context.Context, conn *Conn, opts SessionOptions) (json.RawMessage, error) {
	caps := opts.Capabilities
	if caps == nil {
		caps = DefaultClientCapabilities()
	}
	params := map[string]any{
		"processId":    os.Getpid(),
		"clientInfo":   map[string]any{"name": "lightspeed"},
		"capabilities": caps,
	}
	if opts.RootDir != "" {
		uri := protocol.URIFromPath(opts.RootDir)
		params["rootUri"] = string(uri)
		// rootPath is deprecated but several servers still read it.
		params["rootPath"] = opts.RootDir
		params["workspaceFolders"] = []map[string]any{{
			"uri":  string(uri),
			"name": opts.RootDir,
		}}
	} else {
		params["rootUri"] = nil
	}
	if opts.InitializationOptions != nil {
		params["initializationOptions"] = opts.InitializationOptions
	}

	result, err := conn.Call(ctx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := conn.Notify("initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("initialized: %w", err)
	}
	return result, nil
}

// shutdown sends the polite exit sequence: `shutdown` request then
// `exit` notification, the second only if the first was answered.
func shutdown(ctx context.Context, conn *Conn) error {
	if _, err := conn.Call(ctx, "shutdown", nil); err != nil {
		return err
	}
	return conn.Notify("exit", nil)
}

// Close ends the session with the LSP exit sequence. A caller that
// owns a subprocess should follow it with (*Server).Wait to reap the
// process — or use (*Server).Shutdown, which does both.
func (s *Session) Close(ctx context.Context) error { return shutdown(ctx, s.conn) }

// Conn returns the underlying connection, for the `raw` escape hatch
// and for methods the session does not model.
func (s *Session) Conn() *Conn { return s.conn }

// Capabilities returns what the server advertised.
func (s *Session) Capabilities() *Capabilities { return s.caps }

// ServerName reports the server's self-reported name, "" if unknown.
func (s *Session) ServerName() string { return s.caps.ServerName() }

// Progress returns the progress tracker.
func (s *Session) Progress() *ProgressTracker { return s.progress }

// Gate returns the readiness gate.
func (s *Session) Gate() *Gate { return s.gate }

// Supports reports whether the server advertised the method.
func (s *Session) Supports(method string) bool { return s.caps.Supports(method) }

// Check returns an *UnsupportedMethodError (exit code 3) if the
// method must not be called.
func (s *Session) Check(method string) error { return s.caps.Check(method) }

// Notify sends a notification, without a capability check:
// notifications are document synchronisation, which the docstore
// owns.
func (s *Session) Notify(method string, params any) error { return s.conn.Notify(method, params) }

// Call issues a single capability-checked request with no readiness
// gating. Use it for requests whose answer cannot be misread as
// authoritative emptiness; use Query for everything an agent will act
// on.
func (s *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := s.caps.Check(method); err != nil {
		return nil, err
	}
	return s.conn.Call(ctx, method, params)
}

// Query issues a request under the readiness gate of PLAN §5.2. It
// returns an *UnsupportedMethodError (exit code 3) if the server
// never advertised the method, and a *NotReadyError (exit code 5)
// rather than an answer whose authority it could not establish.
func (s *Session) Query(ctx context.Context, method string, params any) (QueryResult, error) {
	if err := s.caps.Check(method); err != nil {
		return QueryResult{}, err
	}
	// Marshalled once: the gate may issue this request many times,
	// and bad params should fail before any of them.
	raw, err := marshalParams(params)
	if err != nil {
		return QueryResult{}, err
	}
	return s.gate.Query(ctx, method, func(ctx context.Context) (json.RawMessage, error) {
		return s.conn.Call(ctx, method, raw)
	})
}

// AwaitReady blocks until the server's initial progress set has
// drained, or reports a *NotReadyError if it does not in time.
func (s *Session) AwaitReady(ctx context.Context) error { return s.gate.AwaitReady(ctx) }

// handleNotification feeds progress to the tracker and passes every
// notification on to the caller's hook.
func (s *Session) handleNotification(method string, params json.RawMessage) {
	s.progress.HandleNotification(method, params)
	if s.opts.OnNotification != nil {
		s.opts.OnNotification(method, params)
	}
}

// handleRequest answers the server-to-client requests a read-only
// client must answer. window/workDoneProgress/create is the one that
// matters: refusing it makes servers skip progress reporting
// altogether, which would blind the readiness gate.
func (s *Session) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch {
	case s.progress.HandleCreate(method, params):
		return nil, nil
	case method == "client/registerCapability", method == "client/unregisterCapability":
		// We advertise no dynamic registration, but servers send
		// these anyway; acknowledging is cheaper than the log noise
		// an error response produces.
		return nil, nil
	case method == "window/workDoneProgress/cancel":
		return nil, nil
	}
	if s.opts.OnRequest != nil {
		return s.opts.OnRequest(ctx, method, params)
	}
	return nil, fmt.Errorf("%w: %s", ErrMethodNotFound, method)
}
