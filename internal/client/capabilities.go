package client

import (
	"encoding/json"
	"sort"
	"strings"
)

// methodCapability maps an LSP request method to the capability in
// InitializeResult.capabilities that a server must advertise before
// we are willing to call it (PLAN §5.4: "Never call uncapabilitied
// methods"). Dotted paths are nested lookups; a parent advertised as
// the bare value `true` therefore does not satisfy a nested path,
// which is exactly the LSP rule for e.g. prepareRename.
//
// Methods absent from this table are not guarded: server-specific
// extensions (`rust-analyzer/…`) and the lifecycle methods have no
// capability to check, and refusing them would break the `raw`
// escape hatch.
var methodCapability = map[string]string{
	"textDocument/definition":            "definitionProvider",
	"textDocument/declaration":           "declarationProvider",
	"textDocument/typeDefinition":        "typeDefinitionProvider",
	"textDocument/implementation":        "implementationProvider",
	"textDocument/references":            "referencesProvider",
	"textDocument/hover":                 "hoverProvider",
	"textDocument/documentSymbol":        "documentSymbolProvider",
	"textDocument/documentHighlight":     "documentHighlightProvider",
	"workspace/symbol":                   "workspaceSymbolProvider",
	"workspaceSymbol/resolve":            "workspaceSymbolProvider.resolveProvider",
	"textDocument/rename":                "renameProvider",
	"textDocument/prepareRename":         "renameProvider.prepareProvider",
	"textDocument/codeAction":            "codeActionProvider",
	"codeAction/resolve":                 "codeActionProvider.resolveProvider",
	"textDocument/formatting":            "documentFormattingProvider",
	"textDocument/rangeFormatting":       "documentRangeFormattingProvider",
	"textDocument/onTypeFormatting":      "documentOnTypeFormattingProvider",
	"textDocument/prepareCallHierarchy":  "callHierarchyProvider",
	"callHierarchy/incomingCalls":        "callHierarchyProvider",
	"callHierarchy/outgoingCalls":        "callHierarchyProvider",
	"textDocument/prepareTypeHierarchy":  "typeHierarchyProvider",
	"typeHierarchy/supertypes":           "typeHierarchyProvider",
	"typeHierarchy/subtypes":             "typeHierarchyProvider",
	"textDocument/diagnostic":            "diagnosticProvider",
	"workspace/diagnostic":               "diagnosticProvider.workspaceDiagnostics",
	"textDocument/foldingRange":          "foldingRangeProvider",
	"textDocument/selectionRange":        "selectionRangeProvider",
	"textDocument/documentLink":          "documentLinkProvider",
	"textDocument/inlayHint":             "inlayHintProvider",
	"textDocument/signatureHelp":         "signatureHelpProvider",
	"textDocument/completion":            "completionProvider",
	"textDocument/semanticTokens/full":   "semanticTokensProvider.full",
	"textDocument/semanticTokens/range":  "semanticTokensProvider.range",
	"workspace/executeCommand":           "executeCommandProvider",
	"workspace/willRenameFiles":          "workspace.fileOperations.willRename",
	"workspace/willCreateFiles":          "workspace.fileOperations.willCreate",
	"workspace/willDeleteFiles":          "workspace.fileOperations.willDelete",
	"textDocument/moniker":               "monikerProvider",
	"textDocument/linkedEditingRange":    "linkedEditingRangeProvider",
	"textDocument/colorPresentation":     "colorProvider",
	"textDocument/documentColor":         "colorProvider",
	"textDocument/codeLens":              "codeLensProvider",
	"codeLens/resolve":                   "codeLensProvider.resolveProvider",
	"textDocument/inlineValue":           "inlineValueProvider",
	"textDocument/prepareSelectionRange": "selectionRangeProvider",
}

// CapabilityFor reports the capability path a method requires, and
// whether the method is guarded at all.
func CapabilityFor(method string) (string, bool) {
	cap, ok := methodCapability[method]
	return cap, ok
}

// Capabilities records what a server advertised in its
// InitializeResult. A nil *Capabilities means "not recorded" and
// permits every method, so that code paths which never initialized
// (the `raw` escape hatch) keep working.
type Capabilities struct {
	raw           json.RawMessage
	caps          map[string]any
	serverName    string
	serverVersion string
}

// initializeResult is the slice of InitializeResult we care about.
type initializeResult struct {
	Capabilities map[string]any `json:"capabilities"`
	ServerInfo   *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"serverInfo"`
}

// ParseInitializeResult records the capabilities of an
// InitializeResult. A result that is not a JSON object is an error; a
// result without a capabilities field yields empty capabilities,
// which is a server that can do nothing but shut down.
func ParseInitializeResult(raw json.RawMessage) (*Capabilities, error) {
	var res initializeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, err
	}
	c := &Capabilities{raw: raw, caps: res.Capabilities}
	if c.caps == nil {
		c.caps = map[string]any{}
	}
	if res.ServerInfo != nil {
		c.serverName, c.serverVersion = res.ServerInfo.Name, res.ServerInfo.Version
	}
	return c, nil
}

// ServerName reports the server's self-reported name, "" if unknown.
func (c *Capabilities) ServerName() string {
	if c == nil {
		return ""
	}
	return c.serverName
}

// ServerVersion reports the server's self-reported version, "" if
// unknown.
func (c *Capabilities) ServerVersion() string {
	if c == nil {
		return ""
	}
	return c.serverVersion
}

// Raw returns the untouched InitializeResult.
func (c *Capabilities) Raw() json.RawMessage {
	if c == nil {
		return nil
	}
	return c.raw
}

// Lookup resolves a dotted capability path, reporting the value and
// whether it was present.
func (c *Capabilities) Lookup(path string) (any, bool) {
	if c == nil {
		return nil, false
	}
	var cur any = c.caps
	for _, part := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Has reports whether a dotted capability path is advertised as
// something other than false or null. LSP providers are either the
// boolean true or an options object, so anything else present counts
// as support.
func (c *Capabilities) Has(path string) bool {
	v, ok := c.Lookup(path)
	if !ok {
		return false
	}
	switch v := v.(type) {
	case bool:
		return v
	case nil:
		return false
	default:
		return true
	}
}

// Supports reports whether the method may be called: either it is not
// a guarded method, or its capability is advertised.
func (c *Capabilities) Supports(method string) bool {
	if c == nil {
		return true
	}
	path, guarded := methodCapability[method]
	if !guarded {
		return true
	}
	return c.Has(path)
}

// Check returns an *UnsupportedMethodError if the method must not be
// called, and nil otherwise. This is the guard of PLAN §5.4.
func (c *Capabilities) Check(method string) error {
	if c.Supports(method) {
		return nil
	}
	path := methodCapability[method]
	return &UnsupportedMethodError{Method: method, Capability: path, ServerName: c.ServerName()}
}

// Methods lists the guarded methods this server advertised, sorted.
// It is what a capability-derived command surface is built from
// (PLAN §1 build item 7), so `--help` never lies.
func (c *Capabilities) Methods() []string {
	if c == nil {
		return nil
	}
	var out []string
	for method := range methodCapability {
		if c.Supports(method) {
			out = append(out, method)
		}
	}
	sort.Strings(out)
	return out
}
