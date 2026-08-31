// Package router answers the question every lightspeed command starts
// with: which language server handles this file, and what is its
// workspace root? It is PLAN §1 "build ourselves" item 1 — the part
// the gopls CLI never needed, because gopls only ever handles Go.
//
// The input is a path and a set of [serverdef.ServerDef]; the output
// is an ordered list of [Match], each carrying the resolved workspace
// root, or a [NoServerError] if nothing claims the file. The root
// travels with the match because the daemon keys one server session
// per (server, root) pair: two Go modules in one repo are two gopls
// sessions, and a Go file and a Rust file in one module are two
// servers over two different roots.
//
// # Root resolution
//
// Roots come from [serverdef.Activation.RootMarkers], which is an
// ordered list, most significant marker first. Resolution is
// marker-major, as in nvim-lspconfig's root_pattern: for each marker
// in turn, walk up from the file's directory and take the first
// ancestor that has it. Only if a marker is found nowhere does the
// next marker get a turn.
//
// The consequence worth knowing is that a *more significant marker
// further up wins over a less significant one nearby, which is
// exactly what Go wants: with markers ["go.work","go.mod",".git"], a
// file in a workspace member resolves to the go.work directory, not
// to its own go.mod. Within one marker, the nearest ancestor wins, so
// a module nested in a larger repo resolves to the module and not to
// the repo's .git.
//
// Refinements that require reading a marker's contents — resolving a
// Cargo workspace member to the workspace root, for instance — are
// deliberately absent. They are per-server quirks, and PLAN §5.4 puts
// quirks in declarative config, not in this package's control flow.
//
// If no marker matches anywhere, the file's own directory becomes the
// root and [Match.RootMarker] is empty. A single file outside any
// project is a legitimate query — gopls answers for a lone .go file —
// so this is a match, flagged as a fallback, rather than a refusal.
//
// # Claiming
//
// A definition claims a file if the file's language id is listed in
// its activation languages, or if the path matches one of its
// activation globs. Globs are matched against the path relative to
// that definition's resolved root (see [CompileGlob]), which is what
// stops an anchored pattern from reaching into a nested project.
//
// # Ordering
//
// Matches are ordered by descending [serverdef.Activation.Priority],
// then by name, so the order is total and stable and does not depend
// on the order definitions were loaded in. Nothing here merges results
// from several servers; PLAN §8 defers that. The order is a preference
// list for the caller.
//
// # Not here
//
// Where definitions come from — the workspace .lightspeed.toml, the
// user's servers.d, the generated defaults, PATH sniffing — is
// [serverdef]'s and M4's business. This package takes definitions as
// given and only reads directory entries to find roots.
package router
