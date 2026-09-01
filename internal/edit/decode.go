package edit

import (
	"encoding/json"
	"fmt"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// Decode unmarshals a WorkspaceEdit from a server's JSON, rejecting
// edit shapes that the typed decoder would quietly turn into
// something else.
//
// This exists because of a sharp edge in the generated LSP types. The
// `edits` array of a TextDocumentEdit is a union, and its decoder
// tries AnnotatedTextEdit first; encoding/json ignores fields it does
// not know, so *any* object carrying a `range` decodes successfully as
// an AnnotatedTextEdit — with `newText` defaulting to "". A
// SnippetTextEdit, or a future edit kind, or a typo, therefore arrives
// as a well-formed instruction to **delete** that range. The applier
// downstream cannot tell the difference; here, before the union has
// eaten the evidence, we can.
//
// Callers that already hold a decoded *protocol.WorkspaceEdit are not
// wrong to pass it to [Stage] — every other defence still applies —
// but decoding through this function closes the one hole that turns a
// misunderstanding into deleted code.
func Decode(data []byte) (*protocol.WorkspaceEdit, error) {
	// The loose check runs first: its errors name the edit's position
	// in the document the server sent, which is more use than the
	// generated decoder's path into the Go type.
	if err := checkRawEdits(data); err != nil {
		return nil, err
	}
	var we protocol.WorkspaceEdit
	if err := json.Unmarshal(data, &we); err != nil {
		return nil, render.Errorf(render.CodeProtocolError, "edit: decoding workspace edit: %v", err)
	}
	return &we, nil
}

// rawWorkspaceEdit is the same document, decoded loosely enough to see
// what the typed decoder discarded.
type rawWorkspaceEdit struct {
	Changes         map[string][]json.RawMessage `json:"changes"`
	DocumentChanges []json.RawMessage            `json:"documentChanges"`
}

func checkRawEdits(data []byte) error {
	var raw rawWorkspaceEdit
	if err := json.Unmarshal(data, &raw); err != nil {
		return render.Errorf(render.CodeProtocolError, "edit: decoding workspace edit: %v", err)
	}

	for uri, edits := range raw.Changes {
		for i, e := range edits {
			if err := checkRawEdit(fmt.Sprintf("changes[%q][%d]", uri, i), e); err != nil {
				return err
			}
		}
	}
	for i, ch := range raw.DocumentChanges {
		var obj struct {
			TextDocument json.RawMessage   `json:"textDocument"`
			Edits        []json.RawMessage `json:"edits"`
		}
		if err := json.Unmarshal(ch, &obj); err != nil {
			return render.Errorf(render.CodeProtocolError,
				"edit: documentChanges[%d]: %v", i, err)
		}
		if obj.TextDocument == nil {
			continue // a create, rename or delete: no edits array
		}
		for j, e := range obj.Edits {
			if err := checkRawEdit(fmt.Sprintf("documentChanges[%d].edits[%d]", i, j), e); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRawEdit requires an edit element to be recognisably a text
// edit: a range, and a newText that is really a string. Anything else
// is refused by name rather than reinterpreted.
func checkRawEdit(where string, data json.RawMessage) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return render.Errorf(render.CodeEditConflict, "edit: %s: not an object: %v", where, err)
	}
	if _, ok := fields["snippet"]; ok {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: is a snippet edit, which has no meaning outside an editor", where)
	}
	if _, ok := fields["range"]; !ok {
		return render.Errorf(render.CodeEditConflict, "edit: %s: has no range", where)
	}
	newText, ok := fields["newText"]
	if !ok {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: has no newText; refusing to read it as a deletion", where)
	}
	var s string
	if err := json.Unmarshal(newText, &s); err != nil {
		return render.Errorf(render.CodeEditConflict, "edit: %s: newText is not a string", where)
	}
	return nil
}
