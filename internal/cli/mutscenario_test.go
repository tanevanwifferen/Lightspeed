package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// The M2 half of the scripted server (PLAN §7 tests item b).
//
// M1's canned-result harness covers almost everything a mutation test
// needs: a rename, a code action list and a formatting answer are all
// just JSON the server hands back. The one thing it cannot express is a
// server that talks *first* — and that is exactly what a code action
// arriving as a command does, because its edits come back as
// workspace/applyEdit requests rather than as the command's result.
//
// pushEdits is the marker for that case. It is recognised by
// mutationMethod, which scenario_test.go consults while building the
// method table.

// pushMarker is the key that turns a canned result into "send these
// workspace/applyEdit requests, then answer".
const pushMarker = "__push__"

// pushEdits scripts a method that pushes WorkspaceEdits at the client
// before answering. More than one edit is how the refusal to compose
// two independent edit sets is tested.
func pushEdits(edits ...any) map[string]any {
	return map[string]any{pushMarker: edits}
}

// mutationMethod recognises the markers this file defines. It reports
// false for an ordinary canned result, which the caller then serves
// verbatim.
func mutationMethod(method string, result json.RawMessage, record func(string, json.RawMessage)) (fakeserver.Method, bool) {
	var probe struct {
		Push   []json.RawMessage `json:"__push__"`
		Result json.RawMessage   `json:"__result__"`
	}
	if err := json.Unmarshal(result, &probe); err != nil || probe.Push == nil {
		return nil, false
	}
	return func(c *fakeserver.Conn, params json.RawMessage) (any, error) {
		record(method, params)
		for i, e := range probe.Push {
			// Request does not wait for the answer, so this runs on
			// the read loop without deadlocking; the client sees the
			// applyEdit requests before the response to this call.
			_ = c.Request(methodApplyEdit, map[string]any{
				"label": "scripted edit " + string(rune('1'+i)),
				"edit":  e,
			})
		}
		if probe.Result != nil {
			return probe.Result, nil
		}
		return nil, nil
	}, true
}

// mutationServerCaps is what a server that can do the M2 surface
// advertises. extra overrides or removes entries, so a test can take
// one capability away and watch the command fail loudly rather than
// call a method the server never claimed (PLAN §5.4).
func mutationServerCaps(extra map[string]any) map[string]any {
	caps := map[string]any{
		"renameProvider":             map[string]any{"prepareProvider": true},
		"codeActionProvider":         true,
		"documentFormattingProvider": true,
		"textDocumentSync":           1,
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

// tree writes a set of files into a fresh Go module and returns the
// module root. The go.mod is what the router's root-marker walk finds,
// so the workspace root is the directory itself.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.27\n")
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		write(t, path, content)
	}
	return dir
}

// snapshot reads every file under dir, keyed by slash-separated
// relative path. Comparing two snapshots is how "the tree is exactly
// as it was" is asserted without trusting the command's own report.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// assertUnchanged fails unless the tree is byte-identical to before.
func assertUnchanged(t *testing.T, dir string, before map[string]string) {
	t.Helper()
	after := snapshot(t, dir)
	if len(after) != len(before) {
		t.Fatalf("tree changed: %d files before, %d after (%s vs %s)",
			len(before), len(after), fileNames(before), fileNames(after))
	}
	for name, content := range before {
		got, ok := after[name]
		if !ok {
			t.Errorf("%s disappeared", name)
			continue
		}
		if got != content {
			t.Errorf("%s changed:\n got: %q\nwant: %q", name, got, content)
		}
	}
}

func fileNames(m map[string]string) string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// uriOf is the file URI of a path, as a server would send it.
func uriOf(path string) string { return "file://" + path }

// textEdit is one LSP TextEdit in UTF-16 coordinates.
func textEdit(line, startChar, endChar int, newText string) map[string]any {
	return map[string]any{
		"range": map[string]any{
			"start": map[string]any{"line": line, "character": startChar},
			"end":   map[string]any{"line": line, "character": endChar},
		},
		"newText": newText,
	}
}

// changesEdit builds a WorkspaceEdit in the `changes` form.
func changesEdit(perFile map[string][]any) map[string]any {
	changes := map[string]any{}
	for path, edits := range perFile {
		changes[uriOf(path)] = edits
	}
	return map[string]any{"changes": changes}
}
