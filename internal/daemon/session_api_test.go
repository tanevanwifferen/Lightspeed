package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// These tests cover what the CLI needs of a shared session beyond
// Query: describing the server, opening documents with the caller's
// bytes, recording what the server pushes, and per-request readiness
// options. The CLI's own tests exercise the same paths end to end; these
// pin the contract at the package boundary, where a change to either
// side would otherwise surface as a puzzling failure a package away.

// recorder is what the scripted server saw, in order.
type recorder struct {
	mu      sync.Mutex
	opened  []string // didOpen/didChange texts
	methods []string
}

func (r *recorder) note(method string, params json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.methods = append(r.methods, method)
	var p struct {
		TextDocument struct {
			Text string `json:"text"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	switch method {
	case "textDocument/didOpen":
		r.opened = append(r.opened, p.TextDocument.Text)
	case "textDocument/didChange":
		for _, c := range p.ContentChanges {
			r.opened = append(r.opened, c.Text)
		}
	}
}

func (r *recorder) count(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.methods {
		if m == method {
			n++
		}
	}
	return n
}

func (r *recorder) texts() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.opened...)
}

// pushyScenario publishes a diagnostic for every document it is told
// about, pushes one workspace edit for executeCommand, and records the
// notifications it receives.
func pushyScenario(rec *recorder) func(*serverdef.ServerDef, string) fakeserver.Options {
	return func(def *serverdef.ServerDef, root string) fakeserver.Options {
		opts := defaultScenario(def, root)
		opts.Capabilities["executeCommandProvider"] = map[string]any{"commands": []string{"fake.fix"}}
		opts.Methods["workspace/executeCommand"] = func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
			_ = c.Request("workspace/applyEdit", map[string]any{
				"label": "fix it",
				"edit":  map[string]any{"changes": map[string]any{}},
			})
			return nil, nil
		}
		opts.OnNotification = func(c *fakeserver.Conn, method string, params json.RawMessage) {
			rec.note(method, params)
			if method != "textDocument/didOpen" && method != "textDocument/didChange" {
				return
			}
			var p struct {
				TextDocument struct {
					URI string `json:"uri"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(params, &p) == nil {
				_ = c.Notify("textDocument/publishDiagnostics", map[string]any{
					"uri":         p.TextDocument.URI,
					"diagnostics": []any{map[string]any{"message": "boom"}},
				})
			}
		}
		return opts
	}
}

func newService(t *testing.T, fleet *fakeFleet) (*Service, string) {
	t.Helper()
	root := workspace(t)
	return NewService(newTestPool(t, fleet, PoolOptions{}), root), root
}

func TestSessionDescribesTheServer(t *testing.T) {
	svc, root := newService(t, &fakeFleet{})
	target := Target{Path: filepath.Join(root, "main.go")}

	first, err := svc.Session(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if first.Warm {
		t.Error("the first session reports warm; it paid for the spawn")
	}
	if first.Server != "fake-go" || first.Root != root || first.Spawns != 1 {
		t.Errorf("session = %+v, want fake-go rooted at %s after one spawn", first, root)
	}
	caps, err := client.ParseInitializeResult(first.Initialize)
	if err != nil {
		t.Fatalf("the InitializeResult does not parse: %v", err)
	}
	if !caps.Supports("textDocument/references") || caps.Supports("textDocument/rename") {
		t.Errorf("the client would read the wrong capabilities out of %s", first.Initialize)
	}

	second, err := svc.Session(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Warm || second.Spawns != 1 {
		t.Errorf("second session = %+v, want warm with still one spawn", second)
	}
}

func TestOpenSendsTheCallersBytesAndReportsTheServersVersion(t *testing.T) {
	rec := &recorder{}
	svc, root := newService(t, &fakeFleet{scenario: pushyScenario(rec)})
	target := Target{Path: filepath.Join(root, "main.go")}
	main := filepath.Join(root, "main.go")
	open := func(text string) int32 {
		t.Helper()
		res, err := svc.Open(context.Background(), OpenRequest{
			Target:    target,
			Documents: []DocumentSpec{{Path: main, LanguageID: "go", Content: []byte(text)}},
		})
		if err != nil {
			t.Fatal(err)
		}
		return res.Versions[0]
	}

	// The bytes are the caller's, not the file's: main.go on disk says
	// "package fixture", and the server must be told what the caller's
	// Mapper was built from.
	if v := open("caller's text"); v != 1 {
		t.Errorf("first version = %d, want 1", v)
	}
	if v := open("caller's text"); v != 1 {
		t.Errorf("reopening identical content moved the version to %d; it must cost nothing", v)
	}
	if v := open("edited"); v != 2 {
		t.Errorf("changed content version = %d, want 2: a warm session's numbering is the server's, not the caller's", v)
	}
	waitFor(t, "the server to have seen both texts", func() bool { return len(rec.texts()) == 2 })
	if got := rec.texts(); got[0] != "caller's text" || got[1] != "edited" {
		t.Errorf("the server was sent %q, want the caller's bytes", got)
	}

	if _, err := svc.Open(context.Background(), OpenRequest{
		Target:    target,
		Documents: []DocumentSpec{{Path: "relative.go", LanguageID: "go", Content: []byte("x")}},
	}); exitCodeOf(err) != exitUsage {
		t.Errorf("a relative document path: err = %v, want a usage error — the daemon's working directory is not the caller's", err)
	}
}

// TestDiagnosticsAreRememberedUntilTheContentChanges is the rule `check`
// stands on. A warm session remembers what it was told, so an unchanged
// file is answered at once; a changed one must be answered *again*,
// because what the server published was about text that no longer exists.
func TestDiagnosticsAreRememberedUntilTheContentChanges(t *testing.T) {
	rec := &recorder{}
	svc, root := newService(t, &fakeFleet{scenario: pushyScenario(rec)})
	main := filepath.Join(root, "main.go")
	uri := "file://" + main
	target := Target{Path: main}
	open := func(text string) {
		t.Helper()
		if _, err := svc.Open(context.Background(), OpenRequest{
			Target:    target,
			Documents: []DocumentSpec{{Path: main, LanguageID: "go", Content: []byte(text)}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	state := func() *DiagnosticsState {
		t.Helper()
		st, err := svc.Diagnostics(context.Background(), DiagnosticsRequest{Target: target, URIs: []string{uri}, Snapshot: true})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}

	// Never opened: unknown, which is not the same as clean.
	if _, err := svc.Session(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if st := state(); len(st.Missing) != 1 || st.Publishes != 0 {
		t.Fatalf("before any open: %+v, want the file missing and nothing published", st)
	}

	open("one")
	waitFor(t, "the first publish", func() bool { return len(state().Missing) == 0 })
	if st := state(); st.Publishes != 1 || string(st.ByURI[uri]) == "" {
		t.Errorf("after the first publish: %+v", st)
	}

	open("one") // unchanged: the server will not say it again, and must not have to
	if st := state(); len(st.Missing) != 0 {
		t.Errorf("an unchanged document lost its diagnostics: %+v", st)
	}

	open("two") // changed: the old answer is about text that is gone
	waitFor(t, "the republish", func() bool { return state().Publishes == 2 })
	if st := state(); len(st.Missing) != 0 {
		t.Errorf("after the republish: %+v", st)
	}
}

// TestApplyEditIsAcceptedOnlyWhileCollecting tests the hooks directly. The
// end-to-end path — a code action that is a command, through a daemon —
// is the CLI's codeaction tests run with LIGHTSPEED_TEST_VIA_DAEMON=1;
// it cannot be staged here, because this package's fake server talks over
// unbuffered in-process pipes and a server that sends a request from
// inside a handler deadlocks against a client that answers it on its
// read loop. Real servers have OS pipes, which buffer.
func TestApplyEditIsAcceptedOnlyWhileCollecting(t *testing.T) {
	h := newSessionHooks()
	ctx := context.Background()
	params := json.RawMessage(`{"label":"fix it","edit":{"changes":{}}}`)
	applied := func() bool {
		t.Helper()
		res, err := h.onRequest(ctx, "workspace/applyEdit", params)
		if err != nil {
			t.Fatal(err)
		}
		return res.(map[string]any)["applied"] == true
	}

	if applied() {
		t.Error("an applyEdit nobody asked for was accepted: the server could rewrite the tree on its own")
	}
	collect := h.arm()
	if !applied() {
		t.Error("an applyEdit was refused while a command was collecting")
	}
	got := collect()
	if len(got) != 1 || got[0].Label != "fix it" || string(got[0].Edit) != `{"changes":{}}` {
		t.Errorf("collected %+v, want the one pushed edit, verbatim", got)
	}
	if applied() {
		t.Error("an applyEdit was accepted after the command stopped collecting")
	}
	if again := h.arm()(); len(again) != 0 {
		t.Errorf("a later collection saw %+v: edits leaked from one command to the next", again)
	}
	if _, err := h.onRequest(ctx, "workspace/configuration", nil); !errors.Is(err, client.ErrMethodNotFound) {
		t.Errorf("an unrelated server request: err = %v, want method not found", err)
	}
}

func TestRequestGateOptionsOverrideThePools(t *testing.T) {
	fleet := &fakeFleet{scenario: indexingScenario}
	// The pool's own gate would wait two seconds.
	svc, root := newService(t, fleet)
	start := time.Now()
	_, err := svc.Query(context.Background(), Request{
		Path: filepath.Join(root, "main.go"), Method: "textDocument/references",
		Params: json.RawMessage(`{}`), GateTimeout: 150 * time.Millisecond,
	})
	if !errors.Is(err, client.ErrNotReady) || exitCodeOf(err) != exitNotReady {
		t.Fatalf("err = %v (exit %d), want not ready, exit 5", err, exitCodeOf(err))
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("the request waited %s: its own 150ms gate timeout was ignored in favour of the pool's", elapsed)
	}
}

func TestCloseDocumentsAndFilesChangedReachTheServer(t *testing.T) {
	rec := &recorder{}
	svc, root := newService(t, &fakeFleet{scenario: pushyScenario(rec)})
	main := filepath.Join(root, "main.go")
	target := Target{Path: main}
	if _, err := svc.Open(context.Background(), OpenRequest{
		Target:    target,
		Documents: []DocumentSpec{{Path: main, LanguageID: "go", Content: []byte("x")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseDocuments(context.Background(), CloseRequest{Target: target, Paths: []string{main}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.FilesChanged(context.Background(), ChangedRequest{Target: target, Files: []FileChange{{Path: main, Type: 2}}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "didClose and didChangeWatchedFiles", func() bool {
		return rec.count("textDocument/didClose") == 1 && rec.count("workspace/didChangeWatchedFiles") == 1
	})
}

// TestServerErrorsKeepTheirRPCCodeAcrossTheSocket: the CLI words a server's
// own JSON-RPC error with its code and message, and cannot if they were
// flattened into a string on the way.
func TestServerErrorsKeepTheirRPCCodeAcrossTheSocket(t *testing.T) {
	sent := asError(&client.RPCError{Code: -32602, Message: "invalid params: no position"})
	if sent.RPC == nil || sent.RPC.Code != -32602 || sent.RPC.Message != "invalid params: no position" {
		t.Fatalf("asError dropped the server's error: %+v", sent)
	}
	got, ok := fromRPC(toRPC(sent)).(*Error)
	if !ok {
		t.Fatal("fromRPC did not return an *Error")
	}
	if got.RPC == nil || *got.RPC != *sent.RPC || got.Code != CodeServerError || got.Exit != exitProblems {
		t.Errorf("after the trip: %+v (rpc %+v), want the same server error, code %s, exit %d",
			got, got.RPC, CodeServerError, exitProblems)
	}
}

// TestSharedDocumentsCloseWithTheirLastHolder: two commands that opened
// the same file share one document. The first to finish must not close
// it under the other, and a client that dies without closing must not
// keep it open in the warm server for good.
func TestSharedDocumentsCloseWithTheirLastHolder(t *testing.T) {
	rec := &recorder{}
	svc, root := newService(t, &fakeFleet{scenario: pushyScenario(rec)})
	main := filepath.Join(root, "main.go")
	target := Target{Path: main}
	spec := DocumentSpec{Path: main, LanguageID: "go", Content: []byte("shared")}
	as := func(id uint64) context.Context { return context.WithValue(context.Background(), connKey{}, id) }
	open := func(id uint64) {
		t.Helper()
		if _, err := svc.Open(as(id), OpenRequest{Target: target, Documents: []DocumentSpec{spec}}); err != nil {
			t.Fatal(err)
		}
	}
	closeDoc := func(id uint64) {
		t.Helper()
		if err := svc.CloseDocuments(as(id), CloseRequest{Target: target, Paths: []string{main}}); err != nil {
			t.Fatal(err)
		}
	}
	docs := func() *docstore.Store {
		lease, err := svc.pool.Acquire(context.Background(), target)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		return lease.Docs()
	}

	open(1)
	open(2)
	closeDoc(1)
	if len(docs().OpenURIs()) != 1 || rec.count("textDocument/didClose") != 0 {
		t.Fatal("the first command to finish closed the document under the second")
	}
	closeDoc(2)
	waitFor(t, "the last holder's close to reach the server", func() bool { return rec.count("textDocument/didClose") == 1 })
	if n := len(docs().OpenURIs()); n != 0 {
		t.Errorf("%d documents still open after every holder closed", n)
	}

	// A killed client: it opened, and its connection went away.
	open(3)
	open(4)
	svc.dropConn(3)
	if len(docs().OpenURIs()) != 1 {
		t.Fatal("a dropped connection closed a document another connection still holds")
	}
	svc.dropConn(4)
	waitFor(t, "the dropped connections' documents to close", func() bool { return len(docs().OpenURIs()) == 0 })
}

// TestSnapshotIsLimitedToTheFilesAsked: a session has published about
// every file it was ever told of, and a snapshot for one file must not
// hand back the others.
func TestSnapshotIsLimitedToTheFilesAsked(t *testing.T) {
	h := newSessionHooks()
	for _, uri := range []string{"file:///w/a.go", "file:///w/b.go"} {
		h.onNotification(methodPublishDiagnostics, json.RawMessage(`{"uri":"`+uri+`","diagnostics":[{"message":"boom"}]}`))
	}
	st := h.state([]string{"file:///w/b.go"}, true)
	if len(st.ByURI) != 1 || st.ByURI["file:///w/b.go"] == nil {
		t.Errorf("snapshot for b.go = %v, want only b.go", st.ByURI)
	}
	if len(st.Missing) != 0 {
		t.Errorf("missing = %v, want none", st.Missing)
	}
}
