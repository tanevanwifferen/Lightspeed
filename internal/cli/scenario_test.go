package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// The scripted fake server the M1 command tests run against.
//
// A command spawns a language server as a subprocess, so the server
// has to be a program on disk. The test binary re-execs as one (see
// TestMain in raw_test.go) and the script is passed in the
// environment, which is the only channel a subprocess has before the
// LSP handshake starts.
//
// Everything here is hermetic: no network, no real language server,
// no fixture outside the test's own temporary directory.
const (
	// scenarioEnv selects the script. Unset is the M0 fixed script.
	scenarioEnv = "LIGHTSPEED_TEST_SCENARIO"
	// capsEnv is the InitializeResult.capabilities object, as JSON.
	capsEnv = "LIGHTSPEED_TEST_CAPS"
	// resultsEnv maps LSP method → canned result, as a JSON object.
	resultsEnv = "LIGHTSPEED_TEST_RESULTS"
	// traceEnv names a file the server appends one JSON line to per
	// message it receives, so a test can assert on what actually went
	// over the wire rather than on what the answer looked like.
	traceEnv = "LIGHTSPEED_TEST_TRACE"
)

// scenarioScript is the value of scenarioEnv for the scripted server.
const scenarioScript = "script"

// scenarioIndexing is the value of scenarioEnv for a server that
// begins a $/progress token and never ends it, while answering every
// query with an empty array — PLAN §5.2's worst failure mode, and the
// one M1 has to turn into exit 5.
const scenarioIndexing = "indexing"

// positionEcho is a canned result that means "answer with a location
// at whatever position the request carried". It closes the loop on
// PLAN §5.1: the byte column the user typed becomes a UTF-16 position
// on the wire and has to come back as the same byte column, and a
// canned answer cannot prove that.
const positionEcho = `"__echo_position__"`

// echoPosition answers a textDocument/* request with a zero-width
// location at the requested position.
func echoPosition(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
	var req struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
		Position map[string]any `json:"position"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
	}
	return []any{map[string]any{
		"uri":   req.TextDocument.URI,
		"range": map[string]any{"start": req.Position, "end": req.Position},
	}}, nil
}

// runFakeServer is the child-process half of the harness: it reads
// the script from the environment and serves one session.
func runFakeServer() int {
	switch os.Getenv(scenarioEnv) {
	case scenarioScript, scenarioIndexing:
	default:
		if err := fakeserver.Serve(os.Stdin, os.Stdout); err != nil {
			return 1
		}
		return 0
	}

	opts := fakeserver.Options{ServerName: "fake-langserver"}
	if raw := os.Getenv(capsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &opts.Capabilities); err != nil {
			return 1
		}
	}
	var results map[string]json.RawMessage
	if raw := os.Getenv(resultsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &results); err != nil {
			return 1
		}
	}
	trace := newTracer(os.Getenv(traceEnv))
	opts.OnNotification = func(_ *fakeserver.Conn, method string, params json.RawMessage) {
		trace.record(method, params)
	}
	opts.Methods = map[string]fakeserver.Method{}
	for method, result := range results {
		if string(result) == positionEcho {
			opts.Methods[method] = func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
				trace.record(method, params)
				return echoPosition(c, params)
			}
			continue
		}
		if handler, ok := mutationMethod(method, result, trace.record); ok {
			opts.Methods[method] = handler
			continue
		}
		opts.Methods[method] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
			trace.record(method, params)
			return result, nil
		}
	}

	if os.Getenv(scenarioEnv) == scenarioIndexing {
		// A token that is created, begun, reported on — and never
		// ended. Meanwhile every query answers "nothing found",
		// authoritatively-looking and completely worthless.
		opts.OnInitialize = func(c *fakeserver.Conn) {
			_ = c.CreateProgress("indexing")
			_ = c.ProgressBegin("indexing", "indexing workspace")
			_ = c.ProgressReport("indexing", "1/2 crates", 50)
		}
	} else {
		// A server that finishes loading before it answers anything.
		// The events go out during `initialize`, so the client has
		// processed them before the handshake returns and the gate
		// never has to guess.
		opts.OnInitialize = func(c *fakeserver.Conn) {
			_ = c.ProgressBegin("load", "loading workspace")
			_ = c.ProgressEnd("load", "loaded")
		}
	}

	if err := fakeserver.Run(os.Stdin, os.Stdout, opts); err != nil {
		return 1
	}
	return 0
}

// traced is one message the scripted server received.
type traced struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// tracer appends received messages to a file as JSON lines. A file is
// the only channel back from the server subprocess to the test.
type tracer struct{ path string }

func newTracer(path string) *tracer { return &tracer{path: path} }

func (t *tracer) record(method string, params json.RawMessage) {
	if t.path == "" {
		return
	}
	line, err := json.Marshal(traced{Method: method, Params: params})
	if err != nil {
		return
	}
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// scenario configures one command test: which capabilities the server
// advertises and what it answers.
type scenario struct {
	// capabilities is the InitializeResult.capabilities object. Nil
	// means the full read-only set.
	capabilities map[string]any
	// results maps an LSP method to the raw JSON it answers with.
	results map[string]any
	// indexing makes the server report work that never finishes.
	indexing bool
	// trace, when set, is a file the server logs every received
	// message to. Set it with scenario.traceTo.
	trace string
}

// traceTo asks the scripted server to log every message it receives to
// a file in the test's temporary directory, and returns a function
// that reads the log back.
func (s *scenario) traceTo(t *testing.T) func() []traced {
	t.Helper()
	s.trace = filepath.Join(t.TempDir(), "trace.jsonl")
	return func() []traced {
		t.Helper()
		data, err := os.ReadFile(s.trace)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			t.Fatal(err)
		}
		var out []traced
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if line == "" {
				continue
			}
			var msg traced
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				t.Fatalf("bad trace line %q: %v", line, err)
			}
			out = append(out, msg)
		}
		return out
	}
}

// apply points the CLI at this test binary in scripted-server mode.
func (s scenario) apply(t *testing.T) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(serverCommandEnv, exe)
	t.Setenv(fakeServerModeEnv, "1")
	if s.indexing {
		t.Setenv(scenarioEnv, scenarioIndexing)
	} else {
		t.Setenv(scenarioEnv, scenarioScript)
	}

	caps := s.capabilities
	if caps == nil {
		caps = readOnlyCapabilities()
	}
	t.Setenv(capsEnv, mustJSON(t, caps))
	if s.results != nil {
		t.Setenv(resultsEnv, mustJSON(t, s.results))
	}
	t.Setenv(traceEnv, s.trace)
}

// readOnlyCapabilities advertises everything M1's command surface
// needs, and nothing it does not.
func readOnlyCapabilities() map[string]any {
	return map[string]any{
		"referencesProvider":      true,
		"definitionProvider":      true,
		"implementationProvider":  true,
		"hoverProvider":           true,
		"documentSymbolProvider":  true,
		"workspaceSymbolProvider": true,
		"textDocumentSync":        1,
	}
}

// safeBuffer is an io.Writer that may be written from several
// goroutines: the command itself and os/exec's stderr pump.
type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
