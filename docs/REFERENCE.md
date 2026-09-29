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
user manual for what is built. Wiring lightspeed into a coding agent:
[AGENT-SETUP.md](AGENT-SETUP.md).

---

## Install

Go 1.27 or newer. The one third-party module is the official MCP SDK, for
[`lightspeed mcp`](#mcp-server-for-coding-agents):

```
go install github.com/tanevanwifferen/Lightspeed/cmd/lightspeed@latest
```

or from a checkout:

```
go build -o lightspeed ./cmd/lightspeed
```

Language servers are **not** installed for you and never downloaded implicitly.
A file whose server is missing exits 3 with the exact command that would fix it,
usually a [mise](https://mise.jdx.dev) invocation (`lightspeed install gopls`
prints the same command, and `--run` runs it; see
[Server configuration](#server-configuration)):

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

lightspeed outline internal/store/user.go internal/store/repo.go     # symbols as a tree, with ids
lightspeed source 'internal/store/user.go::UserRepo.Find#method'     # just that symbol's text
lightspeed context 'internal/store/user.go::UserRepo.Find#method'    # + imports and hover
lightspeed references --id 'internal/store/user.go::UserRepo.Find#method'
lightspeed tree --prefix internal/store/                             # gitignore-aware file list
lightspeed repo_outline                                              # directories, languages, servers
lightspeed file README.md --start 1 --end 40                         # a line range, confined to the workspace

lightspeed search_symbols 'parse config' --kind function --limit 10  # ranked, whole workspace, never stale
lightspeed repo_map --budget 1500                                    # ranked, signature-level overview
lightspeed find_importers internal/store                             # who imports this package / file
lightspeed dependency_graph internal/store --depth 2 --direction both
lightspeed index status                                              # what is indexed, what is not, and why

lightspeed rename internal/store/user.go:42:8 UserRepository        # preview (diff)
lightspeed rename internal/store/user.go:42:8 UserRepository --apply # write
lightspeed codeaction internal/store/user.go:42:8                   # list
lightspeed codeaction internal/store/user.go:42:8 --index 2 --apply
lightspeed format ./internal/store/user.go --apply

lightspeed check ./internal --format sarif > diagnostics.sarif
lightspeed call_hierarchy --symbol 'store.UserRepo.Find' --direction incoming

lightspeed help                       # the static surface
lightspeed help internal/store/user.go  # what that file's server actually offers

lightspeed daemon status              # the warm servers behind this workspace
lightspeed daemon stop
lightspeed --no-daemon references internal/store/user.go:42:8   # a fresh server, in process

lightspeed servers                    # what is configured, from where, and whether it is installed
lightspeed doctor internal/store/user.go   # why is there no answer for this file?
lightspeed install gopls              # print the mise command; changes nothing
lightspeed install gopls --run        # actually run it
lightspeed --offline install gopls --run   # refused: nothing downloads offline
```

The second command that names a workspace does not start a language server: the
first one left a daemon running, and its warm server answers. See
[The daemon](#the-daemon).

## Command surface

| command | what it does | LSP capability required |
|---|---|---|
| `definition <loc>` | where the symbol is defined | `definitionProvider` |
| `references <loc>` | every reference (`-d` to include the declaration) | `referencesProvider` |
| `implementation <loc>` | implementations of the symbol | `implementationProvider` |
| `hover <loc>` | signature and documentation | `hoverProvider` |
| `symbols <file>` | symbols declared in one file, in document order, each with its id | `documentSymbolProvider` |
| `workspace_symbol <query>` | name search across the workspace, each hit with its id | `workspaceSymbolProvider` |
| `outline <file...>` | the symbols of one or more files as a tree: id, kind, signature, line range | `documentSymbolProvider` |
| `source <id\|loc...>` | the source text of one or more symbols, with a hash | `documentSymbolProvider` |
| `context <id\|loc>` | a symbol's source, its file's import block and the hover text | `documentSymbolProvider` |
| `tree [dir]` | the workspace's files, gitignore-aware, with language and server | — (no server needed) |
| `repo_outline [dir]` | directories with file counts, languages, servers and their install state — and symbol counts when the index is warm | — (no server needed) |
| `file <file>` | a line range of a file inside the workspace | — (no server needed) |
| `search_text <query>` | full-text search of the working tree: `file:line:col` and the line | — (no server needed) |
| `search_symbols <query>` | ranked symbol search across the whole workspace, from the index | `documentSymbolProvider` (per file, once) |
| `repo_map` | token-budgeted overview: files ranked by import centrality, top symbols each | as above |
| `find_importers <target>` | files that import a file, a package directory or a Go import path | — (no server needed) |
| `imports <file>` | what a file imports, resolved to workspace files or marked external | — (no server needed) |
| `dependency_graph [path]` | the import graph from a file, package or the whole workspace | — (no server needed) |
| `dependency_cycles` | import cycles; exit 1 if there are any | — (no server needed) |
| `index status\|build\|clear` | inspect, warm up or clear the persistent index | `documentSymbolProvider` (build) |
| `rename <loc> <newname>` | rename across the workspace; preview by default | `renameProvider` |
| `codeaction <loc\|range>` | list the server's actions, or apply one | `codeActionProvider` |
| `format <path...>` | the server's own formatter | `documentFormattingProvider` |
| `check [path...]` | diagnostics for a file or a tree | — (see below) |
| `call_hierarchy <loc>` | who calls this, and what it calls | `callHierarchyProvider` |
| `type_hierarchy <loc>` | supertypes and subtypes; falls back to `implementation` for subtypes, and says so | `typeHierarchyProvider` (else `implementationProvider`) |
| `check_references <name\|loc>` | is it used anywhere: semantic references plus whole-word text hits, separated, with a verdict | `referencesProvider` (text hits need none) |
| `blast_radius <loc\|file>` | what changing it affects: references, transitive callers, importers, tests; summary first | `referencesProvider`, `callHierarchyProvider` |
| `rename_check <loc> <newname>` | read-only rename verdict: acceptance, files/edits, collisions | `renameProvider` |
| `delete_check <loc>` | read-only delete verdict: outside references, exportedness, importers if last | `referencesProvider` |
| `dead_code [dir]` | unused-symbol candidates with confidence; bounded and resumable | `referencesProvider` |
| `changed_symbols` | symbols a git diff added, modified or removed | `documentSymbolProvider` |
| `churn [path]` / `hotspots` | git churn per file and symbol; churn × size ranking | — (git; symbols need `documentSymbolProvider`) |
| `related <loc>` | siblings, callers/callees, co-changed files, similar names | `callHierarchyProvider` (optional) |
| `task_context <task>` | a task in words → ranked symbols, source, callers, files, and a confidence | `documentSymbolProvider` |
| `guide` / `version` | the agent policy generated from this table; the build identity | — |
| `batch` | one query per input line | — |
| `raw <method>` | send one JSON-RPC request, print the result | — (escape hatch) |
| `daemon status\|stop\|logs` | inspect or stop the workspace's shared daemon | — |
| `servers` | the configured servers, which layer set each key, and whether each is installed | — |
| `install <name>` | print the mise command that installs a server; `--run` runs it | — |
| `doctor [path...]` | diagnose config layers, executables, mise and routing | — |
| `mcp` | serve the query commands as MCP tools over stdio | — |
| `help [<file>\|<dir>]` | the surface, statically or from a live server | — |

The surface is **derived from capabilities at runtime**: `lightspeed help <file>`
reaches the server that handles that file and reports what it actually
advertised, and a command whose capability is missing exits 3 naming the
commands that would have worked. Lightspeed never calls a method a server did
not advertise.

## The daemon

A language server is slow to start and much slower to become useful:
rust-analyzer needs 30–90 seconds to index, and an answer given before it has
finished is exactly the one lightspeed refuses to give (see
[Readiness](#readiness-why-exit-5-exists)). So by default every command that
needs a server talks to a **daemon** — one per workspace, started by the first
command that needs it, which keeps the language servers it started warm. The
second query pays a socket round trip instead of a startup.

```
$ lightspeed references src/lib.rs:10:5     # starts the daemon; pays for rust-analyzer
$ lightspeed references src/lib.rs:20:9     # asks the same rust-analyzer
$ lightspeed daemon status
{"version":1,"ok":true,"data":{"running":true,"workspace":"/home/me/proj","socket":"/run/user/1000/lightspeed/1f0c….sock",
 "log":"/run/user/1000/lightspeed/1f0c….log","pid":48213,"idle_timeout_seconds":1800,"session_idle_timeout_seconds":600,
 "requests":9,"spawns":1,"servers":[{"server":"rust-analyzer","root":"/home/me/proj","state":"ready","requests":9,"open_documents":0,…}]}}
```

(`requests` counts every call a command makes of the daemon — describing the
server, opening a document, the query, closing it — so one command is several.)

Every command listed above goes through it — `definition`, `references`,
`implementation`, `hover`, `symbols`, `workspace_symbol`, `rename`, `codeaction`,
`format`, `check`, `call_hierarchy`, `batch`, `raw`, `help <target>` and
`--symbol` resolution — and the output is **byte-identical** to the in-process
mode: the daemon runs the same code, and an error keeps its code and exit status
across the socket. A daemon whose server is still indexing exits 5, exactly as a
fresh server would; sharing a server never turns "not ready" into an empty
answer.

"Byte-identical" has one exception, and it is the text of a not-ready message,
which carries a wall-clock duration and an attempt count — `gave up after 30.2s
and 41 attempt(s)`. Those measure the run, so they differ between any two runs
of the same command in either mode; the code, the exit status and every other
byte are the same. The parity tests compare the message with that phrase
normalised.

**One daemon per workspace, not per directory.** The socket
(`$XDG_RUNTIME_DIR/lightspeed/<hash>.sock`, or a per-user directory under the
temp dir when that is unset) is keyed on the resolved workspace root: the
nearest ancestor with a `.git`, `.hg`, `.jj`, `.svn`, `go.work` or
`.lightspeed.toml`. `cd internal/store` reaches the daemon the repository root
already has. A repository holding several Go modules gets one daemon and one
gopls session per module.

**It leaves when it is idle.** A daemon with no client connected for thirty
minutes exits (`LIGHTSPEED_DAEMON_TIMEOUT=30s` shortens that for a daemon a
command starts), and a language server nobody has used for ten minutes is shut
down on its own, which returns its memory long before the daemon goes. Nothing is left running forever, and a killed daemon's stale socket is
replaced by the next command.

```sh
lightspeed daemon status [--path DIR]   # pid, socket, workspace, idle timeouts, and each
                                        # pooled server as starting | indexing | ready
lightspeed daemon stop   [--path DIR]   # returns once the daemon *process* has exited
lightspeed daemon logs   [--path DIR] [--lines N]
```

`--path` names any file or directory inside the workspace. "No daemon is running"
is an answer, not an error: `status` says `running:false`, `stop` says
`stopped:false`, both exit 0. `--format text` prints the human reading. The log
is the daemon's stderr, which includes every language server's, and is kept
beside the socket — it is appended to and never rotated.

**`--no-daemon`** (or `LIGHTSPEED_NO_DAEMON=1`) starts a fresh server for the one
command, in this process, and shuts it down when the command ends: nothing is
spawned, dialled or left behind. It is for CI, for debugging, and for anything
that must not depend on state left by an earlier command. It is accepted before
the subcommand or after it, and a `batch` passes it on to its queries.
`daemon status|stop|logs` with `--no-daemon` is a usage error: there is no
daemon to ask.

What a warm server does and does not know:

- A command **closes the documents it opened** when it finishes, so a warm
  server goes back to reading the disk and never answers from the copy it was
  last given. Documents are counted per client: while two commands running at
  once hold the same file it stays open, and it is closed when the last one
  finishes or its connection drops (a killed command does not leave it open).
- `check` reports only on the files it was asked about, however much a warm
  server has published about earlier ones.
- A command that **writes** (`--apply`) tells the server which files it wrote
  (`workspace/didChangeWatchedFiles`), since nothing else is watching the tree.
- A file changed by *something else* between two queries — an editor, `git
  checkout`, another agent — is seen. A file a command opens is re-read each time;
  and before answering **any** request the daemon scans the workspace (a stat per
  file, about a millisecond for a few hundred) and sends every live server
  `workspace/didChangeWatchedFiles` for what is new, changed or gone since the last
  scan, so a file merely reached through a reference cannot be answered from a
  server's stale copy either. Nothing changed, nothing sent. Checked against real
  gopls (which does answer stale without it); not against the other servers (D33).
- The daemon runs with the environment of the command that started it, `PATH`
  included. If a server is not found, or the wrong one is, `lightspeed daemon
  stop` and try again.
- **A changed configuration is never answered from a stale daemon.** The daemon
  reads the [server definitions](#server-configuration) once, when it starts, and
  each command compares a digest of the definitions it resolved with the
  daemon's. If they differ — someone edited `.lightspeed.toml` or a `servers.d`
  file — a daemon nobody else is using is stopped and a new one started (a note
  on stderr says so; its warm servers are lost), and a daemon that another
  command is connected to is left alone and the command fails with
  `daemon_stale`, exit 2, telling you to `lightspeed daemon stop` and retry.
  Edits that do not change a definition (a comment) do not restart anything; a
  change to `PATH` is not a configuration change and is not detected.
- **A daemon left by another build is replaced, never mixed in.** Each command
  compares its own build (the executable's size and modification time, and the
  daemon protocol version) with the daemon's. If they differ — you rebuilt
  `lightspeed`, reinstalled it, or run a checkout's binary beside the installed one —
  a daemon nobody else is using is stopped and this executable's own started, and
  the envelope carries a warning (`replaced the daemon for this workspace (pid N),
  which was a different lightspeed executable (/path, …)`; in `--format text` it is a
  line on stderr). A daemon another command is connected to is left alone and the
  command fails with `daemon_stale`, exit 2. `lightspeed daemon status` shows `build` and,
  when the next command would replace it, `stale_build`. Two builds used alternately
  take turns replacing each other's daemon, and lose its warm servers each time.
  Separately, a daemon whose own executable was rebuilt, reinstalled or deleted exits
  as soon as no client is connected (checked every ten seconds), rather than after the
  thirty-minute idle timeout (D29).
- **A server that dies while it starts says why.** If a language server exits, or
  fails its handshake, during startup, the `spawn_failed` error (exit 4) carries its exit
  status and the last 2 KiB of its stderr — a one-line summary in `error.message`, the
  whole tail in `error.data.stderr_tail` (`exit_status`, `exit_code`, `command` too) —
  with the daemon and with `--no-daemon`, so the reason is not only in the daemon's log
  (D28).

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

### `--id`, a symbol from an earlier answer

Every command that takes a location also takes `--id` with a [symbol
id](#reading-code-by-symbol), and exactly one of a location, `--symbol` and
`--id` may be given:

```sh
lightspeed references --id 'internal/store/user.go::UserRepo.Find#method'
```

`--path` says which workspace the id's path is relative to (default `.`). An id
that no longer names a symbol is exit 1, `stale_id`, with the nearest candidates —
see below.

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

## Reading code by symbol

The point of `outline`, `source` and `context` is to read *one symbol* instead
of a file. An agent lists what a file declares, then fetches just the one it
needs, by a stable id:

The shape of the answers, abridged; the values are illustrative:

```
$ lightspeed outline internal/edit/stage.go
{"version":1,"ok":true,"data":{"files":[{"file":"internal/edit/stage.go","language":"go","count":21,"symbols":[
  {"id":"internal/edit/stage.go::Stage#struct","kind":"struct","name":"Stage","signature":"type Stage struct","line":18,"end_line":31,
   "children":[{"id":"internal/edit/stage.go::Stage.files#field","kind":"field","name":"files","signature":"files map[string]*staged","line":19,"end_line":19}]},
  {"id":"internal/edit/stage.go::Stage.Apply#method","kind":"method","name":"(*Stage).Apply","signature":"func() error","line":84,"end_line":120}, …]}],
  "count":21,"total":21,"truncated":false}}

$ lightspeed source 'internal/edit/stage.go::Stage.Apply#method'
{"version":1,"ok":true,"data":{"symbols":[{"id":"internal/edit/stage.go::Stage.Apply#method","file":"internal/edit/stage.go",
  "kind":"method","line":82,"end_line":120,"source":"// Apply …\nfunc (s *Stage) Apply() error {\n…}","returned_lines":39,"returned_bytes":1034,
  "hash":"sha256:9f2c…","truncated":false,"total_lines":39}],"count":1}}
```

**Ids.** `<workspace-relative path>::<Container.Name>#<kind>`, with `~2`, `~3` for
the later ones of two symbols that would share one (two `init`s in a Go file). A
Go receiver is spelled as its type, so `(*Stage).Apply` is `Stage.Apply`. They are
built from what the language server says about the file
(`textDocument/documentSymbol`, hierarchical or flat) and recomputed each time
they are used — nothing is stored — so an id resolves against the file as it is
now, or it does not. `symbols`, `workspace_symbol`, `call_hierarchy` and `outline`
put the id in every JSON result; a file outside the workspace has none. Every
location command takes it back as `--id`.

**A stale id is an error, never a guess.** If the file or the symbol is gone the
result is `stale_id` (exit 1) with the nearest candidates — the same name of
another kind, the same last segment, near misses; for a moved file, the files of
that name elsewhere — in the message and in `error.data.candidates`. A malformed
id is exit 2; one that is absolute, climbs out with `..` or leaves through a
symlink is `outside_workspace` (exit 2) and nothing is read.

**`outline <file...>`** several files in one call (one language-server session
per server and workspace). `signature` is the server's detail when it gives one and
the declaration as written otherwise; `line`/`end_line` are 1-based and inclusive.
`--limit N` caps the symbols in document order and reports the cut. A file that
cannot be outlined for its own reason — missing, a directory, no server for it — is
that file's entry with an `error` and no symbols (exit 1), and the others are
outlined; only when nothing could be is the whole call the error (D31).

**`source <id|loc...>`** the text of each symbol — an id, or a location, which
names the innermost symbol containing it; `--id` (MCP `id`) is one more way of
giving an id, as on every location command. It is whole lines from the symbol's full
range, doc comment (and decorators, and a `/* ... */` block however its lines
start) included, cut from the file by *bytes*, so CJK and emoji are exact; a CRLF
file keeps its CRLFs. `--context N` adds lines around, `--max-lines` and
`--max-bytes` cap (a byte cap never splits a character), and `truncated:true` says
when one bit. `line`/`end_line` always describe the whole symbol; `returned_lines`
and `returned_bytes` say how much of it `source` holds (`source_line` where it
starts, when `--context` put lines before it), `total_lines` how much there was.
`hash` is the sha256 of the
symbol's own lines, the same however it was asked for, so a caller can tell that a
symbol changed. If some targets are stale the others are returned and the stale
ones are in `errors` (exit 1); if none resolves it is an error envelope.

**`context <id|loc>`** (or `--id ID`, or `--symbol Pkg.Name`; exactly one way of
naming the symbol) the symbol's source, `header` (everything above the file's
first declaration: the package clause and imports; `--max-header-lines`, default
80), `hover` (the server's text; a warning if it does not offer hover) and
`signature`, in one call. A hover that is nothing but the declaration's own
signature — a fenced code block with no more than that, what an undocumented
symbol gets from most servers — is left out, with a warning, because
`signature` and `source`'s own first line already carry it; a hover that says
more (a doc paragraph, a method list, a link) is kept whole (D47).

**`tree [dir]`** the files under a directory, paths relative to the workspace,
each with its `language` and the `server` that would handle it. In a git
repository it is `git ls-files --cached --others --exclude-standard`, so
`.gitignore` applies; elsewhere the directory is walked (hidden and build
directories skipped) and a warning says `.gitignore` was not applied. `--prefix`
filters, `--max-files` (default 500) — or `--limit`, which wins when both are given
and where `0` lifts the cap — caps with `truncated:true` and `total`.

**`repo_outline [dir]`** directories down to `--depth` levels (default 2) with
recursive file counts and languages, the languages with their server, and the
servers with `installed` and, when not, the install command. `--max-dirs` (or
`--limit`, which wins) caps. When the index is warm each directory also carries its symbol
count — in text a `N sym` column and the workspace's total in the header; cold, nothing.

**`file <file> [--start N] [--end M]`** lines N to M, inclusive, with `hash`,
`total_lines`, and the same `--max-lines`/`--max-bytes` caps. It refuses — exit 2,
`outside_workspace`, whether or not the path exists — a path outside the
workspace by `..`, absolute path or symlink, and refuses a binary file.

`tree` and `repo_outline` are confined too: a directory outside the workspace —
by `..`, an absolute path or a symlink — is refused with `outside_workspace`
(exit 2), whether or not it exists. So are `find_importers`, `imports` and
`dependency_graph`: a start outside the workspace is `outside_workspace` whether
or not anything is there.

`tree`, `repo_outline` and `file` start no language server and no daemon.
`--path` (default `.`) names the workspace the ids and the confinement are
relative to (for `tree` and `repo_outline`, also the directory listed when none
is given); the MCP tools take it from `workspace`. The design decisions are
D21–D23 in `docs/DECISIONS.md`.

### `search_text`, for what a symbol search cannot find

```
lightspeed search_text 'retry budget'                  # case-insensitive substring
lightspeed search_text 'func\s+\w+Command' --regex --glob '*.go' --glob '!*_test.go'
lightspeed search_text token --word --case-sensitive --path internal/auth --context 2
lightspeed search_text 'ToUpper' --with-symbol          # each hit + the id of the symbol it is in
```

Searches the live working tree — nothing is indexed, so nothing is stale — and
needs no language server. The files are `tree`'s: git's tracked plus untracked-and-
not-ignored files (a walk with a warning outside a repository). Binary files (a NUL
in the first 8 KiB), files over `--max-file-bytes` (default 1 MiB) and symlinks that
lead out of the workspace are skipped, and each kind is named in `warnings`.
With `--format text` the same warnings follow the matches as `# ` lines, so a skip is
never silent in either format.

- `--regex` is Go RE2: linear time, no backtracking, and an invalid pattern is a
  usage error (exit 2). `--case-sensitive` and `--word` (whole words; letters and
  digits of any script and `_` count as word characters) work with both modes.
- `--glob` is repeatable: a pattern includes, a leading `!` excludes. One with no
  `/` matches at any depth (`*.go`), and a directory name matches everything below
  it. `--path` scopes to a directory or one file inside the workspace; `..`,
  absolute paths and symlinks that leave it are refused with `outside_workspace`,
  exactly as for `tree` (`--root`, default `.`, is the workspace, as `--path` is
  for `tree`; the MCP tool takes it from `workspace`).
- One result per matching line: `file:line:col` (workspace-relative, 1-based **byte**
  column of the first match) and the line; `hits` when the line has more.
  `--context N` adds the lines around each; a line over 240 bytes is cut to a window
  around the match with `[+NB]…` / `…[+NB]` markers and `clipped:true`.
- `--limit N` (default 50; `--limit 0` for all) stops the listing, sets
  `truncated:true` and still reports `total`, the number of matching lines.
  Results are in path, line order, however many files were searched in parallel. No
  match is an answer: `ok:true`, empty, exit 1, as grep's.
- `--with-symbol` adds `symbol`, the id of the innermost symbol containing the
  match, from the outline of that file. Best effort: a file no server handles, or a
  server that cannot answer, leaves the field out and says so once in `warnings`.

The design decisions are D26 in `docs/DECISIONS.md`.

## The workspace index: whole-repo queries

`search_symbols`, `repo_map` and the import-graph commands answer questions about the
whole workspace without reading it file by file. They stand on a persistent index that
the daemon owns (or, with `--no-daemon`, the command builds in its own process): for every
file a language server handles, its `documentSymbol` outline — id, kind, container, lines,
signature, first sentence of its doc comment — and for every file in a language with an
import extractor, what it imports. It lives under
`$XDG_CACHE_HOME/lightspeed/<workspace-hash>/`.

```
lightspeed index status               # counts, coverage per server and language, skipped files and why, staleness, cache path and size
lightspeed index build                # explicit warm-up: revalidate everything, ask the servers about what changed
lightspeed index clear                # forget it (a cache: the next query rebuilds what it needs)

lightspeed search_symbols 'read config' --kind function,method --lang go --glob 'internal/**' --limit 10
lightspeed search_symbols ParseConfig --detail full          # + the source, as `source` would print it
lightspeed search_symbols Alphx --fuzzy                      # only if nothing matched; flagged fuzzy
lightspeed repo_map --budget 1500 --path internal/store
lightspeed find_importers internal/store/user.go --limit 20  # a file, a directory (Go package) or an import path; test-only importers are counted and named
lightspeed find_importers internal/store --include-tests     # ... and listed with the rest, marked [test]
lightspeed imports internal/store/user.go --limit 30
lightspeed dependency_graph internal/store --depth 3 --direction in --limit 50 --format text
lightspeed dependency_cycles --limit 5
```

**It is never stale.** Every query revalidates the files it is about to report on against
the disk — a stat, then a hash where the stat moved — and asks the language server again
only about the files that changed; files that appeared or went are picked up in the same
scan (the same gitignore-aware enumeration as `tree`). A query that had to build says so:
`warnings` names how many files and how long it took, and `data.index` has the numbers. A
query restricted with `--path`, `--glob` or `--lang` revalidates and builds only what it can
reach. Before answering *any* command the daemon also tells its running language servers
which files changed on disk since it last looked (`workspace/didChangeWatchedFiles`), so a
file edited in an editor or by `git checkout` cannot make a warm server answer from an old
copy (D33). The cache is versioned (schema, lightspeed build, server executable, server
version); a part that does not match, or is truncated or damaged, is discarded and
reported, never trusted; writes are atomic and safe under two processes.

Every index-backed command's revalidation report defaults to a one-line
`{"summary":…,"stale":…}` — `data.index` for `search_symbols` and `task_context`, `data.report`
for `repo_map`, `find_importers`, `imports`, `dependency_graph` and `dependency_cycles` —
and `--report` (MCP `report`) restores the full report on any of them: the numbers above, plus
`scan_ns`/`elapsed_ns`/`listed` (D46). `index status` is the one exception: the full report is
that command's entire job, so it is never compacted. `warnings` always names a query that had
to build, and how long it took, whether or not `--report` was given.

**Readiness still means exit 5.** While a server is still indexing the index reports
`not_ready` (exit 5) and records nothing that depended on it, rather than storing the
partial outlines it would answer with. Import queries need no server. A server that is not
installed (or will not start) does not stop a build: its files are indexed for their imports
only, and `index status` says so.

**Search ranking.** Exact name, then name prefix, then BM25 over name (×3), container, signature and
the doc sentence, with `camelCase`/`snake_case` split; ties broken by depth, path and line,
so the order is deterministic. **Every row says how well it matched**: `match`
(`exact`, `prefix`, `token`, `fuzzy`) and `score` at every `--detail`, and in text a
row that is not an exact match ends in `[~prefix]`, `[~token]` or `[~fuzzy]`. When the
query looks like an identifier (`ParseConfig`, `pkg.Type.Method`, `parse_config`) and no
row is an exact or prefix match, the first warning — `data.notice`, and the first line
of text — says `no symbol named X (exact or prefix); these are token matches`: the rows
are then symbols sharing a word, not the symbol. Nothing matching is an answer, not a
guess: `ok:true`, exit 1, the same verdict first, then the nearest names that are
actually near. `--limit` (default 20) reports `truncated` and the total. `--detail
compact|standard|full` trades tokens for detail. `repo_map` ranks files by PageRank over
the import graph and stops at the token budget (and at `--limit` files, if given), saying
what it left out.

**`find_importers` and tests.** A Go `_test.go` file's imports are not edges of the graph
(an external-test package would close cycles that are not in the build), so a package
imported only by tests would look unimported. The answer says so instead:
`test_only_importers` (`count`, the files up to `--limit`, and a warning that names them)
lists the Go test files whose *package* imports the target only from tests; they stay
out of `importers` unless `--include-tests` merges them in, marked `test:true`. A test
file in a package that also imports the target from non-test code, and a package's own
tests, are not listed (D42). `find_importers` lists at most 50 importers by default
(`--limit 0` for all).

**Imports** are read from the file text — Go (`go/parser`), Python, TypeScript/JavaScript,
Rust, C/C++ and Lua, by scanners that ignore comments and strings — and resolved to workspace files
or marked external (`stdlib`, `third-party`, `system`); an import that resolves to the
importing file itself (`use self::Item`) is `self`, not an edge. A Go node is a package; other
languages' nodes are files. A language with no extractor is **`not_covered`** (exit 3),
never "no imports"; `index status` and the graph commands name the languages present
and unread. Design decisions: D32–D36 in `docs/DECISIONS.md`.

**This against a tree-sitter index** (D37, in full). What it buys: symbols are the
language server's own (semantic, not syntactic), and nothing is older than the disk — a
prebuilt index is right when it was built. What it costs: a language server per language
that has to be installed, started and *ready* (exit 5, and rust-analyzer needs 30–90 s)
where tree-sitter parses on the spot; a scan per query where a snapshot is a lookup;
and coverage limited to installed servers — a language without one is not indexed, and
`index status` says so — where tree-sitter covers a hundred languages with no
installation. Measured on this repository with real gopls (268 files, 3 590 symbols): cold
`index build` 3.2 s, warm `search_symbols` about 10 ms, revalidation of the unchanged tree
about 10 ms. Not measured: larger repositories, or any server but gopls.

## Composed analysis tools

The commands above are single questions. These combine them — references, call and type
hierarchy, the index, the import graph, `search_text`, git — so that a coding agent asks once. Every
row names its evidence, every bound is reported when it bites (`truncated:true`, `total`), every
verdict states the rule behind it, and none writes. They take `--id`, a location or `--symbol`, and are
MCP tools of the same names. Decisions: D38–D41.

Four read-only commands that answer a question with a verdict *and the evidence behind it*. Every row
says which query it came from (`evidence`), every verdict carries its `rule` and the counts, and every
list honours `--limit` with `truncated:true` and the total (a top-level `truncated`/`total` summarize
several lists). They take a location, `--symbol` or `--id`, and are MCP tools of the same names.
(docs/DECISIONS.md D38.)

### `type_hierarchy <loc>`

```
lightspeed type_hierarchy --id 'internal/index/manager.go::Backend#interface' [--direction supertypes|subtypes|both] [--depth 1-5] [--limit N]
```

Supertypes and subtypes from `prepareTypeHierarchy`, to `--depth` (default 1) under a node budget and
with cycle detection; rows are `{direction, depth, id, name, kind, file, line, evidence}`. A server
without `typeHierarchyProvider` still answers the *subtypes* through `implementation` (one level, rows
`evidence:"implementation"`, a warning says so and that supertypes are unavailable); `--direction
supertypes` there is exit 3.

### `check_references <name|loc>`

One call for "is this used anywhere?": the argument may be a bare name or dotted path, a location, an
id (`--id`) or `--symbol`. If it resolves to one symbol, `semantic` lists the references (declaration
excluded, each with the id of its enclosing symbol), and `text_only` lists the whole-word occurrences
of the identifier on lines the references do not account for — strings, comments, configs, other
languages, dynamic uses — kept apart. An ambiguous or unknown name is not an error: `resolution` says
`ambiguous` (with candidates), `not_found` or `unavailable`. `verdict` is `used`,
`used_only_in_text`, `unused` (exit 1: nothing found) or `ambiguous`: a bare name that matches several symbols
is never called unused or text-only, because no one symbol was queried; each of the first five candidates gets
its own semantic reference count (`candidates[].references`) so that the answer is not a second call away. `--glob` scopes the text scan.

### `rename_check <loc> <newname>`

A verdict on a rename that writes nothing: whether `prepareRename` accepts the position; the size of the
server's edit set (files, edits, per file) staged but never applied; collisions with existing symbols of
that name in the same scope, found through the index (same container of the same file, and, for Go,
Java and Kotlin, of the same directory); and the whole-word mentions of the old name a rename leaves
alone. `verdict`: `ok`, `collision`, `refused` (the server's reason is quoted), `noop`, `unverified`
(the server cannot rename). Exit 0 only for `ok`. Apply with `rename`.

### `delete_check <loc>`

A verdict on deleting a symbol: the references outside its own declaration (recursion is not a use),
split into tests and other code; whether it is exported and *how that was decided* for the language
(`exported_basis`; `unknown` where the language gives no rule); and, when it is the last symbol of its
file or package, the files that import that file or package. `verdict`: `in_use`,
`used_by_tests_only`, `leaves_importers_dangling`, `unused`. **Exit 0 means the evidence does not stand
against deleting it, exit 1 means it does** — the reverse of `check_references`. An exported symbol
carries a caveat that users outside the workspace are invisible to its references.

### `blast_radius`: what does changing this touch?

```
lightspeed blast_radius --id 'internal/cli/compose.go::isTestPath#function'
lightspeed blast_radius internal/index/symbols.go        # a file: who imports it
lightspeed blast_radius --symbol Server.Handle --depth 3 --limit 30
```

One call instead of `references` + `call_hierarchy` + `find_importers` + a hand count. The
**summary comes first** — reference locations and the symbols that contain them, callers (and
how many are beyond the direct ones), importing files, distinct files and packages, and how many
of them are tests — and is always complete. The rows follow, each saying its evidence:

| `evidence` | comes from | a row is |
|---|---|---|
| `references` | `textDocument/references` | a symbol that refers to it (file, enclosing symbol id, how many references) |
| `call_hierarchy` | incoming calls, the walker of `call_hierarchy` | a caller at `depth` 1, 2, … |
| `imports` | the import graph | a file that imports the subject's file (depth 1), or a package/file beyond it |

`--limit` (default 50) cuts the rows only, shares the room between the three sources, and says
`truncated:true` with the total. **Bounds are reported when they bite:** `--depth` (callers,
default 2, max 5) and `--import-depth` (default the same); `bounds.calls_at_limit` and a warning
count the rows at the last level walked, whose own callers were not followed; the call walk's
500-node budget and cycle handling are `call_hierarchy`'s. A source that could not answer is under
`evidence` with a status (`unavailable`, `not_applicable`, `not_covered`, `not_analysed`), never
left out: a server without call hierarchy, a language with no import extractor, and a **file**
subject (importers only; name a symbol for its references and callers) all say so. `test:true`
marks test files (a filename/directory heuristic). Go test files are not in the import graph, so a
Go package's test callers show as references. Exit 1: nothing is affected. A server that is still
indexing is exit 5.

### `dead_code`: what nothing uses

```
lightspeed dead_code internal/render
lightspeed dead_code --kind function,method --budget 200 --limit 20
lightspeed dead_code --cursor '488|internal/cli/daemoncmd.go::lastLines#function'
```

Symbols with **zero references outside their own declaration** (a recursive call is not a use),
from the index's symbol list and one `references` query each. It is *bounded and resumable*
because that is N queries: `--budget` (default 100) caps the reference queries per call, and when
it runs out `complete:false` and `next_cursor` continue after the last symbol examined. The
answer reports how much it did (`symbols` in scope, `eligible` after the exclusions, `examined`,
`used`, `queries`) and why it skipped the rest (`excluded`: `test_file`, `entry_point`, `ignored`,
`local`, `exported`, `implements`, `unresolved`).

Never examined: tests and test files, entry points (`main`, `init`, Go `Test*/Benchmark*/Example*/
Fuzz*`, Python dunders), exported API unless `--include-exported` (how export-ness is decided is
per language: Go capital letter, Rust `pub`, JS/TS `export`, C `static`, Lua `local`, Python
underscore; anything the language does not settle is `unknown` and stays a candidate at low
confidence), methods that implement an interface (`textDocument/implementation`), a built-in list
of protocol method names (`String`, `Error`, `MarshalJSON`, `ServeHTTP`, …) and what the workspace
ignores:

```toml
# .lightspeed.toml
schema_version = 1
[dead_code]
ignore_names = ["Handle", "Server.Serve", "Test*"]
ignore_paths = ["**/*.pb.go", "gen/**"]
```

The results are **candidates**, each with a `confidence` (`high`, `medium`, `low`) and the
`reasons` that lowered it — a method may satisfy an interface outside the workspace, a Python or JS
name may be built from a string, a Go file with a build constraint or cgo may have its uses in a
file that was not built, an exported symbol may have callers outside this repository. Every answer
says that reflection, build tags, cgo and generated code can hide a use. Exit 1: candidates found.
A server that is still indexing is exit 5, not an empty list.

### Git-aware analysis: `changed_symbols`, `churn`, `hotspots`, `related`

These four answer git-shaped questions about *symbols*, not files. They shell out to `git`
(read-only, bounded; docs/DECISIONS.md D40) and, outside a repository, answer with a warning and
what they can rather than fail (`data.git.repo:false`, exit 0).

```
lightspeed changed_symbols [--base REV] [--staged] [--path P]   what a diff added, modified or removed
lightspeed churn [path] [--since 30d]                            commits/authors/lines per file and per symbol
lightspeed hotspots [--since D] [--path P] [--all]               churn × size, ranked (code only unless --all)
lightspeed related <loc | --id ID | --symbol S>                  siblings, callers/callees, co-changed files, similar names
```

**`changed_symbols`** maps `git diff -U0` hunks onto symbol ranges: working tree against `HEAD`
by default, the index with `--staged`, against another revision with `--base`. The new side's
ranges come from the workspace index (revalidated, so hunks and ranges agree); the old side is an
outline of the old blob, so a *removed* symbol has an id, its old file and lines. Rows are
`{status: added|modified|removed, kind: symbol|file, id, file, line, end_line, hunks, evidence}`;
a change outside every declaration (imports, comments), a binary file, a rename with no edits and
a file of a language with no outline are `kind:"file"` rows. When the old version cannot be outlined
(no server, over the cap of 40 files) the rows say `evidence:"index+hunks"` and a warning says
removals there are not listed. `summary` counts what was found; `--limit` (default 100) reports
`truncated:true` and the total.

**`churn`** reads `git log --numstat` over `--since` (default `90 days`; `30d`/`2w`/`12h`/`6m`/`1y`, or anything git reads:
`30 days`, `2 weeks ago`, `2026-01-31`): per file, commits, distinct authors, lines added/removed
and the newest date; per symbol, for the `--symbol-files` (default 15) most-changed files, the same
attributed to the *current* symbol ranges by carrying each historical hunk forward through later
commits — an **approximation**, which the answer states (`approximate`). **`hotspots`** ranks the
same data by `score = commits × (1 + ln(1 + loc))` (the formula is in the answer): what changes
often *and* is big. It ranks **code**: docs (markdown), data and config (json, yaml, toml, lock
files) are left out, counted in `non_code_left_out` and named in a warning, because a README or a
decision log outranks every source file otherwise; `--all` ranks them too. Lists are bounded by
`--limit` (default 20) with `truncated:true`.

**`related`** is a small ranked list, each row with its `evidence`: `sibling` (same file, nearest
first), `call_hierarchy` (direct callers and callees; a server without one is named, not silently
empty), `co_change` (files that change in the same commits, over the last `--history` commits;
bulk commits are ignored and counted) and `similar_name` (from the index). A symbol several kinds of
evidence agree on is one row with all of them; `sources` says what each kind found and why one was
silent. `--per-source` (default 5) and `--limit` (default 20) bound it.

### `task_context`, a task in words

```
$ lightspeed task_context "make find_importers respect --limit" [--budget 4000] [--limit 8] [--with-source 0] [--expand 3] [--max-lines 60] [--rule] [--report] [--format text]
```

A task in words, one capsule out, and no model: it extracts the code-shaped terms of the task
(camelCase, snake_case, `pkg.Type.Method`, `` `quoted` ``, `--flags`, file paths), ranks the
workspace's symbols against them with the index (exact > prefix > token), expands the best few
with their callers and callees (call hierarchy), the files they import and that import them
(import graph) and their same-file neighbours, and returns one capsule inside `--budget`
tokens (about 4 bytes each): up to 8 ranked symbols with signature and score, up to 5 related
rows (each naming which ranked symbol it neighbours, by position, and how), and the files
involved.

**The JSON answer is token-cheap by default** (`docs/DECISIONS.md` D46): a ranked symbol's
`id` already spells out its name, kind and file (`path::Container.Name#kind`), so the default
answer leaves those — and `end_line`, `doc` and the match evidence — out of each row; source is
not inlined (`--with-source 0`); a related row carries only its identity (`of_index`, `relation`,
`id`, `name`); neither the extracted `terms` nor a composed `verdict` sentence (`confidence`
+ `reason` already say it in one line) are printed; and `confidence_rule` is a bare rule name
(`"term-coverage-v1"`) rather than its text — the `reason` already carries the per-answer
evidence. `--rule` (MCP `rule`) restores all of it in one flag — full symbol fields, every
related row with full detail, the terms, the verdict, and the full ~300-token confidence rule
text in place of the bare name. `--with-source N` inlines the best N symbols' source regardless.
Text output (`--format text`) is never trimmed this way; it always shows the full answer,
including the one-line rule form (not the bare JSON name).

`confidence` is `high`, `medium` or `low`, printed with `confidence_rule` (a rule name in JSON,
a one-line form in text; `--rule` prints the full rule either way) and the `reason` that
decided it: *high* is one exact, unrivalled name match on an identifier-shaped term, or, in a
task of plain words, one symbol that matches every term (one of them exactly) and clearly leads;
*medium* is anything less decisive (a prefix match, a single exact plain word, one symbol covering
half or more of the task, a **compound hit** — one symbol matching two or more of the task's words,
one of them in its own name, as `Pool.ReapIdle` does for "reap idle language servers" — or a named
file), and its reason names the words that matched nothing; *low* is a task with neither: no symbol
covers half of it and none is a compound hit (one generic word matching next to a term that matches
nothing — "oauth token refresh" — is low), a single loose match, or nothing. A word counts 1 as a
symbol's exact name, 0.6 as a prefix or as one of the words of its name, 0.3 in its signature, doc
comment or file path; symbols of test files count half unless the task mentions tests.
**Low means "probably not implemented here"**:
`symbols` is empty, the weak matches are only counted (`withheld`) and at most five `nearest`
names are offered as a hint, and the exit code is 1. Use it as the first call of a task; use
`search_symbols`, `search_text` and `repo_map` when it says low and you still want to look.
It finds what the task *names*: words that are not in the code's own vocabulary find nothing.
**`--budget` is honoured.** `budget.used` is the size of the whole capsule; when the header alone does not
leave room for the best symbol, the index report and then the term list are dropped (`budget.dropped`
says so). A budget below the smallest answer (verdict, reason and best symbol) is reported, not
claimed: `budget.over: true` and a warning. This is separate from the default compaction above:
a related row past the fifth is left out of the default answer and reported in a warning, not in
`budget.dropped`, because it costs nothing against `--budget` — it is one flag (`--rule`) away, not
one budget dollar away. Decisions: D41, D44, D46.

`index`, the revalidation report `task_context` (and `search_symbols`) read before ranking, is a
one-line `{"summary":…,"stale":…}` by default; `--report` (MCP `report`) restores the full report
(`files`/`fresh`/`touched`/`changed`/`added`/`removed`/`skipped`/`uncovered`/`scan_ns`/`elapsed_ns`/`listed`).
`index status` is unaffected — the full report is that command's entire job.

A single capitalised word is a plain word; put a name in backticks (`` `Render` ``) to make it an
identifier. On this repository it takes 0.7–4.6 s in process and about 1 s through a warm
daemon (the call-hierarchy and import queries of the expanded symbols); `--expand 0` skips them.

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

**Paths are relative to the workspace root**, in text and JSON alike, for every
command that prints them, and a file outside the workspace keeps its absolute
path. JSON says what they are relative to in `data.root`; text says so with a
trailing `# paths are relative to <root>` line only when you did not run the
command in the root (from the root they are valid as they stand, e.g. for
`cd <root> && git apply`). `--absolute` (MCP `absolute`) prints absolute paths
everywhere and leaves `root` out (D30).

**A location result is compact by default.** `definition`, `references`,
`implementation`, `workspace_symbol`, `symbols` and `call_hierarchy` answer with
rows of `path`, 1-based `start`/`end` (`{"line":…,"column":…}`) and `text` — the
coordinates a caller pastes back into lightspeed. The LSP `uri`, the UTF-16
`range` and the byte `offset` on `start`/`end` are computed anyway but left out
of the JSON unless asked for: `--verbose-locations` (MCP `verbose_locations`)
restores all three on every row. Nothing is removed from the tool, only from the
default answer (D45):

```json
$ lightspeed references internal/store/user.go:42:8
{"…","results":[{"path":"internal/store/user.go","start":{"line":42,"column":8},"end":{"line":42,"column":16},"text":"func (r *UserRepo) Find(id int) *User {"}]}

$ lightspeed references internal/store/user.go:42:8 --verbose-locations
{"…","results":[{"uri":"file:///…/user.go","path":"internal/store/user.go",
 "range":{"start":{"line":41,"character":7},"end":{"line":41,"character":15}},
 "start":{"line":42,"column":8,"offset":913},"end":{"line":42,"column":16,"offset":921},
 "text":"func (r *UserRepo) Find(id int) *User {"}]}
```

**Token discipline.** The matched line only, by default. `--context N` adds
surrounding lines; `--limit N` caps the result count and always reports
`"truncated":true`, the `total` it cut from, and a warning that says how to widen
it — a silent cutoff would be indistinguishable from a complete answer. **Every
command that returns a list honours `--limit`** (`0` for all), and one test walks
the command table to keep it so; the ones with no default bound of their own get
one that protects a token budget, and it is reported when it bites:
`find_importers` 50, `imports` 100, `dependency_graph` 200 edges,
`dependency_cycles` 20 (`search_symbols` 20 and `search_text` 50 as before; `tree`
and `repo_outline` keep `--max-files`/`--max-dirs`, and `--limit`, when given,
wins). What is not bounded, and why: `rename` and `format` (a preview must show
every edit `--apply` would write), `hover`/`file`/`source`/`context` (one bounded
slice), `index`, `servers`, `doctor`, `daemon` (one report, or bounded by the
configuration). `--limit` never changes an exit code: `check --limit 1` on a tree
with errors still exits 1, and `dependency_cycles --limit 1` with five cycles
still exits 1 (D42).

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

## MCP server (for coding agents)

`lightspeed mcp` serves the command table as an [MCP](https://modelcontextprotocol.io)
server over stdio, so a coding agent gets warm language servers as tools instead
of shelling out. With Claude Code:

```sh
claude mcp add lightspeed -- lightspeed mcp
```

or the same thing as a `.mcp.json` entry:

```json
{"mcpServers": {"lightspeed": {"command": "lightspeed", "args": ["mcp"]}}}
```

Add `--no-daemon` (`lightspeed mcp --no-daemon`) to keep each call's language
server in process, or `--offline` for PLAN §6's kill switch; both are per server,
not per call.

**Tools.** One per command, named as the subcommand: `definition`, `references`,
`implementation`, `hover`, `symbols`, `workspace_symbol`, `rename`, `codeaction`,
`format`, `check`, `call_hierarchy`, `outline`, `source`, `context`, `tree`,
`repo_outline`, `file` and `search_text` (see [Reading code by symbol](#reading-code-by-symbol)),
`search_symbols`, `repo_map`, `find_importers`, `imports`, `dependency_graph`,
`dependency_cycles`, `index_status`, `index_build` and `index_clear` (see
[The workspace index](#the-workspace-index-whole-repo-queries)), plus `servers`, `doctor`
and `daemon_status`, and the composed tools `type_hierarchy`, `check_references`, `blast_radius`,
`rename_check`, `delete_check`, `dead_code`, `changed_symbols`, `churn`, `hotspots`, `related`,
`task_context`, `guide` and `version` (see [Composed analysis tools](#composed-analysis-tools)).
The server's `instructions` are `guide --compact`. (`index_build` and `index_clear` write only the cache, so they carry
the read-only hint.)
Not tools: `batch` (a client already issues calls concurrently), `raw` (it skips
the guards that make an answer trustworthy), `help` (`tools/list` is the surface),
`install` (nothing downloads as a side effect of an agent's call), `mcp`, and
`daemon stop|logs`. Tools that cannot write carry the `readOnlyHint` annotation.

**Parameters** are the command's flags and arguments with typed, described JSON
Schema properties — `tools/list` is the reference. A position is either
`location` (`file:line:col`, the syntax of [Location syntax](#location-syntax))
`symbol` (a dotted name, plus an optional `path` to search) or `id` (a symbol id),
exactly as on the command line. Common to the query tools: `limit`, `context`, `timeout` (a Go
duration such as `"30s"`) and `server`. Every tool takes `workspace`, the
directory relative paths resolve against; it defaults to the server's working
directory. Flags spelled with a dash on the command line use an underscore
(`--allow-dirty` is `allow_dirty`); `--format` and `--indent` do not exist,
because the answer is always the JSON envelope.

**Results.** The envelope of [Output contract](#output-contract), as text content
and as `structuredContent` — byte for byte what the command prints, since it is
the same code — with `isError` set when it says `ok:false`. An empty answer
(`ok:true`, the CLI's exit 1) and a `check` that found errors are answers, not
errors. The exit code the CLI would have had is in the result's
`_meta["lightspeed/exit"]`. A bad argument is an `ok:false` `usage` envelope, not
a protocol error. `not_ready` (exit 5) means the language server is still
indexing; the agent is told, in the server's instructions, to retry and never to
read it as "no results".

**Writing.** `rename`, `codeaction` and `format` preview unless `apply` is
`true`, and a dirty git worktree still refuses `apply` (D8) unless `allow_dirty`
is `true`. Edits are all-or-nothing, as on the command line.

```
references  {"location": "internal/store/user.go:42:8", "limit": 20}
references  {"symbol": "store.UserRepo.Find", "workspace": "/home/me/proj"}
rename      {"symbol": "store.UserRepo.Find", "new_name": "Lookup"}                 → a preview
rename      {"symbol": "store.UserRepo.Find", "new_name": "Lookup", "apply": true}  → writes
```

Calls use the workspace's daemon by default, so the second call on a workspace
reaches a warm server. **A new command is a new tool** with no MCP code: its
table entry declares its parameters (`internal/cli/params.go`), and a test fails
for a command that is neither declared a tool nor declared excluded, or whose
declared parameters disagree with the flags it actually has
(docs/DECISIONS.md D20).

A call is cancelled by `notifications/cancelled`, by the client hanging up and by
a signal — it does not run on to its `timeout`, and `lightspeed mcp` exits when its
input ends even with a call running. (Through the daemon the *call* stops at once
but the daemon finishes its own work for nobody, until that request's readiness
deadline; with `--no-daemon` it stops for real. D25.)

Limits: only tools are served (no resources or prompts); `servers` returns the
full report, which is large. It has been exercised with the go-sdk's own client
and a raw stdio session, not with Claude Code itself.

## Setting up an agent: `guide`, `version`, the Makefile

```
lightspeed guide                      # the agent policy for this build, markdown, generated from the command table
lightspeed guide --format claude-md   # the same as a ready-to-paste CLAUDE.md section, with regeneration markers
lightspeed guide --compact            # the short form the MCP server sends as its instructions (~2 kB)
lightspeed guide --format json        # {version, build, text, groups}; also the MCP tool `guide`
lightspeed version [--format json]    # version, build identity, guide version, Go, VCS revision; also `-v` / `--version`
```

The guide says which tool for which question, how to name a symbol (`--id`, `file:line:col`,
`--symbol`), that `rename`/`codeaction`/`format` preview until `--apply`, what the exit codes mean
(5 is *not* "nothing found"), and — its first rule — never to fall back to Read/Grep/Glob/Bash for
code navigation (Read only before an Edit). It cannot drift: every command line in it is read from
the command table when it is built, and tests fail for a command with MCP tools that is in no group,
a group naming a command that does not exist, and any `--flag` no command registers (D43).

Over MCP — the `guide` tool and the server's instructions — the same guide is rendered in the words
an MCP client has: tool names (`index_status`, `index_build`, `index_clear`, `daemon_status`, not
`index` and `daemon`), parameters (`id`, `apply: true`, `allow_dirty: true`, not `--id`, `--apply`),
the exit code as `_meta["lightspeed/exit"]`, and `lightspeed install <name>` named as what it is
there: a shell command for the user, not a tool.

`docs/AGENT-SETUP.md` is the table of questions an agent asks and the lightspeed command that
answers each, with what to expect from it; what is deliberately not provided (embeddings,
generated summaries, runtime traces, session state, cross-repo maps, languages without a
server); the exact Claude Code setup (`claude mcp add --scope user lightspeed -- lightspeed mcp`,
the CLAUDE.md section, installing servers, why no hooks are needed) and the rollback
(`claude mcp remove lightspeed`). A test checks the table in that document against the command
table.

`make` lists the targets: `build` (to `bin/lightspeed`, stamped with `git describe`), `test`, `vet`,
`install` (`go install ./cmd/lightspeed`), `generate` (the built-in server definitions), `check`
(build + vet + test) and `clean`.

## Server configuration

Six servers are built in — `gopls`, `rust-analyzer`, `pyright`, `vtsls`,
`clangd`, `lua-ls` — generated from the nvim-lspconfig corpus, so the common
case needs no configuration: install the server, and lightspeed finds it on
`PATH`.

A file is handled by the first of four layers that has something to say, and
within a definition each *key* comes from the strongest layer that sets it
(PLAN §6):

1. `.lightspeed.toml` in the **workspace root** — in-tree, version-controlled;
   the only file lightspeed asks anyone to write. The workspace root is the one
   that keys the daemon: the nearest ancestor with a `.git`, `.hg`, `.jj`,
   `.svn`, `go.work` or `.lightspeed.toml`.
2. `$XDG_CONFIG_HOME/lightspeed/servers.d/*.toml` (`$LIGHTSPEED_CONFIG_DIR`
   replaces the whole directory) — your own overrides.
3. The generated built-in defaults.
4. **PATH sniffing** decides where the executable is: `command[0]` if it is a
   path, else `PATH`, else `mise which` — and only when mise is already installed.
   Nothing is installed to find it. A file on `PATH` is not taken as proof that the
   server runs: a **mise shim** (a `shims` directory under mise's) is checked with
   `mise which` in the workspace, since it only works where mise has a version
   active. If it does not, and mise has the tool installed, that installed binary
   is launched and `servers` says so (`note`); if nothing is installed the server
   is *unusable*, not installed — exit 3 with `mise use -g <tool>@<version>`.
   `servers` and `doctor` also start every binary they found (`--version`, two
   seconds, offline) and report one that cannot start; a process that ran and
   merely rejected `--version` (gopls does) counts as started.

A definition is pure data. `.lightspeed.toml`:

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

A file may be a **partial override** of a built-in — `name = "gopls"` and
`[activation] priority = 90` changes the priority and keeps everything else —
or a whole new server, which then claims files nothing else does (`globs =
["**/*.wat"]`). A file that cannot be parsed is exit 2 (`invalid_config`), never
skipped: an override that silently does nothing is the failure this design is
meant to prevent. `--server NAME` picks between servers that both claim a file.

### `lightspeed servers`

```
$ lightspeed servers --format text
workspace: /home/me/proj
mise 2026.9.7 linux-x64 at /usr/bin/mise; 2 of 6 servers installed
  layer workspace .lightspeed.toml
  layer user      not present
  layer builtin   present, no files

gopls           missing   mise use -g go:golang.org/x/tools/gopls@v0.23.0
    defined by workspace /home/me/proj/.lightspeed.toml
    activation.languages           built-in default
    ...
```

Every server, the layer and file that set each key (`key_origins` in the JSON),
what each layer overrode (`overrides`, `shadowed`), whether the executable is
there and how it was found (`path`, `mise`, a `command_path`), and the install
command; a binary that is there and cannot run is `unusable` with the reason and
the fix, and one launched instead of a broken shim carries the `note`.
`--path DIR` says which workspace's `.lightspeed.toml` applies. Exit 1
if a configuration file could not be used — it is listed under `problems`, and
the other servers are still reported.

### `lightspeed doctor [path...]`

Is there a server for these files, is each server's binary present and
runnable, is mise, do the layers override each other, and is offline mode on —
one finding per line, graded ok / info / warn / error, each with the command
that fixes it. Exit 1 if any finding is an error (an unusable configuration
file, a path no server claims, a binary that exists but cannot run — a mise shim
with no active version, a missing library); a merely missing server is a warning,
and so is one that runs from a mise version that is installed but not active.

### `lightspeed install <name>`

```
$ lightspeed install gopls
would run: mise use -g go:golang.org/x/tools/gopls@v0.23.0
then find it with: mise which gopls
nothing was installed; repeat with --run to do it
$ lightspeed install gopls --run          # mise does the download
```

Nothing downloads implicitly, so the default is the plan; `--run` is the
explicit request. `--version V` replaces the version in the definition's mise
spec, `--install-timeout` (default 15 minutes) bounds mise. It fails, with
exit codes, when the server is unknown (2, `no_such_server`), has no
`install.mise` to delegate (3), mise is absent (3, `mise_unavailable`), offline
mode is on (3, `offline`), or mise ran and failed (4, `install_failed`). There is
no fallback downloader.

### `--offline`

`--offline` (before or after the subcommand, or `LIGHTSPEED_OFFLINE=1`) is the
kill switch of PLAN §6: it refuses `install --run`, and a missing-server
envelope under it says so. The environment variable wins over the flag's absence,
never the other way round. It is deliberately narrow: nothing a query does
touches the network from lightspeed's side, so queries work offline, and it does
not restrain the language servers themselves.

A server that is missing when a query needs it is exit 3 with the exact command
in the envelope, before any daemon is contacted:

```
{"version":1,"ok":false,"error":{"code":"server_not_installed",
 "message":"gopls handles this file but \"gopls\" is not on PATH; install it with: mise use -g go:golang.org/x/tools/gopls@v0.23.0",
 "data":{"server":"gopls","command":"gopls","install":"mise use -g go:golang.org/x/tools/gopls@v0.23.0"}}}
```

There is no environment variable that overrides the server command. (The M0
`LIGHTSPEED_SERVER_CMD` was removed, docs/DECISIONS.md D18; write a `servers.d`
file instead.)

## Security posture

**Do not point lightspeed at code you do not trust.**

A language server runs the repository's own build tooling by design: gopls runs
`go list`, rust-analyzer runs `cargo metadata`, and both execute code and
configuration from the checkout. Running lightspeed on a hostile repository is
equivalent to running that repository's build. There is no sandbox in this
version (PLAN §6; `--sandbox` with bubblewrap/landlock is deferred).

What lightspeed does guarantee:

- **Nothing downloads implicitly.** A missing server exits 3 with the command
  that would install it. lightspeed runs mise's `use` only for
  `lightspeed install <name> --run`, never as a side effect of a query, and
  `--offline` / `LIGHTSPEED_OFFLINE=1` refuses even that. Its probing calls
  (`mise --version`, `mise which`) are told not to install.
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

Server processes inherit lightspeed's environment and privileges — with the
daemon, the environment and privileges of the command that started it. The
daemon's socket lives in a directory only its owner can enter, and a socket
owned by another user is refused. Their stderr goes to lightspeed's stderr (to
the daemon's log, for a daemon); machine output is stdout only, so stdout can be
parsed without filtering.

## Not implemented

Honest list, so that nothing above has to be read twice:

- **The composed tools and the agent setup have not met a real agent.** No test starts a real MCP client
  (Claude Code) or any language server but gopls (by hand, and one test that skips without it); the
  composed commands (`blast_radius`, `dead_code`, `task_context`, …) are proven against the scripted
  server. Their heuristics — the export rules outside Go, `dead_code`'s confidence levels,
  `task_context`'s weights and confidence thresholds — are judgement, not measurement, and each
  says so in its output. Test-only importers are reported for Go only. Token cost is measured
  on this repository only (`make bench`). What is deliberately not provided — embeddings,
  generated summaries, runtime traces, session state, cross-repo maps — is listed, with reasons, in
  `docs/AGENT-SETUP.md`.

- **The daemon's latency claim is unmeasured.** PLAN §8 M3 says the second
  `references` on a rust-analyzer workspace returns in under 200ms. What is tested
  is the mechanism — two commands start one server process, hermetically — and not
  the figure, which needs a real rust-analyzer. It also cannot hold for a server
  that never reports `$/progress` (pyright, for one): readiness rule 3 makes every
  answer from such a server wait out `--settle` (750ms) whether the server is warm
  or not, so a warm query costs a little under a second there. A warm session
  helps most exactly where the cost is indexing.
- **The daemon log is never rotated.** (A warm server's view of files changed
  behind lightspeed's back used to be only as good as its own file watching; the daemon now
  tells every live server what changed, D33 — checked against gopls only.)
- **The workspace index has been measured on this repository only,** with gopls only.
  Not measured: repositories of tens of thousands of files (the per-query scan and the
  in-memory index grow with them), pyright, rust-analyzer, clangd or vtsls (their
  readiness signals, symbol shapes, and whether they honour `didChangeWatchedFiles` for
  closed files). An empty outline recorded for a file a ready server had not loaded would
  stay empty until the file changes (D34). Import resolution has no `sys.path`, tsconfig
  `paths`, Cargo workspaces, `-I` flags or Lua `package.path` (D35). Changes to
  `.git/info/exclude` or a global gitignore that move nothing in the tree are not seen until
  something does (D33).
- **`install` and the clean-machine criterion are untested against real
  tools.** PLAN §8 M4's "the six servers answer `references` on a clean machine"
  is proven only as far as the mechanism: resolution, layering and the install
  delegation are tested against a fake `mise` and fake servers, and nothing runs
  a real mise, downloads a real server or queries a real gopls, rust-analyzer,
  pyright, vtsls, clangd or lua-ls.
- **`--offline` guards lightspeed's own downloads only.** It does not stop a
  language server from fetching what it likes, and a change to `PATH` is not
  something the daemon notices (see [The daemon](#the-daemon)).
- **Deferred by PLAN §8:** sandboxing, WASM plugins, a
  library/SDK, and merging several servers' answers into one report — `check`
  reports one workspace at a time and says so when it skips files belonging to
  another.
- **`--symbol` costs a second lookup**, because the symbol names no file and the
  file it resolves to may belong to a different server. With the daemon that is
  two pool lookups, and for the common case — one server, one workspace — the
  second is warm; with `--no-daemon` it is still a second server.
- **Symbol retrieval is tested against a scripted server only.** Ids, `outline`,
  `source` and `context` are checked against documentSymbol answers written by
  hand in both shapes; no real gopls, pyright or rust-analyzer has been asked, so
  how a real server draws a symbol's range (and whether it includes the comment)
  is assumed from the protocol. The doc-comment and import-block rules are
  heuristics (D22), and `source <location>` — unlike `file` and ids — accepts a
  location in any file (D23).
- **Cancelling a call over the daemon does not stop the daemon's own work** on
  that request; it ends at the request's readiness deadline (D25).
- **No integration tests against real servers.** Everything is tested against a
  hermetic scripted fake server; PLAN §7's build-tagged tests against real
  gopls and rust-analyzer are not written.

## Development

`make` has the same targets (`build`, `test`, `vet`, `install`, `generate`, `check`); `make help` lists them.

```sh
go build ./...
go vet ./...
go test ./...        # hermetic: no network, no real language server
gofmt -l .

# every command test again, through a real auto-spawned daemon instead of in process:
LIGHTSPEED_TEST_VIA_DAEMON=1 go test ./internal/cli

# the index's numbers on this repository with a real gopls (skips cleanly without one):
go test ./internal/cli -run TestIndexPerformanceAgainstThisRepo -v
```

Layout follows PLAN §7. The MCP tests (`internal/cli/mcp_test.go`,
`mcp_symbols_test.go`) drive the server through the go-sdk's in-memory client
against the same fake language server. Tests that start a daemon wait for its
*process* to exit before they return (`daemon.WaitChildren`, D24), so they can run
in parallel and under `-count` and `-race` (`go test -race -count=5 ./...` takes about two minutes: the test binaries set `GORACE=atexit_sleep_ms=0` for the servers and daemons they start, since the race runtime otherwise sleeps a second before each of those processes exits). `internal/gopls/` is vendored from gopls and
x/tools; `internal/gen/corpus/` is the nvim-lspconfig corpus. Both are covered
by `ATTRIBUTION` (BSD-3-Clause, The Go Authors; Apache-2.0, nvim-lspconfig
contributors). Design decisions that resolve PLAN's open questions are logged in
`docs/DECISIONS.md`. This project has not declared a licence of its own yet.
