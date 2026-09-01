package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// An Instance is a language server that has been started but not yet
// spoken to: the connection to drive it, and the reaping half of its
// lifecycle.
type Instance struct {
	// Conn is the JSON-RPC connection to the server. The pool
	// performs the LSP handshake on it with client.Connect.
	Conn *client.Conn

	// Wait reaps whatever Launch started, and is called after the
	// LSP `shutdown`/`exit` exchange. A launcher that started no
	// process may leave it nil.
	Wait func(ctx context.Context) error
}

// A Launcher starts the language server described by def, to serve the
// workspace rooted at root. It is the one seam the daemon needs for
// hermetic tests: the default implementation spawns a subprocess, and
// a test substitutes an in-process scripted server (internal/fakeserver)
// over a pipe, with whatever startup delay it wants to imitate.
type Launcher func(ctx context.Context, def *serverdef.ServerDef, root string) (*Instance, error)

// ExecLauncher runs def.Server.Command as a subprocess speaking LSP
// over stdio, forwarding the server's stderr to the given writer —
// which in a daemon is its log, and must never be the stdout carrying
// the JSON envelope.
func ExecLauncher(stderr io.Writer) Launcher {
	return func(ctx context.Context, def *serverdef.ServerDef, root string) (*Instance, error) {
		srv, err := client.StartCommand(def.Server.Command, stderr)
		if err != nil {
			return nil, err
		}
		return &Instance{Conn: srv.Conn, Wait: srv.Wait}, nil
	}
}

// launcherFor picks the launcher to use, defaulting to [ExecLauncher].
func launcherFor(l Launcher, stderr io.Writer) Launcher {
	if l != nil {
		return l
	}
	return ExecLauncher(stderr)
}

// classifyLaunchError turns a failure to start a server into an error
// that keeps its exit code across the socket: a server that is not
// installed is exit 3 with a code the CLI can turn into "run this mise
// command", anything else is a crash.
func classifyLaunchError(def *serverdef.ServerDef, err error) error {
	if isNotFound(err) {
		return &Error{
			Code:    CodeServerNotInstalled,
			Message: fmt.Sprintf("server %q: command %q not found on PATH", def.Name, def.Server.Command[0]),
			Exit:    exitNoServer,
		}
	}
	return &Error{
		Code:    CodeSpawnFailed,
		Message: fmt.Sprintf("server %q: %v", def.Name, err),
		Exit:    exitCrash,
	}
}

func isNotFound(err error) bool {
	var execErr *exec.Error
	if errors.As(err, &execErr) {
		return errors.Is(execErr.Err, exec.ErrNotFound)
	}
	return errors.Is(err, exec.ErrNotFound)
}
