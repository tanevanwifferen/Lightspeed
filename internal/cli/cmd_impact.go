package cli

// The impact commands answer "what does this touch?" from evidence that
// already exists: blast_radius (what changing a symbol or file affects) and
// dead_code (what nothing uses). docs/DECISIONS.md D39.

// impactCommands are the table entries of blast_radius and dead_code.
func impactCommands() []*command {
	return []*command{
		{
			Name:    "blast_radius",
			Args:    "<loc|file>",
			Summary: "what changing a symbol (--symbol, --id or a location) or a file affects: references, transitive callers, importers, tests",
			Method:  methodReferences,
			Run:     blastRadiusCommand,
			MCP: oneTool("blast_radius", blastRadiusDescription,
				locationParams("A file (its importers), or file:line:col of a symbol (references, callers, importers of its file). Give this, symbol or id."),
				[]param{
					{Flag: "depth", Type: typeInt, Min: intp(1), Max: intp(maxCallDepth),
						Desc: "Levels of transitive callers to follow (default 2)."},
					{Flag: "import-depth", Type: typeInt, Min: intp(1), Max: intp(maxCallDepth),
						Desc: "Levels of importing files to follow (default: the same as depth)."},
				},
				commonParams("limit", "timeout", "server", "absolute")),
		},
		{
			Name:    "dead_code",
			Args:    "[dir]",
			Summary: "candidates for unused code: symbols with no references outside their own declaration",
			Method:  methodReferences,
			Run:     deadCodeCommand,
			MCP: oneTool("dead_code", deadCodeDescription,
				[]param{
					{Name: "dir", Type: typeString, Path: true,
						Desc: "Directory or file to examine, inside the workspace (default: the whole workspace). Same as path."},
					{Flag: "path", Type: typeString, Path: true,
						Desc: "Directory or file to examine, inside the workspace (default: all of it)."},
					{Flag: "kind", Type: typeStrings,
						Desc: "Symbol kinds to examine (default: function, method, class, struct, interface, enum, constant)."},
					{Flag: "include-exported", Type: typeBool,
						Desc: "Also examine exported symbols (public API); their callers outside this workspace are invisible, so they are reported with low confidence."},
					{Flag: "budget", Type: typeInt, Min: intp(1),
						Desc: "Most reference queries this call makes (default 100). When it runs out, next_cursor continues."},
					{Flag: "cursor", Type: typeString,
						Desc: "Continue after this candidate: the next_cursor of the previous answer."},
					indexRootParam(),
				},
				commonParams("limit", "timeout", "server", "absolute")),
		},
	}
}

const (
	blastRadiusDescription = "What changing a symbol or file affects: summary, then rows (references, transitive callers, importers, tests), each naming its evidence. Bounds are reported."
	deadCodeDescription    = "Unused-code candidates (zero references outside the declaration; entry points, tests, exports, implementations excluded). Resumable; each has a confidence."
)
