// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file contains a minimal excerpt of the generated LSP type
// declarations from gopls@v0.23.0 internal/protocol/tsprotocol.go and
// tsjson.go (themselves generated from the LSP 3.18 metaModel.json
// schema by protocol/generate).
//
// Only the declarations required by the vendored mapper.go, span.go,
// edits.go and tsdocument_changes.go are included; the full generated
// files are ~10k lines. See ATTRIBUTION at the repository root.

package protocol

import (
	"encoding/json"
	"fmt"
)

// Position in a text document expressed as zero-based line and character
// offset. Prior to 3.17 the offsets were always based on a UTF-16 string
// representation.
//
// Positions are line end character agnostic. So you can not specify a position
// that denotes `\r|\n` or `\n|` where `|` represents the character offset.
//
// @since 3.17.0 - support for negotiated position encoding.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#position
type Position struct {
	// Line position in a document (zero-based).
	Line uint32 `json:"line"`
	// Character offset on a line in a document (zero-based).
	//
	// The meaning of this offset is determined by the negotiated
	// `PositionEncodingKind`.
	Character uint32 `json:"character"`
}

// A range in a text document expressed as (zero-based) start and end positions.
//
// If you want to specify a range that contains a line including the line ending
// character(s) then use an end position denoting the start of the next line.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#range
type Range struct {
	// The range's start position.
	Start Position `json:"start"`
	// The range's end position.
	End Position `json:"end"`
}

// Represents a location inside a resource, such as a line
// inside a text file.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#location
type Location struct {
	URI   DocumentURI `json:"uri"`
	Range Range       `json:"range"`
}

// A text edit applicable to a text document.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#textEdit
type TextEdit struct {
	// The range of the text document to be manipulated. To insert
	// text into a document create a range where start === end.
	Range Range `json:"range"`
	// The string to be inserted. For delete operations use an
	// empty string.
	NewText string `json:"newText"`
}

// A special text edit with an additional change annotation.
//
// @since 3.16.0.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#annotatedTextEdit
type AnnotatedTextEdit struct {
	// The actual identifier of the change annotation
	AnnotationID *ChangeAnnotationIdentifier `json:"annotationId,omitempty"`
	TextEdit
}

// A string value used as a snippet is a template which allows to insert text
// and to control the editor cursor when insertion happens.
//
// @since 3.18.0
// @proposed
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#stringValue
type StringValue struct {
	// The kind of string value.
	Kind string `json:"kind"`
	// The snippet string.
	Value string `json:"value"`
}

// An interactive text edit.
//
// @since 3.18.0
// @proposed
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#snippetTextEdit
type SnippetTextEdit struct {
	// The range of the text document to be manipulated.
	Range Range `json:"range"`
	// The snippet to be inserted.
	Snippet StringValue `json:"snippet"`
	// The actual identifier of the snippet edit.
	AnnotationID *ChangeAnnotationIdentifier `json:"annotationId,omitempty"`
}

// A literal to identify a text document in the client.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#textDocumentIdentifier
type TextDocumentIdentifier struct {
	// The text document's uri.
	URI DocumentURI `json:"uri"`
}

// A text document identifier to optionally denote a specific version of a text document.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#optionalVersionedTextDocumentIdentifier
type OptionalVersionedTextDocumentIdentifier struct {
	// The version number of this document. If a versioned text document identifier
	// is sent from the server to the client and the file is not open in the editor
	// (the server has not received an open notification before) the server can send
	// `null` to indicate that the version is unknown and the content on disk is the
	// truth (as specified with document content ownership).
	Version int32 `json:"version"`
	TextDocumentIdentifier
}

// Describes textual changes on a text document. A TextDocumentEdit describes all changes
// on a document version Si and after they are applied move the document to version Si+1.
// So the creator of a TextDocumentEdit doesn't need to sort the array of edits or do any
// kind of ordering. However the edits must be non overlapping.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#textDocumentEdit
type TextDocumentEdit struct {
	// The text document to change.
	TextDocument OptionalVersionedTextDocumentIdentifier `json:"textDocument"`
	// The edits to be applied.
	//
	// @since 3.16.0 - support for AnnotatedTextEdit. This is guarded using a
	// client capability.
	//
	// @since 3.18.0 - support for SnippetTextEdit. This is guarded using a
	// client capability.
	Edits []Or_TextDocumentEdit_edits_Elem `json:"edits"`
}

// created for Or [AnnotatedTextEdit SnippetTextEdit TextEdit]
type Or_TextDocumentEdit_edits_Elem struct {
	Value any `json:"value"`
}

// A generic resource operation.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#resourceOperation
type ResourceOperation struct {
	// The resource operation kind.
	Kind string `json:"kind"`
	// An optional annotation identifier describing the operation.
	//
	// @since 3.16.0
	AnnotationID *ChangeAnnotationIdentifier `json:"annotationId,omitempty"`
}

// Options to create a file.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#createFileOptions
type CreateFileOptions struct {
	// Overwrite existing file. Overwrite wins over `ignoreIfExists`
	Overwrite bool `json:"overwrite,omitempty"`
	// Ignore if exists.
	IgnoreIfExists bool `json:"ignoreIfExists,omitempty"`
}

// Create file operation.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#createFile
type CreateFile struct {
	// A create
	Kind string `json:"kind"`
	// The resource to create.
	URI DocumentURI `json:"uri"`
	// Additional options
	Options *CreateFileOptions `json:"options,omitempty"`
	ResourceOperation
}

// Rename file options
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#renameFileOptions
type RenameFileOptions struct {
	// Overwrite target if existing. Overwrite wins over `ignoreIfExists`
	Overwrite bool `json:"overwrite,omitempty"`
	// Ignores if target exists.
	IgnoreIfExists bool `json:"ignoreIfExists,omitempty"`
}

// Rename file operation
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#renameFile
type RenameFile struct {
	// A rename
	Kind string `json:"kind"`
	// The old (existing) location.
	OldURI DocumentURI `json:"oldUri"`
	// The new location.
	NewURI DocumentURI `json:"newUri"`
	// Rename options.
	Options *RenameFileOptions `json:"options,omitempty"`
	ResourceOperation
}

// Delete file options
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#deleteFileOptions
type DeleteFileOptions struct {
	// Delete the content recursively if a folder is denoted.
	Recursive bool `json:"recursive,omitempty"`
	// Ignore the operation if the file doesn't exist.
	IgnoreIfNotExists bool `json:"ignoreIfNotExists,omitempty"`
}

// Delete file operation
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#deleteFile
type DeleteFile struct {
	// A delete
	Kind string `json:"kind"`
	// The file to delete.
	URI DocumentURI `json:"uri"`
	// Delete options.
	Options *DeleteFileOptions `json:"options,omitempty"`
	ResourceOperation
}

// Additional information that describes document changes.
//
// @since 3.16.0
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#changeAnnotation
type ChangeAnnotation struct {
	// A human-readable string describing the actual change. The string
	// is rendered prominent in the user interface.
	Label string `json:"label"`
	// A flag which indicates that user confirmation is needed
	// before applying the change.
	NeedsConfirmation bool `json:"needsConfirmation,omitempty"`
	// A human-readable string which is rendered less prominent in
	// the user interface.
	Description string `json:"description,omitempty"`
}

// An identifier to refer to a change annotation stored with a workspace edit.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#changeAnnotationIdentifier
type ChangeAnnotationIdentifier = string // (alias)

// A workspace edit represents changes to many resources managed in the workspace. The edit
// should either provide `changes` or `documentChanges`. If documentChanges are present
// they are preferred over `changes` if the client can handle versioned document edits.
//
// Since version 3.13.0 a workspace edit can contain resource operations as well. If resource
// operations are present clients need to execute the operations in the order in which they
// are provided.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#workspaceEdit
type WorkspaceEdit struct {
	// Holds changes to existing resources.
	Changes map[DocumentURI][]TextEdit `json:"changes,omitempty"`
	// Depending on the client capability `workspace.workspaceEdit.resourceOperations` document changes
	// are either an array of `TextDocumentEdit`s to express changes to n different text documents
	// where each text document edit addresses a specific version of a text document. Or it can contain
	// above `TextDocumentEdit`s mixed with create, rename and delete file / folder operations.
	//
	// Whether a client supports versioned document edits is expressed via
	// `workspace.workspaceEdit.documentChanges` client capability.
	//
	// If a client neither supports `documentChanges` nor `workspace.workspaceEdit.resourceOperations` then
	// only plain `TextEdit`s using the `changes` property are supported.
	DocumentChanges []DocumentChange `json:"documentChanges,omitempty"`
	// A map of change annotations that can be referenced in `AnnotatedTextEdit`s or create, rename and
	// delete file / folder operations.
	//
	// Whether clients honor this property depends on the client capability `workspace.changeAnnotationSupport`.
	//
	// @since 3.16.0
	ChangeAnnotations map[ChangeAnnotationIdentifier]ChangeAnnotation `json:"changeAnnotations,omitempty"`
}

// A parameter literal used in requests to pass a text document and a position inside that
// document.
//
// See https://microsoft.github.io/language-server-protocol/specifications/lsp/3.17/specification#textDocumentPositionParams
type TextDocumentPositionParams struct {
	// The text document.
	TextDocument TextDocumentIdentifier `json:"textDocument"`
	// The position inside the text document.
	//
	// Deprecated: gopls should use [TextDocumentPositionParams.Range] instead.
	Position Position `json:"position"`
	// Range is an optional field representing the user's text selection in the document.
	// If provided, the Position must be contained within this range.
	//
	// Note: This is a non-standard protocol extension. See microsoft/language-server-protocol#377.
	Range Range `json:"range"`
}

// UnmarshalError indicates that a JSON value did not conform to
// one of the expected cases of an LSP union type.
type UnmarshalError struct {
	msg string
}

func (e UnmarshalError) Error() string {
	return e.msg
}

func (t Or_TextDocumentEdit_edits_Elem) MarshalJSON() ([]byte, error) {
	switch x := t.Value.(type) {
	case AnnotatedTextEdit:
		return json.Marshal(x)
	case SnippetTextEdit:
		return json.Marshal(x)
	case TextEdit:
		return json.Marshal(x)
	case nil:
		return []byte("null"), nil
	}
	return nil, fmt.Errorf("type %T not one of [AnnotatedTextEdit SnippetTextEdit TextEdit]", t)
}

func (t *Or_TextDocumentEdit_edits_Elem) UnmarshalJSON(x []byte) error {
	if string(x) == "null" {
		t.Value = nil
		return nil
	}
	var h0 AnnotatedTextEdit
	if err := json.Unmarshal(x, &h0); err == nil {
		t.Value = h0
		return nil
	}
	var h1 SnippetTextEdit
	if err := json.Unmarshal(x, &h1); err == nil {
		t.Value = h1
		return nil
	}
	var h2 TextEdit
	if err := json.Unmarshal(x, &h2); err == nil {
		t.Value = h2
		return nil
	}
	return &UnmarshalError{"unmarshal failed to match one of [AnnotatedTextEdit SnippetTextEdit TextEdit]"}
}
