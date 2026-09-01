package edit

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

func TestDetectEOL(t *testing.T) {
	for _, tc := range []struct {
		content string
		want    lineEnding
	}{
		{"", eolUnknown},
		{"no newline at all", eolUnknown},
		{"a\nb\n", eolLF},
		{"a\r\nb\r\n", eolCRLF},
		{"a\r\nb", eolCRLF},
		{"a\r\nb\nc\r\n", eolUnknown}, // mixed: no safe answer
		{"a\rb", eolUnknown},          // a lone CR is not a line ending we know
	} {
		if got := detectEOL([]byte(tc.content)); got != tc.want {
			t.Errorf("detectEOL(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}

func TestNormalizeEOL(t *testing.T) {
	for _, tc := range []struct {
		text string
		eol  lineEnding
		want string
	}{
		{"a\nb", eolCRLF, "a\r\nb"},
		{"a\r\nb", eolCRLF, "a\r\nb"}, // already correct, not doubled
		{"a\r\nb\nc", eolCRLF, "a\r\nb\r\nc"},
		{"a\nb", eolLF, "a\nb"},
		{"a\nb", eolUnknown, "a\nb"}, // no evidence, no rewriting
		{"same", eolCRLF, "same"},
	} {
		if got := normalizeEOL(tc.text, tc.eol); got != tc.want {
			t.Errorf("normalizeEOL(%q, %v) = %q, want %q", tc.text, tc.eol, got, tc.want)
		}
	}
}

// TestCRLFPreserved: a server that emits \n, as almost all of them do,
// must not turn a CRLF file into a mixed one — that is a whole-file
// change nobody asked for.
func TestCRLFPreserved(t *testing.T) {
	const content = "package a\r\n\r\nfunc Old() {}\r\n"
	root := newTree(t, map[string]string{"a.go": content})

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0,
				te(1, 0, 1, 0, "import \"fmt\"\n"),
				te(2, 5, 2, 8, "New"),
			),
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := readFile(t, filepath.Join(root, "a.go"))
	want := "package a\r\nimport \"fmt\"\r\n\r\nfunc New() {}\r\n"
	if got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if strings.Count(got, "\n") != strings.Count(got, "\r\n") {
		t.Errorf("content has bare newlines: %q", got)
	}
}

// TestFinalNewlineIsNeverInvented: whether a file ends with a newline
// is a property of the file, not of the applier.
func TestFinalNewlineIsNeverInvented(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		edit    protocol.TextEdit
		want    string
	}{
		{
			name:    "LF file without a final newline",
			content: "package a\nfunc Old() {}",
			edit:    te(1, 5, 1, 8, "New"),
			want:    "package a\nfunc New() {}",
		},
		{
			name:    "CRLF file without a final newline",
			content: "package a\r\nfunc Old() {}",
			edit:    te(1, 5, 1, 8, "New"),
			want:    "package a\r\nfunc New() {}",
		},
		{
			name:    "append at end of file without a final newline",
			content: "package a\nfunc Old() {}",
			edit:    te(1, 13, 1, 13, " // done"),
			want:    "package a\nfunc Old() {} // done",
		},
		{
			name:    "file that does end with a newline keeps it",
			content: "package a\n",
			edit:    te(0, 8, 0, 9, "b"),
			want:    "package b\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newTree(t, map[string]string{"a.go": tc.content})
			tx := mustStage(t, &protocol.WorkspaceEdit{
				DocumentChanges: []protocol.DocumentChange{
					docEdit(uriOf(root, "a.go"), 0, tc.edit),
				},
			}, Options{Root: root})
			if _, err := tx.Apply(); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			got := readFile(t, filepath.Join(root, "a.go"))
			if got != tc.want {
				t.Errorf("content = %q, want %q", got, tc.want)
			}
			if hasFinalNewline([]byte(got)) != hasFinalNewline([]byte(tc.content)) {
				t.Errorf("final newline changed: %q -> %q", tc.content, got)
			}
		})
	}
}

// TestMixedLineEndingsLeftAlone: a file that already mixes terminators
// gives no evidence of what it wants, and guessing would rewrite lines
// the edit did not touch.
func TestMixedLineEndingsLeftAlone(t *testing.T) {
	const content = "a\r\nb\nc\r\n"
	root := newTree(t, map[string]string{"a.txt": content})

	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uriOf(root, "a.txt"): {te(1, 0, 1, 1, "B\nB")},
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := readFile(t, filepath.Join(root, "a.txt")), "a\r\nB\nB\nc\r\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestCRLFRoundTrip: an edit whose text is identical to what it
// replaces leaves the file byte-for-byte as it was, so nothing is
// written at all.
func TestCRLFRoundTrip(t *testing.T) {
	const content = "package a\r\n\r\nfunc Old() {}"
	root := newTree(t, map[string]string{"a.go": content})
	before := snapshot(t, root)

	tx := mustStage(t, &protocol.WorkspaceEdit{
		Changes: map[protocol.DocumentURI][]protocol.TextEdit{
			uriOf(root, "a.go"): {te(0, 0, 2, 13, content)},
		},
	}, Options{Root: root})
	res, err := tx.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.Changed() {
		t.Errorf("a no-op edit reported changes: %+v", res)
	}
	assertUnchanged(t, root, before)
}
