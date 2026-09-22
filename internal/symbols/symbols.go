// Package symbols is what lightspeed knows how to do with a language server's
// textDocument/documentSymbol answer, independent of who asked: decode both of
// its shapes into one flat list of [Symbol]s, locate a flat symbol's name in its
// declaration, and give every symbol its stable id (docs/DECISIONS.md D21).
//
// It exists as a package of its own because two things need exactly the same
// ids: the `outline`, `source` and `context` commands (internal/cli) and the
// workspace index (internal/index, fed by internal/daemon). Two copies of the
// id rules would let an id the index reports fail to resolve in `source`.
package symbols

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The id is `path::Container.Name#kind`. The path is split at the *first*
// separator and the kind at the *last* mark, because a qualified name can
// contain either (`ns::Foo` in C++) and a path cannot contain the first.
const (
	IDSeparator = "::"
	IDKindMark  = "#"
)

// protocolError reports a malformed server response. It is exit code 4
// rather than 1: we did not get an answer, so we do not know that the
// answer is empty.
func protocolError(what string, err error) error {
	return render.Errorf(render.CodeProtocolError, "malformed %s in server response: %v", what, err)
}

// isJSONNull reports whether a raw result is absent or the JSON null,
// the two ways a server says "nothing".
func isJSONNull(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed == "" || trimmed == "null"
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

// Symbol is one entry of a symbols or workspace_symbol answer,
// flattened out of whichever shape the server used.
type Symbol struct {
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
	// Full is the whole declaration — DocumentSymbol.range, or the range of
	// a flat SymbolInformation's location — where Range is the name. It is
	// what `source` slices. Zero for a workspace symbol, which has no more
	// than a location.
	Full protocol.Range
	// Signature is the server's own detail for a DocumentSymbol
	// ("func(a int) string"), which is not what a flat SymbolInformation's
	// Detail (its container name) is: that one is not a signature.
	Signature string
	// Container is a flat SymbolInformation's containerName: the qualified
	// name of the symbol it belongs to, which is not always one whose range
	// contains it (a Go method is declared outside its type).
	Container string
	// Parent is 1 + the index of the enclosing symbol in the same list, or 0
	// at the top level. It is set for document symbols only, and is what
	// `outline` nests by.
	Parent int
	// Flat marks a symbol that came from a flat SymbolInformation answer to
	// documentSymbol, whose location range is the whole declaration and has no
	// selectionRange: Range is then the declaration's start (the `func`
	// keyword) until PinpointNames finds the name inside it.
	Flat bool
}

// DecodeDocumentSymbols decodes textDocument/documentSymbol, which may
// answer with either the hierarchical or the flat shape, and returns
// the symbols in document order with hierarchy flattened into
// qualified names.
func DecodeDocumentSymbols(raw json.RawMessage, uri protocol.DocumentURI) ([]Symbol, error) {
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
		syms, err := decodeSymbolInformation(elems)
		if err != nil {
			return nil, err
		}
		for i := range syms {
			syms[i].Flat = true
		}
		nestByContainment(syms)
		return syms, nil
	}

	var tree []documentSymbol
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, protocolError("documentSymbol", err)
	}
	var out []Symbol
	var walk func(prefix string, parent int, syms []documentSymbol)
	walk = func(prefix string, parent int, syms []documentSymbol) {
		for _, s := range syms {
			qualified := s.Name
			if prefix != "" {
				qualified = prefix + "." + s.Name
			}
			out = append(out, Symbol{
				Name:      s.Name,
				Qualified: qualified,
				Kind:      KindName(s.Kind),
				Detail:    s.Detail,
				URI:       uri,
				Range:     s.SelectionRange,
				HasRange:  true,
				Full:      s.Range,
				Signature: s.Detail,
				Parent:    parent,
			})
			walk(qualified, len(out), s.Children)
		}
	}
	walk("", 0, tree)
	return out, nil
}

// DecodeWorkspaceSymbols decodes workspace/symbol.
func DecodeWorkspaceSymbols(raw json.RawMessage) ([]Symbol, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("workspace/symbol", err)
	}
	return decodeSymbolInformation(elems)
}

func decodeSymbolInformation(elems []json.RawMessage) ([]Symbol, error) {
	out := make([]Symbol, 0, len(elems))
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
		out = append(out, Symbol{
			Name:      info.Name,
			Qualified: qualified,
			Kind:      KindName(info.Kind),
			Detail:    info.ContainerName,
			URI:       info.Location.URI,
			Range:     info.Location.Range,
			HasRange:  hasRange(elem),
			Full:      info.Location.Range,
			Container: info.ContainerName,
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

// KindName names a SymbolKind, falling back to the number for a
// value from a newer protocol revision than this build knows.
func KindName(kind int) string {
	if kind > 0 && kind < len(symbolKinds) {
		return symbolKinds[kind]
	}
	if kind == 0 {
		return ""
	}
	return fmt.Sprintf("kind-%d", kind)
}

// KnownKind reports whether name is a kind KindName can produce: one of the
// LSP SymbolKind names, or the `kind-N` fallback for a newer protocol revision.
func KnownKind(name string) bool {
	if strings.HasPrefix(name, "kind-") {
		return len(name) > len("kind-")
	}
	for _, k := range symbolKinds[1:] {
		if k == name {
			return true
		}
	}
	return false
}

// KindNames lists the names KindName produces, in SymbolKind order.
func KindNames() []string { return symbolKinds[1:] }

// goReceiver matches the receiver gopls puts in a method's name —
// `(*Stage).Apply`, `(Set[T]).Add` — so that the id can spell it `Stage.Apply`,
// the way the code reads and the way a container-nested method is spelled.
var goReceiver = regexp.MustCompile(`^\(\*?([A-Za-z_][A-Za-z0-9_]*)(?:\[[^\]]*\])?\)\.`)

// IDName is the `Container.Name` of an id: the server's qualified name, with
// a Go receiver rewritten to the type it names.
func IDName(qualified string) string {
	return goReceiver.ReplaceAllString(qualified, "$1.")
}

// IDKind is the kind of an id. A server that sends no kind still gets a
// well-formed id.
func IDKind(kind string) string {
	if kind == "" {
		return "symbol"
	}
	return kind
}

// BaseID is the id of a symbol before duplicates are told apart.
func BaseID(rel string, sym Symbol) string {
	return rel + IDSeparator + IDName(sym.Qualified) + IDKindMark + IDKind(sym.Kind)
}

// IDs computes the ids of one file's symbols, parallel to syms.
//
// Two symbols that would share an id — an overload, an `init` in a Go
// package, a redeclaration — are told apart by position: the first, in
// document order, keeps the plain id and the later ones take `~2`, `~3`. The
// order is the symbols' own positions and not the server's answer order, so
// two servers that list the same file differently still agree. What this
// costs is that deleting an earlier duplicate renumbers the later ones, and
// the id then resolves to a different symbol; the alternative, numbering
// every duplicate from `~1`, would change the id of a symbol nobody touched
// the moment a second one appeared.
func IDs(rel string, syms []Symbol) []string {
	order := make([]int, len(syms))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return PosBefore(syms[order[a]].Range.Start, syms[order[b]].Range.Start)
	})
	seen := map[string]int{}
	ids := make([]string, len(syms))
	for _, i := range order {
		id := BaseID(rel, syms[i])
		seen[id]++
		if n := seen[id]; n > 1 {
			id += "~" + strconv.Itoa(n)
		}
		ids[i] = id
	}
	return ids
}

// PosBefore orders two LSP positions.
func PosBefore(a, b protocol.Position) bool {
	return a.Line < b.Line || (a.Line == b.Line && a.Character < b.Character)
}

// RangeContains reports whether outer covers inner, and is not the same range.
func RangeContains(outer, inner protocol.Range) bool {
	if outer == inner {
		return false
	}
	return !PosBefore(inner.Start, outer.Start) && !PosBefore(outer.End, inner.End)
}

// nestByContainment sets Parent for a flat symbol list — a server that answers
// with SymbolInformation has no hierarchy to give. A symbol belongs to the one
// its containerName names, when exactly one symbol has that qualified name (a
// Go method is declared outside its type, so its range is no evidence), and
// otherwise to the smallest symbol whose declaration strictly contains its own.
func nestByContainment(syms []Symbol) {
	byName := map[string][]int{}
	for i, sym := range syms {
		byName[sym.Qualified] = append(byName[sym.Qualified], i)
	}
	for i := range syms {
		if named := byName[syms[i].Container]; syms[i].Container != "" && len(named) == 1 && named[0] != i {
			syms[i].Parent = named[0] + 1
			continue
		}
		best := -1
		for j := range syms {
			if i == j || !RangeContains(syms[j].Full, syms[i].Full) {
				continue
			}
			if best < 0 || RangeContains(syms[best].Full, syms[j].Full) {
				best = j
			}
		}
		syms[i].Parent = best + 1
	}
}

// PinpointNames moves the Range of every flat symbol from the start of its
// declaration to its name. A flat SymbolInformation has no selectionRange, and
// real gopls answers documentSymbol in that form unless the client advertises
// hierarchicalDocumentSymbolSupport, so its Range starts at the `func` keyword:
// definition, references, hover and call_hierarchy asked about that position
// find no identifier. The name is looked for in the declaration's own text — the
// first whole-word occurrence outside a leading parenthesised group, which is
// what keeps `func (s *Stage) Apply` from being pinned to the receiver `s`. The
// warning names how many names could not be found; those symbols keep the
// declaration start, which is all the server said.
func PinpointNames(m *protocol.Mapper, syms []Symbol) []string {
	missed := 0
	for i := range syms {
		if !syms[i].Flat {
			continue
		}
		if rng, ok := findName(m, syms[i]); ok {
			syms[i].Range = rng
		} else {
			missed++
		}
	}
	if missed == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"%d symbols' names could not be located in their declarations; their positions are the declaration start",
		missed)}
}

// findName is the range of sym's name inside its declaration.
func findName(m *protocol.Mapper, sym Symbol) (protocol.Range, bool) {
	start, end, err := m.RangeOffsets(sym.Full)
	if err != nil || end > len(m.Content) {
		return protocol.Range{}, false
	}
	decl := m.Content[start:end]
	name := goReceiver.ReplaceAllString(sym.Name, "")
	for _, cand := range []string{name, LastSegment(name)} {
		if cand == "" {
			continue
		}
		at := wordIndex(decl, cand, true)
		if at < 0 {
			at = wordIndex(decl, cand, false)
		}
		if at < 0 {
			continue
		}
		from, err := m.OffsetPosition(start + at)
		if err != nil {
			return protocol.Range{}, false
		}
		to, err := m.OffsetPosition(start + at + len(cand))
		if err != nil {
			return protocol.Range{}, false
		}
		return protocol.Range{Start: from, End: to}, true
	}
	return protocol.Range{}, false
}

// wordIndex is the byte offset of the first occurrence of word in text that is
// a whole identifier. With shallow set, occurrences inside parentheses are
// skipped. -1 when there is none.
func wordIndex(text []byte, word string, shallow bool) int {
	depth := 0
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		}
		if (shallow && depth > 0) || !bytes.HasPrefix(text[i:], []byte(word)) {
			continue
		}
		if before, _ := utf8.DecodeLastRune(text[:i]); i > 0 && isIdentRune(before) {
			continue
		}
		if after, _ := utf8.DecodeRune(text[i+len(word):]); i+len(word) < len(text) && isIdentRune(after) {
			continue
		}
		return i
	}
	return -1
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func LastSegment(name string) string {
	if i := strings.LastIndexAny(name, ".:"); i >= 0 {
		return name[i+1:]
	}
	return name
}
