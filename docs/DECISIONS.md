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
