// Package serverdef holds the in-memory form of a language-server
// definition — the declarative, pure-data description of one server
// (PLAN §6) — plus a parser for its TOML encoding and a small set of
// built-in defaults.
//
// A definition answers two questions. "Does this server handle that
// file?" is answered by [Activation] (language ids, globs, root
// markers, priority) and evaluated by internal/router. "How do I run
// or obtain this server?" is answered by [Server] and [Install]; the
// daemon and the installer own that half.
//
// # Scope
//
// This package is deliberately inert: it parses, it validates, it
// copies. It does no file or process I/O at all. Config layering
// (workspace .lightspeed.toml over user servers.d over built-ins),
// PATH sniffing, the build-time generated corpus and mise delegation
// are M4 and live elsewhere; [Builtins] is a hand-written stand-in for
// the generated table, not that table.
//
// # TOML subset
//
// [Parse] implements the part of TOML v1.0.0 that server definitions
// need, and rejects the rest with a line-numbered error rather than
// guessing:
//
//	supported    comments; bare, quoted and dotted keys; [table]
//	             headers; basic and literal strings; integers
//	             (decimal, 0x, 0o, 0b, underscores); floats; booleans;
//	             arrays, including multi-line and trailing commas;
//	             inline tables, nested
//	rejected     arrays of tables ([[x]]), multi-line strings,
//	             offset/local date-times
//
// Newlines and a trailing comma inside inline tables are accepted,
// which TOML v1.0.0 forbids and v1.1 allows; nothing else is more
// permissive than the spec. A dependency-free parser is used because lightspeed has no
// third-party module dependencies yet (docs/DECISIONS.md D3).
package serverdef
