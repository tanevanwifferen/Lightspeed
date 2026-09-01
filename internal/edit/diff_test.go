package edit

import (
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// mixedEdit exercises every shape a WorkspaceEdit has, over files with
// awkward bytes: CJK, a CRLF file, and a file with no final newline.
func mixedEdit(t *testing.T) (root string, we *protocol.WorkspaceEdit) {
	t.Helper()
	root = newTree(t, map[string]string{
		"a.go":       "package a\n\nfunc Old() {}\n",
		"b.go":       "package a\n\nfunc useB() { Old() }\n",
		"crlf.go":    "package a\r\n\r\nfunc Old() {}\r\n",
		"tail.go":    "package a\n\nfunc Old() {}",
		"unicode.go": "// 日本語\nvar 変数 = \"🎉\"\n",
		"gone.go":    "package gone\n",
		"moved.go":   "package moved\n\nfunc Old() {}\n",
	})
	return root, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "b.go"), 0, te(2, 14, 2, 17, "New")),
			docEdit(uriOf(root, "crlf.go"), 0, te(1, 0, 1, 0, "import \"fmt\"\n"), te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "tail.go"), 0, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "unicode.go"), 0, te(1, 4, 1, 6, "名前")),
			createOp(uriOf(root, "sub/new.go"), nil),
			docEdit(uriOf(root, "sub/new.go"), 0, te(0, 0, 0, 0, "package sub\n\nfunc New() {}\n")),
			renameOp(uriOf(root, "moved.go"), uriOf(root, "elsewhere.go"), nil),
			docEdit(uriOf(root, "elsewhere.go"), 0, te(2, 5, 2, 8, "New")),
			deleteOp(uriOf(root, "gone.go"), nil),
		},
	}
}

// TestChangeSetIsWhatGetsWritten: the preview is not a second,
// independently computed answer — it is the staged bytes.
func TestChangeSetIsWhatGetsWritten(t *testing.T) {
	root, we := mixedEdit(t)
	tx := mustStage(t, we, Options{Root: root})
	cs := tx.ChangeSet()
	if len(cs.Changes) != 8 {
		t.Fatalf("changes = %d, want 8: %+v", len(cs.Changes), cs.Changes)
	}

	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for _, c := range cs.Changes {
		switch c.Kind {
		case render.ChangeDelete:
			if exists(c.Path) {
				t.Errorf("%s: previewed as deleted but still on disk", c.Path)
			}
		case render.ChangeRename:
			if exists(c.Path) {
				t.Errorf("%s: previewed as renamed away but still on disk", c.Path)
			}
			if got := readFile(t, c.NewPath); got != c.After {
				t.Errorf("%s: on disk %q, previewed %q", c.NewPath, got, c.After)
			}
		default:
			if got := readFile(t, c.Path); got != c.After {
				t.Errorf("%s: on disk %q, previewed %q", c.Path, got, c.After)
			}
		}
	}
}

// TestDiffPipedToGitApplyReproducesApply is PLAN §8: the diff a caller
// previews and the write --apply performs are the same change. The
// test proves it by doing both to two copies of the same tree and
// comparing them byte for byte.
func TestDiffPipedToGitApplyReproducesApply(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}

	root, we := mixedEdit(t)
	tx := mustStage(t, we, Options{Root: root})

	patch, err := tx.Diff(render.Options{})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if strings.Contains(patch, root) {
		t.Errorf("patch contains absolute paths, so it is not -p1 appliable:\n%s", patch)
	}

	// A copy of the tree as it is now, to apply the patch to.
	mirror := filepath.Join(t.TempDir(), "mirror")
	copyTree(t, root, mirror)

	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	patchFile := filepath.Join(t.TempDir(), "change.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(git, "apply", "-p1", "--whitespace=nowarn", patchFile)
	cmd.Dir = mirror
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s\npatch:\n%s", err, out, patch)
	}

	applied := contentsOf(t, root)
	patched := contentsOf(t, mirror)
	for name, want := range applied {
		if got, ok := patched[name]; !ok {
			t.Errorf("%s: --apply wrote it, the patch did not", name)
		} else if got != want {
			t.Errorf("%s: --apply gave %q, the patch gave %q", name, want, got)
		}
	}
	for name := range patched {
		if _, ok := applied[name]; !ok {
			t.Errorf("%s: the patch wrote it, --apply did not", name)
		}
	}
}

// TestChangeMatchesRenderConstructor: for the ordinary single-file
// case, the staged change is exactly what render's own constructor
// builds from the same inputs. Staging computes its own bytes because
// it has to fold several operations together; this pins the two
// answers to each other for the case where both apply.
func TestChangeMatchesRenderConstructor(t *testing.T) {
	const content = "package a\n\nfunc Old() {}\n"
	root := newTree(t, map[string]string{"a.go": content})
	edits := []protocol.TextEdit{te(2, 5, 2, 8, "New"), te(0, 8, 0, 9, "b")}

	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{uriOf(root, "a.go"): edits},
	}, Options{Root: root})

	mine := tx.ChangeSet().Changes[0]
	theirs, err := render.NewChange(protocol.NewMapper(uriOf(root, "a.go"), []byte(content)), edits)
	if err != nil {
		t.Fatalf("render.NewChange: %v", err)
	}

	if mine.Before != theirs.Before || mine.After != theirs.After {
		t.Errorf("content differs:\n staged %q -> %q\n render %q -> %q",
			mine.Before, mine.After, theirs.Before, theirs.After)
	}
	if a, b := mustJSON(t, mine), mustJSON(t, theirs); a != b {
		t.Errorf("rendered change differs:\n staged %s\n render %s", a, b)
	}
}

// TestDiffOfNothingIsNothing: an edit set that changes nothing must
// not produce a patch that claims otherwise.
func TestDiffOfNothingIsNothing(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uriOf(root, "a.go"): {te(0, 8, 0, 9, "a")},
		},
	}, Options{Root: root})

	patch, err := tx.Diff(render.Options{})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if strings.Contains(patch, "@@") {
		t.Errorf("patch has hunks for a no-op edit:\n%s", patch)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	return string(out)
}

// contentsOf reads a tree into a map of relative path to content,
// ignoring permission bits: git apply honours the umask for new files
// and the applier sets the mode explicitly, so the two can differ
// legitimately.
func contentsOf(t *testing.T, root string) map[string]string {
	t.Helper()
	out := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(content)
		return nil
	})
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	return out
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o644)
	})
	if err != nil {
		t.Fatalf("copying %s: %v", src, err)
	}
}
