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
