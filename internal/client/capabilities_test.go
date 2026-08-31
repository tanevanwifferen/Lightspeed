package client

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// PLAN §5.4: never call a method the server did not advertise. The
// guard has to be exact in both directions — refusing a supported
// method makes the tool useless, allowing an unsupported one produces
// a MethodNotFound that looks like a crash.

func parseCaps(t *testing.T, body string) *Capabilities {
	t.Helper()
	caps, err := ParseInitializeResult(json.RawMessage(body))
	if err != nil {
		t.Fatalf("ParseInitializeResult: %v", err)
	}
	return caps
}

func TestCapabilitiesGuard(t *testing.T) {
	caps := parseCaps(t, `{
		"capabilities": {
			"referencesProvider": true,
			"definitionProvider": {"workDoneProgress": true},
			"hoverProvider": false,
			"renameProvider": {"prepareProvider": true},
			"codeActionProvider": {"codeActionKinds": ["quickfix"]}
		},
		"serverInfo": {"name": "gopls", "version": "0.23.0"}
	}`)

	supported := []string{
		"textDocument/references",
		"textDocument/definition",
		"textDocument/rename",
		"textDocument/prepareRename",
		"textDocument/codeAction",
	}
	for _, method := range supported {
		if !caps.Supports(method) {
			t.Errorf("Supports(%q) = false, want true", method)
		}
		if err := caps.Check(method); err != nil {
			t.Errorf("Check(%q) = %v, want nil", method, err)
		}
	}

	unsupported := map[string]string{
		"textDocument/hover":                "hoverProvider",
		"textDocument/implementation":       "implementationProvider",
		"workspace/symbol":                  "workspaceSymbolProvider",
		"textDocument/formatting":           "documentFormattingProvider",
		"codeAction/resolve":                "codeActionProvider.resolveProvider",
		"textDocument/documentSymbol":       "documentSymbolProvider",
		"textDocument/prepareCallHierarchy": "callHierarchyProvider",
	}
	for method, wantCap := range unsupported {
		if caps.Supports(method) {
			t.Errorf("Supports(%q) = true, want false", method)
		}
		err := caps.Check(method)
		var unsup *UnsupportedMethodError
		if !errors.As(err, &unsup) {
			t.Fatalf("Check(%q) = %v, want *UnsupportedMethodError", method, err)
		}
		if !errors.Is(err, ErrUnsupportedMethod) {
			t.Errorf("Check(%q): errors.Is(err, ErrUnsupportedMethod) = false", method)
		}
		if unsup.Capability != wantCap {
			t.Errorf("Check(%q).Capability = %q, want %q", method, unsup.Capability, wantCap)
		}
		if unsup.ServerName != "gopls" {
			t.Errorf("Check(%q).ServerName = %q, want gopls", method, unsup.ServerName)
		}
		// The CLI maps this to an exit code without importing our
		// internals, by asserting an anonymous interface.
		ec, ok := err.(interface{ ExitCode() int })
		if !ok {
			t.Fatalf("Check(%q) error does not implement ExitCode() int", method)
		}
		if got := ec.ExitCode(); got != 3 {
			t.Errorf("ExitCode() = %d, want 3 (no server)", got)
		}
	}
}

// TestCapabilitiesNestedPaths: a bare `renameProvider: true` means
// rename without prepareRename. Reading it as "everything rename" is
// how you get a MethodNotFound in front of a user.
func TestCapabilitiesNestedPaths(t *testing.T) {
	caps := parseCaps(t, `{"capabilities":{"renameProvider":true,"semanticTokensProvider":{"full":true}}}`)
	if !caps.Supports("textDocument/rename") {
		t.Error("rename should be supported")
	}
	if caps.Supports("textDocument/prepareRename") {
		t.Error("prepareRename must not be inferred from a bare renameProvider")
	}
	if !caps.Supports("textDocument/semanticTokens/full") {
		t.Error("semanticTokens/full should be supported")
	}
	if caps.Supports("textDocument/semanticTokens/range") {
		t.Error("semanticTokens/range must not be inferred")
	}
}

// TestCapabilitiesUnguardedMethods: server extensions and lifecycle
// methods have no capability to check, and the `raw` escape hatch
// must keep working.
func TestCapabilitiesUnguardedMethods(t *testing.T) {
	caps := parseCaps(t, `{"capabilities":{}}`)
	for _, method := range []string{"rust-analyzer/expandMacro", "shutdown", "$/setTrace", "fake/echo"} {
		if !caps.Supports(method) {
			t.Errorf("Supports(%q) = false; unguarded methods must pass through", method)
		}
	}
	if _, guarded := CapabilityFor("rust-analyzer/expandMacro"); guarded {
		t.Error("CapabilityFor claims a server extension is guarded")
	}
	if cap, guarded := CapabilityFor("textDocument/references"); !guarded || cap != "referencesProvider" {
		t.Errorf("CapabilityFor(references) = %q, %v", cap, guarded)
	}
}

// TestCapabilitiesNilPermits: an unrecorded capability set (the raw
// command, which never parses one) must not refuse anything.
func TestCapabilitiesNilPermits(t *testing.T) {
	var caps *Capabilities
	if !caps.Supports("textDocument/references") {
		t.Error("a nil *Capabilities must permit every method")
	}
	if err := caps.Check("textDocument/references"); err != nil {
		t.Errorf("Check on nil = %v, want nil", err)
	}
	if caps.ServerName() != "" || caps.Raw() != nil || caps.Methods() != nil {
		t.Error("nil *Capabilities accessors must be zero-valued")
	}
}

// TestCapabilitiesMethods feeds the capability-derived command
// surface (PLAN §1, build item 7): --help must not offer what the
// server cannot do.
func TestCapabilitiesMethods(t *testing.T) {
	caps := parseCaps(t, `{"capabilities":{"referencesProvider":true,"hoverProvider":true}}`)
	got := caps.Methods()
	for _, want := range []string{"textDocument/references", "textDocument/hover"} {
		if !slices.Contains(got, want) {
			t.Errorf("Methods() = %v, missing %q", got, want)
		}
	}
	if slices.Contains(got, "textDocument/rename") {
		t.Errorf("Methods() offers rename, which was not advertised: %v", got)
	}
	if !slices.IsSorted(got) {
		t.Errorf("Methods() = %v, want sorted", got)
	}
}

// TestCapabilitiesMalformed: a server that answers initialize with
// something that is not an object is a broken server, and must be
// reported as such rather than silently permitting everything.
func TestCapabilitiesMalformed(t *testing.T) {
	if _, err := ParseInitializeResult(json.RawMessage(`"nope"`)); err == nil {
		t.Error("ParseInitializeResult accepted a non-object result")
	}
	caps := parseCaps(t, `{}`)
	if caps.Supports("textDocument/references") {
		t.Error("a result with no capabilities must support no guarded method")
	}
}
