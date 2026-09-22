// Package daemon is the shared-server daemon of PLAN §3: a long-lived
// process that keeps language servers warm so that the second query
// against a workspace does not pay rust-analyzer's 30–90 second
// indexing cost again.
//
// The design is ported from gopls's `remote.go`/`serve.go` and the
// `lsprpc` package behind them, as PLAN §1 instructs. What is kept
// from that design:
//
//   - Auto-spawn. The first client that fails to dial the socket
//     re-executes lightspeed's own binary in daemon mode and retries
//     the dial with backoff ([Dial], [SpawnConfig]).
//   - A thin forwarding client. The CLI process does no LSP work of
//     its own; it sends one request over a unix socket and prints the
//     answer ([Client]).
//   - Idle exit on a listen timeout. A daemon with no connected
//     clients for [Options.ListenTimeout] exits, so nothing is left
//     running forever ([ErrIdleTimeout]). The accept loop is gopls's
//     jsonrpc2.Serve loop, with its connection-count timer.
//   - A status/stop surface ([Client.Status], [Client.Stop]),
//     corresponding to gopls's `remote sessions` subcommand.
//
// What is different, and is PLAN §1 "build ourselves" item 2: gopls's
// daemon serves exactly one language server, because gopls *is* the
// language server. Ours owns a [Pool] of N heterogeneous servers,
// keyed by (server definition, resolved workspace root), each with its
// own LSP lifecycle, its own readiness gate, its own open-document
// store, and its own idle deadline. Reaping one server does not
// disturb the others and does not stop the daemon.
//
// # Two modes, one API
//
// [Open] returns a [Handle], which is the whole API a command needs:
// [Handle.Query], [Handle.Status], [Handle.Stop]. With
// [Options.NoDaemon] set — PLAN §3's `--no-daemon`, for CI and
// debugging — the handle is a [Service] driving a [Pool] inside the
// calling process, and nothing is spawned, listened on or dialled.
// Otherwise it is a [Client] talking to the shared daemon. The two
// paths run the same [Service] code and return the same errors with
// the same exit codes, which is the property that makes `--no-daemon`
// a debugging tool rather than a second implementation.
//
// # Addressing
//
// The socket is $XDG_RUNTIME_DIR/lightspeed/<workspace-hash>.sock
// (PLAN §3), where the hash is over the *resolved workspace root*, so
// every subdirectory of a repository addresses one daemon and `cd`
// does not start a second one. [Workspace] resolves that root by
// walking up to the nearest VCS or workspace marker; it is a coarser
// question than internal/router's per-server root resolution, and the
// two deliberately differ: a repository with a nested Go module gets
// one daemon and two sessions, not two daemons.
//
// # Recovery
//
// A daemon that was killed leaves its socket file behind. Both sides
// handle it: a client that cannot dial spawns a replacement, and a
// starting daemon binds a temporary socket and renames it onto the
// published path, which is atomic — so a stale socket is replaced
// without a window in which the address does not exist, and two
// daemons racing to start end with one of them addressable rather
// than both half-bound. A socket owned by another user is refused
// outright, as in gopls's verifyRemoteOwnership.
//
// # A shared session
//
// A pooled session is shared by every command that asks about its
// workspace, so nothing about it can be per-command. The pool
// advertises one set of client capabilities (see
// [PoolOptions.Capabilities]), records what the server pushes — its
// workspace/applyEdit requests while a request asks to collect them,
// its published diagnostics always ([sessionHooks]) — and takes the
// readiness gate's timeout and settle window with each request. A
// command describes the session it got with [Handle.Session], announces
// its documents *with their content* with [Handle.Open] and closes them
// again with [Handle.CloseDocuments], so that a warm server goes back
// to reading the disk. docs/DECISIONS.md D15 has the reasoning.
//
// # What is not here
//
// Command wiring lives in internal/cli, which owns `lightspeed daemon
// serve|status|stop|logs` and the `--no-daemon` flag. Position mapping
// is also absent: requests carry LSP params verbatim, and byte-column
// to UTF-16 conversion stays with the caller, which can do it with
// internal/docstore without a server.
package daemon
