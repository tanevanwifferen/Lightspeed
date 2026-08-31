// Package cli wires lightspeed's command surface. M0 ships only the
// `raw` escape hatch; the full surface of PLAN §4 is derived from
// server capabilities in later milestones.
package cli

import (
	"fmt"
	"io"

	"github.com/owner/lightspeed/internal/render"
)

// Exit codes, PLAN §4.
const (
	ExitOK       = 0 // success
	ExitProblems = 1 // problems found / server-reported error
	ExitUsage    = 2 // bad invocation
	ExitNoServer = 3 // no server available
	ExitCrash    = 4 // server crash or timeout
	ExitNotReady = 5 // indexing timeout (unused until M1 readiness gating)
)

const usageText = `lightspeed — LSP command line (M0 spike)

usage:
  lightspeed raw <method> [--params <json>] [--timeout <duration>]

The raw subcommand sends one JSON-RPC request to the language server
(after the initialize handshake) and prints the result in the JSON
envelope. The server command is hardcoded to "gopls serve" for M0;
set LIGHTSPEED_SERVER_CMD to override it.
`

// Main runs the CLI and returns the process exit code. Machine
// output (the JSON envelope) goes to stdout, human diagnostics to
// stderr.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		return usage(stdout, "missing subcommand")
	}
	switch args[0] {
	case "raw":
		return rawCommand(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stderr, usageText)
		return ExitOK
	default:
		fmt.Fprint(stderr, usageText)
		return usage(stdout, fmt.Sprintf("unknown subcommand %q", args[0]))
	}
}

// usage emits a usage-error envelope and returns the usage exit code.
func usage(stdout io.Writer, msg string) int {
	_ = render.Fail(stdout, "usage", msg)
	return ExitUsage
}
