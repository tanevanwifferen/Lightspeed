package edit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// newTree writes a fixture tree and returns its root. Keys are slash
// separated paths relative to the root; a key ending in "/" is an
// empty directory.
func newTree(t *testing.T, files map[string]string) string {
	t.Helper()
	// Resolve the temp dir: on some systems it is itself a symlink,
	// and the applier compares resolved paths.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolving temp dir: %v", err)
	}
	for name, content := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if strings.HasSuffix(name, "/") {
			mustMkdir(t, path)
			continue
		}
		mustMkdir(t, filepath.Dir(path))
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	return root
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
}

// snapshot fingerprints a tree: every entry's relative path, kind,
// permission bits and content hash. Comparing two snapshots is how the
// tests assert "not a byte was written".
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			out[rel] = "symlink -> " + target
		case d.IsDir():
			out[rel] = "dir"
		default:
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			out[rel] = fmt.Sprintf("file %04o %s", info.Mode().Perm(), hex.EncodeToString(sum[:8]))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return out
}

// assertUnchanged fails with a readable diff if the tree moved.
func assertUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := snapshot(t, root)
	var problems []string
	for name, want := range before {
		if got, ok := after[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: gone (was %s)", name, want))
		} else if got != want {
			problems = append(problems, fmt.Sprintf("%s: %s -> %s", name, want, got))
		}
	}
	for name, got := range after {
		if _, ok := before[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s: appeared (%s)", name, got))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("tree changed but nothing should have been written:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(content)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// uriOf builds a file URI for a path inside root.
func uriOf(root, rel string) protocol.DocumentURI {
	return protocol.URIFromPath(filepath.Join(root, filepath.FromSlash(rel)))
}

// rng builds an LSP range from 0-based line/UTF-16-character pairs.
func rng(sl, sc, el, ec uint32) protocol.Range {
	return protocol.Range{
		Start: protocol.Position{Line: sl, Character: sc},
		End:   protocol.Position{Line: el, Character: ec},
	}
}

// te builds a TextEdit.
func te(sl, sc, el, ec uint32, newText string) protocol.TextEdit {
	return protocol.TextEdit{Range: rng(sl, sc, el, ec), NewText: newText}
}

// elems wraps TextEdits as the documentChanges union the wire uses.
func elems(edits ...protocol.TextEdit) []protocol.Or_TextDocumentEdit_edits_Elem {
	out := make([]protocol.Or_TextDocumentEdit_edits_Elem, 0, len(edits))
	for _, e := range edits {
		out = append(out, protocol.Or_TextDocumentEdit_edits_Elem{Value: e})
	}
	return out
}

// docEdit builds a TextDocumentEdit document change.
func docEdit(uri protocol.DocumentURI, version int32, edits ...protocol.TextEdit) protocol.DocumentChange {
	return protocol.DocumentChange{
		TextDocumentEdit: &protocol.TextDocumentEdit{
			TextDocument: protocol.OptionalVersionedTextDocumentIdentifier{
				Version:                version,
				TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uri},
			},
			Edits: elems(edits...),
		},
	}
}

// memSource is a Source backed by a map, for the version and overlay
// cases that the disk cannot express.
type memSource struct {
	docs map[string]Document
}

func (m memSource) ReadDocument(path string) (Document, error) {
	doc, ok := m.docs[path]
	if !ok {
		return Document{}, fmt.Errorf("open %s: %w", path, fs.ErrNotExist)
	}
	return doc, nil
}

// mustStage stages an edit that is expected to be valid.
func mustStage(t *testing.T, we *protocol.WorkspaceEdit, opts Options) *Transaction {
	t.Helper()
	tx, err := Stage(we, opts)
	if err != nil {
		t.Fatalf("Stage: unexpected error: %v", err)
	}
	return tx
}

// stageError stages an edit that is expected to be rejected and
// returns the message, having first asserted that the error is an edit
// conflict rather than a crash.
func stageError(t *testing.T, we *protocol.WorkspaceEdit, opts Options) string {
	t.Helper()
	tx, err := Stage(we, opts)
	if err == nil {
		t.Fatalf("Stage: expected an error, got a transaction touching %v", tx.Files())
	}
	return err.Error()
}

// wantContains fails unless got contains every fragment.
func wantContains(t *testing.T, got string, fragments ...string) {
	t.Helper()
	for _, f := range fragments {
		if !strings.Contains(got, f) {
			t.Errorf("message %q does not mention %q", got, f)
		}
	}
}

var (
	errInjected = errors.New("injected failure")
	errCause    = errors.New("the original problem")
)

func chmod(path string, m fs.FileMode) error { return os.Chmod(path, m) }

func mode(t *testing.T, path string) fs.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
}
