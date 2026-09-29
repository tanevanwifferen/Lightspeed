"""What an agent reads to answer a question: without lightspeed, and with it.

Each scenario is one everyday question an agent asks about this repository,
answered twice:

  without  the way a coding agent does it with built-in tools: Read a whole
           file (numbered lines, as Claude Code's Read returns them), grep -rn
           for a name, list every file.
  with     the one lightspeed call that answers it, in its default JSON --
           byte for byte what the MCP tool of the same name returns.

The "without" column counts only the first step. An agent that greps usually
opens some of the files it found next, so the real cost of that path is
higher, never lower.

Tokens are estimated as bytes / 4, the same estimate as bench_test.go and
task_context's --budget. It is not a real tokenizer: read the ratios, not the
absolute numbers.

Usage, from the root of this checkout with lightspeed and gopls installed:

    python3 docs/bench/savings.py            # a markdown table
    python3 docs/bench/savings.py --json     # the raw numbers
"""

import json
import subprocess
import sys

MODULE = "github.com/tanevanwifferen/Lightspeed"


def read(path):
    """A whole file with numbered lines, the shape of Claude Code's Read."""
    return f"awk '{{printf \"%6d\\t%s\\n\", NR, $0}}' {path}"


def ls(*args):
    return ["lightspeed", *args, "--format", "json"]


SCENARIOS = [
    {
        "question": "Read one method (`Pool.Acquire`)",
        "without": [read("internal/daemon/pool.go")],
        "with": ls("source", "internal/daemon/pool.go::Pool.Acquire#method"),
    },
    {
        "question": "What is in this file? (`internal/cli/index.go`)",
        "without": [read("internal/cli/index.go")],
        "with": ls("outline", "internal/cli/index.go"),
    },
    {
        "question": "A type, its docs and the file's imports (`Gate`)",
        "without": [read("internal/client/gate.go")],
        "with": ls("context", "internal/client/gate.go::Gate#struct"),
    },
    {
        "question": "Where is `ReapIdle` defined?",
        "without": ["grep -rn ReapIdle --include='*.go' ."],
        "with": ls("search_symbols", "ReapIdle", "--limit", "5"),
    },
    {
        "question": "Who calls `Pool.Acquire`?",
        "without": ["grep -rnw Acquire --include='*.go' ."],
        "with": ls("references", "--id", "internal/daemon/pool.go::Pool.Acquire#method"),
    },
    {
        "question": "What breaks if I change `edit.Apply`?",
        "without": ["grep -rn 'Apply(' --include='*.go' ."],
        "with": ls("blast_radius", "--id", "internal/edit/stage.go::Apply#function"),
    },
    {
        "question": "Who imports `internal/render`?",
        "without": [f"grep -rn '\"{MODULE}/internal/render\"' --include='*.go' ."],
        "with": ls("find_importers", "internal/render"),
    },
    {
        "question": "Where is the readiness gate implemented?",
        "without": [
            "grep -rli readiness --include='*.go' .",
            read("internal/client/gate.go"),
        ],
        "with": ls("task_context", "how does the readiness gate decide an answer is authoritative"),
    },
    {
        "question": "Give me an overview of the repository",
        "without": ["git ls-files"],
        "with": ls("repo_outline"),
    },
]


def run_shell(cmds):
    return sum(len(subprocess.run(c, shell=True, capture_output=True).stdout) for c in cmds)


def run_ls(argv):
    out = subprocess.run(argv, capture_output=True)
    # Exit 1 is an authoritative answer ("problems found"); anything else is not a result.
    if out.returncode not in (0, 1):
        sys.exit(f"{' '.join(argv)}: exit {out.returncode}\n{out.stdout.decode()}{out.stderr.decode()}")
    return len(out.stdout)


def tokens(n):
    return round(n / 4)


def main():
    rows = []
    for s in SCENARIOS:
        without, with_ = run_shell(s["without"]), run_ls(s["with"])
        rows.append({"question": s["question"], "without": tokens(without), "with": tokens(with_)})

    if "--json" in sys.argv:
        json.dump(rows, sys.stdout, indent=2)
        print()
        return

    total_without = sum(r["without"] for r in rows)
    total_with = sum(r["with"] for r in rows)
    print("| question | without | with lightspeed | saved |")
    print("|---|---:|---:|---:|")
    for r in rows:
        saved = 1 - r["with"] / r["without"]
        print(f"| {r['question']} | {r['without']:,} | {r['with']:,} | {saved:.0%} |")
    print(f"| **all nine** | **{total_without:,}** | **{total_with:,}** | **{1 - total_with / total_without:.0%}** |")


if __name__ == "__main__":
    main()
