package cli

// gitCommands are the git-aware analysis commands (docs/DECISIONS.md D40):
// changed_symbols, churn, hotspots and related. They shell out to git the way
// D8's dirty-worktree check does, and outside a repository they answer with what
// they can and a warning.
func gitCommands() []*command {
	return []*command{
		{
			Name:    "changed_symbols",
			Args:    "[--base <rev>] [--staged] [--path P]",
			Summary: "print the symbols a diff added, modified or removed, mapped from git's hunks",
			Run:     changedSymbolsCommand,
			MCP: oneTool("changed_symbols", "Symbols a diff added, modified or removed, by id: git hunks mapped onto symbol ranges. Working tree vs HEAD by default; base/staged change that.",
				[]param{
					{Flag: "base", Type: typeString, Desc: "Compare against this revision instead of HEAD."},
					{Flag: "staged", Type: typeBool, Desc: "Only the staged changes: the index against the base."},
					{Flag: "path", Type: typeString, Path: true, Desc: "Only changes under this directory, or this file, inside the workspace."},
					indexRootParam(),
				}, commonParams("limit", "timeout", "server")),
		},
		{
			Name:    "churn",
			Args:    "[path] [--since <d>]",
			Summary: "print how much files, and the symbols in them, changed in a window of git history",
			Run:     churnCommand,
			MCP: oneTool("churn", "Commits, authors and lines changed per file (and per symbol for the top files) over a git window (default 90 days). Truncation is reported.",
				churnParams(false), commonParams("limit", "timeout", "server")),
		},
		{
			Name:    "hotspots",
			Args:    "[--since <d>] [--path P]",
			Summary: "rank files and symbols by churn times size: what changes often and is big",
			Run:     hotspotsCommand,
			MCP: oneTool("hotspots", "Code files and symbols ranked by churn x size over a git window; the formula is in the answer. Docs and data files are left out unless all.",
				churnParams(true), commonParams("limit", "timeout", "server")),
		},
		{
			Name:    "related",
			Args:    "<loc> [--per-source N]",
			Summary: "print what is related to a symbol: siblings, callers and callees, co-changed files, similar names",
			Method:  methodPrepareCallHierarchy,
			Run:     relatedCommand,
			MCP: oneTool("related", "What to look at next to a symbol: same-file siblings, direct callers/callees, co-changed files, similar names; each row names its evidence.",
				locationParams(pointDesc), []param{
					{Flag: "per-source", Type: typeInt, Min: intp(1), Desc: "At most this many rows from each kind of evidence (default 5)."},
					{Flag: "history", Type: typeInt, Min: intp(1), Max: intp(maxRelatedHistory), Desc: "How many recent commits the co-change evidence reads (default 500)."},
				}, commonParams("limit", "timeout", "server")),
		},
	}
}

// churnParams are the parameters of churn and hotspots: they share the window and
// the per-symbol attribution, and differ in how the path is given.
func churnParams(hot bool) []param {
	ps := []param{
		{Flag: "since", Type: typeString, Desc: "The window, 30d, 2w, 12h, 6m, 1y, or anything git reads as a date: '30 days', '2 weeks ago', 2026-01-31 (default 90 days)."},
		{Flag: "symbol-files", Type: typeInt, Min: intp(0), Max: intp(maxSymbolFiles), Desc: "Attribute history to symbols for this many of the most-changed files (default 15, 0 for files only)."},
		indexRootParam(),
	}
	if hot {
		return append(ps,
			param{Flag: "path", Type: typeString, Path: true, Desc: "Only files under this directory, or this file, inside the workspace."},
			param{Flag: "all", Type: typeBool, Desc: "Rank every file, not only code: docs (markdown), data and config (json, yaml, toml, lock files) too."})
	}
	return append(ps, param{Name: "path", Type: typeString, Path: true, Desc: "Directory or file to restrict to, inside the workspace (default: the workspace)."})
}
