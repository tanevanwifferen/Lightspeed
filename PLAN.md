# Lightspeed — an LSP CLI

**Status:** implemented through M5 (M0–M5 merged), plus the MCP surface (M6) and
symbol-addressed retrieval (M7: ids, `outline`, `source`, `context`, `tree`,
`repo_outline`, `file`), `search_text` and a round of robustness fixes (M8), and the
persistent workspace index with the whole-repo queries on it (M9: `search_symbols`,
`repo_map`, the import graph, `index status|build|clear`), the composed analysis tools
(M10: `type_hierarchy`, `blast_radius`, `check_references`, `rename_check`, `delete_check`,
`dead_code`, `changed_symbols`, `churn`, `hotspots`, `related`, `task_context`, and a `--limit`
on every list) and the agent setup kit (M11: `guide`, `version`, the
Makefile, `docs/AGENT-SETUP.md`, the capabilities test). The M3 daemon and the M4
server resolution are both wired to the command line: every server-backed
command goes through the per-workspace daemon unless `--no-daemon` says
otherwise, and resolves its server through the four layers of §6.
`lightspeed servers`, `install` and `doctor` exist, and `lightspeed mcp` serves the
command table to coding agents as MCP tools. See §8 for the
per-milestone state and the deferrals. `README.md` is the user-facing manual and
records the same gaps.
**Binary:** `lightspeed`
**Language:** Go (≥1.26; the module is `go 1.27.0`)
**Primary consumer:** coding agents (LLMs shelling out), secondary: scripted refactors, humans

**One-line pitch:** *`gopls`'s command-line interface, generalized to every language server.*

---

## 0. Prior art

This idea is **not novel**. It has been built several times and one official implementation
ships inside a major coding agent. The plan below therefore assumes reuse by default.

### Directly overlapping (same product)

| Project | What it is | Overlap |
|---|---|---|
| **`gopls` CLI** | gopls has a full CLI: `definition`, `references`, `rename`, `symbols`, `codeaction`, `call_hierarchy`, `check`, `format`, `prepare_rename`, `workspace_symbol` — plus a shared daemon (`-remote=auto`, `-listen.timeout`) and an MCP mode. By the Go team, BSD-3-Clause. | **~100% of the design, for one language.** This is the reference implementation. Our contribution is generalization, not invention. |
| **`@lsproxy/cli`** (npm) | LSP-driven CLI; builds its subcommand surface *at runtime* from advertised server capabilities. | ~90%. Their runtime command surface is better than a static list. |
| **`@lspeasy/cli`** (npm) | Server-agnostic **write-side** refactor CLI: project-wide rename, file-move with importer updates, move-symbol. | ~80%, and aimed at exactly our differentiator. |
| **GitHub Copilot CLI** | Official per-language LSP server config for definitions, references, renames. | Our primary use case, with distribution we can't match. |
| **Kiro** | Built-in code intelligence; config is `name`/`command`/`args`/`file_extensions`/`file_patterns`. | Their schema is essentially our manifest. |

*Verification note:* lsproxy and lspeasy are assessed from search snippets only — npm pages
didn't extract. The claim that Claude Code has a built-in LSP tool comes from lspeasy's own
marketing copy. **Verify both before relying on this table.**

### Adjacent (MCP-shaped, same substance)

**Serena** (`oraios/serena`, most mature), **`agent-lsp`** (whose headline feature is "warm
language server sessions" — our daemon, shipped), **`jonrad/lsp-mcp`**, **`Tritlo/lsp-mcp`**,
**`mcp-language-server`** (Go, BSD-style, uses edited gopls code), **`multilspy`** (Microsoft
research, Python).

### Installer / registry prior art

- **Mason** (`mason.nvim` + `mason-registry`) — mature package manager for language servers,
  DAP servers, linters, formatters. Installs from GitHub releases, npm, pip, cargo, gem,
  golang, composer, nuget, opam. Multi-registry priority, metadata API, cache staleness,
  optional Socket.dev supply-chain screening. Neovim-coupled, and packages can run arbitrary
  install logic.
- **mise** — already installed on this machine. 19 backends (aqua, ubi, github, npm, pipx,
  cargo, go, gem, http, …), 1017 registry entries, lockfiles with checksums, not editor-coupled.
  `mise ls-remote go:golang.org/x/tools/gopls` resolves gopls versions directly. **This is our
  installer.**
- **Helix `languages.toml`** / **nvim-lspconfig** — large curated open corpora of server
  config (command, args, root markers, language IDs) in the exact shape we wanted to invent.

### What is actually unclaimed

1. **Generalization of the gopls CLI model** to arbitrary servers, in one static binary with
   no Node/Python runtime. lsproxy/lspeasy need Node; Serena needs Python.
2. **Correctness under adversarial input** — UTF-16 positions, indexing-readiness detection,
   atomic all-or-nothing multi-file edits. Unglamorous, unadvertised, and the difference
   between a useful tool and one that silently corrupts code an agent trusts.
3. **Declarative-only server definitions** with mandatory checksum pins (vs Mason's arbitrary
   install logic), delegating installation to mise.
4. **CLI-first rather than MCP-first** — composes with shell, `git apply`, Make, CI.

**Honest assessment:** (2) and (3) are real engineering value. (1) is a convenience, (4) a
preference. Enough for a good personal tool and a plausible niche OSS one. Not enough to
displace Serena or Copilot CLI. The plan should not pretend otherwise.

---

## 1. Reuse inventory — the core of this plan

Everything here is verified present and permissively licensed unless marked **VERIFY**.

### Vendor (copy with attribution — BSD-3-Clause, Go Authors)

Precedent: `mcp-language-server` already vendors edited gopls LSP code under BSD.
gopls's `internal/` packages can't be imported, so copying with attribution is the intended path.

| File (from `gopls@v0.23.0/internal/protocol/`) | LOC | Solves |
|---|---|---|
| `mapper.go` + `mapper_test.go` | 368 | **Hard part #1.** Converts between byte offsets, `go/token` positions, 1-based line/byte-col, and LSP UTF-16 positions. Battle-tested by the Go team. |
| `span.go` + `cmd/parsespan.go` | 138 | `file.go:line:col`, ranges, `file.go:#offset` location syntax. Adopt this convention rather than inventing one. |
| `edits.go` | 186 | TextEdit ⇄ diff conversion, edit sorting, application. |
| `tsdocument_changes.go` | 81 | `documentChanges` union unmarshalling. |
| `x/tools/internal/diff` | — | Unified-diff generation for `--format diff`. |

That is roughly 800 lines that removes the single riskiest item in the project.

### Depend on

| Need | Package | Notes |
|---|---|---|
| LSP types + JSON-RPC client | `go.lsp.dev/protocol`, `/jsonrpc2`, `/uri` | LSP 3.18, generated from Microsoft's `metaModel.json`, typed client dispatcher, needs Go ≥1.26. **VERIFY license.** |
| Server installation | **`mise`** (shell out) | `mise use -g go:golang.org/x/tools/gopls@v0.23.0`, then `mise which`. Gets 19 backends, checksums and lockfiles for free. Fall back to `ubi`/`aqua` if absent. |
| MCP surface | `modelcontextprotocol/go-sdk` | Now a gopls dependency, so it's the blessed choice. **Built** (M6): `lightspeed mcp`, table-driven. |

### Read as reference (don't copy — learn the shape)

- **`gopls/internal/cmd/`** — one file per subcommand; the CLI conventions, flag naming and
  output formats to match. `remote.go` + `serve.go` are the proven daemon design:
  `-remote=auto` auto-spawns a shared server, `-listen.timeout` idle-exits. Do not design a
  daemon from scratch; copy this.
- **Helix `languages.toml`** (MPL-2.0 — **VERIFY** embedding/attribution rules) and
  **nvim-lspconfig** (**VERIFY**, believed Apache-2.0) — source corpora for built-in defaults.

### Build ourselves (the actual delta)

1. **Multi-server router.** gopls only knows Go. Path → server(s) via root-marker walk-up,
   glob, language id, priority. Multi-root and polyglot repos.
2. **Multi-server daemon.** gopls's daemon serves one server; ours pools N heterogeneous
   servers keyed by workspace root, with lifecycle and idle reaping per server.
3. **Readiness gating.** *Genuinely novel here.* The gopls CLI doesn't need it — it owns its
   own server and knows when it's loaded. We drive third-party servers and must detect
   indexing state from `$/progress` alone. See §5.2.
4. **Atomic cross-file WorkspaceEdit applier.** gopls's `rename -w` writes files; it does not
   have to defend against a hostile or buggy third-party server's edit set. Ours must.
5. **Agent-facing output contract.** JSON envelope, machine error codes, exit-code taxonomy,
   token discipline. §4.
6. **Config layering** + PATH sniffing + mise delegation. §6.
7. **Capability-derived command surface** (lsproxy's idea, worth stealing).

---

## 2. Goal and non-goals

```
$ lightspeed refs internal/store/user.go:42:8
$ lightspeed rename internal/store/user.go:42:8 UserRepository --apply
$ lightspeed symbols --query 'handle*' --kind function
$ lightspeed diagnostics ./internal --format sarif
```

**Non-goals:** not an editor or TUI; no interactive prompts (agents can't answer them); not a
language server (we never analyse); not a build tool; **not a package registry** (mise);
**not a position-mapping library** (gopls); no UI-only LSP features (inlay hints, semantic
tokens, signature help) in v1.

---

## 3. Architecture

```
   lightspeed (thin client, ~instant)
     │  unix socket: $XDG_RUNTIME_DIR/lightspeed/<workspace-hash>.sock
     ▼
   lightspeed daemon           ← design copied from gopls remote.go/serve.go
     ├── router                file path ─▶ which server(s)          [build]
     ├── session mgr           spawn / initialize / readiness        [build]
     ├── doc store             open docs + gopls Mapper per file     [vendor]
     ├── edit applier          WorkspaceEdit ─▶ disk, atomically     [build]
     └── server pool
           ├── gopls · rust-analyzer · pyright · vtsls · clangd · lua-ls
```

**Why a daemon is non-optional:** rust-analyzer needs 30–90s to index; jdtls minutes. Both
gopls and `agent-lsp` independently concluded the same. Auto-spawn, idle-exit, socket keyed on
resolved workspace root so `cd` into a subdir reuses it. `--no-daemon` for CI and debugging.

---

## 4. Command surface and output contract

Mirror gopls's names where they exist, so muscle memory and docs transfer:

```
lightspeed definition|references|implementation|hover|symbols|workspace_symbol
lightspeed rename <loc> <newname>       preview by default; --apply to write
lightspeed codeaction <loc|range>       list/apply
lightspeed format <path...>
lightspeed check [path...]              diagnostics; exit 1 if errors
lightspeed call_hierarchy <loc>
lightspeed raw <method> --params <json> escape hatch

lightspeed servers | install <name> | daemon status|stop|logs | doctor

lightspeed outline <file...>            symbols as a tree, with stable ids   (M7)
lightspeed source <id|loc...>           the source of symbols, byte-exact    (M7)
lightspeed context <id|loc>             source + import block + hover        (M7)
lightspeed tree [dir] | repo_outline [dir] | file <file> [--start N --end M]  (M7)
lightspeed search_text <query> [--regex --word --glob G --with-symbol]  live full-text search, no server

lightspeed index status|build|clear                                    the persistent symbol + import index   (M9)
lightspeed search_symbols <query> [--kind --lang --glob --path --detail --fuzzy]  ranked, whole workspace
lightspeed repo_map [--budget N]                                       files by import centrality, top symbols
lightspeed find_importers <file|package> | imports <file>              the import graph, resolved/external
lightspeed dependency_graph [path] [--depth --direction] | dependency_cycles

lightspeed type_hierarchy <sym> | blast_radius <sym|file> | check_references <name|sym>     composed analysis   (M10)
lightspeed rename_check <sym> <newname> | delete_check <sym> | dead_code [path]            read-only verdicts with evidence
lightspeed changed_symbols | churn [path] | hotspots | related <sym>                       git-aware
lightspeed task_context <task text> [--budget N]                                           no-LLM plan_turn / assemble_task_context

lightspeed guide [--format claude-md] | version                                            agent setup kit     (M11)
```

Derive the *available* subcommands from advertised capabilities at runtime, so `--help` and
`lightspeed servers` never lie.

**Location syntax:** gopls's span format — `file.go:line:col`, `file.go:line:col-line:col`,
`file.go:#offset`. 1-based lines, **byte** columns. Plus `--symbol 'pkg.Type.Method'`, because
agents are bad at computing columns and this is the ergonomic win they actually need — and, since
M7, `--id 'path::Container.Name#kind'`, the stable symbol id that `symbols`, `outline` and the other
listing commands emit (docs/DECISIONS.md D21).

**Output:**
- `--format json` (default when not a TTY): `{"version":1,"ok":true,"data":…,"warnings":[…]}`.
  Errors use the same envelope with `ok:false` and a machine code — never a bare stack trace.
- `--format text`: `file:line:col: text`, grep-compatible, one result per line. Paths are
  relative to the workspace root (`data.root` in JSON, a `# paths are relative to …` line in
  text when run from elsewhere); `--absolute` opts out (D30).
- `--format diff`: unified diff, feedable to `git apply`. Default for `rename` preview.
- `--format sarif`: diagnostics only (SARIF 2.1.0).
- **Token discipline:** matched line only; `--context N` opt-in; `--limit N` with explicit
  `truncated: true` rather than silent cutoff.
- **Exit codes:** 0 ok · 1 problems found · 2 usage · 3 no server · 4 crash/timeout ·
  5 not-ready/indexing timeout.

---

## 5. The hard parts

### 5.1 Position encoding — *solved by vendoring*
LSP columns are UTF-16 code units; CLIs and humans think bytes. Any non-ASCII identifier
shifts every result. **Do not write this.** Vendor gopls's `Mapper` and its tests. Add our own
emoji/CJK fixtures on top.

### 5.2 Readiness — *the one genuinely new problem*
A server answers "0 references" while still indexing. This is the worst failure mode
available to us: it looks authoritative and will make an agent delete live code. gopls's CLI
sidesteps it by owning its server; we cannot.

Mitigation: track `$/progress` and `window/workDoneProgress/create` tokens; hold the workspace
not-ready until the initial progress set drains; fall back to "retry until the result is
stable for 750ms, up to `--timeout`"; **exit 5 rather than return an empty result of unknown
authority.** This deserves the most test effort in the project.

### 5.3 Atomic WorkspaceEdit application
Handle `changes` *and* `documentChanges`, versioned edits, `create`/`rename`/`delete` ops in
order, overlap rejection, reverse-order application within a file, CRLF and final-newline
preservation. Temp file + atomic rename. All-or-nothing across files: stage in memory,
validate, commit; on any failure write nothing. Vendored `edits.go` covers conversion and
sorting; the transactional guarantee is ours.

### 5.4 Lesser but real
Documents must be `didOpen`ed before most servers answer. Never call uncapabilitied methods.
Server quirks (gopls wants a real module root; rust-analyzer emits custom progress tokens;
jdtls needs `-data`; pyright wants a config file; some need `didChangeConfiguration` after
init) live in declarative config, not Go `switch` statements.

---

## 6. Server definitions — consume, don't build

Resolution order for "what handles this file":

1. `.lightspeed.toml` in the workspace root — in-tree, version-controlled, highest priority.
   The only file we ask users to write.
2. `$XDG_CONFIG_HOME/lightspeed/servers.d/*.toml` — user overrides.
3. **Built-in defaults generated at build time** from Helix `languages.toml` / nvim-lspconfig
   via `go generate` into an embedded table. Current without owning a registry.
4. **PATH sniffing** — if `gopls` is on PATH, use it. Zero-config, and the common case.
   A file on PATH is not evidence it runs: a mise shim is verified with `mise which` in the
   workspace (falling back to the installed version, or exit 3), and `servers`/`doctor` start
   what they found (docs/DECISIONS.md D27).

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
mise = "go:golang.org/x/tools/gopls@v0.23.0"   # delegate; mise handles checksums+lockfile
```

**Security posture:** nothing downloads implicitly — missing server exits 3 with the exact
command to run. Definitions are pure data. `--offline` / `LIGHTSPEED_OFFLINE=1` hard-disables
network. **No sandbox in v1, documented loudly:** language servers execute repo build tooling
by design (gopls runs `go list`, rust-analyzer runs `cargo metadata`), i.e. arbitrary code from
the checkout. Do not point this at untrusted code. `--sandbox` (bubblewrap/landlock) later.

---

## 7. Repo layout and tests

```
cmd/lightspeed/          entrypoint
internal/cli/            command wiring, flags, span parsing
internal/render/         json / text / diff / sarif
internal/daemon/         socket server, spawn, idle reaper
internal/client/         LSP lifecycle, capabilities, progress, readiness
internal/router/         path ─▶ server resolution
internal/docstore/       open docs, Mapper cache
internal/symbols/        documentSymbol decoding, stable ids, doc-comment rules (shared by outline and index)
internal/index/          persistent LSP-fed symbol + import index, freshness, search, repo map, import graph
internal/edit/           transactional multi-file apply
internal/serverdef/      config layers, schema, mise delegation
internal/gopls/          VENDORED mapper.go, span.go, edits.go + ATTRIBUTION
internal/gen/            build-time import of helix/lspconfig corpora
schema/serverdef.schema.json
ATTRIBUTION              BSD-3-Clause notice for vendored Go Authors code
```

**Tests:** (a) vendored gopls tests kept as-is, plus emoji/CJK fixtures; (b) a **fake language
server** with scripted responses — including "answers empty while indexing" and "emits a
malicious overlapping WorkspaceEdit" — for fast, hermetic protocol tests; (c) build-tagged
integration tests against real gopls and rust-analyzer.

---

## 8. Milestones

Status marker: **done** means built, tested and reachable from the command line;
**built, not wired** means the package is complete and tested but no subcommand
exposes it. A criterion that names a real server's behaviour is proven only as
far as the fake server can prove it, and each milestone says where that stops.

**M-1 — Verification pass (do this first, ~half a day).** *Not done.* No
third-party tool was installed or probed, and the licence checks were made only
where the code needed them (`go.lsp.dev` was avoided entirely — see
docs/DECISIONS.md D3 — and nvim-lspconfig's Apache-2.0 is recorded in
ATTRIBUTION; Helix's MPL corpus stays unused, so §9.3 remains open). The gap
assessment in §0 is therefore still search-snippet evidence, and the honest
assessment there stands unverified. Install and probe `lsproxy`,
`lspeasy` and `agent-lsp`. Test each for the §5 failure modes: CJK-identifier rename,
query-during-indexing, partial multi-file edit. Confirm licenses for `go.lsp.dev/protocol`,
Helix and nvim-lspconfig. *Outcome: either evidence of a real gap, or a decision to contribute
upstream instead.*

**M0 — Spike. Done.** Vendor the gopls files with ATTRIBUTION. `lightspeed raw` works end to end
against a hardcoded server.

**M1 — Read-only. Done.** Router, docstore, readiness. `definition`, `references`,
`implementation`, `hover`, `symbols`, all formats.
*Done when:* `references` on a CJK fixture is byte-exact, and a mid-indexing query exits 5
rather than returning an empty list. — both hold, and both are tested.

**M2 — Mutation. Done.** Transactional applier, `rename` (preview + `--apply`), `codeaction`,
`format`.
*Done when:* a 3-file rename either fully applies or leaves the tree untouched; a scripted
malicious overlapping edit is rejected with nothing written; `--format diff | git apply`
reproduces `--apply` exactly. — all three hold, and all three are tested.

**M3 — Daemon. Done.** Port gopls's remote design to N servers.
*Done when:* second `references` on a rust-analyzer workspace returns in <200ms.

`internal/daemon` implements auto-spawn, idle reaping and the N-server pool
keyed by workspace root, and `internal/cli` now uses it: `definition`,
`references`, `implementation`, `hover`, `symbols`, `workspace_symbol`,
`rename`, `codeaction`, `format`, `check`, `call_hierarchy`, `batch`, `raw`,
`help <target>` and `--symbol` resolution all reach their server through the
auto-spawned daemon, so the second query on a workspace reuses the warm
session. `--no-daemon` (or `LIGHTSPEED_NO_DAEMON=1`) runs the same service in
process instead, and the two modes print byte-identical envelopes — with one
caveat: the text of a not-ready message carries a wall-clock duration and an
attempt count ("gave up after 30.2s and 41 attempt(s)"), which differ between
any two runs of the same command, in either mode. The code, the exit status and
every other byte are the same, and the tests compare the timing text after
normalising it.
`lightspeed daemon status|stop|logs` inspect and stop it, and the socket is
keyed on the resolved workspace root so a subdirectory reaches the same daemon.
Decisions taken here are logged as D14–D16 in docs/DECISIONS.md; D24 later made
`daemon stop` (and the test harness) wait for the daemon *process* and not only
its socket, which fixed a test flake.

What is proven, and what is not. Proven hermetically, against the scripted fake
server: two consecutive invocations start **one** language-server process
(counted by the server itself, not by the daemon's report), `--no-daemon`
starts one per call, a warm server that is still indexing still exits 5 with
the same envelope as in process, and the whole `internal/cli` suite passes in
both modes (`LIGHTSPEED_TEST_VIA_DAEMON=1`). **Not proven: the <200ms latency
figure itself.** It needs a real rust-analyzer, which no test in the tree
starts (see the deferrals below); what the tests establish is the mechanism the
figure follows from — the second query does not pay for a server.

**M4 — Server resolution. Done.** Config layers, generated defaults, PATH sniffing, mise-backed
`install`, `servers`, `doctor`.
*Done when:* gopls, rust-analyzer, pyright, vtsls, clangd and lua-ls all answer `references`
on a clean machine with no hand-written config.

`internal/serverdef` implements the layering, the generated defaults, PATH
sniffing, the TOML subset and mise delegation, with per-key provenance, and the
six definitions are checked against real install artifacts. `internal/cli` now
resolves every path through it: `.lightspeed.toml` in the workspace root over
`$XDG_CONFIG_HOME/lightspeed/servers.d/*.toml` over the generated defaults, then
PATH sniffing for the executable — for the client's routing and for the pool the
daemon (or `--no-daemon`) routes with. `lightspeed servers` lists the resolved
servers with the layer and file each key came from and whether each is
installed; `lightspeed doctor` is `serverdef.Doctor`; `lightspeed install
<name>` prints the exact mise command and runs it only with `--run`. A missing
server exits 3 with that command in the envelope. `--offline` (or
`LIGHTSPEED_OFFLINE=1`) is a global flag over serverdef's kill switch, and it
refuses `install --run`. The M0 `LIGHTSPEED_SERVER_CMD` override is gone; tests
name the fake server in `servers.d`, as a user would. A daemon that was started
under different definitions than the command resolved is restarted if nobody
else is using it and refused (`daemon_stale`, exit 2) if somebody is. Decisions
taken here are logged as D17–D19 in docs/DECISIONS.md.

What is proven, and what is not. Proven hermetically: each layer winning, both
in `servers` output and in which executable is actually started; a definition
in `.lightspeed.toml` alone routing a file type nothing else claims; PATH
sniffing finding a server with no configuration at all; the exit-3 envelope's
install command; `install` planning by default, running only on `--run`, and
refusing offline (from the flag, the environment and a batch); a config change
restarting an idle daemon and being refused by a busy one. The `install` tests
run against a fake `mise` script. **Not proven: the criterion itself.** No test
runs a real mise, downloads a real server, or asks a real gopls,
rust-analyzer, pyright, vtsls, clangd or lua-ls anything, so "answers
`references` on a clean machine" holds as far as the mechanism goes — the
definitions are checked against install artifacts, the resolution and the
install delegation are tested against fakes — and no further.

**M5 — Polish. Done.** `check` + SARIF, call hierarchy, `--symbol` resolution, batch/stdin mode, docs.
Decisions taken here are logged as D10–D13 in docs/DECISIONS.md: how `check`
decides it has every diagnostic (readiness gate + a publish per opened file, exit
5 otherwise), how `--symbol` handles ambiguity (refuse, with every candidate as a
location), and batch mode's per-line envelopes and severity-ranked exit code.

Deferred: sandboxing, WASM plugins, library/SDK,
multi-server merging (`check` reports one workspace at a time and warns about the
files it skipped). Also outstanding, and not on the original list: PLAN §7's
build-tagged integration tests against real gopls and rust-analyzer — every test
in the tree is hermetic against `internal/fakeserver` (or a fake `mise`), which
also means M3's latency criterion and M4's clean-machine criterion have never
been measured on a real server.

**M6 — MCP surface. Done.** `lightspeed mcp`: the command table as an MCP server
over stdio, on the official `go-sdk`, so that lightspeed can stand in for an MCP
code-navigation server in a coding agent.
*Done when:* every query and mutation command is a tool with a typed, described
input schema; a tool's result is the CLI's envelope with `isError` on `ok:false`;
the mutating tools preview unless `apply`; and a command added to the table is a
tool with no MCP code.

Every command-table entry declares `MCP` tools with a per-command parameter spec
(`internal/cli/params.go`) or `NoMCP` with the reason. The schema, the argument
vector and the read-only hint are derived from the spec, a call runs the
command's own `Run` in process against the daemon, and a test fails when a
command is neither exposed nor excluded or when the spec and the command's flags
disagree. Decisions taken here are logged as D20 in docs/DECISIONS.md.

What is proven, and what is not. Proven hermetically, through the go-sdk's
in-memory client against the scripted fake server, in process and (with
`LIGHTSPEED_TEST_VIA_DAEMON=1`) through a daemon: the tool list and its schemas;
that a `references` call returns the CLI's envelope byte for byte; that an error
envelope is an `isError` result; that `rename` without `apply` writes nothing and
with it writes, and that the dirty-worktree refusal still applies; that a
not-ready workspace is an error result with the not-ready code; and one real
stdio session through `lightspeed mcp`. **Not proven:** anything with a real MCP
client such as Claude Code, and anything with a real language server — the same
gap as M3 and M4. (Cancellation, which was missing here, was added afterwards: D25.)

**M7 — Symbol-addressed retrieval. Done.** What it takes for lightspeed to be
the code-navigation server of a coding agent: an agent reads one
symbol instead of a file.
*Done when:* every location command takes `--id` and every symbol-listing command
emits it; `outline`, `source` and `context` return a file's symbols, a symbol's
byte-exact text with explicit truncation, and its import block and hover; `tree`,
`repo_outline` and `file` describe the workspace without a language server; and all of
it is an MCP tool with no MCP code.

Ids are `path::Container.Name#kind[~N]`, built from `documentSymbol` in both of its
shapes and recomputed each use; a stale one is `stale_id` with candidates (D21).
`outline`/`source`/`context` slice by bytes, include the doc comment, cap explicitly
and hash (D22). `tree`/`repo_outline` use git's file list or a walk, and `file`, `tree` and
`repo_outline` are confined to the workspace (D23). The same change closed three leftovers: a flaky
daemon test (D24), MCP calls that ignored cancellation (D25) and a parameter-spec test
that did not check positionals (D25).

What is proven, and what is not. Proven hermetically, against a scripted server that
answers `documentSymbol` both hierarchically and flat: the ids of a file with CJK and
emoji, two `init`s and a two-line signature; the same ids from both shapes;
byte-exact `source` including a byte cap that falls inside an emoji and a location
whose byte and UTF-16 columns differ; a stale id (typo, wrong kind, deleted, moved
file) with its candidates from the CLI and MCP; `--id` on every location command;
ids on `workspace_symbol` and `call_hierarchy`; `tree` against a real git repository
and a plain directory; the confinement of `file`, `tree` and `repo_outline` by `..`,
absolute path and symlink (CLI and MCP); and
all six as MCP tools with the CLI's envelope. `go test ./... -count=5` and `-race`
are clean, and so is the suite through a real daemon
(`LIGHTSPEED_TEST_VIA_DAEMON=1`). Checked by hand against gopls 0.23 (not by a test):
`--id` on `definition`, `references`, `hover` and `call_hierarchy`, and `references`
through `lightspeed mcp`; gopls answers `documentSymbol` flat with the range starting
at the `func` keyword, so the name is located inside the declaration (D21).
**Not proven:** anything against pyright or rust-analyzer — how they draw a symbol's
range, whether it includes the comment, whether their `documentSymbol` names match the
id rules — and any measure of the token saving that motivated it. The comment and import-block rules are heuristics.

**M8 — `search_text` and robustness. Done.** `lightspeed search_text` (D26): a live,
parallel full-text search of the working tree with no server and no index, so it is never
stale; substring or RE2 regex, case, whole word, globs, `--path` scoping, byte columns,
explicit clipping and truncation, and `--with-symbol` for the enclosing symbol id. And the
bugs found by running the real binary against real gopls: a broken mise shim was reported
as installed (D27: shims are verified with `mise which`, the installed version is launched
or the server is unusable with the exact `mise use` command, and `servers`/`doctor` start
what they sniff); `spawn_failed` was opaque (D28: exit status and a bounded stderr tail in
`error.data`, same in both modes); a daemon of another build silently served a newer CLI
(D29: build identity, replacement with a warning, exit when idle if its executable is
gone); text paths were absolute (D30: workspace-relative, `data.root`, `--absolute`); and
the first review's leftovers on `source`, `context` and `outline` (D31).

What is proven, and what is not. Proven hermetically: each item has regression tests that
were checked to fail without their fix, and the suite passes in process and through a real
daemon. The mise behaviour was also checked by hand on this machine's real mise 2026.9.7
(shim with no active version, gopls 0.23.0 installed); `search_text` was timed on this
repository (17–35 ms). **Not proven:** any of it against a real language server in a test
(the deferral of §8 stands); `--with-symbol` against anything but the scripted server; the
stale-daemon replacement across two genuinely different releases (a copy of the test binary
with another mtime stands in); a shim from a version manager other than mise.

**M9 — The workspace index. Done.** A daemon-side, persistent, LSP-fed symbol index and
the whole-repo queries on it, so that lightspeed can answer the whole-repo questions an
index-based navigation server answers, without giving up what its design has and a prebuilt index lacks:
*nothing served is older than the disk* (D32–D37).
*Done when:* `index status|build|clear`, `search_symbols` (BM25 with exact/prefix boosts,
filters, `--detail`, `--fuzzy` only when nothing matched, an authoritative empty answer),
`repo_map` (token-budgeted, PageRank), `repo_outline` symbol counts when warm, and
`find_importers`, `imports`, `dependency_graph`, `dependency_cycles` exist as commands and
MCP tools; every query revalidates the files it reports on against the disk; a server that
is still indexing is exit 5 and records nothing; the cache is versioned, atomic and safe
under two writers; and the daemon tells its servers about files changed outside lightspeed.

`internal/index` holds the entries, the persistence, the freshness (`Tracker`: stat, racy
window, hash on change), the search, the map and the graph, and knows nothing of sessions:
the daemon gives it a `Backend` over its pool (`internal/daemon/index.go`), which is also where
the reconciliation of the servers with the disk lives (D33, closing the gap D15 recorded).
The id, decoding and doc-comment code `outline` used moved to `internal/symbols` so the index
and the commands share one definition (D21). Import extractors (Go via `go/parser`; Python,
TypeScript/JavaScript, Rust, C/C++ and Lua by lexers that ignore comments and strings) are
pluggable behind one interface; a language with no extractor is `not_covered` (exit 3), never
"no imports" (D35). Also done: the three small leftovers — `search_text --with-symbol`'s text
form prints the full id, an `outline` batch's per-file error uses the workspace-relative
path, and the `spawn_failed` summary quotes the first *informative* stderr line as well as
the last.

What is proven, and what is not. Proven hermetically, against a scripted server that
outlines files from their text and caches its view of the disk the way a real one does: a
cold build and a warm one that builds nothing; edit, add, delete and rename between two
queries (index) and an external edit between two `references` through a warm daemon (with the
reconciliation removed the test fails); a same-size edit in the same clock tick; corrupt,
truncated, foreign, schema-bumped, build-bumped and server-changed caches; a not-ready
server (exit 5, nothing recorded); two concurrent builders; ranking (exact > prefix > token >
fuzzy, guaranteed by construction); every extractor with its comment and string traps; cycles;
workspace confinement; and every new tool through MCP. Measured with real gopls 0.23.0 on
this repository (`TestIndexPerformanceAgainstThisRepo`, skipped without gopls): cold build
3.2 s, warm `search_symbols` ~10 ms, revalidation of the unchanged tree ~10 ms; and, by hand,
a warm gopls that answers *stale* without the reconciliation and correctly with it.
**Not proven:** anything against pyright, rust-analyzer, clangd or vtsls; repositories much
larger than this one; that servers other than gopls honour `didChangeWatchedFiles`; the token
cost beyond this repository (`make bench` measures it here); the comment and range rules against servers other than gopls.

**M10 — Composed analysis tools. Done.** Eleven commands that answer one question by combining
evidence that already exists (references, implementation, call and type hierarchy, the index, the
import graph, `search_text`, git), so a coding agent asks once instead of chaining five calls:
`type_hierarchy`, `blast_radius`, `check_references`, `rename_check`, `delete_check`, `dead_code`,
`changed_symbols`, `churn`, `hotspots`, `related`, `task_context` (D38–D41). Each row names its
evidence, each bound is reported when it bites, each verdict states its rule, none writes. The
index gained an unranked `symbols` operation (`internal/index/symbols.go`) — the enclosing-symbol
and enumeration primitive the composed commands share — and `internal/cli/compose.go` is the kit
(subject identification, enclosing symbol, bounds, test-path heuristic). The same milestone
closed the parity review's leftovers (D42): every list-returning command honours `--limit`
(one table-driven test enforces it), `search_symbols` rows carry their match quality,
`find_importers` names the test-only importers it leaves out, and the confinement, message and
text-output leftovers.
*Done when:* every command above is a CLI command and an MCP tool with a parameter spec, is
covered hermetically in process and through a daemon, and the limit test passes for all of them.

What is proven, and what is not.

**Verdict tools (D38).** Proven hermetically against the scripted server: each of `type_hierarchy`
(native, `--depth` cycles, `--limit`, the `implementation` fallback and its warning, exit 3 when
neither exists), `check_references` (four ways of naming a symbol give one answer, the two lists are
disjoint, an unknown name is text-only, an ambiguous one is verdict `ambiguous` with per-candidate counts (D44), no `referencesProvider` degrades to text),
`rename_check` (ok, collision through the index, refused, noop, unverified; nothing written) and
`delete_check` (in use, own-declaration references ignored, tests only, last symbol of a package with
its importers), in CLI json/text, as MCP tools, and through a real daemon. Checked by hand against gopls
0.23 on this repository: native type hierarchy, references with enclosing ids, a rename refused for a
name conflict. **Not proven:** anything against pyright, rust-analyzer, clangd or vtsls; the export
rules for languages other than Go beyond unit tests; the collision scope of languages other than Go.

**`blast_radius` and `dead_code` (D39).** Proven hermetically against the scripted "word server"
(references, outline and implementation answered from the words in the files; call hierarchy
canned): blast_radius's summary, rows per evidence source, grouping by enclosing symbol, test flags,
depth bounds reported (`calls_at_limit`), fair `--limit`, a server without call hierarchy, file
subjects, not-ready exit 5, MCP; dead_code's candidates vs used/tested/exported/implementation/
entry-point/ignored symbols, budget + cursor resumption to the same set, `--include-exported`,
scope, `.lightspeed.toml` `[dead_code]` (which `internal/serverdef` now accepts), not-ready exit 5,
MCP; the suites also pass through a real daemon and under `-race`. Run by hand against real
gopls 0.23 on this repository: `blast_radius` on a helper (33 rows, ~4 s cold), `dead_code` over
`internal/render` and `internal/cli` (found `symbolByID`, which was unused). **Not proven:** anything
against pyright, rust-analyzer, clangd or vtsls (export rules, implementation lookup, outline
positions); the confidence levels are a heuristic with no measurement behind them; blast_radius of a
*file* reports importers only.

**Git-aware tools (D40).** `changed_symbols`, `churn`, `hotspots`, `related`. Proven hermetically
against real temporary git repositories and the scripted server (whose outline can give each
function its body as its range): hunk→symbol mapping (added / modified / removed, innermost symbol,
renames, deleted files, file-level rows), `--staged`, `--base`, untracked files, `--path`, the
hunk-only degradation with its warning, a non-repository (warning, exit 0), churn per file and per
symbol over a window (the mutation "carry lines forward through later commits" is checked to fail
the test), hotspot ranking and its formula, `related` with all four kinds of evidence, merging and
ranking, a server without call hierarchy, truncation for each, MCP, and through a real daemon.
Checked by hand with real gopls 0.23 on this repository: `changed_symbols` (old side outlined
through a scratch file), `churn`, `hotspots`, `related --id`. **Not proven:** the old-side outline
against servers that refuse a document outside their workspace (falls back to hunk-only, reported);
per-symbol churn over history with many moves (it is an approximation, stated); `related`'s call
hierarchy beyond gopls.

**`task_context` (GOAL A.8, D41).** Proven hermetically (text server + scripted call graph): a
task naming an existing symbol is high with its source, a caller, callee totals and siblings; two
exact matches are medium; an exact plain word is medium; absent or loosely matching tasks are
low, "probably not implemented here", with nothing padded and the weak matches only counted; the
budget bites on symbols, on source (cut and marked) and is reported; file terms boost and list;
tests are demoted; usage errors, exit 5 while indexing and the MCP tool. **Not proven:** the
weights and the confidence thresholds against a corpus of real tasks (they were tuned on the
fixtures and one run on this repository); call hierarchy and import rows against any server but
the scripted one and gopls; languages other than Go.

Review round 3 (D44): `blast_radius` on a type/constant/field survives gopls's `prepareCallHierarchy` error (fake
server now reproduces it), `check_references` on an ambiguous name is `ambiguous` not `used_only_in_text`, and
`task_context --budget` is honoured (one-line rule, `--rule` for the full one, diagnostics shed, `budget.over`).
Checked by hand against gopls on this repository.

Review round 4 (D41, D43, D40): `task_context` no longer answers low for a feature that exists and is described in
words — a word of a symbol's own name counts (`name_token`), the plain words are also searched together, a compound
hit (`Pool.ReapIdle` for "reap idle language servers") or half of the task in one name is medium, and the code leads
its tests; absent features ("oauth token refresh" and ten more) stay low. The `guide` MCP tool and the server
instructions name tools and parameters, not commands and flags. `hotspots` ranks code only unless `--all`.
**Still judgement, not measurement:** the `task_context` qualities and thresholds, checked on this repository's
named cases and the hermetic fixtures, not on a corpus of real tasks.

Limits and review leftovers (D42): every list command honours `--limit` (enforced by one table-driven test,
`TestEveryListCommandHonoursLimit`, which also fails for a command with neither a limit case nor a
written reason); `find_importers` gained a 50-row default, `--limit`, `test_only_importers` and
`--include-tests`; `search_symbols` rows carry match quality at every detail level and text marks
non-exact rows, with the "no symbol named X (exact or prefix)" notice first; a start outside the
workspace is `outside_workspace` even when nothing is there; `outline`/`source` no longer double the
target in a warning; `repo_outline` text shows symbol counts when warm.
Proven: hermetically (each fix checked to fail without it; suite in process and via daemon) and by hand
against real gopls on this repository. Not proven: the default bounds (50/100/200/20) are judgement;
test-only importers are Go only; nothing measured on other servers.
Remaining: none of Goal B; new list commands must register a limit case (or a reason) in their own test file.

**Not proven, for all of M10:** the composed commands have not been measured against a real MCP
client, nor against language servers other than gopls; their token cost is
measured on this repository only (`make bench`).

**M11 — The agent setup kit. Done.** Everything needed to serve Claude Code with `lightspeed mcp`
(D43). `lightspeed guide` (CLI, MCP tool, and the MCP server's
instructions) generates the agent policy from the command table; `--format claude-md` frames it with
markers for regeneration. `docs/AGENT-SETUP.md` lists the capabilities an agent relies on and the
command that provides each, lists what is not provided and why, and gives the exact setup and rollback steps.
`lightspeed version` reports the build; the Makefile has build, test, vet, install, generate, check.
*Done when:* a capabilities test fails if a listed capability's command is missing from the table or not an MCP
tool, and if the setup doc drifts from it; the guide cannot name a command or flag the binary
lacks.

What is proven, and what is not. Proven hermetically: the guide's generation from the table (an
edited summary shows up), the three drift tests, the compact form under its cap and carrying the two
rules a client acts on, the MCP `guide` tool equal to the CLI, the server's instructions equal to the
compact guide, `version` in both formats and `-v`, a CLAUDE.md section regenerated idempotently by the
documented snippet (run by hand on a scratch file), and every Makefile target run (`install` into a
scratch GOBIN). **Not proven:** anything with a real Claude Code — that it reads the instructions, that
the section shrinks its context as intended, that `claude mcp add` invocations are exactly as
documented for its current version; the setup steps were written, not executed against a real
`~/.claude`.

**What remains, for the branch.** No test in the tree starts a real MCP client, pyright, rust-analyzer,
clangd, vtsls or lua-language-server (only gopls, by hand and one skipped-without-gopls test); the
composed commands were proven against the scripted fake server; the comment and range rules are only
checked against gopls; large repositories are unmeasured; wider analysis
(complexity, coupling, layer violations, PR risk) is not attempted; the setup steps are Claude Code only.

---

## 9. Open questions

1. Module path / repo host for `go.mod`.
2. **Is read-only (M1) worth building**, given Copilot CLI and gopls already cover it? A
   mutation-only tool is a sharper wedge — exactly how `lspeasy` positions itself. But M1 is
   the natural way to build the plumbing M2 needs, so the answer may be "build it, don't
   market it".
3. Helix `languages.toml` is MPL-2.0 — check whether embedding generated data triggers
   file-level copyleft obligations before depending on it. nvim-lspconfig may be the safer
   corpus.
4. Should `rename --apply` refuse a dirty git worktree by default? (Suggest yes, with
   `--allow-dirty` — an agent's only reliable undo is `git checkout`.)
5. Hard-depend on mise, or vendor a minimal `ubi`-style downloader as fallback? (Suggest:
   mise if present, `path` sniffing otherwise, no third path in v1.)
