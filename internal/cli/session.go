package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// shutdownGrace is how long a server is given to leave politely once
// the command has its answer.
const shutdownGrace = 3 * time.Second

// resolveTarget answers "which server handles this path", the question
// PLAN §1 build-item 1 exists for, against the definitions of the
// workspace the path belongs to: .lightspeed.toml over the user's
// servers.d over the generated defaults (PLAN §6, docs/DECISIONS.md D17).
func (e *env) resolveTarget(path, languageID, serverName string) (router.Match, error) {
	cfg, err := e.workspaceConfig(path)
	if err != nil {
		return router.Match{}, err
	}
	matches, err := cfg.router.ResolveAs(path, languageID)
	if err != nil {
		var noServer *router.NoServerError
		if errors.As(err, &noServer) {
			return router.Match{}, render.Errorf(render.CodeNoServer, "%s", noServer.Error()).
				WithDetails(map[string]any{"path": noServer.Path, "language": noServer.LanguageID})
		}
		return router.Match{}, render.Errorf(render.CodeInternal, "resolving %s: %v", path, err)
	}
	if serverName != "" {
		for _, m := range matches {
			if m.Server.Name == serverName {
				return m, nil
			}
		}
		names := make([]string, len(matches))
		for i, m := range matches {
			names[i] = m.Server.Name
		}
		return router.Match{}, render.Errorf(render.CodeNoServer,
			"no server named %q handles %s (candidates: %s)", serverName, path, strings.Join(names, ", "))
	}
	return matches[0], nil
}

// A session is one command's view of a language server: which server
// it is talking to, what that server advertised, and the handle that
// carries its requests — to the shared daemon (PLAN §3), or, with
// --no-daemon, to a pool that lives and dies inside this process. It
// owns no subprocess itself, which is what lets a warm one outlive the
// command.
//
// Both handles run the same daemon.Service, so a command cannot tell
// which it has; that is what makes --no-daemon a debugging tool and not
// a second implementation.
type session struct {
	match router.Match
	h     daemon.Handle
	// target names the server to the pool. Its path is absolute:
	// the daemon's working directory is not this process's.
	target daemon.Target
	// gate is this command's own --timeout and --settle, sent with
	// every request, because the session's are whoever started it.
	gate client.GateOptions
	// caps is what the server advertised, parsed here so that a
	// method it never claimed is refused before it crosses the socket
	// and with the same error whichever handle is in use.
	caps *client.Capabilities
	// lsp is the capability-checked face commands use.
	lsp *lspView
	// docs maps positions. It has no server to notify: the server's
	// own copy of each document lives in the pool, and is told about
	// it by [session.openAs] with the very bytes this store built its
	// Mapper from.
	docs *docstore.Store
	// warm reports that the server was already running when this
	// command asked for it.
	warm bool
	// base is the invocation's context (env.base): every request the
	// session makes for its command is derived from it.
	base context.Context
	// offline is the kill switch as it stood when the session started,
	// for the install hint of a not-installed answer.
	offline bool
}

// sessionOptions are the per-command deviations from the default
// session. Only the readiness gate is left: the client capabilities and
// the handlers for the server's own traffic used to be per-command, and
// are the pool's now, because a pooled session is shared (see
// daemon.PoolOptions.Capabilities).
type sessionOptions struct {
	// gate configures the readiness gate of PLAN §5.2 for this
	// command's requests.
	gate client.GateOptions
}

// lspView is the capability-checked face of a session that commands
// use to ask what the server can do.
type lspView struct{ s *session }

// Supports reports whether the server advertised the method.
func (v *lspView) Supports(method string) bool { return v.s.caps.Supports(method) }

// Check returns an *client.UnsupportedMethodError if the method must
// not be called.
func (v *lspView) Check(method string) error { return v.s.caps.Check(method) }

// Capabilities returns what the server advertised.
func (v *lspView) Capabilities() *client.Capabilities { return v.s.caps }

// ServerName reports the server's self-reported name, "" if unknown.
func (v *lspView) ServerName() string { return v.s.caps.ServerName() }

// AwaitReady blocks until the server's initial progress set has
// drained, or reports a not-ready error if it does not in time.
func (v *lspView) AwaitReady(ctx context.Context) error {
	return v.s.h.Ready(ctx, daemon.ReadyRequest{
		Target:      v.s.target,
		GateTimeout: v.s.gate.Timeout,
		Settle:      v.s.gate.Settle,
	})
}

// startSession finds or starts the server for the matched path and
// describes it. The caller must call close.
func startSession(ctx context.Context, e *env, match router.Match, gate client.GateOptions) (*session, error) {
	return startSessionWith(ctx, e, match, sessionOptions{gate: gate})
}

// startSessionWith is startSession with the per-command deviations
// spelled out.
func startSessionWith(ctx context.Context, e *env, match router.Match, sopts sessionOptions) (*session, error) {
	if match.Path == "" {
		return nil, render.Errorf(render.CodeInternal, "%s: the resolved match carries no path to route by", match.Server.Name)
	}
	// Checked here as well as by whoever launches the server, so that
	// "not installed" always reads the same — with the exact command
	// that would fix it — however the server would have been started,
	// and before a daemon is asked to start anything.
	cfg, err := e.workspaceConfig(match.Path)
	if err != nil {
		return nil, err
	}
	if err := cfg.require(ctx, match.Server.Name); err != nil {
		return nil, err
	}

	h, err := openHandle(ctx, e, match.Path)
	if err != nil {
		return nil, err
	}
	s := &session{
		match:   match,
		base:    e.base(),
		offline: cfg.res.Offline,
		h:       h,
		target:  daemon.Target{Path: match.Path, LanguageID: match.LanguageID, Server: match.Server.Name},
		gate:    sopts.gate,
		docs:    docstore.New(nil, docstore.Options{}),
	}
	s.lsp = &lspView{s: s}

	info, err := h.Session(ctx, s.target)
	if err != nil {
		_ = h.Close()
		return nil, s.translate("initialize", err)
	}
	caps, err := client.ParseInitializeResult(info.Initialize)
	if err != nil {
		_ = h.Close()
		return nil, render.Errorf(render.CodeProtocolError, "%s: malformed initialize result: %v", match.Server.Name, err)
	}
	s.caps, s.warm = caps, info.Warm
	return s, nil
}

// close releases the handle: for a daemon that closes the connection
// and leaves the server warm, in process it shuts the pool down.
//
// Documents this command opened are closed first, so that a server which
// outlives the command answers from the disk again and not from the
// copy it was given (see daemon.Service.CloseDocuments). Best effort:
// the command already has its answer.
func (s *session) close() {
	if s == nil {
		return
	}
	// A cancelled command does not send the polite close. Over a socket it
	// would queue behind the request being abandoned — the daemon answers one
	// connection's requests in order — and the call would not return until that
	// request did, which is what cancelling was to avoid. Nothing is lost:
	// the daemon gives a connection's documents back when it ends (D15), and
	// an in-process pool is shut down by the Close below.
	if uris := s.docs.OpenURIs(); len(uris) > 0 && s.base.Err() == nil {
		paths := make([]string, len(uris))
		for i, uri := range uris {
			paths[i] = uri.Path()
		}
		ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		_ = s.h.CloseDocuments(ctx, daemon.CloseRequest{Target: s.target, Paths: paths})
		cancel()
	}
	_ = s.h.Close()
}

// filesChanged tells the server which files a command has just written.
// Best effort, like every notification: the write is done and reported
// either way.
func (s *session) filesChanged(cs render.ChangeSet) {
	if s == nil {
		return
	}
	var files []daemon.FileChange
	for _, c := range cs.Changes {
		switch c.Kind {
		case render.ChangeCreate:
			files = append(files, daemon.FileChange{Path: c.Path, Type: 1})
		case render.ChangeDelete:
			files = append(files, daemon.FileChange{Path: c.Path, Type: 3})
		case render.ChangeRename:
			files = append(files,
				daemon.FileChange{Path: c.Path, Type: 3},
				daemon.FileChange{Path: c.NewPath, Type: 1})
		default:
			files = append(files, daemon.FileChange{Path: c.Path, Type: 2})
		}
	}
	if len(files) == 0 {
		return
	}
	ctx, cancel := s.requestContext()
	defer cancel()
	_ = s.h.FilesChanged(ctx, daemon.ChangedRequest{Target: s.target, Files: files})
}

// requestContext bounds the requests that are not already bounded by a
// caller's own context: opening a document, for one.
func (s *session) requestContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(s.base, s.gate.Timeout+gateSlack)
}

// open reads path from disk and announces it to the server. Most
// servers answer nothing about a file they were never told about
// (PLAN §5.4), so every command that names a file goes through here.
// The language id comes from the router's decision rather than from
// the file extension, because the server definition is what claimed
// the file in the first place.
func (s *session) open(path string) (*docstore.Document, error) {
	return s.openAs(path, s.match.LanguageID)
}

// openAs is open with the language id spelled out, for a command that
// opens several kinds of file in one workspace: `check .` on a Go
// module opens both the .go files and the go.mod, and announcing the
// latter as "go" would be telling the server something untrue about a
// file it is about to parse.
//
// The bytes are read once and used twice: to build the Mapper every
// position of this command is converted with, and as the content the
// server is told the document has. Were the pool to read the file
// itself, a write between the two reads would leave every position one
// edit away from the text the server is looking at.
func (s *session) openAs(path, languageID string) (*docstore.Document, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, render.Errorf(render.CodeNoSuchFile, "%s: no such file", path)
		}
		return nil, render.Errorf(render.CodeIOError, "reading %s: %v", path, err)
	}
	doc, err := s.docs.OpenContent(path, languageID, content)
	if err != nil {
		return nil, render.Errorf(render.CodeServerCrash, "opening %s on %s: %v", path, s.match.Server.Name, err)
	}

	ctx, cancel := s.requestContext()
	defer cancel()
	res, err := s.h.Open(ctx, daemon.OpenRequest{
		Target:    s.target,
		Documents: []daemon.DocumentSpec{{Path: doc.Path, LanguageID: languageID, Content: content}},
	})
	if err != nil {
		var remote *daemon.Error
		if errors.As(err, &remote) && remote.Code != daemon.CodeInternal {
			return nil, s.translate("textDocument/didOpen", err)
		}
		return nil, render.Errorf(render.CodeServerCrash, "opening %s on %s: %v", path, s.match.Server.Name, err)
	}
	if len(res.Versions) != 1 {
		return nil, render.Errorf(render.CodeProtocolError, "opening %s: the daemon answered %d versions for one document", path, len(res.Versions))
	}
	// The version the *server* has, which is not this store's own
	// count once the session is warm; a versioned edit is checked
	// against it (see docSource).
	doc.Version = res.Versions[0]
	return doc, nil
}

// request is the daemon request every query and call starts from.
func (s *session) request(method string, params any) (daemon.Request, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return daemon.Request{}, render.Errorf(render.CodeInternal, "encoding %s params: %v", method, err)
	}
	return daemon.Request{
		Path:        s.target.Path,
		LanguageID:  s.target.LanguageID,
		Server:      s.target.Server,
		Method:      method,
		Params:      raw,
		GateTimeout: s.gate.Timeout,
		Settle:      s.gate.Settle,
	}, nil
}

// query issues a gated LSP request and translates the failures into
// the code/exit taxonomy. A not-ready workspace keeps its own code and
// exit status — the error crosses the socket as a daemon.Error carrying
// both — so that PLAN §5.2's guarantee survives the trip.
func (s *session) query(ctx context.Context, method string, params any) (client.QueryResult, error) {
	if err := s.caps.Check(method); err != nil {
		return client.QueryResult{}, s.translate(method, err)
	}
	req, err := s.request(method, params)
	if err != nil {
		return client.QueryResult{}, err
	}
	resp, err := s.h.Query(ctx, req)
	if err != nil {
		return client.QueryResult{}, s.translate(method, err)
	}
	return client.QueryResult{
		Result:   resp.Result,
		Ready:    resp.Ready,
		Warnings: resp.Warnings,
		Attempts: resp.Attempts,
		Waited:   resp.Waited,
	}, nil
}

// call issues an ungated, capability-checked request, with the same
// error translation as query.
//
// The readiness gate of PLAN §5.2 is deliberately absent. These are
// follow-ups to a request that was already gated — codeAction/resolve
// filling in an action we already have, workspace/executeCommand
// running one — and the gate reissues a request whose answer it cannot
// yet believe. Reissuing a command that changes the workspace is not a
// retry, it is a second execution.
func (s *session) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, _, err := s.callRaw(ctx, method, params, false)
	return raw, err
}

// callCollecting is call for workspace/executeCommand: it also returns
// the workspace edits the server pushed back while the command ran.
func (s *session) callCollecting(ctx context.Context, method string, params any) (json.RawMessage, []daemon.PushedEdit, error) {
	return s.callRaw(ctx, method, params, true)
}

// rawCall is call without the capability check, for the `raw` escape
// hatch: it may ask a server anything, including what it never
// advertised. The errors come back as the daemon reported them, for raw
// to word.
func (s *session) rawCall(ctx context.Context, method string, params any) (json.RawMessage, error) {
	req, err := s.request(method, params)
	if err != nil {
		return nil, err
	}
	req.Raw = true
	resp, err := s.h.Query(ctx, req)
	if err != nil {
		return nil, err
	}
	return resp.Result, nil
}

func (s *session) callRaw(ctx context.Context, method string, params any, collect bool) (json.RawMessage, []daemon.PushedEdit, error) {
	if err := s.caps.Check(method); err != nil {
		return nil, nil, s.translate(method, err)
	}
	req, err := s.request(method, params)
	if err != nil {
		return nil, nil, err
	}
	req.Raw, req.CollectEdits = true, collect
	resp, err := s.h.Query(ctx, req)
	if err != nil {
		return nil, nil, s.translate(method, err)
	}
	return resp.Result, resp.Pushed, nil
}

// translate maps a failed request onto the code/exit taxonomy.
//
// A daemon.Error is the same error whether it crossed the socket or
// was produced in process, so it is classified first and by its code.
// Only the classification survives a socket — the wrapped cause does
// not — which is why nothing here may depend on errors.Is reaching the
// original.
func (s *session) translate(method string, err error) error {
	var unsupported *client.UnsupportedMethodError
	var remote *daemon.Error
	switch {
	case errors.As(err, &unsupported):
		return unsupportedMethodError(unsupported, s.caps)
	case errors.As(err, &remote):
		return s.fromDaemon(method, remote)
	case errors.Is(err, context.DeadlineExceeded):
		return render.Errorf(render.CodeTimeout, "%s: %s did not answer in time", method, s.match.Server.Name)
	case errors.Is(err, context.Canceled):
		return render.Errorf(render.CodeCancelled, "%s: cancelled", method)
	}
	var rpcErr *client.RPCError
	if errors.As(err, &rpcErr) {
		return render.Errorf(render.CodeServerError, "%s: %s returned error %d: %s",
			method, s.match.Server.Name, rpcErr.Code, rpcErr.Message)
	}
	return render.Errorf(render.CodeServerCrash, "%s: %v", method, err)
}

// fromDaemon words a daemon-reported failure the way the CLI worded the
// same failure when it owned the server itself.
func (s *session) fromDaemon(method string, e *daemon.Error) error {
	switch e.Code {
	case daemon.CodeTimeout:
		return render.Errorf(render.CodeTimeout, "%s: %s did not answer in time", method, s.match.Server.Name)
	case daemon.CodeCancelled:
		return render.Errorf(render.CodeCancelled, "%s: cancelled", method)
	case daemon.CodeServerError:
		if e.RPC != nil {
			return render.Errorf(render.CodeServerError, "%s: %s returned error %d: %s",
				method, s.match.Server.Name, e.RPC.Code, e.RPC.Message)
		}
	case daemon.CodeServerNotInstalled:
		return notInstalledError(s.match.Server, s.match.Server.Server.Command[0], "", nil, s.offline)
	}
	return remoteFailure{e}
}

// remoteFailure is a daemon.Error as the renderer wants to see it: the
// machine code and the exit status the daemon decided, unchanged.
//
// Returning the daemon's own error would lose the code, because
// render's Coder interface names render.Code and daemon cannot import
// render. Everything that survives a socket is here, so this is the one
// place that has to keep up when the taxonomy grows.
type remoteFailure struct{ e *daemon.Error }

func (f remoteFailure) Error() string          { return f.e.Message }
func (f remoteFailure) Unwrap() error          { return f.e }
func (f remoteFailure) ErrorCode() render.Code { return render.Code(f.e.Code) }
func (f remoteFailure) ExitCode() int          { return f.e.ExitCode() }
func (f remoteFailure) ErrorDetails() any      { return f.e.ErrorDetails() }

// notInstalledError is exit code 3 with the exact command that would
// fix it — PLAN §6's security posture is that nothing installs
// implicitly, so the message has to carry the instruction.
//
// problem is empty when the executable was not found at all, and
// otherwise says why what was found cannot run. offline says the kill
// switch is on, so that the instruction is not offered as if `lightspeed
// install` would follow it.
func notInstalledError(def *serverdef.ServerDef, binary, problem string, fix []string, offline bool) error {
	var msg string
	if problem == "" {
		msg = fmt.Sprintf("%s handles this file but %q is not on PATH", def.Name, binary)
	} else {
		msg = fmt.Sprintf("%s handles this file but %q cannot be run (%s)", def.Name, binary, problem)
	}
	details := map[string]any{"server": def.Name, "command": binary}
	verb := "run"
	if len(fix) == 0 {
		verb = "install it with"
		if spec := def.Install.Mise; spec != "" {
			fix = []string{serverdef.MiseName, "use", "-g", spec}
		}
	}
	if len(fix) > 0 {
		// The probe's own repair when it has one — `mise use -g` of the
		// version mise already has, for a shim with none active — else the
		// definition's install spec.
		msg += fmt.Sprintf("; %s: %s", verb, strings.Join(fix, " "))
		details["install"] = strings.Join(fix, " ")
	} else {
		msg += fmt.Sprintf("; put %q on PATH, or set server.command in %s", binary, serverdef.WorkspaceFile)
	}
	if offline {
		msg += fmt.Sprintf(" (offline mode is on, so `lightspeed install` will refuse until --offline and %s are cleared)", serverdef.EnvOffline)
		details["offline"] = true
	}
	return render.Errorf(render.CodeServerNotInstalled, "%s", msg).WithDetails(details)
}

// anchorFile finds a file inside dir that some server claims, so that a
// workspace-wide command has something concrete to resolve a server
// and a root from. Directories have no language id of their own, and
// guessing one from the directory name would be worse than looking.
//
// The walk is deterministic (lexical order), skips the directories
// that are never source — VCS metadata, dependency caches, build
// output — and gives up after anchorScanLimit entries so that pointing
// this at a huge tree is slow at worst, never unbounded.
func anchorFile(r *router.Router, dir string) (string, bool) {
	scanned := 0
	found := ""
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable subtree is not a reason to fail the search
		}
		if scanned++; scanned > anchorScanLimit {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, err := r.Resolve(path); err != nil {
			return nil // no server for this file; try the next one
		}
		found = path
		return filepath.SkipAll
	})
	return found, found != ""
}

// anchorScanLimit bounds the directory walk of anchorFile.
const anchorScanLimit = 20000

// skipDir reports whether a directory can be skipped when looking for
// a file that identifies the workspace's language.
func skipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "__pycache__":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// resolveWorkspace resolves a server for a directory-scoped command
// such as workspace_symbol. An explicit --language names the language
// directly; otherwise a file in the tree is found to speak for it, and
// the ordinary path resolution runs on that file — so --server and
// root-marker resolution behave exactly as they do everywhere else.
func (e *env) resolveWorkspace(dir, languageID, serverName string) (router.Match, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeUsage, "resolving %s: %v", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeNoSuchFile, "%s: no such file or directory", dir)
	}
	if !info.IsDir() || languageID != "" {
		return e.resolveTarget(abs, languageID, serverName)
	}

	cfg, err := e.workspaceConfig(abs)
	if err != nil {
		return router.Match{}, err
	}
	anchor, ok := anchorFile(cfg.router, abs)
	if !ok {
		return router.Match{}, render.Errorf(render.CodeNoServer,
			"no server handles any file under %s; name its language with --language", abs).
			WithDetails(map[string]any{"path": abs})
	}
	return e.resolveTarget(anchor, "", serverName)
}
