package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The LSP methods behind PLAN §4's read-only command surface. They are
// named here rather than inline so that the command table, the
// capability guard and the request all agree by construction.
const (
	methodDefinition      = "textDocument/definition"
	methodReferences      = "textDocument/references"
	methodImplementation  = "textDocument/implementation"
	methodHover           = "textDocument/hover"
	methodDocumentSymbol  = "textDocument/documentSymbol"
	methodWorkspaceSymbol = "workspace/symbol"
)

// textDocumentPosition builds the parameters shared by definition,
// references, implementation and hover.
func textDocumentPosition(uri protocol.DocumentURI, pos protocol.Position) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": string(uri)},
		"position":     map[string]any{"line": pos.Line, "character": pos.Character},
	}
}

// locationLink is the LocationLink half of the definition result
// union. Only the target fields matter to us: originSelectionRange
// describes where the *question* was asked, which the caller already
// knows.
type locationLink struct {
	TargetURI            protocol.DocumentURI `json:"targetUri"`
	TargetRange          protocol.Range       `json:"targetRange"`
	TargetSelectionRange *protocol.Range      `json:"targetSelectionRange"`
}

// decodeLocations decodes the three shapes a location-valued response
// may take: a single Location, an array of Location, or an array of
// LocationLink. Servers pick freely between them — gopls answers
// definition with Location[], rust-analyzer with LocationLink[] — and
// a client that only handles one silently reports "nothing found" for
// the other, which is precisely the failure PLAN §5.2 is about.
func decodeLocations(raw json.RawMessage) ([]protocol.Location, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	if raw[0] == '{' {
		var single protocol.Location
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, protocolError("location", err)
		}
		if single.URI == "" {
			// Not a Location after all; a LocationLink is also an
			// object, and some servers return a bare one.
			var link locationLink
			if err := json.Unmarshal(raw, &link); err != nil || link.TargetURI == "" {
				return nil, protocolError("location", fmt.Errorf("object is neither a Location nor a LocationLink"))
			}
			return []protocol.Location{link.location()}, nil
		}
		return []protocol.Location{single}, nil
	}

	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("locations", err)
	}
	out := make([]protocol.Location, 0, len(elems))
	for _, elem := range elems {
		var loc protocol.Location
		if err := json.Unmarshal(elem, &loc); err != nil {
			return nil, protocolError("location", err)
		}
		if loc.URI != "" {
			out = append(out, loc)
			continue
		}
		var link locationLink
		if err := json.Unmarshal(elem, &link); err != nil || link.TargetURI == "" {
			return nil, protocolError("location", fmt.Errorf("array element is neither a Location nor a LocationLink"))
		}
		out = append(out, link.location())
	}
	return out, nil
}

// location narrows a LocationLink to the range a CLI should print:
// the selection range (the identifier) when the server supplied one,
// the whole target otherwise.
func (l locationLink) location() protocol.Location {
	rng := l.TargetRange
	if l.TargetSelectionRange != nil {
		rng = *l.TargetSelectionRange
	}
	return protocol.Location{URI: l.TargetURI, Range: rng}
}

// hoverResult is the textDocument/hover response.
type hoverResult struct {
	Contents json.RawMessage `json:"contents"`
	Range    *protocol.Range `json:"range"`
}

// decodeHover decodes a hover response into its plain text and the
// range it describes, handling every shape the protocol still permits:
// MarkupContent, a bare string, a {language,value} MarkedString, and
// an array of either.
func decodeHover(raw json.RawMessage) (text string, rng *protocol.Range, err error) {
	if isJSONNull(raw) {
		return "", nil, nil
	}
	var res hoverResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", nil, protocolError("hover", err)
	}
	text, err = decodeMarkup(res.Contents)
	if err != nil {
		return "", nil, err
	}
	return strings.TrimRight(text, "\n"), res.Range, nil
}

// decodeMarkup flattens the MarkupContent / MarkedString union.
func decodeMarkup(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", nil
	}
	switch raw[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", protocolError("hover contents", err)
		}
		return s, nil
	case '{':
		var obj struct {
			Kind     string `json:"kind"`
			Value    string `json:"value"`
			Language string `json:"language"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return "", protocolError("hover contents", err)
		}
		return obj.Value, nil
	case '[':
		var elems []json.RawMessage
		if err := json.Unmarshal(raw, &elems); err != nil {
			return "", protocolError("hover contents", err)
		}
		parts := make([]string, 0, len(elems))
		for _, elem := range elems {
			part, err := decodeMarkup(elem)
			if err != nil {
				return "", err
			}
			if part != "" {
				parts = append(parts, part)
			}
		}
		return strings.Join(parts, "\n\n"), nil
	default:
		return "", protocolError("hover contents", fmt.Errorf("unexpected JSON value"))
	}
}

// documentSymbol is the hierarchical DocumentSymbol shape. Range
// covers the whole declaration and SelectionRange just the name; the
// name is what a `file:line:col` result should point at.
type documentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail"`
	Kind           int              `json:"kind"`
	Deprecated     bool             `json:"deprecated"`
	Range          protocol.Range   `json:"range"`
	SelectionRange protocol.Range   `json:"selectionRange"`
	Children       []documentSymbol `json:"children"`
}

// symbolInformation is the flat SymbolInformation / WorkspaceSymbol
// shape. WorkspaceSymbol is allowed to send a location carrying only a
// uri, so Location.Range may be the zero range and mean "unknown".
type symbolInformation struct {
	Name          string            `json:"name"`
	Kind          int               `json:"kind"`
	ContainerName string            `json:"containerName"`
	Location      protocol.Location `json:"location"`
}

// symbol is one entry of a symbols or workspace_symbol answer,
// flattened out of whichever shape the server used.
type symbol struct {
	Name string
	// Qualified is Name prefixed by its enclosing symbols, e.g.
	// "Server.Handle". It is what an agent greps for.
	Qualified string
	Kind      string
	Detail    string
	URI       protocol.DocumentURI
	// Range is where to point. It is the zero range when a
	// WorkspaceSymbol gave a uri and nothing else.
	Range protocol.Range
	// HasRange distinguishes "the symbol is at 0:0" from "the server
	// did not say where it is".
	HasRange bool
}

// decodeDocumentSymbols decodes textDocument/documentSymbol, which may
// answer with either the hierarchical or the flat shape, and returns
// the symbols in document order with hierarchy flattened into
// qualified names.
func decodeDocumentSymbols(raw json.RawMessage, uri protocol.DocumentURI) ([]symbol, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("documentSymbol", err)
	}
	if len(elems) == 0 {
		return nil, nil
	}
	// The two shapes are told apart by the field that only one of
	// them has: SymbolInformation carries "location", DocumentSymbol
	// carries "selectionRange".
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(elems[0], &probe); err != nil {
		return nil, protocolError("documentSymbol", err)
	}
	if _, flat := probe["location"]; flat {
		return decodeSymbolInformation(elems)
	}

	var tree []documentSymbol
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, protocolError("documentSymbol", err)
	}
	var out []symbol
	var walk func(prefix string, syms []documentSymbol)
	walk = func(prefix string, syms []documentSymbol) {
		for _, s := range syms {
			qualified := s.Name
			if prefix != "" {
				qualified = prefix + "." + s.Name
			}
			out = append(out, symbol{
				Name:      s.Name,
				Qualified: qualified,
				Kind:      symbolKindName(s.Kind),
				Detail:    s.Detail,
				URI:       uri,
				Range:     s.SelectionRange,
				HasRange:  true,
			})
			walk(qualified, s.Children)
		}
	}
	walk("", tree)
	return out, nil
}

// decodeWorkspaceSymbols decodes workspace/symbol.
func decodeWorkspaceSymbols(raw json.RawMessage) ([]symbol, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("workspace/symbol", err)
	}
	return decodeSymbolInformation(elems)
}

func decodeSymbolInformation(elems []json.RawMessage) ([]symbol, error) {
	out := make([]symbol, 0, len(elems))
	for _, elem := range elems {
		var info symbolInformation
		if err := json.Unmarshal(elem, &info); err != nil {
			return nil, protocolError("symbol", err)
		}
		if info.Location.URI == "" {
			return nil, protocolError("symbol", fmt.Errorf("symbol %q has no location", info.Name))
		}
		qualified := info.Name
		if info.ContainerName != "" {
			qualified = info.ContainerName + "." + info.Name
		}
		out = append(out, symbol{
			Name:      info.Name,
			Qualified: qualified,
			Kind:      symbolKindName(info.Kind),
			Detail:    info.ContainerName,
			URI:       info.Location.URI,
			Range:     info.Location.Range,
			HasRange:  hasRange(elem),
		})
	}
	return out, nil
}

// hasRange reports whether a SymbolInformation-shaped element actually
// carried a range. A WorkspaceSymbol may send `{"uri":…}` alone, and
// reporting that as line 1 column 1 without saying so would be a
// location the user cannot trust.
func hasRange(elem json.RawMessage) bool {
	var probe struct {
		Location map[string]json.RawMessage `json:"location"`
	}
	if err := json.Unmarshal(elem, &probe); err != nil {
		return false
	}
	_, ok := probe.Location["range"]
	return ok
}

// symbolKinds maps LSP SymbolKind numbers to names. Index 0 is unused:
// SymbolKind is 1-based.
var symbolKinds = [...]string{
	"", "file", "module", "namespace", "package", "class", "method",
	"property", "field", "constructor", "enum", "interface", "function",
	"variable", "constant", "string", "number", "boolean", "array",
	"object", "key", "null", "enum-member", "struct", "event",
	"operator", "type-parameter",
}

// symbolKindName names a SymbolKind, falling back to the number for a
// value from a newer protocol revision than this build knows.
func symbolKindName(kind int) string {
	if kind > 0 && kind < len(symbolKinds) {
		return symbolKinds[kind]
	}
	if kind == 0 {
		return ""
	}
	return fmt.Sprintf("kind-%d", kind)
}

// isJSONNull reports whether a raw result is absent or the JSON null,
// the two ways a server says "nothing".
func isJSONNull(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
}

// protocolError reports a malformed server response. It is exit code 4
// rather than 1: we did not get an answer, so we do not know that the
// answer is empty.
func protocolError(what string, err error) error {
	return render.Errorf(render.CodeProtocolError, "malformed %s in server response: %v", what, err)
}
