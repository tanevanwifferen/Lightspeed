# Token-budget bench

Two ways to measure what a query costs an agent, in the same eleven
questions: a committed regression test that runs on every `make bench`, and
a manual harness that drives any MCP stdio server — lightspeed, another
build of lightspeed, or another server entirely — with the same questions.

## `make bench`: the regression test

```sh
make bench
```

runs `internal/cli/bench_test.go`'s `TestBenchTokenBudget`: it asks
`benchQueries` (the same eleven questions as `calls.py`'s `ls` list, kept in
step by hand) through the CLI's default JSON — byte-for-byte what
`lightspeed mcp`'s tool of the same name answers, docs/DECISIONS.md D20, so
there is one code path to measure and this test does not also reimplement
the MCP wire protocol — against this repository, with a real gopls. It fails
a query whose answer grew more than 10% past `baseline.json`.

**Token count: bytes and words, not a real tokenizer.** No `tiktoken`
dependency was pulled in for a 10%-regression gate; bytes/4 is the same
rough estimate `task_context`'s own `--budget` already uses internally, and
is what the token counts in docs/DECISIONS.md (D45–D48) and PLAN.md were
computed with. `words` (whitespace-split) rides along as a second,
tokenizer-independent signal in the log but is not what the 10% gate checks
— JSON's punctuation-heavy shape makes bytes the steadier proxy of the two
for a GPT-style tokenizer.

It needs a real, installed gopls (see `lightspeed doctor`, or `mise use -g
go:golang.org/x/tools/gopls@v0.23.0`) and skips cleanly without one, under
`go test -short`, and with `LIGHTSPEED_SKIP_PERF=1` — the same conditions as
`TestIndexPerformanceAgainstThisRepo` (`internal/cli/perf_test.go`), since
both are measurements against the real world, not hermetic behaviour tests.

Grown a query's answer on purpose (a new field, a richer default)?

```sh
go test ./internal/cli/ -run TestBenchTokenBudget -update-bench
```

rewrites `baseline.json` with what the run measured. Say why in the commit
that does it — a regenerated baseline with no explanation is indistinguishable
from a regression nobody looked at.

## Comparing two servers by hand

`calls.py` and `mcpbench.py` are the harness the numbers in
docs/DECISIONS.md D45–D48 were measured with. `mcpbench.py` speaks plain MCP
over stdio, so it can drive any server that does.

```sh
export LIGHTSPEED_BENCH_WORKSPACE=/path/to/this/checkout   # default: cwd

python3 calls.py /tmp/bench-out                            # writes ls_calls.json

python3 mcpbench.py 'lightspeed|mcp' /tmp/bench-out/out_ls /tmp/bench-out/ls_calls.json
```

Each run prints one line per query — label, bytes, milliseconds, whether the
call errored — and saves the raw answer text to
`<outdir>/<label>.txt`, for reading by eye or diffing between two lightspeed
builds (run the same command against each build into two output directories
and `diff -r` them).

To compare against another MCP stdio server, add a second call list to
`calls.py` (there is a commented example) with the same labels in the same
order, each row carrying that server's tool name and arguments for the same
question, and drive it with the same script:

```sh
python3 mcpbench.py '<command>|<arg>|<arg>' /tmp/bench-out/out_other /tmp/bench-out/other_calls.json
# (spell the command however that server starts: a venv's python, a wrapper
# script, an npx invocation — whatever `claude mcp list` already runs for it.)
```

Row *i* of one list is compared against row *i* of the other, so both must
stay the same length and order. Output directories are a local run's scratch
and are not committed.

**To ask a different question:** edit the list in `calls.py` and, if you
want the new question in the regression test too, add it to `benchQueries`
in `internal/cli/bench_test.go` and regenerate the baseline.

## Files

| file | what it is |
|---|---|
| `calls.py` | the eleven questions as lightspeed tool calls — `python3 calls.py <outdir>` writes `ls_calls.json`; a commented example shows how to add a second server's list |
| `mcpbench.py` | drives one MCP server over stdio with a `calls.json`, prints byte sizes and timings, saves each answer's text to `<outdir>/<label>.txt` (not committed — a local run's scratch output) |
| `baseline.json` | the committed reference `make bench` checks new runs against: `[{"label","bytes","words"}, …]` |
