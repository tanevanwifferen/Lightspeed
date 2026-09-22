package cli

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// textServer is the part of the scripted server that behaves like a language
// server about *files*: it answers documentSymbol from the text it was given (or
// read from disk, when the file is not open) and it answers references from a
// view of the disk that it caches on first read and drops only when told the
// file changed (workspace/didChangeWatchedFiles).
//
// The second half is the point. It is what makes a stale answer possible, so
// that a test can show the daemon prevents it: a server that re-read the disk
// on every request would pass a test of the reconciliation whether or not the
// reconciliation existed.
type textServer struct {
	on   bool
	mu   sync.Mutex
	open map[string]string // uri -> text of an open document
	disk map[string]string // uri -> the server's cached reading of the file
}

func newTextServer(on bool) *textServer {
	return &textServer{on: on, open: map[string]string{}, disk: map[string]string{}}
}

func (s *textServer) observe(method string, params json.RawMessage) {
	if !s.on {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch method {
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct{ URI, Text string } `json:"textDocument"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.open[p.TextDocument.URI] = p.TextDocument.Text
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument   struct{ URI string }    `json:"textDocument"`
			ContentChanges []struct{ Text string } `json:"contentChanges"`
		}
		if json.Unmarshal(params, &p) == nil && len(p.ContentChanges) > 0 {
			s.open[p.TextDocument.URI] = p.ContentChanges[len(p.ContentChanges)-1].Text
		}
	case "textDocument/didClose":
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		if json.Unmarshal(params, &p) == nil {
			delete(s.open, p.TextDocument.URI)
		}
	case "workspace/didChangeWatchedFiles":
		var p struct {
			Changes []struct{ URI string } `json:"changes"`
		}
		if json.Unmarshal(params, &p) == nil {
			for _, c := range p.Changes {
				delete(s.disk, c.URI)
			}
		}
	}
}

// view is what the server believes a file says: the open document, else its
// cached reading of the disk, made on first use.
func (s *textServer) view(uri string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text, ok := s.open[uri]; ok {
		return text
	}
	if text, ok := s.disk[uri]; ok {
		return text
	}
	u, err := url.Parse(uri)
	if err != nil {
		return ""
	}
	data, _ := os.ReadFile(u.Path)
	s.disk[uri] = string(data)
	return string(data)
}

// outlineSpansEnv makes the scripted outline give each function its whole body as
// its range (changed_symbols and churn map hunks onto ranges, so they need one).
const outlineSpansEnv = "LIGHTSPEED_TEST_OUTLINE_SPANS"

var textFunc = regexp.MustCompile(`^func (\w+)\(`)

func (s *textServer) install(methods map[string]fakeserver.Method, record func(string, json.RawMessage)) {
	if !s.on {
		return
	}
	methods["textDocument/documentSymbol"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		record("textDocument/documentSymbol", params)
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
		}
		out := []any{}
		lines := strings.Split(s.view(p.TextDocument.URI), "\n")
		for i, line := range lines {
			m := textFunc.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			nameAt := len("func ")
			// With LIGHTSPEED_TEST_OUTLINE_SPANS a function spans to its closing
			// brace at column 0, as a real server's range does; without it the
			// range is the func line alone, which the older tests rely on.
			endLine, endChar := i, len(line)
			if os.Getenv(outlineSpansEnv) == "1" {
				for j := i; j < len(lines); j++ {
					if lines[j] == "}" || (j == i && strings.HasSuffix(lines[j], "}")) {
						endLine, endChar = j, len(lines[j])
						break
					}
				}
			}
			out = append(out, map[string]any{
				"name": m[1], "kind": 12, "detail": "",
				"range": map[string]any{
					"start": map[string]any{"line": i, "character": 0},
					"end":   map[string]any{"line": endLine, "character": endChar},
				},
				"selectionRange": map[string]any{
					"start": map[string]any{"line": i, "character": nameAt},
					"end":   map[string]any{"line": i, "character": nameAt + len(m[1])},
				},
			})
		}
		return out, nil
	}
	// references: "every NEEDLE in other.go", from the cached view of that file.
	methods["textDocument/references"] = func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		record("textDocument/references", params)
		var p struct {
			TextDocument struct{ URI string } `json:"textDocument"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fakeserver.Error{Code: -32602, Message: err.Error()}
		}
		other := "file://" + filepath.Join(filepath.Dir(strings.TrimPrefix(p.TextDocument.URI, "file://")), "other.go")
		out := []any{}
		for i, line := range strings.Split(s.view(other), "\n") {
			if at := strings.Index(line, "NEEDLE"); at >= 0 {
				out = append(out, map[string]any{
					"uri": other,
					"range": map[string]any{
						"start": map[string]any{"line": i, "character": at},
						"end":   map[string]any{"line": i, "character": at + len("NEEDLE")},
					},
				})
			}
		}
		return out, nil
	}
}
