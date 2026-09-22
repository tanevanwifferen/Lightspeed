package cli

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBenchTokenBudget is `make bench`: it re-runs docs/bench/calls.py's
// lightspeed call list against this build, on this repository, through the
// real command path (byte-for-byte what the MCP tool of the same name would
// answer over stdio, docs/DECISIONS.md D20 — so this test does not also
// speak the MCP wire protocol; there is one definition of each query and one
// path that answers it, and re-running that path is what the benchmark it
// guards actually measured), and fails a query whose DEFAULT JSON answer
// grew more than benchRegressionFraction past the committed baseline
// (docs/bench/baseline.json).
//
// Token count: bytes and words, not a real tokenizer. docs/bench/README.md
// says why (no tiktoken dependency; bytes/4 is the same rough estimate
// task_context's own --budget uses, and is what this repo's benchmark numbers
// in docs/DECISIONS.md D45-D48 were computed with); words is carried
// alongside as a second, tokenizer-independent signal. Bytes is the metric
// the 10% gate checks.
//
// It needs a real, installed gopls and is skipped, cleanly, without one,
// under -short, and with LIGHTSPEED_SKIP_PERF=1 — the same conditions
// TestIndexPerformanceAgainstThisRepo uses (perf_test.go), because this is
// the same kind of test: a measurement against the real world, not a proof
// of behaviour, and the two share a machine dependency.
//
// Run it with `make bench`, or `go test ./internal/cli/ -run
// TestBenchTokenBudget -v`. -update-bench rewrites docs/bench/baseline.json
// with what this run measured, instead of checking against it.
func TestBenchTokenBudget(t *testing.T) {
	if testing.Short() || os.Getenv("LIGHTSPEED_SKIP_PERF") == "1" {
		t.Skip("bench skipped")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("not running inside the lightspeed repository")
	}
	t.Chdir(root)
	daemonEnv(t, "120s")
	t.Setenv(fakeServerModeEnv, "")

	// One warm-up query builds the index once; every measured query after it
	// runs against a warm index and a warm gopls, which is the state a coding
	// agent's session is normally in — and the state the token benchmark this
	// guards was itself measured in.
	if code, out, stderr := runMain("index", "build", "--timeout", "120s"); code != ExitOK {
		t.Skipf("index build did not succeed (gopls not usable here?): exit %d\n%s%s", code, out, stderr)
	}

	got := make([]benchEntry, 0, len(benchQueries))
	for _, q := range benchQueries {
		code, out, stderr := runMain(q.Args...)
		if code != ExitOK && code != ExitProblems {
			t.Fatalf("%s (%v): exit %d\n%s%s", q.Label, q.Args, code, out, stderr)
		}
		entry := benchEntry{Label: q.Label, Bytes: len(out), Words: len(strings.Fields(out))}
		got = append(got, entry)
		t.Logf("%-18s %6d bytes  %5d words", entry.Label, entry.Bytes, entry.Words)
	}

	baselinePath := filepath.Join(root, "docs", "bench", "baseline.json")
	if *updateBench {
		writeBenchBaseline(t, baselinePath, got)
		return
	}

	baseline := readBenchBaseline(t, baselinePath)
	byLabel := make(map[string]benchEntry, len(baseline))
	for _, b := range baseline {
		byLabel[b.Label] = b
	}
	for _, g := range got {
		base, ok := byLabel[g.Label]
		if !ok {
			t.Errorf("%s: no baseline entry in %s; run with -update-bench", g.Label, baselinePath)
			continue
		}
		limit := int(float64(base.Bytes) * (1 + benchRegressionFraction))
		if g.Bytes > limit {
			t.Errorf("%s: %d bytes, more than %.0f%% over the %d-byte baseline (limit %d); "+
				"run `go test ./internal/cli/ -run TestBenchTokenBudget -update-bench` if this growth is intended, and say why in the commit",
				g.Label, g.Bytes, benchRegressionFraction*100, base.Bytes, limit)
		}
	}
	for _, b := range baseline {
		if _, ok := benchIndex[b.Label]; !ok {
			t.Errorf("baseline has %q, which benchQueries no longer asks; remove it with -update-bench", b.Label)
		}
	}
}

// updateBench rewrites docs/bench/baseline.json with the run's own
// measurements instead of checking against the committed one.
var updateBench = flag.Bool("update-bench", false, "rewrite docs/bench/baseline.json instead of checking against it")

// benchRegressionFraction is how much larger than the baseline a query's
// default answer may get before the bench fails: 10%, the threshold the
// goal this bench exists for set.
const benchRegressionFraction = 0.10

// benchQuery is one question docs/bench/calls.py's `ls` list also asks, as
// the CLI argument vector that asks it (the positionals and flags after
// `lightspeed`, `--format json` implied by the test harness's non-terminal
// stdout, exactly as an MCP call's argv is built, D20).
type benchQuery struct {
	Label string
	Args  []string
}

// benchQueries mirrors docs/bench/calls.py's `ls` list: the same eleven
// questions, so that comparing this test's numbers with a hand run of
// mcpbench.py against `lightspeed mcp` compares like with like. The two
// lists are kept in step by hand — this one drives CI, calls.py drives a
// human comparison against another MCP server (docs/bench/README.md).
var benchQueries = []benchQuery{
	{"01_outline", []string{"outline", "internal/cli/cli.go"}},
	{"02_source", []string{"source", "internal/cli/command.go::init#function"}},
	{"03_references", []string{"references", "--id", "internal/cli/cli.go::usage#function"}},
	{"04_search_symbols", []string{"search_symbols", "readiness gate", "--limit", "10"}},
	{"05_search_text", []string{"search_text", "NotReadyError", "--limit", "20"}},
	{"06_tree", []string{"tree"}},
	{"07_repo_outline", []string{"repo_outline"}},
	{"08_importers", []string{"find_importers", "internal/render"}},
	{"09_task", []string{"task_context", "how does the readiness gate decide an answer is authoritative"}},
	{"10_context", []string{"context", "internal/client/gate.go::Gate#struct"}},
	{"11_resolve", []string{"index", "status"}},
}

// benchIndex is benchQueries by label, built once.
var benchIndex = func() map[string]benchQuery {
	m := make(map[string]benchQuery, len(benchQueries))
	for _, q := range benchQueries {
		m[q.Label] = q
	}
	return m
}()

// benchEntry is one committed measurement in docs/bench/baseline.json.
type benchEntry struct {
	Label string `json:"label"`
	Bytes int    `json:"bytes"`
	Words int    `json:"words"`
}

func readBenchBaseline(t *testing.T, path string) []benchEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v (run with -update-bench to create it)", path, err)
	}
	var baseline []benchEntry
	if err := json.Unmarshal(raw, &baseline); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	return baseline
}

func writeBenchBaseline(t *testing.T, path string, entries []benchEntry) {
	t.Helper()
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	t.Logf("wrote %s", path)
}
