// Package docstore tracks the documents lightspeed has opened on a
// language server and caches a vendored gopls protocol.Mapper per
// file (PLAN §3, §5.1, §5.4).
//
// Two jobs, one cache:
//
//   - Lifecycle. Most servers answer nothing about a file until it
//     has been didOpen'd, and a document's version number must
//     increase monotonically across didChange notifications. The
//     store owns that bookkeeping so no command has to.
//
//   - Positions. LSP columns are UTF-16 code units; the CLI's
//     location syntax (file.go:line:col) is 1-based lines and *byte*
//     columns. Every conversion goes through gopls's Mapper, which is
//     vendored rather than reimplemented — PLAN §5.1 is explicit that
//     writing this ourselves is the wrong move.
//
// The store talks to the server through the Notifier interface, so it
// does not depend on internal/client and can be tested without one.
package docstore
