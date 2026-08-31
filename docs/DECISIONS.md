# Decisions

Running log of decisions that resolve open questions from PLAN.md.

## D1 — Module path (PLAN §9.1)

**Decision:** `github.com/owner/lightspeed`.

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
