package cli

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// An env is the ambient I/O a command runs against. Every command
// writes its machine-readable answer to stdout and nothing else; human
// diagnostics, server logs and usage text go to stderr, so that a
// caller can parse stdout without filtering it.
type env struct {
	// stdin is the query stream `batch` reads. It is nil for a command
	// run inside a batch, which is what makes `batch` inside `batch`
	// impossible rather than merely discouraged.
	stdin  io.Reader
	stdout io.Writer
	stderr io.Writer
	// noDaemon keeps the command's server in this process instead of
	// the workspace's shared daemon: PLAN §3's --no-daemon, or
	// LIGHTSPEED_NO_DAEMON in the environment. It is per invocation, so
	// a batch's queries inherit it from the batch that ran them.
	noDaemon bool
	// offline is --offline, PLAN §6's kill switch over anything that
	// could touch the network. serverdef ORs it with LIGHTSPEED_OFFLINE
	// (see env.serverdefOptions), so the environment cannot be overridden
	// by leaving the flag off. Like noDaemon it is per invocation and a
	// batch's queries inherit it.
	offline bool
	// configs caches the server definitions this invocation has loaded,
	// per workspace root. Use env.configCache, which makes it.
	configs *configCache
	// ctx bounds the whole invocation. The command line has none (nil is
	// context.Background, and a process that is killed needs no
	// cancelling); an MCP call runs under the request's context, so that a
	// client that cancels or hangs up aborts the call instead of leaving it
	// to run to its own timeout. Use env.base, never ctx directly.
	ctx context.Context
	// overMCP says the command is a tool call of `lightspeed mcp`, not a command
	// line. Only what describes the interface itself reads it: `guide` names
	// tools and their parameters there, and commands and flags here.
	overMCP bool
	// onArity, when set, is told the positional-argument count every command
	// asks parseFlagsRange to enforce, before it enforces it. It is the seam
	// that lets TestParamSpecMatchesCLIFlags compare a parameter spec with
	// what the command really accepts instead of with a second description of
	// it; nothing in production sets it.
	onArity func(min, max int)
}

// base is the context every request the command makes is derived from.
func (e *env) base() context.Context {
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// fail renders err as a failed envelope on stdout and returns the
// process exit code for it. Both the code and the exit status come
// from internal/render, so the taxonomy of PLAN §4 cannot drift
// between subcommands.
func (e *env) fail(err error) int {
	_ = render.FailError(e.stdout, err)
	return render.ExitCode(err)
}

// usagef renders a usage error (exit 2) with a formatted message.
func (e *env) usagef(format string, args ...any) int {
	return e.fail(render.Errorf(render.CodeUsage, format, args...))
}

// A command is one subcommand of PLAN §4's read-only surface.
//
// Method is the LSP request the command is built on, and it is the
// reason this table exists rather than a switch: a server's advertised
// capabilities decide which of these commands can actually answer, so
// `--help` is generated from Method + the server's InitializeResult
// instead of being written down twice (PLAN §4, last line of the
// command-surface block).
type command struct {
	// Name is the subcommand as typed.
	Name string
	// Args is the argument summary for usage text.
	Args string
	// Summary is the one-line description.
	Summary string
	// Method is the LSP request the command needs, or "" for a
	// command that talks to no server.
	Method string
	// Run executes the command and returns the process exit code.
	// It is handed its own table entry so that the table can be a
	// package-level literal without a lookup cycle.
	Run func(e *env, c *command, args []string) int
	// MCP lists the tools the command provides to an MCP client, each with
	// its parameters (params.go). It is the only thing a new command has
	// to declare to become one: internal/cli/mcp.go builds the tool, its
	// schema and its handler from it.
	MCP []toolSpec
	// NoMCP is the reason the command is not an MCP tool. Every command
	// has exactly one of MCP and NoMCP, and a test fails for one that has
	// neither, so that leaving a command off the agent surface is a
	// decision somebody wrote down rather than something nobody noticed.
	NoMCP string
}

// oneTool is the MCP surface of a command that is one tool named after it.
func oneTool(name, description string, params ...[]param) []toolSpec {
	return []toolSpec{{Name: name, Description: description, Params: withParams(params...)}}
}

// Capability reports the InitializeResult capability path a server
// must advertise for this command to work, and whether the command
// depends on one at all.
func (c *command) Capability() (string, bool) {
	if c.Method == "" {
		return "", false
	}
	return client.CapabilityFor(c.Method)
}

// commands is the command surface of PLAN §4: the read-only commands
// of M1 and the mutations of M2. `raw` is the escape hatch from M0 and
// deliberately carries no method: it is the one command that may call
// anything, including methods no capability covers.
//
// The table is filled in init rather than declared as a literal
// because the commands and the capability partition below refer to
// each other: `help` prints the surface, and the surface is a list of
// commands.
var commands []*command

func init() {
	commands = []*command{
		{
			Name:    "definition",
			Args:    "<loc>",
			Summary: "print where the symbol at a location is defined",
			Method:  methodDefinition,
			Run:     locationCommand,
			MCP: oneTool("definition", "Where the symbol at a location is defined.",
				locationParams(pointDesc), queryParams()),
		},
		{
			Name:    "references",
			Args:    "<loc>",
			Summary: "print every reference to the symbol at a location",
			Method:  methodReferences,
			Run:     locationCommand,
			MCP: oneTool("references", "Every reference to the symbol at a location.",
				locationParams(pointDesc), queryParams(), []param{
					{Flag: "declaration", Aliases: []string{"d"}, Type: typeBool,
						Desc: "Include the symbol's own declaration among the references."},
				}),
		},
		{
			Name:    "implementation",
			Args:    "<loc>",
			Summary: "print the implementations of the symbol at a location",
			Method:  methodImplementation,
			Run:     locationCommand,
			MCP: oneTool("implementation", "Implementations of the interface or method at a location.",
				locationParams(pointDesc), queryParams()),
		},
		{
			Name:    "hover",
			Args:    "<loc>",
			Summary: "print the documentation and signature at a location",
			Method:  methodHover,
			Run:     hoverCommand,
			MCP: oneTool("hover", "Signature and documentation of the symbol at a location.",
				locationParams(pointDesc), queryParams()),
		},
		{
			Name:    "symbols",
			Args:    "<file>",
			Summary: "print the symbols declared in one file",
			Method:  methodDocumentSymbol,
			Run:     symbolsCommand,
			MCP: oneTool("symbols", "Symbols declared in one file, in document order, each with its id.",
				[]param{
					{Name: "file", Type: typeString, Path: true, Required: true, Desc: "File to list."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
						Desc: "Any directory inside the workspace the ids are relative to (default: workspace)."},
				}, queryParams()),
		},
		{
			Name:    "workspace_symbol",
			Args:    "<query>",
			Summary: "search the whole workspace for a symbol by name",
			Method:  methodWorkspaceSymbol,
			Run:     workspaceSymbolCommand,
			MCP: oneTool("workspace_symbol", "Search the workspace for symbols by name, in the server's relevance order, each with its id.",
				[]param{
					{Name: "query", Type: typeString, Required: true, Desc: "Name (or part of one) to search for."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
						Desc: "Directory whose workspace to search (default: workspace)."},
					{Flag: "language", Type: typeString,
						Desc: "Language id of the workspace, when no file in it identifies one."},
				}, queryParams()),
		},
		{
			Name:    "rename",
			Args:    "<loc> <newname>",
			Summary: "rename the symbol at a location across the workspace",
			Method:  methodRename,
			Run:     renameCommand,
			MCP: oneTool("rename", "Rename the symbol at a location across the workspace, all files or none.",
				locationParams(pointDesc),
				[]param{{Name: "new_name", Type: typeString, Required: true, Desc: "The new name."}},
				mutationParams(), queryParams()),
		},
		{
			Name:    "codeaction",
			Args:    "<loc|range>",
			Summary: "list the server's code actions at a location, or apply one",
			Method:  methodCodeAction,
			Run:     codeActionCommand,
			MCP: oneTool("codeaction", "List the server's code actions at a location or range; with index or title, preview or apply one.",
				locationParams("file:line:col, or file:line:col-line:col for a range (1-based line, byte column). Give this, symbol or id."),
				[]param{
					{Flag: "index", Type: typeInt, Min: intp(1), Desc: "Take the nth action of the list (1-based)."},
					{Flag: "title", Type: typeString, Desc: "Take the action with this title."},
					{Flag: "kind", Type: typeString,
						Desc: "Comma-separated CodeActionKinds to ask for, e.g. quickfix,source.organizeImports."},
				}, mutationParams(), queryParams()),
		},
		{
			Name:    "format",
			Args:    "<path...>",
			Summary: "format files with the server's own formatter",
			Method:  methodFormatting,
			Run:     formatCommand,
			MCP: oneTool("format", "Format files with the language server's formatter, all files or none.",
				[]param{
					{Name: "paths", Type: typeStrings, Path: true, Required: true, Desc: "Files to format; all in one workspace."},
					{Flag: "tab-size", Type: typeInt, Min: intp(1), Desc: "Width of a tab stop, for servers that ask (default 4)."},
					{Flag: "insert-spaces", Type: typeBool, Desc: "Indent with spaces instead of tabs, for servers that ask."},
				}, mutationParams(), queryParams()),
		},
		{
			Name:    "check",
			Args:    "[path...]",
			Summary: "collect diagnostics for a file or a tree; exit 1 if any are errors",
			// Deliberately unguarded. Diagnostics arrive as
			// textDocument/publishDiagnostics, a notification any
			// server may send without advertising anything; the pull
			// model is an optimisation this command uses when the
			// server does advertise it. Naming a method here would
			// make `help` call the command unavailable on servers
			// that answer it perfectly well.
			Run: checkCommand,
			MCP: oneTool("check", "Diagnostics for files or a tree. A file the server never reported on is not_ready, not clean.",
				[]param{
					{Name: "paths", Type: typeStrings, Path: true, WorkspaceDefault: true,
						Desc: "Files or directories to check (default: workspace)."},
					{Flag: "diagnostics", Type: typeString, Enum: []string{collectAuto, collectPull, collectPush},
						Desc: "How to collect: auto, pull (textDocument/diagnostic) or push (publishDiagnostics)."},
					{Flag: "allow-silent", Type: typeBool,
						Desc: "Report even when the server never published about some files; they are named in the warnings."},
					{Flag: "max-files", Type: typeInt, Min: intp(1), Desc: "Open at most this many files (default 200), reporting truncation."},
					{Flag: "language", Type: typeString, Desc: "Language id of the tree, when no file in it identifies one."},
				}, queryParams()),
		},
		{
			Name:    "call_hierarchy",
			Args:    "<loc>",
			Summary: "print who calls the symbol at a location, and whom it calls",
			Method:  methodPrepareCallHierarchy,
			Run:     callHierarchyCommand,
			MCP: oneTool("call_hierarchy", "Callers and callees of the symbol at a location.",
				locationParams(pointDesc), []param{
					{Flag: "direction", Type: typeString, Enum: []string{directionIncoming, directionOutgoing, directionBoth},
						Desc: "incoming (callers), outgoing (callees) or both (default)."},
					{Flag: "depth", Type: typeInt, Min: intp(1), Max: intp(maxCallDepth),
						Desc: "Levels to expand (default 1: direct calls only)."},
				}, queryParams()),
		},
		{
			Name:    "outline",
			Args:    "<file...>",
			Summary: "print the symbols of one or more files as a tree, with stable ids",
			Method:  methodDocumentSymbol,
			Run:     outlineCommand,
			MCP: oneTool("outline", "Symbols of one or more files as a tree: id, kind, name, signature, line range. Use it instead of reading the files.",
				[]param{
					{Name: "files", Type: typeStrings, Path: true, Required: true, Desc: "Files to outline."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
						Desc: "Any directory inside the workspace the ids are relative to (default: workspace)."},
				}, commonParams("limit", "timeout", "server")),
		},
		{
			Name:    "source",
			Args:    "<id|loc...>",
			Summary: "print the source of one or more symbols, by id or location",
			Method:  methodDocumentSymbol,
			Run:     sourceCommand,
			MCP: oneTool("source", "Source text of one or more symbols by id (or location), with its doc comment and a content hash. Use it instead of reading the file.",
				[]param{
					{Name: "ids", Type: typeStrings, Path: true, Location: true, IDOrLocation: true,
						Desc: "Symbol ids from outline, symbols or workspace_symbol; a location file:line[:col] names the symbol containing it. Give this or id."},
					{Flag: "id", Type: typeString,
						Desc: "One symbol id (path::Container.Name#kind), as an alternative to ids."},
				},
				sourceParams(), commonParams("context", "timeout", "server", "absolute")),
		},
		{
			Name:    "context",
			Args:    "<id|loc>",
			Summary: "print a symbol's source with its file's import block and hover text",
			Method:  methodDocumentSymbol,
			Run:     contextCommand,
			MCP: oneTool("context", "A symbol's source plus its file's package/import block and the server's hover text, in one call.",
				[]param{
					{Name: "location", Type: typeString, Path: true, Location: true, IDOrLocation: true,
						Desc: "Symbol id from outline, symbols or workspace_symbol, or a location file:line[:col]. Give this, id or symbol."},
					{Flag: "id", Type: typeString,
						Desc: "Symbol id (path::Container.Name#kind). Alternative to location and symbol; a stale id is an error naming the nearest candidates."},
					{Flag: "symbol", Type: typeString,
						Desc: "Dotted symbol path such as pkg.Type.Method, resolved with workspace/symbol. Alternative to location and id; an ambiguous name is an error listing every candidate."},
				},
				sourceParams(),
				[]param{{Flag: "max-header-lines", Type: typeInt, Min: intp(0),
					Desc: "Cap the import block at this many lines (default 80, 0 for no cap)."}},
				commonParams("context", "timeout", "server")),
		},
		{
			Name:    "tree",
			Args:    "[dir]",
			Summary: "list the files of the workspace, gitignore-aware, with language and server",
			Run:     treeCommand,
			MCP: oneTool("tree", "Files of the workspace (or a directory of it), gitignore-aware, each with its language and the server that handles it.",
				[]param{
					{Name: "dir", Type: typeString, Path: true, Desc: "Directory to list, inside the workspace (default: the workspace, or path)."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true, Desc: treePathDesc},
					{Flag: "prefix", Type: typeString, Desc: "Only files whose workspace-relative path starts with this."},
					{Flag: "max-files", Type: typeInt, Min: intp(1), Desc: "List at most this many files (default 500); truncation is reported. limit is the same bound and wins when both are given."},
				}, commonParams("limit", "timeout")),
		},
		{
			Name:    "repo_outline",
			Args:    "[dir]",
			Summary: "summarize the workspace: directories with file counts, languages, servers and whether they are installed",
			Run:     repoOutlineCommand,
			MCP: oneTool("repo_outline", "Directories with file counts, language breakdown, and the servers that handle them with their install state.",
				[]param{
					{Name: "dir", Type: typeString, Path: true, Desc: "Directory to summarize, inside the workspace (default: the workspace, or path)."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true, Desc: treePathDesc},
					{Flag: "depth", Type: typeInt, Min: intp(1), Desc: "Directory levels to list (default 2)."},
					{Flag: "max-dirs", Type: typeInt, Min: intp(1), Desc: "List at most this many directories (default 200); truncation is reported. limit is the same bound and wins when both are given."},
				}, commonParams("limit", "timeout")),
		},
		{
			Name:    "file",
			Args:    "<file>",
			Summary: "print a line range of a file inside the workspace",
			Run:     fileCommand,
			MCP: oneTool("file", "A line range of a file inside the workspace. Use outline and source for code; this is for files with no symbols.",
				[]param{
					{Name: "file", Type: typeString, Path: true, Required: true, Desc: "File to read; it must be inside the workspace."},
					{Flag: "start", Type: typeInt, Min: intp(1), Desc: "First line, 1-based (default 1)."},
					{Flag: "end", Type: typeInt, Min: intp(1), Desc: "Last line, inclusive (default the end of the file)."},
				}, sourceParams(), commonParams()),
		},
		{
			Name:    "search_text",
			Args:    "<query>",
			Summary: "search the files of the workspace for text, without a language server",
			Run:     searchTextCommand,
			MCP: oneTool("search_text", "Full-text search of the workspace's files (gitignore-aware, live, no index): file:line:col and the matched line.",
				[]param{
					{Name: "query", Type: typeString, Required: true, Desc: "Text to find (a regular expression with regex)."},
					{Flag: "regex", Type: typeBool, Desc: "Treat the query as a Go RE2 regular expression."},
					{Flag: "case-sensitive", Type: typeBool, Desc: "Match case (default: case-insensitive)."},
					{Flag: "word", Type: typeBool, Desc: "Whole words only (letters, digits, _ of any script)."},
					{Flag: "glob", Type: typeStrings, Desc: "Only files matching these globs, e.g. **/*.go; a leading ! excludes. No / in a glob matches at any depth."},
					{Flag: "path", Type: typeString, Path: true, Desc: "Search only this directory or file, inside the workspace (default: all of it)."},
					{Flag: "root", Type: typeString, Path: true, WorkspaceDefault: true, Desc: treePathDesc},
					{Flag: "max-file-bytes", Type: typeInt, Min: intp(1), Desc: "Skip files larger than this, reported in warnings (default 1048576)."},
					{Flag: "with-symbol", Type: typeBool, Desc: "Add the id of the symbol containing each match, when a language server can say."},
					{Flag: "limit", Type: typeInt, Min: intp(0), Desc: "Maximum matching lines (default 50, 0 for no limit). Truncation is reported with the total."},
				}, commonParams("context", "timeout", "server")),
		},
		{
			Name:    "index",
			Args:    indexSubcommands + " [--root DIR]",
			Summary: "inspect, warm up or clear the workspace's persistent symbol and import index",
			MCP: []toolSpec{
				{Name: "index_status", Prefix: []string{"status"},
					Description: "Symbol and import index state: counts, coverage per server and language, files skipped and why, staleness, cache path and size. Builds nothing.",
					Params:      withParams(indexRootParams(), managementParams())},
				{Name: "index_build", Prefix: []string{"build"},
					Description: "Warm up the whole index now. Queries build what they need lazily; this revalidates every file and asks the servers about the changed ones.",
					Params:      withParams(indexRootParams(), managementParams())},
				{Name: "index_clear", Prefix: []string{"clear"},
					Description: "Forget the index, in memory and on disk (a cache only). The next query rebuilds what it needs.",
					Params:      withParams(indexRootParams(), managementParams())},
			},
			Run: indexCommand,
		},
		{
			Name:    "search_symbols",
			Args:    "<query>",
			Summary: "ranked symbol search across the whole workspace, from the index",
			Run:     searchSymbolsCommand,
			MCP: oneTool("search_symbols", "Ranked symbol search across the workspace (exact, prefix, then BM25 over name, signature, doc). Never stale. detail=compact is ids only; full inlines source.",
				[]param{
					{Name: "query", Type: typeString, Required: true, Desc: "Name or words to find; camelCase and snake_case are split."},
					{Flag: "kind", Type: typeStrings, Desc: "Only these symbol kinds, e.g. function, method, struct, class, interface."},
					{Flag: "lang", Type: typeStrings, Desc: "Only these language ids, e.g. go, python, typescript."},
					{Flag: "glob", Type: typeStrings, Desc: "Only files matching these globs; a leading ! excludes. No / in a glob matches at any depth."},
					{Flag: "path", Type: typeString, Path: true, Desc: "Only files under this directory, or this file, inside the workspace."},
					{Flag: "detail", Type: typeString, Enum: []string{detailCompact, detailStandard, detailFull}, Desc: "compact: id, kind, file, line. standard (default): adds signature, doc, container. full: adds the source."},
					{Flag: "fuzzy", Type: typeBool, Desc: "When nothing matches, return the nearest names by trigram and edit distance, flagged fuzzy."},
					{Flag: "max-lines", Type: typeInt, Min: intp(1), Desc: "With detail full: cap each symbol's source at this many lines."},
					{Flag: "max-bytes", Type: typeInt, Min: intp(1), Desc: "With detail full: cap each symbol's source at this many bytes."},
					{Flag: "limit", Type: typeInt, Min: intp(0), Desc: "Maximum results (default 20, 0 for no limit). Truncation is reported with the total."},
					indexRootParam(),
				}, commonParams("context", "timeout", "report")),
		},
		{
			Name:    "repo_map",
			Args:    "",
			Summary: "token-budgeted overview of the repository: files ranked by import centrality with their top symbols",
			Run:     repoMapCommand,
			MCP: oneTool("repo_map", "Repository overview within a token budget: files ranked by import centrality (PageRank) with their top symbols. Truncation is reported.",
				[]param{
					{Flag: "budget", Type: typeInt, Min: intp(1), Desc: "Token budget (default 2000; about 4 bytes per token)."},
					{Flag: "per-file", Type: typeInt, Min: intp(1), Desc: "Symbols for each of the top 20 files (default 8; the rest get at most 3)."},
					{Flag: "glob", Type: typeStrings, Desc: "Only files matching these globs; a leading ! excludes."},
					{Flag: "lang", Type: typeStrings, Desc: "Only these language ids."},
					{Flag: "path", Type: typeString, Path: true, Desc: "Only files under this directory, or this file."},
					indexRootParam(),
				}, commonParams("limit", "timeout", "report")),
		},
		{
			Name:    "find_importers",
			Args:    "<target>",
			Summary: "print the files that import a file, a directory (Go package) or a Go import path",
			Run:     findImportersCommand,
			MCP: oneTool("find_importers", "Files that import a file, a package directory or a Go import path, with the line. A language with no import extractor is not_covered, not empty.",
				[]param{
					{Name: "target", Type: typeString, Required: true, Desc: "A file or directory (relative to the workspace), or a Go import path."},
					{Flag: "include-tests", Type: typeBool, Desc: "Also list the Go test files whose package imports the target only from tests (they are counted and named either way)."},
					indexRootParam(),
				}, commonParams("limit", "timeout", "report")),
		},
		{
			Name:    "imports",
			Args:    "<file>",
			Summary: "print what a file imports, resolved to workspace files or marked external",
			Run:     importsCommand,
			MCP: oneTool("imports", "What a file imports: each import with its line and what it resolves to in the workspace, or external with a category. No extractor: not_covered.",
				[]param{
					{Name: "file", Type: typeString, Path: true, Required: true, Desc: "File to read the imports of."},
					indexRootParam(),
				}, commonParams("limit", "timeout", "report")),
		},
		{
			Name:    "dependency_graph",
			Args:    "[path]",
			Summary: "print the import graph from a file, directory or package, or the whole workspace",
			Run:     dependencyGraphCommand,
			MCP: oneTool("dependency_graph", "Import graph edges from a file, directory or Go package, following imports out, in or both to a depth; no start means the whole workspace.",
				[]param{
					{Name: "path", Type: typeString, Desc: "File, directory or Go import path to start from (default: the whole workspace)."},
					{Flag: "depth", Type: typeInt, Min: intp(1), Desc: "Edges to follow (default 2)."},
					{Flag: "direction", Type: typeString, Enum: []string{"out", "in", "both"}, Desc: "out (what it imports, default), in (what imports it) or both."},
					{Flag: "external", Type: typeBool, Desc: "Include standard-library and third-party imports as leaf edges."},
					indexRootParam(),
				}, commonParams("limit", "timeout", "report")),
		},
		{
			Name:    "dependency_cycles",
			Args:    "",
			Summary: "print the import cycles of the workspace; exit 1 if there are any",
			Run:     dependencyCyclesCommand,
			MCP: oneTool("dependency_cycles", "Import cycles of the workspace (strongly connected components), each starting at its smallest node.",
				[]param{indexRootParam()}, commonParams("limit", "timeout", "report")),
		},
		{
			Name:    "batch",
			Args:    "[--file <path>]",
			Summary: "run one query per input line and print one envelope per line",
			Run:     batchCommand,
			NoMCP:   "an MCP client already issues calls concurrently; batch exists to save process startups",
		},
		{
			Name:    "raw",
			Args:    "<method> [--params <json>] [--path <file|dir>]",
			Summary: "send one JSON-RPC request and print the raw result",
			Run:     rawCommand,
			NoMCP:   "the escape hatch: it skips the capability guard and the readiness gate that make an answer trustworthy",
		},
		{
			Name:    "daemon",
			Args:    daemonSubcommands + " [--path DIR]",
			Summary: "inspect or stop the workspace's shared language-server daemon",
			Run:     daemonCommand,
			// Only `status` is a tool. `stop` would let an agent throw away
			// the warm servers every later call depends on, and `logs`
			// and `serve` are for a human debugging and for the auto-spawn.
			MCP: []toolSpec{{
				Name:        "daemon_status",
				Prefix:      []string{"status"},
				Description: "State of the workspace's warm language-server daemon and each server it holds.",
				Params: withParams([]param{
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
						Desc: "Any file or directory inside the workspace (default: workspace)."},
				}, managementParams()),
			}},
		},
		{
			Name:    "servers",
			Args:    "[--path DIR]",
			Summary: "list the configured servers, where each setting came from, and whether each is installed",
			Run:     serversCommand,
			MCP: oneTool("servers", "Configured language servers, which layer set each key, and whether each is installed.",
				[]param{{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
					Desc: "Any file or directory inside the workspace whose .lightspeed.toml applies (default: workspace)."}},
				managementParams()),
		},
		{
			Name:    "install",
			Args:    "<name> [--run] [--version V]",
			Summary: "print the mise command that installs a server; with --run, run it",
			Run:     installCommand,
			NoMCP:   "it can download and run an installer; nothing does that as a side effect of an agent's call (PLAN §6), so a human runs it",
		},
		{
			Name:    "doctor",
			Args:    "[path...]",
			Summary: "diagnose the server setup: config layers, executables, mise, routing",
			Run:     doctorCommand,
			MCP: oneTool("doctor", "Diagnose the server setup for paths: config layers, executables, mise, routing.",
				[]param{{Name: "paths", Type: typeStrings, Path: true, WorkspaceDefault: true,
					Desc: "Files or directories to diagnose (default: workspace)."}},
				managementParams()),
		},
		{
			Name:    "mcp",
			Args:    "",
			Summary: "serve the query commands as MCP tools over stdio",
			Run:     mcpCommand,
			NoMCP:   "it is the server",
		},
		{
			Name:    "help",
			Args:    "[<file>|<dir>]",
			Summary: "list the subcommands, or the ones a server can answer",
			Run:     helpCommand,
			NoMCP:   "tools/list is the surface; a server that lacks a capability answers with unsupported_method naming what it can do",
		},
	}
	// The composed analysis commands sit before `batch`, so that the table
	// keeps its queries first and its plumbing (batch, raw, daemon, servers,
	// install, doctor, mcp, help) last.
	at := slices.IndexFunc(commands, func(c *command) bool { return c.Name == "batch" })
	commands = slices.Insert(commands, at, composedCommands()...)
}

// lookupCommand finds a subcommand by name.
func lookupCommand(name string) *command {
	for _, c := range commands {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// commandNames lists the subcommands, sorted, for error messages.
func commandNames() []string {
	out := make([]string, 0, len(commands))
	for _, c := range commands {
		out = append(out, c.Name)
	}
	slices.Sort(out)
	return out
}

// partitionByCapabilities splits the command surface into the commands
// this server can answer and the ones it cannot. It is the runtime
// half of the capability-derived surface: `help <file>` prints it, and
// an unsupported method quotes it back at the user, so neither can
// claim a command that would fail.
func partitionByCapabilities(caps *client.Capabilities) (available, unavailable []*command) {
	for _, c := range commands {
		if _, guarded := c.Capability(); !guarded || caps.Supports(c.Method) {
			available = append(available, c)
			continue
		}
		unavailable = append(unavailable, c)
	}
	byName := func(a, b *command) int { return cmp.Compare(a.Name, b.Name) }
	slices.SortFunc(available, byName)
	slices.SortFunc(unavailable, byName)
	return available, unavailable
}

// unsupportedMethodError turns internal/client's capability refusal
// into a coded error whose message names the commands that would have
// worked. The exit code is 3 either way; the difference is that the
// caller is told what to do instead.
func unsupportedMethodError(err *client.UnsupportedMethodError, caps *client.Capabilities) error {
	available, _ := partitionByCapabilities(caps)
	names := make([]string, 0, len(available))
	for _, c := range available {
		if c.Method != "" {
			names = append(names, c.Name)
		}
	}
	msg := err.Error()
	if len(names) > 0 {
		msg += "; this server can answer: " + strings.Join(names, ", ")
	} else {
		msg += "; this server can answer no lightspeed command"
	}
	return render.Errorf(render.CodeUnsupportedMethod, "%s", msg).
		WithDetails(map[string]any{
			"method":     err.Method,
			"capability": err.Capability,
			"server":     err.ServerName,
			"available":  names,
		})
}

// writeUsage prints the top-level usage to stderr. The capability
// column is the *static* half of the surface — which capability each
// command needs — because without a file there is no server to ask;
// `help <file>` resolves one and prints what it actually advertises.
func writeUsage(w io.Writer) {
	fmt.Fprint(w, `lightspeed — gopls's command-line interface, generalized to every language server

usage:
  lightspeed <command> [flags] [arguments]

commands:
`)
	writeCommandTable(w, commands, true)
	fmt.Fprint(w, `
Locations use gopls's span syntax, with 1-based lines and *byte* columns:
  file.go            file.go:12         file.go:12:5
  file.go:12:5-12:9  file.go:#1234

Or name the symbol instead of computing a column:
  --symbol 'pkg.Type.Method'   [--path DIR to say which workspace to search]
  --id 'internal/cli/cli.go::Main#function'   a symbol id from outline, symbols
                               or workspace_symbol (a stale one lists candidates)

common flags:
  --format json|text|diff   output format (json unless stdout is a terminal;
                            diff when previewing edits, sarif for diagnostics)
  --context N               N lines of source around each match
  --limit N                 at most N results, with truncated:true when it bites
  --indent                  pretty-print JSON
  --timeout D               how long to wait for the workspace to become ready
  --settle D                how long a result must be unchanged before it is believed
  --server NAME             pick a server when several claim the file
  --no-daemon               run the server in this process instead of the workspace's
                            shared daemon (env: LIGHTSPEED_NO_DAEMON=1)
  --offline                 refuse anything that could download, i.e. install --run
                            (env: LIGHTSPEED_OFFLINE=1)

flags of the commands that write:
  --apply                   write the edits (default: preview them only)
  --allow-dirty             --apply even though the git worktree is dirty

exit codes:
  0 ok · 1 problems found (including an authoritative empty answer) ·
  2 usage · 3 no server · 4 crash or timeout · 5 not ready / still indexing

Run "lightspeed help <file>" to see what the server for that file can answer.
`)
}

// writeCommandTable prints one aligned line per command.
func writeCommandTable(w io.Writer, cmds []*command, withCapability bool) {
	width := 0
	for _, c := range cmds {
		if n := len(c.Name) + 1 + len(c.Args); n > width {
			width = n
		}
	}
	for _, c := range cmds {
		invocation := c.Name
		if c.Args != "" {
			invocation += " " + c.Args
		}
		fmt.Fprintf(w, "  %-*s  %s", width, invocation, c.Summary)
		if capability, guarded := c.Capability(); withCapability && guarded {
			fmt.Fprintf(w, " [needs %s]", capability)
		}
		fmt.Fprintln(w)
	}
}
