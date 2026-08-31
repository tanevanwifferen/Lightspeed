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
