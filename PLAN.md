# Lightspeed — an LSP CLI

**Status:** plan / not implemented
**Binary:** `lightspeed`
**Language:** Go (≥1.26)
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
| MCP surface (deferred) | `modelcontextprotocol/go-sdk` | Now a gopls dependency, so it's the blessed choice. |

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
```

Derive the *available* subcommands from advertised capabilities at runtime, so `--help` and
`lightspeed servers` never lie.

**Location syntax:** gopls's span format — `file.go:line:col`, `file.go:line:col-line:col`,
`file.go:#offset`. 1-based lines, **byte** columns. Plus `--symbol 'pkg.Type.Method'`, because
agents are bad at computing columns and this is the ergonomic win they actually need.

**Output:**
- `--format json` (default when not a TTY): `{"version":1,"ok":true,"data":…,"warnings":[…]}`.
  Errors use the same envelope with `ok:false` and a machine code — never a bare stack trace.
- `--format text`: `file:line:col: text`, grep-compatible, one result per line.
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

**M-1 — Verification pass (do this first, ~half a day).** Install and probe `lsproxy`,
`lspeasy` and `agent-lsp`. Test each for the §5 failure modes: CJK-identifier rename,
query-during-indexing, partial multi-file edit. Confirm licenses for `go.lsp.dev/protocol`,
Helix and nvim-lspconfig. *Outcome: either evidence of a real gap, or a decision to contribute
upstream instead.*

**M0 — Spike.** Vendor the gopls files with ATTRIBUTION. `lightspeed raw` works end to end
against a hardcoded server.

**M1 — Read-only.** Router, docstore, readiness. `definition`, `references`,
`implementation`, `hover`, `symbols`, all formats.
*Done when:* `references` on a CJK fixture is byte-exact, and a mid-indexing query exits 5
rather than returning an empty list.

**M2 — Mutation.** Transactional applier, `rename` (preview + `--apply`), `codeaction`,
`format`.
*Done when:* a 3-file rename either fully applies or leaves the tree untouched; a scripted
malicious overlapping edit is rejected with nothing written; `--format diff | git apply`
reproduces `--apply` exactly.

**M3 — Daemon.** Port gopls's remote design to N servers.
*Done when:* second `references` on a rust-analyzer workspace returns in <200ms.

**M4 — Server resolution.** Config layers, generated defaults, PATH sniffing, mise-backed
`install`, `servers`, `doctor`.
*Done when:* gopls, rust-analyzer, pyright, vtsls, clangd and lua-ls all answer `references`
on a clean machine with no hand-written config.

**M5 — Polish.** `check` + SARIF, call hierarchy, `--symbol` resolution, batch/stdin mode, docs.

Deferred: MCP surface (`go-sdk`), sandboxing, WASM plugins, library/SDK, multi-server merging.

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
