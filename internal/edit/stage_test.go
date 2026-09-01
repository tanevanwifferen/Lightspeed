package edit

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// TestStageWritesNothing is the premise the rest of the package rests
// on: staging is pure.
func TestStageWritesNothing(t *testing.T) {
	root := newTree(t, map[string]string{
		"a.go": "package a\n\nfunc Old() {}\n",
		"b.go": "package a\n\nfunc callsOld() { Old() }\n",
	})
	before := snapshot(t, root)

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "b.go"), 0, te(2, 18, 2, 21, "New")),
		},
	}, Options{Root: root})

	assertUnchanged(t, root, before)
	if tx.Empty() {
		t.Error("transaction reports itself empty but stages two edits")
	}
	if got, want := len(tx.Files()), 2; got != want {
		t.Errorf("Files() = %d entries, want %d", got, want)
	}
}

// TestThreeFileEditRejectedOnLastFile is the M2 done-when criterion:
// a multi-file edit that is bad anywhere is bad everywhere, and the
// tree is byte-identical afterwards.
func TestThreeFileEditRejectedOnLastFile(t *testing.T) {
	root := newTree(t, map[string]string{
		"a.go": "package a\n\nfunc Old() {}\n",
		"b.go": "package a\n\nfunc callsOld() { Old() }\n",
		"c.go": "package a\n",
	})
	before := snapshot(t, root)

	msg := stageError(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "b.go"), 0, te(2, 18, 2, 21, "New")),
			// c.go has two lines; line 40 does not exist.
			docEdit(uriOf(root, "c.go"), 0, te(40, 0, 40, 3, "boom")),
		},
	}, Options{Root: root})

	wantContains(t, msg, "c.go", "41:1-41:4", "outside the document")
	assertUnchanged(t, root, before)
}

// TestOverlappingEditsRejected is the scripted-malicious-server case:
// two edits fighting over the same bytes, where the result would
// depend on which one won.
func TestOverlappingEditsRejected(t *testing.T) {
	root := newTree(t, map[string]string{
		"a.go": "package a\n\nfunc Old() {}\n",
		"b.go": "package a\n",
	})
	before := snapshot(t, root)

	for _, tc := range []struct {
		name  string
		edits []protocol.TextEdit
		want  string
	}{
		{
			name:  "identical ranges",
			edits: []protocol.TextEdit{te(2, 5, 2, 8, "New"), te(2, 5, 2, 8, "Other")},
			want:  "3:6-3:9 and 3:6-3:9",
		},
		{
			name:  "partial overlap",
			edits: []protocol.TextEdit{te(2, 0, 2, 8, "func New"), te(2, 5, 2, 13, "Old() {}")},
			want:  "3:1-3:9 and 3:6-3:14",
		},
		{
			name:  "insertion inside a replacement",
			edits: []protocol.TextEdit{te(2, 0, 2, 13, "func New() {}"), te(2, 6, 2, 6, "X")},
			want:  "3:1-3:14 and 3:7",
		},
		{
			name:  "reversed order in the array",
			edits: []protocol.TextEdit{te(2, 5, 2, 13, "New() {}"), te(2, 0, 2, 8, "func New")},
			want:  "3:1-3:9 and 3:6-3:14",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := stageError(t, &protocol.WorkspaceEdit{
				DocumentChanges: []protocol.DocumentChange{
					docEdit(uriOf(root, "b.go"), 0, te(0, 0, 0, 7, "package")),
					docEdit(uriOf(root, "a.go"), 0, tc.edits...),
				},
			}, Options{Root: root})
			wantContains(t, msg, "a.go", "overlapping edits", tc.want, "nothing was written")
			assertUnchanged(t, root, before)
		})
	}
}

// TestInsertionsAtSameOffsetAllowed guards the other side of the
// overlap rule: several insertions at one point are not a conflict,
// and the vendored stable sort keeps them in the order the server
// listed them.
func TestInsertionsAtSameOffsetAllowed(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uriOf(root, "a.go"): {
				te(1, 0, 1, 0, "import \"fmt\"\n"),
				te(1, 0, 1, 0, "import \"os\"\n"),
			},
		},
	}, Options{Root: root})

	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "package a\nimport \"fmt\"\nimport \"os\"\n"
	if got := readFile(t, filepath.Join(root, "a.go")); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestRejectsEditOfMissingFile: an edit set that assumes a file we do
// not have is wrong about the workspace, not a licence to create one.
func TestRejectsEditOfMissingFile(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)

	msg := stageError(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "nope.go"), 0, te(0, 0, 0, 0, "x")),
		},
	}, Options{Root: root})
	wantContains(t, msg, "nope.go", "does not exist")
	assertUnchanged(t, root, before)
}

// TestVersionMismatch: the server computed the edit against a document
// that has since moved on. Applying it would edit the right offsets of
// the wrong text.
func TestVersionMismatch(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	path := filepath.Join(root, "a.go")
	src := memSource{docs: map[string]Document{
		path: {Content: []byte("package a\n"), Version: 7},
	}}
	before := snapshot(t, root)

	edit := func(version int32) *protocol.WorkspaceEdit {
		return &protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), version, te(0, 8, 0, 9, "b")),
		}}
	}

	msg := stageError(t, edit(3), Options{Root: root, Source: src})
	wantContains(t, msg, "a.go", "version 3", "version 7")
	assertUnchanged(t, root, before)

	// The matching version stages fine, and version 0 (LSP null) means
	// "the content on disk is the truth", so it is not checked.
	for _, v := range []int32{7, 0} {
		if _, err := Stage(edit(v), Options{Root: root, Source: src}); err != nil {
			t.Errorf("Stage with version %d: %v", v, err)
		}
	}
}

// TestUnversionedContentIsReported: an unverifiable version claim is
// accepted by default but never silently.
func TestUnversionedContentIsReported(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	we := &protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{
		docEdit(uriOf(root, "a.go"), 4, te(0, 8, 0, 9, "b")),
	}}

	tx := mustStage(t, we, Options{Root: root})
	warnings := tx.Warnings()
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unversioned") {
		t.Errorf("warnings = %q, want one mentioning unversioned content", warnings)
	}

	msg := stageError(t, we, Options{Root: root, RequireVersions: true})
	wantContains(t, msg, "a.go", "version 4", "unversioned")
}

// TestDocumentChangesWinOverChanges: both keys present is a
// specification violation with an unambiguous resolution. Applying
// both would double-apply their intersection.
func TestDocumentChangesWinOverChanges(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uriOf(root, "a.go"): {te(0, 8, 0, 9, "ignored")},
		},
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(0, 8, 0, 9, "b")),
		},
	}, Options{Root: root})

	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := readFile(t, filepath.Join(root, "a.go")), "package b\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if len(tx.Warnings()) == 0 || !strings.Contains(tx.Warnings()[0], "changes ignored") {
		t.Errorf("warnings = %q, want one saying changes was ignored", tx.Warnings())
	}
}

// TestChangesMapIsDeterministic: a map has no order, so the applier
// has to impose one. Same input, same staged bytes, every time.
func TestChangesMapIsDeterministic(t *testing.T) {
	files := map[string]string{}
	for _, name := range []string{"a.go", "b.go", "c.go", "d.go", "e.go"} {
		files[name] = "package a\n"
	}
	root := newTree(t, files)

	changes := map[protocol.DocumentURI][]protocol.TextEdit{}
	for name := range files {
		changes[uriOf(root, name)] = []protocol.TextEdit{te(0, 8, 0, 9, "z")}
	}

	var first string
	for i := range 10 {
		tx := mustStage(t, &protocol.WorkspaceEdit{Changes: changes}, Options{Root: root})
		out, err := json.Marshal(tx.Files())
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = string(out)
		} else if string(out) != first {
			t.Fatalf("staging order is not stable: %s vs %s", first, out)
		}
	}
}

// TestSnippetEditRejected: a snippet edit is an editor instruction,
// not a text edit. The vendored AsTextEdits panics on one; a server
// must not be able to panic the process.
func TestSnippetEditRejected(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)

	msg := stageError(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{{
			TextDocumentEdit: &protocol.TextDocumentEdit{
				TextDocument: protocol.OptionalVersionedTextDocumentIdentifier{
					TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uriOf(root, "a.go")},
				},
				Edits: []protocol.Or_TextDocumentEdit_edits_Elem{{
					Value: protocol.SnippetTextEdit{
						Range:   rng(0, 0, 0, 7),
						Snippet: protocol.StringValue{Kind: "snippet", Value: "${1:x}"},
					},
				}},
			},
		}},
	}, Options{Root: root})

	wantContains(t, msg, "a.go", "snippet")
	assertUnchanged(t, root, before)
}

// TestMalformedDocumentChangeRejected covers the union being neither
// one thing nor the other.
func TestMalformedDocumentChangeRejected(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	for name, ch := range map[string]protocol.DocumentChange{
		"empty": {},
		"two at once": {
			CreateFile: &protocol.CreateFile{Kind: "create", URI: uriOf(root, "new.go")},
			DeleteFile: &protocol.DeleteFile{Kind: "delete", URI: uriOf(root, "a.go")},
		},
		"null edit element": {TextDocumentEdit: &protocol.TextDocumentEdit{
			TextDocument: protocol.OptionalVersionedTextDocumentIdentifier{
				TextDocumentIdentifier: protocol.TextDocumentIdentifier{URI: uriOf(root, "a.go")},
			},
			Edits: []protocol.Or_TextDocumentEdit_edits_Elem{{Value: nil}},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			before := snapshot(t, root)
			stageError(t, &protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{ch}},
				Options{Root: root})
			assertUnchanged(t, root, before)
		})
	}
}

// TestRejectionsExitAsProblems: a rejected edit set is exit 1 —
// lightspeed worked and the answer is bad news — not exit 4, which
// would say lightspeed broke.
func TestRejectionsExitAsProblems(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	_, err := Stage(&protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{
		docEdit(uriOf(root, "a.go"), 0, te(9, 0, 9, 1, "x")),
	}}, Options{Root: root})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := render.ExitCode(err); got != render.ExitProblems {
		t.Errorf("ExitCode = %d, want %d", got, render.ExitProblems)
	}
	if got := render.CodeForError(err); got != render.CodeEditConflict {
		t.Errorf("CodeForError = %q, want %q", got, render.CodeEditConflict)
	}
}

// TestMissingRootRejected: there is no such thing as an unbounded
// apply.
func TestMissingRootRejected(t *testing.T) {
	if _, err := Stage(&protocol.WorkspaceEdit{}, Options{}); err == nil {
		t.Fatal("expected an error for an empty root")
	}
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	if _, err := Stage(&protocol.WorkspaceEdit{}, Options{Root: filepath.Join(root, "a.go")}); err == nil {
		t.Fatal("expected an error for a root that is not a directory")
	}
}

// TestNilEditIsEmpty: no edit is a successful no-op, not a crash.
func TestNilEditIsEmpty(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)
	tx := mustStage(t, nil, Options{Root: root})
	if !tx.Empty() {
		t.Error("nil edit is not empty")
	}
	res, err := tx.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Changed() {
		t.Errorf("nil edit reported changes: %+v", res)
	}
	assertUnchanged(t, root, before)
}

// TestUTF16Positions: the ranges a server sends are UTF-16 code unit
// offsets. Getting this wrong on a CJK or emoji line silently edits
// the wrong bytes, which is why the conversion is the vendored
// Mapper's job and not ours.
func TestUTF16Positions(t *testing.T) {
	const content = "// 日本語のコメント\nvar 変数 = \"🎉🎉\"\n"
	root := newTree(t, map[string]string{"a.go": content})

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			// "変数" starts at UTF-16 offset 4 on line 1 and is 2 units
			// long; the two emoji are a surrogate pair each.
			docEdit(uriOf(root, "a.go"), 0,
				te(1, 4, 1, 6, "名前"),
				te(1, 10, 1, 14, "🚀"),
			),
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "// 日本語のコメント\nvar 名前 = \"🚀\"\n"
	if got := readFile(t, filepath.Join(root, "a.go")); got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestSplitSurrogateRejected: a range landing inside a surrogate pair
// is not a position in the file.
func TestSplitSurrogateRejected(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "x := \"🎉\"\n"})
	before := snapshot(t, root)
	// The emoji occupies UTF-16 units 6 and 7; 7 is its second half.
	msg := stageError(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(0, 7, 0, 8, "x")),
		},
	}, Options{Root: root})
	wantContains(t, msg, "a.go")
	assertUnchanged(t, root, before)
}

// TestSequentialVersionsOnOneFile: two edits to one document are two
// document versions. An edit set that names the same version twice has
// ranges relative to text that no longer exists by the time the second
// one is applied.
func TestSequentialVersionsOnOneFile(t *testing.T) {
	const content = "package a\n\nfunc Old() {}\n"
	root := newTree(t, map[string]string{"a.go": content})
	path := filepath.Join(root, "a.go")
	opts := Options{Root: root, Source: memSource{docs: map[string]Document{
		path: {Content: []byte(content), Version: 7},
	}}}

	edits := func(v1, v2 int32) *protocol.WorkspaceEdit {
		return &protocol.WorkspaceEdit{DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), v1, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "a.go"), v2, te(0, 8, 0, 9, "b")),
		}}
	}

	tx := mustStage(t, edits(7, 8), opts)
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := readFile(t, path), "package b\n\nfunc New() {}\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}

	msg := stageError(t, edits(7, 7), opts)
	wantContains(t, msg, "a.go", "version 7", "version 8")
}
