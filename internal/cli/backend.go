package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The environment that steers how a command reaches its server.
const (
	// noDaemonEnv is the environment spelling of --no-daemon (PLAN §3):
	// any value strconv.ParseBool reads as true.
	noDaemonEnv = "LIGHTSPEED_NO_DAEMON"

	// daemonTimeoutEnv sets how long an auto-spawned daemon lingers
	// with no client connected, as a Go duration. It exists for tests
	// and for anyone who wants a daemon that leaves sooner; the
	// default is daemon.DefaultListenTimeout.
	daemonTimeoutEnv = "LIGHTSPEED_DAEMON_TIMEOUT"

	// daemonExeCheckEnv sets how often a daemon looks for its own
	// executable having been replaced, as a Go duration; test-only in
	// spirit, like daemonTimeoutEnv. The default is
	// daemon.DefaultExecutableCheck.
	daemonExeCheckEnv = "LIGHTSPEED_DAEMON_EXECHECK"
)

// maxSocketPath is the longest socket path accepted: sockaddr_un's
// sun_path is 108 bytes on Linux and 104 on the BSDs and macOS, less its
// terminator.
const maxSocketPath = 100

// noDaemonFlag is the global flag that keeps a command in process.
const noDaemonFlag = "--no-daemon"

// offlineFlag is the global flag over PLAN §6's kill switch.
const offlineFlag = "--offline"

// envNoDaemon reports whether LIGHTSPEED_NO_DAEMON asks for the
// in-process mode. An unparsable value is not "yes": silently running
// without the daemon is the surprising direction to guess in.
func envNoDaemon() bool {
	v, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(noDaemonEnv)))
	return err == nil && v
}

// listenTimeout is the idle-exit deadline for a daemon this process
// starts. Zero leaves the daemon package's default in charge.
func listenTimeout() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(daemonTimeoutEnv)))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// exeCheck is how often a daemon this process starts asks whether its own
// executable is still the one on disk. Zero leaves the default in charge.
func exeCheck() time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(os.Getenv(daemonExeCheckEnv)))
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// openHandle returns the handle a command reaches its server through:
// a client of the workspace's auto-spawned daemon, or with --no-daemon a
// pool inside this process that is shut down when the handle is closed.
//
// path is the file or directory the command is about. It decides which
// workspace — and so which daemon — is addressed, and because the
// workspace root is resolved by walking up from it, every subdirectory
// of a repository reaches the same one (PLAN §3).
func openHandle(ctx context.Context, e *env, path string) (daemon.Handle, error) {
	opts, err := daemonOptions(e, path)
	if err != nil {
		return nil, err
	}
	// The workspace's server definitions decide what the pool routes
	// with. In process that is the pool this command owns; with the
	// daemon it is the identity the daemon must have been started with,
	// or it is not the daemon to serve this command (D17).
	cfg, err := e.configForRoot(opts.Workspace)
	if err != nil {
		return nil, err
	}
	if e.noDaemon {
		pool := cfg.poolOptions(e.stderr)
		// One command's worth of servers, shut down by Close: a
		// reaper would only be a goroutine to stop.
		pool.SessionIdleTimeout = -1
		opts.Pool = pool
	} else {
		opts.ConfigID = cfg.id
		opts.OnStale = func(why string) { fmt.Fprintf(e.stderr, "lightspeed: %s\n", why) }
		opts.OnReplaced = e.addNote
	}
	h, err := daemon.Open(ctx, opts)
	if err != nil {
		return nil, daemonOpenError(err)
	}
	return h, nil
}

// daemonOptions is the one place that decides how the daemon package is
// configured, for a command and for `daemon status|stop|logs` alike, so
// that they cannot disagree about which socket a workspace lives at. It
// reads no server configuration, so that `daemon stop` still works in a
// workspace whose .lightspeed.toml is broken: openHandle adds the pool
// or the config id for a command that needs one.
func daemonOptions(e *env, path string) (daemon.Options, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return daemon.Options{}, render.Errorf(render.CodeUsage, "resolving %s: %v", path, err)
	}
	opts := daemon.Options{
		Path:          abs,
		NoDaemon:      e.noDaemon,
		ListenTimeout: listenTimeout(),
	}
	root, err := opts.WorkspaceRoot()
	if err != nil {
		return daemon.Options{}, render.Errorf(render.CodeInternal, "%v", err)
	}
	opts.Workspace = root

	if e.noDaemon {
		return opts, nil
	}

	socket, err := opts.SocketPath()
	if err != nil {
		return daemon.Options{}, render.Errorf(render.CodeInternal, "%v", err)
	}
	// A unix socket address is limited to about a hundred bytes. Left
	// to the kernel, a longer one surfaces as "connect: invalid
	// argument" after the client has waited out its whole spawn budget
	// for a daemon that could never have bound it.
	if len(socket) > maxSocketPath {
		return daemon.Options{}, render.Errorf(render.CodeUsage,
			"the daemon socket path %s is %d bytes, longer than a unix socket address allows (about %d); point XDG_RUNTIME_DIR at a shorter directory, or pass %s",
			socket, len(socket), maxSocketPath, noDaemonFlag)
	}
	opts.Socket = socket
	opts.Spawn = daemon.SpawnConfig{
		Args: func(socket string) []string {
			args := []string{"daemon", "serve", "--listen", socket, "--workspace", root}
			if d := listenTimeout(); d > 0 {
				args = append(args, "--listen.timeout", d.String())
			}
			return args
		},
		Logfile: logPath(socket),
	}
	return opts, nil
}

// logPath is where the daemon for a socket writes its own stderr,
// including every language server's. It sits beside the socket so that
// `daemon logs` needs nothing but the workspace to find it.
func logPath(socket string) string {
	return strings.TrimSuffix(socket, ".sock") + ".log"
}

// daemonOpenError classifies a failure to reach the daemon. The
// daemon's own errors already carry their code and exit status.
func daemonOpenError(err error) error {
	if de, ok := err.(*daemon.Error); ok {
		return remoteFailure{de}
	}
	return render.Errorf(render.CodeSpawnFailed, "%v", err)
}

// sessionCapabilities is the client capabilities every pooled session
// advertises: the defaults plus everything a mutation, `check` or
// `call_hierarchy` needs a server to know.
//
// One set, because a session is shared. The read-only commands that
// meet these extra promises never make the requests they are about —
// resource operations only change what rename and codeAction answer,
// publishDiagnostics and diagnostic only what `check` is sent, and
// workspace/applyEdit is refused by the pool unless a command asked to
// collect it (daemon.Request.CollectEdits). docs/DECISIONS.md D14.
func sessionCapabilities() map[string]any {
	caps := client.DefaultClientCapabilities()
	for _, extra := range []map[string]any{
		mutationCapabilities(),
		checkCapabilities(),
		callHierarchyCapabilities(),
		typeHierarchyCapabilities(),
	} {
		mergeCapabilities(caps, extra)
	}
	return caps
}

// mergeCapabilities lays src over dst, recursing into nested objects so
// that two commands' additions to `textDocument` both survive.
func mergeCapabilities(dst, src map[string]any) {
	for key, value := range src {
		if from, ok := value.(map[string]any); ok {
			if into, ok := dst[key].(map[string]any); ok {
				mergeCapabilities(into, from)
				continue
			}
		}
		dst[key] = value
	}
}
