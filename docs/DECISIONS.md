# Decisions

Running log of decisions that resolve open questions from PLAN.md.

## D1 — Module path (PLAN §9.1)

**Decision:** `github.com/tanevanwifferen/Lightspeed`.

There is no remote yet; GitHub is the assumed eventual host. The path is
cheap to change before the first public release (single `go.mod` line plus
an import rewrite), so this does not need to block M0. The vendored-code
provenance in ATTRIBUTION references this path and must be updated if it
changes.

## D2 — gopls vendor version

Vendored from `golang.org/x/tools/gopls@v0.23.0`, exactly the version PLAN
§1 references, and `golang.org/x/tools@v0.47.1-0.20260707181000-a299dadba899`
(the x/tools version gopls v0.23.0 itself requires) for `internal/diff`.
See ATTRIBUTION for the file-by-file list and the adaptations made.

## D3 — Own JSON-RPC framing instead of go.lsp.dev (M0 only)

PLAN §1 suggests `go.lsp.dev/protocol` + `/jsonrpc2` but flags its license
as unverified (an M-1 task that has not been done). For the M0 spike the
client needs only: LSP base-protocol framing (Content-Length headers),
request/response correlation, and the initialize/shutdown lifecycle —
about 200 lines in `internal/client`. Written by hand for now; swapping in
`go.lsp.dev` (or `x/tools/internal/jsonrpc2` vendored) stays open for M1
once the license check happens. No third-party module dependencies yet.

## D4 — `raw` server command in M0

M0 requires `raw` to work "against a hardcoded stdio server command". The
hardcoded default is `gopls serve` (the reference server, PLAN §0).
The environment variable `LIGHTSPEED_SERVER_CMD` (whitespace-split argv)
overrides it; this exists so the hermetic fake-server test can point the
CLI at itself, and is M0 scaffolding — real server resolution is M4
(router + serverdef), at which point the variable is removed or formalized.

*Resolved by D18: removed.*

## D5 — Vendored `internal/cmd` span parser kept unexported

`internal/gopls/cmd` keeps gopls's `package cmd` shape with its unexported
`span`/`point`/`parseSpan`; its own vendored test exercises it. An exported
wrapper API belongs to `internal/cli` span parsing in M1 — deciding the
exported surface now, before there is a caller, would be guessing.

## D6 — Readiness rules and what "authoritative" means (PLAN §5.2)

`internal/client.Gate` accepts an answer under exactly four rules, in order:

1. progress drained and the answer is non-empty (first attempt, no waiting);
2. progress drained and the answer was stable for 750ms — required before an
   *empty* answer is believed;
3. the server never sent `$/progress` at all (500ms grace) and the answer was
   stable for 750ms;
4. progress was announced but never drained, has been silent for 750ms, and a
   *non-empty* answer was stable for 750ms.

Rules 3 and 4 attach a warning to the envelope, because their readiness is
inferred rather than observed. An empty answer never qualifies under rule 4:
"no references" from a server with unfinished work is precisely the dangerous
case. Anything else is a `*NotReadyError`, exit 5.

Deliberate consequences:

- A server that never speaks the progress protocol *can* return an empty
  answer (rule 3). Refusing that would make lightspeed useless against every
  server without progress support; the warning is the honest half-measure.
- While a server is actively reporting progress the request is not issued at
  all — its answer could only be discarded.
- `ContentModified` (-32801) and `ServerCancelled` (-32802) are treated as
  readiness signals, not failures: they are what a server sends when its state
  moved under the request, and they reset the stability window.
- The timeout is per query, not per session, so a daemon-pooled session
  (PLAN §3) does not hand its second query an already-expired budget.

## D7 — Exit codes travel on the error, not in a shared enum

Errors from `internal/client` carry `ExitCode() int` (5 for `*NotReadyError`,
3 for `*UnsupportedMethodError`, 1 for a server-reported `*RPCError`) and
`Code() string` for the envelope's machine code. The CLI maps any error with a
type assertion on an anonymous `interface{ ExitCode() int }`, so the exit-code
taxonomy of PLAN §4 stays in `internal/cli` and `internal/client` does not
import it. `errors.Is` works too, against `ErrNotReady` and
`ErrUnsupportedMethod`.

## D8 — `--apply` refuses a dirty git worktree (PLAN §9.4)

**Decision:** yes, refuse, with `--allow-dirty` to override. Only tracked
modifications count, and anything short of a clean answer from git is a
warning rather than a refusal.

PLAN §9.4 asks whether `rename --apply` should refuse a dirty worktree and
suggests yes. It does, and so do `codeaction --apply` and `format --apply`:
the reason is a property of writing, not of renaming.

An agent's only reliable undo is `git checkout`, and that undo only works if
everything the command finds in the worktree afterwards was put there by the
command. Mixed with the agent's own uncommitted work, `git checkout` stops
being an undo and becomes a second, larger mistake — so the safe state has to
exist *before* lightspeed writes, not after. The check therefore runs as a
precondition, before a language server is even started: finding out after a
90-second rust-analyzer load would make the refusal useless.

Untracked files are not dirt. `git checkout` does not remove them, so their
presence costs the caller nothing, and refusing over a stray build artefact
would train callers to pass `--allow-dirty` reflexively — which would cost the
check its whole value.

No git, no repository, or a git that declines to answer (a "dubious ownership"
refusal looks exactly like "not a repository") produces an envelope warning
saying there is no undo, and the write proceeds. lightspeed is not a git tool,
and being unusable outside a repository would be a worse failure than the one
this prevents.

Detection shells out to `git rev-parse --show-toplevel` and
`git status --porcelain -z --untracked-files=no`; no internal package knows
anything about git, and vendoring a repository reader to avoid two subprocess
calls that only run on `--apply` would be a poor trade.

## D9 — A code action that arrives as a command (PLAN §4, M2)

**Decision:** resolve it with `codeAction/resolve` where the server advertises
it; otherwise run it with `workspace/executeCommand` and stage the
`workspace/applyEdit` requests it pushes back. Exactly one pushed edit set per
command is accepted.

The protocol lets a code action carry no edit at all, and both remaining routes
end in the same transactional applier, so an agent picking action 2 does not
have to know which shape it got. Resolve is preferred because it computes an
edit without running anything.

Two consequences are deliberate and visible in the output:

- A *preview* of a command-shaped action has to run the command, because the
  edits do not exist until it has. That is a side effect inside a preview, so
  it is warned about by name rather than hidden.
- More than one pushed edit set is refused rather than merged. Two edit sets
  computed against the same starting state cannot be composed without knowing
  which of them the second was written against, and guessing would turn a
  server's two safe edits into one wrong one. If a real server ever needs it,
  the fix is to teach `internal/edit` to stage a sequence, not to paper over it
  in the CLI.

## D10 — When `check` believes it has every diagnostic (PLAN §4, M5)

**Decision:** the pull model where the server advertises it; otherwise the
readiness gate *plus* one `publishDiagnostics` per opened file *plus* a settle
window, and exit 5 — not a clean report — when a file was never mentioned.

Diagnostics are the one answer LSP does not return from a request. A server
pushes `textDocument/publishDiagnostics` whenever it likes, for whichever files
it likes, and never says "that is all of them". So `check` has to decide, and
the decision is the command.

Where `textDocument/diagnostic` is advertised it is used, because a request that
returns beats any amount of inference about notifications. `--diagnostics
pull|push|auto` makes the choice inspectable, which matters for reproducing what
an editor sees and for debugging this decision.

In push mode a report is printed only when the readiness gate of PLAN §5.2 says
the workspace is loaded (the same gate and the same evidence as every other
command), *and* every file the command opened has been published about at least
once — an empty array counts — *and* no diagnostic has arrived for `--settle`.

The middle condition is the one that costs something. A file the server has
never mentioned is not a clean file; it is a file we know nothing about, and
reporting it as clean is PLAN §5.2's failure with the stakes raised: an agent
that trusts a silent `check` commits. Servers do publish per open document
(gopls and pyright both do, empty arrays included), so the cost is normally
nothing; when it is not, `--allow-silent` accepts the silence and says so in the
envelope in those words — *an assumption and not an answer*. The flag exists
because a tool that is unusable against a server with unusual publishing
behaviour would be a worse failure than the one it prevents, and because an
assumption a caller opted into and can see is not the same as one we made for
them.

The exit code is computed on the whole set before `--limit` truncates it.
Otherwise `check --limit 1` would be a way to make CI pass.

`check` is deliberately *not* capability-guarded, unlike every other command
with a method: `publishDiagnostics` is a notification any server may send
without advertising anything, and naming a method in the command table would
make `help` call the command unavailable on servers that answer it perfectly
well.

## D11 — `--symbol` ambiguity is refused, not resolved (PLAN §4, M5)

**Decision:** several matches is exit 2, with every candidate reported as a
`file:line:col` location and in `error.data`. Never a first match, never a
heuristic ranking.

`--symbol` exists because agents are bad at computing columns, which PLAN §4
calls the ergonomic win they actually need. The failure it must not introduce is
worse than the one it fixes: two symbols answering to one name are two different
pieces of code, and choosing between them is how an agent renames the wrong
`Handle`. A relevance ranking would make that choice invisible.

Exit 2 rather than exit 1, because nothing failed to be found — the *invocation*
failed to identify one thing, and the fix is on the command line. The candidates
are reported as locations so the retry is a copy-paste rather than a second
search.

The matching rule is written down (segment-boundary suffix of
`containerName.name`, exact whole-path matches winning over suffix matches,
identical locations deduped) because a caller has to be able to predict it. The
query sent to the server is the *last segment only*: servers differ on whether
`workspace/symbol` does substring, prefix or fuzzy matching, and every one of
them can find a symbol by its own name, so the path is applied on our side where
the rule is testable.

Resolution runs in its own short-lived session. The symbol names no file, so the
server to ask is the one that handles `--path`, and the file the answer points at
may be handled by a different one. That is a second server startup today, and two
pool lookups once the M3 daemon is wired up; the alternative — resolving inside
the query's session — would be wrong exactly when the workspace is polyglot.

## D12 — Batch mode emits one envelope per line (PLAN §8, M5)

**Decision:** one query per input line, one envelope per output line, streamed.
Not one envelope containing every result.

A single wrapping envelope would print nothing until the last query finished, and
a batch is exactly where the last query is the one that hangs on
rust-analyzer. Per-line envelopes stream: an agent has answer 1 while answer 7 is
still waiting, and a batch killed by a timeout leaves the answers it did produce,
each one complete and valid. It is also JSON-lines, which every consumer already
has a parser for.

Each line is byte-for-byte what the standalone command would have printed, with
one added `query` key naming the invocation and its own exit code — so a caller
develops against `lightspeed references …` and batches it later without
re-reading anything. Annotating beats nesting for the same reason: no consumer
needs a second shape. A query whose output is not an envelope (`--format
text|diff|sarif`) is wrapped in one with its bytes as a string, because a single
non-JSON line would break every consumer of every other line.

The input is a command line, tokenized with a shell's quoting rules and nothing
else — no globbing, substitution or pipelines. A JSON object per line would be
more precise but would make an agent learn a second calling convention for the
same commands; a line of batch input is a line it could have typed.

**Exit code:** the most severe outcome, ranked *ok < problems (1) < no server (3)
< not ready (5) < usage (2) < crash (4)*. The ranking is "how much should the
caller worry", not the numeric order: a crash means we do not know what happened,
a usage error means the caller's own input is wrong and every later line is
suspect, and not-ready outranks a real answer because unknown authority is the
one thing an agent must not treat as an answer. Reporting the first failure
instead would let a batch whose second line found problems hide a crash on its
tenth; reporting the last would depend on input order. `--summary` adds a final
counting envelope, opt-in, because the default contract is one envelope per query
and nothing else.

## D13 — `call_hierarchy` bounds, and what a row points at (PLAN §4, M5)

**Decision:** `--depth` 1 to 5 (default 1), a 500-entry budget, and a visited
set; every bound that bites is reported. A row points at the other symbol's
declaration, with the call site in `detail`.

A call graph has cycles, and a breadth of twenty at depth four is 160 000
requests, so the traversal needs bounds. `--depth` above 5 is a usage error
rather than a clamp: silently serving 5 would misreport what was searched. The
visited set is reset between the incoming and outgoing halves of `--direction
both`, so one half is not pruned by what the other showed.

A row points at the caller's (or callee's) *declaration* rather than at the call
site, because that is where the next command wants to go and because a
declaration is one place while a call site is often many. The sites are not lost:
the first is named in `detail` with a count of the rest. The label carries an
arrow and two spaces of indentation per level, so the flat, grep-compatible text
format still reads as a tree — and the result order is the traversal order, not
sorted, because indentation only means something in traversal order.

Several items from `prepareCallHierarchy` means the server could not tell which
symbol the position belongs to either. One hierarchy is more useful than none, so
the first is used — but the warning names the others, because an answer about a
symbol the caller did not mean has to be recognisable as one. That is weaker than
`--symbol`'s refusal (D11) deliberately: nothing is written here, and a wrong
read is recoverable in a way a wrong rename is not.

## D14 — `--no-daemon` is the same service in process, not the old code path (PLAN §3, M3)

**Decision:** a command always talks to a `daemon.Handle`. With the daemon it is
a client of the workspace's auto-spawned process; with `--no-daemon` (or
`LIGHTSPEED_NO_DAEMON=1`) it is a `daemon.Service` over a pool in the calling
process, shut down when the command ends. The CLI's own "start a subprocess, run
the handshake, close it" code is gone, not kept alongside.

The requirement that the two modes print byte-identical envelopes — the
timing text of a not-ready message excepted, which is a wall-clock duration and
an attempt count ("gave up after 30.2s and 41 attempt(s)") and so differs
between any two runs, in either mode — is easy to promise and easy to lose: with two implementations, every fix has to be made
twice, and the first one that is not is a bug that only shows up in the mode
nobody was using that day. With one implementation reached two ways, identity is
a consequence, and what has to be tested is the *transport*: that an error keeps
its code and exit status across the socket (a not-ready daemon must exit 5, not
crash), and that nothing depends on `errors.Is` reaching an error's cause, which
does not survive a socket. Both are covered — `TestDaemonWhileIndexingStillExitsNotReady`
and `TestModesAreByteIdentical` run the same command both ways and compare bytes,
and `LIGHTSPEED_TEST_VIA_DAEMON=1 go test ./internal/cli` runs every command test
in the suite through a real daemon.

Consequences, all deliberate:

- The in-process mode is no longer "exactly today's code", only today's
  behaviour. Where the wording of an error depended on who owned the process, the
  daemon's error carries enough to say the same thing (`daemon.Error.RPC` keeps a
  server's JSON-RPC code and message, because the CLI words those as
  `<method>: <server> returned error <code>: <message>`). Two messages did move:
  `raw` on a missing binary is `server_not_installed` (exit 3, as every other
  command reports it) instead of `no_server`, and a server that dies during the
  handshake reports the daemon's `spawn_failed` wording.
- `raw` routes like every other command, by `--path` (default the working
  directory), instead of the M0 hardcoded `gopls serve`. It still sends its
  request ungated and without a capability check — it is the escape hatch — which
  is why the daemon's raw path uses the connection and not `Session.Call`.
- The client resolves a path against the built-in table for its own purposes
  (exit 3 before any process starts, the multi-file consistency checks of `check`
  and `format`) and sends the daemon the absolute path plus language id
  (`router.Match.Path`). The daemon re-resolves it. Both used the same built-in
  table, so they agreed; once configuration could differ between the two they had
  to be made to agree deliberately, which is D17.
- The daemon's environment is that of the command that first started it: its
  `PATH` (and, until D18 removed it, its `LIGHTSPEED_SERVER_CMD`). A client's `LookPath` check for the server
  runs in the *client's* environment and reports "not installed" with the exact
  command that fixes it; the daemon's own launch failure is mapped to the same
  message. `daemon stop` is the remedy for a daemon that started with the wrong
  environment.

## D15 — A pooled session is shared, so nothing about it may be per-command (PLAN §3, M3)

**Decision:** the daemon boundary stays at the LSP level, and everything that
used to be a per-command property of a session is either sent with each request
or owned by the pool.

What that meant in practice:

- **One client-capability set, the union.** A session used to advertise extra
  capabilities to the command that needed them (resource operations and
  `workspace/applyEdit` for mutations, `publishDiagnostics` and `diagnostic` for
  `check`, `callHierarchy`). A pooled session is started by whichever command
  came first, so its handshake cannot depend on that: a `rename` that reached a
  session started by `references` would find the resource operations it needs
  never advertised. The pool advertises the union (`sessionCapabilities`). The
  cost is that a promise is now made to commands that do not use it; it is safe
  because the read-only commands never make the requests those promises are
  about, and because the pool — not the command — honours the server-to-client
  half.
- **The pool answers what the server pushes.** `workspace/applyEdit` is refused
  unless a request said `CollectEdits` (a code action that is a command,
  D9), and collecting requests are serialised per session so that two commands
  cannot be handed each other's edits. `publishDiagnostics` is recorded per file
  in every session. A recorded file is forgotten *before* new content for it is
  sent to the server, because the server may publish the moment it receives it;
  forgetting afterwards could discard the answer being waited for. Identical
  content is not forgotten — the server will not repeat itself — so a warm
  `check` on an unchanged tree returns at once, and a changed file still has to
  be reported on again (D10's rule 2, unweakened). A snapshot for `check` is
  limited to the files that command asked about: the session has published for
  every file any earlier command opened, about content it no longer has open, and
  handing those back would report a.go's old problems in a run about b.go — output
  a fresh server never produces, and so a break of D14's byte-identical rule.
- **Documents travel with their content.** The CLI builds its position `Mapper`
  from the bytes it read and sends the daemon those same bytes; if the daemon
  re-read the file, a write between the two reads would put every position one
  edit away from the text the server is looking at. The daemon answers with the
  version the *server* has, which is not the CLI's count once the session is warm,
  and a versioned edit is checked against that one.
- **Readiness options are per request.** `--timeout` and `--settle` are sent
  with each query (`client.Gate.With`), not fixed when the session is created:
  a session created for a command that said `--timeout 5s` must not hand the next
  one a five-second budget. The derived gate shares the session's progress tracker
  and start time, so a warm server is still a warm server to the no-progress
  grace.
- **A command closes what it opened, and says what it wrote.** Documents left
  open would make a warm server answer from the copy it was given, after an editor
  or `git checkout` had changed the file — PLAN §5.2's stale authority arriving
  by another road. So `session.close` sends `didClose` for what the command
  opened. And a command that writes (`--apply`) tells the server with
  `workspace/didChangeWatchedFiles`, because nothing else is watching the tree
  and a server that outlives the command would otherwise keep its old reading of
  files never opened. Both are best effort: the answer is already produced.
- **Documents are reference-counted per connection.** Two commands running at
  once on one workspace (an agent issuing parallel queries) that open the same
  file share one document, so the first to finish must not close it under the
  other. The service counts holders per pooled document; `didClose` goes out when
  the last one lets go. It also remembers what each connection holds, and gives
  it back when the connection ends, so a client that is killed mid-command does
  not leave its documents open in the warm server for good. Limits that remain:
  while two commands hold a file, a change of content by one is pushed to the
  server and the other's positions were built from the old text (the same
  window an editor has against a file changing under it); and in the in-process
  mode every caller is one connection, which is harmless because the pool dies
  with the process.

Not done, and worth knowing: a file changed *by something other than lightspeed*
between two queries is only noticed by the server for files a command opens (their
content is compared and re-sent each time). A file merely reached through a
reference is read by the server however it likes — its own watcher, or a stale
cache. Real servers differ; this needs measuring against them, which is the
integration-test gap that is already on the list.

*(Closed by D33: before answering any request the daemon now scans the workspace and
tells every live server what changed on disk with `workspace/didChangeWatchedFiles`. Measured
against real gopls, which did answer from its stale view without it.)*

## D16 — What `daemon status|stop|logs` say, and when (PLAN §4, M3)

**Decision:** "no daemon is running" is an *answer*, exit 0, not a failure.

`status` reports `running:false` with the workspace and socket it looked at;
`stop` reports `stopped:false`; `logs` reports `exists:false` with an empty list
and a warning. An agent that runs `daemon stop` to be sure of a clean slate has
succeeded when there is nothing to stop, and one that runs `daemon status` to
decide whether to warm a workspace needs the answer no, not an exit code that
also means "the tool broke". Exit 4 is kept for a daemon that is there and
misbehaves: a stop that was accepted and did not finish within `--timeout`.

`stop` returns when the daemon has *gone*, not when it has started going
(*amended by D24: "gone" was the socket, which the daemon removes first; it is
now the process*). The
daemon answers before it shuts down, and a caller that stops a daemon and
immediately starts one — or removes the directory — must not race the old one's
last seconds. `status` and `logs` name the workspace by `--path` (default `.`)
and resolve the root by walking up, exactly as the commands that started the
daemon did, so all three work from a subdirectory.

Status describes each pooled server as `starting`, `indexing` or `ready`. The
middle one is the state in which a query would be answered with an empty result
of unknown authority and lightspeed refuses with exit 5 (PLAN §5.2); showing it
next to the servers is how a user finds out why.

Idle exit is the daemon package's own (`--listen.timeout`, default
`daemon.DefaultListenTimeout`); `LIGHTSPEED_DAEMON_TIMEOUT` sets it for a daemon
a command starts, which is what makes it testable and lets someone who wants a
daemon that leaves sooner have one. The daemon's own log — including every
language server's stderr, which must never reach a client's stdout — is
`<runtime dir>/<workspace-hash>.log`, beside the socket. It is appended to and
never rotated; that is a known gap, not a decision.

`daemon serve` exists because the auto-spawn re-executes the binary as
`lightspeed daemon serve --listen <socket> --workspace <root>`. It is not
documented as something to type; it exits 0 when it goes idle and when it finds
another daemon already on the socket, because two clients racing to start one
daemon means one of them lost, not that it failed.

## D17 — Server resolution is per workspace, on both sides of the socket, and a stale daemon is restarted or refused (PLAN §6, M4)

**Decision:** every path is resolved against the layered definitions of *its
workspace root* — `.lightspeed.toml` over `servers.d/*.toml` over the generated
defaults, executables found by PATH sniffing — by the client and, separately, by
the daemon. A daemon started under different effective definitions than the
command resolved is restarted when the command is its only client, and refused
(`daemon_stale`, exit 2) when another is connected.

The workspace root is `daemon.Workspace(path)`, the same root that keys the
daemon's socket, not "the nearest `.lightspeed.toml`". One root has one config,
one daemon and one router, and a subdirectory reaches all three. A file
`.lightspeed.toml` is itself one of the markers that make a directory a root, so
putting one in a nested project gives that project its own daemon, which is what
a definition scoped to it needs. A configuration file that cannot be used is
exit 2 (`invalid_config`) and never skipped — an override that silently does
nothing is the failure PLAN §6 exists to prevent — with two exceptions that
exist to explain it: `servers` and `doctor` list the broken file and carry on,
and `daemon status|stop|logs` read no definitions at all, so the way out of a
bad configuration is never blocked by it.

The client and the daemon each load the layers, in their own environment,
rather than the client sending its definitions across the socket. D14 left this
open ("they will need to be made to agree deliberately"). Sending them would make
the daemon's table whatever the last client said and remove the reason a daemon
has one; loading twice makes disagreement possible, and detectable: the pool
carries a *config id*, a digest of the canonical JSON of the effective
definitions, which the daemon reports in its status and the client compares with
its own before it uses the daemon. The id covers the *configuration* and
deliberately nothing about the machine — where an executable is, whether mise is
present — because a daemon's environment is documented to be that of the command
that started it (D14), and a `PATH` change is what `daemon stop` is for.

What "stale" does is the decision. **Restart when idle, refuse when busy.** The
usual cause is someone editing `.lightspeed.toml` and running the next command; a
refusal to be cleared by hand every time would teach callers to stop the daemon
reflexively, and a warm session started under the old definition would answer
with a command or settings the configuration no longer names. But the daemon is
shared, and stopping it under a command that is mid-query turns that command's
answer into a crash — so a daemon with another client connected is left alone
and the command refuses, naming `lightspeed daemon stop` as the way out. A daemon
this command has just started, or restarted, that still disagrees is refused
too: restarting it again would only repeat itself. A daemon that predates config
tracking reports no id and is stale by this test, the safe direction. The
restart costs the warm servers, and says so on stderr.

Known limits: the check is one `status` round trip per command, and the window
between it and the command's first request is not closed — an edit in that window
is caught by the next command, not this one. A daemon with a client connected
that is *idle* still counts as busy.

The launcher starts each server from the executable the probe found
(`workspaceConfig.launcher`), not from command[0] looked up again on the
launching process's PATH: that is what makes a server that is reachable only
through `mise which` runnable at all, and the process that answers is the one
`servers` reported.

## D18 — `LIGHTSPEED_SERVER_CMD` is removed, not formalized (PLAN §8 M4, D4)

**Decision:** the variable is gone from production code. A test that needs the
fake language server writes a `servers.d` file that overrides `command`, which
is a user's own mechanism.

D4 promised "removed or formalized" once real resolution existed. Formalizing
would have kept a second, undocumented way to change what runs: an environment
variable that outranks every layer of PLAN §6, invisible to `servers`, honoured
in the *daemon's* environment and not the client's, and therefore able to make
`servers` say one thing while a query starts another. With the layers wired the
variable had no job the layers could not do, and a tool whose stated posture is
that server definitions are pure data should not carry an environment override of
them for its own tests.

The tests moved rather than lost coverage: `useServerCommand` writes a partial
override of `command` for each built-in server into a per-test
`LIGHTSPEED_CONFIG_DIR` (`internal/cli/serverenv_test.go`), which also makes
every command test an exercise of the user layer. The suite points that variable
at an empty directory and clears `LIGHTSPEED_OFFLINE` before it starts, so
neither a developer's own `servers.d` nor an exported switch decides what a
hermetic test resolves. `LIGHTSPEED_CONFIG_DIR` is serverdef's own, existing
override of the user layer's location, not a new hook.

## D19 — `install` plans, `--run` runs; `servers` and `doctor` exit 1 on problems; `--offline` guards downloads (PLAN §4, §6, M4)

**Decision:** `lightspeed install <name>` prints the exact mise command and
changes nothing; `--run` executes it. `--offline` (and `LIGHTSPEED_OFFLINE`)
refuses `--run`, and only that. `servers` exits 1 when a configuration file could
not be used; `doctor` exits 1 when any finding is an error.

PLAN §6 says nothing downloads implicitly. A command called `install` looks like
an explicit request, but it is the one an agent runs "to see what it does", and a
download that happens because of a curious invocation is an implicit one. So the
default is the plan — which is `serverdef`'s dry run, a question rather than a
refusal, and so available offline — and running is a flag whose name says it.
`--run` with the switch on is exit 3 (`offline`) with the command to run yourself
when online; with no mise it is exit 3 (`mise_unavailable`), because PLAN §9.5
has no fallback that downloads; a mise that ran and failed is exit 4
(`install_failed`), since the result is unknown, not empty. `--install-timeout`
(default 15 minutes) bounds mise, not `--timeout`: a server built from source
takes minutes, and the 30 seconds that bound a query would kill it half done.

`--offline` is global, accepted before the subcommand and after it, inherited by
a batch's queries, and ORed by serverdef with the environment, so an environment
that says offline cannot be argued out of it by leaving the flag off. It is
deliberately narrow. Nothing a query does touches the network from lightspeed's
side, so it does not stop queries; it does not sandbox the language servers,
which may fetch what they like (README, Security posture); and a missing-server
envelope under it says that `lightspeed install` will refuse, so the instruction
is never offered as if following it would work.

`servers` reports what is configured, which is a report even when a file in the
configuration is broken; the broken file is listed and the exit code is 1, so a
script that only checks exit codes notices it. `doctor` grades findings, and a
merely missing server is a warning (lightspeed works for the others); an
unclaimed path, an unusable file or a conflict is an error. `servers` adds one
thing to serverdef's report: for every key of each definition, the file that won
it (`key_origins`), because "which layer set this" is the question the
per-layer contribution lists only answer with some arithmetic. The three commands
start no language server and no daemon, and read no daemon state.

New codes for the taxonomy, in `internal/render`: `invalid_config`,
`config_conflict`, `no_such_server` (exit 2), `mise_unavailable` (3),
`install_failed` (4), and `daemon_stale` (2). serverdef's errors carry their own
`Code()` and `ExitCode()`; `serverdefFailure` passes both through unchanged and a
test asserts that render's table agrees with every one of them.


## D20 — The MCP server is the command table, not a second surface (PLAN §1, M6)

**Decision:** `lightspeed mcp` registers every command-table entry that declares
`MCP` tools, with an input schema derived from a declarative per-command
parameter spec, and runs each call through the command's own `Run` in process. A
new command becomes a tool by declaring its parameters in its table entry; there
is no MCP code to write for it.

*The spec checks the flags; it does not register them.* The parameter spec
(`internal/cli/params.go`) is the single description of a command's inputs that
the MCP layer reads. The alternative — making the spec register the CLI's flags
too, so that drift is impossible rather than detected — would have meant
rewriting how eleven commands declare and read their flags, which today are
local variables filled by `flag.FlagSet`, in a change whose purpose is a new
front end. So the drift is *detected* instead: `TestParamSpecMatchesCLIFlags`
asks each command for its flags the way a user does (`-h`) and fails when the
command has a flag the spec does not declare, the spec names one the command does
not have, or the types disagree. A flag that is deliberately not offered is
declared `CLIOnly` with the reason, so "forgot" and "decided against" cannot look
alike. `TestMCPEveryCommandIsExposedOrExcluded` does the same for whole commands:
each has exactly one of `MCP` and `NoMCP` (the reason), and the two lists are
spelled out a second time in the test, so an addition or a removal is a decision
somebody made twice.

*A call is an argument vector.* The arguments become the command line the same
command would have been typed with — flags first, positionals after `--` so that
no value can be read as a flag, `--format=json` always — and `Run` is called with
a buffer for stdout. The envelope on that buffer is the tool's result, byte for
byte what the CLI prints (`TestMCPReferencesMatchesCLI`), because it is the same
code, the same daemon and the same rendering; there is no second path to keep
in step (the reason D14 has one implementation reached two ways). Relative paths
are made absolute against the call's `workspace` (default the server's working
directory) *before* the command sees them, and a path flag that defaults to `.`
defaults to the workspace instead. `chdir` would have done it in one line and
been wrong: a process has one working directory and an agent issues calls
concurrently.

*Results.* Text content and `structuredContent` are the envelope; `isError` is
exactly `ok:false`. An empty authoritative answer (`ok:true`, exit 1) and a
`check` that found errors are answers, not failures of the tool, and so are not
errors — the exit code the CLI would have had is in the result's `_meta` under
`lightspeed/exit` for a caller that wants it. A bad argument, an unknown
parameter and a missing workspace are the same kind of result (a `usage` or
`no_such_file` envelope), never a JSON-RPC error: the model can read the first
and can do nothing with the second. A panic inside a command is caught and
reported the same way, so one bad call does not end the session.

*Each call gets its own `env`.* The server lives for a whole session, and D17's
staleness rule — a `.lightspeed.toml` edited between two commands is seen by the
second — is per command. A server-wide definition cache would break it, so there
is none; the cost is one read of a few small files per call.

*Mutation.* `rename`, `codeaction` and `format` preview unless `apply` is true,
and the dirty-worktree refusal (D8) is the command's own, so it applies
unchanged. `allow_dirty` is offered rather than withheld: an agent that is told
"refused, worktree dirty" needs a way to say it knows, and withholding it would
only move the decision to a shell. Whether a tool is read-only is *derived* — it
has no `apply` — rather than declared next to its parameters, so a tool that
gains one is not still marked `readOnlyHint` by omission.

*Exclusions, and why.* `batch` (a client already issues calls concurrently; it
exists to save process starts), `raw` (skips the capability guard and the
readiness gate that make an answer trustworthy), `help` (`tools/list` is the
surface), `mcp` (it is the server), and `install` (it can download and run an
installer; nothing does that as a side effect of an agent's call, PLAN §6). Of
`daemon`, only `status` is a tool as `daemon_status`: `stop` would let an agent
throw away the warm servers every later call depends on, and `logs` and `serve`
are for a human and for the auto-spawn. That `logs` is read-only and harmless is
true and is not enough to have put it on the list; adding it is one table entry.

*The SDK.* The dependency is `github.com/modelcontextprotocol/go-sdk` (v1.6.0),
the official one. The tools are registered with the low-level `Server.AddTool`
and a schema built at run time: the SDK's typed `AddTool` infers a schema from a
Go struct, which is exactly the per-tool code this design exists to avoid, and it
means the SDK does no argument validation — `toolSpec.argv` does it (types,
enums, integer bounds, required, unknown names). It is the first third-party
module in the tree, which D3 had put off. The MCP code is in `internal/cli`
rather than its own package because the table it reads is unexported; exporting
the table to keep the import graph tidy would have been the wrong trade.

*Amended by D25: a call is now cancelled by `notifications/cancelled`, by the
client hanging up and by a signal; the limit that follows describes the
original, and what is left of it is stated in D25.*

Known limits: a call cannot be cancelled once running — the commands bound
themselves with `timeout` rather than a context passed down, so a client that
gives up on a call leaves it running to its own deadline (an `apply` that has
started still finishes, which is the better failure); and only tools are served
— no resources, no prompts. `servers` returns the full report, which is large;
it is the same envelope as the CLI's and was not trimmed for the agent.


## D21 — Symbol ids: `path::Container.Name#kind`, recomputed and never guessed (PLAN §4, M7)

**Decision:** a symbol is named by `<workspace-relative path>::<Container.Name>#<kind>`,
with `~2`, `~3` for later duplicates in the file, built from
`textDocument/documentSymbol` in either of its shapes. Every command that takes
a location also takes `--id` (MCP `id`), and every command that lists symbols
puts the id in its JSON output. An id that no longer resolves is `stale_id`
(exit 1) naming the nearest candidates, never a guess.

The point is the round trip: an agent lists a file's symbols and, a call later,
wants one symbol's text or references. A byte column is the thing it is bad at
(PLAN §4), and a name alone is ambiguous (D11), so the id carries the file, the
qualified name and the kind — enough to be unique in nearly every file — and the
duplicates that remain are numbered.

*Built from the file each time, stored nowhere.* An id is recomputed from the
current outline whenever it is used, so there is no index to keep in step with
the tree and nothing for a stale one to disagree with: it resolves, or it does
not. The cost is one `documentSymbol` per file per command, which the warm
daemon makes cheap.

*The rules, so that a caller can predict one.* The path is relative to the
workspace root — `daemon.Workspace` of `--path` (default the working directory;
MCP: the call's `workspace`), the same root that keys the daemon (D17) — with
forward slashes. The name is the server's qualified name (containers joined by
dots), except that a Go receiver is spelled as its type: gopls lists a method as
`(*Stage).Apply`, and the id says `Stage.Apply`, which is also what a server
that reports the container separately produces, so two servers agree on the
method. The kind is the SymbolKind's name. Duplicates are told apart in *position*
order, not the server's answer order, and the first keeps the plain id: numbering
every duplicate from `~1` would change the id of a symbol nobody touched the
moment a second appeared. What that costs is that deleting an earlier duplicate
renumbers the later ones, so `~2` then names a different symbol; the hash that
`source` returns (D22) is how a caller notices.

*Both shapes.* A flat `SymbolInformation` answer has no hierarchy, so the tree
`outline` shows is inferred: a symbol belongs to the one its `containerName`
names when exactly one has that qualified name (a Go method is declared outside
its type, so its range says nothing), and otherwise to the smallest symbol whose
range strictly contains it. The ids are the same either way; a test feeds one
file both ways and compares them.

*A flat answer says where the declaration starts, not where the name is.* A
`SymbolInformation` has a location and no `selectionRange`, and real gopls answers
in that form whenever the client does not advertise
`hierarchicalDocumentSymbolSupport`; its range starts at the `func` keyword, column
1. An id resolved to that position asks the server about a keyword, and
`definition`, `hover`, `references` and `call_hierarchy` all find no identifier
(`references --id` came back `no identifier found`). So a flat symbol's name is
looked for in the declaration's own text (`pinpointNames`): the first whole-word
occurrence of the name outside a leading parenthesised group, which is what keeps
`func (s *Stage) Apply` from resolving to the receiver, converted back to UTF-16
through the Mapper. The alternative, advertising hierarchical support so that gopls
sends `selectionRange`, was not taken as the fix: it would not help a server that
only knows the flat shape, and it changes which symbols `symbols` lists for every
existing caller (struct fields become children). A name that is not in its
declaration keeps the declaration's start, and one warning says how many; that is
the honest answer, since the server said nothing else. `symbols` prints the same
name position, so its location can be fed back.

*A file outside the workspace has no ids.* An id's path is relative to the
workspace by definition, so a symbol in the standard library or another
checkout is listed without one, and without a warning, rather than with a path
that means something else from another directory.

*Refusals.* A malformed id is a usage error (exit 2), and one whose path is
absolute, climbs out with `..` or reaches outside through a symlink is
`outside_workspace` (exit 2): nothing is read for either. A stale one is exit 1,
like `not_found`, because it is an authoritative "not there" and the invocation
was well formed; it has its own code, `stale_id`, because an agent should
respond by outlining the file again and not by searching. Nearest candidates
are ranked: the same name of another kind or duplicate index, the same last
segment, one name containing the other, then near misses by edit distance; a
missing file lists the files of that name elsewhere, closest path first. The
same list is in the message and in `error.data.candidates`.

*Lists that do not come with an outline.* `workspace_symbol` and
`call_hierarchy` return symbols one at a time, and a `~N` depends on every
duplicate in the file, so their ids are found by outlining each distinct file
(at most 30, on the same server) and matching the symbol's name position. A result
that cannot be matched — beyond the cap, in a file another server handles, not in
the outline — has no id, and one warning says how many, and why. An id that might
be the wrong duplicate would be the guess this decision rules out.

Known limits: ids are only as stable as the server's symbol table — two servers
for one language may name the same thing differently; only Go receivers are
normalised; and only gopls 0.23 has been checked as a real server (`--id` on
`definition`, `references`, `hover`, `call_hierarchy`, over the CLI and `lightspeed
mcp`), by hand — the tests script the fake server with both shapes, the flat one with
a location that starts at the declaration keyword, as gopls sends it. The name search
is textual, so a symbol whose server-given name is not written in its declaration
(an anonymous or synthesised one) gets the declaration start.

## D22 — `outline`, `source`, `context`: what a slice of source is (PLAN §4, M7)

**Decision:** `outline` lists symbols as a tree with ids, signatures and line
ranges; `source` returns whole symbols by id or location, byte-exact, with an
explicit cap and hash; `context` adds the file's import block and the hover.

*A symbol's source is whole lines.* It is cut from the file by the symbol's
full `range` (not the name's `selectionRange`), from the start of its first line
to the end of its last, so a method's indentation survives and the text is
exactly what `sed -n` would print. The range is UTF-16 and is converted through
the vendored Mapper (PLAN §5.1); the slice is bytes, so CJK and emoji are
exact — tested with both in the comment, the body and a string, and with a
location whose byte and UTF-16 columns disagree.

*The doc comment is included*, because gopls' range starts at `func` and an
agent that asked for a function wants what documents it. Lines directly above the
declaration that open a comment or a decorator or attribute (`//`, `/*`, `*`,
`--`, `#` except the C preprocessor, `@`) are taken, and a blank line stops it.
This is a heuristic across languages and deliberately a short one; it is wrong
for a language whose comments open otherwise, and right for the ones agents most
often read.

*Caps are explicit.* `--context N` adds lines around; `--max-lines` cuts on a
line boundary and `--max-bytes` on a rune boundary, so a cap that falls inside a
CJK character or an emoji drops it instead of splitting it. `truncated` is always
in the answer, `total_lines` says how much there was, `line`/`end_line` describe
what was returned, and a cut is also a warning. The `hash` (sha256) is of the
symbol's own lines, comment included and context and caps excluded, so it is the
same however the call was made and changes only when the symbol does.

*A batch is answered as far as it can be.* `source` takes several ids or
locations. Targets that cannot be resolved — stale, malformed, a location inside
no symbol — go in `errors` with their codes and candidates while the rest are
returned, exit 1; if none resolves the envelope fails with the error of the one
target, or a `stale_id` listing all of them. A failure of the *server* ends the
command, though: after a not-ready or a crash nothing else in the batch could be
believed either, which is the same rule as everywhere else. A location names the
innermost symbol containing it; ids and locations mix in one call (an id has a
`::` and a `#kind` after it, which no location does).

*`context`* is the one call for understanding a symbol: its source, the file's
header and the server's hover. The header is everything above the file's first
declaration and its comment — the package clause and imports for every language
whose first declaration follows them — capped at 80 lines with `truncated`. It is
taken from the symbols, not from a list of `import`-like keywords per language,
which would be wrong somewhere; the price is that a file whose first symbol is a
constant above its imports has a short header. The hover is an extra: a server
that does not advertise it, or fails, costs a warning and not the answer.

*`outline`.* The signature is the server's own detail when it gives one and the
declaration as written otherwise: the symbol's first line, and more while a
bracket it opened is open (at most six lines and 240 bytes), cut at `{`. Several
files are one call, checked before any server is asked, and one session per
(server, workspace root) serves them. `--limit` caps symbols in document order
and reports the cut. Line numbers are the declaration's, so an outline's `line`
can be one or more after `source`'s, which includes the comment.

Known limits: the comment and header heuristics above; a symbol whose range the
server reports badly is sliced as reported; a range that begins mid-line is
widened to the line.

## D23 — `tree`, `repo_outline`, `file`: reading the workspace without a server (PLAN §4, M7)

**Decision:** `tree` and `repo_outline` list the workspace from git's own answer
when there is one and from a walk when there is not; `file` reads a line range
and is confined to the workspace. None starts a language server or a daemon.

*git's answer.* `git ls-files --cached --others --exclude-standard` is the
tracked files plus the untracked ones that are not ignored, so `.gitignore`,
`.git/info/exclude` and the user's global excludes apply without lightspeed
reimplementing any of them (D8 also shells out to git rather than vendoring a
reader). A tracked file deleted from the worktree is left out. Without git, or
where it will not answer, the tree is walked, skipping hidden and build
directories, capped at 200 000 files, and a warning says that `.gitignore` was not
applied — a walk is a guess at what the ignore file would say and is reported as
one. Paths are relative to the workspace root (D17), so a listing of a
subdirectory has paths that can be pasted into `outline`.

*Language and server.* The language is the router's id for the path, the server
the first definition the router resolves for it (D17's layers included), named
whether or not it is installed. `tree` caps at 500 files by default and reports
`total` and `truncated`; `--prefix` filters. `repo_outline` gives directories
down to `--depth` levels (default 2) with recursive counts and a language
breakdown, and the servers those languages resolve to with whether each runs
(`servers`' own probe) and, when it does not, the install command. Which server a
language resolves to is decided by one representative file per language; the
definitions claim files by language and glob, and resolving every file would be a
walk up the tree each.

*`file`.* `--start`/`--end` are 1-based and inclusive; an `--end` past the end is
the end, a `--start` past it is a usage error. It refuses a binary file (a NUL in
the first 8 KiB) and one over 16 MiB, and shares `source`'s caps and hash. It is
confined to the workspace — `outside_workspace`, exit 2 — by three checks that are
each needed: the path is compared lexically first, so that a path out of the
workspace is refused *whether or not it exists* and "no such file" cannot be used
to probe what is out there; then symlinks are resolved and compared again, since
a link inside the workspace can lead out of it; and the root is the workspace of
`--path`, so an MCP call is confined to its `workspace` and not to wherever the
server was started.

*`tree` and `repo_outline` are confined the same way.* They list whatever
directory they are given, so a `dir` of `..`, `/etc` or a symlink to somewhere
else would name every file there. The first version derived the workspace from
`dir` itself, which made the check compare a directory with a root computed from
that same directory: it could never fail, and `tree /etc` through MCP listed 1 790
files. The workspace is now the one `--path` (default the working directory; the
MCP `workspace`, or `path`) belongs to, never anything derived from `dir`, and
`dir` — default `--path` itself — must be inside it, checked lexically and again
after symlinks are resolved, before it is stat'ed, so that a missing directory
outside is `outside_workspace` and not "no such directory". Same code, same exit 2
as `file`. A symlink inside the workspace to a directory inside it is fine; the
link is judged by where it leads. A file symlink inside the tree that points out
is listed by name only — `tree` never reads file contents.

Known limits: `source <location>` and the position commands accept a location in
any file, as `hover` always did; only `file`, `tree`, `repo_outline` and ids are
confined, because those are the ones that read a path an agent can be talked into. A language server that
handles the file is the only thing between `source /elsewhere/f.go:1` and its
text.

## D24 — A daemon is waited for as a process, not as a socket (PLAN §3, M3)

**Decision:** `daemon stop`, the restart of a stale daemon and the test harness
return when the daemon *process* has exited. `spawnDaemon` reaps the child it
starts instead of releasing it, and `daemon.WaitChildren` and `daemon.WaitExit`
wait for processes.

*The flake, and its cause.* Under a full parallel `go test ./...`,
`TestCheckOnAWarmServerReportsOnlyWhatItWasAskedAbout` failed once with
`TempDir RemoveAll cleanup: unlinkat …/003: directory not empty`, and the same
error reproduced on demand (ten of twelve parallel runs of the daemon tests) with
the leftover being a `spawns.log` containing one line, `exit`: a file that had been
deleted and then written again. `t.TempDir` removes its whole tree in one cleanup
that runs last, and by then the test's cleanup had stopped the daemon — but the
daemon unpublishes its socket *first* on shutdown (so that a client arriving
during it spawns a fresh one; `Server.Shutdown`), and only then drains requests and
shuts its language servers down. `retireDaemons` waited for the socket to vanish,
which is the start of the daemon's exit and not the end of it, and the fake
language server the daemon still held was writing its `exit` line into a directory
`RemoveAll` was emptying. Every daemon-spawning test had the same window; this one
lost the race most often because it runs four in-process commands before it ends.

*The fix is at the cause, not at the test.* `daemon stop` documents that it
returns when the daemon has gone (D16) and only waited for the socket, so it broke
its own contract for any caller that then removed a directory or started the next
daemon. It now reads the daemon's pid before asking it to stop and waits for the
process. The restart of a stale daemon (D17) waits the same way, so the old
daemon's servers and the new one's do not overlap. The pid is polled with signal 0,
and a zombie — a daemon whose parent is a process that does not reap — counts as
gone where `/proc` says so.

*Reaping.* The spawn used to `Release` the child: never waited for, so a zombie
for as long as its parent lived. For a command that is a moment; for `lightspeed
mcp`, which lives for a whole agent session and outlives every idle-exiting daemon
it starts, it was a leak. The child is now waited for in a goroutine and recorded
until it exits, which is also what lets a process that owns the daemons it spawned
— the tests, and a long-lived server shutting down — wait for exactly those:
`WaitChildren`. `retireDaemons` stops every daemon under the suite's runtime
directory and then calls it, `daemonEnv`'s cleanup and the daemon package's
`socketDir` do the same, and the suite waits once more before it exits. There are
no retries and no sleeps standing in for this; a daemon that outlives its test is a
failure with its pid.

Proof: the same twelve parallel runs, eight iterations each, that failed ten
times, pass; `TestDaemonStatusStopLogs` now asserts, without polling, that
`daemon stop` returned with the process gone and its language server's `exit`
logged; `go test ./... -count=5` and `-race` are clean.

*Why `-race` was ten times slower.* Every command in `internal/cli` waits for the
language server it started to exit, and the race runtime sleeps `atexit_sleep_ms`
(1s) before a race-built process exits. The fake server and the daemon are the test
binary, so each session cost a second of wall time and no CPU: `go test -race
./internal/cli` took 303s against 19s, and `-count=5` overran go test's 10-minute
default. The `TestMain`s of `internal/cli` and `internal/daemon` now add
`atexit_sleep_ms=0` to `GORACE` for their children (the runtime read it at its own
start, so the test process is unaffected and races are still reported). The waiting
stayed; only the sleep before exit went. `-race -count=5 ./...` takes about two
minutes.

## D25 — A call's context is the request's, and the server's own lifetime ends it (PLAN §1, M6)

**Decision:** an MCP call runs under the request's context and under the
server's own lifetime, and both are threaded to every request the command makes.
Cancelling the call, the client hanging up and a signal all end it.

*Threading.* `env` has a `ctx` (nil is `context.Background`, which is what a
command line has), `session` remembers it as its `base`, and everything that was
rooted at `context.Background()` — the connect, the gated query, the readiness
wait, `check`'s poll loop (a `time.Sleep`) — is rooted at it. What is left on
`Background` is deliberate: `git status` is bounded by its own ten seconds. The
polite `didClose` of the documents a command opened is *skipped* for a cancelled
command, which the first version of this got wrong: through the daemon it queues
behind the request being abandoned (a connection's requests are answered in
order), so the call did not return until that request did. Nothing is lost by
skipping it — the daemon gives a connection's documents back when the connection
ends (D15), and an in-process pool is shut down whole. Running the suite through a
daemon (`LIGHTSPEED_TEST_VIA_DAEMON=1`) is what found this.

*What the SDK does.* Only `notifications/cancelled` cancels a handler's context in
go-sdk v1.6.0. When the client's end of the pipe closes, the connection waits for
the calls in flight to finish — so a call left to its `--timeout` would hold
`lightspeed mcp`, and the language server it owns, open for that long, and so
would a SIGTERM. So `mcpServer.life` is cancelled by a signal or by end of input
(the transport's reader reports it) and every call also runs under it.

*Tests.* `TestMCPCancelledCallStops` cancels a call on a workspace that never
finishes indexing and sees it stop in under two seconds of a five-second timeout;
`TestMCPHangUpCancelsARunningCall` does the same through the real stdio command
and its exit; `TestMCPCallRunsUnderTheRequestContext` checks the result is a
`cancelled` envelope. Each was checked to fail with its part of the fix removed.

Known limits, and they are the daemon's: the daemon serves a connection's requests
on its read loop, which cannot notice that the client went away while a handler is
blocked, so an abandoned request keeps running *there* until its own gate deadline.
In process (`--no-daemon`) it stops for real; over the daemon the *call* stops at
once and the daemon finishes its work for nobody. Fixing that means serving a
connection's requests concurrently, which D15 deferred. An `apply` that has begun
committing still finishes, as it should.

*The parameter spec is also checked against positionals now* (D20's known gap):
`parseFlagsRange` reports the positional count it is about to enforce to an
`env.onArity` seam that only tests set, and `TestParamSpecMatchesCLIFlags` compares
it — minimum, maximum, a list being last — and the usage line's names, order,
optionality and variadic-ness with the spec's `Required` and `Name`. Making a
required positional optional in a spec now fails the test.

## D26 — `search_text`: a live full-text search that needs no server (PLAN §4, M8)

**Decision:** `search_text <query>` reads the workspace's files from the working
tree at the moment of the call and reports `file:line:col` and the line. No index,
so nothing to go stale; no language server and no daemon, so it answers on a
machine where none is installed. It exists because a symbol search cannot find a
string, a comment, a config value or a name that is not a declaration, and a
text search is what an agent reaches for then.

*Which files.* Exactly `tree`'s (D23): `git ls-files --cached --others
--exclude-standard`, or the walk with its warning outside a repository. The
enumeration is shared code (`listWorkspaceFiles`), not a second copy, so
`.gitignore`, `.git/info/exclude` and global excludes apply the same way. Skipped
and *said*, never silent: a file with a NUL byte in its first 8 KiB (the rule
`file` uses) is binary; a file over `--max-file-bytes` (default 1 MiB — a search,
unlike `file`, has no use for a generated bundle or a data dump, and reading one
costs every call) is too large; a non-regular file (a FIFO would block the read)
and an unreadable one are counted too. Each kind is one warning naming the first
five files, and `files_skipped` counts them. A file that is ignored is not
"skipped": it is not in the set, as it is not in `tree`.

*What matches.* The query is a case-insensitive substring by default;
`--case-sensitive`, `--word` and `--regex` change that independently. `--regex` is Go's
RE2, so a hostile or careless pattern (`(a+)+$` over a 40 000-byte line, which is a
test) cannot backtrack and cost a call its timeout; an invalid pattern is a usage
error (exit 2) before any file is read. Matching is per line, on the line without its
terminator (a CRLF file's `\r` is not part of it). `--word` is not `\b`, which is
ASCII-only: a match counts as a word when the characters next to it are not letters,
digits (any script) or `_`. The cost is that CJK text, which has no spaces, is a
"word" only where something else separates it; that is stated, not fixed with a
segmenter. The rule is applied to each match of the line, so a match that fails it
does not hide a later one, though it can hide an overlapping one.

*One result per matching line, not per occurrence.* `col` is the 1-based **byte** column
of the first match on the line (PLAN §4's convention, the same as `references`), in
the line as it is on disk even when the text printed is clipped; `hits` says when there
were more. The alternative, a result per occurrence, would repeat the same line's text
for every one, which is the token waste this command is meant to avoid. `total` counts
lines.

*Output discipline.* `--limit` (default 50) stops the listing and still reports
`total`, `truncated:true` and a warning: every file is searched to the end, because an
agent that is told "50 of at least 50" has learned nothing about how broad its query
was. `--limit 0` is no limit (the flag's value is inspected, so an explicit 0 is not the
default). Each worker keeps only its file's first `limit` matches, so memory follows the
limit and not the number of matches. Results are merged in path, line order, so output
is the same whatever the parallelism (`GOMAXPROCS` workers over the files). A line over
240 bytes is cut to a window of about that size around the match, on rune boundaries,
with `[+NB]…` before and `…[+NB]` after saying how many bytes were dropped, and
`clipped:true`; the column stays the file's. `--context N` gives `before` and `after`
lines, clipped the same way. The text form is grep's (`file:line:col: text`, context
as `file-line- text`, `--` between groups that are not adjacent, and a context line
shared by two matches printed once). No match is `ok:true`, an empty list and exit 1:
an authoritative empty answer, as for `references` and grep.

*Globs.* `--glob` reuses the router's (`**`, `{a,b}`, `[a-z]`), repeatable, with a
leading `!` to exclude: a file must match some include when there are any and no
exclude. Two conveniences ripgrep users expect are added on top of the router's
rules, which anchor every pattern: one without a `/` matches at any depth (`*.go` is
`**/*.go`), and any pattern also matches everything below a directory of that name
(`internal/cli` is also `internal/cli/**`). A malformed glob is a usage error.

*Confinement is `tree`'s, and has one more edge.* `--path` scopes the search to a
directory or a single file, and the workspace it must be inside is `--root` (default `.`;
MCP: the call's `workspace`) — never derived from `--path`, which was D23's bug. `..`,
an absolute path and a symlink out are `outside_workspace` (exit 2), whether or not
the path exists. Where `tree` only lists names, this command *reads contents*, so a file
symlink in the tree that points out of the workspace is checked per file and skipped with
a warning: `tree` was allowed to list it, and `search_text` would have printed its text.
The flag is `--root` and not a second meaning for `--path` because the MCP call needs the
workspace as a parameter of its own, and `--path` had to be the scope the goal names.

*Warnings in text.* `--format text` prints the envelope's warnings after the matches as
`# ` notice lines (the convention of every text format, which `git apply` and a
grep-style consumer skip), so skipped files, a missing `.gitignore` and truncation are
never silent there; the truncation notice is the warning's, not a second footer.

*`--with-symbol`.* Each listed match gets `symbol`, the id (D21) of the innermost
symbol whose range contains it, from the outline of its file — the same
`sessionSet.load` and `enclosingSymbol` that `source <loc>` uses, so the ids are the
same ones — with the match's UTF-16 position computed from the line, since the ranges are
UTF-16. It is best effort and bounded: only the listed matches (at most `limit`) are
looked up, one outline per distinct file; a file no server handles or one whose server
fails leaves its matches without an id and adds one warning naming the extension and the
reason; the first failure for an extension stops further attempts for it; and the wait for
a server is capped at 10 seconds however large `--timeout` is, because a workspace that
is still indexing must not hold a text search hostage to an extra. A match in a doc
comment above a function is not "inside" it under the servers' ranges (gopls' starts at
`func`), so it has no id. Without the flag no server is contacted.

*MCP.* A tool with the CLI's envelope and no MCP code beyond its parameter spec, which
needed one generalisation: a repeatable flag (`glob`, a list) is sent as one
`--glob=…` per item. `limit` is declared on the tool itself, not through `commonParams`,
because the common flag's "0 means no limit" text is wrong for the default of 50.

Known limits: no index means a large repository is read on every call (this repo's 247
files, 2 MB, take about 35 ms with the file cache warm; the cost is the read and the
regexp, and it scales with the tree, not with the results); `--word` for languages
without spaces; a match in a file that changes during the search is found or not as
the read went; an UTF-16 column for `--with-symbol` is computed from the bytes on disk,
which is what the server was sent.

## D27 — A file on PATH is not a server that runs: mise shims and the start probe (PLAN §6, M4)

**Decision:** PATH sniffing verifies a mise shim before believing it, and `servers`
and `doctor` start what they sniffed before reporting it installed.

*The failure.* On the machine this was found on, `gopls` on PATH was
`~/.local/share/mise/shims/gopls`, gopls 0.23.0 was installed in mise, and no
version was active for the directory, so running the shim printed `mise ERROR No
version is set for shim: gopls` and exited. `probeBinary` asked only whether the
file exists and is executable, which a shim is: `servers` and `doctor` said
`installed`, severity ok, and every query failed on spawn.

*A shim is recognised, then asked.* A shim is a file in a directory called `shims`
that is mise's (under a `mise` directory, under `$MISE_DATA_DIR`, or a link to the
mise executable — not another manager's `shims`). It is verified with
`mise -C <workspace> which <binary>`: mise is the shim's owner and this is what the
shim would resolve to, from the directory the server is run in (the workspace
root, which is also the daemon's server working directory), without running the
tool. Resolves: unchanged, the shim is the server. It does not: the tool is taken
from the definition's `install.mise` and `mise ls --json --installed <tool>` says
which versions are on disk. One is chosen (the definition's pin if installed — `v`
prefix ignored, mise lists `0.23.0` for a pin of `v0.23.0` — else the highest) and
`mise which --tool <tool>@<version> <binary>` says where its binary is; that binary
is launched (`Source: mise`) and `Binary.Note` says why, in `servers`, `doctor`
(severity **warn**: it runs, a shell would not) and the text output, with the
`mise use -g <tool>@<version>` that makes it the default. Nothing installed, no
`install.mise`, or mise not answering: the binary is **unusable** — `Source:
unusable`, not runnable, doctor severity **error**, and a query exits 3 with
`mise use -g <tool>@<version>` for the installed-but-unlaunchable case or the
definition's install command when nothing is installed (`error.data.install`, and
the message says `run:` for a repair the probe chose and `install it with:` for the
definition's). All of it runs with `MISE_OFFLINE=1` and
`MISE_NOT_FOUND_AUTO_INSTALL=0`; nothing downloads, and `mise use` remains the
user's to run. Without mise there is nobody to ask; the hot path keeps the shim
and the start probe below still catches it.

*The generic rule: a start probe.* `Resolution.StartProbe` runs each runnable
binary as `<binary> --version` in the workspace directory, two seconds at most,
stdin empty, in parallel, and is called by `Servers` and `Doctor`. It is not in
`Probe`/`ProbeServer`, which run before every query: a query that starts the server
is itself the probe there, and paying a process per query for a check is not worth
it (a failed spawn now reports its exit status and stderr). What counts as failure is narrow on purpose,
because `--version` is not a convention servers follow — `gopls --version` is
`flag provided but not defined`, exit 2 — and a probe that failed every server that
rejects a flag would report working servers broken, which is the mistake this
decision exists to remove in the other direction. A process that ran is `started`
whatever it exited with, and so is one still running at the deadline. Failure is:
the exec failing (missing, not executable, wrong format), exit 126 or 127 (a
wrapper's "cannot execute" / "not found"), or output that says `mise ERROR`, `no
version is set for shim` or `error while loading shared libraries`. The outcome is
`Binary.StartProbe` (`ok`, `started`, `failed`); a failure makes the binary not
runnable with `cannot be started: <first line>`. The seam is `Options.Start`, next
to `Options.Run`, so tests never run a fake binary unless they mean to.

Known limits: mise is asked in the workspace root, so a subdirectory with its own
`mise.toml` that differs from the root's is not seen; the tool for the fallback
comes from the definition's `install.mise`, so a definition without one (a
user-written server) gets an unusable shim with no fix rather than a guess; only
mise's shims are recognised; the probe cannot tell a server that starts but cannot
initialise, which is what a spawn's error is for.

Proof: `internal/serverdef/shim_test.go` (recognition, the three outcomes with the
workspace passed to mise, numeric version choice, the probe's classification and
its production runner against real scripts including a hang) and
`internal/cli/shim_test.go` (the whole path through `definition`, `servers` and
`doctor` on a PATH with a shim and a fake mise, and a non-shim binary that cannot
load). Each fails with the shim check removed. Checked by hand against the real
machine's mise 2026.9.7: `mise which gopls` fails with "not currently active", and
`mise which --tool go:golang.org/x/tools/gopls@0.23.0 gopls` and
`mise ls --json --installed <tool>` answer offline.

## D28 — A server that dies while it starts says why: exit status and stderr tail (PLAN §5.4, M8)

**Decision:** when a language server's process ends, or its handshake fails, during startup,
the `spawn_failed` envelope carries how the process ended and what it last wrote to stderr —
a one-line summary in `error.message`, the detail in `error.data` — in the daemon and with
`--no-daemon` alike.

*The failure that motivated it.* Here `gopls` on PATH was a mise shim for a tool with no
version set. The shim printed `mise ERROR No version is set for shim: gopls` and exited, and
the envelope said only `server "gopls": initialize failed: initialize: jsonrpc: connection
closed`. The reason was in the daemon's log, which a caller of `--no-daemon` or of the MCP
tool does not have and an agent does not think to read. "Connection closed" is what every
way of dying looks like from the client end; the process knows the rest.

*What is captured.* `client.StartCommand` keeps the last 2 KiB of the child's stderr
(`client.StderrTailBytes`) in a ring while still forwarding every byte to the writer it was
given — the daemon's log, or the terminal — so nothing that used to be logged is lost.
The tail starts on a rune boundary and, when it was cut, after the first newline so it does
not open mid-line. `Server.Report` says, once `Wait` has returned, whether the process
*exited on its own* (its `exit status 42`, `signal: …`, exit code) or was killed by `Wait`
after it hung: a server that was still running when the handshake timed out has no exit
status worth reporting — the kill is ours — but its stderr, which is usually where a hung
server said what it was waiting for, is still included. The pool asks for the report after
it has reaped the process (`Instance.Report`; a launcher with no process leaves it nil).

*Where it goes.* `error.message`: the original text plus `(the server exited: exit status
42); its stderr ended: <last non-blank line, at most 200 runes>`. `error.data`: `server`,
`command`, `exit_status`, `exit_code` (only when it exited), `stderr_tail` and
`stderr_truncated`. The data is raw JSON in `daemon.Error.Data` in both modes, so the
envelope is the same bytes in-process and across the socket
(`TestStartupDeathEnvelopeIsTheSameInBothModes`); `render.FailError` copies any error's
`ErrorDetails()` into `error.data`, because the daemon cannot import `render` to build a
`CodedError` (D7).

*Not done.* A server that starts and answers `initialize` and then dies is a `server_crash`
of a later request, not a start failure, and gets no report; and the process's stderr is
not attached to a `not_ready`. `spawn_failed` is also the code for a `cmd.Start` that fails
for a reason other than "not found", which has no process and so no tail.

Proof: `TestStartupDeathIsReportedWithExitStatusAndStderr` (both modes, through a shell
script that imitates the shim), `TestStartupDeathStderrTailIsBounded` (400 lines of log →
the last whole lines, under 2 KiB, `stderr_truncated`), and in `internal/client` the report
for an exit, for a server `Wait` killed, and the tail's line-boundary cut. Each fails without
the change: the first two got `initialize: jsonrpc: connection closed` and no data.

## D29 — A daemon is another program if its build differs: replace it, and go when the binary goes (PLAN §3, M8)

**Decision:** a client compares its own build identity with the daemon's and, when they
differ, stops that daemon and starts one from its own executable, with a warning in the
envelope; a daemon whose executable has been rebuilt, reinstalled or deleted exits as soon as
no client is connected, instead of lingering for its whole listen timeout.

*The failure that motivated it.* A daemon left running by a since-rebuilt temporary binary
(`/tmp/…/rv/lightspeed`) silently answered the commands of a newer `lightspeed`. Nothing
compared the two; D17 compares *configuration*, and the two builds had the same one. A
protocol change would have been worse than a behaviour change: two versions of the wire
format talking to each other.

*The identity* (`daemon.Build`): the executable's **size and modification time**, read once
when the process starts, plus `daemon.ProtocolVersion` (1). Two other things were considered
and not taken. The path is *reported* (`daemon status` shows it; the replacement warning
names it) and **not compared**, because one binary reached through a symlink or a hard link
is one build, and two copies of one build at two paths would otherwise replace each other's
daemon on every command. A content hash of the binary is exact and costs a read of tens of
megabytes on every start of a tool that is supposed to start at once; a rebuilt or
reinstalled binary has a new mtime whatever its name, and a `cp -p` of the same build is the
same build. An embedded VCS revision (`debug.ReadBuildInfo`) is *also* reported, for a human,
but it is empty for `go build` outside a checkout and equal for two builds of one dirty tree,
so it decides nothing. What the mtime scheme gets wrong: a build whose file was restored with
`touch -r` looks like the old one. The daemon reports its identity in `Status` and
`Handshake`; `daemon status` prints it as `build`, and as `stale_build` (the reason) when the
next command would replace it.

*When it is checked.* `daemon.Open` asks the daemon for its status — the same call D17's
configuration check makes, so a command that used to make one round trip still makes one —
and compares it with `SelfBuild()`, unless the call is `--no-daemon` (nothing to meet), the
client just started this daemon itself (`Client.Spawned`: it is this executable by
construction), or the caller may not start one (`Options.NoSpawn`: `daemon status` and `stop`
must be able to see and stop the stale daemon, not replace it). A daemon that predates the
identity reports none and so differs — the safe direction, as in D17. A different protocol
version is a difference like any other.

*Idle is replaced, busy is refused* — D17's rule again, for its reason: the daemon is shared,
and stopping it under a command that is mid-query turns that command's answer into a crash.
So a stale daemon with no other client is stopped (waiting, as in D24, for the *process*),
its replacement started from this executable, and a warning added to the command's envelope:
`replaced the daemon for this workspace (pid N), which was a different lightspeed executable
(/path, modified …); warm language servers were discarded`. One with another command connected is
left alone and the command fails with `daemon_stale` (exit 2) naming the other build and
`lightspeed daemon stop`. The cost, stated: two builds in use at once — an installed binary
and a checkout's, or an MCP server and a shell — take turns replacing each other's daemon and
its warm servers, and each replacement is reported, so it is loud rather than silent.

*The warning.* Commands assemble their own warnings and there are dozens of them, so the
note joins the output where they all end up: `env.addNote` wraps `env.stdout`, and the first
write, if it is a JSON envelope, gets the note prepended to `warnings` (the other fields are
passed through as raw JSON, so nothing else in the bytes changes); output that is not an
envelope — text, diff, SARIF — stays its format and the note goes to stderr as `lightspeed:
warning: …`. A batch line and an MCP call each have an `env` of their own, so each gets its
own note.

*A daemon that outlives its binary.* The server asks `Build.Current()` — is the file at my
path still the file I started from (same inode, size and mtime), and does it exist at all —
every 10 s (`daemon.DefaultExecutableCheck`; `LIGHTSPEED_DAEMON_EXECHECK` shortens it for a
test) and again whenever its last client leaves, and returns `ErrExecutableReplaced` (exit 0,
like an idle timeout) when the answer is no and nobody is connected. A connected client is
never cut off for it. The daemon's own identity is read at start, not lazily: after the
binary is replaced, a later stat could not say what the running one was.

Proof, with a real second build — a copy of the test binary with another mtime, started as
`daemon serve`: `TestDaemonFromAnotherBuildIsReplacedWithAWarning` (warning in the
envelope, old process gone, new one is this executable, second command quiet and the same
daemon), `…NoteInTextFormatGoesToStderr`, `…IsNotReplacedUnderAConnectedCommand` (refused
with the way out, replaced once the other client leaves), `TestNoDaemonIgnoresAForeignDaemon`,
`TestDaemonLeavesWhenItsExecutableIsGoneAndItIsIdle` (deleted binary: stays while a client is
connected, exits 0 within seconds of its leaving with a ten-minute listen timeout), and in
`internal/daemon` the `Differs` table, the handshake/status carrying the build and the two
server tests. Each of the four cli tests fails with its part removed (build check off /
executable check off).

Known limits: the check is on start and on a timer, not on every request, so a daemon
replaced *while* a client is connected keeps serving that client until it leaves; a `PATH` or
toolchain change is still not a difference (D17); and Windows is not addressed, as for the rest
of the daemon (`sys_other.go`).

## D30 — Paths in output are relative to the workspace root; `--absolute` opts out (PLAN §4, M8)

**Decision:** every command that prints file paths — `definition`, `references`,
`implementation`, `hover`, `symbols`, `workspace_symbol`, `call_hierarchy`,
`codeaction`, and (as they already were) `check`, `rename` and `format` — reports a file
inside the workspace relative to the workspace root, in text and in JSON, and a file
outside it by its absolute path. `--absolute` (MCP `absolute`) restores absolute paths
everywhere.

*Why.* An absolute path on every result line is the same prefix, repeated, in every
answer an agent pays for. `tree`, `outline`, `source` and the ids were already
workspace-relative, and `check` and the edit previews already were, so the position
commands were the odd ones out. The text format and JSON now agree, which is what the
old split made impossible to promise. The root is the one that keys the daemon (D17):
`match.Root` of the session that answered.

*The root is said, not assumed.* A relative path is only useful if the reader knows
what it is relative to. JSON says: the payload of the results, diagnostics and changes
has a `root` (the workspace root, absolute) when the paths in it are relative, and
omits it under `--absolute`. Text has no envelope and a line of it on every answer is a
cost every caller pays, so it says so only when it has to: when the command did not run
in the root (symlinks resolved), a trailing `# paths are relative to <root>` notice — the
same `#` line the format already uses for truncation and warnings, which a grep-style
consumer skips. From the root itself, the paths are valid as they stand for `cd <root> &&
git apply`, an editor or a shell, and nothing is added. Diffs never carry the notice
(they are for `git apply`); their labels are root-relative, as before, and `--absolute`
makes them absolute (`git apply` then needs `-p0` or the paths as they are).

*What this changes.* The JSON `path` of a position command's results was absolute and is
relative now, with `data.root` beside it. That is a change to a `version:1` envelope's
values, made deliberately because the tokens saved are the point and because the field
that says how to resolve it is there; a caller that needs the old shape passes
`--absolute`. A batch line takes `--absolute` like any command line, so batch and CLI
agree; the MCP tools answer in JSON, so they are relative and offer `absolute`.

Known limits: the notice compares the working directory with the root after resolving
symlinks and does not know about a subdirectory that is *inside* the root and would make
a relative path from it valid in a different sense; it says the root and lets the reader
decide. `outline`, `tree`, `source` and `file` never had absolute paths for files inside
the workspace and are unchanged.

## D31 — `source`, `context` and `outline`: the leftovers of the first review (PLAN §4, M7)

**Decision:** four corrections to D22, none of which changes what a symbol's text is.

*(a) The doc comment is found by reading the block, not each line.* `isLeadingLine`
looked at one line at a time, so a `/* ... */` whose last line was text (` bar */`) or
whose body had no leading `*` was not recognised as a comment and the symbol lost its
documentation. A line that ends in `*/` is now taken as the end of a block, and the block
is walked up to the line that opens it with `/*` (at most 400 lines, a blank line inside
allowed, since a licence has them). Code that merely ends in a comment (`x := 1 /* c */`),
a `*/` with no opener and a `*/` that closes an earlier comment are not blocks. CRLF files
were already handled — a line's terminator is trimmed with its `\r` — and are now tested:
the source of a CRLF file is its bytes, `\r\n` included, and the hash is of them.

*(b) `--id` on `source` and `context`, and `location` and `symbol` on `context`.* Every
other command that takes a position takes `--id`; these two were the exceptions, and an
agent that had an id in a variable had to know which spelling each command wanted. `source`
takes ids and locations as arguments and `--id` as one more (MCP: `id` beside `ids`; at
least one is required). `context` names its symbol exactly one way — an argument (an id or a
location, as before), `--id` or `--symbol` (resolved with workspace/symbol like every
command, an ambiguous name an error listing the candidates) — and more or fewer is a usage
error. The MCP `context` tool's `id` was a positional; it is a flag now and `location` is the
positional, so a caller that sent `{"id": ...}` is unaffected.

*(c) A missing file in `outline` is that file's error.* `outline a.go missing.go` used to
fail as a whole with `no_such_file`, which cost the caller `a.go` for a typo. Each file now
has its own entry, in the order given, and a file that cannot be outlined for its own
reason — it does not exist, is a directory, no server handles it, the server is not
installed or cannot outline — carries an `error` (`target`, `code`, `message`) and no
symbols, is named in the warnings, and makes the exit status 1. A failure of the *server*
(not ready, crash, timeout) still ends the command, as in `source`: nothing after it could
be believed. If no file was outlined at all the command fails with the first file's error
and code, with every file's in `error.data.errors` when there were several, so one missing
file is exactly what it was.

*(d) `line`/`end_line` describe the symbol, and the slice has its own fields.* They used to
describe `source` — including `--context` lines and stopping where a cap cut — so a
truncated symbol looked like a shorter one and the warning read "2 of 2 lines returned"
(the context lines counted as the symbol's). `line` and `end_line` are now the whole symbol,
doc comment included, whatever was asked for around it and however it was capped;
`returned_lines` and `returned_bytes` measure `source`; `source_line` says where `source`
starts when `--context` put lines before it; `truncated` and `total_lines` are as they
were. The warning counts the symbol's own lines that came back ("2 of its 4 lines returned
(67 bytes)"), and a cap that only took `--context` lines says the symbol is complete. The
text header shows `[cut: 2 of 4 lines returned]`. `file` keeps its own `start_line`/
`end_line`, which are the requested range and stop where a cap cut, because a range is what
that command is asked for.

Proof: `TestLeadingComment` (fourteen shapes), `TestSourceTakesBlockCommentsAndCRLF`,
`TestSourceAndContextTakeID`, `TestMCPSourceTakesID`, `TestOutline` (missing file among
others, only missing files, text) and `TestSourceContextAndCaps`.

## D32 — A persistent, language-server-fed index the daemon owns (PLAN §3, M9)

**Decision:** `internal/index` is a per-workspace index of what the language servers say
about every file they handle, and of what every file imports. The daemon owns it (it is the
process that has the warm servers), `--no-daemon` builds the same thing in its own process
through the same `daemon.Service`, and both persist it under
`$XDG_CACHE_HOME/lightspeed/<workspace-hash>/` (`~/.cache` when unset; `<workspace-hash>` is
the socket's). The package knows nothing of sessions, sockets or the CLI: what it needs — the
file list, which server handles a file, an outline of one file, readiness — arrives through
one small `Backend` interface, which `internal/daemon/index.go` implements over the pool and
the tests implement over a table. A daemon request (`lightspeed/index`, ops `status`, `build`,
`clear`, `search`, `repo_map`, `imports`, `importers`, `graph`, `cycles`, `counts`) carries a
query in and the typed answer of `internal/index` out, so the CLI and the MCP tools render
the same numbers from both modes.

*An entry* is one file: path, language, the server that handles it, `size` + `mtime` (the
cheap first check), the SHA-256 of its content (the authority), when it was recorded, its
outline (id, kind, name, container, the declaration's lines, signature, first sentence of the
comment above it, parent) and its imports as written. It is immutable once published, so a
reader never sees half of an update. Files a server handles *and* files only an import
extractor covers are indexed; a file neither covers is counted by language in `index status`
as uncovered, and a file that is too large (over 1 MiB), binary (a NUL in the first 8 KiB),
a symlink or otherwise not a regular file is skipped *with the reason*. Symlinks are never
read — not even inside the workspace — so a link out of it cannot make the index read what
is out there (D23's confinement, kept).

*One id rule.* The ids the index records are computed by the code `outline`/`source` use, and
that code moved from `internal/cli` to `internal/symbols` (decoding both `documentSymbol`
shapes, `pinpointNames`, `symbolIDs`, the line index and the doc-comment rules) so that an id
the index reports cannot fail to resolve in `source`: two copies of D21 would drift.
`internal/cli` keeps its old names as aliases; a test checks `search_symbols --detail full`
against `source` byte for byte, and the search ids against `outline`'s.

*Persistence.* One file per *part* — the files of one server, or the part `-` for files no
server handles that are indexed for their imports alone — so a gopls upgrade throws away
Go's outlines and keeps Python's. A part is a JSON object whose `files` member is covered by
a SHA-256 in `checksum`, and whose key is `{schema version, lightspeed build id, server
name, server key}`. Any mismatch discards *that part*, and the discard is reported (a
warning naming the part and why), never silent. The build id is the executable's identity
(`daemon.Build.ID`: path, size, mtime), because a new build may extract differently. The
server key is the resolved path, size and mtime of the server's executable, which changes on
an upgrade and needs no server to be started — *cold*, the only version there is. The version
the server reports in `initialize` is recorded too and compared, when known, as soon as the
server is next needed and something has to be built: a different version empties that part
and rebuilds it, with a warning. (A version manager's shim keeps its path and size across
upgrades, so for a shim only the second check catches an upgrade, and only when a build is
due; that is a stated gap, not a bug.) A file that is truncated, edited, not JSON or not an
index part is discarded whole — `TestCorruptCacheIsDiscardedNotTrusted` truncates it, replaces it
with garbage, edits one byte of an entry, empties it and puts someone else's JSON in its place.

*Atomic and concurrent.* A part is written to a temporary file in the same directory,
fsynced and renamed over the old one, so a reader sees one file or the other. Two writers —
two daemons, a daemon and a `--no-daemon` command, two CLIs — each leave a complete valid file
behind; before writing, a process merges in the entries another process wrote since it loaded
for paths it does not itself hold, and never replaces one it holds (its own were revalidated
against the disk, the other's may be older). What the loser lost is rebuilt on demand, never
corrupted (`TestTwoConcurrentBuildersLeaveAValidCache`, three rounds, `-race`). Temporary
files a killed writer left are swept when they are over an hour old.

*A server that cannot be used at all* — not installed, will not start — is not "not ready":
its files are indexed for their imports only, the build goes on with the other servers, one
warning says so, and the server is not asked again until its executable changes. This
repository has six Lua files and no `lua-language-server`; before this rule `index build`
was exit 3 here. Exit 5 stays for a server that *is* there and is still working.

*Memory.* The whole index is in memory in the daemon (this repository: 3.5k symbols, 1.2 MB
of JSON). It is a per-workspace structure, not a database: a repository with millions of
symbols would want a different store, and this one has not been measured past this
repository.

Proof: `internal/index` (`manager_test.go`, `persist` and `tracker` tests) hermetically; the
CLI suite in both modes; `internal/symbols`.

## D33 — Never serve a stale entry, and never let a warm server be stale either (PLAN §5.2, M9)

**Decision:** the reason this index exists rather than a prebuilt tree-sitter one is one
property: nothing it reports is older than the file on disk. It is achieved in two places
that share one change detection (`index.Tracker`).

*Every query revalidates the files it is about to report on.* A scan lists the workspace —
the same enumeration as `tree` and `search_text` (`index.ListFiles`, moved out of the CLI,
which keeps thin wrappers) — and lstats every listed file. An entry is *current* when its
size and mtime are the scan's and neither was taken racily; otherwise the file is read and
hashed: the same hash means the file was touched and only its stat is updated, a different
one means it changed and it is rebuilt (imports, and an outline asked of its server, with the
bytes that were hashed — a server is told the content the index read, not "whatever is on
disk now"). Files that appeared are added; entries for files that are gone, newly ignored or
replaced by a symlink are dropped whatever the query's scope. A query is scoped by
everything that narrows its answer (`--path`, `--glob`, `--lang`) so it revalidates, and
therefore builds, only what it can reach — and only entries it revalidated can reach the
answer: a search restricted to `two/` builds its search index from the entries it just
checked, never from the rest of the map (`TestStaleEntryOutsideTheScopeCannotReachTheAnswer`).

*Racy stats.* A file edited twice within the resolution of the clock keeps its size and
mtime if the second edit is the same length (git calls it "racily clean"). A file whose mtime
is within two seconds of the moment it was recorded — or of the scan — is compared by hash
until it has aged past that (`TestSameSizeEditInTheSameTickIsSeen`: a same-size edit with the
mtime put back is seen). The same rule makes a directory modified within two seconds
"young": the next scan lists again instead of trusting its mtime.

*Cheap when nothing moved.* A scan reuses the previous file list unless the mtime of a
directory of the workspace changed (something was created, removed or renamed in it), a
listed file vanished, or a `.gitignore` changed; only then is `git ls-files` run again. "Every
directory" includes the ones that hold no listed file: the first file created in an empty
directory changes that directory's mtime and nothing else's, so a tracker that watched only the
ancestors of listed files never saw it (found in review round 3: `mkdir -p old/sub`, wait, write
`old/sub/f.go`, `search_symbols` said nothing, indefinitely). The directory set is made by a
walk each time the list is made again (`watchDirs`), not on every scan; it does not enter `.git`
nor a hidden or build directory ([`SkipDir`]) unless a listed file lives there, and does not
apply ignore rules (a needless relist beats a missed file); it stops at 50 000 directories and
says so in the report's warnings. An unchanged tree costs one lstat per directory and per file
and no process: 0.9 ms for this repository's 291 files. *Not seen*: a change to `.git/info/exclude` or the user's global
excludes that adds no file and moves no directory's mtime — the ignore *rules* changed but
nothing in the tree did — until something in the tree moves. A file system with a coarse or
absent directory mtime (some network mounts) would defeat the directory check; the file
stats would still see edits and deletions, not new files, until a listed directory moved.

*The servers are told too* — this closes the gap D15 recorded ("a file changed by something
other than lightspeed is only noticed by the server for files a command opens"). Before it
answers **any** request (`Service.Query`, and `acquire`, which every session-level call goes
through) the daemon scans the workspace, diffs the scan against the last one it made, and
sends `workspace/didChangeWatchedFiles` — created, changed, deleted — for the difference to
**every** live session, because the editor that would send them is not there and a file
changed while server B was not being asked is still one B must hear about. The first scan is
the baseline and is taken before the first server is spawned, so a server that starts later
reads a disk no older than the baseline. Nothing changed, nothing sent
(`TestExternalEditIsSeenByReferencesThroughAWarmDaemon`: a fake server that caches its
reading of a file the way a real one does, an external edit of a file no command opened, the
answer moves from line 3 to line 5, exactly one notification, and none on the next query;
with the reconciliation removed the same test fails with the stale answer). Checked by hand
against gopls 0.23.0 in a two-file module: a second `references` through the warm daemon
listed a new use added to `b.go` by `printf >`; the same binary built *without* the
reconciliation answered the old list. So the gap was real for gopls, and this closes it. It
is not verified for pyright, rust-analyzer, clangd or vtsls, which may watch files
themselves or may not honour `didChangeWatchedFiles` for closed files; the notification is
the standard mechanism and the cost of a server ignoring it is the gap that existed before.

*Cost.* Each request scans (about 1 ms here) and a request that changed files sends one
notification. A workspace of tens of thousands of files makes each scan proportionally more
expensive (one lstat each) and could exceed the 150 ms target of the goal; the target was
measured on this repository only.

## D34 — The index respects the readiness gate, builds incrementally, and says when it built (PLAN §5.2, M9)

**Decision:** an outline is a claim about a file, and a server that is still indexing can
answer `documentSymbol` with an empty list. So the index never records what a server said
until the server can be believed.

*Readiness once per server, before anything is recorded.* A build finds what needs a server
(changed, added, or lacking an outline the query needs), then waits for each of those
servers with the gate's `AwaitReady` (D6, the query's own `--timeout`/`--settle`), and only
then asks for outlines — as raw requests, in parallel (default `min(8, 2×CPUs)`), not one
gated request per file, which would cost every file the settle window when the server sends
no progress. A server that is not ready fails the whole query with its own error — exit 5,
`not_ready` — and *records nothing that depended on it*: results built without a server
(imports) may be kept, an entry that lacks the outline the query needs is never published
(`TestNotReadyServerRecordsNothing`, and through the CLI with no cache file written, exit 5).
An import query needs no server and answers while one is indexing (`TestImportsOnlyNeedsNoServer`).
An outline the server answers with an error is about that file: the file is skipped with the
server's words, not indexed, and retried by the next query that needs it; anything else —
the server died, was cancelled — stops the build with entries built so far intact.

*Incremental and resumable.* Only files whose entry is missing or stale are read; a build
publishes each entry when its outline arrives and writes the cache every 500 of them, so a
build that is killed resumes from its last checkpoint. It is bounded-parallel for reading
and asking. A second `index build` of an unchanged repository reads nothing and asks no
server.

*Lazy, said aloud.* A query builds what it needs and adds a warning: "the index was built
lazily for this query: 263 files (257 outlines from gopls) in 1.35s; `index build` warms the
whole workspace ahead of time"; the same data is in the envelope (`data.index`: how many
files were fresh, touched, changed, added, removed, built, and how long the scan and the
whole took). `index build` is the explicit warm-up; `index status` builds nothing and says
how far the index is from the disk (fresh, stale, not indexed, entries for deleted files),
per server and per language, what was skipped and why, what is uncovered, the cache's path
and size, and the last build's duration; `index clear` removes the cache and the memory.

*Empty outlines.* An empty `documentSymbol` for a non-empty file is recorded as empty once
the server is ready. That is the same trade D6 makes for a stable-and-drained empty answer,
without the settle window; a server that reports readiness early and then answers empty for
files it has not loaded would leave those files without symbols until they change. It has not
been observed for gopls; it has not been looked for in other servers.

*Measured* (this repository, real gopls 0.23.0, `TestIndexPerformanceAgainstThisRepo`,
which skips when gopls is unusable): cold `index build` 3.2 s (268 files, 262 outlines, 3 590
symbols, 1 462 imports; 4.6 s by the shell including the daemon's and gopls's start); warm
`search_symbols` median 9.9 ms (worst 17.6 ms) over 25 runs, through the CLI and the daemon
socket; revalidation of the unchanged repository median 10.2 ms (of which the scan is 0.9 ms
and the whole revalidation in the index 5 ms). By the shell, one warm `search_symbols` is
about 8 ms, process start included.

## D35 — The import graph: extractors, nodes, and "not covered" (PLAN §4, M9)

**Decision:** `find_importers`, `imports`, `dependency_graph` and `dependency_cycles` are
answered from imports read out of the files' *text* — the language servers are the parsers
for symbols, and none offers an import graph — behind one interface:
`Extractor{Languages, Extract(path, src) []ImportRef}`. No tree-sitter and no cgo.

*Extractors.* Go uses `go/parser` (`ImportsOnly`), with `"C"` skipped. Python, JavaScript/
TypeScript (and JSX/TSX), Rust and Lua share one small lexer that yields tokens with line
numbers and knows comments (nestable for Rust), strings with escapes, template literals with
`${}`, JS regular-expression literals (a heuristic), Rust raw strings and char/lifetime
quotes, Lua long brackets and Python triple-quoted strings with their prefixes, and are
written as matchers over tokens — never regular expressions over raw text — so that an
import-looking line in a comment, a string, a docstring, a template literal or a raw string
is not an import (each language's tests have the trap). C and C++ blank comments and raw
strings and then read `#include` and `#import` lines. Multi-line forms work (`import {\n a,\n
b\n} from 'x'`, parenthesised Python imports, Rust `use a::{b, c::d}` expanded to one ref per
leaf).

*Resolution* is redone against the current file list whenever a graph is built and is never
persisted (a file created next to an importer changes what an old import resolves to): Go by
the `module` line of every `go.mod` in the workspace (nested modules: the longest prefix
wins) and local `replace` directives; Python by dots or by trying each ancestor directory of
the importer (and `src/`); TS/JS relative specifiers with extension and `index` probing and
`.js`→`.ts`; Rust `mod foo;`, `crate::`/`super::`/`self::` through the module files that
exist; C/C++ quote includes relative to the includer, the root, `include/`, then a unique
path suffix; Lua `a.b` → `a/b.lua` or `a/b/init.lua`. Anything else is *external*, with a
category (`stdlib`, `third-party`, `system`); a relative import that does not resolve is
`unresolved`, not external. An import that resolves to the importing file itself (Rust
`use self::Item`, `crate::Item` in the crate root, Python `from . import name` in an
`__init__.py`) names an item of that module: it is category `self`, with no target and no
edge, so it can never show up as a one-file import cycle (fixed in review round 2).

*Nodes.* Go's nodes are **packages** (directories), everything else's are files, and edges
from `_test.go` files are left out of the graph (an external test package importing the
package it tests is a cycle only if files are the nodes; `imports` still lists them, marked).
An argument that is a file, a directory or a Go import path resolves to the same node.
`dependency_graph` walks out, in or both to `--depth`, `find_importers` lists importing files
with the line, cycles are strongly connected components (each starting at its smallest node,
in a stable order) and are exit 1 when found, as `check` with errors is; the text form of the
graph is an indented tree that marks `(seen)` and `(cycle)`.

*Uncovered is not empty.* A language with no extractor gives **no edges and says so**: `imports`
and `find_importers` of such a file are `not_covered` (a new code, exit 3 like `no_server` —
nothing is able to answer), never an empty list; graph and cycle answers warn with the
languages that are present and unread. Languages that have no imports at all (JSON, Markdown,
go.mod, …) are not warned about. `index status` counts uncovered files by language.

*Limits.* No `sys.path`, tsconfig `paths`, package `exports`, Cargo workspace membership,
`#[path]` or macros, `-I` flags or Lua `package.path`; TypeScript path aliases and monorepo
package names resolve as external. C++20 `import` is not read. A JavaScript file that
confuses the regular-expression heuristic can lose the imports after the confusion.

## D36 — `search_symbols`, `repo_map` and `repo_outline`'s counts (PLAN §4, M9)

**Decision:** *`search_symbols <query>`* ranks over the whole workspace: BM25 (k1 1.2, b
0.75) over a weighted document per symbol — name tokens ×3, container ×1.5, signature ×1,
comment sentence ×1 — with `camelCase`, `PascalCase`, `ACRONYMWords`, `snake_case` and digit
boundaries split into lowercase tokens, scaled by the fraction of the query's tokens that
matched. Four tiers are then applied so that the order **exact name > prefix > token > fuzzy**
holds by construction and not by luck — the score is `tier + s/(1+s)`, so no BM25 value
reaches the next tier — with case-sensitive equality winning within the exact tier. Ties
break on shallower nesting, path, line and id: the result is the same whatever order files
were indexed in. Filters (`--kind`, `--lang`, `--glob`, `--path`) apply before ranking and are
part of the revalidation's scope (D33). `--limit` (default 20, `0` for all) gives
`truncated:true` and the total; `--detail compact` is id, name, kind, file, line and match;
`standard` (default) adds end line, score, container, signature, first comment sentence and
language; `full` also inlines the source, cut by the same code as `source` (`--max-lines`,
`--max-bytes`, `--context`), from the file as it is *now* — and a file whose hash is no longer
the one the hit was indexed at is left without source and named in a warning.

*Fuzzy* is used only with `--fuzzy` **and** only when nothing matched (trigram Jaccard ≥ 0.3
or Damerau–Levenshtein (an adjacent transposition is one edit, the commonest typo) ≤ max(1, len/4) on the normalised name), its hits are flagged `match:"fuzzy"`,
`fuzzy:true` and a warning, and it is never mixed into a non-empty answer. *No match* is
`ok:true`, exit 1 and `nearest`: up to five names that are actually near (within half the
query's length in edits, or sharing trigrams, or containing one another) — a query that
resembles nothing lists nothing, because a list of unrelated names reads as a guess. A typo
with one good token (`serverNmae`) still token-matches `server*`: fuzzy is the fallback for
*nothing*, not for a bad match.

*`repo_map`* lays files out in order of import-graph centrality — PageRank (damping 0.85, a
fixed iteration cap, deterministic) over the file graph, a Go package's rank spread over its
non-test files — and lists the top symbols of each as signature lines, greedily until the
token budget (`--budget`, default 2000; a token is estimated as 4 bytes) would be exceeded.
The first 20 files get `--per-file` symbols (default 8), the rest at most 3; candidates are
top-level symbols and members of type-like symbols, exported names first, then type-like
before function before method, printed in document order. The budget is exact — the rendered
body is at most 4×budget bytes, and a budget that cannot hold even the first file's header
returns no files and `truncated:true` — and truncation always says how many files were listed
of how many. Files of languages with no extractor are ranked by symbol count alone, and the
answer says so.

*`repo_outline`* gains symbol counts (per directory, and by kind) when the index is warm —
read from what the daemon or an earlier command *persisted*, validated by the same keys as a
load, never built and no server or daemon started for it. Counts cover files whose entry is
current by its stat; those that are not are counted as `not_current` with a warning, so a
count is never a stale one (`TestRepoOutlineCarriesSymbolCountsOnlyWhenWarm`).

## D37 — This design against a tree-sitter index: what it buys and what it costs

An honest comparison, because the obvious alternative design for an MCP code-navigation
server is a prebuilt tree-sitter index.

**Exactness.** Symbols come from the compiler's own front end — gopls, rust-analyzer, pyright —
not from a grammar that approximates it. Ids, kinds, and declaration ranges are what the
language says (a Go method is attached to its receiver type even though its range is outside
the type; a rust-analyzer `impl` is a symbol a grammar would have to guess at), and a rename
or a `references` after a `search_symbols` uses the same server's understanding. A
tree-sitter index is syntax-exact and semantics-blind: it cannot tell two `Parse`s apart by
package, resolve a call, or know that a name is a re-export. Import *resolution* here is
weaker than a compiler's and weaker than a tree-sitter index's in some languages (D35's
limits); the extraction itself is text-level in both.

**Freshness.** This is the design's actual argument. A prebuilt tree-sitter index is a
snapshot: it is right when it was built and wrong after `git checkout`, an editor save or
another agent's write, until something re-indexes; the best such an index can do is
flag its own answer as possibly stale. This index revalidates every file it
reports on against the disk, by stat then hash, on every query, and the daemon tells its
language servers what changed before answering anything (D33), which closed a case where
real gopls answered from a stale view. The price is a scan per query (~1 ms per few hundred
files, one lstat each) instead of a lookup, and it grows with the file count.

**Cold-start cost.** Tree-sitter parses a repository in well under a second per thousand
files, with no server, no readiness, no warm-up, and no per-language install: this repository
is 291 files. Ours needs a language server started, ready (rust-analyzer 30–90 s, jdtls
minutes — the reason for the daemon), and asked once per file: 3.2 s here for gopls, warm
afterwards, and *first use of an unindexed workspace* is a real wait that a tree-sitter index
does not have. The persistence makes a second process cheap (the cache is read, the changed
files are re-asked), and `index build` can be run ahead of time; but a server that is
unreadily indexing is exit 5 where tree-sitter would simply answer.

**Language coverage.** Limited to installed servers (plus the six import-extractor families: Go, Python, TypeScript/JavaScript, Rust, C/C++ and Lua).
Tree-sitter has grammars for well over a hundred languages, and a file in one of them is
indexed whether or not anyone installed anything. Here a language with no server installed is
*not indexed* — reported, in `index status`, as uncovered or skipped with the reason
(`server_unavailable`), never silently — and its imports are covered only for the six
extractor families. In a polyglot repository with servers missing, coverage is exactly the
set of things installed, which for an agent on an unfamiliar machine may be very little.

**Robustness.** A tree-sitter index cannot be wrong about a file because a server crashed or
disagreed with the build system, and it works on code that does not compile. A language
server's `documentSymbol` for a file that does not build may be partial, and what it returns
is recorded as fact once the workspace is ready. Servers also differ in what they call a
symbol and where its range begins (gopls: flat, from the `func` keyword; others: the whole
declaration including the comment), which the id and comment rules absorb by heuristics
(D21, D22) that have been checked only against gopls and a scripted server.

**What was not measured.** Only this repository (291 files, one server) has been indexed with
a real server. Not measured: repositories of tens of thousands of files (scan and memory
costs), pyright, rust-analyzer, clangd or vtsls (their readiness signals, their symbol shapes,
their honouring of `didChangeWatchedFiles`), or the token cost beyond this
repository. The 30 s / 100 ms / 150 ms targets are met on this repository by a large margin
(3.2 s, 10 ms, 10 ms) and are not evidence about others.

## D38 — Composed verdict tools: `type_hierarchy`, `check_references`, `rename_check`, `delete_check` (PLAN §4, M10)

**Decision:** four commands that answer a question with a verdict and the evidence behind it,
assembled from queries lightspeed already makes. Every row carries `evidence` (`type_hierarchy`,
`implementation`, `references`, `search_text`, `index`, `importers`, `prepare_rename`, `rename`),
every verdict carries `rule` (the sentence that produced it, with the counts), and every list is
bounded by `--limit` with `truncated:true`, the total and a warning that says how to widen it. A
top-level `truncated` and `total` summarize the several lists of one answer, so a caller checks one
bit. All four take a location, `--symbol` or `--id` (`parseLocationFlags` + `openSubject`, the kit
of `compose.go`), go through the daemon unless `--no-daemon` says otherwise, and are MCP tools.

- **`type_hierarchy`** asks `prepareTypeHierarchy` and `supertypes`/`subtypes` to `--depth` (1–5,
  default 1) under a node budget of 300 and a visited set. It is deliberately not capability-guarded
  in the table (like `check`): a server without `typeHierarchyProvider` still answers the *subtypes*
  through `textDocument/implementation`, one level deep, each row `evidence:"implementation"` and a
  warning naming the fallback and that supertypes are unavailable. Supertypes alone on such a server
  is exit 3 (`unsupported_method`, naming `typeHierarchyProvider`); a server with neither is exit 3.
  The session advertises `textDocument.typeHierarchy` (`sessionCapabilities`), without which servers
  do not offer it. A type outside the workspace is a row with its absolute path and `external:true`.
  `depth` is stated in the answer rather than reported as "cut": whether a leaf has more below it is
  not known without asking, and asking would double the requests for a flag the caller set.
- **`check_references <name|loc>`** resolves its argument four ways — id, location, `--symbol`, or a
  bare name (as `--symbol`) — and if it names exactly one symbol lists the semantic references
  (declaration excluded, each with its enclosing symbol's id from the index) in `semantic`, and, in a
  separate `text_only`, the whole-word, case-sensitive occurrences of the identifier on lines no
  semantic reference or the declaration accounts for (`search_text`'s engine and skip rules). A name
  that is ambiguous, unknown, or has no server is *not* an error: `resolution.kind` says
  `ambiguous` (with candidates), `not_found` or `unavailable`, the semantic list is empty, and the
  text list stands alone. The verdict is `used` (≥1 semantic reference), `used_only_in_text`, or
  `unused`; exit 1 for `unused` only, grep's convention for "nothing found". A location that is a
  *use* rather than a declaration is followed to its declaration with `definition` to name the
  symbol and to leave the declaration out of the text list.
- **`rename_check <loc> <newname>`** runs `prepareRename` when advertised and a real `rename`
  request whose edit set is staged by `internal/edit` and never written: files, edits, per-file
  counts and resource operations. A server error or an edit set the applier refuses is a
  `refused` verdict with the server's words, not a failure of the command. Collisions come from the
  index's symbols: another symbol with the new name and the *same container* in the declaring file,
  and, for languages whose unit of scope is the directory (Go, Java, Kotlin), in the other files of
  the directory; for the others only the file is checked and `collisions.detail` says so. A
  collision the index confirms outranks a server refusal that says the same thing. `unrenamed_mentions`
  are whole-word text occurrences of the old name on lines the rename does not edit (strings,
  comments, configs, other languages). Verdicts: `ok`, `collision`, `refused`, `noop` (same name),
  `unverified` (no rename support, so only the index spoke). Exit 0 only for `ok`.
- **`delete_check <loc>`** lists references outside the symbol's own declaration (a recursive call or
  the declaration itself, which some servers return whatever `includeDeclaration` says, is counted
  as `inside_declaration`, not as a use), split into tests and other code (`isTestPath`, a
  heuristic). It states whether the symbol is exported and *how that was decided*, per language:
  Go by the capital letter, Python by the leading underscore, JS/TS by `export` on the declaration
  or a named export in its file, Rust by `pub` (`pub(…)` is `unknown`), Java/C# by `public`/`private`
  (else `unknown`), Kotlin by `private`/`internal`, Lua by `local`, C/C++ by `static` and the header;
  anything else, and any class member in JS/TS, is `unknown`. An exported or unknown symbol earns a
  caveat: users outside the workspace are invisible to its references. When the symbol is the last
  top-level symbol of its file (and directory) by the index, `last_symbol` says so and the importers
  of the file (the package directory for Go-like languages) are listed from the import graph; a
  language with no extractor is said to be unknown, not empty. Verdicts: `in_use`,
  `used_by_tests_only`, `leaves_importers_dangling`, `unused`. **Exit 0 means the evidence does not
  stand against deleting it; exit 1 means it does** — the reverse of `check_references`, whose
  question is "is there anything?" where this one's is "is it safe?".

**Why:** each of these is a question an agent asks before it edits, and each is a place where a bare "yes" is what deletes live code. A
reference list from a server that is still indexing is exit 5 (the gate); an unresolved name still
gets an honest text answer; a reflective or generated use is exactly what `text_only` is for.

**Not proven:** anything but gopls on real code (`type_hierarchy` natively and `rename_check`'s
refusals were seen there; the fallback, the collision rules and the export rules are tested against
the scripted server and unit tests); the export rules of every language but Go as the language's
own tooling would see them; `unused` on a symbol used through reflection, code generation, build
tags or a language the server does not cover.

## D39 — `blast_radius` and `dead_code`: what a change touches and what nothing uses (PLAN §4, M10)

**Decision:** two composed commands, built from evidence that already exists and saying which
evidence each row rests on. Both are read-only, go through the daemon like every query, and are
MCP tools from their table entries alone.

*`blast_radius <loc|file>`* (`--id`, `--symbol` or a location for a symbol; a path that exists
for a file or directory). The **summary comes first and is always complete**: reference
locations and the symbols that contain them, callers and how many are beyond the direct ones,
importers and how many are beyond depth 1, the distinct files and packages across all rows, and
how many of those files are tests. `--limit` (default 50) then cuts only the rows, dividing the
room **fairly** between the three sources — a long list of references must not hide every
importer — and says `truncated:true`, the total and a warning. Rows are `references` (from
`textDocument/references`, **grouped by file and enclosing symbol id**, the enclosing symbol
taken from the index's outline of the file: the thing you would have to re-read, not thirty
lines), `call_hierarchy` (incoming calls by the same walker as `call_hierarchy`, so D13's cycle
set, node budget of 500 and readiness gate apply and are not re-implemented) and `imports` (the
importers of the subject's file from the import graph — with the line — and, beyond depth 1, the
nodes the graph walk reaches). `test:true` marks a row in a test file by `isTestPath` (a
heuristic: `_test.go`, `test_*.py`, `*.test.*`, `*.spec.*`, `tests/`, …). **Bounds are reported when
they bite**: `--depth` (callers, default 2, at most 5) and `--import-depth` (default the same);
the rows at the *last level walked* are counted (`calls_at_limit`, `imports_at_limit`) and a
warning says their own callers or importers were not followed, because a walk that stops at its
depth looks exactly like one that ran out of callers. A source that cannot answer is listed
under `evidence` with a status — `unavailable` (a server without call hierarchy: a warning, not
a silent omission), `not_applicable` (a type or constant has no callers), `not_covered` (no
import extractor for the language: importers are *unknown*, never "none", D35) or
`not_analysed` (a **file** subject reports importers only: a file has no single position to ask a
server about, and the answer says so and names how to get the rest). Go test files are not in
the import graph (D36), so a Go package's test importers appear as references, not imports.
A server that is still indexing is exit 5 before any row is built. Exit 1 is an authoritative
"nothing is affected".

*`dead_code [dir]`* is N reference queries and is therefore **bounded and resumable** instead
of fast. The index lists the symbols in scope (`--path`/the positional, `--kind`; default
function, method, class, struct, interface, enum, constant — fields and variables are read
through serialisation and reflection far more often than functions and are left out). Exclusions
that need no query are counted by reason in `excluded`: `test_file`, `entry_point` (`main`,
`init`, `TestMain`, Go `Test*/Benchmark*/Example*/Fuzz*`, Python dunders), `ignored`, `local`
(declared inside a function) and `exported` (unless `--include-exported`). What is left is
ordered `(file, line, id)` and each gets one `textDocument/references` with the declaration
excluded, **and any reference inside its own declaration too** (recursion is not use). A method
with none is then asked `textDocument/implementation`: one that implements an interface is not
dead, whoever calls the interface (`excluded.implements`); a server without
`implementationProvider` leaves methods as candidates with **low** confidence and the reason.
`--budget` (default 100) caps the reference queries per call; when it runs out `complete:false`
and `next_cursor` — `<line>|<id>` of the last symbol examined — resumes strictly after it, so a
resume is stable across edits that shift lines. `--limit` caps the rows shown, as everywhere, and
`total` is the candidates found in this call; `examined`, `eligible`, `used`, `symbols`,
`queries` say how much work was done. Exit 1 means candidates were found (as `check` and
`dependency_cycles`). A not-ready server is exit 5, never an empty list: an empty candidate list from a
server that is still indexing would send someone to delete live code.

*Export-ness is per language, and "unknown" is a fourth answer.* Go: a capital letter. Rust:
`pub` (not `pub(…)`). JS/TS: `export` on the declaration; a class member is `no` if `private`/`#`,
else unknown. C/C++: `static` is no, otherwise unknown (headers). Lua: `local` is no, otherwise
unknown. Python: a leading underscore is no, otherwise *unknown* (a convention, not a rule — a
script's `helper` is not public API). Everything else is unknown. Unknown stays a candidate, at low
confidence, with the reason.

*Results are candidates, each with a confidence and the reasons that lowered it*: **high** is an
unexported symbol the server outlined, in a file compiled unconditionally; **medium** is a method
(it may satisfy an interface declared outside the workspace, or be called by reflection) or a
name in a language that resolves names at run time (Python, JS/TS, Ruby, Lua, PHP); **low** is
exported, export-unknown, a Go file with a build constraint, a GOOS/GOARCH file name or cgo (a use
may sit in a file that was not built), or a method on a server that cannot answer
implementation. Every answer carries the standing warning that reflection, build tags, cgo,
generated code and callers outside the workspace can hide a use.

*Ignore lists.* Built in, for methods only, the names a language's own protocols call so that no
reference exists: Go `String Error GoString Format Unwrap Is As Marshal*/Unmarshal*(JSON, Text,
Binary) Read Write Close Len Less Swap ServeHTTP Scan Value`, JS/TS `constructor toString toJSON
valueOf`, Rust `fmt drop deref deref_mut next from into default eq cmp partial_cmp hash clone
try_from from_str as_ref`. The workspace adds its own in `.lightspeed.toml`:

```toml
schema_version = 1
[dead_code]
ignore_names = ["Handle", "Server.Serve", "Test*"]   # a name, a Type.Name, or a * pattern
ignore_paths = ["**/*.pb.go", "gen/**"]              # workspace-relative globs
```

`internal/serverdef`'s strict parser rejected an unknown top-level table, so it was taught this
one (and only this one): `[dead_code]` is allowed beside either definition shape or alone (a file
that sets only `[dead_code]` defines no server, and the schema has a third `oneOf` branch for
it), its keys are checked like every other table's (`ignore_name` is an error, not an ignore
that silently does nothing), and it is read by `serverdef.LoadDeadCodeConfig`, not folded into a
definition.

**Symbols are positioned by the server, listed by the index.** The index knows every symbol
cheaply and persistently but stores no column, and a column is what a references request
needs; so each file with something to examine is outlined once more by the session's server
(`documentSymbol`, which the daemon keeps warm) and the symbol is matched by id — the same id
function both use (D21). A symbol the server's outline no longer lists (the disk moved between
the two) is `excluded.unresolved` with a warning, never guessed at.

**What is not proven.** Only gopls has been run for real (blast_radius on this repository: 33
rows in ~4 s cold, dead_code over `internal/cli` finding `symbolByID`, which was in fact
unused); everything else is proven against a scripted "word server" that answers references from
the words in the files. Confidence is a heuristic: no measurement backs the three levels. The
per-language export rules were not checked against pyright, rust-analyzer or vtsls.
`dead_code` does not look for symbols referenced only from tests (they count as used).

## D40 — Git-aware tools: `changed_symbols`, `churn`, `hotspots`, `related` (PLAN §4, M10)

**Decision:** four commands that combine git with the index, all shelling out to `git` the way D8
does (`internal/cli/gitkit.go`): read-only, `--no-pager`, `-c core.quotepath=off`,
`GIT_OPTIONAL_LOCKS=0` (a `git diff` must not refresh the index, which would be a write), stdout
capped at 32 MiB (the process is killed at the cap and the answer says the history was cut), a
30 s bound on history queries, `--relative` so that a workspace that is a subdirectory of the
repository sees its own paths and nothing outside it, and `:(literal)` pathspecs for any
user-given path. A `--base` or `--since` that starts with `-` is a usage error, never an option.
Outside a repository, or with no git, each answers `ok:true`, exit 0, `git:{repo:false,reason}`
and a warning, with what it can (`related` keeps three of its four kinds of evidence). Exit 0 and
not 1: 1 is an *authoritative* empty answer (a clean tree, an empty window), and this is neither.

*`changed_symbols [--base <rev>] [--staged] [--path P]`* runs `git diff -U0 --no-prefix -M`
(working tree against HEAD; `--staged` is the index against the base; an untracked file is wholly
added; a repository with no commit is compared with the empty tree) and maps each hunk onto
symbols. **New side:** the index's `symbols` operation (`internal/index/symbols.go`, revalidated
like every query, D32) — so the hunks and the ranges describe the same bytes. **Old side:** an
outline of the old blob, `git show <base>:./path`, written to a scratch file *outside* the
repository and opened on the file's own server (a transient `didOpen`, undone by the session's
`didClose`; nothing is written to the workspace and the warm session ends as it began), ids
computed by the index's own code (`index.BuildSymbols`) so old and new speak the same ids and
coordinates. A rename compares symbols by the part of the id after the path. Statuses: a new
symbol whose id is not in the old outline is `added`; one in both whose lines a hunk touches
(an added or replaced line inside it, or a deletion strictly between its first and last line)
is `modified` — attributed to the *innermost* symbol, so a change inside a method is the
method's and not its type's; an old symbol absent from the new outline is `removed`, carrying
its old file and lines. A hunk that touches no symbol (a package clause, imports, comments) is a
file-level row (`kind:"file"`), as are binary files, pure renames, and files of a language with
no outline. **Where the old side is not available** (no server, a cap of 40 outlined files, a
failure, a blob over 2 MiB) the row says `evidence:"index+hunks"`, the answer counts
`old_unavailable`, a warning says removed symbols there cannot be named, and the rule degrades
to hunks only: a symbol wholly covered by one pure insertion is `added`, any other touched one
`modified`. Bounded: 200 files mapped, 8 hunks per row, `--limit` (default 100) with
`truncated:true` and the total. With `--staged`, a file that also has unstaged edits is warned
about: the index maps the working tree's symbols, so its staged line numbers may not line up.

*`churn [path] --since <d>`* is `git log -z --numstat --no-renames --no-merges` over the window
(default 90 days; git's approxidate, so `30 days`, `2 weeks ago` and `2026-01-31` all work, and
the compact `30d`, `2w`, `12h`, `6m`, `1y`, which approxidate does *not* read — it takes
`--since=30d` for no date and answers with an empty log, reported as "no commit in the window", a
wrong answer stated as fact — are translated to "N units ago" first):
commits, distinct authors, lines added and removed per file, the newest date, files deleted
since marked. Renames are not followed and merges are excluded (their changes are their
parents'); a mass-reformat commit counts like any other. **Per symbol**, for the
`--symbol-files` (default 15, at most 60) most-changed files: the patches of the window
(`git log -p -U0`) are read and each hunk's new-side lines are attributed to the *current*
symbol ranges from the index. Lines are carried forward to now through the zero-context hunks of
every later commit and of the uncommitted diff (`shifter`: an insertion above moves a line down,
a later rewrite of the line itself is counted as "rewritten" and attributed to the place it was
in) — so a symbol changed once, then pushed down by a header added later, is still credited
once. It is an approximation and says so in `approximate`: moves, splits and rewrites are not
tracked, which is what `git log -L` would do at a cost of one process per symbol.

*`hotspots`* is the same data ranked by `score = commits × (1 + ln(1 + loc))` for a file (`loc`
the current line count) and the same with the symbol's line count for a symbol; the formula is
in every answer. Files that no longer exist are dropped, since a ranking of what to look at
cannot point at them. So are files that are **not code**, unless `--all`: prose (markdown, rst,
latex), data and configuration (json, yaml, toml, xml, go.mod/go.sum/go.work) and files of no
known language (a lock file, a LICENSE), by the router's language table (`hotspotNonCode`). On
this repository docs/DECISIONS.md, PLAN.md and README.md outranked every source file — they
change with every commit and are long, which is exactly what the formula rewards and not what a
code-navigation ranking is for. The answer says how many were left out (`non_code_left_out` and
a warning naming the first few and `--all`); a scope that holds only such files is an empty
ranking with that warning and exit 1, not a silent one. The test is the file's language, not
"has symbols": that would need the index warm for every changed file and would drop code in a
language with no installed server, whose churn is as real. `churn` is history, not a ranking of
code, and keeps every file. Size, not the symbol count, drives the score (a file of many tiny symbols
is not thereby harder than one of a few long ones); the count is reported beside it for the
files listed. Both commands bound their lists by `--limit` (default 20; files and symbols each)
and report `truncated:true` with the totals.

*`related <loc|--id|--symbol>`* returns a small ranked list, every row carrying its
`evidence`: `sibling` (the same file's other symbols, nearest first, from the index),
`call_hierarchy` (direct callers and callees, one level, capability-guarded — a server without
it is a warning and a `note` on the source, not an error, and a not-ready server yields no
callers rather than a wrong empty list), `co_change` (files that appear in the same commits, over
the last `--history` commits of the repository, default 500, at most 5000; a commit touching
more than 30 files says nothing about which belong together and is ignored and counted in
`bulk_commits_ignored`; files that no longer exist are dropped) and `similar_name` (an index
search on the subject's short name, `exact`/`prefix`/`token` matches only, excluding the subject
and same-file siblings). Rows that several kinds agree on are merged, with all their evidence.
Ranking is a sum of weights — call hierarchy 4, sibling 2 plus a closeness bonus, co-change 2
scaled by its count against the top file's, similar name 1 plus 0.5 for an exact and 0.25 for a
prefix match — so what calls or is called by the subject outranks what merely sounds like it;
`--per-source` (default 5, callers and callees each) bounds each kind and `--limit` (default 20)
the whole, with `truncated:true` and the total. `sources` says what each kind found and why one
was silent.

**Why:** these are the git-shaped questions an agent asks, and each is an answer where a wrong one
misleads: a diff mapped onto a stale symbol table, a removed function nobody can name, a
"related" list with no reason attached. Ranges from the revalidated index and an outline of the
real old bytes keep the first honest; the evidence column keeps the last.

**Not proven:** the old-side outline against servers other than gopls (a server that refuses a
document outside its workspace makes the file hunk-only, with a warning — the failure is
reported, not hidden); per-symbol churn on history with many moves; `related`'s call hierarchy
beyond gopls; co-change over a repository whose history is mostly bulk commits.

## D41 — `task_context`: a task in words, a capsule out, and an honest "probably not here" (PLAN §4, M10)

**Decision:** `lightspeed task_context <task>` (MCP tool `task_context`) reads a free-text
task, ranks the symbols it is about, expands the best few and returns one token-budgeted
capsule with a confidence and the rule that produced it. It answers "where do I start on
this task" without a model. It
is built from what already exists — the index's ranked search, the call hierarchy, the
import graph, the outline and `source`'s slicing — and adds no state.

**No LLM.** A ranking an agent cannot predict is one it cannot correct. Every step is a rule
that can be read here, tested, and re-run to the same answer; the answer prints the terms it
read, what each matched, and the rule behind the confidence, so a wrong answer is diagnosable
as "it read the wrong terms" and not as "the model was wrong". The cost is the obvious one: a
task that never uses the code's own words ("make the thing that talks to the database faster")
finds nothing, and says so (low confidence) instead of guessing.

**Terms** (`extractTerms`). Backticked and quoted single tokens, camelCase / PascalCase (two
humps) / snake_case / `foo()` tokens and dotted paths (`pkg.Type.Method`) are *identifiers*
(weight 3, 3.5 when quoted; the qualifier segments of a dotted path 1.5). `--flags` are 2,
file paths 2.5 (used to boost, not searched), a path's file stem 1.2. An identifier also
contributes its split words at 0.8. Plain words are 1.0 after a short, documented stopword
list of English function words and the generic verbs and nouns of a request (add, fix,
implement, make, code, function, file, …). A word that is also a plausible identifier
(handle, parse, read, search, index, limit, path) is deliberately *not* a stopword: dropping
it would make a task about that code unanswerable. Words under three letters and numbers
go. At most 12 terms are searched, heaviest first; the rest are counted (`terms_dropped`).
`terms` in the output says, for each term, its kind, weight, the rule that produced it, what
it was used for and how many symbols it matched (best tier).

**Ranking.** Each term is one `search` on the index (so it is revalidated against the disk,
D32, and a still-indexing server is exit 5, D34), and the plain words of a task of two or more
are searched once more *together*, because a compound name is found by its words jointly: the
fifteen best hits for "idle" need not include `Pool.ReapIdle`, the one symbol that is also about
"reap". A hit's score contribution is the term's weight × its match quality: exact 1.0 and
prefix 0.6 are the index's own tiers; its token tier does not say *where* the word was found, so
every candidate is read once more against every plain word — a word of the symbol's **own
name** (`idle` in `Pool.ReapIdle`, receiver included) is `name_token`, 0.6, as much as a prefix;
a word of its signature or doc comment is `token`, 0.3; a word of its file's path (`daemon` in
`internal/daemon/pool.go`) is `path`, 0.3. A plural matches its singular (`servers` / `Server`);
there is no other stemming. A symbol's score is the sum over the terms that matched it, each
counted once at its best reading, and `evidence` says which (`match:name_token:idle`). A symbol
in a file (or directory) the task names is × 1.5, and a named file's first six top-level symbols
are added at 0.5 when nothing else speaks for them. Symbols in test files are × 0.5 unless the
task says "test": a test's name is a sentence about the feature
(`TestPoolReapIdleLeavesOtherServersAlone`) and collects more of a task's words than the code
it tests, so at the earlier × 0.7 tests led the code they test. Ties go to the symbol with more
of the task in its own name, then the declaration before a member, the index's BM25 score, then
path and line. Candidates under 15% of the best score are not listed (`withheld`): a weak match
next to a strong one is padding.

**Confidence** (printed as `confidence_rule` with every answer). Round 2 of the review found
the first rule wrong on both required cases: it lifted "oauth token refresh" to medium because
`token` names an unrelated struct, and it capped every plain-word task at medium, so "readiness
gate" (which exists) could never be high. A single generic word must not carry a task whose
distinctive term matches nothing, and a task in plain English must be able to reach high. The
rule now weighs *coverage*: `cover` of a symbol is its name-evidence score (weight × match
quality, before the file and test adjustments) over the weight of all searched terms.
- *high* — (a) a name-shaped term (identifier or flag) matches one symbol's name exactly, no other
  exactly matching symbol scores within 1.5× of it, and it scores at least 1.25× the
  runner-up (the exact name and its `findFooCache` prefix relatives are not rivals; two
  `Render`s in two packages are); or (b) in a task of plain words, the top symbol matches every
  term, one of them exactly, covers at least 60%, and scores at least 1.25× the next symbol.
  A member that shares the declaration's name (the accessor `Session.Gate` next to `Gate`) is
  not a rival, and on equal score the declaration sorts before the member.
- *medium* — something matches but not decisively: an exact match that is not clearly ahead,
  a name-shaped term matching as a prefix, **half or more** of a plain-word task covered by one
  symbol, or a **compound hit** — one symbol matching two or more of the task's terms, at least
  one of them in its own name — without meeting (b), a single plain word matching a name
  exactly, or only a file the task names. The reason names the symbol, the terms it matched and
  the terms that matched nothing ("… so that part may not exist yet").
- *low* — in a task of two or more plain-word terms, no symbol covers half of the weight **and**
  there is no compound hit (so one generic word matching next to a term that matches nothing is
  still low), a single word matching only loosely, or nothing.

**Round 4: low was wrong for features that exist.** The previous low rule was coverage alone,
and strict ("no symbol covers *more than* half"). Against this repository it answered low —
"no symbol to start from, so treat it as new code" — for "reap idle language servers"
(`Pool.ReapIdle`), "daemon idle shutdown", "exit code taxonomy", "test-only importers"
(`Manager.Importers` covers exactly 50%), "apply refused on dirty worktree", "import graph
pagerank" and more: every generic word of a short task ("language", "servers") dilutes a share,
and the token tier gave `ReapIdle` nothing for holding both `reap` and `idle` in its name. A
false low is the expensive error — the agent re-implements what is there — so low now needs the
absence of *both* kinds of evidence, and what a camel-case name says is counted (`name_token`
above). All of those are now medium, led by the code rather than its tests (`Pool.ReapIdle`,
`Server.Shutdown` / `ErrIdleTimeout`, `testOnlyImporters`, `CodeDirtyWorktree` /
`dirtyWorktreeError`, `Graph.PageRank`, `searchTextCommand`); "readiness gate" is still high.
Checked against absent features too, which stay low with nothing listed: "oauth token refresh",
"graphql schema stitching", "kubernetes pod autoscaler", "user login password reset", "websocket
reconnect backoff", "sqlite migration rollback", "send email notification", "rate limiter
redis", "jwt signature validation", "stripe payment webhook", "cache invalidation redis".

The price is stated, in the other direction now: a two-word task with one generic word that
names an unrelated symbol ("oauth token") is medium, not low, because one exact name is half of
it; its reason says "oauth matches no symbol, so that part may not exist yet", and medium's
verdict already says to read the candidates before relying on them. And a compound hit can be a
coincidence of a common word in a name and another in a doc comment. Both err toward showing
code that may be irrelevant rather than hiding code that exists. The thresholds (0.5, 0.6) and
qualities are judgement, checked on the cases above and the hermetic tests
(`TestTaskContextCompoundNameIsNotLow`, `TestTaskContextHalfCoveredByOneSymbolIsNotLow`,
`TestTaskContextUnmatchedDistinctiveTermIsLow`), not measured on a corpus.

**Low confidence says so and pads nothing.** `verdict` starts "probably not implemented here",
`symbols` is empty, the weak matches are only *counted* (`withheld`), and at most five
`nearest` names are given, labelled a hint, not matches (from the weak matches, or the fuzzy
neighbours of the heaviest term). Exit 1, the code of every authoritative empty answer. The
alternative — listing the least-bad matches so the answer is never empty — is the failure this
command exists to avoid: an agent that is handed five plausible-looking symbols will believe
one of them.

**Expansion.** The best `--expand` (default 3) symbols get, each with its evidence: direct
callers and callees from the call hierarchy (functions, methods, constructors; capability-
guarded — a server without it is a warning and the rows are absent, never guessed; at most 4
of each, with the totals in `callers_total` / `callees_total`), the workspace files their file
imports and the files that import it from the import graph (source importers before tests;
"not covered" languages are a warning), and the three nearest same-file symbols from the
outline. Every failure of an expansion is a warning: the ranking stands without its
neighbourhood, and the answer says what is missing.

**Budget.** `--budget` (default 4000 tokens, about 4 bytes per token like `repo_map`) bounds
the JSON `data` of the answer; the envelope's warnings are not counted. Order of spending: the
fixed part (terms, verdict, rule), the ranked symbols (at most 35% of the budget, always at
least one), the source of the best `--with-source` (default 3, each at most `--max-lines`,
default 60, cut to what remains before the last 20% which is kept for neighbours; a source
that would be cut under 400 bytes is dropped, since it says less than its signature), then the
neighbours by usefulness (callers, callees, importers, imports, siblings), then the files
involved. What does not fit is dropped and counted in `budget.dropped`, `budget.truncated` is
set and a warning names it. `--limit` (default 10) bounds the listed symbols separately and
reports `truncated` / `total` like every list.

**Run on this repository** (real gopls 0.23.0, cold index 7 s, then 0.7–4.6 s in process and
about 1 s through a warm daemon, the time going to call-hierarchy and import queries for the
expanded symbols): "fix `callWalker` cycle detection" is *high* (3.7 against 1.1 for the next);
"make find_importers respect --limit" is *medium* — `findImportersCommand` starts with the name,
nothing is named exactly `find_importers`, which is the rule working as written; a long sentence
of plain words ("how does the readiness gate decide a server is not ready") is *medium*
(`Gate.AwaitReady` matches gate, ready and server, two of them in its name); the short form "readiness gate" is *high* (`Gate` matches both words, 1.3 against 0.9); "oauth token refresh" is
*low* (`token` covers a third of the task, `oauth` matches nothing); "add a kubernetes operator" is *low*, "probably not implemented here", nothing listed. A single
capitalised word (`Render is slow`) is a plain word, not an identifier — quote it in backticks to
say it is a name — because sentence-initial and proper-noun capitals are indistinguishable from
type names without a model.

**Consequences.** It is a name matcher over the index, so its recall is the index's: languages
with no server have no symbols, and it says "no match", not "not covered". It finds the
symbols a task *names*, not the ones it *means*; `search_symbols --fuzzy`, `search_text` and
`repo_map` are the tools for that. The weights and thresholds are judgement, settled by the
hermetic tests and one run on this repository (README, "task_context"), not measured against
a corpus of real tasks — that is not done.

## D42 — Limits everywhere, match quality, test-only importers, and the leftovers of the parity review (PLAN §4, M10)

**Decision:** *Every command that returns a list honours `--limit`* — at most N rows, `0` for
all, `truncated:true`, the total it cut from, and a warning that says how to widen it — or
it is named in `registerNoLimit` with the reason it cannot. This is enforced, not hoped for:
`TestEveryListCommandHonoursLimit` walks the real command table, fails for a command that has
neither a `limitCase` nor a reason, and for each case runs it with `--limit 2` over a fixture of
at least four rows and requires exactly two rows, `truncated:true` and the total. A new list
command therefore cannot be added without deciding. What it found: `find_importers --limit`
was ignored (37 rows came back for 5), and so was it on `imports`, `dependency_graph`,
`dependency_cycles`, `repo_map`, `tree` and `repo_outline` (the last two had their own
`--max-files`/`--max-dirs`; `--limit`, when given, now wins and `0` lifts it, the own flag being
the default when `--limit` is absent). The commands whose lists were unbounded by default get a
default that protects a token budget, reported when it bites and lifted by `--limit 0`:
`find_importers` 50, `imports` 100, `dependency_graph` 200 edges (the nodes shown are the start
and the ends of the edges shown), `dependency_cycles` 20 (the exit code is still decided on all
of them: a limit is a display concession, as for `check`). `repo_map` keeps its token budget as
its bound and gains `--limit` on the files as a second one. Not bounded, with the reason in the
test: `rename` and `format` (a preview must show every edit `--apply` would write — cutting it
would misreport what apply does), `hover`, `file`, `source` and `context` (one bounded slice by
`--max-lines`/`--max-bytes`), `index` (one report), `servers`, `doctor` and `daemon` (bounded by
the configuration, not by the workspace), `batch`, `raw`, `help`, `mcp` and `install`.

*`search_symbols` shows how well each row matched.* `match` (`exact`, `prefix`, `token`,
`fuzzy`) and `score` are in the JSON at every `--detail` (compact used to leave the score
out), and in text every row that is not an exact match ends in `[~prefix]`, `[~token]` or
`[~fuzzy]`, so a reader skimming the rows cannot take a word overlap for the symbol. When the
query looks like an identifier (name segments joined by `.` or `::`, no spaces) and *no* row is
an exact or prefix match, the first thing said — `data.notice`, the first warning, and the first
line of text — is `no symbol named X (exact or prefix); these are token matches`; the
authoritative empty answer (`no symbol matches "X" among N candidates`, exit 1) is likewise the
first thing said instead of trailing the lazy-build notice. Ranking is unchanged: the tiers
already guaranteed exact > prefix > token > fuzzy (D36); this only stops the answer hiding it.

*`find_importers` says what it left out.* The graph has no edges out of Go `_test.go` files
(D35: an external-test package would close cycles that are not in the build), so a package
imported *only* by tests looked unimported. The graph now keeps those edges on the side and
`find_importers` reports the Go test files whose **package imports the target only from tests**
as `test_only_importers` — `count`, the files (bounded by `--limit`), and a warning naming
them — while leaving them out of `importers` (that is what "importers" means: the build's
edges). `--include-tests` merges them into the list, marked `test:true`. Not counted, on
purpose: a test file in a package whose non-test files also import the target (it changes
nothing `importers` does not say) and a package's own tests importing the package. Test files
of other languages are ordinary graph nodes and appear as importers; the graph does not
classify them.

*The leftovers.* A start that is outside the workspace is `outside_workspace` (exit 2) whether
or not anything exists there: `targetRel` used to refuse only paths that existed, so
`dependency_graph ../nosuch` answered `no_such_file`; a relative path that climbs out, and an
absolute one, are places and never import paths, and one that resolves back inside the
workspace (`../x.go` from a subdirectory) is that workspace-relative file, not an escape.
`outline`/`source` no longer say `nosuch.go: nosuch.go: no such file` (the item's message
already began with its target, which the warning and the text writer prefixed again).
`repo_outline`'s text shows the workspace's symbol count in its header and a `N sym` column per
directory when the index is warm — JSON had it — and nothing when it is cold, as before.
`index build` in text and JSON, in process and through the daemon, on a cold and a warm
index, does not print the lazy-build notice (checked against the real binary; the earlier
fix stood, and the test now covers text as well as JSON).

**Why:** the parity review's method was to run the real binary against the questions an agent
asks of it. Each item was a place where an answer looked complete and was not: a limit that
did nothing, a list of "importers" that omitted the only ones a package had, a row that read
as the symbol asked for and was a shared word, an "outside the workspace" that depended on
whether the directory existed.

**Not done:** `dependency_graph`'s `--limit` cuts edges in the order the walk produced them
(by depth, then id), not by any importance; `--limit` on `repo_outline` cuts directories in
path order, as `--max-dirs` always did. Test-only importers exist for Go only, because Go is
the one language whose graph drops test edges. The limit test proves the mechanism on the
scripted server and the text server; the defaults (50, 100, 200, 20) are judgement, not
measured against real agent sessions.

## D43 — The agent setup kit: a guide generated from the table, a capabilities test, no hooks (PLAN §4, M11)

**Decision:** replacing a paid MCP code-navigation server means replacing three things
besides its tools: the policy text an agent reads, the proof that every tool the policy
relied on has an answer, and the steps to swap. Each is made hard to get out of date.

*The guide is generated, not written.* `lightspeed guide` (CLI, MCP tool `guide`, and the
MCP server's `Instructions` in compact form) is built from the command table at the moment
it runs. What is written by hand is the prose that is true of the whole tool (never fall back
to Read/Grep/Glob/Bash for navigation, Read only before Edit; ids, locations, `--symbol`;
preview/apply and the dirty-worktree refusal; exit codes, and that 5 is never "nothing found";
truncation) and `guideGroups`, which says which commands answer which kind of question.
Every command line — name, arguments, summary, flags — is read from the table, so a renamed
flag or a new command shows up in the next build. Three tests keep the two halves honest: a
command with MCP tools that no group names fails (a new tool cannot be left out of the
policy), a group that names a command that does not exist fails, and any `--flag` in any
rendering that no command registers fails (checked against what `-h` really prints, not
against the spec that generated the text). The flags the prose relies on are also checked
per command (`guideFlagClaims`). `--format claude-md` frames the same text with
`<!-- lightspeed guide vN build ID -->` … `<!-- /lightspeed guide -->` so a CLAUDE.md section
can be regenerated in place; the version is `guideVersion`, bumped when the rules change, and
the build id is the daemon's (D29). The compact form (about 2 kB; a test caps it at 2 500
characters) is the instructions every MCP session carries, and keeps the two things the old
hand-written instructions had that a client acts on: `not_ready` is not "no results", and
mutations preview unless `apply`. `guide` needs no language server and no daemon. Unlike the
query commands its default is markdown even off a terminal: it is prose, and
`lightspeed guide > file` should work; `--format json` is the envelope (the MCP tool's answer).

*Two surfaces, one table.* The command line and the MCP client do not share words: `index` is
the tools `index_status`, `index_build` and `index_clear`, `daemon` is `daemon_status`,
`--allow-dirty` is `allow_dirty`, `--apply` is `apply: true`, `install` is no tool at all, and an
exit code is `_meta["lightspeed/exit"]`. The first guide was the same text on both, so over MCP it
told the agent to call things it does not have. The guide now has a style for each
(`guideText` / `guideClaudeMD` for the command line, `guideMCP` for the tool and the server
instructions), chosen by where the call came from (`env.overMCP`, set by the MCP server for
every call): the MCP rendering lists each *tool* with its own description and its parameters by
their schema names, spells the prose with parameters, and says of a missing server that
`doctor` reports it and installing is `lightspeed install <name>` in a shell — the user's call,
not the agent's. The JSON `groups` carry tool names there too. A test over a real MCP session
(`TestMCPGuideNamesToolsAndParametersNotCommandsAndFlags`) fails for any `--` in the MCP guide or
the instructions, for a listed line that is not a tool, for a listed parameter no tool has, for a
tool that is not listed, and for `install`, `index` or `daemon` named as callable; it also checks
the command line kept its flags. A CLAUDE.md section is read by an agent that may use either, and
is generated in the command line's words, which `lightspeed <command> -h` explains.

*The capabilities are a table in a test.* `agentCapabilities` (internal/cli/capabilities_test.go)
lists the questions an agent asks about a codebase — file outline, symbol source, context bundle,
symbol and text search, tree and repo outline, importers, dependency graph and cycles, blast
radius, changed symbols, dead code, type and call hierarchy, implementations, related symbols,
rename and delete checks, churn, hotspots, task context — with the lightspeed command and MCP
tool that provide each. It fails if a named command is not in the table, is not exposed over
MCP, or a tool belongs to another command, and a second test fails if docs/AGENT-SETUP.md has
no row naming a command of the table. The doc is the human form of the table; the table is what
cannot silently rot. "Who references this" is `references`, the semantic question, and "who
imports this file" is `find_importers`, the import-graph question; the doc keeps them apart on
purpose because an agent that means the first is misled by an answer to the second.

*No hooks.* The setup installs no hook entries in `~/.claude/settings.json`. A hook in that
position would exist to keep a prebuilt index fresh after an edit or to nudge
the agent; lightspeed has no prebuilt index (every query revalidates against the disk and the
daemon tells its servers about outside changes, D32–D33), and the nudge is the guide. A hook
that pre-read files or re-indexed would be work with no effect, and one that blocked Read
would break the "Read before Edit" the harness itself requires. The document only tells the
user which files to edit and backs them up first; nothing edits `~/.claude`.

*Version and build.* `lightspeed version` (and `-v`, `--version`) prints the version, the
daemon's build identity, the guide version, Go, module and, when the toolchain embedded them,
VCS revision, time and modified flag. The version is stamped with
`-ldflags -X …/internal/cli.version` (the Makefile does it from `git describe`) and falls back
to the module version, then `dev+<revision>`; the MCP server reports the same string. It is a
tool too (`version`), so an agent can say which build it is talking to. The Makefile has
`build`, `test`, `vet`, `install`, `generate`, `check`, `clean` and a default `help`.

**Not decided here:** setup for
agents other than Claude Code; the guide and the MCP server are agent-neutral, the settings
steps are not.

## D44 — Review round 3: composed tools survive real gopls, an ambiguous name is not "unused", the budget is honoured (PLAN §4, M10)

*`blast_radius` on a type, constant or field.* A real gopls does not answer
`prepareCallHierarchy` with an empty list at a non-callable position, it answers a JSON-RPC
error ("Gate is not a function"). The fake server returned `[]`, so the tests missed it and
the whole verdict failed with `server_error` for the most common target. A `server_error`
from `prepareCallHierarchy` is now an answer about that position: the `call_hierarchy`
evidence is `not_applicable`, the server's message is a warning, and the references and
importers stay. Any other failure (not ready, timeout, crash) is not an answer and still
fails, as `rename_check` already treats `prepareRename`. The fake grew the gopls behaviour
(`wordSpec.NotCallable`) so that the test exercises the error path.

*`check_references` on an ambiguous name.* "used_only_in_text" for a name whose type has 17
semantic references was the wrong headline: nothing had been asked of the server for the
name as a whole. An ambiguous name now has its own verdict, `ambiguous` (exit 0: it is not
a finding of "nothing found"), and the first five candidates each get a `references` count,
one query each, so the caller learns whether any candidate is used without a second call. A
candidate that cannot be counted is left out of the counts with a warning, never a zero. The
alternative, running the full semantic query for every candidate and merging, was rejected:
it would turn one bare name into an unbounded number of queries and blur the rule that the
semantic and text lists are never merged (D38).

*`task_context` and its budget.* `budget.used` reported 628 of 300 because the fixed part of
the answer (the 1.2 KB confidence rule, the index report, the term list) was counted but never
shed. The rule is now one line in the answer (`--rule` prints the full text; the `reason` is
the per-answer evidence and stays), and when the fixed part plus the best symbol still exceed
the budget the index report, then the term list, are dropped and listed in `budget.dropped`.
A budget below the smallest answer cannot be met; the answer says so (`budget.over`, a
warning) rather than claiming to fit. `budget.used` still counts the data, not the envelope's
warnings. This does not touch the confidence rule itself (D41).

## D45 — Location results default to path + byte start/end, not uri + range + offset (token benchmark)

**Decision:** a location result (`definition`, `references`, `implementation`,
`workspace_symbol`, `symbols`, `call_hierarchy`) prints `path`, 1-based
`start`/`end` as `{"line":…,"column":…}`, and `text` by default. The LSP `uri`,
the UTF-16 `range` and the byte `offset` on `start`/`end` — always computed,
never wrong — are left out of the JSON unless `--verbose-locations` (MCP
`verbose_locations`) is given, which restores all three on every row.

**Why:** a token benchmark on this repository's own checkout (mcpbench.py,
docs/bench/) found `references` costing 478 tokens for one symbol with a
handful of references, and every row was
paying for three representations of the same span: `uri` (a `file://` restating
`path`), `range` (the server's own UTF-16 columns, of interest only to someone
debugging the LSP conversion itself) and `offset` on both `start` and `end`
(redundant with `line`+`column` for every consumer who is not re-slicing the
file by byte count). None of the three is what `references --symbol
Type.Method` or a location pasted back into `lightspeed` actually needs: that
is `path:line:col`. Cutting them dropped the same query to 269 tokens with
nothing an agent asks for removed — the hard rule this pass worked under is
that no information leaves the tool, only the default answer, so `range` and
`offset` stay one flag away.

**1-based byte columns are the canonical form, not the UTF-16 range**, because
every location a human or an agent *writes* — the command-line span syntax of
`file.go:12:5`, an MCP `location` parameter — is already byte columns (D21's
`Point`, vendored from gopls's own Mapper, PLAN §5.1). Making the default
answer echo back the same coordinate system it accepts is what makes a result
pasteable; printing UTF-16 characters by default would mean neither the common
case (ASCII, where byte and UTF-16 columns agree) nor the CJK/emoji case (where
`--verbose-locations` is what a caller debugging the conversion wants) is well
served by leaving `range` in every row.

**Implementation.** `render.Options` gained `VerboseLocations`; `resultsJSON`
builds either `[]compactResultView` (`path`, `start`/`end` without `offset`,
`text`, the optional `id`/`kind`/`detail`/`label`) or the existing
`[]resultView` (unchanged: `uri`, `path`, `range`, `start`/`end` with `offset`,
`text`, …) depending on it. `Result`/`Span` themselves are unchanged — every
field is still computed and available internally (`--context`, the diff
applier) — only the JSON encoding differs. `--verbose-locations` is one of
`commonFlags`, exposed through `queryParams()` (the same mechanism `--absolute`
uses, D30), so it reaches every command that answers with a result set;
commands whose rows were already compact (`check_references`'s `refRow`,
`blast_radius`'s `blastRow`, `search_text`'s `searchMatch`, none of which ever
carried `uri`/`range`/`offset`) are unaffected and do not need the flag.

**Not done:** diagnostics (`check`) keep their own `Range` field — SARIF
requires it, and `check`'s token cost was not what the benchmark measured;
`source`/`context`/`outline` describe a symbol, not a location, and already
print only `line`/`end_line` (D22), so they are out of this decision's scope.

## D46 — `task_context` and the index report default to their token-cheap shape (token benchmark)

**Decision:** `task_context`'s JSON answer defaults to id-only symbols (no
`name`, `kind`, `file`, `end_line`, `doc` or `evidence` — the id already
spells out name, kind and path, D21), no `terms` array, no composed `verdict`
sentence (`confidence` + `reason` already say it in one line), no inlined
source (`with_source` now defaults to 0), and at most 5 `related` rows, each
with only its identity (`of_index`, `relation`, `id`, `name`) instead of the
full row. `--rule` (MCP `rule`) restores all of it — the full confidence rule
text, the terms, the verdict, full symbol fields, unlimited related rows with
every field — in one flag, because these are one feature (task_context's
answer at full detail), not five. Separately, every index-backed command's
revalidation report (`files`/`fresh`/`touched`/`changed`/`added`/`removed`/
`skipped`/`uncovered`/`scan_ns`/`elapsed_ns`/`listed`) defaults to a one-line
`{"summary":"…","stale":bool}` in place of the full object; `--report` (MCP
`report`) restores it. `index status` is unaffected — the full report is that
command's entire job, so it never goes through this.

**Why:** the same token benchmark behind D45 measured `task_context` at
4258 tokens, the single largest answer in the whole set. Every field five of eleven benchmark
queries paid for was already reachable another way: `name`/`kind`/`file` are
the id parsed; `doc` and full `evidence` explain a *match*, not the code, and
`reason` already gives the one-sentence version; a `terms` array and a
composed `verdict` sentence are commentary on `confidence` + `reason`, not new
facts; a related row's full detail (kind, line, file, every evidence entry) is
one `related <id>` call away, and inlined source is one `source`/`context`
call away. None of it is *wrong* to want, which is why all of it stays behind
one flag rather than being deleted.

**Measured**, the exact query `mcpbench.py`/`calls.py` sends (`how does the
readiness gate decide an answer is authoritative`, default budget): the
pre-D46 answer (10 symbols, 3 inlined sources, unbounded related, full
terms/verdict, the full index report embedded) ran well past 4000 tokens on a
query this size; the first D46 pass got it to ~990 tokens, still short of the
700-token target, because `confidence_rule` was still carrying
`confidenceRuleShort` — a one-line but still ~70-token paragraph — in every
JSON answer, when the per-answer evidence a reader needs is already in
`reason`. A second pass replaced that paragraph with a bare rule identifier,
`confidenceRuleName` (`"term-coverage-v1"`, 4 tokens), in the JSON default
only; `--rule` still restores `confidenceRule` in full, and text output is
untouched (it keeps printing `confidenceRuleShort` — a human reads it, a byte
budget does not apply). That took the answer to **3150 bytes, ~788 tokens**
measured on this repository's own feature-branch worktree — the same
figure `docs/bench/baseline.json`'s `09_task` row pins, so `make bench`
catches the next regression against it — and to 2990 bytes, ~748 tokens,
measured against this repository's master checkout. Both are above the
700-token target (by about 13% and 7%), and the byte count is *not* only the
workspace root's path length folded into every `root`/`file` field (a real
but small effect, tens of bytes): this branch's own bench-only files
(`docs/bench/calls.py`, `internal/cli/bench_test.go`), absent from the master
checkout, change which symbols this query's ranker puts in the top 8 —
`docs/bench/calls.py`'s `TASK` constant, whose value is the literal benchmark
sentence, outranks a symbol master answers with instead — so the two
checkouts are not comparing byte-for-byte identical JSON, only the same
query and the same code. `files` (explicitly kept, D46 does not touch it) and
eight ranked symbols (`id`+`line`+`signature`+`score`) remain the two largest
sections, both intentionally: cutting either was outside this decision's
brief. `search_symbols`' `index` field (the other place a full `SyncReport`
was embedded, not just warned about) shrinks from a report the size of
`index status`'s whole answer to two short fields.

**Implementation.** `taskContextJSONView` embeds `taskContextData` and
shadows `Terms`, `Verdict`, `Rule`, `Symbols` and `Related` with `any`-typed
or `omitempty` fields — plain Go field shadowing (shallower wins), not a
JSON-specific trick, so it works with `encoding/json.Marshal` unchanged. Text
output renders `taskContextData` directly and never sees this type, so it is
unaffected, per this pass's rule that only MCP/JSON responses were in scope.
`compactSymbols`/`compactRelatedRows` build the trimmed views; the related cap
does not set `Budget.Truncated` (that field means the *token budget* bit,
tested independently) — a warning names how many rows and how to see them
instead. `indexReportField(rep, full bool) any` picks `*index.SyncReport` or
the new `*reportSummary{Summary, Stale}` (`SyncReport.Summary()`/`.Stale()`);
every index-backed command below calls it. `--report` is a `commonFlags`
boolean exposed through `commonParams`, the same mechanism
`--verbose-locations` (D45) and `--absolute` (D30) use.

`repo_map`, `find_importers`, `imports`, `dependency_graph` and
`dependency_cycles` get the same report compaction as `search_symbols` and
`task_context`: their result types (`RepoMapOutcome`, `ImportersOutcome`,
`ImportsOutcome`, `GraphOutcome`, `CyclesOutcome`, `internal/index/queries.go`)
still declare `Report *SyncReport` unconditionally — that field is the wire
format between the daemon and the CLI, so its JSON tag cannot be dropped
without breaking every caller — but each command's CLI-facing data struct
(`repoMapData`, `importersData`, `importsData`, `graphData`, `cyclesData`,
`internal/cli/index.go`) now declares its own `Report any
\`json:"report,omitempty"\`` field, set from `indexReportField` and shadowing
the embedded one, with the embedded `Report` nilled out first so nothing
stale is reachable through it. `commonParams` for these five commands now
includes `"report"`, so an MCP client can set it.

## D47 — `context`'s header stays capped at 80 lines; hover that is only the signature is omitted (token benchmark)

**Decision:** `--max-header-lines` keeps its existing default of 80 — already
"the package clause and imports" by construction (`fileHeader` stops at the
file's first declaration, D22), so a typical Go file's header is 5–20 lines
and the cap rarely bites; it was already sane and is left alone. New: a
hover that is nothing but a fenced code block holding exactly the
declaration's own signature — `signature` and `source`'s first line already
carry that — is omitted, with a warning saying so; a hover that adds
anything else (doc prose, a method list, a doc link) is kept whole.

**Why, and what the benchmark showed.** The token benchmark's `context` query
(`internal/client/gate.go::Gate#struct`, this repository) measured 1165
tokens, the third-largest answer in the set. Breaking the
answer down: `header` is 224 bytes (already tight, not the cause); `source`
is 1452 bytes (the symbol's own 31-line body — this is what was asked for,
not overhead); `hover` is the largest single field at over 1600 bytes,
because gopls's hover for a documented struct restates the struct body in a
second fenced code block, restates the full doc comment as prose, and adds
the method list and a pkg.go.dev link. Only the first of those four is a pure
duplicate of `source`; the doc-comment restatement is *not* byte-identical to
`source`'s doc comment (gopls re-wraps it), and the method list and link are
new information `source` does not carry. **The dedup this decision makes is
therefore narrower than the whole hover**: it fires when hover is *only* the
signature — the common case for an undocumented symbol, where gopls (and
most servers) answer nothing else — not when hover also documents. Measured
on this repository: an undocumented function's hover
(`` ```go\nfunc matchNote(t taskTerm) string\n``` ``) is now omitted
entirely, saving the whole field; the benchmark's own id, `Gate#struct`, has
a doc comment and a method list, so its hover survives unchanged and the
benchmark number does not move. This is stated plainly rather than claimed
as a fix it is not: the benchmark's 2.3x is dominated by `source` (the
symbol genuinely has a 31-line body) and the richer half of `hover`
(doc-restatement, methods, link), neither of which this decision touches.

**Implementation.** `hoverIsJustSignature(hover, signature string) bool` in
`internal/cli/source.go`: trims hover, requires a single fenced block
(``` ```lang\n…\n``` ``` or ``` ```\n…\n``` ```, the language tag stripped
when present), and compares the block's content, trimmed, against
`data.Signature` — not literally "the first line of `source`" as raw bytes,
because `source`'s first line is the *doc comment* for any documented symbol
(the declaration is further down) and `Signature` is already exactly "the
declaration as written" that a plain-signature hover restates; comparing
against it is comparing against the same content the first *code* line of
`source` would be. No flag was added: nothing is lost by the omission (the
identical text is still in the answer, once, as `signature`), so there is
nothing to restore.

**Not done:** deduping the richer hover case (its doc paragraph against
`source`'s comment, its embedded code block against `source`'s body when the
symbol *is* documented) — that would mean parsing gopls's hover markdown
structurally rather than pattern-matching a single fenced block, a larger
change than this pass's budget, and the review that measured 2.3x did not
ask for it by name; `--max-header-lines`'s default was checked, not changed,
because nothing measured showed it was not already sane.

## D48 — `tree` and `repo_outline` keep a `language` on every row (token benchmark)

**Decision:** left as is, not cut. `tree`'s `files[]` keeps `language` on
every entry; `repo_outline`'s `directories[]` keeps its per-directory
`languages` breakdown. Neither drops anything, and this entry is the "say
why" the goal that asked for the cut allowed for.

**Why.** The idea on the table was: drop a file's `language` in `tree` when
it equals its directory's dominant language, stating the dominant language
once per directory instead. Two things ruled it out.

First, it does not apply where the goal expected it to help most.
`repo_outline`'s `languages` field is *already* a per-directory aggregate
(`{"go":299,"json":16,"lua":6,…}` for this repository's root) — there is no
per-file `language` in `repo_outline` to begin with, so there is nothing to
deduplicate against a directory default there. The only place with an actual
per-file `language` is `tree`'s flat file list.

Second, where it does apply, it fails the goal's own condition. `tree` lists
every file in one flat, path-sorted array; a directory's "dominant language"
would have to live in a second structure (a per-directory map, or a running
header) that a JSON consumer joins against each file's path prefix to learn
what a missing `language` means. That is not "trivially consumable" — it
trades one self-describing field per row for a cross-reference a consumer
must compute, which is exactly what the hard rule behind this whole pass
(JSON stays self-describing) rules out paying for a token saving that, on
this repository, is not needed anyway: **the benchmark's `tree` query** (4027 tokens) **is the one
line of the set nothing had flagged as too large** before this pass
touched it. Cutting self-description
from the one query that was not asking for it, for a saving nobody needed,
was the wrong trade.

**Not done, and not needed:** grouping `tree`'s files by directory instead of
listing them flat would let a per-directory default work cleanly, but that is
a different, larger change than "drop a redundant field," and nothing in the
benchmark asked for it.

