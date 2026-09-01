# Lightspeed

**`gopls`'s command-line interface, generalized to every language server.**

Lightspeed is an LSP client with a command line instead of an editor. It resolves
which language server handles a file, starts it, asks it a question, and prints
the answer as JSON — for coding agents first, scripted refactors second, humans
third.

Its reason to exist is not the command surface (gopls, Copilot CLI and several
MCP servers already have one) but the three unglamorous properties underneath
it:

- **Positions are byte-exact.** LSP columns are UTF-16 code units; command lines
  and humans think in bytes. The conversion uses gopls's own `Mapper`, vendored
  with attribution, so a CJK or emoji identifier does not shift every result by
  a few columns.
- **An empty answer is never guessed at.** A server that is still indexing will
  cheerfully answer "0 references", and an agent that believes it deletes live
  code. Lightspeed refuses: exit code 5, not an empty list. See
  [Readiness](#readiness-why-exit-5-exists).
- **Writes are all-or-nothing.** A rename across three files either applies
  completely or leaves the tree byte-identical, including when the server sends
  overlapping or out-of-range edits.

`PLAN.md` is the design document and the authority on scope; this file is the
user manual for what is built.

---

## Install

Go 1.27 or newer, no third-party module dependencies:

```
go install github.com/tanevanwifferen/Lightspeed/cmd/lightspeed@latest
```

or from a checkout:

```
go build -o lightspeed ./cmd/lightspeed
```

Language servers are **not** installed for you and never downloaded implicitly.
A file whose server is missing exits 3 with the exact command that would fix it,
usually a [mise](https://mise.jdx.dev) invocation:

```
$ lightspeed definition internal/store/user.go:42:8
{"version":1,"ok":false,"error":{"code":"server_not_installed",
 "message":"gopls handles this file but \"gopls\" is not on PATH; install it with: mise use -g go:golang.org/x/tools/gopls@v0.23.0", …}}
```

## Quick start

```sh
lightspeed definition internal/store/user.go:42:8
lightspeed references --symbol 'store.UserRepo.Find'
lightspeed hover internal/store/user.go:42:8 --format text
lightspeed symbols internal/store/user.go
lightspeed workspace_symbol 'handle*' --path ./internal

lightspeed rename internal/store/user.go:42:8 UserRepository        # preview (diff)
lightspeed rename internal/store/user.go:42:8 UserRepository --apply # write
lightspeed codeaction internal/store/user.go:42:8                   # list
lightspeed codeaction internal/store/user.go:42:8 --index 2 --apply
lightspeed format ./internal/store/user.go --apply

lightspeed check ./internal --format sarif > diagnostics.sarif
lightspeed call_hierarchy --symbol 'store.UserRepo.Find' --direction incoming

lightspeed help                       # the static surface
lightspeed help internal/store/user.go  # what that file's server actually offers
```

## Command surface

| command | what it does | LSP capability required |
|---|---|---|
| `definition <loc>` | where the symbol is defined | `definitionProvider` |
| `references <loc>` | every reference (`-d` to include the declaration) | `referencesProvider` |
| `implementation <loc>` | implementations of the symbol | `implementationProvider` |
| `hover <loc>` | signature and documentation | `hoverProvider` |
| `symbols <file>` | symbols declared in one file, in document order | `documentSymbolProvider` |
| `workspace_symbol <query>` | name search across the workspace | `workspaceSymbolProvider` |
| `rename <loc> <newname>` | rename across the workspace; preview by default | `renameProvider` |
| `codeaction <loc\|range>` | list the server's actions, or apply one | `codeActionProvider` |
| `format <path...>` | the server's own formatter | `documentFormattingProvider` |
| `check [path...]` | diagnostics for a file or a tree | — (see below) |
| `call_hierarchy <loc>` | who calls this, and what it calls | `callHierarchyProvider` |
| `batch` | one query per input line | — |
| `raw <method>` | send one JSON-RPC request, print the result | — (escape hatch) |
| `help [<file>\|<dir>]` | the surface, statically or from a live server | — |

The surface is **derived from capabilities at runtime**: `lightspeed help <file>`
starts the server that handles that file and reports what it actually
advertised, and a command whose capability is missing exits 3 naming the
commands that would have worked. Lightspeed never calls a method a server did
not advertise.

## Location syntax

gopls's span syntax, with **1-based lines** and **1-based byte columns**:

```
file.go                 the start of the file
file.go:12              line 12
file.go:12:5            line 12, byte column 5
file.go:12:5-12:9       a range
file.go:#1234           byte offset
file.go:#1234-#1240     a range of byte offsets
```

Every location lightspeed prints can be pasted back into lightspeed. Columns are
bytes on both sides of the conversion, whatever the file's encoding contains.

### `--symbol`, for when you cannot count columns

```sh
lightspeed references --symbol 'store.UserRepo.Find' --path ./internal
```

`--symbol` accepts a dotted path and resolves it through `workspace/symbol`, so
that a caller who knows the name but not the column (an agent, mostly) does not
have to guess one. `--path` says which workspace to search (default `.`).

The matching rule, in full:

1. A candidate's own dotted path is the server's `containerName` and name joined
   with a dot — exactly what `lightspeed workspace_symbol` prints.
2. The query matches when its segments are a **suffix** of that path, compared
   segment by segment and case-sensitively. `Type.Method` matches
   `pkg.Type.Method`; `Method` matches both; `Other.Method` matches neither.
3. An **exact** whole-path match discards the merely-suffix ones.
4. Candidates pointing at the same file and range are one candidate.

**Ambiguity is never resolved by picking one.** Two symbols answering to one
name are two different pieces of code, and choosing between them is how an agent
renames the wrong `Handle`. Several matches is exit 2, with every candidate
reported as a location so the retry is a copy-paste:

```
$ lightspeed references --symbol 'Handle' --path ./internal
{"version":1,"ok":false,"error":{"code":"usage",
 "message":"--symbol \"Handle\" matches 2 symbols; pass one of these locations instead: pkg.Server.Handle at server.go:31:18; pkg.Client.Handle at client.go:12:19",
 "data":{"symbol":"Handle","candidates":[{"symbol":"pkg.Server.Handle","kind":"method","location":"server.go:31:18"}, …]}}}
```

The resolved location is also reported in the envelope's `warnings`, so an agent
can see which symbol it got.

## Output contract

`--format json` (the default when stdout is not a terminal) wraps everything in
one envelope:

```json
{"version":1,"ok":true,"data":{…},"warnings":["…"]}
```

Failures use the same envelope with `ok:false` and a machine-readable code —
never a bare stack trace:

```json
{"version":1,"ok":false,"error":{"code":"not_ready","message":"…","data":{…}}}
```

| format | shape | available for |
|---|---|---|
| `json` | the envelope above, one line (`--indent` to pretty-print) | everything |
| `text` | `file:line:col: text`, one result per line, grep-compatible | queries, diagnostics, edits |
| `diff` | unified diff, feedable to `git apply`; the default for an edit preview | `rename`, `codeaction`, `format` |
| `sarif` | SARIF 2.1.0, **not** wrapped in the envelope | `check` |

Asking for a format that cannot describe a command's answer is a usage error
(exit 2) rather than an empty file.

**Token discipline.** The matched line only, by default. `--context N` adds
surrounding lines; `--limit N` caps the result count and always reports
`"truncated":true` plus a warning — a silent cutoff would be indistinguishable
from a complete answer. `--limit` never changes an exit code: `check --limit 1`
on a tree with errors still exits 1.

## Exit codes

| code | meaning |
|---|---|
| 0 | ok |
| 1 | problems found: diagnostics with errors, an authoritative empty answer, a rejected edit set |
| 2 | usage: the invocation is wrong, and no server was consulted |
| 3 | no server: nothing can answer, and installing or configuring something would fix it |
| 4 | crash or timeout: the result is unknown, not empty |
| 5 | not ready: the server is still indexing and any answer would be of unknown authority |

Exit 1 and exit 5 are never conflated. That distinction is the whole point.

### Readiness: why exit 5 exists

A language server answers requests while it is still loading the workspace, and
its answers are wrong in the worst possible way: they look authoritative and
they are empty. Lightspeed tracks `$/progress` and accepts an answer only when
one of these holds:

1. progress drained and the answer is non-empty;
2. progress drained and the answer was unchanged for `--settle` (750ms by
   default) — required before an **empty** answer is believed;
3. the server never used the progress protocol at all, and the answer was
   stable for `--settle` (reported as a warning: stability is all the evidence
   there is);
4. progress was announced but never drained, has been quiet for `--settle`, and
   a non-empty answer was stable (also warned about).

Anything else waits until `--timeout` (30s by default) and then exits 5. An
answer accepted under rule 3 or 4 carries a warning in the envelope, so a
second-class answer is visibly second-class.

## `check` and how it knows it has all the diagnostics

Diagnostics are the one answer LSP does not return from a request: servers push
`textDocument/publishDiagnostics` whenever they like, for whichever files they
like, and never say "that is all of them".

Where a server advertises the **pull** model (`textDocument/diagnostic`),
`check` uses it — a request that returns beats any amount of inference. `check`
selects that automatically; `--diagnostics pull|push|auto` overrides.

In push mode, a report is printed only when all three of these hold:

1. the readiness gate above says the workspace is loaded;
2. **every file `check` opened has been published about at least once**, an
   empty array included;
3. no diagnostic has arrived for `--settle`.

Condition 2 is the one with teeth. A file the server has never mentioned is not
a clean file, it is a file we know nothing about, and an agent that trusts a
silent `check` will commit. So a silent file is exit 5 with the file named:

```
$ lightspeed check ./internal
{"version":1,"ok":false,"error":{"code":"not_ready",
 "message":"gopls published no diagnostics for 1 of 12 file(s) (internal/gen/corpus.go) within 30s; a file the server never mentioned is unknown, not clean — pass --allow-silent to accept the silence, or --timeout to wait longer", …}}
```

`--allow-silent` accepts it and says so in the warnings, in those words: *this
is an assumption and not an answer*. Other flags: `--max-files N` bounds how
many documents one invocation opens (200 by default, truncation reported), and
`--language` names the language of a tree that nothing in it identifies.

`check` exits 1 if any diagnostic is error-severity, and 0 otherwise —
warnings, hints and notes do not fail the command. Diagnostics without a
severity are treated as warnings.

## Batch mode

```sh
$ printf '%s\n' \
    'references internal/store/user.go:42:8' \
    'definition --symbol store.UserRepo.Find --path ./internal' \
    'check ./internal/store' \
  | lightspeed batch
{"version":1,"ok":true,"data":{…},"query":{"index":1,"command":"references","argv":[…],"exit":0}}
{"version":1,"ok":true,"data":{…},"query":{"index":2,"command":"definition","argv":[…],"exit":0}}
{"version":1,"ok":true,"data":{…},"query":{"index":3,"command":"check","argv":[…],"exit":1}}
```

One query per input line, one envelope per output line. Each line is exactly
what the standalone command would have printed, plus a `query` field naming the
invocation — so a caller can develop against `lightspeed references …` and batch
it later without re-reading anything. Blank lines and `#` comments are skipped;
`--file <path>` reads from a file instead of stdin.

A per-query `--indent` is ignored: the answer is re-encoded compactly so the
stream stays JSON-lines. `lightspeed batch --indent` pretty-prints every line
instead, which is for reading by eye and breaks the one-envelope-per-line
contract on purpose.

Output is per-line rather than one big envelope because a batch is exactly where
the last query is the one that hangs: answers stream, and a batch killed by a
timeout still leaves the answers it produced, each a complete envelope. A query
whose output is not an envelope (`--format text|diff|sarif`) is wrapped in one
with its bytes as a string, so every line is JSON.

The line is tokenized like a shell's argument vector — single and double quotes,
backslash escapes — and nothing more: no globbing, no substitution, no
pipelines. An unterminated quote is an error, not a guess.

**Exit code:** the most severe outcome, ranked *ok < problems (1) < no server
(3) < not ready (5) < usage (2) < crash (4)* — "how much should the caller
worry", not the numeric order. A batch whose second line found problems must not
be able to hide a crash on its tenth. `--fail-fast` stops at the first non-zero
query; `--summary` adds a final envelope with the counts (opt-in, because the
default contract is one envelope per query and nothing else).

## `call_hierarchy`

```sh
lightspeed call_hierarchy internal/store/user.go:42:8 --direction incoming --depth 2
```

`--direction incoming|outgoing|both` (default `both`) and `--depth N` (1 to 5,
default 1). Each row points at the *other* symbol's declaration — the caller's
name for an incoming call, the callee's for an outgoing one — because that is
where the next command wants to go; the call site itself, and whatever signature
the server volunteered, are in the JSON payload's `detail` field. The label
carries an arrow and two spaces of indentation per level, so even the flat text
format reads as a tree:

```
$ lightspeed call_hierarchy internal/store/user.go:42:8 --direction incoming --depth 2 --format text
internal/http/router.go:31:6: <- registerRoutes
internal/main.go:12:1:   <- main
```

(`--format json` carries the same rows with `detail` filled in, e.g.
`"called at internal/http/router.go:44:9 (+2 more)"`.)

A call graph has cycles, so the traversal is bounded three ways: `--depth`, a
500-entry budget, and a visited set. Every bound that bites is reported in the
warnings; nothing is silently pruned. If the server reports several callable
symbols at the position, the hierarchy is for the first and the warnings name
the others.

## Server configuration

Six servers are built in — `gopls`, `rust-analyzer`, `pyright`, `vtsls`,
`clangd`, `lua-ls` — generated from the nvim-lspconfig corpus, so the common
case needs no configuration: install the server, and lightspeed finds it on
`PATH`.

A definition is pure data (PLAN §6). `.lightspeed.toml` in the workspace root:

```toml
schema_version = 1
name = "gopls"

[activation]
languages    = ["go", "gomod", "gotmpl"]
globs        = ["**/*.go", "**/go.mod"]
root_markers = ["go.work", "go.mod", ".git"]
priority     = 50

[server]
command   = ["gopls", "serve"]
transport = "stdio"
initialization_options = { usePlaceholders = false }
settings = { gopls = { "ui.diagnostic.staticcheck" = true } }

[install]
mise = "go:golang.org/x/tools/gopls@v0.23.0"
```

`--server NAME` picks between servers that both claim a file. Path resolution
walks up for root markers, so a nested module gets its own root.

**What is wired and what is not.** `internal/serverdef` implements the full
layering of PLAN §6 — `.lightspeed.toml`, then
`$XDG_CONFIG_HOME/lightspeed/servers.d/*.toml`, then the generated defaults,
then PATH sniffing, with per-key provenance — and `internal/daemon` implements
the warm-server pool. Neither is reachable from the command line yet: today the
CLI resolves against the **built-in table only** and starts a fresh server per
invocation. See [Not implemented](#not-implemented). `LIGHTSPEED_SERVER_CMD`
overrides the resolved server command; it is test and debugging scaffolding, not
configuration.

## Security posture

**Do not point lightspeed at code you do not trust.**

A language server runs the repository's own build tooling by design: gopls runs
`go list`, rust-analyzer runs `cargo metadata`, and both execute code and
configuration from the checkout. Running lightspeed on a hostile repository is
equivalent to running that repository's build. There is no sandbox in this
version (PLAN §6; `--sandbox` with bubblewrap/landlock is deferred).

What lightspeed does guarantee:

- **Nothing downloads implicitly.** A missing server exits 3 with the command
  that would install it. `serverdef` shells out to mise only on an explicit
  install request, and its probing calls are told not to install.
- **Nothing is written without `--apply`.** Every mutating command previews by
  default, and `--apply` refuses a dirty git worktree unless `--allow-dirty` is
  passed — an agent's only reliable undo is `git checkout`, and that only works
  if the worktree was clean first. Untracked files do not count as dirt.
- **A server cannot rewrite the tree on its own.** `workspace/applyEdit` is
  accepted only while a command that asked for edits is running, and every edit
  goes through the transactional applier: overlaps, stale versions and paths
  outside the workspace are refused with nothing written.
- **Server definitions are data.** No install scripts, no hooks, no code in
  configuration.

Server processes inherit lightspeed's environment and privileges. Their stderr
goes to lightspeed's stderr; machine output is stdout only, so stdout can be
parsed without filtering.

## Not implemented

Honest list, so that nothing above has to be read twice:

- **The daemon is not wired to the CLI.** `internal/daemon` is complete and
  tested (auto-spawn, idle reaping, an N-server pool keyed by workspace root),
  but no `lightspeed daemon` subcommand exposes it and commands do not dial it,
  so every invocation pays server startup. This is the single largest
  performance item left.
- **Config layering is not wired to the CLI.** `internal/serverdef` implements
  it; `resolveTarget` still consults the built-in table only, and there are no
  `servers`, `install` or `doctor` subcommands yet.
- **`--offline` / `LIGHTSPEED_OFFLINE`** exists in `serverdef` but has no flag
  on the command line, because nothing the CLI reaches can touch the network.
- **Deferred by PLAN §8:** the MCP surface, sandboxing, WASM plugins, a
  library/SDK, and merging several servers' answers into one report — `check`
  reports one workspace at a time and says so when it skips files belonging to
  another.
- **`--symbol` costs a second server session**, because the symbol names no file
  and the file it resolves to may belong to a different server. With the daemon
  wired up that becomes two pool lookups instead.
- **No integration tests against real servers.** Everything is tested against a
  hermetic scripted fake server; PLAN §7's build-tagged tests against real
  gopls and rust-analyzer are not written.

## Development

```sh
go build ./...
go vet ./...
go test ./...        # hermetic: no network, no real language server
gofmt -l .
```

Layout follows PLAN §7. `internal/gopls/` is vendored from gopls and
x/tools; `internal/gen/corpus/` is the nvim-lspconfig corpus. Both are covered
by `ATTRIBUTION` (BSD-3-Clause, The Go Authors; Apache-2.0, nvim-lspconfig
contributors). Design decisions that resolve PLAN's open questions are logged in
`docs/DECISIONS.md`. This project has not declared a licence of its own yet.
