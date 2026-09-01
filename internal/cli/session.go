package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// shutdownGrace is how long a server is given to leave politely once
// the command has its answer.
const shutdownGrace = 3 * time.Second

// resolveTarget answers "which server handles this path", the question
// PLAN §1 build-item 1 exists for.
//
// The definition layer is the built-in table only: the .lightspeed.toml
// / servers.d / generated-defaults / PATH-sniffing layering of PLAN §6
// is M4, and inventing half of it here would be a second thing for M4
// to delete. serverCommandEnv still overrides the resolved command, so
// tests can point a real resolution at the fake server.
func resolveTarget(path, languageID, serverName string) (router.Match, error) {
	r, err := router.New(serverdef.Builtins()...)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeInternal, "built-in server definitions are invalid: %v", err)
	}
	matches, err := r.ResolveAs(path, languageID)
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

// A session is one command's worth of language server: the subprocess,
// the initialized LSP connection with its capabilities and readiness
// gate, and the document store that owns didOpen and the Mappers.
type session struct {
	match  router.Match
	server *client.Server
	lsp    *client.Session
	docs   *docstore.Store
}

// sessionOptions are the per-command deviations from the default
// handshake. A read-only command needs none of them; a mutation
// command advertises more client capabilities (it can honour
// documentChanges, resource operations and workspace/applyEdit, and
// the read-only surface deliberately does not) and installs a handler
// for the requests those capabilities invite.
type sessionOptions struct {
	// gate configures the readiness gate of PLAN §5.2.
	gate client.GateOptions
	// capabilities overrides the advertised client capabilities; nil
	// means client.DefaultClientCapabilities.
	capabilities map[string]any
	// onRequest handles server-to-client requests the client library
	// does not answer itself.
	onRequest client.RequestHandler
	// onNotification sees every server notification after the
	// progress tracker has. `check` needs it, because diagnostics are
	// pushed rather than answered; it runs on the read loop and must
	// not block.
	onNotification client.NotificationHandler
}

// startSession spawns the matched server, performs the handshake and
// returns a session. The caller must call close.
func startSession(ctx context.Context, e *env, match router.Match, gate client.GateOptions) (*session, error) {
	return startSessionWith(ctx, e, match, sessionOptions{gate: gate})
}

// startSessionWith is startSession with the per-command handshake
// deviations spelled out.
func startSessionWith(ctx context.Context, e *env, match router.Match, sopts sessionOptions) (*session, error) {
	argv := serverCommand(match)
	if _, err := exec.LookPath(argv[0]); err != nil {
		return nil, notInstalledError(match, argv[0])
	}

	server, err := client.StartCommand(argv, e.stderr)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, notInstalledError(match, argv[0])
		}
		return nil, render.Errorf(render.CodeSpawnFailed, "starting %s: %v", match.Server.Name, err)
	}

	opts := client.SessionOptions{
		RootDir:        match.Root,
		Gate:           sopts.gate,
		Capabilities:   sopts.capabilities,
		OnRequest:      sopts.onRequest,
		OnNotification: sopts.onNotification,
	}
	if len(match.Server.Server.InitializationOptions) > 0 {
		opts.InitializationOptions = match.Server.Server.InitializationOptions
	}
	if len(match.Server.Server.Settings) > 0 {
		opts.Settings = match.Server.Server.Settings
	}

	lsp, err := client.Connect(ctx, server.Conn, opts)
	if err != nil {
		reap(server)
		return nil, handshakeError(match, err)
	}

	s := &session{match: match, server: server, lsp: lsp}
	s.docs = docstore.New(lsp, docstore.Options{})
	return s, nil
}

// close releases the server. Documents are closed first so that a
// server which outlives this command — the M3 daemon's pool — is left
// with the document set it started with.
func (s *session) close() {
	if s == nil {
		return
	}
	_ = s.docs.CloseAll()
	reap(s.server)
}

// reap ends a server process, politely if it will cooperate.
func reap(server *client.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = server.Shutdown(ctx)
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
	return doc, nil
}

// query issues a gated LSP request and translates the failures into
// the code/exit taxonomy. A not-ready workspace keeps its own error —
// internal/client's *NotReadyError already reports exit code 5 — so
// that PLAN §5.2's guarantee survives the trip through the CLI.
func (s *session) query(ctx context.Context, method string, params any) (client.QueryResult, error) {
	res, err := s.lsp.Query(ctx, method, params)
	if err == nil {
		return res, nil
	}
	return res, s.translate(method, err)
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
	raw, err := s.lsp.Call(ctx, method, params)
	if err != nil {
		return nil, s.translate(method, err)
	}
	return raw, nil
}

// translate maps a failed request onto the code/exit taxonomy. A
// not-ready workspace keeps its own error — internal/client's
// *NotReadyError already reports exit code 5 — so that PLAN §5.2's
// guarantee survives the trip through the CLI.
func (s *session) translate(method string, err error) error {
	var unsupported *client.UnsupportedMethodError
	switch {
	case errors.As(err, &unsupported):
		return unsupportedMethodError(unsupported, s.lsp.Capabilities())
	case errors.Is(err, client.ErrNotReady):
		return err
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

// serverCommand is the definition's command, with the test/debug
// override applied. The override is M0 scaffolding: M4 replaces it
// with real configuration layering.
func serverCommand(match router.Match) []string {
	if override := os.Getenv(serverCommandEnv); override != "" {
		return strings.Fields(override)
	}
	return match.Server.Server.Command
}

// notInstalledError is exit code 3 with the exact command that would
// fix it — PLAN §6's security posture is that nothing installs
// implicitly, so the message has to carry the instruction.
func notInstalledError(match router.Match, binary string) error {
	msg := fmt.Sprintf("%s handles this file but %q is not on PATH", match.Server.Name, binary)
	details := map[string]any{"server": match.Server.Name, "command": binary}
	if spec := match.Server.Install.Mise; spec != "" {
		msg += fmt.Sprintf("; install it with: mise use -g %s", spec)
		details["install"] = "mise use -g " + spec
	}
	return render.Errorf(render.CodeServerNotInstalled, "%s", msg).WithDetails(details)
}

// handshakeError classifies a failed initialize. A server that dies
// during the handshake is a crash, not an empty answer.
func handshakeError(match router.Match, err error) error {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return render.Errorf(render.CodeTimeout, "%s did not complete the initialize handshake in time", match.Server.Name)
	case errors.Is(err, context.Canceled):
		return render.Errorf(render.CodeCancelled, "initialize: cancelled")
	default:
		return render.Errorf(render.CodeServerCrash, "%s: initialize failed: %v", match.Server.Name, err)
	}
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
func resolveWorkspace(dir, languageID, serverName string) (router.Match, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeUsage, "resolving %s: %v", dir, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeNoSuchFile, "%s: no such file or directory", dir)
	}
	if !info.IsDir() || languageID != "" {
		return resolveTarget(abs, languageID, serverName)
	}

	r, err := router.New(serverdef.Builtins()...)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeInternal, "built-in server definitions are invalid: %v", err)
	}
	anchor, ok := anchorFile(r, abs)
	if !ok {
		return router.Match{}, render.Errorf(render.CodeNoServer,
			"no server handles any file under %s; name its language with --language", abs).
			WithDetails(map[string]any{"path": abs})
	}
	return resolveTarget(anchor, "", serverName)
}
