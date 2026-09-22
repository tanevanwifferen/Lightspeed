// Package index is the workspace's persistent, language-server-fed symbol
// index and the whole-repo queries on top of it: ranked symbol search, a
// token-budgeted repository map and the import graph.
//
// It differs from a prebuilt tree-sitter index in one property, and the rest
// of the design follows from it: nothing it reports is older than the file on
// disk. Every query revalidates the files it is about to report on (stat, then
// hash when the stat moved) and asks the language server again only about the
// files that changed. See docs/DECISIONS.md D32–D36.
//
// The package is deliberately free of the daemon, the CLI and the LSP client.
// What it needs from the outside — the file list, which server handles a file,
// and the outline of a file — arrives through [Backend], so that the daemon
// (which owns the warm language servers) and the tests (which have none) can
// both drive it.
package index

// SchemaVersion is the version of the persisted format and of the shape of a
// [File]. Bumping it discards every cache written by an older build.
const SchemaVersion = 1

// A Symbol is one declared symbol of an indexed file, as the language server
// described it.
type Symbol struct {
	// ID is the stable symbol id, `path::Container.Name#kind[~N]` (D21).
	ID string `json:"id"`
	// Name is the symbol's own name as the server reported it.
	Name string `json:"name"`
	// Qualified is the id's `Container.Name` part.
	Qualified string `json:"qualified"`
	Kind      string `json:"kind"`
	// Container is the qualified name of the enclosing symbol, "" at the top
	// level.
	Container string `json:"container,omitempty"`
	// Line and EndLine are 1-based and inclusive: the declaration itself, not
	// its doc comment.
	Line    int `json:"line"`
	EndLine int `json:"end_line"`
	// Parent is 1 + the index of the enclosing symbol in File.Symbols, 0 at
	// the top level.
	Parent int `json:"parent,omitempty"`
	// Signature is the server's detail, or the declaration as written.
	Signature string `json:"signature,omitempty"`
	// Doc is the first sentence of the leading doc comment.
	Doc string `json:"doc,omitempty"`
}

// An ImportRef is one import as the file's text says it, before it is resolved
// against the workspace: resolution depends on files other than this one, so
// it is redone whenever the graph is built and never persisted.
type ImportRef struct {
	// Spec is the module specifier as written: a Go import path, a Python
	// dotted module (with its leading dots for a relative one), a JS/TS string,
	// a Rust path, a C include path, a Lua module name.
	Spec string `json:"spec"`
	// Names are the names imported from Spec, for the languages where they can
	// name a submodule (`from pkg import mod` in Python).
	Names []string `json:"names,omitempty"`
	// Line is the 1-based line of the import.
	Line int `json:"line"`
	// Kind refines Spec where the language has more than one form: "import",
	// "require", "dynamic", "reexport", "use", "mod", "include", "system".
	Kind string `json:"kind,omitempty"`
}

// A File is one indexed workspace file: what it was when it was read, and what
// was learned from it.
type File struct {
	// Path is relative to the workspace root, with forward slashes.
	Path     string `json:"path"`
	Language string `json:"language,omitempty"`
	// Server is the name of the server definition that handles the file, ""
	// when none does (a file can still have imports).
	Server string `json:"server,omitempty"`

	// Size, MTimeNs and Hash say which bytes the rest was learned from. Hash
	// is the hex SHA-256 of the content and is the authority; the other two
	// are the cheap first check.
	Size    int64  `json:"size"`
	MTimeNs int64  `json:"mtime_ns"`
	Hash    string `json:"hash"`
	// RecordedNs is when the entry was written. A file modified within
	// racyWindow of that moment cannot be told from an unmodified one by its
	// stat alone (an edit in the same clock tick keeps size and mtime), so it
	// is hash-checked until it has aged past it.
	RecordedNs int64 `json:"recorded_ns"`

	// HasOutline is true once the server has been asked: it distinguishes a
	// file with no symbols from one whose outline was never built.
	HasOutline bool     `json:"has_outline,omitempty"`
	Symbols    []Symbol `json:"symbols,omitempty"`

	// HasImports is true once the file has been scanned for imports by an
	// extractor. A language with no extractor never has it, which is how "no
	// imports" is told from "not covered".
	HasImports bool        `json:"has_imports,omitempty"`
	Imports    []ImportRef `json:"imports,omitempty"`
}
