package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// defaultServerCommand is the hardcoded M0 server (PLAN §8 M0: "raw
// works end to end against a hardcoded server"). Replaced by the
// router + serverdef resolution in M4.
var defaultServerCommand = []string{"gopls", "serve"}

// serverCommandEnv overrides the hardcoded server command; it exists
// so hermetic tests can point the CLI at the fake server. M0
// scaffolding, see docs/DECISIONS.md D4.
const serverCommandEnv = "LIGHTSPEED_SERVER_CMD"

// rawCommand implements `lightspeed raw <method> [--params <json>]`:
// spawn the server, initialize, send one request, print the result in
// the JSON envelope, shut down.
func rawCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		fmt.Fprintln(stderr, "usage: lightspeed raw <method> [--params <json>] [--timeout <duration>]")
		return usage(stdout, "raw: missing <method> argument")
	}
	method := args[0]

	fs := flag.NewFlagSet("raw", flag.ContinueOnError)
	fs.SetOutput(stderr)
	params := fs.String("params", "", "JSON parameters for the request")
	timeout := fs.Duration("timeout", 30*time.Second, "overall deadline for the request")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(stdout, fmt.Sprintf("raw: %v", err))
	}
	if fs.NArg() > 0 {
		return usage(stdout, fmt.Sprintf("raw: unexpected arguments %q", fs.Args()))
	}

	var paramsRaw json.RawMessage
	if *params != "" {
		if !json.Valid([]byte(*params)) {
			return usage(stdout, "raw: --params is not valid JSON")
		}
		paramsRaw = json.RawMessage(*params)
	}

	argv := defaultServerCommand
	if env := os.Getenv(serverCommandEnv); env != "" {
		argv = strings.Fields(env)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	srv, err := client.StartCommand(argv, stderr)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			_ = render.Fail(stdout, "no_server",
				fmt.Sprintf("server command %q not found on PATH", argv[0]))
			return ExitNoServer
		}
		_ = render.Fail(stdout, "spawn_failed", err.Error())
		return ExitCrash
	}
	// Always reap the subprocess, even on the error paths.
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	cwd, err := os.Getwd()
	if err != nil {
		cwd = ""
	}

	initResult, err := srv.Initialize(ctx, cwd)
	if err != nil {
		return failRPC(stdout, "initialize", err)
	}

	var result json.RawMessage
	if method == "initialize" {
		// Already sent during the handshake; a second initialize is a
		// protocol violation, so return the handshake's result.
		result = initResult
	} else {
		result, err = srv.Call(ctx, method, paramsRaw)
		if err != nil {
			return failRPC(stdout, method, err)
		}
	}

	if err := render.OK(stdout, result); err != nil {
		fmt.Fprintf(stderr, "lightspeed: writing output: %v\n", err)
		return ExitCrash
	}
	return ExitOK
}

// failRPC maps a failed request to the envelope + exit-code taxonomy
// of PLAN §4.
func failRPC(stdout io.Writer, method string, err error) int {
	var rpcErr *client.RPCError
	switch {
	case errors.As(err, &rpcErr):
		// The server answered; that's a result, not a crash.
		_ = render.Fail(stdout, "server_error",
			fmt.Sprintf("%s: server returned error %d: %s", method, rpcErr.Code, rpcErr.Message))
		return ExitProblems
	case errors.Is(err, context.DeadlineExceeded):
		_ = render.Fail(stdout, "timeout", fmt.Sprintf("%s: timed out waiting for server", method))
		return ExitCrash
	default:
		_ = render.Fail(stdout, "server_crash", fmt.Sprintf("%s: %v", method, err))
		return ExitCrash
	}
}
