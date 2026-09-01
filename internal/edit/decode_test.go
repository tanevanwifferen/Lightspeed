package edit

import (
	"encoding/json"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

const snippetEditJSON = `{"documentChanges":[{
	"textDocument":{"uri":"file:///w/a.go","version":1},
	"edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":9}},
	          "snippet":{"kind":"snippet","value":"package ${1:b}"}}]
}]}`

// TestTypedDecoderTurnsASnippetIntoADeletion documents the hazard
// Decode exists for. The generated union decoder tries
// AnnotatedTextEdit first and encoding/json ignores fields it does not
// know, so an edit kind we do not support arrives as a well-formed
// instruction to delete the range it covers. If this test ever starts
// failing because the vendored types learned to reject it, Decode's
// newText check has become redundant and can go.
func TestTypedDecoderTurnsASnippetIntoADeletion(t *testing.T) {
	var we protocol.WorkspaceEdit
	if err := json.Unmarshal([]byte(snippetEditJSON), &we); err != nil {
		t.Fatalf("the typed decoder rejected it outright: %v", err)
	}
	edits := we.DocumentChanges[0].TextDocumentEdit.Edits
	if len(edits) != 1 {
		t.Fatalf("edits = %d, want 1", len(edits))
	}
	switch v := edits[0].Value.(type) {
	case protocol.AnnotatedTextEdit:
		if v.NewText != "" {
			t.Fatalf("newText = %q; the hazard has changed shape", v.NewText)
		}
	case protocol.SnippetTextEdit:
		t.Skip("the vendored decoder now recognises snippet edits")
	default:
		t.Fatalf("unexpected type %T", v)
	}

	if _, err := Decode([]byte(snippetEditJSON)); err == nil {
		t.Fatal("Decode accepted a snippet edit")
	} else {
		wantContains(t, err.Error(), "snippet")
	}
}

func TestDecodeRejections(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
	}{
		{
			name: "no newText",
			data: `{"changes":{"file:///w/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}}}]}}`,
			want: "refusing to read it as a deletion",
		},
		{
			name: "newText is not a string",
			data: `{"changes":{"file:///w/a.go":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},"newText":42}]}}`,
			want: "newText is not a string",
		},
		{
			name: "no range",
			data: `{"changes":{"file:///w/a.go":[{"newText":"x"}]}}`,
			want: "has no range",
		},
		{
			name: "edit is not an object",
			data: `{"changes":{"file:///w/a.go":["nope"]}}`,
			want: "not an object",
		},
		{
			name: "snippet in documentChanges",
			data: snippetEditJSON,
			want: "snippet",
		},
		{
			name: "not JSON at all",
			data: `{"changes":`,
			want: "decoding workspace edit",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Decode([]byte(tc.data))
			if err == nil {
				t.Fatal("Decode accepted it")
			}
			wantContains(t, err.Error(), tc.want)
		})
	}
}

// TestDecodeAcceptsRealEdits: the check must not get in the way of the
// shapes servers actually send, including annotated edits and resource
// operations, which carry no edits array at all.
func TestDecodeAcceptsRealEdits(t *testing.T) {
	const data = `{"documentChanges":[
		{"kind":"create","uri":"file:///w/new.go","options":{"overwrite":true}},
		{"kind":"rename","oldUri":"file:///w/a.go","newUri":"file:///w/b.go"},
		{"kind":"delete","uri":"file:///w/gone.go"},
		{"textDocument":{"uri":"file:///w/b.go","version":null},
		 "edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":1}},
		           "newText":"x","annotationId":"rename"}]}
	]}`
	we, err := Decode([]byte(data))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(we.DocumentChanges) != 4 {
		t.Fatalf("documentChanges = %d, want 4", len(we.DocumentChanges))
	}
	for i, ch := range we.DocumentChanges {
		if !ch.Valid() {
			t.Errorf("documentChanges[%d] is not a valid union value", i)
		}
	}
	if v := we.DocumentChanges[3].TextDocumentEdit.TextDocument.Version; v != 0 {
		t.Errorf("a null version decoded as %d, want 0 (unversioned)", v)
	}
}

// TestDecodeEmpty: an empty edit is a legitimate answer — a rename
// that changes nothing — and not something to fail on.
func TestDecodeEmpty(t *testing.T) {
	for _, data := range []string{`{}`, `null`, `{"changes":{}}`, `{"documentChanges":[]}`} {
		if _, err := Decode([]byte(data)); err != nil {
			t.Errorf("Decode(%s): %v", data, err)
		}
	}
}
