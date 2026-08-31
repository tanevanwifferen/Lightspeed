package client

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// These are PLAN §5.2 end to end: a real JSON-RPC conversation with a
// scripted server over OS pipes, no network, no language server, no
// real sleeps. The scripts are the adversarial ones the plan names —
// empty-while-indexing, tokens that never end, rust-analyzer's custom
// tokens, no progress at all, and a flapping answer.
//
// OS pipes rather than io.Pipe: the client answers
// window/workDoneProgress/create from its read loop, and two
// synchronous in-memory pipes would deadlock the moment both sides
// write at once. A kernel pipe buffer is what a real stdio server
// gives us anyway.

type sessionFixture struct {
	t       *testing.T
	session *Session
	server  *fakeserver.Conn
	clock   *testClock

	once   sync.Once
	served chan error
}

// shutdown ends the session and waits for the scripted server to
// leave, so a test may assert on what the server saw. It is
// idempotent: the fixture's cleanup calls it too.
func (f *sessionFixture) shutdown() {
	f.once.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := f.session.Close(ctx); err != nil {
			f.t.Errorf("session close: %v", err)
		}
		select {
		case err := <-f.served:
			if err != nil {
				f.t.Errorf("fakeserver: %v", err)
			}
		case <-time.After(5 * time.Second):
			f.t.Error("fakeserver did not exit")
		}
	})
}

func startSession(t *testing.T, opts fakeserver.Options, gate GateOptions) *sessionFixture {
	t.Helper()

	clientR, serverW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	serverR, clientW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}

	var (
		mu      sync.Mutex
		srvConn *fakeserver.Conn
	)
	ready := make(chan struct{})
	opts.OnStart = func(c *fakeserver.Conn) {
		mu.Lock()
		srvConn = c
		mu.Unlock()
		close(ready)
	}

	served := make(chan error, 1)
	go func() { served <- fakeserver.Run(serverR, serverW, opts) }()
	<-ready

	clock := newTestClock()
	gate.Clock = clock
	if gate.Timeout == 0 {
		gate.Timeout = 5 * time.Second
	}

	conn := NewConn(clientR, clientW)
	session, err := Connect(context.Background(), conn, SessionOptions{
		RootDir: t.TempDir(),
		Gate:    gate,
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}

	mu.Lock()
	f := &sessionFixture{t: t, session: session, server: srvConn, clock: clock, served: served}
	mu.Unlock()

	t.Cleanup(func() {
		f.shutdown()
		clientW.Close()
		serverW.Close()
		clientR.Close()
		serverR.Close()
	})
	return f
}

// referencesCaps is the minimal capability set for these scripts.
var referencesCaps = map[string]any{"referencesProvider": true}

// refParams is a plausible textDocument/references request.
var refParams = map[string]any{
	"textDocument": map[string]any{"uri": "file:///ws/a.go"},
	"position":     map[string]any{"line": 41, "character": 7},
	"context":      map[string]any{"includeDeclaration": false},
}

// TestSessionEmptyWhileIndexing is the failure this whole milestone
// exists to prevent: the server answers "no references" while its
// indexing progress is still running. The gate must report not-ready
// (exit 5) instead of handing back an authoritative-looking [].
func TestSessionEmptyWhileIndexing(t *testing.T) {
	const token = "gopls/index"
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		ServerName:   "fake-gopls",
		AfterInitialized: func(c *fakeserver.Conn) {
			_ = c.CreateProgress(token)
			_ = c.ProgressBegin(token, "Loading packages")
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				// Emitted before the response, so the client sees the
				// progress no later than the answer it belongs to.
				_ = c.ProgressReport(token, "still indexing", 50)
				return []any{}, nil
			},
		},
	}, GateOptions{Timeout: 3 * time.Second})

	realStart := time.Now()
	res, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	nre := assertNotReady(t, err, NotReadyIndexing)

	if res.Result != nil {
		t.Errorf("result = %s, want nil rather than an empty list of unknown authority", res.Result)
	}
	if len(nre.Active) != 1 || nre.Active[0] != token {
		t.Errorf("Active = %v, want [%s]", nre.Active, token)
	}
	if real := time.Since(realStart); real > 2*time.Second {
		t.Errorf("query took %v of real time; the injected clock is not in charge", real)
	}
}

// TestSessionEmptyAfterProgressDrains is the necessary complement:
// once the progress set drains and the empty answer holds still, "no
// references" is a real answer and must be returned. A gate that only
// ever says "not ready" would be just as useless as one that always
// trusts the server.
func TestSessionEmptyAfterProgressDrains(t *testing.T) {
	const token = "gopls/index"
	var calls int
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		AfterInitialized: func(c *fakeserver.Conn) {
			_ = c.CreateProgress(token)
			_ = c.ProgressBegin(token, "Loading packages")
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				calls++
				if calls == 2 {
					_ = c.ProgressEnd(token, "done")
				}
				return []any{}, nil
			},
		},
	}, GateOptions{})

	res, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.Empty() {
		t.Errorf("result = %s, want the empty list", res.Result)
	}
	if res.Ready != ReadyDrainedStable {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyDrainedStable)
	}
	if !f.session.Progress().Drained() {
		t.Error("progress not drained")
	}
}

// TestSessionUnendedToken: a server that creates a progress token and
// never begins or ends it (a real rust-analyzer bug class) leaves the
// workspace unverifiable. The client must still have *answered* the
// create request — refusing it is how you end up with no progress
// information at all.
func TestSessionUnendedToken(t *testing.T) {
	const token = "rustAnalyzer/Roots Scanned"
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		AfterInitialized: func(c *fakeserver.Conn) {
			_ = c.CreateProgress(token)
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				_ = c.CreateProgress(token) // re-announced, never begun
				return []any{}, nil
			},
		},
	}, GateOptions{Timeout: 3 * time.Second})

	_, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	nre := assertNotReady(t, err, NotReadyIndexing)
	if len(nre.Active) != 1 || nre.Active[0] != token {
		t.Errorf("Active = %v, want [%s]", nre.Active, token)
	}

	for _, resp := range f.server.ClientResponses() {
		if resp.Method != "window/workDoneProgress/create" {
			continue
		}
		if resp.Error != nil {
			t.Fatalf("client refused %s: %+v; a server that cannot create tokens reports no progress at all",
				resp.Method, resp.Error)
		}
		return
	}
	t.Fatal("client never answered window/workDoneProgress/create")
}

// TestSessionRustAnalyzerStyleProgress: custom string tokens,
// announced with window/workDoneProgress/create, several at a time,
// ending across separate requests.
func TestSessionRustAnalyzerStyleProgress(t *testing.T) {
	const (
		roots    = "rustAnalyzer/Roots Scanned"
		indexing = "rustAnalyzer/Indexing"
	)
	var calls int
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		ServerName:   "rust-analyzer",
		AfterInitialized: func(c *fakeserver.Conn) {
			for _, token := range []string{roots, indexing} {
				_ = c.CreateProgress(token)
				_ = c.ProgressBegin(token, token)
			}
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				calls++
				switch calls {
				case 1:
					_ = c.ProgressReport(indexing, "1/300", 1)
					return []any{}, nil
				case 2:
					_ = c.ProgressEnd(roots, "")
					return []any{}, nil
				default:
					_ = c.ProgressEnd(indexing, "")
					return []any{map[string]any{
						"uri":   "file:///ws/lib.rs",
						"range": map[string]any{"start": map[string]any{"line": 3, "character": 4}},
					}}, nil
				}
			},
		},
	}, GateOptions{})

	res, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyDrained {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyDrained)
	}
	if res.Empty() {
		t.Errorf("result = %s, want the reference that appeared once indexing finished", res.Result)
	}
	if f.session.ServerName() != "rust-analyzer" {
		t.Errorf("ServerName = %q", f.session.ServerName())
	}
	snap := f.session.Progress().Snapshot()
	if len(snap) != 2 {
		t.Fatalf("progress snapshot = %+v, want both custom tokens", snap)
	}
	for _, tok := range snap {
		if !tok.Created || !tok.Ended {
			t.Errorf("token %+v: want created and ended", tok)
		}
	}
}

// TestSessionNoProgressAtAll: many servers never send progress. The
// answer is returned on stability alone, and carries a warning
// saying so — the envelope's warnings array exists for exactly this
// (PLAN §4).
func TestSessionNoProgressAtAll(t *testing.T) {
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				return []any{map[string]any{"uri": "file:///ws/a.go"}}, nil
			},
		},
	}, GateOptions{})

	res, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyNoProgress {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyNoProgress)
	}
	if len(res.Warnings) == 0 {
		t.Error("an inferred readiness must be warned about")
	}
	if f.session.Progress().Seen() {
		t.Error("Seen() = true for a server that sent no progress")
	}
}

// TestSessionFlappingResult: the server's answer grows as it works,
// with no progress protocol to say when it is done. Only the settled
// answer is returned.
func TestSessionFlappingResult(t *testing.T) {
	answers := []string{`[]`, `[{"uri":"file:///ws/a.go"}]`,
		`[{"uri":"file:///ws/a.go"},{"uri":"file:///ws/b.go"}]`}
	var calls int
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				i := calls
				calls++
				if i >= len(answers) {
					i = len(answers) - 1
				}
				return json.RawMessage(answers[i]), nil
			},
		},
	}, GateOptions{})

	res, err := f.session.Query(context.Background(), "textDocument/references", refParams)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(res.Result) != answers[len(answers)-1] {
		t.Errorf("result = %s, want the settled %s", res.Result, answers[len(answers)-1])
	}
	if res.Attempts < len(answers) {
		t.Errorf("Attempts = %d, want more than the %d distinct answers", res.Attempts, len(answers))
	}
}

// TestSessionCapabilityGuard: the request for a method the server did
// not advertise never reaches the wire (PLAN §5.4).
func TestSessionCapabilityGuard(t *testing.T) {
	var definitionCalls int
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				return []any{map[string]any{"uri": "file:///ws/a.go"}}, nil
			},
			"textDocument/definition": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				definitionCalls++
				return []any{}, nil
			},
		},
	}, GateOptions{})

	_, err := f.session.Query(context.Background(), "textDocument/definition", refParams)
	var unsup *UnsupportedMethodError
	if !errors.As(err, &unsup) {
		t.Fatalf("err = %v, want *UnsupportedMethodError", err)
	}
	if ec, ok := err.(interface{ ExitCode() int }); !ok || ec.ExitCode() != 3 {
		t.Errorf("exit code for an unadvertised method = %v, want 3", err)
	}
	if definitionCalls != 0 {
		t.Errorf("the server was asked %d times for a method it never advertised", definitionCalls)
	}
	if _, err := f.session.Call(context.Background(), "textDocument/definition", refParams); !errors.Is(err, ErrUnsupportedMethod) {
		t.Errorf("Call bypassed the capability guard: %v", err)
	}

	// The advertised one still works.
	if _, err := f.session.Query(context.Background(), "textDocument/references", refParams); err != nil {
		t.Errorf("references: %v", err)
	}
}

// TestSessionLifecycle covers the handshake and the polite exit: the
// server sees initialized, then shutdown and exit, and the recorded
// capabilities are the ones it advertised.
func TestSessionLifecycle(t *testing.T) {
	f := startSession(t, fakeserver.Options{
		Capabilities: map[string]any{"referencesProvider": true, "hoverProvider": true},
		ServerName:   "fake-gopls",
	}, GateOptions{})

	if got := f.session.ServerName(); got != "fake-gopls" {
		t.Errorf("ServerName = %q", got)
	}
	if !f.session.Supports("textDocument/hover") || f.session.Supports("textDocument/rename") {
		t.Errorf("capabilities recorded wrong: %s", f.session.Capabilities().Raw())
	}

	// Ends the session and waits for the server to leave, so its
	// record of the conversation is complete.
	f.shutdown()

	var sawInitialized, sawExit bool
	for _, method := range f.server.Notifications() {
		switch method {
		case "initialized":
			sawInitialized = true
		case "exit":
			sawExit = true
		}
	}
	if !sawInitialized || !sawExit {
		t.Errorf("notifications = %v, want initialized and exit", f.server.Notifications())
	}
}

// TestSessionUnknownServerRequest: a server request we do not
// implement is declined with MethodNotFound rather than ignored,
// which is what leaves a server waiting forever.
func TestSessionUnknownServerRequest(t *testing.T) {
	f := startSession(t, fakeserver.Options{
		Capabilities: referencesCaps,
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(c *fakeserver.Conn, _ json.RawMessage) (any, error) {
				_ = c.Request("workspace/applyEdit", map[string]any{"edit": map[string]any{}})
				return []any{map[string]any{"uri": "file:///ws/a.go"}}, nil
			},
		},
	}, GateOptions{})

	if _, err := f.session.Query(context.Background(), "textDocument/references", refParams); err != nil {
		t.Fatalf("Query: %v", err)
	}
	// Give the read loop a moment to deliver the refusal; the
	// response is written after the query's own answer.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, resp := range f.server.ClientResponses() {
			if resp.Method == "workspace/applyEdit" {
				if resp.Error == nil || resp.Error.Code != fakeserver.CodeMethodNotFound {
					t.Fatalf("applyEdit answered with %+v, want MethodNotFound", resp)
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("client never answered workspace/applyEdit")
}
