package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// The daemon owns the workspace's index (docs/DECISIONS.md D32): the warm
// language servers are here, so this is where an outline can be had cheaply, and
// a `--no-daemon` command builds the same index in its own process through the
// same Service. The index itself (internal/index) knows nothing of sessions; this
// file is the Backend it is given.

// MethodIndex runs one index operation. Params are an [IndexRequest], the result
// is an [IndexResponse].
const MethodIndex = "lightspeed/index"

// The operations of [IndexRequest.Op].
const (
	IndexOpStatus    = "status"
	IndexOpBuild     = "build"
	IndexOpClear     = "clear"
	IndexOpSearch    = "search"
	IndexOpRepoMap   = "repo_map"
	IndexOpImports   = "imports"
	IndexOpImporters = "importers"
	IndexOpGraph     = "graph"
	IndexOpCycles    = "cycles"
	IndexOpCounts    = "counts"
	IndexOpSymbols   = "symbols"
)

// An IndexRequest is one operation on the workspace's index. Which fields are
// read depends on Op; the result's type is the matching one of internal/index.
type IndexRequest struct {
	Op string `json:"op"`

	// Search is the query of "search".
	Search *index.SearchQuery `json:"search,omitempty"`
	// Symbols is the query of "symbols": the unranked symbols of a set of files.
	Symbols *index.SymbolsQuery `json:"symbols,omitempty"`
	// RepoMap are the options of "repo_map".
	RepoMap *index.RepoMapOptions `json:"repo_map,omitempty"`
	// Path is the file of "imports"; Target the file, directory or import path
	// of "importers"; Root the start of a "graph" walk.
	Path   string `json:"path,omitempty"`
	Target string `json:"target,omitempty"`
	Root   string `json:"root,omitempty"`
	// Depth, Direction and External shape a "graph" walk.
	Depth     int    `json:"depth,omitempty"`
	Direction string `json:"direction,omitempty"`
	External  bool   `json:"external,omitempty"`

	// GateTimeout and Settle are the readiness options for the servers the
	// operation has to ask, as on a query.
	GateTimeout time.Duration `json:"gate_timeout_ns,omitempty"`
	Settle      time.Duration `json:"settle_ns,omitempty"`
}

// An IndexResponse carries the result of an [IndexRequest], JSON-encoded, as
// the type of internal/index the operation names.
type IndexResponse struct {
	Data json.RawMessage `json:"data"`
}

// IndexCacheDir is where a workspace's index is persisted:
// $XDG_CACHE_HOME/lightspeed/<workspace-hash>/ (~/.cache when unset). It is not
// created here.
func IndexCacheDir(root string) (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("daemon: no cache directory: %w", err)
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, RuntimeDirName, WorkspaceHash(root)), nil
}

// SetIndexCacheDir overrides where the index is persisted; empty keeps the
// index in memory only. It must be called before the first index operation.
func (s *Service) SetIndexCacheDir(dir string) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	s.idxDir, s.idxDirSet = dir, true
}

// manager is the workspace's index, made on first use.
func (s *Service) manager() (*index.Manager, error) {
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if s.idx != nil {
		return s.idx, nil
	}
	if s.workspace == "" {
		return nil, &Error{Code: CodeUsage, Message: "daemon: no workspace, so no index", Exit: exitUsage}
	}
	dir := s.idxDir
	if !s.idxDirSet {
		d, err := IndexCacheDir(s.workspace)
		if err == nil {
			dir = d
		}
	}
	s.idx = index.New(index.Options{
		Root:     s.workspace,
		CacheDir: dir,
		Build:    SelfBuild().ID(),
		Backend:  &indexBackend{s: s},
		Tracker:  s.tracker(),
		Logf:     s.pool.opts.Logf,
	})
	return s.idx, nil
}

// tracker is the change detection the index and the reconciliation of the
// servers share.
func (s *Service) tracker() *index.Tracker {
	s.trackerOnce.Do(func() { s.trk = index.NewTracker(s.workspace) })
	return s.trk
}

// Index runs one index operation.
func (s *Service) Index(ctx context.Context, req IndexRequest) (*IndexResponse, error) {
	m, err := s.manager()
	if err != nil {
		return nil, err
	}
	s.requests.Add(1)
	ctx = context.WithValue(ctx, gateKey{}, client.GateOptions{Timeout: req.GateTimeout, Settle: req.Settle})
	var out any
	switch req.Op {
	case IndexOpStatus:
		out, err = m.Status(ctx)
	case IndexOpBuild:
		out, err = m.Build(ctx)
	case IndexOpClear:
		out, err = m.Clear(ctx)
	case IndexOpSearch:
		if req.Search == nil {
			return nil, indexUsage("search needs a query")
		}
		out, err = m.Search(ctx, *req.Search)
	case IndexOpSymbols:
		if req.Symbols == nil {
			return nil, indexUsage("symbols needs a query")
		}
		out, err = m.Symbols(ctx, *req.Symbols)
	case IndexOpRepoMap:
		opts := index.RepoMapOptions{}
		if req.RepoMap != nil {
			opts = *req.RepoMap
		}
		out, err = m.RepoMap(ctx, opts)
	case IndexOpImports:
		out, err = m.Imports(ctx, req.Path)
	case IndexOpImporters:
		out, err = m.Importers(ctx, req.Target)
	case IndexOpGraph:
		dir, derr := index.ParseDirection(req.Direction)
		if derr != nil {
			return nil, indexUsage(derr.Error())
		}
		out, err = m.DependencyGraph(ctx, req.Root, req.Depth, dir, req.External)
	case IndexOpCycles:
		out, err = m.Cycles(ctx)
	case IndexOpCounts:
		out, err = m.Counts(ctx)
	default:
		return nil, indexUsage(fmt.Sprintf("unknown index operation %q", req.Op))
	}
	if err != nil {
		return nil, indexError(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, &Error{Code: CodeInternal, Message: "daemon: encoding the index answer: " + err.Error(), Exit: exitCrash}
	}
	return &IndexResponse{Data: raw}, nil
}

func indexUsage(msg string) *Error {
	return &Error{Code: CodeUsage, Message: "index: " + msg, Exit: exitUsage}
}

// indexError words an error from the index for the wire: the ones that carry
// their own code and exit status keep them, a missing graph node is a bad
// argument, and everything else is asError's.
func indexError(err error) error {
	if errors.Is(err, index.ErrNoSuchNode) {
		return &Error{Code: "no_such_file", Message: err.Error(), Exit: exitUsage, wrapped: err}
	}
	return asError(err)
}

// indexBackend is the [index.Backend] over the service's pool: the server that
// handles a file is the router's, and an outline is a documentSymbol request on
// the warm session of that server.
type indexBackend struct{ s *Service }

// Server implements [index.Backend].
func (b *indexBackend) Server(rel string) string {
	if def := b.s.pool.opts.Router.Claim(b.s.workspace, rel); def != nil {
		return def.Name
	}
	return ""
}

// ServerKey implements [index.Backend]; see [ExecutableKey].
func (b *indexBackend) ServerKey(server string) string {
	if def := b.def(server); def != nil {
		return ExecutableKey(def)
	}
	return ""
}

// ExecutableKey is a cheap identity of a server's executable — its resolved
// path, size and modification time — that changes when the server is upgraded
// and needs nothing to be started. "" when the command is not on PATH. It is
// the "server version" an index part is keyed by while the server is not
// running; the version the server reports once it is is compared as well.
func ExecutableKey(def *serverdef.ServerDef) string {
	if def == nil || len(def.Server.Command) == 0 {
		return ""
	}
	path, err := exec.LookPath(def.Server.Command[0])
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	fi, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return path + "|" + strconv.FormatInt(fi.Size(), 10) + "|" + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

func (b *indexBackend) def(server string) *serverdef.ServerDef {
	for _, d := range b.s.pool.opts.Router.Servers() {
		if d.Name == server {
			return d
		}
	}
	return nil
}

func (b *indexBackend) abs(rel string) string {
	return filepath.Join(b.s.workspace, filepath.FromSlash(rel))
}

// Ready implements [index.Backend]: the readiness gate of D6, applied once to
// the server that is about to be asked for a batch of outlines, and its version.
func (b *indexBackend) Ready(ctx context.Context, server, rel string) (string, error) {
	lease, err := b.s.pool.Acquire(ctx, Target{Path: b.abs(rel), Server: server})
	if err != nil {
		return "", unavailableOr(server, err)
	}
	defer lease.Release()
	gate, _ := ctx.Value(gateKey{}).(client.GateOptions)
	if err := lease.Session().AwaitReadyWith(ctx, gate); err != nil {
		return "", b.s.decorate(err, lease)
	}
	return lease.Session().Capabilities().ServerVersion(), nil
}

// Outline implements [index.Backend]. The file is announced with the bytes the
// index read (so the outline is of those bytes and not of whatever the server
// read last), asked about, and closed again: a warm server is left with the
// document set it had, and a document another command holds open is not closed
// under it (the holders count, D15).
func (b *indexBackend) Outline(ctx context.Context, server, rel string, content []byte) ([]symbols.Symbol, error) {
	abs := b.abs(rel)
	lease, err := b.s.pool.Acquire(ctx, Target{Path: abs, Server: server})
	if err != nil {
		return nil, unavailableOr(server, err)
	}
	defer lease.Release()
	if !lease.Session().Supports("textDocument/documentSymbol") {
		return nil, &index.FileError{Err: fmt.Errorf("%s does not advertise textDocument/documentSymbol", lease.Server().Name)}
	}

	lang := router.LanguageID(abs)
	doc, err := b.s.openHeld(ctx, lease, DocumentSpec{Path: abs, LanguageID: lang, Content: content})
	if err != nil {
		return nil, err
	}
	defer b.s.releaseHeld(ctx, lease, abs)

	params := map[string]any{"textDocument": map[string]any{"uri": string(doc.URI)}}
	var raw json.RawMessage
	for attempt := 0; ; attempt++ {
		raw, err = lease.Session().Call(ctx, "textDocument/documentSymbol", params)
		if err == nil || attempt >= 3 || !client.IsRetryable(err) {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		var rpc *client.RPCError
		if errors.As(err, &rpc) {
			// The server answered, and refused this file: that is about the file.
			return nil, &index.FileError{Err: fmt.Errorf("%s: documentSymbol: %s (code %d)", lease.Server().Name, rpc.Message, rpc.Code)}
		}
		return nil, b.s.decorate(err, lease)
	}
	syms, _, err := symbols.DecodeFile(raw, doc.URI, doc.Mapper)
	if err != nil {
		return nil, &index.FileError{Err: err}
	}
	return syms, nil
}

// unavailableOr classifies a failure to get a session: a server that is not
// installed or will not start is unavailable (the index goes on without it); any
// other failure is the caller's to see.
func unavailableOr(server string, err error) error {
	var de *Error
	if errors.As(err, &de) {
		switch de.Code {
		case CodeServerNotInstalled, CodeSpawnFailed, router.CodeNoServer:
			return &index.ServerUnavailableError{Server: server, Err: err}
		}
	}
	if errors.Is(err, exec.ErrNotFound) {
		return &index.ServerUnavailableError{Server: server, Err: err}
	}
	return err
}

// releaseHeld gives back a hold taken by openHeld, closing the document when no
// one else holds it.
func (s *Service) releaseHeld(ctx context.Context, lease *Lease, path string) {
	s.docMu.Lock()
	defer s.docMu.Unlock()
	key := heldDoc{docs: lease.Docs(), path: filepath.Clean(path)}
	id := connID(ctx)
	if s.held[id][key] > 0 {
		if s.held[id][key]--; s.held[id][key] == 0 {
			delete(s.held[id], key)
		}
		s.holders[key]--
	}
	if s.holders[key] > 0 {
		return
	}
	delete(s.holders, key)
	_ = key.docs.Close(path)
}

// --- keeping the servers' view of the disk current ---

// reconcile tells every running language server about the files that changed
// on disk since the last time it looked, before a query is answered (D33).
//
// Until now a server heard about a file only when a command opened it or wrote
// it: a file edited in an editor, or by `git checkout`, and reached by a query
// only through a reference, was answered about from the server's own idea of
// it, which for a warm server may be a stale cache. This closes that with the
// same change detection the index revalidates by ([index.Tracker]): a cheap
// scan, and `workspace/didChangeWatchedFiles` for each file that is new, changed
// or gone, to every live session, because the editor that would send them is
// not there.
func (s *Service) reconcile(ctx context.Context) { s.reconcileExcept(ctx, nil) }

// reconcileExcept is reconcile for a request that itself names some files as
// changed (a command that wrote them, D15): those are left out of what is sent,
// because the request is about to send them — with the type it knows, a rename's
// "created" or "deleted" included — and telling the server twice would only be
// noise. They are still absorbed into the baseline, so they are not reported
// again by the next request.
func (s *Service) reconcileExcept(ctx context.Context, skip map[string]bool) {
	if s.workspace == "" {
		return
	}
	s.recMu.Lock()
	defer s.recMu.Unlock()
	snap, err := s.tracker().Scan(ctx)
	if err != nil {
		return // the query itself will say what is wrong with the workspace
	}
	changes := index.Diff(s.recPrev, snap)
	s.recPrev = snap
	if len(changes) == 0 {
		return
	}
	events := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		if skip[c.Path] {
			continue
		}
		events = append(events, map[string]any{
			"uri":  string(protocol.URIFromPath(filepath.Join(s.workspace, filepath.FromSlash(c.Path)))),
			"type": int(c.Kind),
		})
	}
	if len(events) == 0 {
		return
	}
	s.pool.eachLive(func(l *Lease) {
		_ = l.Session().Notify("workspace/didChangeWatchedFiles", map[string]any{"changes": events})
	})
	s.reconciled.Add(int64(len(events)))
}

// idxState is the index and reconciliation state of a Service.
type idxState struct {
	idxMu     sync.Mutex
	idx       *index.Manager
	idxDir    string
	idxDirSet bool

	trackerOnce sync.Once
	trk         *index.Tracker

	recMu      sync.Mutex
	recPrev    *index.Snapshot
	reconciled atomic.Int64
}

// gateKey is the context key of the readiness options an index request carries
// down to the Backend.
type gateKey struct{}
