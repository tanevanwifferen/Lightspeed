package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
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

// The symbol model — decoding documentSymbol in both of its shapes, kinds — is
// internal/symbols; these are its names inside this package.
type symbol = symbols.Symbol

var (
	decodeDocumentSymbols  = symbols.DecodeDocumentSymbols
	decodeWorkspaceSymbols = symbols.DecodeWorkspaceSymbols
	symbolKindName         = symbols.KindName
)

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
