// Package edit will apply WorkspaceEdits to disk transactionally:
// all-or-nothing across files, overlap rejection, CRLF and final
// newline preservation (PLAN §5.3). Implemented in M2.
package edit
