# Corpus snapshot: nvim-lspconfig

Upstream:   https://github.com/neovim/nvim-lspconfig
Commit:     bff1bd61cb1455040533201ca1edf1e84efa578f
Committed:  2026-08-19T18:24:55Z
License:    Apache License 2.0 — see LICENSE.md, copied verbatim from the
            same commit. Apache-2.0 permits redistribution and derivative
            works with attribution; the derived table in
            internal/serverdef/builtins_gen.go and the repository-root
            ATTRIBUTION file carry that attribution.
Snapshot:   the six server configurations PLAN.md §8 M4 requires, and
            nothing else. The upstream `lsp/` directory holds 411 files;
            vendoring all of them would trade a build-time fetch (which
            PLAN.md forbids) for a large unreviewed blob, which is no
            better.

Files, verbatim, with the sha256 they were copied with:

    a6cba85bc92e0cff7a450b1d873c0eaa2e9fc96bf472df0247a26bec77bf3ff9  LICENSE.md
    eed1bfb65d706e739d73e33c17ccbeaf29856ffa093eca0715fb8a560e9a8285  lsp/clangd.lua
    9ce957e30e4a5206a1fbb91ef42d2babb266b1c240c6a400bccc1471eb057de6  lsp/gopls.lua
    bf63a7cac2b94afc047e2b75a10284bd86af41c1b312b124a98c42bdfd8ddf66  lsp/lua_ls.lua
    2fb628e69752f027785893687cd929ad4842d560df926b39996f8ddf7b7b299b  lsp/pyright.lua
    870eeaeb31157866442383a6fa6eed441bdebfa1350e33112f7ec879a7db645d  lsp/rust_analyzer.lua
    e01d83a0d80eed0baec2890d034ea42924a2a5129d28f85aad34341ca665eb7b  lsp/vtsls.lua

`internal/gen/corpus_test.go` re-checks these digests, so a file that is
edited in place — the tempting way to "fix" a built-in — fails the tests
instead of quietly making the snapshot a fork.

## Why this corpus and not Helix

PLAN.md §9.3 leaves the choice open and flags Helix's `languages.toml` as
MPL-2.0, whose file-level copyleft would attach to whatever file the data
is embedded in. nvim-lspconfig is Apache-2.0, which has no such clause, so
it is the corpus with no open licensing question. The generator therefore
never reads Helix data, and the question in §9.3 can stay unanswered
without blocking M4.

## Refreshing the snapshot

    git -C /path/to/nvim-lspconfig checkout <commit>
    cp /path/to/nvim-lspconfig/lsp/<name>.lua internal/gen/corpus/nvim-lspconfig/lsp/
    cp /path/to/nvim-lspconfig/LICENSE.md      internal/gen/corpus/nvim-lspconfig/
    sha256sum LICENSE.md lsp/*.lua   # update the digests above and in corpus_test.go
    go generate ./internal/serverdef/

Nothing in the build fetches anything: the generator reads only this
directory, embedded with go:embed, so `go generate` works offline and on a
machine that has never seen Neovim.
