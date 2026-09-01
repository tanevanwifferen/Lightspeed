package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// The M5 half of the scripted server (PLAN §7 tests item b).
//
// Two behaviours M1's canned-result harness cannot express:
//
//  1. *Pushed diagnostics.* `check` exists because diagnostics are a
//     notification, not an answer, so a canned result proves nothing
//     about it. This script publishes in reply to didOpen, and can be
//     told to stay silent about a file — which is the case that has to
//     become exit 5 rather than a clean report.
//  2. *A call graph.* A depth-2 traversal asks about the items the
//     first answer contained, so the answer has to depend on the
//     request. One canned result per method would answer the same
//     thing at every level and prove only that the cycle guard works.
const (
	// diagnosticsEnv maps an absolute path to a diagnostics array, as
	// JSON. A null value means "publish nothing for this file".
	diagnosticsEnv = "LIGHTSPEED_TEST_DIAGNOSTICS"
	// callGraphEnv maps a call-hierarchy item name to
	// {"incoming":[…],"outgoing":[…]}, as JSON.
	callGraphEnv = "LIGHTSPEED_TEST_CALLS"
)

// diagnosticsScript is a server that publishes diagnostics for the
// documents the client opens.
type diagnosticsScript struct {
	// byPath is the script; a present-but-null entry means silence.
	byPath map[string]json.RawMessage
	// known records which paths the script mentions at all, so that a
	// null entry can be told from an absent one.
	known map[string]bool
}

// newDiagnosticsScript parses the script from its environment
// variable. An empty variable yields a script that publishes nothing
// at all, which is what every non-check test wants.
func newDiagnosticsScript(raw string) *diagnosticsScript {
	s := &diagnosticsScript{byPath: map[string]json.RawMessage{}, known: map[string]bool{}}
	if raw == "" {
		return s
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return s
	}
	for path, diags := range parsed {
		s.known[path] = true
		if strings.TrimSpace(string(diags)) != "null" {
			s.byPath[path] = diags
		}
	}
	return s
}

// active reports whether the script has anything to do.
func (s *diagnosticsScript) active() bool { return len(s.known) > 0 }

// handle publishes diagnostics for a document the client just opened.
//
// It runs on the server's read loop, so the publish is written before
// the response to whatever the client asks next — which is what makes
// the test deterministic without a sleep anywhere.
func (s *diagnosticsScript) handle(c *fakeserver.Conn, method string, params json.RawMessage) {
	if !s.active() || method != "textDocument/didOpen" {
		return
	}
	var opened struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &opened); err != nil || opened.TextDocument.URI == "" {
		return
	}
	uri, err := protocol.ParseDocumentURI(opened.TextDocument.URI)
	if err != nil {
		return
	}
	path := uri.Path()
	if s.known[path] {
		diags, ok := s.byPath[path]
		if !ok {
			return // scripted silence: the server never mentions this file
		}
		_ = c.Notify("textDocument/publishDiagnostics", map[string]any{
			"uri":         opened.TextDocument.URI,
			"diagnostics": diags,
		})
		return
	}
	// A file the script does not mention is published as clean, which
	// is what a real server does for a file with no problems.
	_ = c.Notify("textDocument/publishDiagnostics", map[string]any{
		"uri":         opened.TextDocument.URI,
		"diagnostics": []any{},
	})
}

// installCallGraph adds per-item incomingCalls/outgoingCalls handlers.
// The item's name selects the answer, so a traversal that expands the
// answers gets different ones at each level.
func installCallGraph(methods map[string]fakeserver.Method, raw string, record func(string, json.RawMessage)) {
	if raw == "" {
		return
	}
	var graph map[string]struct {
		Incoming json.RawMessage `json:"incoming"`
		Outgoing json.RawMessage `json:"outgoing"`
	}
	if err := json.Unmarshal([]byte(raw), &graph); err != nil {
		return
	}
	answer := func(method string, outgoing bool) fakeserver.Method {
		return func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
			record(method, params)
			var req struct {
				Item struct {
					Name string `json:"name"`
				} `json:"item"`
			}
			if err := json.Unmarshal(params, &req); err != nil {
				return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
			}
			entry, ok := graph[req.Item.Name]
			if !ok {
				return []any{}, nil
			}
			if outgoing {
				return entry.Outgoing, nil
			}
			return entry.Incoming, nil
		}
	}
	methods[methodIncomingCalls] = answer(methodIncomingCalls, false)
	methods[methodOutgoingCalls] = answer(methodOutgoingCalls, true)
}

// -- fixtures shared by the M5 tests ----------------------------------

// diagnostic builds one LSP Diagnostic in UTF-16 coordinates, the way
// a server sends it.
func diagnostic(line, startChar, endChar, severity int, message, source, code string) map[string]any {
	d := map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": line, "character": startChar},
			"end":   map[string]any{"line": line, "character": endChar},
		},
		"severity": severity,
		"message":  message,
	}
	if source != "" {
		d["source"] = source
	}
	if code != "" {
		d["code"] = code
	}
	return d
}

// callItemJSON builds one CallHierarchyItem.
func callItemJSON(name string, kind int, file string, line, startChar, endChar int) map[string]any {
	rng := map[string]any{
		"start": map[string]any{"line": line, "character": startChar},
		"end":   map[string]any{"line": line, "character": endChar},
	}
	return map[string]any{
		"name":           name,
		"kind":           kind,
		"uri":            uriOf(file),
		"range":          rng,
		"selectionRange": rng,
	}
}

// incomingCall wraps an item as a CallHierarchyIncomingCall.
func incomingCall(item map[string]any, ranges ...map[string]any) map[string]any {
	return map[string]any{"from": item, "fromRanges": asAny(ranges)}
}

// outgoingCall wraps an item as a CallHierarchyOutgoingCall.
func outgoingCall(item map[string]any, ranges ...map[string]any) map[string]any {
	return map[string]any{"to": item, "fromRanges": asAny(ranges)}
}

// callRange is one entry of fromRanges, in UTF-16 coordinates.
func callRange(line, startChar, endChar int) map[string]any {
	return map[string]any{
		"start": map[string]any{"line": line, "character": startChar},
		"end":   map[string]any{"line": line, "character": endChar},
	}
}

func asAny(items []map[string]any) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// checkCapabilitiesFor is what a server able to answer M5's commands
// advertises. extra overrides or removes entries, so a test can take a
// capability away and watch the command refuse to call the method.
func m5Capabilities(extra map[string]any) map[string]any {
	caps := map[string]any{
		"textDocumentSync":        1,
		"callHierarchyProvider":   true,
		"workspaceSymbolProvider": true,
		"referencesProvider":      true,
		"definitionProvider":      true,
		"hoverProvider":           true,
		"documentSymbolProvider":  true,
	}
	for k, v := range extra {
		if v == nil {
			delete(caps, k)
			continue
		}
		caps[k] = v
	}
	return caps
}

// decodeDiagnosticsPayload mirrors the payload internal/render writes
// for a diagnostic set, so the tests can assert on the numbers an
// agent branches on.
type diagnosticsPayload struct {
	Diagnostics []struct {
		Path     string `json:"path"`
		Severity string `json:"severity"`
		Code     string `json:"code"`
		Source   string `json:"source"`
		Message  string `json:"message"`
		Start    struct {
			Line, Column, Offset int
		} `json:"start"`
		Tags []string `json:"tags"`
	} `json:"diagnostics"`
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Errors    int  `json:"errors"`
}

func decodeDiagnostics(t *testing.T, stdout string) diagnosticsPayload {
	t.Helper()
	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false, error: %+v", env.Error)
	}
	raw, err := json.Marshal(env.Data)
	if err != nil {
		t.Fatal(err)
	}
	var payload diagnosticsPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("data is not a diagnostic set: %v (%s)", err, raw)
	}
	return payload
}
