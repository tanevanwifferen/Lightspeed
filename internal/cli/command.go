package cli

import (
	"cmp"
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
		},
		{
			Name:    "references",
			Args:    "<loc>",
			Summary: "print every reference to the symbol at a location",
			Method:  methodReferences,
			Run:     locationCommand,
		},
		{
			Name:    "implementation",
			Args:    "<loc>",
			Summary: "print the implementations of the symbol at a location",
			Method:  methodImplementation,
			Run:     locationCommand,
		},
		{
			Name:    "hover",
			Args:    "<loc>",
			Summary: "print the documentation and signature at a location",
			Method:  methodHover,
			Run:     hoverCommand,
		},
		{
			Name:    "symbols",
			Args:    "<file>",
			Summary: "print the symbols declared in one file",
			Method:  methodDocumentSymbol,
			Run:     symbolsCommand,
		},
		{
			Name:    "workspace_symbol",
			Args:    "<query>",
			Summary: "search the whole workspace for a symbol by name",
			Method:  methodWorkspaceSymbol,
			Run:     workspaceSymbolCommand,
		},
		{
			Name:    "rename",
			Args:    "<loc> <newname>",
			Summary: "rename the symbol at a location across the workspace",
			Method:  methodRename,
			Run:     renameCommand,
		},
		{
			Name:    "codeaction",
			Args:    "<loc|range>",
			Summary: "list the server's code actions at a location, or apply one",
			Method:  methodCodeAction,
			Run:     codeActionCommand,
		},
		{
			Name:    "format",
			Args:    "<path...>",
			Summary: "format files with the server's own formatter",
			Method:  methodFormatting,
			Run:     formatCommand,
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
		},
		{
			Name:    "call_hierarchy",
			Args:    "<loc>",
			Summary: "print who calls the symbol at a location, and whom it calls",
			Method:  methodPrepareCallHierarchy,
			Run:     callHierarchyCommand,
		},
		{
			Name:    "batch",
			Args:    "[--file <path>]",
			Summary: "run one query per input line and print one envelope per line",
			Run:     batchCommand,
		},
		{
			Name:    "raw",
			Args:    "<method> [--params <json>]",
			Summary: "send one JSON-RPC request and print the raw result",
			Run:     func(e *env, _ *command, args []string) int { return rawCommand(args, e.stdout, e.stderr) },
		},
		{
			Name:    "help",
			Args:    "[<file>|<dir>]",
			Summary: "list the subcommands, or the ones a server can answer",
			Run:     helpCommand,
		},
	}
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

common flags:
  --format json|text|diff   output format (json unless stdout is a terminal;
                            diff when previewing edits, sarif for diagnostics)
  --context N               N lines of source around each match
  --limit N                 at most N results, with truncated:true when it bites
  --indent                  pretty-print JSON
  --timeout D               how long to wait for the workspace to become ready
  --settle D                how long a result must be unchanged before it is believed
  --server NAME             pick a server when several claim the file

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
