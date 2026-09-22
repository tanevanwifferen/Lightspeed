package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// daemonSubcommands is the surface of `lightspeed daemon`, in the order
// usage text lists it. `serve` is what a client's auto-spawn runs and is
// not meant to be typed; the other three are PLAN §4's
// `daemon status|stop|logs`.
const daemonSubcommands = "status|stop|logs"

// daemonCommand implements `lightspeed daemon status|stop|logs`, and the
// hidden `daemon serve` that the auto-spawn re-executes (PLAN §3).
func daemonCommand(e *env, c *command, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return e.usagef("daemon: missing subcommand (want one of %s)", daemonSubcommands)
	}
	switch args[0] {
	case "serve":
		return daemonServe(e, args[1:])
	case "status":
		return daemonStatus(e, c, args[1:])
	case "stop":
		return daemonStop(e, c, args[1:])
	case "logs":
		return daemonLogs(e, c, args[1:])
	}
	return e.usagef("daemon: unknown subcommand %q (want one of %s)", args[0], daemonSubcommands)
}

// daemonServe runs the daemon itself: the same daemon.Serve a test's
// child process calls, over the workspace's layered server definitions.
// It serves until
// it has been idle for --listen.timeout, is told to stop, or receives
// SIGINT/SIGTERM — and leaves nothing behind in any of the three cases.
//
// Going idle and finding another daemon already on the socket are both
// successful outcomes, so both exit 0: the auto-spawn races two clients
// to start one daemon, and the loser has done nothing wrong.
func daemonServe(e *env, args []string) int {
	fs := flag.NewFlagSet("daemon serve", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	listen := fs.String("listen", "", "unix socket to listen on")
	workspace := fs.String("workspace", "", "workspace root this daemon is keyed on")
	idle := fs.Duration("listen.timeout", 0, "exit after this long with no client connected (0 = the default; negative = never)")
	if err := fs.Parse(args); err != nil {
		return e.flagError(render.Errorf(render.CodeUsage, "daemon serve: %v", err))
	}
	if *listen == "" || *workspace == "" {
		return e.usagef("daemon serve: --listen and --workspace are required")
	}

	// The daemon reads the workspace's definitions itself, from the
	// environment it was started in. Its config id is what a later
	// command compares its own against (D17).
	cfg, err := e.configForRoot(*workspace)
	if err != nil {
		return e.fail(err)
	}
	pool := cfg.poolOptions(e.stderr)
	logf := func(format string, args ...any) {
		fmt.Fprintf(e.stderr, "%s lightspeed daemon: %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
	}
	pool.Logf = logf

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = daemon.Serve(ctx, daemon.Options{
		Pool:          pool,
		Workspace:     *workspace,
		Socket:        *listen,
		ListenTimeout: *idle,
		// The test hook of D29: how soon a daemon whose binary was
		// replaced notices.
		ExecutableCheck: exeCheck(),
		Logf:            logf,
	})
	switch {
	case err == nil, errors.Is(err, daemon.ErrIdleTimeout), errors.Is(err, daemon.ErrExecutableReplaced), errors.Is(err, daemon.ErrAlreadyRunning):
		return ExitOK
	default:
		logf("exiting: %v", err)
		return ExitCrash
	}
}

// daemonTarget is what `daemon status|stop|logs` have in common: which
// workspace they mean, and how the answer is to be written.
type daemonTarget struct {
	common *commonFlags
	format render.Format
	opts   daemon.Options
}

// parseDaemonTarget parses the flags of a management subcommand. --path
// names any file or directory in the workspace, not its root: the root
// is resolved by walking up, so these work from a subdirectory exactly
// as the commands that started the daemon did.
func parseDaemonTarget(e *env, c *command, sub string, args []string, extra func(*flag.FlagSet)) (*daemonTarget, error) {
	var path string
	cc := *c
	cc.Name = "daemon " + sub
	cc.Args = "[--path DIR]"
	common, _, err := parseFlagsRange(e, &cc, args, 0, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&path, "path", ".", "any file or directory inside the workspace")
		if extra != nil {
			extra(fs)
		}
	})
	if err != nil {
		return nil, err
	}
	if e.noDaemon {
		return nil, render.Errorf(render.CodeUsage,
			"daemon %s: %s and %s mean there is no daemon to ask", sub, noDaemonFlag, noDaemonEnv)
	}
	format, err := common.resolveFormat(e.stdout)
	if err != nil {
		return nil, err
	}
	if format != render.FormatJSON && format != render.FormatText {
		return nil, render.Errorf(render.CodeUnsupportedFormat,
			"format %q has no meaning for daemon %s (want one of json, text)", format, sub)
	}
	opts, err := daemonOptions(e, path)
	if err != nil {
		return nil, err
	}
	opts.NoSpawn = true
	return &daemonTarget{common: common, format: format, opts: opts}, nil
}

// dial connects to the workspace's daemon without starting one. A nil
// handle with a nil error means nothing is running there, which is an
// answer and not a failure: `daemon status` exists to give it.
func (t *daemonTarget) dial(ctx context.Context) (daemon.Handle, error) {
	h, err := daemon.Open(ctx, t.opts)
	switch {
	case err == nil:
		return h, nil
	case errors.Is(err, daemon.ErrNotRunning):
		return nil, nil
	}
	return nil, daemonOpenError(err)
}

func (t *daemonTarget) write(e *env, data any, warnings []string) int {
	err := render.WriteEnvelope(e.stdout, render.Envelope{
		Version: render.EnvelopeVersion, OK: true, Data: data, Warnings: warnings,
	}, render.Options{Indent: t.common.indent})
	if err != nil {
		return e.fail(err)
	}
	return ExitOK
}

// daemonServerData is one pooled server in `daemon status`.
type daemonServerData struct {
	Server     string `json:"server"`
	ServerName string `json:"server_name,omitempty"`
	Root       string `json:"root"`
	// State is "starting" while the initialize handshake is running,
	// "indexing" while the server still reports unfinished $/progress
	// work — the state in which it would answer a query with an empty
	// result of unknown authority, and lightspeed exits 5 instead —
	// and "ready" otherwise.
	State         string    `json:"state"`
	Progress      []string  `json:"progress,omitempty"`
	Requests      int64     `json:"requests"`
	InFlight      int       `json:"in_flight"`
	OpenDocuments int       `json:"open_documents"`
	Started       time.Time `json:"started"`
	IdleSeconds   float64   `json:"idle_seconds"`
}

// daemonStatusData is the payload of `daemon status`.
type daemonStatusData struct {
	Running   bool   `json:"running"`
	Workspace string `json:"workspace"`
	Socket    string `json:"socket"`
	Log       string `json:"log"`

	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
	// Build is the daemon's build identity, and StaleBuild says how it
	// differs from this executable's when it does: the next command
	// would replace it (D29).
	Build      string `json:"build,omitempty"`
	StaleBuild string `json:"stale_build,omitempty"`
	// Started is a pointer because encoding/json's omitempty never
	// omits a struct: with no daemon running a time.Time would print
	// as 0001-01-01T00:00:00Z, a date that reads as data.
	Started  *time.Time `json:"started,omitempty"`
	Uptime   float64    `json:"uptime_seconds,omitempty"`
	Clients  int        `json:"clients,omitempty"`
	Requests int64      `json:"requests,omitempty"`
	Spawns   int64      `json:"spawns,omitempty"`
	// IdleTimeoutSeconds is how long the daemon lingers with no client
	// connected before it exits; SessionIdleTimeoutSeconds how long one
	// unused language server is kept.
	IdleTimeoutSeconds        float64            `json:"idle_timeout_seconds,omitempty"`
	SessionIdleTimeoutSeconds float64            `json:"session_idle_timeout_seconds,omitempty"`
	Servers                   []daemonServerData `json:"servers"`
}

func serverState(s daemon.SessionStatus) string {
	switch {
	case s.Starting:
		return "starting"
	case s.Indexing:
		return "indexing"
	}
	return "ready"
}

// daemonStatus implements `lightspeed daemon status`.
func daemonStatus(e *env, c *command, args []string) int {
	t, err := parseDaemonTarget(e, c, "status", args, nil)
	if err != nil {
		return e.flagError(err)
	}
	ctx, cancel := context.WithTimeout(e.base(), t.common.timeout)
	defer cancel()

	data := daemonStatusData{
		Workspace: t.opts.Workspace,
		Socket:    t.opts.Socket,
		Log:       logPath(t.opts.Socket),
		Servers:   []daemonServerData{},
	}
	h, err := t.dial(ctx)
	if err != nil {
		return e.fail(err)
	}
	if h != nil {
		defer h.Close()
		st, err := h.Status(ctx)
		if err != nil {
			return e.fail(remoteOrPlain(err))
		}
		data.Running = true
		data.PID, data.Executable = st.PID, st.Executable
		data.Build = st.Build.ID()
		data.StaleBuild = daemon.SelfBuild().Differs(st.Build)
		if !st.Started.IsZero() {
			started := st.Started
			data.Started = &started
		}
		data.Uptime = st.Uptime.Seconds()
		data.Clients, data.Requests, data.Spawns = st.Clients, st.Requests, st.Spawns
		data.IdleTimeoutSeconds = st.ListenTimeout.Seconds()
		data.SessionIdleTimeoutSeconds = st.SessionIdleTimeout.Seconds()
		for _, s := range st.Sessions {
			data.Servers = append(data.Servers, daemonServerData{
				Server: s.Server, ServerName: s.ServerName, Root: s.Root,
				State: serverState(s), Progress: s.Progress,
				Requests: s.Requests, InFlight: s.InFlight, OpenDocuments: s.OpenDocuments,
				Started: s.Started, IdleSeconds: s.Idle.Seconds(),
			})
		}
	}

	if t.format == render.FormatText {
		writeStatusText(e.stdout, data)
		return ExitOK
	}
	return t.write(e, data, nil)
}

// writeStatusText is the human reading of `daemon status`.
func writeStatusText(w io.Writer, d daemonStatusData) {
	if !d.Running {
		fmt.Fprintf(w, "no daemon is running for %s\n", d.Workspace)
		return
	}
	fmt.Fprintf(w, "daemon %d for %s\n", d.PID, d.Workspace)
	fmt.Fprintf(w, "  socket:       %s\n", d.Socket)
	if d.StaleBuild != "" {
		fmt.Fprintf(w, "  stale:        %s; the next command replaces it\n", d.StaleBuild)
	}
	fmt.Fprintf(w, "  idle timeout: %s (session: %s)\n",
		secondsString(d.IdleTimeoutSeconds), secondsString(d.SessionIdleTimeoutSeconds))
	fmt.Fprintf(w, "  requests:     %d, servers started: %d, clients: %d\n", d.Requests, d.Spawns, d.Clients)
	if len(d.Servers) == 0 {
		fmt.Fprintln(w, "  no language servers are running")
	}
	for _, s := range d.Servers {
		fmt.Fprintf(w, "  %s %s [%s] requests=%d open=%d idle=%s\n",
			s.Server, s.Root, s.State, s.Requests, s.OpenDocuments, secondsString(s.IdleSeconds))
	}
}

func secondsString(s float64) string {
	return (time.Duration(s * float64(time.Second))).Round(time.Millisecond).String()
}

// remoteOrPlain wraps a daemon error so it renders with its own code.
func remoteOrPlain(err error) error {
	var de *daemon.Error
	if errors.As(err, &de) {
		return remoteFailure{de}
	}
	return render.Errorf(render.CodeServerCrash, "%v", err)
}

// daemonStopData is the payload of `daemon stop`.
type daemonStopData struct {
	// Stopped reports that a daemon was running and has gone; false
	// with no error means there was nothing to stop.
	Stopped   bool   `json:"stopped"`
	Workspace string `json:"workspace"`
	Socket    string `json:"socket"`
}

// daemonStop implements `lightspeed daemon stop`. It returns once the
// daemon has actually gone — Stop is answered before the shutdown
// finishes, and a caller who runs `daemon stop && rm -rf` or starts a
// new daemon straight after must not race the old one's last seconds.
func daemonStop(e *env, c *command, args []string) int {
	t, err := parseDaemonTarget(e, c, "stop", args, nil)
	if err != nil {
		return e.flagError(err)
	}
	ctx, cancel := context.WithTimeout(e.base(), t.common.timeout)
	defer cancel()

	data := daemonStopData{Workspace: t.opts.Workspace, Socket: t.opts.Socket}
	h, err := t.dial(ctx)
	if err != nil {
		return e.fail(err)
	}
	if h != nil {
		// The pid is asked for before the stop, because after it there is
		// nobody to ask.
		var pid int
		if st, err := h.Status(ctx); err == nil && st.PID != os.Getpid() {
			pid = st.PID
		}
		err := h.Stop(ctx)
		_ = h.Close()
		if err != nil {
			return e.fail(remoteOrPlain(err))
		}
		for {
			gone, err := t.gone(ctx)
			if err != nil {
				return e.fail(err)
			}
			if gone {
				break
			}
			select {
			case <-ctx.Done():
				return e.fail(render.Errorf(render.CodeTimeout,
					"the daemon for %s was told to stop and was still running after %s", t.opts.Workspace, t.common.timeout))
			case <-time.After(20 * time.Millisecond):
			}
		}
		// The socket is unpublished first and the daemon is still shutting
		// its language servers down for a while after that, so "gone" is the
		// process, not the socket.
		if err := daemon.WaitExit(ctx, pid); err != nil {
			return e.fail(render.Errorf(render.CodeTimeout,
				"the daemon for %s was told to stop and was still running after %s: %v", t.opts.Workspace, t.common.timeout, err))
		}
		data.Stopped = true
	}

	if t.format == render.FormatText {
		if data.Stopped {
			fmt.Fprintf(e.stdout, "stopped the daemon for %s\n", data.Workspace)
		} else {
			fmt.Fprintf(e.stdout, "no daemon is running for %s\n", data.Workspace)
		}
		return ExitOK
	}
	return t.write(e, data, nil)
}

// gone reports whether nothing answers on the workspace's socket any
// more.
func (t *daemonTarget) gone(ctx context.Context) (bool, error) {
	h, err := t.dial(ctx)
	if err != nil {
		return false, err
	}
	if h == nil {
		return true, nil
	}
	_ = h.Close()
	return false, nil
}

// logTailLimit bounds how much of the log `daemon logs` reads, from the
// end. A daemon's stderr is also every language server's, and rust-analyzer
// is not quiet.
const logTailLimit = 1 << 20

// daemonLogsData is the payload of `daemon logs`.
type daemonLogsData struct {
	Path string `json:"path"`
	// Exists is false when no daemon has ever logged for the workspace.
	Exists bool     `json:"exists"`
	Lines  []string `json:"lines"`
}

// daemonLogs implements `lightspeed daemon logs`: the last --lines lines
// of the log the auto-spawned daemon's stderr is redirected to. It reads
// a file, so it works whether the daemon is running or has gone — which
// is when a log is most wanted.
func daemonLogs(e *env, c *command, args []string) int {
	var lines int
	t, err := parseDaemonTarget(e, c, "logs", args, func(fs *flag.FlagSet) {
		fs.IntVar(&lines, "lines", 50, "how many trailing lines to print (0 = all that were read)")
	})
	if err != nil {
		return e.flagError(err)
	}
	if lines < 0 {
		return e.usagef("daemon logs: --lines must not be negative (got %d)", lines)
	}

	data := daemonLogsData{Path: logPath(t.opts.Socket), Lines: []string{}}
	tail, err := readTail(data.Path, logTailLimit)
	var warnings []string
	switch {
	case err == nil:
		data.Exists = true
		data.Lines = lastLines(tail, lines)
	case errors.Is(err, os.ErrNotExist):
		warnings = append(warnings, "no daemon has logged for this workspace yet")
	default:
		return e.fail(render.Errorf(render.CodeIOError, "reading %s: %v", data.Path, err))
	}

	if t.format == render.FormatText {
		for _, line := range data.Lines {
			fmt.Fprintln(e.stdout, line)
		}
		return ExitOK
	}
	return t.write(e, data, warnings)
}

// readTail returns up to limit bytes from the end of a file.
func readTail(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := int64(0)
	if info.Size() > limit {
		start = info.Size() - limit
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if start > 0 {
		// Began mid-line; the fragment is not a line.
		if i := strings.IndexByte(string(buf), '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return buf, nil
}

// lastLines splits text into lines and keeps the last n (all of them if
// n is 0).
func lastLines(text []byte, n int) []string {
	trimmed := strings.TrimRight(string(text), "\n")
	if trimmed == "" {
		return []string{}
	}
	lines := strings.Split(trimmed, "\n")
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}
