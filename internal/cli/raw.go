package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// rawCommand implements `lightspeed raw <method> [--params <json>]`:
// find or start the server for --path, send one request without the
// readiness gate or the capability check, and print the result in the
// JSON envelope.
//
// It is the escape hatch, so it may call anything — including methods
// no capability covers and methods the server never advertised — and it
// is not gated: whatever the server says is what is printed, empty or
// not. Since M3 it routes like every other command, by --path (default
// the working directory) instead of the M0 hardcoded `gopls serve`.
func rawCommand(e *env, c *command, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(e.stderr, "usage: lightspeed raw <method> [--params <json>] [--path <file|dir>] [--timeout <duration>]")
		return usage(e.stdout, "raw: missing <method> argument")
	}
	method := args[0]

	fs := flag.NewFlagSet("raw", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	params := fs.String("params", "", "JSON parameters for the request")
	timeout := fs.Duration("timeout", 30*time.Second, "overall deadline for the request")
	path := fs.String("path", ".", "file or directory that selects the server and the workspace")
	language := fs.String("language", "", "language id of the target, when nothing in it identifies one")
	serverName := fs.String("server", "", "name of the server to use when several claim the target")
	noDaemon := fs.Bool("no-daemon", false, "run the language server inside this process instead of the workspace's shared daemon")
	offline := fs.Bool("offline", false, offlineUsage)
	if err := fs.Parse(args[1:]); err != nil {
		return usage(e.stdout, fmt.Sprintf("raw: %v", err))
	}
	if fs.NArg() > 0 {
		return usage(e.stdout, fmt.Sprintf("raw: unexpected arguments %q", fs.Args()))
	}
	if *noDaemon {
		e.noDaemon = true
	}
	if *offline {
		e.offline = true
	}

	var paramsRaw json.RawMessage
	if *params != "" {
		if !json.Valid([]byte(*params)) {
			return usage(e.stdout, "raw: --params is not valid JSON")
		}
		paramsRaw = json.RawMessage(*params)
	}

	match, err := e.resolveHelpTarget(*path, *language, *serverName)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), *timeout)
	defer cancel()
	s, err := startSession(ctx, e, match, client.GateOptions{Timeout: *timeout})
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	var result json.RawMessage
	if method == "initialize" {
		// Already sent during the handshake; a second initialize is a
		// protocol violation, so return the handshake's result.
		result = s.caps.Raw()
	} else {
		result, err = s.rawCall(ctx, method, paramsRaw)
		if err != nil {
			return failRPC(e, method, err)
		}
	}

	if err := render.OK(e.stdout, result); err != nil {
		fmt.Fprintf(e.stderr, "lightspeed: writing output: %v\n", err)
		return ExitCrash
	}
	return ExitOK
}

// failRPC maps a failed request to the envelope + exit-code taxonomy
// of PLAN §4. The wording is raw's own — it is not gated and not
// capability-checked, so the vocabulary of the other commands' errors
// would promise more than it delivers.
func failRPC(e *env, method string, err error) int {
	var remote *daemon.Error
	if errors.As(err, &remote) {
		switch {
		case remote.RPC != nil:
			// The server answered; that's a result, not a crash.
			_ = render.Fail(e.stdout, "server_error",
				fmt.Sprintf("%s: server returned error %d: %s", method, remote.RPC.Code, remote.RPC.Message))
			return ExitProblems
		case remote.Code == daemon.CodeTimeout:
			_ = render.Fail(e.stdout, "timeout", fmt.Sprintf("%s: timed out waiting for server", method))
			return ExitCrash
		}
		return e.fail(remoteFailure{remote})
	}
	if errors.Is(err, context.DeadlineExceeded) {
		_ = render.Fail(e.stdout, "timeout", fmt.Sprintf("%s: timed out waiting for server", method))
		return ExitCrash
	}
	_ = render.Fail(e.stdout, "server_crash", fmt.Sprintf("%s: %v", method, err))
	return ExitCrash
}
