// Package serverdef owns everything about "which language servers exist
// and how do I run one": the in-memory form of a definition (PLAN §6),
// its TOML encoding, the layered configuration that produces the
// effective set, PATH sniffing, and delegation of installation to mise.
//
// A definition answers two questions. "Does this server handle that
// file?" is answered by [Activation] (language ids, globs, root markers,
// priority) and evaluated by internal/router. "How do I run or obtain
// this server?" is answered by [Server] and [Install], and by the
// resolution below.
//
// # The layers
//
// [Load] folds PLAN §6's layers together, strongest first:
//
//  1. .lightspeed.toml in the workspace root        [LayerWorkspace]
//  2. $XDG_CONFIG_HOME/lightspeed/servers.d/*.toml  [LayerUser]
//  3. the table generated from the vendored corpus  [LayerBuiltin]
//  4. PATH sniffing                                 [BinaryPATH]
//
// The first three supply definitions and merge key by key: a workspace
// file that sets one key keeps everything else the weaker layers said
// (see [Fragment.ApplyTo] for the exact rules). The fourth supplies
// executables rather than definitions — a binary on PATH only means
// something once a layer has said what to do with it — so it is a
// [BinarySource] and not a [Layer]. "Zero configuration" is therefore a
// built-in definition plus a binary found on PATH, and every server in a
// [Resolution] says which layer gave it which key ([Resolved.Overrides])
// and where its executable came from ([Resolved.Binary]).
//
// Nothing is ever silently shadowed. A stronger layer's override is
// recorded, not applied invisibly; two definitions of one server inside
// a single layer are refused rather than resolved by filename luck; and
// a configuration file that cannot be parsed is an error, not a file
// that quietly does nothing.
//
// # Installing
//
// [InstallServer] shells out to mise — `mise use -g <spec>`, then
// `mise which` — and nothing in this package downloads anything itself
// (PLAN §1, §6). A server that is missing produces a
// [NotInstalledError]: exit code 3, carrying the exact command to run.
// [Options.Offline], ORed with [EnvOffline], is a hard kill switch on
// that one operation; resolution and queries keep working offline,
// because nothing they do needs the network.
//
// # The three commands
//
// [Servers], [InstallServer] and [Doctor] are the data behind
// `lightspeed servers`, `install` and `doctor`. They return structured
// reports and never print: rendering belongs to internal/render, and an
// agent reads the JSON envelope, not our prose.
//
// # TOML subset
//
// [ParseFragments] (and [Parse], for a single whole definition)
// implements the part of TOML v1.0.0 that server definitions need, and
// rejects the rest with a line-numbered error rather than guessing:
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
// permissive than the spec. A dependency-free parser is used because
// lightspeed has no third-party module dependencies yet
// (docs/DECISIONS.md D3).
//
// A file describes either one server, in the shape PLAN §6 shows, or
// several under [servers.<name>] tables — a polyglot repo needs more
// than one server in its single .lightspeed.toml.
//
// # Not here
//
// Path routing is internal/router's, which imports this package; the
// dependency does not point back, so [Doctor] takes a
// [Options.PathCheck] closure rather than a router. Spawning a server is
// internal/client's. Rendering is internal/render's.
package serverdef
