import json, os, sys

# W is the absolute path to the lightspeed workspace (this checkout, typically).
# Override with an env var for your machine; the default is the current directory.
W = os.environ.get("LIGHTSPEED_BENCH_WORKSPACE", os.getcwd())
TASK = "how does the readiness gate decide an answer is authoritative"
ls = [
 {"label":"01_outline",        "tool":"outline",        "args":{"workspace":W,"files":["internal/cli/cli.go"]}},
 {"label":"02_source",         "tool":"source",         "args":{"workspace":W,"id":"internal/cli/command.go::init#function"}},
 {"label":"03_references",     "tool":"references",     "args":{"workspace":W,"id":"internal/cli/cli.go::usage#function"}},
 {"label":"04_search_symbols", "tool":"search_symbols", "args":{"workspace":W,"query":"readiness gate","limit":10}},
 {"label":"05_search_text",    "tool":"search_text",    "args":{"workspace":W,"query":"NotReadyError","limit":20}},
 {"label":"06_tree",           "tool":"tree",           "args":{"workspace":W}},
 {"label":"07_repo_outline",   "tool":"repo_outline",   "args":{"workspace":W}},
 {"label":"08_importers",      "tool":"find_importers", "args":{"workspace":W,"target":"internal/render"}},
 {"label":"09_task",           "tool":"task_context",   "args":{"workspace":W,"task":TASK}},
 {"label":"10_context",        "tool":"context",        "args":{"workspace":W,"id":"internal/client/gate.go::Gate#struct"}},
 {"label":"11_resolve",        "tool":"index_status",   "args":{"workspace":W}},
]

# To compare against another MCP stdio server, add a second list with the same
# labels in the same order, each row carrying that server's tool name and
# arguments for the same question, and dump it beside ls_calls.json. For example:
#
# other = [
#  {"label":"01_outline", "tool":"<its outline tool>", "args":{"<its args>":"internal/cli/cli.go"}},
#  ...one row per label above...
# ]
# json.dump(other, open(sys.argv[1]+"/other_calls.json","w"))
#
# Row i of one list is compared against row i of the other, so both must stay
# the same length and order.

# Usage: python3 calls.py <outdir>   -- writes <outdir>/ls_calls.json, which
# mcpbench.py reads. Edit the list above (and the queries in
# internal/cli/bench_test.go's benchQueries, kept in step by hand) to bench a
# different question.
json.dump(ls, open(sys.argv[1]+"/ls_calls.json","w"))
