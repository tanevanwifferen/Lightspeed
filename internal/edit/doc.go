// Package edit applies an LSP WorkspaceEdit to disk transactionally.
//
// The contract (PLAN §5.3) is all-or-nothing across files: [Stage]
// reads every file the edit touches, validates the whole edit set and
// computes the resulting bytes in memory, and [Transaction.Apply]
// commits them with a temp file plus atomic rename per file. If
// anything at all is wrong — one range out of bounds in the third of
// three files, one path that leaves the workspace, one version that
// has moved on — Stage fails and not a byte is written.
//
// The edit set comes from a third-party language server, so it is
// treated as hostile input rather than as an instruction: see
// [Stage] for the list of things that are rejected.
//
// The same staged bytes are what [Transaction.ChangeSet] renders, so
// `--format diff` piped to `git apply` reproduces `--apply` exactly
// (PLAN §8) — the preview is not a second, independently computed
// answer.
package edit
