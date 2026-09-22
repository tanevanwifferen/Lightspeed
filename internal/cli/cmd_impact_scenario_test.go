package cli

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// The word server is the scripted server's answer to "a server that knows where
// a name is used", for the commands that ask about uses (blast_radius,
// dead_code). It reads the workspace's .go files as text:
//
//   - documentSymbol lists `func Name(`, `func (r *T) Name(` (as gopls names it,
//     `(*T).Name`, kind method) and `type Name struct|interface` lines;
//   - references lists every whole-word occurrence of the word at the requested
//     position in the workspace's .go files, except the occurrence at the
//     position itself when includeDeclaration is false;
//   - implementation answers a location elsewhere for the method names the
//     scenario says implement an interface, and nothing for the rest.
//
// It is what makes "zero references" and "one reference" different answers per
// symbol, which a canned result cannot be.
const wordServerEnv = "LIGHTSPEED_TEST_WORDSERVER"

// wordSpec is the scenario's script for the word server.
type wordSpec struct {
	// Implements are the method names for which implementation answers a
	// location (they satisfy some interface).
	Implements []string `json:"implements,omitempty"`
	// NotCallable makes prepareCallHierarchy answer the way gopls does for a
	// type, constant or field: a JSON-RPC error ("X is not a function"), not an
	// empty list. On a line that declares a function it answers an empty list.
	NotCallable bool `json:"not_callable,omitempty"`
}

var (
	wordFuncRE   = regexp.MustCompile(`^func (\w+)\(`)
	wordMethodRE = regexp.MustCompile(`^func \((\w+) (\*?)(\w+)\) (\w+)\(`)
	wordTypeRE   = regexp.MustCompile(`^type (\w+) (struct|interface)`)
)

func installWordServer(methods map[string]fakeserver.Method, raw string, record func(string, json.RawMessage)) {
	if raw == "" {
		return
	}
	var spec wordSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return
	}
	pos := func(line, col int) map[string]any { return map[string]any{"line": line, "character": col} }
	rng := func(line, a, b int) map[string]any {
		return map[string]any{"start": pos(line, a), "end": pos(line, b)}
	}
	methods["textDocument/documentSymbol"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		record("textDocument/documentSymbol", params)
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
		}
		u, _ := url.Parse(p.TextDocument.URI)
		data, _ := os.ReadFile(u.Path)
		lines := strings.Split(string(data), "\n")
		out := []any{}
		for i, line := range lines {
			end := i
			if !strings.HasSuffix(strings.TrimSpace(line), "}") {
				for j := i + 1; j < len(lines); j++ {
					if lines[j] == "}" {
						end = j
						break
					}
				}
			}
			decl := func(name string, kind int, display string) {
				at := strings.Index(line, name+"(")
				if kind == 23 || kind == 11 {
					at = strings.Index(line, name+" ")
				}
				out = append(out, map[string]any{
					"name": display, "kind": kind, "detail": "",
					"range":          map[string]any{"start": pos(i, 0), "end": pos(end, len(lines[end]))},
					"selectionRange": rng(i, at, at+len(name)),
				})
			}
			switch {
			case wordMethodRE.MatchString(line):
				m := wordMethodRE.FindStringSubmatch(line)
				decl(m[4], 6, "("+m[2]+m[3]+")."+m[4])
			case wordFuncRE.MatchString(line):
				m := wordFuncRE.FindStringSubmatch(line)
				decl(m[1], 12, m[1])
			case wordTypeRE.MatchString(line):
				m := wordTypeRE.FindStringSubmatch(line)
				kind := 23
				if m[2] == "interface" {
					kind = 11
				}
				decl(m[1], kind, m[1])
			}
		}
		return out, nil
	}
	methods["textDocument/references"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		record("textDocument/references", params)
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     struct{ Line, Character int }
			Context      struct{ IncludeDeclaration bool }
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
		}
		u, _ := url.Parse(p.TextDocument.URI)
		data, _ := os.ReadFile(u.Path)
		lines := strings.Split(string(data), "\n")
		if p.Position.Line >= len(lines) {
			return []any{}, nil
		}
		word := wordAt(lines[p.Position.Line], p.Position.Character)
		out := []any{}
		if word == "" {
			return out, nil
		}
		re := regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`)
		root := filepath.Dir(u.Path)
		for dir := root; ; dir = filepath.Dir(dir) {
			if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
				root = dir
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
		_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			text, _ := os.ReadFile(path)
			for i, line := range strings.Split(string(text), "\n") {
				for _, m := range re.FindAllStringIndex(line, -1) {
					if !p.Context.IncludeDeclaration && path == u.Path && i == p.Position.Line && m[0] <= p.Position.Character && p.Position.Character <= m[1] {
						continue
					}
					out = append(out, map[string]any{"uri": "file://" + path, "range": rng(i, m[0], m[1])})
				}
			}
			return nil
		})
		return out, nil
	}
	if spec.NotCallable {
		methods["textDocument/prepareCallHierarchy"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
			record("textDocument/prepareCallHierarchy", params)
			var p struct {
				TextDocument struct{ URI string } `json:"textDocument"`
				Position     struct{ Line, Character int }
			}
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
			}
			u, _ := url.Parse(p.TextDocument.URI)
			data, _ := os.ReadFile(u.Path)
			lines := strings.Split(string(data), "\n")
			if p.Position.Line >= len(lines) {
				return []any{}, nil
			}
			if line := lines[p.Position.Line]; wordFuncRE.MatchString(line) || wordMethodRE.MatchString(line) {
				return []any{}, nil
			}
			return nil, &fakeserver.Error{Code: 0, Message: wordAt(lines[p.Position.Line], p.Position.Character) + " is not a function"}
		}
	}
	methods["textDocument/implementation"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		record("textDocument/implementation", params)
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
			Position     struct{ Line, Character int }
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
		}
		u, _ := url.Parse(p.TextDocument.URI)
		data, _ := os.ReadFile(u.Path)
		lines := strings.Split(string(data), "\n")
		if p.Position.Line >= len(lines) {
			return []any{}, nil
		}
		word := wordAt(lines[p.Position.Line], p.Position.Character)
		for _, name := range spec.Implements {
			if name == word {
				iface := "file://" + filepath.Join(filepath.Dir(u.Path), "iface.go")
				return []any{map[string]any{"uri": iface, "range": rng(0, 0, 1)}}, nil
			}
		}
		return []any{}, nil
	}
}

// wordAt is the identifier containing (or ending at) column col of line.
func wordAt(line string, col int) string {
	if col > len(line) {
		return ""
	}
	isWord := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	lo, hi := col, col
	for lo > 0 && isWord(line[lo-1]) {
		lo--
	}
	for hi < len(line) && isWord(line[hi]) {
		hi++
	}
	return line[lo:hi]
}

// impactCapabilities is what a server that can answer the impact commands
// advertises.
func impactCapabilities() map[string]any {
	return m5Capabilities(map[string]any{"implementationProvider": true})
}
